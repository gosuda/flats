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

// A spare that exits after its spec was written but before it acknowledged
// it leaves the start to a new process.
func TestSpareDiesBeforeSpecAck(t *testing.T) {
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

// A spare that exits after acknowledging its spec has begun the flat's
// startup: that is the flat's startup failure, reported as for a new
// process, and the flat's code is not run a second time.
func TestSpareDiesAfterSpecAck(t *testing.T) {
	t.Parallel()
	plain := newManager(t)
	plain.NoSpare = true
	_, execErr := startFlat(t, plain, testDieAckedFlat, spareTestFiles(), "index.js", nil)
	if execErr == nil {
		t.Fatal("a worker that exits during startup started")
	}
	m, mlog := spareManager(t)
	mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	sp := waitSpare(t, m, 0)
	_, err := startFlat(t, m, testDieAckedFlat, spareTestFiles(), "index.js", nil)
	if err == nil || err.Error() != execErr.Error() {
		t.Fatalf("adopted startup error %q, want %q", err, execErr)
	}
	if mlog.find("pre-started worker unavailable") {
		t.Fatalf("started a second process after the spec was acknowledged:\n%s", mlog)
	}
	waitGone(t, sp.p.cmd.Process.Pid)
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
	// The spare is stopped in the background: its socket goes once the
	// stop finished, shortly after the process is reaped.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		socks := spareSockets(t, m)
		if len(socks) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("spare sockets remain: %v", socks)
		}
	}
	// A new instance gets a new spare.
	mustStart(t, m, "c", spareTestFiles(), "index.js", nil)
	if next := waitSpare(t, m, 0); next.p.cmd.Process.Pid == pid {
		t.Fatal("old spare reused")
	}
}

// Stopping the last instance does not wait for the spare, which exits on
// stdin EOF only once it finished preparing; Close does.
func TestSpareStopDoesNotWait(t *testing.T) {
	t.Parallel()
	m, mlog := spareManager(t)
	m.Env = append(m.Env, testSlowWarmUpEnv+"=1")
	f := mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	sp := waitSpare(t, m, 0)
	mlog.waitFor(t, "test-warm-up: "+sp.p.warmDir)
	begin := time.Now()
	f.inst.Stop()
	t.Logf("Stop took %s", time.Since(begin))
	select {
	case <-sp.p.done:
		t.Fatal("Stop waited for the spare, which was preparing")
	default:
	}
	// Let it finish preparing, so that it exits on its own.
	release := sp.p.warmDir + ".release"
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(release)
	m.Close()
	select {
	case <-sp.p.done:
	default:
		t.Fatal("Close returned before the spare exited")
	}
	if sp.p.err != nil {
		t.Fatalf("spare exit: %v", sp.p.err)
	}
	for _, path := range []string{sp.sock, sp.p.warmDir} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s remains: %v", path, err)
		}
	}
}

// A spare killed during its database warm-up leaves no warm-up directory,
// also in the temp directory that Managers without a private one share,
// where other Managers' entries stay.
func TestSpareKilledDuringWarmUp(t *testing.T) {
	t.Parallel()
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared=%v", shared), func(t *testing.T) {
			t.Parallel()
			m, mlog := spareManager(t)
			m.Env = append(m.Env, testSlowWarmUpEnv+"=1")
			if shared {
				// Too long for socket paths: the Manager falls back to the
				// shared temp directory.
				long := filepath.Join(m.DataDir, strings.Repeat("d", 80))
				if err := os.Mkdir(long, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(cacheRoot, "cache"), filepath.Join(long, "cache")); err != nil {
					t.Fatal(err)
				}
				m.DataDir = long
			}
			if err := m.init(); err != nil {
				t.Fatal(err)
			}
			if fallback := filepath.Join(os.TempDir(), fmt.Sprintf("flats-%d", os.Getuid())); (m.sockDir == fallback) != shared {
				t.Fatalf("socket directory %q (shared %v)", m.sockDir, shared)
			}
			if m.scratchDir == "" {
				t.Fatal("no scratch directory")
			}
			other, err := os.MkdirTemp(m.scratchDir, "sother")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(other) })
			f := mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
			sp := waitSpare(t, m, 0)
			mlog.waitFor(t, "test-warm-up: "+sp.p.warmDir)
			if filepath.Dir(sp.p.warmDir) != m.scratchDir {
				t.Fatalf("warm-up directory %q is not in %q", sp.p.warmDir, m.scratchDir)
			}
			if left, err := os.ReadDir(sp.p.warmDir); err != nil || len(left) == 0 {
				t.Fatalf("warm-up directory during the warm-up: %v %v", left, err)
			}
			if err := syscall.Kill(sp.p.cmd.Process.Pid, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			<-sp.p.done
			if _, err := os.Stat(sp.p.warmDir); !os.IsNotExist(err) {
				t.Fatalf("warm-up directory remains: %v", err)
			}
			if _, err := os.Stat(other); err != nil {
				t.Fatalf("another Manager's entry was removed: %v", err)
			}
			// Its replacement, if one started, is stopped (killed in its
			// warm-up after the stop timeout) and leaves nothing either;
			// only the private directory can be checked as a whole.
			f.inst.Stop()
			m.Close()
			if left, _ := os.ReadDir(m.scratchDir); !shared && (len(left) != 1 || left[0].Name() != filepath.Base(other)) {
				t.Fatalf("scratch directory after Close: %v", left)
			}
		})
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

