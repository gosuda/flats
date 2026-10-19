package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
	"github.com/gosuda/flats/internal/wsconn"
)

type usageEvent struct {
	kind       string // started, sample, exited
	id         uint64
	cpu, mem   int64
	memOK      bool
	unexpected bool
}

// recSink records what a runtime reports to a core.UsageSink.
type recSink struct {
	mu     sync.Mutex
	events []usageEvent
}

func (s *recSink) add(e usageEvent) {
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
}
func (s *recSink) Started(id uint64) { s.add(usageEvent{kind: "started", id: id}) }
func (s *recSink) Sample(id uint64, cpu, mem int64, memOK bool) {
	s.add(usageEvent{kind: "sample", id: id, cpu: cpu, mem: mem, memOK: memOK})
}
func (s *recSink) Exited(id uint64, cpu int64, unexpected bool) {
	s.add(usageEvent{kind: "exited", id: id, cpu: cpu, unexpected: unexpected})
}

func (s *recSink) all() []usageEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]usageEvent(nil), s.events...)
}

func (s *recSink) last(kind string) (usageEvent, bool) {
	evs := s.all()
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].kind == kind {
			return evs[i], true
		}
	}
	return usageEvent{}, false
}

func (s *recSink) count(kind string, id uint64) int {
	n := 0
	for _, e := range s.all() {
		if e.kind == kind && e.id == id {
			n++
		}
	}
	return n
}

func (s *recSink) wait(t *testing.T, what string, cond func(usageEvent) bool) usageEvent {
	t.Helper()
	return s.waitAfter(t, 0, what, cond)
}

