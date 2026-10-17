package runtime

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
)

const spareTestScript = `export default { fetch() { return new Response("ok"); } }`

func spareTestFiles() map[string]string { return map[string]string{"index.js": spareTestScript} }

// spareManager is a test Manager whose operational log is also collected.
func spareManager(t *testing.T) (*Manager, *logs) {
	t.Helper()
	m := newManager(t)
	out := &logs{}
	m.Logf = func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		out.add("manager", msg)
		t.Log(msg)
	}
	return m, out
}

func currentSpare(m *Manager) *spare {
	m.spare.mu.Lock()
	defer m.spare.mu.Unlock()
	return m.spare.cur
}

func waitSpare(t *testing.T, m *Manager, not int) *spare {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if sp := currentSpare(m); sp != nil && sp.p.cmd.Process.Pid != not {
			return sp
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no pre-started worker appeared")
	return nil
}

func processAlive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Fatalf("process %d still running", pid)
	}
}

func spareSockets(t *testing.T, m *Manager) []string {
	t.Helper()
	socks, err := filepath.Glob(filepath.Join(m.sockDir, "s*.sock"))
	if err != nil {
		t.Fatal(err)
	}
	return socks
}

func getOK(t *testing.T, f *flat) {
	t.Helper()
	if r := f.do(t, "GET", "/", ""); r.status != 200 || r.body != "ok" {
		t.Fatalf("GET = %d %q; logs:\n%s", r.status, r.body, f.logs)
	}
}

func TestSpareAdoption(t *testing.T) {
	t.Parallel()
	m, _ := spareManager(t)
	mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	sp := waitSpare(t, m, 0)
	pid := sp.p.cmd.Process.Pid
	waiting := time.Now()
	f := mustStart(t, m, "second", spareTestFiles(), "index.js", nil)
	in := f.inst.(*instance)
	if in.pid() != pid {
		t.Fatalf("instance pid %d, want the pre-started worker %d", in.pid(), pid)
	}
	// The worker's socket moved to the instance's path and serves there.
	if _, err := os.Stat(sp.sock); !os.IsNotExist(err) {
		t.Fatalf("pre-started socket still exists: %v", err)
	}
	if _, err := os.Stat(in.sock); err != nil {
		t.Fatal(err)
	}
	getOK(t, f)
	// Crash backoff measures the worker's life from adoption, not from exec.
	in.mu.Lock()
	started := in.p.started
	in.mu.Unlock()
	if !started.After(waiting) {
		t.Fatalf("started %v is not the adoption time (after %v)", started, waiting)
	}
	// Adoption requests a replacement.
	next := waitSpare(t, m, pid)
	if next.p.cmd.Process.Pid == pid {
		t.Fatal("adopted worker is still the spare")
	}
	// A crash after adoption restarts the instance as before.
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	f.logs.waitFor(t, "worker exited unexpectedly")
	getOK(t, f)
	if in.pid() == pid || in.pid() == 0 {
		t.Fatalf("worker not restarted: %d", in.pid())
	}
}

// A spare that died while waiting is noticed before its spec is written.
func TestSpareDeadBeforeAdoption(t *testing.T) {
	t.Parallel()
	m, mlog := spareManager(t)
	mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	sp := waitSpare(t, m, 0)
	// Detach it so its exit is not noticed, kill it, and put it back: the
	// state of a spawn that takes it while the exit is being reported.
	m.spare.mu.Lock()
	m.spare.cur = nil
	m.spare.mu.Unlock()
	if err := sp.p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-sp.p.done
	m.spare.mu.Lock()
	m.spare.cur = sp
	m.spare.mu.Unlock()
	f := mustStart(t, m, "second", spareTestFiles(), "index.js", nil)
	if pid := f.inst.(*instance).pid(); pid == sp.p.cmd.Process.Pid || pid == 0 {
		t.Fatalf("pid %d", pid)
	}
	getOK(t, f)
	mlog.waitFor(t, "pre-started worker unavailable")
	if _, err := os.Stat(sp.sock); !os.IsNotExist(err) {
		t.Fatalf("dead spare's socket remains: %v", err)
	}
}

