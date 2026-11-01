package core

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/store"
	"github.com/gosuda/flats/internal/wsconn"
)

func TestLiveNotebookPublicEpochTrusted(t *testing.T) {
	s, _ := newTestService(t)
	newNotebook(t, s, "notes")
	f, err := s.st.GetFlat(t.Context(), "notes")
	if err != nil {
		t.Fatal(err)
	}
	f.Visibility, f.PublicAccess = store.Public, store.AccessView
	if err := s.st.UpdateFlat(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	d := *s.currentDeployment("notes")
	d.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Observed-Epoch", r.Header.Get(AccessEpochHeader))
		w.Header().Set("Observed-Access", r.Header.Get("X-Flats-Access"))
	})
	s.state("notes").cur.Store(&d)
	request := func(h http.Handler) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("GET", "http://notes.test/", nil)
		r.Header["x-flats-access-epoch"] = []string{"forged-lower"}
		r.Header.Set(AccessEpochHeader, "forged")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	old := s.siteHandler("notes", true)
	first := request(old)
	epoch := first.Header().Get("Observed-Epoch")
	if first.Code != 200 || epoch == "" || epoch == "forged" || epoch == "forged-lower" {
		t.Fatalf("Public route did not supply a trusted epoch: %d %q", first.Code, epoch)
	}
	if got := request(s.siteHandler("notes", true)).Header().Get("Observed-Epoch"); got != epoch {
		t.Fatalf("same exposure changed epoch: %q -> %q", epoch, got)
	}
	private := request(s.siteHandler("notes", false))
	if private.Header().Get("Observed-Epoch") != "" || private.Header().Get("Observed-Access") != "private" {
		t.Fatalf("Private route inherited Public metadata: %v", private.Header())
	}
	if err := s.stopPublicRoutes(t.Context(), "notes"); err != nil {
		t.Fatal(err)
	}
	if w := request(old); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("withdrawn handler returned %d", w.Code)
	}
	next := request(s.siteHandler("notes", true)).Header().Get("Observed-Epoch")
	if next == "" || next == epoch {
		t.Fatalf("re-exposure reused epoch %q", next)
	}
	current, err := s.st.GetFlat(t.Context(), "notes")
	if err != nil || current.AccessSeq != f.AccessSeq {
		t.Fatalf("route withdrawal changed access sequence: %+v, %v", current, err)
	}
}

// Keep the first snapshot incomplete even when the loopback peer reads quickly.
// Close must unblock the writer, just as closing a real network connection does.
type notebookPublicGateConn struct {
	net.Conn
	closed    chan struct{}
	once      sync.Once
	snapshots int
}

func (c *notebookPublicGateConn) Write(p []byte) (int, error) {
	var frame struct {
		Type string `json:"type"`
	}
	if len(p) > 0 && p[0] == '{' && json.Unmarshal(p, &frame) == nil && frame.Type == "snapshot" {
		c.snapshots++
		if c.snapshots > 1 {
			<-c.closed
			return 0, net.ErrClosed
		}
	}
	return c.Conn.Write(p)
}

func (c *notebookPublicGateConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

type notebookPublicGateWriter struct{ http.ResponseWriter }

func (w notebookPublicGateWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	return &notebookPublicGateConn{Conn: c, closed: make(chan struct{})}, rw, nil
}

func notebookPublicConnect(t *testing.T, h http.Handler, hello map[string]any) (*wsconn.Conn, map[string]json.RawMessage, <-chan struct{}) {
	t.Helper()
	ended := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(ended)
		h.ServeHTTP(notebookPublicGateWriter{w}, r)
	}))
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
	t.Cleanup(func() { ws.Close(1006, ""); <-ws.Done() })
	raw, err := json.Marshal(hello)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFrame(wsconn.OpText, raw); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		_ = ws.NetConn().SetReadDeadline(time.Now().Add(5 * time.Second))
		raw, err := ws.ReadMessage(512 << 10)
		if err != nil {
			t.Fatal(err)
		}
		var frame map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &frame); err != nil {
			t.Fatal(err)
		}
		if string(frame["type"]) == `"snapshot"` {
			return ws, frame, ended
		}
		if string(frame["type"]) != `"welcome"` {
			t.Fatalf("unexpected live frame: %s", raw)
		}
	}
	t.Fatal("snapshot not received")
	return nil, nil, nil
}

