package core_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
	"github.com/gosuda/flats/internal/wsconn"
)

// originConn adds an Origin header to the handshake written through it.
type originConn struct {
	net.Conn
	origin string
	done   bool
}

func (c *originConn) Write(p []byte) (int, error) {
	if !c.done && bytes.HasSuffix(p, []byte("\r\n\r\n")) {
		c.done = true
		q := append(slices.Clone(p[:len(p)-2]), []byte("Origin: "+c.origin+"\r\n\r\n")...)
		if _, err := c.Conn.Write(q); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	return c.Conn.Write(p)
}

// Withdrawing Public access closes the open Public canvas sockets of a
// board at once (board rule 53, board-TECH 7.1).
func TestBoardPrivateSwitchClosesPublicSockets(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, err := e.svc.CreateBoard(ctx, "plan", "Plan", core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	permitPortal(t, e.svc, "plan")
	setVisibility := func(vis store.Visibility) {
		t.Helper()
		r, err := e.svc.SetVisibility(ctx, "plan", vis, core.ViaMCP, "")
		if err != nil || r.Status != "pending_approval" {
			t.Fatalf("visibility %s: %+v %v", vis, r, err)
		}
		if _, err := e.svc.Decide(ctx, r.Approval.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	setVisibility(store.Public)
	u, err := url.Parse(e.pub.URL("plan"))
	if err != nil {
		t.Fatal(err)
	}
	ws, err := wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) {
		c, err := net.Dial("tcp", "127.0.0.1:"+u.Port())
		if err != nil {
			return nil, err
		}
		return &originConn{Conn: c, origin: e.pub.URL("plan")}, nil
	}, u.Host, "/_board/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close(1000, "")
	hello, _ := json.Marshal(map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}, "editor": "viewer"})
	if err := ws.WriteFrame(wsconn.OpText, hello); err != nil {
		t.Fatal(err)
	}
	msgs := make(chan map[string]any, 64)
	go func() {
		defer close(msgs)
		for {
			raw, err := ws.ReadMessage(1 << 20)
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal([]byte(raw), &m) == nil {
				msgs <- m
			}
		}
	}()
	welcome := <-msgs
	if welcome["t"] != "welcome" || welcome["access"] != "public" {
		t.Fatalf("public welcome = %v", welcome)
	}
	setVisibility(store.Private)
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, open := <-msgs:
			if !open {
				return
			}
		case <-timeout:
			t.Fatal("the Public socket stayed open after the board became Private")
		}
	}
}