// The spare is requested spareDelay after the first instance became ready,
// not when its start began: a start slower than the delay gets no spare
// beside it.
func TestSpareDelayFromReady(t *testing.T) {
	t.Parallel()
	m, _ := spareManager(t)
	busy := 2*spareDelay + 100*time.Millisecond
	slow := fmt.Sprintf("const t0 = Date.now(); while (Date.now() - t0 < %d) {}\n%s", busy.Milliseconds(), spareTestScript)
	begin := time.Now()
	f := mustStart(t, m, "slow", map[string]string{"index.js": slow}, "index.js", nil)
	m.spare.mu.Lock()
	due := m.spare.due
	m.spare.mu.Unlock()
	if earliest := begin.Add(busy + spareDelay); due.Before(earliest) {
		t.Fatalf("spare requested for %v after the start began, want at least %v", due.Sub(begin), busy+spareDelay)
	}
	if sp := waitSpare(t, m, 0); sp.p.started.Before(due) {
		t.Fatalf("spare started %v before it was due", due.Sub(sp.p.started))
	}
	getOK(t, f)
}

func spareDue(m *Manager) (time.Time, bool) {
	m.spare.mu.Lock()
	defer m.spare.mu.Unlock()
	return m.spare.due, m.spare.timer != nil
}

// The replacement spare is requested spareRefillDelay after the adoption
// finished (the adopted worker reported ready), not when it began, so that
// it does not compete with the woken worker for CPU.
func TestSpareRefillDelayFromAdoptionEnd(t *testing.T) {
	t.Parallel()
	m, _ := spareManager(t)
	mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	pid := waitSpare(t, m, 0).p.cmd.Process.Pid
	busy := 300 * time.Millisecond
	slow := fmt.Sprintf("const t0 = Date.now(); while (Date.now() - t0 < %d) {}\n%s", busy.Milliseconds(), spareTestScript)
	begin := time.Now()
	f := mustStart(t, m, "slow", map[string]string{"index.js": slow}, "index.js", nil)
	end := time.Now()
	if got := f.inst.(*instance).pid(); got != pid {
		t.Fatalf("instance pid %d, want the pre-started worker %d", got, pid)
	}
	due, pending := spareDue(m)
	if !pending || currentSpare(m) != nil {
		t.Fatal("no replacement pending after the adoption")
	}
	if earliest := begin.Add(busy + spareRefillDelay); due.Before(earliest) {
		t.Fatalf("replacement due %v after the adoption began, want at least %v", due.Sub(begin), busy+spareRefillDelay)
	}
	if latest := end.Add(spareRefillDelay); due.After(latest) {
		t.Fatalf("replacement due %v after the adoption finished, want at most %v", due.Sub(end), spareRefillDelay)
	}
	next := waitSpare(t, m, pid)
	if next.p.started.Before(due) {
		t.Fatalf("replacement started %v before it was due", due.Sub(next.p.started))
	}
	getOK(t, f)
}

// Adoptions within the refill window leave one pending replacement, which
// keeps its due time and starts one spare.
func TestSpareRefillOnePending(t *testing.T) {
	t.Parallel()
	m, _ := spareManager(t)
	mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	pid := waitSpare(t, m, 0).p.cmd.Process.Pid
	mustStart(t, m, "second", spareTestFiles(), "index.js", nil)
	due, pending := spareDue(m)
	if !pending {
		t.Fatal("no replacement pending after the adoption")
	}
	m.spare.mu.Lock()
	seq := m.spare.seq
	m.spare.mu.Unlock()
	m.refillSpare()
	m.instanceReady() // an instance that became ready meanwhile
	m.spare.mu.Lock()
	again, seqAgain := m.spare.due, m.spare.seq
	m.spare.mu.Unlock()
	if !again.Equal(due) || seqAgain != seq {
		t.Fatalf("pending replacement rescheduled: due %v -> %v, seq %d -> %d", due, again, seq, seqAgain)
	}
	// A spawn in the window starts a process the ordinary way.
	third := mustStart(t, m, "third", spareTestFiles(), "index.js", nil)
	getOK(t, third)
	if again, _ := spareDue(m); !again.Equal(due) {
		t.Fatalf("a spawn without a spare rescheduled the replacement: %v -> %v", due, again)
	}
	next := waitSpare(t, m, pid)
	time.Sleep(100 * time.Millisecond) // a second start would have begun with the first
	if socks := spareSockets(t, m); len(socks) != 1 || socks[0] != next.sock {
		t.Fatalf("spare sockets %v, want only %s", socks, next.sock)
	}
}