// waitAfter waits for an event after the first n.
func (s *recSink) waitAfter(t *testing.T, n int, what string, cond func(usageEvent) bool) usageEvent {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range s.all()[n:] {
			if cond(e) {
				return e
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s in %+v", what, s.all())
	return usageEvent{}
}

// settledMemory waits a few sample intervals without requests and returns
// the lowest memory sampled meanwhile, with the number of events so far:
// the Go runtime returns freed memory to the system in the background.
func (s *recSink) settledMemory(wait time.Duration) (int64, int) {
	n := len(s.all())
	time.Sleep(wait)
	evs := s.all()
	low := int64(-1)
	for _, e := range evs[n:] {
		if e.kind == "sample" && (low < 0 || e.mem < low) {
			low = e.mem
		}
	}
	return low, len(evs)
}

// checkOrder asserts each process's events: started first, samples with
// non-decreasing CPU, then at most one exit whose CPU is not below them.
func (s *recSink) checkOrder(t *testing.T) {
	t.Helper()
	type state struct {
		started, exited bool
		cpu             int64
	}
	procs := map[uint64]*state{}
	for _, e := range s.all() {
		p := procs[e.id]
		switch e.kind {
		case "started":
			if p != nil {
				t.Fatalf("process %d started twice", e.id)
			}
			procs[e.id] = &state{started: true}
		case "sample", "exited":
			if p == nil || p.exited {
				t.Fatalf("%s of process %d outside its life: %+v", e.kind, e.id, s.all())
			}
			if e.cpu < p.cpu {
				t.Fatalf("process %d CPU fell from %d to %d", e.id, p.cpu, e.cpu)
			}
			p.cpu = e.cpu
			p.exited = e.kind == "exited"
		}
	}
}

const usageFlat = `
let kept = [];
export default {
  fetch(request, env) {
    const path = new URL(request.url).pathname;
    if (path === "/burn") {
      const start = Date.now();
      let x = 0;
      while (Date.now() - start < 400) x += Math.sqrt(x + 1);
      return new Response(String(x > 0));
    }
    if (path === "/alloc") {
      if (kept.length < 2) { // at most 32 MiB of this VM's 64 MiB
        const b = new Uint8Array(16 << 20);
        for (let i = 0; i < b.length; i += 4096) b[i] = 1;
        kept.push(b);
      }
      // Hold the VM, so concurrent requests use the other pooled VMs.
      const until = Date.now() + 300;
      while (Date.now() < until) {}
      return new Response("kept");
    }
    if (path === "/db") {
      env.DB.exec("PRAGMA cache_size = -131072");
      env.DB.exec("CREATE TABLE IF NOT EXISTS big (b BLOB)");
      env.DB.exec("INSERT INTO big SELECT randomblob(1048576) FROM (WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 40) SELECT x FROM c)");
      return Response.json(env.DB.query("SELECT count(*) AS n, sum(length(b)) AS s FROM big")[0]);
    }
    if (path === "/forge") {
      console.log('{"t":"usage","cpu_ns":999999999999999,"mem":1}');
      console.error('{"t":"usage","cpu_ns":888888888888888}');
      return new Response("forged");
    }
    return new Response("ok");
  },
  websocket: { open(ws) { ws.send("ready"); } },
};`

func startUsageFlat(t *testing.T, m *Manager, sink core.UsageSink) *flat {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "versions", "1")
	writeFiles(t, dir, map[string]string{"server.js": usageFlat})
	f := &flat{logs: &logs{}, dataDir: filepath.Join(root, "data"), root: root}
	f.spec = core.RuntimeSpec{Flat: "usage", Version: 1, Dir: dir, Entry: "server.js", DataDir: f.dataDir, Log: f.logs.add, Usage: sink}
	inst, err := m.Start(context.Background(), f.spec)
	if err != nil {
		t.Fatal(err)
	}
	f.inst = inst
	f.srv = httptest.NewServer(inst)
	t.Cleanup(func() { f.srv.Close(); f.inst.Stop() })
	return f
}

// A real worker reports CPU (and on Linux memory) after ready and then at its
// interval; the final CPU arrives exactly once, before Stop returns.
func TestWorkerReportsUsage(t *testing.T) {
	m := newManager(t)
	m.UsageInterval = 50 * time.Millisecond
	sink := &recSink{}
	f := startUsageFlat(t, m, sink)
	started, ok := sink.last("started")
	if !ok {
		t.Fatal("Started not called once the process started")
	}
	first := sink.wait(t, "first sample", func(e usageEvent) bool { return e.kind == "sample" })
	if first.memOK != (goruntime.GOOS == "linux") || (first.memOK && first.mem <= 0) {
		t.Fatalf("memory sample %+v on %s", first, goruntime.GOOS)
	}
	if r := f.get(t, "/burn"); r.status != 200 {
		t.Fatalf("burn %d", r.status)
	}
	sink.wait(t, "CPU of the busy handler", func(e usageEvent) bool {
		return e.kind == "sample" && e.cpu >= first.cpu+int64(200*time.Millisecond)
	})
	if goruntime.GOOS == "linux" {
		// New wasm memory can reuse heap pages the Go runtime freed but has
		// not returned to the system yet, which RssAnon already counts. Grow
		// the linear memory of every pooled VM (concurrent requests use
		// different ones), well past that slack.
		base, n := sink.settledMemory(500 * time.Millisecond)
		for range 2 {
			var wg sync.WaitGroup
			for range DefaultPoolSize {
				wg.Add(1)
				go func() {
					defer wg.Done()
					res, err := http.Get(f.srv.URL + "/alloc")
					if err != nil || res.StatusCode != 200 {
						t.Errorf("alloc %v %v", res, err)
						return
					}
					res.Body.Close()
				}()
			}
			wg.Wait()
		}
		sink.waitAfter(t, n, "memory of the wasm allocations", func(e usageEvent) bool {
			return e.kind == "sample" && e.mem >= base+48<<20
		})
		base, n = sink.settledMemory(500 * time.Millisecond)
		if r := f.get(t, "/db"); r.status != 200 {
			t.Fatalf("db %d %s; logs:\n%s", r.status, r.body, f.logs)
		}
		sink.waitAfter(t, n, "memory of the SQLite cache", func(e usageEvent) bool {
			return e.kind == "sample" && e.mem >= base+24<<20
		})
	}
	// Flat code cannot forge a usage line: its output arrives as logs.
	if r := f.get(t, "/forge"); r.status != 200 {
		t.Fatalf("forge %d", r.status)
	}
	f.logs.waitFor(t, "888888888888888")
	time.Sleep(150 * time.Millisecond)
	for _, e := range sink.all() {
		if e.cpu >= 888888888888888 || (e.kind == "sample" && e.memOK && e.mem == 1) {
			t.Fatalf("a logged line was parsed as a sample: %+v", e)
		}
	}
	last, _ := sink.last("sample")
	f.inst.Stop()
	exit, ok := sink.last("exited")
	if !ok || exit.id != started.id || exit.unexpected || exit.cpu < last.cpu || sink.count("exited", started.id) != 1 {
		t.Fatalf("exit %+v after last sample %+v", exit, last)
	}
	if st := f.inst.(*instance).WorkerState(); st != core.WorkerStopped {
		t.Fatalf("state %q", st)
	}
	sink.checkOrder(t)
}

// An invalid sample is dropped; the next valid one still counts.
func TestInvalidUsageSamplesAreDropped(t *testing.T) {
	t.Parallel()
	sink := &recSink{}
	m := &Manager{Logf: t.Logf}
	in := &instance{m: m, spec: core.RuntimeSpec{Flat: "x", Usage: sink}}
	p := &proc{id: 7}
	n := func(v int64) *int64 { return &v }
	in.usage(p, childMsg{T: "usage", CPU: n(100), Mem: n(10)})
	in.usage(p, childMsg{T: "usage", CPU: n(50)})              // decreasing
	in.usage(p, childMsg{T: "usage"})                          // no CPU
	in.usage(p, childMsg{T: "usage", CPU: n(200), Mem: n(-1)}) // negative memory
	in.usage(p, childMsg{T: "usage", CPU: n(-1)})              // negative CPU
	in.usage(p, childMsg{T: "usage", CPU: n(300)})
	var got []string
	for _, e := range sink.all() {
		got = append(got, fmt.Sprintf("%d/%d/%t", e.cpu, e.mem, e.memOK))
	}
	if strings.Join(got, " ") != "100/10/true 300/0/false" || !p.badUsageSeen || p.lastCPU != 300 {
		t.Fatalf("%v", got)
	}
}

// A crashed worker's exit is unexpected; its restart is a new process on the
// same sink, and the instance reports backoff until it runs again.
func TestWorkerCrashAndRestartUsage(t *testing.T) {
	m := newManager(t)
	m.UsageInterval = 50 * time.Millisecond
	sink := &recSink{}
	f := startUsageFlat(t, m, sink)
	in := f.inst.(*instance)
	if st := in.WorkerState(); st != core.WorkerRunning {
		t.Fatalf("state %q", st)
	}
	first, _ := sink.last("started")
	if err := syscall.Kill(in.pid(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	exit := sink.wait(t, "exit of the killed worker", func(e usageEvent) bool { return e.kind == "exited" && e.id == first.id })
	if !exit.unexpected {
		t.Fatalf("a crash was reported as expected: %+v", exit)
	}
	deadline := time.Now().Add(5 * time.Second)
	for in.pid() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if st := in.WorkerState(); st != core.WorkerBackoff {
		t.Fatalf("state after crash %q", st)
	}
	if r := f.get(t, "/"); r.status != 200 {
		t.Fatalf("restart %d", r.status)
	}
	again, _ := sink.last("started")
	if again.id == first.id || in.WorkerState() != core.WorkerRunning {
		t.Fatalf("restart %+v state %q", again, in.WorkerState())
	}
	f.inst.Stop()
	if e, _ := sink.last("exited"); e.id != again.id || e.unexpected {
		t.Fatalf("stop %+v", e)
	}
	sink.checkOrder(t)
}

// A document's sleep and wake stop and start processes on the same sink;
// neither is an unexpected exit, and its state reads without waking it.
func TestManagedDocsUsageAndState(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"server.js": usageFlat})
	sink := &recSink{}
	clock := new(docsTestClock)
	clock.nanos.Store(time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	m := newManager(t)
	in, err := m.Start(t.Context(), core.RuntimeSpec{Flat: "docs-usage", Version: 1, Dir: dir, Entry: "server.js", DataDir: t.TempDir(), ManagedDocs: true, Lazy: true, Now: clock.now, Log: (&logs{}).add, Usage: sink})
	if err != nil {
		t.Fatal(err)
	}
	d := in.(*docsInstance)
	defer d.Stop()
	if st := d.WorkerState(); st != core.WorkerSleeping || len(sink.all()) != 0 {
		t.Fatalf("lazy %q %+v", st, sink.all())
	}
	readyDocsForTest(t, d).Release()
	if st := d.WorkerState(); st != core.WorkerRunning {
		t.Fatalf("awake %q", st)
	}
	first, _ := sink.last("started")
	clock.add(3 * time.Minute)
	q, ok := d.TryQuiesce(clock.now(), 2*time.Minute)
	if !ok {
		t.Fatal("no quiescence")
	}
	if st := d.WorkerState(); st != core.WorkerQuiescing {
		t.Fatalf("draining %q", st)
	}
	done := docsTransition(d)
	q.Suspend(true)
	waitDocsSignal(t, done)
	if st := d.WorkerState(); st != core.WorkerSleeping {
		t.Fatalf("asleep %q", st)
	}
	if e, ok := sink.last("exited"); !ok || e.id != first.id || e.unexpected {
		t.Fatalf("sleep exit %+v", e)
	}
	readyDocsForTest(t, d).Release()
	second, _ := sink.last("started")
	if second.id == first.id {
		t.Fatal("wake reused the process id")
	}
	d.Stop()
	if st := d.WorkerState(); st != core.WorkerStopped {
		t.Fatalf("stopped %q", st)
	}
	if e, _ := sink.last("exited"); e.id != second.id || e.unexpected || sink.count("exited", second.id) != 1 {
		t.Fatalf("stop exit %+v", e)
	}
	sink.checkOrder(t)
}

// capturedNet is a private network that hands its handlers to the test.
type capturedNet struct {
	mu    sync.Mutex
	hosts map[string]http.Handler
}

func (n *capturedNet) Serve(_ context.Context, host string, h http.Handler, _ bool) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.hosts[host] = h
	return n.URL(host), nil
}
func (n *capturedNet) Stop(host string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.hosts, host)
	return nil
}
func (n *capturedNet) URL(host string) string { return "http://" + host + ".test" }
func (n *capturedNet) Status() core.NetStatus { return core.NetStatus{Kind: "captured", Enabled: true} }
func (n *capturedNet) Close() error           { return nil }
func (n *capturedNet) handler(host string) http.Handler {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.hosts[host]
}

