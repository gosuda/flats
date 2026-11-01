package notebook

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/wsconn"
)

func TestLiveStopClosesPrivateSockets(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
		stop func(*Instance)
	}{
		{name: "stop", stop: (*Instance).Stop},
		{name: "restart", stop: func(in *Instance) { in.Shutdown("") }},
		{name: "unavailable", stop: func(in *Instance) { in.Shutdown("unavailable") }},
		{name: "deleted", code: "deleted", stop: func(in *Instance) { in.Shutdown("deleted") }},
		{name: "renamed", code: "renamed", stop: func(in *Instance) { in.Shutdown("renamed") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWriterFixture(t)
			ws, _ := liveDial(t, f.in, "private", nil)
			liveHelloSend(t, ws, f.session.Token, f.page.ID)
			liveUntil(t, ws, "welcome")
			liveUntil(t, ws, "snapshot")
			tc.stop(f.in)

			_ = ws.NetConn().SetReadDeadline(time.Now().Add(4 * time.Second))
			fatal := ""
			for {
				raw, err := ws.ReadMessage(liveServerFrame)
				if err != nil {
					var closed *wsconn.CloseError
					if !errors.As(err, &closed) || closed.Code != 1001 {
						t.Fatalf("expected a going-away close, got %v", err)
					}
					break
				}
				var frame struct {
					Type string `json:"type"`
					Code string `json:"code"`
				}
				if err := json.Unmarshal([]byte(raw), &frame); err != nil {
					t.Fatal(err)
				}
				if frame.Type == "fatal" {
					if tc.code == "" || fatal != "" {
						t.Fatalf("unexpected fatal: %s", raw)
					}
					fatal = frame.Code
				}
			}
			if fatal != tc.code {
				t.Fatalf("fatal code = %q, want %q", fatal, tc.code)
			}
		})
	}
}

func TestLiveRecoveryUnknownSession(t *testing.T) {
	for _, pending := range []int{0, 1} {
		t.Run(fmt.Sprintf("pending=%d", pending), func(t *testing.T) {
			f := newWriterFixture(t)
			ws, _ := liveDial(t, f.in, "private", nil)
			liveSend(t, ws, map[string]any{"type": "hello", "version": 1, "session": "unknown-session", "pending": map[string]any{"count": pending}})
			fatal := liveUntil(t, ws, "fatal")
			if string(fatal["code"]) != `"session_expired"` {
				t.Fatalf("unknown session response: %s", fatal)
			}
		})
	}
}
