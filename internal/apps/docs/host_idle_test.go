package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	flatsruntime "github.com/gosuda/flats/internal/runtime"
	"github.com/gosuda/flats/internal/store"
)

// idleRuntime observes real child launches without substituting the worker.
type idleRuntime struct {
	manager      *flatsruntime.Manager
	mu           sync.Mutex
	pids         map[string][]int
	spawns       map[string]int
	managed      map[string]core.ManagedDocsInstance
	instanceWrap func(core.RuntimeSpec, core.Instance) core.Instance
}

func (r *idleRuntime) Start(ctx context.Context, spec core.RuntimeSpec) (core.Instance, error) {
	if spec.ManagedDocs {
		next := spec.NextGeneration
		spec.NextGeneration = func(ctx context.Context) (int64, error) {
			r.mu.Lock()
			if r.spawns == nil {
				r.spawns = map[string]int{}
			}
			r.spawns[spec.Flat]++
			r.mu.Unlock()
			if next != nil {
				return next(ctx)
			}
			return spec.Generation, nil
		}
	}
	log := spec.Log
	spec.Log = func(level, msg string) {
		if raw, ok := strings.CutPrefix(msg, "docs test worker pid:"); ok {
			if pid, err := strconv.Atoi(raw); err == nil && spec.ManagedDocs {
				r.mu.Lock()
				r.pids[spec.Flat] = append(r.pids[spec.Flat], pid)
				r.mu.Unlock()
			}
		}
		if log != nil {
			log(level, msg)
		}
	}
	inst, err := r.manager.Start(ctx, spec)
	if err == nil && r.instanceWrap != nil {
		inst = r.instanceWrap(spec, inst)
	}
	if err == nil && spec.ManagedDocs {
		r.mu.Lock()
		if r.managed == nil {
			r.managed = map[string]core.ManagedDocsInstance{}
		}
		r.managed[spec.Flat] = inst.(core.ManagedDocsInstance)
		r.mu.Unlock()
	}
	return inst, err
}
func (r *idleRuntime) spawnCount(slug string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.spawns[slug]
}
func (r *idleRuntime) controller(slug string) core.ManagedDocsInstance {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.managed[slug]
}
func (r *idleRuntime) launched(slug string) []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.pids[slug]...)
}
func (r *idleRuntime) pid(slug string) int {
	p := r.launched(slug)
	if len(p) == 0 {
		return 0
	}
	return p[len(p)-1]
}
func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

type idleHost struct {
	s      *core.Service
	st     *store.Store
	net    *captureNet
	rt     *idleRuntime
	offset atomic.Int64
	data   string
}