// A spare that exits after receiving its spec, without a startup error,
// leaves the start to a new process.
func TestSpareDiesAfterSpec(t *testing.T) {
	t.Parallel()
	m, mlog := spareManager(t)
	mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	sp := waitSpare(t, m, 0)
	f := mustStart(t, m, testDieAdoptedFlat, spareTestFiles(), "index.js", nil)
	if pid := f.inst.(*instance).pid(); pid == sp.p.cmd.Process.Pid || pid == 0 {
		t.Fatalf("pid %d", pid)
	}
	getOK(t, f)
	mlog.waitFor(t, "pre-started worker unavailable")
	if f.logs.find("exited unexpectedly") {
		t.Fatalf("fallback reported as a crash:\n%s", f.logs)
	}
}

// A failed socket move kills the spare and starts a new process.
func TestSpareRenameFailure(t *testing.T) {
	t.Parallel()
	m, mlog := spareManager(t)
	mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	sp := waitSpare(t, m, 0)
	if err := os.Remove(sp.sock); err != nil {
		t.Fatal(err)
	}
	f := mustStart(t, m, "second", spareTestFiles(), "index.js", nil)
	if pid := f.inst.(*instance).pid(); pid == sp.p.cmd.Process.Pid || pid == 0 {
		t.Fatalf("pid %d", pid)
	}
	getOK(t, f)
	mlog.waitFor(t, "pre-started worker unavailable")
	waitGone(t, sp.p.cmd.Process.Pid)
}

// A startup error of the flat itself is reported the same way on an adopted
// worker, without a second attempt.
func TestSpareStartupErrorUnchanged(t *testing.T) {
	t.Parallel()
	m, _ := spareManager(t)
	_, execErr := startFlat(t, m, "broken", map[string]string{"index.js": "export default {"}, "index.js", nil)
	if execErr == nil {
		t.Fatal("syntax error started")
	}
	mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	sp := waitSpare(t, m, 0)
	_, err := startFlat(t, m, "broken", map[string]string{"index.js": "export default {"}, "index.js", nil)
	if err == nil || err.Error() != execErr.Error() {
		t.Fatalf("adopted startup error %q, want %q", err, execErr)
	}
	waitGone(t, sp.p.cmd.Process.Pid)
	if currentSpare(m) == sp {
		t.Fatal("spare not taken")
	}
}

func TestSpareConcurrentSpawns(t *testing.T) {
	t.Parallel()
	m, _ := spareManager(t)
	mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	pid := waitSpare(t, m, 0).p.cmd.Process.Pid
	const n = 4
	var wg sync.WaitGroup
	flats := make([]*flat, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			flats[i], errs[i] = startFlat(t, m, fmt.Sprintf("concurrent-%d", i), spareTestFiles(), "index.js", nil)
		}()
	}
	wg.Wait()
	adopted := 0
	for i, f := range flats {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if f.inst.(*instance).pid() == pid {
			adopted++
		}
		getOK(t, f)
	}
	if adopted != 1 {
		t.Fatalf("%d instances adopted the one spare", adopted)
	}
}

// Before adoption a spare's output belongs to the Manager log; afterwards
// to its instance.
func TestSpareLogRouting(t *testing.T) {
	t.Parallel()
	m, mlog := spareManager(t)
	m.Env = append(m.Env, testUnassignedLogEnv+"=1")
	first := mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	if !first.logs.find("test-worker-unassigned") {
		t.Fatalf("exec'd worker output not in its log:\n%s", first.logs)
	}
	waitSpare(t, m, 0)
	mlog.waitFor(t, "test-worker-unassigned")
	f := mustStart(t, m, "second", spareTestFiles(), "index.js", nil)
	f.logs.waitFor(t, "test-env-keys: ")
	if f.logs.find("test-worker-unassigned") {
		t.Fatalf("pre-adoption output in the instance log:\n%s", f.logs)
	}
}

func TestSpareStopsWithLastInstance(t *testing.T) {
	t.Parallel()
	m, _ := spareManager(t)
	a := mustStart(t, m, "a", spareTestFiles(), "index.js", nil)
	b := mustStart(t, m, "b", spareTestFiles(), "index.js", nil)
	pid := waitSpare(t, m, 0).p.cmd.Process.Pid
	a.inst.Stop()
	if m.SparePID() != pid || !processAlive(pid) {
		t.Fatal("spare stopped while an instance remains")
	}
	b.inst.Stop()
	if m.SparePID() != 0 {
		t.Fatal("spare remains after the last instance stopped")
	}
	waitGone(t, pid)
	if socks := spareSockets(t, m); len(socks) != 0 {
		t.Fatalf("spare sockets remain: %v", socks)
	}
	// A new instance gets a new spare.
	mustStart(t, m, "c", spareTestFiles(), "index.js", nil)
	if next := waitSpare(t, m, 0); next.p.cmd.Process.Pid == pid {
		t.Fatal("old spare reused")
	}
}

