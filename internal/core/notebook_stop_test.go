package core

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/store"
	"github.com/gosuda/flats/internal/wsconn"
)

func TestNotebookHostCloseDoesNotStopEditing(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s, err := New(t.Context(), Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	closeHost := func() {
		once.Do(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(closeHost)
	newNotebook(t, s, "notes")
	handler := s.siteHandler("notes", false)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	ws, err := wsconn.Dial(t.Context(), func(ctx context.Context) (net.Conn, error) {
		c, err := new(net.Dialer).DialContext(ctx, "tcp", host)
		if err != nil {
			return nil, err
		}
		return &originConn{Conn: c, origin: server.URL}, nil
	}, host, "/_notebook/ws")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close(1000, ""); <-ws.Done() })
	if err := ws.WriteFrame(wsconn.OpText, []byte(`{"type":"hello","version":1,"pending":{"count":0}}`)); err != nil {
		t.Fatal(err)
	}
	_ = ws.NetConn().SetReadDeadline(time.Now().Add(4 * time.Second))
	raw, err := ws.ReadMessage(512 << 10)
	if err != nil {
		t.Fatal(err)
	}
	var frame struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(raw), &frame); err != nil || frame.Type != "welcome" {
		t.Fatalf("welcome = %s, %v", raw, err)
	}
	closeHost()
	for {
		raw, err := ws.ReadMessage(512 << 10)
		if err != nil {
			var closed *wsconn.CloseError
			if !errors.As(err, &closed) || closed.Code != 1001 {
				t.Fatalf("host close = %v, want going-away close", err)
			}
			break
		}
		if err := json.Unmarshal([]byte(raw), &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Type == "fatal" {
			t.Fatalf("host close stopped editing: %s", raw)
		}
	}
}