func newIdleHost(t *testing.T) *idleHost {
	t.Helper()
	h := &idleHost{data: t.TempDir()}
	var err error
	h.st, err = store.Open(filepath.Join(h.data, "host.db"))
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runtimeDir, err := os.MkdirTemp("/tmp", "flats-idle-host-")
	if err != nil {
		h.st.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runtimeDir) })
	h.rt = &idleRuntime{manager: &flatsruntime.Manager{DataDir: runtimeDir, Exe: exe, Env: []string{"FLATS_DOCS_TEST_WORKER=1"}}, pids: map[string][]int{}}
	t.Cleanup(func() { h.close(); h.st.Close() })
	h.open(t)
	return h
}
func (h *idleHost) close() {
	if h.s != nil {
		s := h.s
		h.s = nil
		s.Close()
	}
}
func (h *idleHost) open(t *testing.T) {
	t.Helper()
	h.net = &captureNet{handlers: map[string]http.Handler{}}
	var err error
	h.s, err = core.New(t.Context(), core.Config{DataDir: h.data, Store: h.st, Private: h.net, Runtime: h.rt,
		Now:     func() time.Time { return time.Now().Add(time.Duration(h.offset.Load())) },
		DocsApp: core.DocsApp{FS: FS(), Hash: Hash(), Entry: Entry, ContentModule: ContentModule}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
}
func (h *idleHost) save(t *testing.T, slug string) {
	t.Helper()
	_, err := h.s.SaveVersion(t.Context(), slug, []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte(initialText)}, {Path: "asset.txt", Data: []byte("persisted asset")}}, core.SaveMeta{}, core.ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
}
func (h *idleHost) expire(t *testing.T, slug string) {
	t.Helper()
	pid := h.rt.pid(slug)
	if !alive(pid) {
		t.Fatalf("expected running child before expiration: %d", pid)
	}
	h.offset.Add(int64(3 * time.Minute))
	deadline := time.Now().Add(10 * time.Second)
	for alive(pid) && time.Now().Before(deadline) {
		// Closing a client is asynchronous at the reverse proxy. Advance again
		// if its final release lands after the preceding clock adjustment.
		h.offset.Add(int64(3 * time.Minute))
		h.s.TickDocs(t.Context())
		time.Sleep(10 * time.Millisecond)
	}
	waitIdleExit(t, pid)
	h.waitClean(t, slug)
}
func (h *idleHost) waitClean(t *testing.T, slug string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !h.rt.controller(slug).CleanDormant() && time.Now().Before(deadline) {
		h.offset.Add(int64(3 * time.Minute))
		h.s.TickDocs(t.Context())
		time.Sleep(10 * time.Millisecond)
	}
	if !h.rt.controller(slug).CleanDormant() {
		t.Fatal("worker exit did not complete clean suspension")
	}
}
func waitIdleExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if alive(pid) {
		t.Fatalf("worker %d did not exit", pid)
	}
}
func idleGET(t *testing.T, url string) string {
	t.Helper()
	response, err := (&http.Client{Timeout: 15 * time.Second}).Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", url, response.StatusCode, body)
	}
	return string(body)
}
func idleDocument(t *testing.T, url string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(idleGET(t, url+"/_docs/api/document")), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestHostIdleRealWorkerCycles(t *testing.T) {
	h := newIdleHost(t)
	h.save(t, "idle-doc")
	srv := httptest.NewServer(h.net.handler("idle-doc"))
	defer srv.Close()
	initial := idleDocument(t, srv.URL)
	history, err := h.s.ListRevisions(t.Context(), "idle-doc", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var first idleResources
	for i := 0; i < 20; i++ {
		previous := h.rt.pid("idle-doc")
		h.expire(t, "idle-doc")
		resources := idleResourceSnapshot(t, h)
		if i == 0 {
			first = resources
		}
		if i == 19 && (resources.fds > first.fds+8 || resources.goroutines > first.goroutines+10) {
			t.Fatalf("resource growth across20cycles: first=%+v last=%+v", first, resources)
		}
		t.Logf("IDLE_RESOURCES cycle=%d fds=%d goroutines=%d sockets=%d", i+1, resources.fds, resources.goroutines, resources.sockets)
		launches := h.rt.spawnCount("idle-doc")
		for j := 0; j < 2; j++ {
			h.s.TickDocs(t.Context())
		}
		revs, err := h.s.ListRevisions(t.Context(), "idle-doc", 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(revs.Revisions) != len(history.Revisions) {
			t.Fatal("sleep changed history")
		}
		if h.rt.spawnCount("idle-doc") != launches {
			t.Fatal("maintenance woke document")
		}
		start := time.Now()
		idleGET(t, srv.URL+"/")
		page := time.Since(start)
		next := idleDocument(t, srv.URL)
		if next["markdown"] != initial["markdown"] || next["epoch"] != initial["epoch"] || next["seq"] != initial["seq"] {
			t.Fatalf("state changed on wake: %v -> %v", initial, next)
		}
		start = time.Now()
		ws := connect(t, srv.URL, "private")
		ws.hello(t, nil)
		hello := time.Since(start)
		ws.Close()
		if got := h.rt.pid("idle-doc"); got == previous || !alive(got) {
			t.Fatalf("expected fresh live process: %d -> %d", previous, got)
		}
		if h.rt.spawnCount("idle-doc") != launches+1 {
			t.Fatal("wake launched multiple workers")
		}
		t.Logf("IDLE_WAKE sample=%d pid=%d page_us=%d hello_us=%d", i+1, h.rt.pid("idle-doc"), page.Microseconds(), hello.Microseconds())
	}
	if idleGET(t, srv.URL+"/asset.txt") != "persisted asset" {
		t.Fatal("asset lost")
	}
}

func TestHostIdleWebSocketPinsWorkerAndRestoreIsLazy(t *testing.T) {
	h := newIdleHost(t)
	h.save(t, "idle-doc")
	srv := httptest.NewServer(h.net.handler("idle-doc"))
	ws := connect(t, srv.URL, "private")
	ws.hello(t, nil)
	pid := h.rt.pid("idle-doc")
	h.offset.Add(int64(3 * time.Minute))
	launches := h.rt.spawnCount("idle-doc")
	until := time.Now().Add(4500 * time.Millisecond)
	for time.Now().Before(until) {
		h.s.TickDocs(t.Context())
		time.Sleep(50 * time.Millisecond)
	}
	if !alive(pid) || h.rt.pid("idle-doc") != pid || h.rt.spawnCount("idle-doc") != launches {
		t.Fatal("quiet websocket lost/replaced worker")
	}
	ws.send(t, map[string]any{"t": "ping"})
	ws.next(t, "pong")
	ws.Close()
	srv.Close()
	h.expire(t, "idle-doc")
	before := h.rt.spawnCount("idle-doc")
	h.close()
	h.open(t)
	if h.rt.spawnCount("idle-doc") != before {
		t.Fatal("restore started dormant document")
	}
	srv = httptest.NewServer(h.net.handler("idle-doc"))
	defer srv.Close()
	if idleDocument(t, srv.URL)["markdown"] != initialText {
		t.Fatal("restored content differs")
	}
}

func TestHostIdleRealClockAndHumanRevision(t *testing.T) {
	if os.Getenv("FLATS_IDLE_REAL_CLOCK") != "1" {
		t.Skip("set FLATS_IDLE_REAL_CLOCK=1 for real two-minute expiry and worker group maturity")
	}
	h := newIdleHost(t)
	h.save(t, "idle-doc")
	srv := httptest.NewServer(h.net.handler("idle-doc"))
	defer srv.Close()
	ws := connect(t, srv.URL, "private")
	ws.hello(t, nil)
	edit(t, ws, "idle-human", fixture(t).Independent)
	ws.Close()
	original := idleDocument(t, srv.URL)
	pid := h.rt.pid("idle-doc")
	start := time.Now()
	// Real wall time advances both the host idle clock and worker Date.now().
	for time.Since(start) < 125*time.Second {
		time.Sleep(time.Second)
		h.s.TickDocs(t.Context())
		if time.Since(start) < 119*time.Second && !alive(pid) {
			t.Fatal("child exited before real idle interval")
		}
	}
	h.s.TickDocs(t.Context())
	waitIdleExit(t, pid)
	history, err := h.s.ListRevisions(t.Context(), "idle-doc", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Revisions) < 2 || history.Revisions[0].Kind != "person" {
		t.Fatalf("pending person revision missing: %+v", history.Revisions)
	}
	got := idleDocument(t, srv.URL)
	if got["markdown"] != original["markdown"] {
		t.Fatal("acknowledged human edit lost")
	}
	ws = connect(t, srv.URL, "private")
	ws.hello(t, nil)
	edit(t, ws, "idle-human", fixture(t).Independent)
	ws.Close()
	if idleDocument(t, srv.URL)["markdown"] != original["markdown"] {
		t.Fatal("replayed update duplicated text")
	}
	t.Logf("REAL_IDLE elapsed=%s old_pid=%d new_pid=%d revisions=%d", time.Since(start), pid, h.rt.pid("idle-doc"), len(history.Revisions))
}

func TestHostIdleStoredDocumentScaling(t *testing.T) {
	if os.Getenv("FLATS_IDLE_MEASURE") != "1" {
		t.Skip("set FLATS_IDLE_MEASURE=1 for serial 1/10/100 document process measurements")
	}
	h := newIdleHost(t)
	for i := 1; i <= 100; i++ {
		slug := fmt.Sprintf("idle-%03d", i)
		h.save(t, slug)
		h.expire(t, slug)
		if i == 1 || i == 10 || i == 100 {
			running := 0
			h.rt.mu.Lock()
			for _, pids := range h.rt.pids {
				for _, pid := range pids {
					if alive(pid) {
						running++
					}
				}
			}
			h.rt.mu.Unlock()
			if running != 0 {
				t.Fatalf("%d saved docs retained %d workers", i, running)
			}
			logIdleMemory(t, i, "dormant")
		}
	}
	// Restore has a logical route per doc but starts no physical children.
	h.close()
	before := 0
	h.rt.mu.Lock()
	for _, count := range h.rt.spawns {
		before += count
	}
	h.rt.mu.Unlock()
	h.open(t)
	after := 0
	h.rt.mu.Lock()
	for _, count := range h.rt.spawns {
		after += count
	}
	h.rt.mu.Unlock()
	if after != before {
		t.Fatal("100-doc restore started workers")
	}
	logIdleMemory(t, 100, "restored-dormant")
	var sockets []*socket
	var servers []*httptest.Server
	defer func() {
		for _, x := range sockets {
			x.Close()
		}
		for _, server := range servers {
			server.Close()
		}
	}()
	for i := 1; i <= 10; i++ {
		server := httptest.NewServer(h.net.handler(fmt.Sprintf("idle-%03d", i)))
		servers = append(servers, server)
		x := connect(t, server.URL, "private")
		x.hello(t, nil)
		sockets = append(sockets, x)
		if i == 1 || i == 5 || i == 10 {
			logIdleMemory(t, 100, fmt.Sprintf("active-%d", i))
			for j := 1; j <= i; j++ {
				pid := h.rt.pid(fmt.Sprintf("idle-%03d", j))
				if !alive(pid) {
					t.Fatal("active worker absent")
				}
				raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
				t.Logf("IDLE_ACTIVE pid=%d error=%v\n%s", pid, err, raw)
			}
		}
	}
}

func TestHostIdleColdCompilationWakeTimings(t *testing.T) {
	if os.Getenv("FLATS_IDLE_MEASURE") != "1" {
		t.Skip("set FLATS_IDLE_MEASURE=1 for 20 cold-compilation process wakes")
	}
	h := newIdleHost(t)
	h.save(t, "idle-doc")
	server := httptest.NewServer(h.net.handler("idle-doc"))
	defer server.Close()
	for i := 0; i < 20; i++ {
		h.expire(t, "idle-doc")
		// Dedicated manager cache belongs solely to this test, with no live child.
		if err := os.RemoveAll(filepath.Join(h.rt.manager.DataDir, "cache", "wazero")); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		idleGET(t, server.URL+"/")
		page := time.Since(start)
		start = time.Now()
		x := connect(t, server.URL, "private")
		x.hello(t, nil)
		hello := time.Since(start)
		x.Close()
		t.Logf("IDLE_COLD_WAKE sample=%d pid=%d page_us=%d hello_us=%d", i+1, h.rt.pid("idle-doc"), page.Microseconds(), hello.Microseconds())
	}
}
func logIdleMemory(t *testing.T, n int, phase string) {
	t.Helper()
	if tasks, err := os.ReadDir("/proc/self/task"); err == nil {
		seen := map[string]bool{}
		for _, task := range tasks {
			raw, err := os.ReadFile("/proc/self/task/" + task.Name() + "/children")
			if err != nil {
				continue
			}
			for _, pid := range strings.Fields(string(raw)) {
				seen[pid] = true
			}
		}
		t.Logf("IDLE_CHILDREN docs=%d phase=%s count=%d pids=%v", n, phase, len(seen), seen)
		if (phase == "dormant" || phase == "restored-dormant") && len(seen) != 0 {
			t.Fatalf("dormant host retained children: %v", seen)
		}
	}
	for _, path := range []string{"/sys/fs/cgroup/memory.current", "/sys/fs/cgroup/memory.stat", "/proc/self/smaps_rollup"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Logf("MEMORY_UNAVAILABLE path=%s error=%v", path, err)
			continue
		}
		t.Logf("IDLE_MEMORY docs=%d phase=%s path=%s\n%s", n, phase, path, raw)
	}
}

type idleResources struct{ fds, goroutines, sockets int }

func idleResourceSnapshot(t *testing.T, h *idleHost) idleResources {
	t.Helper()
	fdpath := "/proc/self/fd"
	if runtime.GOOS == "darwin" {
		fdpath = "/dev/fd"
	}
	fds, err := os.ReadDir(fdpath)
	if err != nil {
		t.Fatal(err)
	}
	sockets, err := filepath.Glob(filepath.Join(h.rt.manager.DataDir, "run", "*.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sockets) != 0 {
		t.Fatalf("dormant worker sockets remain: %v", sockets)
	}
	return idleResources{fds: len(fds), goroutines: runtime.NumGoroutine(), sockets: len(sockets)}
}