// A sleeping document counts as an instance; an instance that stops before
// the creation delay never gets a spare.
func TestSpareLazyDocsAndShortInstances(t *testing.T) {
	t.Parallel()
	m, _ := spareManager(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"server.js": spareTestScript})
	spec := core.RuntimeSpec{Flat: "lazy-doc", Version: 1, Dir: dir, Entry: "server.js", DataDir: t.TempDir(), ManagedDocs: true, Lazy: true}
	short, err := m.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	short.Stop()
	time.Sleep(2 * spareDelay)
	if m.SparePID() != 0 || len(spareSockets(t, m)) != 0 {
		t.Fatal("a spare started for an instance that stopped before the delay")
	}
	inst, err := m.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Stop()
	d := inst.(*docsInstance)
	pid := waitSpare(t, m, 0).p.cmd.Process.Pid
	lease, err := d.Wake(t.Context(), func(ctx context.Context, h http.Handler) error {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, "GET", "http://flat/", nil))
		if rec.Code != 200 {
			return fmt.Errorf("prepare status %d", rec.Code)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	d.mu.Lock()
	child := d.child
	d.mu.Unlock()
	if child.pid() != pid {
		t.Fatalf("document woke in %d, want the spare %d", child.pid(), pid)
	}
}

func TestSpareClose(t *testing.T) {
	t.Parallel()
	m, _ := spareManager(t)
	mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	pid := waitSpare(t, m, 0).p.cmd.Process.Pid
	m.Close()
	if m.SparePID() != 0 {
		t.Fatal("spare remains after Close")
	}
	waitGone(t, pid)
	f := mustStart(t, m, "after-close", spareTestFiles(), "index.js", nil)
	getOK(t, f)
	time.Sleep(2 * spareDelay)
	if m.SparePID() != 0 || len(spareSockets(t, m)) != 0 {
		t.Fatal("spare started after Close")
	}
	m.Close() // idempotent
}

func TestSpareDisabled(t *testing.T) {
	t.Parallel()
	m, _ := spareManager(t)
	m.NoSpare = true
	f := mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	time.Sleep(2 * spareDelay)
	if m.SparePID() != 0 || len(spareSockets(t, m)) != 0 {
		t.Fatal("spare started with NoSpare")
	}
	getOK(t, f)
}

// A spare that exits before adoption is replaced after a backoff.
func TestSpareReplacedAfterExit(t *testing.T) {
	t.Parallel()
	m, mlog := spareManager(t)
	mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	sp := waitSpare(t, m, 0)
	pid := sp.p.cmd.Process.Pid
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	mlog.waitFor(t, "replacing it in "+spareRetryMin.String())
	if _, err := os.Stat(sp.sock); !os.IsNotExist(err) {
		t.Fatalf("exited spare's socket remains: %v", err)
	}
	next := waitSpare(t, m, pid)
	if err := syscall.Kill(next.p.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	mlog.waitFor(t, "replacing it in "+(2*spareRetryMin).String())
	waitSpare(t, m, next.p.cmd.Process.Pid)
}

func TestSpareStaleSocketsRemoved(t *testing.T) {
	t.Parallel()
	m, _ := spareManager(t)
	run := filepath.Join(m.DataDir, "run")
	if err := os.MkdirAll(run, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sdeadbeef.sock", "wdeadbeef.sock"} {
		if err := os.WriteFile(filepath.Join(run, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	for _, name := range []string{"sdeadbeef.sock", "wdeadbeef.sock"} {
		if _, err := os.Stat(filepath.Join(run, name)); !os.IsNotExist(err) {
			t.Fatalf("stale %s remains: %v", name, err)
		}
	}
}

// A pre-started worker that is stopped before it gets a spec exits quietly.
func TestSpareQuietExit(t *testing.T) {
	t.Parallel()
	m, mlog := spareManager(t)
	f := mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	sp := waitSpare(t, m, 0)
	f.inst.Stop()
	<-sp.p.done
	if sp.p.err != nil {
		t.Fatalf("spare exit: %v", sp.p.err)
	}
	if s := mlog.String(); strings.Contains(s, "error") || strings.Contains(s, "fatal") {
		t.Fatalf("spare logged an error:\n%s", s)
	}
}
