package metrics

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagementCountsBySurfaceAndClass(t *testing.T) {
	m := New(Options{})
	refuse := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host == "attacker.example" {
				http.Error(w, "refused", http.StatusForbidden) // a guard in front of the mux
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	h := m.Management(refuse(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/missing":
			http.NotFound(w, r)
		case "/console/api/settings":
			w.WriteHeader(http.StatusNoContent)
		case "/mcp":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusEarlyHints) // informational first
			w.WriteHeader(http.StatusAccepted)
		case "/console/":
			http.Redirect(w, r, "/", http.StatusFound)
		case "/api/boom":
			panic("boom")
		}
		// Anything else writes nothing: 200.
	})))
	do := func(host, path string) {
		t.Helper()
		defer func() { _ = recover() }()
		req := httptest.NewRequest("GET", "http://"+host+path, nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	do("127.0.0.1", "/api/flats")
	do("127.0.0.1", "/api/missing")
	do("attacker.example", "/api/flats")
	do("127.0.0.1", "/console/api/settings")
	do("127.0.0.1", "/mcp")
	do("127.0.0.1", "/metrics")
	do("attacker.example", "/metrics")
	do("127.0.0.1", "/console/")
	do("127.0.0.1", "/llms.txt")
	do("127.0.0.1", "/api")
	do("127.0.0.1", "/mcpx")
	do("127.0.0.1", "/api/boom")
	for _, tc := range []struct {
		surface, class string
		want           uint64
	}{
		{"api", "2xx", 1}, {"api", "4xx", 2}, {"api", "5xx", 1},
		{"console", "2xx", 1},
		{"mcp", "2xx", 1}, {"mcp", "1xx", 0},
		{"metrics", "2xx", 1}, {"metrics", "4xx", 1},
		{"other", "3xx", 1}, {"other", "2xx", 3},
	} {
		if got := m.ManagementCount(tc.surface, tc.class); got != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.surface, tc.class, got, tc.want)
		}
	}
	vec := samplesOf(t, m)
	if v, ok := value(vec, "flats_management_requests_total", map[string]string{"surface": "api", "code": "4xx"}); !ok || v != 2 {
		t.Errorf("exposed api 4xx = %v %v", v, ok)
	}
	if n := series(vec, "flats_management_requests_total"); n != 25 {
		t.Errorf("flats_management_requests_total has %d series, want 25", n)
	}
}

// A panic is re-raised unchanged. Before any status it counts as 5xx, except
// http.ErrAbortHandler (a departed client), which is not counted at all; after
// a status both keep that status.
func TestManagementPanicIsReraised(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		value  any
		class  string // "": not counted
	}{
		{"panic before a status is 5xx", 0, "boom", "5xx"},
		{"panic after a status keeps it", http.StatusCreated, "boom", "2xx"},
		{"abort before a status is not counted", 0, http.ErrAbortHandler, ""},
		{"abort after a status keeps it", http.StatusOK, http.ErrAbortHandler, "2xx"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(Options{})
			h := m.Management(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				panic(tc.value)
			}))
			func() {
				defer func() {
					if r := recover(); r != tc.value {
						t.Fatalf("recovered %v, want %v", r, tc.value)
					}
				}()
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/x", nil))
			}()
			for _, class := range managementClasses {
				want := uint64(0)
				if class == tc.class {
					want = 1
				}
				if got := m.ManagementCount("api", class); got != want {
					t.Errorf("api %s = %d, want %d", class, got, want)
				}
			}
		})
	}
}

// readFromRecorder is a ResponseWriter whose ReadFrom stands for net/http's
// sendfile path.
type readFromRecorder struct {
	*httptest.ResponseRecorder
	readFrom int
}

func (r *readFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	r.readFrom++
	return io.Copy(r.ResponseRecorder, src)
}

// A file copied into the response reaches the inner writer's ReadFrom, so
// management responses keep sendfile; without one it is copied through Write.
func TestManagementKeepsReadFrom(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asset.js")
	if err := os.WriteFile(path, []byte("console.log(1)"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(Options{})
	h := m.Management(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, err := os.Open(path)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		if _, ok := w.(io.ReaderFrom); !ok {
			t.Error("the wrapper hides io.ReaderFrom")
		}
		io.Copy(w, f)
	}))
	inner := &readFromRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(inner, httptest.NewRequest("GET", "/console/app.js", nil))
	if inner.readFrom != 1 || inner.Body.String() != "console.log(1)" || inner.Code != http.StatusOK {
		t.Fatalf("ReadFrom calls %d, body %q, code %d", inner.readFrom, inner.Body.String(), inner.Code)
	}
	plain := httptest.NewRecorder()
	h.ServeHTTP(plain, httptest.NewRequest("GET", "/console/app.js", nil))
	if plain.Body.String() != "console.log(1)" {
		t.Fatalf("copied body %q", plain.Body.String())
	}
	if got := m.ManagementCount("other", "2xx"); got != 2 {
		t.Fatalf("other 2xx = %d", got)
	}
}

// An event stream flushes through the wrapper, both through the
// ResponseController (the MCP SDK) and through an http.Flusher assertion.
func TestManagementKeepsEventStreamsFlushing(t *testing.T) {
	m := New(Options{})
	release := make(chan struct{})
	srv := httptest.NewServer(m.Management(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
			t.Errorf("SetWriteDeadline through Unwrap: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: one\n\n")
		if err := rc.Flush(); err != nil {
			t.Errorf("ResponseController.Flush: %v", err)
		}
		<-release
		io.WriteString(w, "data: two\n\n")
		w.(http.Flusher).Flush()
	})))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil || line != "data: one\n" {
		t.Fatalf("first event before the handler returned: %q %v", line, err)
	}
	close(release)
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "data: two") {
		t.Fatalf("second event: %q", rest)
	}
	resp.Body.Close()
	srv.Close() // waits for the handler, so the count is final
	if m.ManagementCount("mcp", "2xx") != 1 {
		t.Fatalf("mcp 2xx = %d", m.ManagementCount("mcp", "2xx"))
	}
}

// A hijacked connection (a WebSocket upgrade) works through the wrapper and
// counts as 1xx.
func TestManagementKeepsHijack(t *testing.T) {
	m := New(Options{})
	done := make(chan struct{})
	srv := httptest.NewServer(m.Management(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		if _, ok := w.(http.Hijacker); !ok {
			t.Error("wrapper hides http.Hijacker")
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n")
		rw.Flush()
		line, _ := rw.ReadString('\n')
		rw.WriteString("echo: " + line)
		rw.Flush()
	})))
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET /api/ws HTTP/1.1\r\nHost: x\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	io.WriteString(conn, "hello\n")
	line, err := br.ReadString('\n')
	if err != nil || line != "echo: hello\n" {
		t.Fatalf("echo: %q %v", line, err)
	}
	<-done
	srv.Close()
	if m.ManagementCount("api", "1xx") != 1 {
		t.Fatalf("api 1xx = %d", m.ManagementCount("api", "1xx"))
	}
}

func TestNilMetricsManagementPassesThrough(t *testing.T) {
	var m *Metrics
	h := http.NotFoundHandler()
	if got := m.Management(h); got == nil {
		t.Fatal("nil handler")
	}
}
