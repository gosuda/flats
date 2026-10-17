package core

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/store"
)

func classCounts(u *flatUsage, route int) [classCount]uint64 {
	var out [classCount]uint64
	for i := range out {
		out[i] = u.requests[route][i].Load()
	}
	return out
}

func only(class int) [classCount]uint64 {
	var out [classCount]uint64
	out[class] = 1
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestUsageWriterCountsStatus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cancel    bool // the visitor left before the handler returned
		handler   func(w http.ResponseWriter, r *http.Request)
		class     int
		timed     bool
		bytes     uint64
		repanic   any
		stream    bool
		statusDay bool
	}{
		{name: "WriteHeader", handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }, class: class4xx, timed: true},
		{name: "first Write is 200", handler: func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hello") }, class: class2xx, timed: true, bytes: 5},
		{name: "implicit 200", handler: func(w http.ResponseWriter, r *http.Request) {}, class: class2xx, timed: true},
		{name: "informational then 302", handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(103); w.WriteHeader(302) }, class: class3xx, timed: true},
		{name: "ReadFrom", handler: func(w http.ResponseWriter, r *http.Request) { io.Copy(w, strings.NewReader("abcdef")) }, class: class2xx, timed: true, bytes: 6},
		{name: "event stream", handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
		}, class: class2xx, stream: true},
		{name: "503 is an error", handler: func(w http.ResponseWriter, r *http.Request) { http.Error(w, "busy", 503) }, class: class5xx, timed: true, bytes: 5},
		{name: "abort keeps the sent status", handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); panic(http.ErrAbortHandler) }, class: class2xx, timed: true, repanic: http.ErrAbortHandler},
		{name: "panic before a status is 5xx", handler: func(w http.ResponseWriter, r *http.Request) { panic("boom") }, class: class5xx, timed: true, repanic: "boom"},
		{name: "panic after a status keeps it", handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201); panic("boom") }, class: class2xx, timed: true, repanic: "boom"},
		{name: "departed visitor answered 502 is closed", cancel: true, handler: func(w http.ResponseWriter, r *http.Request) { http.Error(w, "bad gateway", 502) }, class: classClosed, bytes: 12},
		{name: "departed visitor answered 503 is closed", cancel: true, handler: func(w http.ResponseWriter, r *http.Request) { http.Error(w, "waking", 503) }, class: classClosed, bytes: 7},
		{name: "departed visitor answered nothing is closed", cancel: true, handler: func(w http.ResponseWriter, r *http.Request) {}, class: classClosed},
		{name: "abort after a departed visitor is closed", cancel: true, handler: func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) }, class: classClosed, repanic: http.ErrAbortHandler},
		{name: "disconnect while sending keeps the status", cancel: true, handler: func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "partial") }, class: class2xx, timed: true, bytes: 7},
		{name: "disconnect after a 500 keeps the error", cancel: true, handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }, class: class5xx, timed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newFlatUsage(func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
			var streamsDuring int64
			h := func(w http.ResponseWriter, r *http.Request) {
				defer func() { streamsDuring = u.openStreams.Load() }()
				if tc.cancel {
					cancel()
				}
				tc.handler(w, r)
			}
			var got any
			func() {
				defer func() { got = recover() }()
				serveCounted(u, routePublic, httptest.NewRecorder(), r, h)
			}()
			if got != tc.repanic {
				t.Fatalf("panic %v, want %v", got, tc.repanic)
			}
			if c := classCounts(u, routePublic); c != only(tc.class) {
				t.Fatalf("public counts %v, want %v", c, only(tc.class))
			}
			if c := classCounts(u, routePrivate); c != ([classCount]uint64{}) {
				t.Fatalf("private counts %v", c)
			}
			if timed := u.durCount.Load() == 1; timed != tc.timed {
				t.Fatalf("timed %t, want %t", timed, tc.timed)
			}
			if b := u.responseBytes.Load(); b != tc.bytes {
				t.Fatalf("bytes %d, want %d", b, tc.bytes)
			}
			if (streamsDuring == 1) != tc.stream || u.openStreams.Load() != 0 {
				t.Fatalf("open streams during %d after %d", streamsDuring, u.openStreams.Load())
			}
			days := u.swap()
			if d := days["2026-10-18"]; d == nil || d.classes[tc.class] != 1 || (d.timed == 1) != tc.timed {
				t.Fatalf("day buffer %+v", days)
			}
		})
	}
}

