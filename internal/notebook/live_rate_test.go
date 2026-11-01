package notebook

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/wsconn"
)

func TestLivePublicWatchFloodPreservesNewVisitorAdmission(t *testing.T) {
	f := newWriterFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	var peers []*wsconn.Conn
	var workers sync.WaitGroup
	var sent atomic.Int64
	defer func() {
		cancel()
		for _, peer := range peers {
			peer.Abort()
		}
		workers.Wait()
	}()
	for range 32 {
		peer, _ := liveDial(t, f.in, "public", nil)
		peers = append(peers, peer)
		liveHelloSend(t, peer, "", f.page.ID)
		liveUntil(t, peer, "welcome")
	}
	for _, peer := range peers {
		workers.Go(func() {
			for {
				if _, err := peer.ReadMessage(liveServerFrame); err != nil {
					return
				}
			}
		})
		workers.Go(func() {
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for generation := 2; ; generation++ {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
				raw, _ := json.Marshal(liveWatch{Type: "watch", Generation: generation, Page: f.page.ID, Databases: []ID{}, Pending: []liveScope{}})
				if peer.WriteFrame(wsconn.OpText, raw) != nil {
					return
				}
				sent.Add(1)
			}
		})
	}
	time.Sleep(time.Second)
	visitor, _ := liveDial(t, f.in, "public", nil)
	start := time.Now()
	liveHelloSend(t, visitor, "", f.page.ID)
	if err := visitor.NetConn().SetReadDeadline(start.Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		raw, err := visitor.ReadMessage(liveServerFrame)
		if err != nil {
			t.Fatalf("new visitor received no snapshot or retryable refusal within 3s (%d watch frames): %v", sent.Load(), err)
		}
		var frame map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &frame); err != nil {
			t.Fatal(err)
		}
		switch liveType(frame) {
		case "snapshot":
		case "fatal":
			if string(frame["code"]) != `"unavailable"` {
				t.Fatalf("non-retryable refusal: %s", raw)
			}
		default:
			continue
		}
		t.Logf("new visitor received %s in %s under %d watch frames", liveType(frame), time.Since(start), sent.Load())
		if sent.Load() < 32*8 {
			t.Fatal("flood did not reach every connection's burst allowance")
		}
		return
	}
}

func TestLivePublicStartTimeoutIsRetryable(t *testing.T) {
	f := newWriterFixture(t)
	f.in.live.mu.Lock()
	f.in.live.starts = liveRate{last: time.Now().Add(time.Hour)}
	f.in.live.mu.Unlock()
	peer, _ := liveDial(t, f.in, "public", nil)
	start := time.Now()
	liveHelloSend(t, peer, "", f.page.ID)
	liveUntil(t, peer, "welcome")
	for generation := 2; generation <= 8; generation++ {
		time.Sleep(400 * time.Millisecond)
		liveSend(t, peer, liveWatch{Type: "watch", Generation: generation, Page: f.page.ID, Databases: []ID{}, Pending: []liveScope{}})
	}
	if err := peer.NetConn().SetReadDeadline(start.Add(12 * time.Second)); err != nil {
		t.Fatal(err)
	}
	raw, err := peer.ReadMessage(liveServerFrame)
	if err != nil {
		t.Fatalf("start timeout left visitor without a retryable refusal: %v", err)
	}
	var frame map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &frame); err != nil {
		t.Fatal(err)
	}
	if liveType(frame) != "fatal" || string(frame["code"]) != `"unavailable"` {
		t.Fatalf("start timeout was not retryable: %s", raw)
	}
}

func TestLiveWatchKeepsNewestGenerationWhilePublicStartWaits(t *testing.T) {
	for _, access := range []string{"public", "private"} {
		t.Run(access, func(t *testing.T) {
			f := newWriterFixture(t)
			f.in.live.mu.Lock()
			f.in.live.starts = liveRate{last: time.Now().Add(time.Hour)}
			f.in.live.mu.Unlock()
			peer, _ := liveDial(t, f.in, access, nil)
			session := ""
			if access == "private" {
				session = f.session.Token
			}
			liveHelloSend(t, peer, session, f.page.ID)
			liveUntil(t, peer, "welcome")
			for generation := 2; generation <= 8; generation++ {
				liveSend(t, peer, liveWatch{Type: "watch", Generation: generation, Page: f.page.ID, Databases: []ID{}, Pending: []liveScope{}})
			}
			f.in.live.mu.Lock()
			f.in.live.starts = liveRate{}
			f.in.live.mu.Unlock()
			for {
				frame := liveUntil(t, peer, "snapshot")
				if string(frame["generation"]) == "8" && string(frame["scope"]) == `{"kind":"page","id":"`+string(f.page.ID)+`"}` {
					break
				}
			}
			if access == "private" {
				action := writerAction(f.page, "Still editing")
				raw, err := EncodeAction(action)
				if err != nil {
					t.Fatal(err)
				}
				var frame map[string]any
				if err := json.Unmarshal(raw, &frame); err != nil {
					t.Fatal(err)
				}
				frame["type"] = "action"
				liveSend(t, peer, frame)
				liveUntil(t, peer, "ack")
			}
		})
	}
}