// Stopping the last instance or Close cancels a pending replacement.
func TestSpareRefillCancelled(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"stop", "close"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			m, _ := spareManager(t)
			first := mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
			waitSpare(t, m, 0)
			second := mustStart(t, m, "second", spareTestFiles(), "index.js", nil)
			if _, pending := spareDue(m); !pending {
				t.Fatal("no replacement pending after the adoption")
			}
			if how == "stop" {
				first.inst.Stop()
				second.inst.Stop()
			} else {
				m.Close()
			}
			if _, pending := spareDue(m); pending {
				t.Fatal("replacement still pending")
			}
			time.Sleep(spareRefillDelay + 200*time.Millisecond)
			if m.SparePID() != 0 || len(spareSockets(t, m)) != 0 {
				t.Fatal("a replacement started after it was cancelled")
			}
		})
	}
}

// SpareReady reports a spare that is waiting, alive and prepared.
func TestSpareReady(t *testing.T) {
	t.Parallel()
	m, _ := spareManager(t)
	if m.SpareReady() {
		t.Fatal("ready without an instance")
	}
	mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	deadline := time.Now().Add(15 * time.Second)
	for !m.SpareReady() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	sp := currentSpare(m)
	if !m.SpareReady() || sp == nil || sp.p.warmAt.Load() == 0 {
		t.Fatal("no ready spare")
	}
	// Detached so that its exit is not noticed: SpareReady checks it is alive.
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
	if m.SpareReady() {
		t.Fatal("an exited spare is ready")
	}
	m.spare.mu.Lock()
	m.spare.cur = nil
	m.spare.mu.Unlock()
	os.Remove(sp.sock)
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
	// A database warm-up left behind by a pre-started worker that died.
	left := filepath.Join(run, "prewarm", "db123")
	if err := os.MkdirAll(left, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(left, "db.sqlite"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	for _, name := range []string{"sdeadbeef.sock", "wdeadbeef.sock", "prewarm/db123"} {
		if _, err := os.Stat(filepath.Join(run, name)); !os.IsNotExist(err) {
			t.Fatalf("stale %s remains: %v", name, err)
		}
	}
}

// Pre-started workers warm up SQLite in the Manager's private scratch
// directory and leave nothing in it.
func TestSpareDatabaseWarmUp(t *testing.T) {
	t.Parallel()
	m, mlog := spareManager(t)
	mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	deadline := time.Now().Add(15 * time.Second)
	for !m.SpareReady() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !m.SpareReady() {
		t.Fatal("no ready spare")
	}
	if m.scratchDir != filepath.Join(m.sockDir, "prewarm") {
		t.Fatalf("scratch directory %q", m.scratchDir)
	}
	fi, err := os.Stat(m.scratchDir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("scratch directory: %v %v", fi, err)
	}
	if left, _ := os.ReadDir(m.scratchDir); len(left) != 0 {
		t.Fatalf("warm-up left %v", left)
	}
	if s := mlog.String(); strings.Contains(s, "warm-up") {
		t.Fatalf("warm-up failed:\n%s", s)
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

// startWithSink is startFlat with a usage sink.
func startWithSink(t *testing.T, m *Manager, name string, files map[string]string, entry string, sink core.UsageSink) (*flat, error) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "versions", "1")
	writeFiles(t, dir, files)
	f := &flat{logs: &logs{}, dataDir: filepath.Join(root, "data"), root: root}
	f.spec = core.RuntimeSpec{Flat: name, Version: 1, Dir: dir, Entry: entry, DataDir: f.dataDir, Log: f.logs.add, Usage: sink}
	inst, err := m.Start(context.Background(), f.spec)
	if err != nil {
		return nil, err
	}
	f.inst = inst
	f.srv = httptest.NewServer(inst)
	t.Cleanup(func() { f.srv.Close(); f.inst.Stop() })
	return f, nil
}

// An adopted pre-started worker charges the flat only for the CPU it used
// after it received its spec, never for its warm-up.
func TestSpareUsageExcludesWarmUp(t *testing.T) {
	t.Parallel()
	m, _ := spareManager(t)
	mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	sp := waitSpare(t, m, 0)
	sink := &recSink{}
	f, err := startWithSink(t, m, "second", spareTestFiles(), "index.js", sink)
	if err != nil {
		t.Fatal(err)
	}
	if pid := f.inst.(*instance).pid(); pid != sp.p.cmd.Process.Pid {
		t.Fatalf("instance pid %d, want the pre-started worker %d", pid, sp.p.cmd.Process.Pid)
	}
	if n := sink.count("started", sp.p.id); n != 1 {
		t.Fatalf("Started called %d times for the adopted worker", n)
	}
	getOK(t, f)
	f.inst.Stop()
	ev, ok := sink.last("exited")
	if !ok || ev.id != sp.p.id || ev.unexpected {
		t.Fatalf("exit %+v (found %v)", ev, ok)
	}
	// Stop returned after done closed, so the reader's fields are settled.
	ps := sp.p.cmd.ProcessState
	total := (ps.UserTime() + ps.SystemTime()).Nanoseconds()
	if sp.p.baseCPU <= 0 {
		t.Fatal("no warm-up CPU recorded with the spec acknowledgement")
	}
	if want := max(sp.p.lastCPU, total-sp.p.baseCPU); ev.cpu != want || ev.cpu >= total {
		t.Fatalf("exit CPU %d, want %d (process total %d, warm-up %d)", ev.cpu, want, total, sp.p.baseCPU)
	}
}

// A pre-started worker that exits before acknowledging its spec ran nothing
// of the flat: its exit is neither CPU nor an unexpected exit of the flat,
// and the new process that starts the flat is accounted as usual.
func TestSpareDiesBeforeSpecAckUsage(t *testing.T) {
	t.Parallel()
	m, _ := spareManager(t)
	mustStart(t, m, "first", spareTestFiles(), "index.js", nil)
	sp := waitSpare(t, m, 0)
	sink := &recSink{}
	f, err := startWithSink(t, m, testDieAdoptedFlat, spareTestFiles(), "index.js", sink)
	if err != nil {
		t.Fatal(err)
	}
	getOK(t, f)
	var spareExit *usageEvent
	started := 0
	for _, e := range sink.all() {
		if e.kind == "exited" && e.id == sp.p.id {
			spareExit = &e
		}
		if e.kind == "started" {
			started++
		}
	}
	if spareExit == nil || spareExit.cpu != 0 || spareExit.unexpected {
		t.Fatalf("spare exit %+v", spareExit)
	}
	if started != 2 {
		t.Fatalf("Started %d times, want the spare and the new process", started)
	}
}

// A pre-started worker that was already waited for cannot be adopted, so no
// Started is reported without its Exited.
func TestReapedSpareIsNotAssigned(t *testing.T) {
	sink := &recSink{}
	in := &instance{spec: core.RuntimeSpec{Usage: sink}}
	p := &proc{id: 7, tail: &tailBuf{}}
	if owner := p.reap(); owner != nil {
		t.Fatalf("owner %v", owner)
	}
	if p.assign(in, make(chan error, 1)) {
		t.Fatal("assigned a process that was already waited for")
	}
	if n := sink.count("started", 7); n != 0 {
		t.Fatalf("Started reported %d times", n)
	}
	q := &proc{id: 8, tail: &tailBuf{}}
	if !q.assign(in, make(chan error, 1)) || q.reap() != in || sink.count("started", 8) != 1 {
		t.Fatal("a live pre-started worker was not assigned")
	}
}

// Exit classification of pre-started and exec'd workers.
func TestUnexpectedExitRules(t *testing.T) {
	for _, c := range []struct {
		name                         string
		exec, acked, stopped, failed bool
		want                         bool
	}{
		{"spare exits on its own before the ack", false, false, false, false, false},
		{"adoption gave up before the ack (stdin)", false, false, true, false, false},
		{"start timeout before the ack", false, false, true, true, true},
		{"adopted worker crashes after the ack", false, true, false, false, true},
		{"adopted worker stopped", false, true, true, false, false},
		{"exec'd worker crashes during startup", true, false, false, false, true},
		{"exec'd worker stopped", true, true, true, false, false},
		{"exec'd startup error", true, true, true, true, true},
	} {
		p := &proc{exec: c.exec}
		p.specAcked.Store(c.acked)
		p.failed.Store(c.failed)
		p.stopped.Store(c.stopped)
		if got := p.unexpectedExit(); got != c.want {
			t.Errorf("%s: unexpected = %v, want %v", c.name, got, c.want)
		}
	}
}