// A request is counted on the day its status was sent, not the day it ends.
func TestUsageCountsTheDayTheStatusWasSent(t *testing.T) {
	var now atomic.Pointer[time.Time]
	day1 := time.Date(2026, 10, 17, 23, 59, 59, 0, time.UTC)
	now.Store(&day1)
	u := newFlatUsage(func() time.Time { return *now.Load() })
	serveCounted(u, routePrivate, httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		day2 := day1.Add(time.Hour)
		now.Store(&day2)
	})
	days := u.swap()
	if days["2026-10-17"] == nil || days["2026-10-17"].classes[class2xx] != 1 || days["2026-10-17"].timed != 1 || days["2026-10-18"] != nil {
		t.Fatalf("%+v", days)
	}
}

func TestUsageWriterResponseControllerAndUpgrades(t *testing.T) {
	u := newFlatUsage(time.Now)
	// An upstream that upgrades, like a server flat's worker accepting a
	// WebSocket, behind the same kind of reverse proxy the runtime uses.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "echo" {
			// Deadlines reach the real writer through Unwrap.
			if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
				t.Errorf("deadline: %v", err)
			}
			io.WriteString(w, "plain")
			return
		}
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		io.Copy(c, brw)
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorLog = nil
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveCounted(u, routePrivate, w, r, func(w http.ResponseWriter, r *http.Request) {
			if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
				t.Errorf("deadline through Unwrap: %v", err)
			}
			proxy.ServeHTTP(w, r)
		})
	}))
	defer front.Close()

	res, err := http.Get(front.URL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	waitFor(t, "plain request", func() bool { return u.requests[routePrivate][class2xx].Load() == 1 })

	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: flat\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		t.Fatalf("upgrade: %q %v", status, err)
	}
	for line, _ := br.ReadString('\n'); line != "\r\n" && line != ""; line, _ = br.ReadString('\n') {
	}
	fmt.Fprint(conn, "ping\n")
	if echo, err := br.ReadString('\n'); err != nil || echo != "ping\n" {
		t.Fatalf("echo %q %v", echo, err)
	}
	if n := u.openStreams.Load(); n != 1 {
		t.Fatalf("open streams %d during the WebSocket", n)
	}
	if c := u.requests[routePrivate][class1xx].Load(); c != 1 {
		t.Fatalf("101 counted %d times while open", c)
	}
	conn.Close()
	waitFor(t, "stream end", func() bool { return u.openStreams.Load() == 0 })
	if n := u.durCount.Load(); n != 1 {
		t.Fatalf("timed %d: the WebSocket must stay out of response times", n)
	}
}

// The reverse proxy's 502 for a visitor who stopped waiting counts as closed.
func TestUsageProxyErrorForDepartedVisitorIsClosed(t *testing.T) {
	u := newFlatUsage(time.Now)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(release)
	target, _ := url.Parse(upstream.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorLog = nil
	var status atomic.Int64
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveCounted(u, routePrivate, w, r, func(w http.ResponseWriter, r *http.Request) {
			proxy.ServeHTTP(w, r)
			status.Store(int64(http.StatusBadGateway))
		})
	}))
	defer front.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", front.URL, nil)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("request was not cancelled")
	}
	waitFor(t, "closed request", func() bool { return u.requests[routePrivate][classClosed].Load() == 1 })
	if c := classCounts(u, routePrivate); c != only(classClosed) || status.Load() != http.StatusBadGateway {
		t.Fatalf("counts %v", c)
	}
}