func notebookPublicDisconnect(t *testing.T, ws *wsconn.Conn, ended <-chan struct{}) {
	t.Helper()
	ws.Close(1006, "")
	<-ws.Done()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("Public handler did not finish disconnecting")
	}
}

func TestLiveNotebookPublicWithdrawalDeletesDisconnectedCapture(t *testing.T) {
	for _, mode := range []string{"legacy", "lifecycle", "slug"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := newTestService(t)
			newNotebook(t, s, "notes")
			ops := make([]string, 32)
			for i := range ops {
				ops[i] = fmt.Sprintf(`{"op":"create_page","ref":%q,"parent":null,"before":null,"title":%q}`, fmt.Sprint("p", i), strings.Repeat("<", 8000))
			}
			_, result := writeNotebook(t, s, "notes", "large-tree", strings.Join(ops, ","))
			f, err := s.st.GetFlat(t.Context(), "notes")
			if err != nil {
				t.Fatal(err)
			}
			f.Visibility, f.PublicAccess = store.Public, store.AccessView
			if err := s.st.UpdateFlat(t.Context(), f); err != nil {
				t.Fatal(err)
			}
			hello := map[string]any{"type": "hello", "version": 1, "pending": map[string]any{"count": 0}}
			h := s.siteHandler("notes", true)
			ws, first, ended := notebookPublicConnect(t, h, hello)
			var token string
			if err := json.Unmarshal(first["spill"], &token); err != nil || token == "" {
				t.Fatalf("snapshot spill: %s, %v", first, err)
			}
			notebookPublicDisconnect(t, ws, ended)

			// Resuming before withdrawal proves the handle survived the disconnect.
			hello["spill"], hello["next_window"] = token, 0
			hello["watermark"] = first["seq"]
			ws, resumed, ended := notebookPublicConnect(t, h, hello)
			if string(resumed["spill"]) != string(first["spill"]) {
				t.Fatalf("ordinary reconnect replaced retained capture: %s / %s", first["spill"], resumed["spill"])
			}
			notebookPublicDisconnect(t, ws, ended)
			paths, err := filepath.Glob(filepath.Join(s.cfg.DataDir, "cache", "notebook-spills", "owner-*", "spills", "notebook-spill-"+token))
			if err != nil || len(paths) != 1 {
				t.Fatalf("expected retained spill directory, found %d: %v", len(paths), err)
			}
			if mode != "legacy" {
				s.cfg.Lifecycle = newLifecycleRouteNet()
			}
			withdraw := s.stopPublicRoutes
			if mode == "slug" {
				withdraw = s.stopSlugRoutes
			}
			if err := withdraw(t.Context(), "notes"); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
				t.Fatalf("withdrawal left disconnected spill on disk: %v", err)
			}
			if mode == "lifecycle" {
				f.Visibility, f.PublicAccess = store.Private, ""
				if err := s.st.UpdateFlat(t.Context(), f); err != nil {
					t.Fatal(err)
				}
				writeNotebook(t, s, "notes", "private-title", notebookTitleOp(t, result.Refs["p0"].Ref, "Privately changed"))
				f.Visibility, f.PublicAccess = store.Public, store.AccessView
				if err := s.st.UpdateFlat(t.Context(), f); err != nil {
					t.Fatal(err)
				}
			}
			ws, fresh, ended := notebookPublicConnect(t, s.siteHandler("notes", true), hello)
			if string(fresh["spill"]) == string(first["spill"]) {
				t.Fatal("re-exposure resumed a withdrawn capture")
			}
			if mode == "lifecycle" && (string(fresh["seq"]) == string(first["seq"]) || !strings.Contains(string(fresh["data"]), "Privately changed")) {
				t.Fatal("re-exposure did not capture the privately changed content")
			}
			if mode != "lifecycle" && string(fresh["seq"]) != string(first["seq"]) {
				t.Fatalf("sequence changed during withdrawal: %s -> %s", first["seq"], fresh["seq"])
			}
			notebookPublicDisconnect(t, ws, ended)
		})
	}
}