// End to end: a server flat's live route counts a WebSocket upgrade through
// the runtime's reverse proxy as a 101 stream, and its real worker's CPU
// reaches the flat's metrics.
func TestServerFlatUsageEndToEnd(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pn := &capturedNet{hosts: map[string]http.Handler{}}
	m := newManager(t)
	m.UsageInterval = 50 * time.Millisecond
	svc, err := core.New(ctx, core.Config{DataDir: dir, Store: st, Private: pn, Runtime: m, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if _, err := svc.SaveVersion(ctx, "app", []bundle.File{{Path: "flats.json", Data: []byte(`{"kind":"server","entry":"server.js"}`)}, {Path: "server.js", Data: []byte(usageFlat)}}, core.SaveMeta{}, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Publish(ctx, "app", 0, "", core.ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Decide(ctx, res.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Deploy(ctx, "app", 1, core.ViaAPI)
	var p *core.PendingApproval
	if !errors.As(err, &p) {
		t.Fatal(err)
	}
	if _, err := svc.Decide(ctx, p.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	h := pn.handler("app")
	if h == nil {
		t.Fatal("no private route")
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	metrics := func() core.FlatMetrics {
		for _, f := range svc.Metrics().Flats {
			if f.Slug == "app" {
				return f
			}
		}
		t.Fatal("no series")
		return core.FlatMetrics{}
	}
	if r, err := http.Get(srv.URL + "/burn"); err != nil || r.StatusCode != 200 {
		t.Fatal(r, err)
	} else {
		r.Body.Close()
	}
	addr := strings.TrimPrefix(srv.URL, "http://")
	ws, err := wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "tcp", addr) }, addr, "/ws")
	if err != nil {
		t.Fatal(err)
	}
	ws.NetConn().SetDeadline(time.Now().Add(10 * time.Second))
	if msg, err := ws.ReadMessage(MaxWSMessage); err != nil || msg != "ready" {
		t.Fatalf("%q %v", msg, err)
	}
	during := metrics()
	if during.OpenStreams != 1 || during.Requests[core.RoutePrivate][core.Class1xx] != 1 || during.Requests[core.RoutePrivate][core.Class2xx] != 1 || during.DurationCount != 1 {
		t.Fatalf("during the WebSocket %+v", during)
	}
	ws.Close(1006, "")
	<-ws.Done()
	deadline := time.Now().Add(10 * time.Second)
	for metrics().OpenStreams != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	for metrics().CPUSeconds < 0.2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	after := metrics()
	if after.OpenStreams != 0 || after.DurationCount != 1 || !after.HasWorker || after.State != core.WorkerRunning || after.CPUSeconds < 0.2 {
		t.Fatalf("after %+v", after)
	}
	if after.MemoryOK != (goruntime.GOOS == "linux") {
		t.Fatalf("memory measured %t on %s", after.MemoryOK, goruntime.GOOS)
	}
}

// A worker whose startup fails is an unexpected exit, whether it exits on its
// own first or is killed after reporting the failure.
func TestFailedStartupIsUnexpectedExit(t *testing.T) {
	m := newManager(t)
	root := t.TempDir()
	dir := filepath.Join(root, "versions", "1")
	writeFiles(t, dir, map[string]string{"server.js": `throw new Error("broken at load");`})
	sink := &recSink{}
	spec := core.RuntimeSpec{Flat: "broken", Version: 1, Dir: dir, Entry: "server.js", DataDir: filepath.Join(root, "data"), Log: (&logs{}).add, Usage: sink}
	if inst, err := m.Start(context.Background(), spec); err == nil {
		inst.Stop()
		t.Fatal("a flat that throws at load started")
	}
	ev, ok := sink.last("exited")
	if !ok || !ev.unexpected {
		t.Fatalf("exit event %+v (found %v), want unexpected", ev, ok)
	}
}