// docsDemand's 503 for a visitor who stopped waiting for a wake is closed.
func TestUsageDocsWakeCancelIsClosed(t *testing.T) {
	m := &idleTestInstance{}
	d := &deployed{inst: m}
	s := &Service{stop: make(chan struct{})}
	mu := &sync.Mutex{}
	mu.Lock() // a wake in progress holds the flat's storage lock
	defer mu.Unlock()
	s.locks.Store("notes", mu)
	u := newFlatUsage(time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	rec := httptest.NewRecorder()
	serveCounted(u, routePrivate, rec, httptest.NewRequest("GET", "/", nil).WithContext(ctx), s.docsDemand("notes", d).ServeHTTP)
	if rec.Code != 503 {
		t.Fatalf("status %d", rec.Code)
	}
	if c := classCounts(u, routePrivate); c != only(classClosed) {
		t.Fatalf("counts %v", c)
	}
}

// A route revocation ends the epoch-derived context, not the visitor's: the
// public route's 503 then counts as an error, never as closed.
func TestUsageSiteHandlerCountsFlatsOwnAnswers(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	if _, err := s.CreateFlat(ctx, "plain", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	lf := s.state("plain")
	serve := func(public bool) int {
		rec := httptest.NewRecorder()
		s.siteHandler("plain", public).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		return rec.Code
	}
	if code := serve(false); code != 503 { // not deployed
		t.Fatalf("not deployed: %d", code)
	}
	if code := serve(true); code != 503 { // a Private flat on the public route
		t.Fatalf("public: %d", code)
	}
	lf.limiter = &limiter{rate: 0.001, last: time.Now()}
	if code := serve(false); code != 429 {
		t.Fatalf("rate limited: %d", code)
	}
	if c := classCounts(lf.usage, routePrivate); c[class5xx] != 1 || c[class4xx] != 1 || c[classClosed] != 0 {
		t.Fatalf("private %v", c)
	}
	if c := classCounts(lf.usage, routePublic); c != only(class5xx) {
		t.Fatalf("public %v", c)
	}
	if n := lf.usage.rateLimited.Load(); n != 1 {
		t.Fatalf("rate limited %d", n)
	}
	s.revokeRoute("public:plain")
	if code := serve(true); code != 503 {
		t.Fatalf("revoked: %d", code)
	}
	if c := classCounts(lf.usage, routePublic); c[class5xx] != 2 || c[classClosed] != 0 {
		t.Fatalf("revoked route %v", c)
	}
}

func TestUsagePreviewsAreNotCounted(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	if _, err := s.SaveVersion(ctx, "site", []bundle.File{{Path: "index.html", Data: []byte("<p>v1")}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := approvedInternalDeploy(t, s, ctx, "site", 0); err != nil {
		t.Fatal(err)
	}
	pv, err := s.OpenPreview(ctx, "site", 1)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	p := s.prevs[pv.Host]
	s.mu.Unlock()
	rec := httptest.NewRecorder()
	s.previewHandler(p).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 {
		t.Fatalf("preview %d", rec.Code)
	}
	lf := s.state("site")
	if c := classCounts(lf.usage, routePrivate); c != ([classCount]uint64{}) {
		t.Fatalf("preview counted: %v", c)
	}
	rec = httptest.NewRecorder()
	s.siteHandler("site", false).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if c := classCounts(lf.usage, routePrivate); c != only(class2xx) {
		t.Fatalf("live request: %v", c)
	}
}

// The design instance's event stream counts with its status, raises the open
// streams gauge and stays out of response times; it flushes through the
// writer's http.Flusher.
func TestUsageDesignEventStream(t *testing.T) {
	s, _ := newTestService(t)
	newDesign(t, s, "studio")
	srv := httptest.NewServer(s.siteHandler("studio", false))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/_design/api/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(res.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "event: hello") {
		t.Fatalf("%q %v", line, err)
	}
	u := s.state("studio").usage
	if u.openStreams.Load() != 1 || u.requests[routePrivate][class2xx].Load() != 1 {
		t.Fatalf("streams %d 2xx %d", u.openStreams.Load(), u.requests[routePrivate][class2xx].Load())
	}
	cancel()
	res.Body.Close()
	waitFor(t, "stream end", func() bool { return u.openStreams.Load() == 0 })
	if u.durCount.Load() != 0 {
		t.Fatal("event stream was timed")
	}
}

// HTTP/2 extended CONNECT WebSockets stay disabled (Go keeps them off unless
// GODEBUG=http2xconnect=1), so upgrades only come through Hijack.
func TestNoHTTP2ExtendedConnect(t *testing.T) {
	if strings.Contains(os.Getenv("GODEBUG"), "http2xconnect") {
		t.Fatal("GODEBUG enables http2xconnect")
	}
	root := filepath.Join("..", "..")
	for _, p := range []string{"go.mod", "netbird/go.mod", "cmd/flats", "netbird/cmd/flats"} {
		_ = filepath.Walk(filepath.Join(root, p), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !(strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "go.mod")) {
				return nil
			}
			b, _ := os.ReadFile(path)
			if strings.Contains(string(b), "http2xconnect") {
				t.Errorf("%s sets http2xconnect", path)
			}
			return nil
		})
	}
}

// --- flush ---

func TestUsageFlushNeverBlocksRequests(t *testing.T) {
	ctx := context.Background()
	s, dir := newTestService(t)
	if _, err := s.SaveVersion(ctx, "site", []bundle.File{{Path: "index.html", Data: []byte("<p>v1")}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := approvedInternalDeploy(t, s, ctx, "site", 0); err != nil {
		t.Fatal(err)
	}
	h := s.siteHandler("site", false)
	get := func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != 200 {
			t.Fatalf("status %d", rec.Code)
		}
	}
	for range 3 {
		get()
	}
	lf := s.state("site")
	f, _ := s.st.GetFlat(ctx, "site")
	// Another connection holds the write lock: the flush waits on SQLite.
	db, err := sql.Open("sqlite", filepath.Join(dir, "flats.db")+"?_pragma=busy_timeout(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		fctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		s.flushViews(fctx, "site", lf)
		s.flushUsage(fctx, lf)
	}()
	time.Sleep(100 * time.Millisecond)
	served := make(chan struct{})
	go func() { get(); close(served) }()
	select {
	case <-served:
	case <-flushed:
		t.Fatal("the flush ended before the concurrent request was served")
	case <-time.After(time.Second):
		t.Fatal("a request waited for the blocked flush")
	}
	<-flushed
	if rows, _ := s.st.UsageSince(ctx, f.ID, ""); len(rows) != 0 {
		t.Fatalf("blocked flush wrote %+v", rows)
	}
	conn.ExecContext(ctx, "ROLLBACK")
	s.flushUsage(ctx, lf)
	s.flushUsage(ctx, lf)
	rows, err := s.st.UsageSince(ctx, f.ID, "")
	if err != nil || len(rows) != 1 || rows[0].C2xx != 4 || rows[0].Timed != 4 || rows[0].Requests() != 4 {
		t.Fatalf("after retry %+v %v", rows, err)
	}
}

// --- worker CPU and memory ---

func TestWorkerUsageExitSeenExactlyOnce(t *testing.T) {
	w := newWorkerUsage(time.Now)
	const procs = 50
	var stop atomic.Bool
	errs := make(chan error, 1)
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			last := int64(0)
			for !stop.Load() {
				n := w.cpuNS()
				if n < last {
					select {
					case errs <- fmt.Errorf("total dipped from %d to %d", last, n):
					default:
					}
					return
				}
				last = n
			}
		}()
	}
	var writers sync.WaitGroup
	for i := range procs {
		writers.Add(1)
		go func(id uint64) {
			defer writers.Done()
			w.Started(id)
			for c := int64(1); c <= 10; c++ {
				w.Sample(id, c*1000, 10, true)
			}
			// The final CPU is at least the last sample.
			w.Exited(id, 12_000, id%2 == 0)
		}(uint64(i + 1))
	}
	writers.Wait()
	stop.Store(true)
	readers.Wait()
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
	if got := w.cpuNS(); got != procs*12_000 {
		t.Fatalf("total %d, want %d", got, procs*12_000)
	}
	if w.unexpectedExits() != procs/2 {
		t.Fatalf("unexpected exits %d", w.unexpectedExits())
	}
	if m := w.memory(); m.workers != 0 || m.bytes != 0 || m.at.IsZero() {
		t.Fatalf("memory after every exit %+v", m)
	}
	// A final CPU below the last sample never lowers the total; samples after
	// the exit or of unknown processes are ignored.
	w.Started(100)
	w.Sample(100, 5000, 1, true)
	w.Exited(100, 10, false)
	w.Sample(100, 9_000_000, 1, true)
	w.Sample(999, 9_000_000, 1, true)
	if got := w.cpuNS(); got != procs*12_000+5000 {
		t.Fatalf("total %d", got)
	}
}

func TestWorkerMemoryIsTheRunningSum(t *testing.T) {
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	clock := &now
	w := newWorkerUsage(func() time.Time { return *clock })
	w.Started(1)
	w.Started(2)
	w.Sample(1, 1, 100, true)
	later := now.Add(10 * time.Second)
	clock = &later
	w.Sample(2, 1, 50, true)
	if m := w.memory(); m.bytes != 150 || m.workers != 2 || !m.at.Equal(now) {
		t.Fatalf("%+v", m)
	}
	if p, ok := w.takePeak(); !ok || p != 150 {
		t.Fatal(p, ok)
	}
	if _, ok := w.takePeak(); ok {
		t.Fatal("peak not reset")
	}
	w.Exited(1, 2, false)
	if m := w.memory(); m.bytes != 50 || m.workers != 1 {
		t.Fatalf("%+v", m)
	}
	w.Exited(2, 2, false)
	if m := w.memory(); m.bytes != 0 || m.workers != 0 || !m.at.Equal(later) {
		t.Fatalf("a stopped worker holds no memory: %+v", m)
	}
}

// usageRuntime is a fake runtime whose processes report through the spec's
// usage sink: 1 s of CPU at start and 2 s when stopped.
type usageRuntime struct {
	mu    sync.Mutex
	next  uint64
	specs []RuntimeSpec
}

type usageInstance struct {
	http.Handler
	id    uint64
	usage UsageSink
	once  sync.Once
	state WorkerState
}

func (i *usageInstance) Stop() {
	i.once.Do(func() {
		if i.usage != nil {
			i.usage.Exited(i.id, 2e9, false)
		}
	})
}

func (i *usageInstance) WorkerState() WorkerState { return i.state }

func (r *usageRuntime) Start(_ context.Context, spec RuntimeSpec) (Instance, error) {
	r.mu.Lock()
	r.next++
	id := r.next
	r.specs = append(r.specs, spec)
	r.mu.Unlock()
	if spec.Usage != nil {
		spec.Usage.Started(id)
		spec.Usage.Sample(id, 1e9, 1<<20, true)
	}
	return &usageInstance{id: id, usage: spec.Usage, state: WorkerRunning, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })}, nil
}

func (r *usageRuntime) since(n int) []RuntimeSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RuntimeSpec(nil), r.specs[n:]...)
}

func (r *usageRuntime) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.specs)
}

var serverFiles = []bundle.File{{Path: "flats.json", Data: []byte(`{"kind":"server"}`)}, {Path: "server.js", Data: []byte("export default {}")}}

func deployServer(t *testing.T, s *Service, slug string) {
	t.Helper()
	if _, err := s.SaveVersion(context.Background(), slug, serverFiles, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := approvedInternalDeploy(t, s, context.Background(), slug, 0); err != nil {
		t.Fatal(err)
	}
}

func metricsOf(t *testing.T, s *Service, slug string) FlatMetrics {
	t.Helper()
	for _, f := range s.Metrics().Flats {
		if f.Slug == slug {
			return f
		}
	}
	t.Fatalf("no series for %s in %+v", slug, s.Metrics().Flats)
	return FlatMetrics{}
}

func roleCPU(s *Service, slug, role string) float64 {
	for _, r := range s.Metrics().RoleCPU {
		if r.Slug == slug && r.Role == role {
			return r.CPUSeconds
		}
	}
	return 0
}

// Every build passes the sink of its role: activation checks and restore
// trials the check sink, previews the preview sink, and live builds the
// flat's live sink, including host restore, restarts and both rename paths.
func TestUsageSinkOfEveryBuild(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	open := func() (*Service, *store.Store, *usageRuntime) {
		st, err := store.Open(filepath.Join(dir, "flats.db"))
		if err != nil {
			t.Fatal(err)
		}
		rt := &usageRuntime{}
		s, err := New(ctx, Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Runtime: rt, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		return s, st, rt
	}
	s, st, rt := open()
	sinkIs := func(what string, spec RuntimeSpec, want UsageSink) {
		t.Helper()
		if spec.Usage != want {
			t.Fatalf("%s (%s) reported to the wrong sink", what, spec.DataDir)
		}
	}
	deployServer(t, s, "app")
	lf := s.state("app")
	specs := rt.since(0)
	if len(specs) != 2 {
		t.Fatalf("%d starts", len(specs))
	}
	sinkIs("activation check", specs[0], lf.checkSink())
	sinkIs("live activation", specs[1], lf.liveSink())

	n := rt.count()
	if _, err := s.OpenPreview(ctx, "app", 1); err != nil {
		t.Fatal(err)
	}
	sinkIs("version preview", rt.since(n)[0], lf.previewSink())

	n = rt.count()
	if _, err := s.RenameSlug(ctx, "app", "renamed", ViaAPI); err != nil {
		t.Fatal(err)
	}
	if got := rt.since(n); len(got) != 1 || got[0].Flat != "renamed" {
		t.Fatalf("rename rebuild %+v", got)
	} else {
		sinkIs("rename rebuild", got[0], lf.liveSink())
	}
	s.mu.Lock()
	if s.live["renamed"] != lf || s.live["app"] != nil {
		t.Fatal("rename moved another state")
	}
	s.mu.Unlock()

	n = rt.count()
	cur := lf.cur.Load()
	lf.cur.Store(nil)
	if err := s.restart(ctx, "renamed", cur); err != nil {
		t.Fatal(err)
	}
	sinkIs("restart", rt.since(n)[0], lf.liveSink())

	n = rt.count()
	f, _ := s.st.GetFlat(ctx, "renamed")
	v, _ := s.st.GetVersion(ctx, "renamed", 1)
	if err := s.trialRun(ctx, f, v, filepath.Join(t.TempDir(), "none.db"), cur.environment); err != nil {
		t.Fatal(err)
	}
	sinkIs("restore trial", rt.since(n)[0], lf.checkSink())

	// Host restore after a restart of Flats.
	if _, err := s.OpenPreview(ctx, "renamed", 1); err != nil {
		t.Fatal(err)
	}
	s.Close()
	st.Close()
	s, st, rt = open()
	defer func() { s.Close(); st.Close() }()
	lf = s.state("renamed")
	var live, preview int
	for _, spec := range rt.since(0) {
		switch spec.Usage {
		case lf.liveSink():
			live++
		case lf.previewSink():
			preview++
		default:
			t.Fatalf("restored build (%s) reported to the wrong sink", spec.DataDir)
		}
	}
	if live != 1 || preview != 1 {
		t.Fatalf("restore: %d live, %d preview", live, preview)
	}
	if lf.uidOf() != f.ID {
		t.Fatal("restore did not bind the flat ID")
	}
}

// A rename, a restart, a version switch and preview and trial workers keep
// the flat's live CPU monotonic and on the flat; preview and check CPU land in
// their roles only.
func TestUsageCPUFollowsTheFlat(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	rt := &usageRuntime{}
	s.cfg.Runtime = rt
	deployServer(t, s, "app")
	m := metricsOf(t, s, "app")
	if !m.HasWorker || m.CPUSeconds != 1 || m.State != WorkerRunning || !m.Traffic {
		t.Fatalf("after activation %+v", m)
	}
	if got := roleCPU(s, "app", UsageRoleCheck); got != 2 {
		t.Fatalf("check CPU %v", got)
	}
	last := m.CPUSeconds
	step := func(what string) {
		t.Helper()
		slug := "app"
		if _, err := s.st.GetFlat(ctx, "app"); err != nil {
			slug = "renamed"
		}
		m := metricsOf(t, s, slug)
		if m.CPUSeconds < last {
			t.Fatalf("%s: CPU fell from %v to %v", what, last, m.CPUSeconds)
		}
		last = m.CPUSeconds
	}
	if _, err := s.OpenPreview(ctx, "app", 1); err != nil {
		t.Fatal(err)
	}
	step("preview")
	if roleCPU(s, "app", UsageRolePreview) != 1 || metricsOf(t, s, "app").CPUSeconds != 1 {
		t.Fatal("preview CPU left its role")
	}
	if err := s.dropPreviews(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameSlug(ctx, "app", "renamed", ViaAPI); err != nil {
		t.Fatal(err)
	}
	step("rename")
	// The stopped worker's final 2 s plus the rebuilt one's first 1 s.
	if m := metricsOf(t, s, "renamed"); m.CPUSeconds != 3 {
		t.Fatalf("after rename %v", m.CPUSeconds)
	}
	for _, f := range s.Metrics().Flats {
		if f.Slug == "app" && f.Traffic {
			t.Fatal("old slug kept traffic series")
		}
	}
	// A version switch: the old version drains while the new one runs.
	if _, err := s.SaveVersion(ctx, "renamed", append(serverFiles[:1:1], bundle.File{Path: "server.js", Data: []byte("export default {} // 2")}), SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := approvedInternalDeploy(t, s, ctx, "renamed", 0); err != nil {
		t.Fatal(err)
	}
	step("version switch")
	if m := metricsOf(t, s, "renamed"); m.CPUSeconds != 4 {
		t.Fatalf("while the old version drains %v", m.CPUSeconds)
	}
	if got := roleCPU(s, "renamed", UsageRoleCheck); got != 4 {
		t.Fatalf("check CPU after two activations %v", got)
	}
}

// Shutdown persists usage after the workers stopped, so their final CPU
// reaches flats.db.
func TestUsageShutdownPersistsFinalCPU(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := New(ctx, Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Runtime: &usageRuntime{}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	deployServer(t, s, "app")
	f, _ := st.GetFlat(ctx, "app")
	s.Sweep(ctx)
	if rows, _ := st.UsageSince(ctx, f.ID, ""); len(rows) != 1 || rows[0].CPUMS != 1000 {
		t.Fatalf("sweep %+v", rows)
	}
	s.Close()
	rows, err := st.UsageSince(ctx, f.ID, "")
	if err != nil || len(rows) != 1 || rows[0].CPUMS != 2000 {
		t.Fatalf("after shutdown %+v %v", rows, err)
	}
}

type sleepingDocs struct{ *idleTestInstance }

func (sleepingDocs) WorkerState() WorkerState { return WorkerSleeping }

// Metrics and stats never wake a sleeping document, which stays running in
// the flat view while its worker state is sleeping.
func TestUsageSleepingDocumentStaysAsleep(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	if _, err := s.CreateFlat(ctx, "notes", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	m := &idleTestInstance{}
	m.wake = func(context.Context, func(context.Context, http.Handler) error) (RuntimeLease, error) {
		t.Error("woken")
		return nil, errors.New("must not wake")
	}
	inst := sleepingDocs{m}
	d := &deployed{version: store.Version{Number: 1, Kind: "server", Manifest: []byte(`{"type":"docs"}`)}, inst: inst, redact: noRedact}
	d.handler = s.docsDemand("notes", d)
	s.state("notes").cur.Store(d)
	s.Sweep(ctx)
	fm := metricsOf(t, s, "notes")
	if fm.Type != "docs" || !fm.HasWorker || fm.State != WorkerSleeping {
		t.Fatalf("%+v", fm)
	}
	rep, err := s.Usage(ctx, "notes", 30)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Now.State != WorkerSleeping || rep.Now.CPU != UsageMeasured || rep.Now.Workers == nil || *rep.Now.Workers != 0 {
		t.Fatalf("%+v", rep.Now)
	}
	if v, err := s.GetFlat(ctx, "notes"); err != nil || !v.Running {
		t.Fatalf("running %+v %v", v.Running, err)
	}
	if m.wakes != 0 || len(m.foreground) != 0 {
		t.Fatalf("wakes %d acquires %d", m.wakes, len(m.foreground))
	}
	for _, c := range s.Metrics().Counts {
		if c.Type == "docs" && !c.Running {
			t.Fatalf("a sleeping document counted as not running: %+v", c)
		}
	}
}

// Metrics reads memory only: it reports complete figures with the store
// closed.
func TestMetricsDoesNoStoreQuery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Runtime: &usageRuntime{}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	deployServer(t, s, "app")
	if _, err := s.SaveVersion(ctx, "site", []bundle.File{{Path: "index.html", Data: []byte("<p>")}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFlat(ctx, "empty", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetVisibility(ctx, "app", store.Public, ViaAPI, "test"); err != nil && !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.siteHandler("app", false).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	s.Sweep(ctx)
	before := s.Metrics()
	st.Close()
	after := s.Metrics()
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("figures changed without the store:\n%+v\n%+v", before, after)
	}
	if len(after.Flats) != 3 || after.PendingApprovals != 1 || after.DBBytes == 0 || after.SnapshotAt.IsZero() {
		t.Fatalf("%+v", after)
	}
	var app FlatMetrics
	for _, f := range after.Flats {
		if f.Slug == "app" {
			app = f
		}
		if f.DiskMeasuredAt.IsZero() {
			t.Fatalf("%s has no disk figure", f.Slug)
		}
	}
	if app.Requests[RoutePrivate][Class2xx] != 1 || app.DurationCount != 1 || len(app.DurationCounts) != len(UsageBuckets)+1 || !app.HasWorker || app.CPUSeconds != 1 {
		t.Fatalf("app %+v", app)
	}
	total := 0
	for _, c := range after.Counts {
		total += c.N
	}
	if total != 3 {
		t.Fatalf("counts %+v", after.Counts)
	}
}

func TestUsageReport(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	s.cfg.Runtime = &usageRuntime{}
	deployServer(t, s, "app")
	if _, err := s.SaveVersion(ctx, "site", []bundle.File{{Path: "index.html", Data: []byte("<p>")}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := approvedInternalDeploy(t, s, ctx, "site", 0); err != nil {
		t.Fatal(err)
	}
	f, _ := s.st.GetFlat(ctx, "app")
	today := s.now().UTC().Format(usageDayLayout)
	old := s.now().UTC().AddDate(0, 0, -40).Format(usageDayLayout)
	// 100 requests: 90 in the 25-50 ms bucket, 10 in 250-500 ms; one 5xx.
	buckets := make([]int64, usageBucketCount)
	buckets[3], buckets[6] = 90, 10
	if _, err := s.st.AddUsage(ctx, f.ID, []store.UsageDay{
		{Day: today, C2xx: 98, C5xx: 1, Closed: 1, RateLimited: 2, Timed: 100, DurationMS: 5000, LatencyVersion: 1, Latency: buckets, CPUMS: 1500, MemoryPeak: 1 << 20},
		{Day: old, C2xx: 1000},
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Usage(ctx, "app", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Days) != 1 || rep.Days[0].Day != today || rep.Days[0].Requests != 100 || rep.Days[0].Status[Class5xx] != 1 ||
		rep.Days[0].Status[Class1xx] != 0 || rep.Days[0].CPUSeconds == nil {
		t.Fatalf("days %+v", rep.Days)
	}
	tot := rep.Totals
	if tot.Requests != 100 || tot.Errors != 1 || tot.ErrorRate == nil || *tot.ErrorRate != 0.01 || tot.P50MS == nil || tot.P95MS == nil {
		t.Fatalf("totals %+v", tot)
	}
	// p50: rank 50 of 90 within 25-50 ms; p95: rank 95, 5 of 10 within 250-500 ms.
	if *tot.P50MS != 38.9 || *tot.P95MS != 375 {
		t.Fatalf("p50 %v p95 %v", *tot.P50MS, *tot.P95MS)
	}
	// The activation's sampled second was flushed with the read.
	if *tot.CPUSeconds != 2.5 || *tot.MemoryPeakBytes != 1<<20 {
		t.Fatalf("cpu %v mem %v", *tot.CPUSeconds, *tot.MemoryPeakBytes)
	}
	if rep.Now.CPU != UsageMeasured || rep.Now.State != WorkerRunning || *rep.Now.Workers != 1 {
		t.Fatalf("now %+v", rep.Now)
	}
	if memoryMeasured != (rep.Now.Memory == UsageMeasured) || (!memoryMeasured && rep.Now.Memory != UsageUnsupported) {
		t.Fatalf("memory %q", rep.Now.Memory)
	}

	site, err := s.Usage(ctx, "site", 7)
	if err != nil {
		t.Fatal(err)
	}
	if site.Now.CPU != UsageNotSeparate || site.Now.Memory != UsageNotSeparate || site.Now.State != "" || site.Now.Workers != nil ||
		site.Totals.ErrorRate != nil || site.Totals.P50MS != nil || site.Totals.CPUSeconds != nil || len(site.Days) != 0 {
		t.Fatalf("website %+v", site)
	}
	if _, err := s.Usage(ctx, "missing", 7); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestUsageQuantile(t *testing.T) {
	for _, tc := range []struct {
		q      float64
		counts []int64
		want   float64
		ok     bool
	}{
		{0.5, make([]int64, 13), 0, false},
		{0.5, []int64{2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, 2.5, true},
		{0.95, []int64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 4}, 30000, true}, // +Inf: the highest finite bound
		{1, []int64{1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, 10, true},
	} {
		got, ok := usageQuantile(tc.q, tc.counts)
		if got != tc.want || ok != tc.ok {
			t.Errorf("q%v %v: %v %t", tc.q, tc.counts, got, ok)
		}
	}
}

// A deleted flat's usage disappears and a recreated slug starts empty, even
// when the old state is flushed late.
func TestUsageDeleteAndRecreateStartsEmpty(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	if _, err := s.SaveVersion(ctx, "site", []bundle.File{{Path: "index.html", Data: []byte("<p>")}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := approvedInternalDeploy(t, s, ctx, "site", 0); err != nil {
		t.Fatal(err)
	}
	old := s.state("site")
	s.siteHandler("site", false).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	s.Sweep(ctx)
	old.usage.countRequest(routePrivate, class2xx, old.usage.today())
	if _, err := s.Delete(ctx, "site", ViaConsole, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveVersion(ctx, "site", []bundle.File{{Path: "index.html", Data: []byte("<p>new")}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	s.flushUsage(ctx, old) // a late flush of the deleted flat's state
	rep, err := s.Usage(ctx, "site", 30)
	if err != nil || len(rep.Days) != 0 || rep.Totals.Requests != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	if lf := s.state("site"); lf == old || lf.uidOf() == old.uidOf() {
		t.Fatal("the recreated flat kept the old state")
	}
}
