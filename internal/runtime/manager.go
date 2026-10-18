package runtime

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gosuda/flats/internal/core"
)

// Manager starts server flats as "flats worker" child processes. It
// implements core.Runtime.
type Manager struct {
	DataDir      string               // runtime state (sockets, compilation cache); "" = temp dir
	Exe          string               // binary to re-exec; "" = os.Executable()
	Logf         func(string, ...any) // operational log; nil = log.Printf
	Args         []string             // worker argv after Exe; nil = ["worker"]
	Env          []string             // the worker's entire environment; nil = empty (tests add a dispatch variable)
	Timeout      time.Duration        // per request; 0 = DefaultTimeout
	StartTimeout time.Duration        // worker start; 0 = 60s
	// NoSpare disables the pre-started worker (tests sensitive to process
	// counts or to the timing of the exec path).
	NoSpare bool
	// UsageInterval is how often workers report CPU and memory; 0 = 15s.
	UsageInterval time.Duration

	once       sync.Once
	sockDir    string
	scratchDir string // parent of pre-started workers' database warm-up directories; "" = none
	initErr    error
	spare      spareState
	procs      atomic.Uint64 // process ids for RuntimeSpec.Usage, unique per Manager
}

var _ core.Runtime = (*Manager)(nil)

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

func (m *Manager) init() error {
	m.once.Do(func() {
		if m.Exe == "" {
			m.Exe, m.initErr = os.Executable()
			if m.initErr != nil {
				return
			}
		}
		// Unix socket paths are limited to ~104 bytes; keep them short.
		dir := ""
		if m.DataDir != "" {
			dir = filepath.Join(m.DataDir, "run")
		}
		if dir == "" || len(dir)+24 > 100 {
			dir = filepath.Join(os.TempDir(), fmt.Sprintf("flats-%d", os.Getuid()))
		}
		if m.initErr = privateDir(dir); m.initErr != nil {
			m.initErr = fmt.Errorf("runtime socket dir: %w", m.initErr)
			return
		}
		m.sockDir = dir
		scratch := filepath.Join(dir, "prewarm")
		if m.DataDir != "" && dir == filepath.Join(m.DataDir, "run") {
			// Private to this data dir: sockets left by an unclean exit are
			// stale, both instances' (w) and pre-started workers' (s), and
			// so is anything a pre-started worker left in its scratch
			// directory.
			for _, pattern := range []string{"w*.sock", "s*.sock"} {
				stale, _ := filepath.Glob(filepath.Join(dir, pattern))
				for _, p := range stale {
					os.Remove(p)
				}
			}
			os.RemoveAll(scratch)
		}
		// Pre-started workers warm up SQLite with throwaway databases in
		// their own subdirectories of it (newSpare); without it they skip
		// that.
		if privateDir(scratch) == nil {
			m.scratchDir = scratch
		}
	})
	return m.initErr
}

// privateDir creates dir (0700) or checks that an existing one is a real
// directory owned by this user and not accessible to others: the sockets in
// it carry every request to the workers.
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is owned by another user", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

func (m *Manager) cacheDir() string {
	if m.DataDir == "" {
		return ""
	}
	return filepath.Join(m.DataDir, "cache", "wazero")
}

// Start implements core.Runtime: it starts a worker and waits until it is
// ready (or returns its startup error, e.g. a syntax error in the script).
func (m *Manager) Start(ctx context.Context, spec core.RuntimeSpec) (core.Instance, error) {
	if spec.ManagedDocs {
		return m.startDocs(ctx, spec)
	}
	if spec.Lazy {
		return nil, errors.New("lazy runtime requires managed docs")
	}
	untrack := m.track()
	in, err := m.startChild(ctx, spec)
	if err != nil {
		untrack()
		return nil, err
	}
	in.untrack = untrack
	m.instanceReady()
	return in, nil
}

func (m *Manager) startChild(ctx context.Context, spec core.RuntimeSpec) (*instance, error) {
	if err := m.init(); err != nil {
		return nil, err
	}
	if spec.Log == nil {
		spec.Log = func(level, msg string) { m.logf("flat %s v%d: %s: %s", spec.Flat, spec.Version, level, msg) }
	}
	var rnd [8]byte
	rand.Read(rnd[:])
	in := &instance{
		m:    m,
		spec: spec,
		sock: filepath.Join(m.sockDir, "w"+hex.EncodeToString(rnd[:])+".sock"),
		ws: workerSpec{Flat: spec.Flat, Version: spec.Version, Generation: spec.Generation, Dir: spec.Dir, Entry: spec.Entry,
			DataDir: spec.DataDir, Env: spec.Env, NetworkOrigins: append([]string(nil), spec.NetworkOrigins...), CacheDir: m.cacheDir(), TimeoutMS: m.Timeout.Milliseconds(),
			ImagePath: spec.ImagePath, UsageIntervalMS: m.UsageInterval.Milliseconds()},
	}
	if in.ws.Env == nil {
		in.ws.Env = map[string]string{}
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", in.sock)
		},
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     60 * time.Second,
		DisableCompression:  true,
		// Backstop only: the worker answers within its request timeout.
		ResponseHeaderTimeout: in.ws.timeout() + 30*time.Second,
	}
	in.tr = tr
	in.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "flat"
			pr.Out.Host = pr.In.Host
			proto := pr.In.Header.Get("X-Forwarded-Proto")
			pr.SetXForwarded()
			// Portal terminates TLS in-process without a *tls.Conn and marks
			// requests with X-Forwarded-Proto: https; keep that scheme.
			if pr.In.TLS == nil && (proto == "https" || proto == "http") {
				pr.Out.Header.Set("X-Forwarded-Proto", proto)
			}
		},
		Transport:     tr,
		FlushInterval: -1,
		ModifyResponse: func(res *http.Response) error {
			// The worker closes its connection when it refuses a body it has
			// not read. Close the visitor's too: otherwise net/http would
			// first try to drain the rest of a stalled upload.
			if b, _ := res.Request.Context().Value(visitorBodyKey{}).(*visitorBody); b != nil && res.Close && !b.finished() {
				res.Header.Set("Connection", "close")
			}
			return nil
		},
		ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if b, _ := r.Context().Value(visitorBodyKey{}).(*visitorBody); b != nil && b.timedOut.Load() {
				w.Header().Set("Connection", "close")
				http.Error(w, "request body arrived too slowly", http.StatusRequestTimeout)
				return
			}
			in.spec.Log("error", fmt.Sprintf("%s %s: worker unreachable: %v", r.Method, r.URL.RequestURI(), err))
			http.Error(w, "Bad Gateway: the server flat is not responding.", http.StatusBadGateway)
		},
	}
	t0 := time.Now()
	if err := in.spawn(ctx); err != nil {
		return nil, err
	}
	in.spec.Log("info", fmt.Sprintf("worker started in %dms", time.Since(t0).Milliseconds()))
	return in, nil
}

// instance is a running server flat: one worker process at a time.
type instance struct {
	m     *Manager
	spec  core.RuntimeSpec
	ws    workerSpec
	sock  string
	tr    *http.Transport
	proxy *httputil.ReverseProxy

	mu       sync.Mutex
	p        *proc
	stopped  bool
	failures int
	retryAt  time.Time
	starting chan struct{} // non-nil while a restart is in progress
	lastErr  error         // result of the last restart
	untrack  func()        // ends the Manager's count of this instance; nil for docs children
}

type proc struct {
	id      uint64 // RuntimeSpec.Usage process id
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	done    chan struct{} // closed when the process exited and stderr drained
	err     error         // exit status
	started time.Time     // exec, or adoption of a pre-started worker
	tail    *tailBuf
	rdone   chan struct{} // stderr fully read
	stopped atomic.Bool   // exit was requested
	failed  atomic.Bool   // killed because startup failed: still an unexpected exit
	// exec is true for a process started for its instance, false for a
	// pre-started worker (adopted or not).
	exec    bool
	warmDir string // a pre-started worker's database warm-up directory, removed after exit; "" = none
	// A pre-started worker reports "warm" when it finished preparing and
	// "spec" once it received its flat (every worker sends the latter).
	warmAt    atomic.Int64 // UnixNano of "warm"; 0 before
	specAcked atomic.Bool

	// The instance the process serves and its readiness report; nil for a
	// pre-started worker until spawn adopts it.
	mu    sync.Mutex
	owner *instance
	ready chan<- error
	// reaped is set once the process was waited for; an owner can no longer
	// be assigned, so the exit is accounted for exactly the owner it had.
	reaped bool

	// Written by readStderr only; read by the Wait goroutine after rdone.
	// baseCPU is the CPU the process used before it received its spec (a
	// pre-started worker's warm-up): it is never the flat's.
	baseCPU      int64
	lastCPU      int64 // latest valid CPU sample since the spec (ns)
	badUsageSeen bool  // an invalid sample was logged
}

// assign gives a pre-started worker to in. Lines read from its stderr from
// now on are in's; earlier ones went to the Manager log. It returns false
// when the process was already waited for: it is gone and was not given in.
func (p *proc) assign(in *instance, ready chan<- error) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reaped {
		return false
	}
	p.tail.reset()
	p.started = time.Now()
	p.owner, p.ready = in, ready
	// Under p.mu, as reap is: Started always precedes the owner's Exited.
	if u := in.spec.Usage; u != nil {
		u.Started(p.id)
	}
	return true
}

// reap marks the process waited for and returns its final owner (nil for a
// pre-started worker nobody adopted).
func (p *proc) reap() *instance {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reaped = true
	return p.owner
}

func (p *proc) assigned() (*instance, chan<- error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.owner, p.ready
}

// tailBuf keeps the last lines of plain stderr output for error messages.
type tailBuf struct {
	mu    sync.Mutex
	lines []string
}

func (t *tailBuf) add(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, s)
	if len(t.lines) > 20 {
		t.lines = t.lines[len(t.lines)-20:]
	}
}

func (t *tailBuf) reset() {
	t.mu.Lock()
	t.lines = nil
	t.mu.Unlock()
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "\n")
}

func (m *Manager) command() *exec.Cmd {
	args := m.Args
	if args == nil {
		args = []string{"worker"}
	}
	cmd := exec.Command(m.Exe, args...)
	// A minimal environment: nothing of the parent's (HOME, tokens, PATH...).
	cmd.Env = append([]string{}, m.Env...)
	cmd.Dir = "/"
	return cmd
}

// spawn starts a worker process and waits for it to report ready. It adopts
// the Manager's pre-started worker when there is one and starts a new process
// when that is gone or fails before reporting ready or a startup error.
func (in *instance) spawn(ctx context.Context) error {
	if sp := in.m.takeSpare(); sp != nil {
		adopted, err := in.adopt(ctx, sp)
		in.m.refillSpare()
		if adopted {
			return err
		}
		in.m.logf("flat %s v%d: pre-started worker unavailable; starting a new one", in.spec.Flat, in.spec.Version)
	}
	return in.spawnExec(ctx)
}

// listenFile binds a Unix socket at path and returns its listening file
// for a child's fd 3. The socket file outlives the returned file.
func listenFile(path string) (*os.File, error) {
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("worker socket: %w", err)
	}
	ul := ln.(*net.UnixListener)
	ul.SetUnlinkOnClose(false)
	lf, err := ul.File()
	ul.Close()
	if err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("worker socket: %w", err)
	}
	return lf, nil
}

// startProc starts a worker process on the listening socket lf. owner is
// nil for a pre-started worker, whose warm-up directory warmDir is removed
// after the process exited, before done is closed.
func (m *Manager) startProc(lf *os.File, owner *instance, ready chan<- error, warmDir string) (*proc, error) {
	cmd := m.command()
	cmd.ExtraFiles = []*os.File{lf} // fd 3
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = nil // /dev/null
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start worker: %w", err)
	}
	p := &proc{id: m.procs.Add(1), cmd: cmd, stdin: stdin, done: make(chan struct{}), started: time.Now(), tail: &tailBuf{}, rdone: make(chan struct{}), warmDir: warmDir, owner: owner, ready: ready, exec: owner != nil}
	if owner != nil && owner.spec.Usage != nil {
		owner.spec.Usage.Started(p.id)
	}
	go p.readStderr(m, stderr)
	go func() {
		<-p.rdone // Wait must not run before all reads from the pipe completed
		p.err = cmd.Wait()
		// Exit accounting runs before done closes: Stop returns on done, so
		// once it returns the final CPU is in the sink. exited() is not the
		// place, as it returns early for stopped or superseded processes.
		owner := p.reap()
		if owner != nil && owner.spec.Usage != nil {
			owner.spec.Usage.Exited(p.id, p.finalCPU(), p.unexpectedExit())
		}
		if p.warmDir != "" {
			// The worker removes it itself unless it was killed or
			// crashed during the warm-up.
			os.RemoveAll(p.warmDir)
		}
		close(p.done)
		if owner != nil {
			owner.exited(p)
		} else {
			m.spareExited(p)
		}
	}()
	return p, nil
}

// spawnExec starts a new worker process.
func (in *instance) spawnExec(ctx context.Context) (err error) {
	lf, err := listenFile(in.sock)
	if err != nil {
		return err
	}
	defer lf.Close()
	defer func() {
		if err != nil { // no worker serves it (e.g. a startup error)
			os.Remove(in.sock)
		}
	}()
	ready := make(chan error, 1)
	p, err := in.m.startProc(lf, in, ready, "")
	if err != nil {
		return err
	}
	b, _ := json.Marshal(in.ws)
	if _, err := p.stdin.Write(append(b, '\n')); err != nil {
		p.failed.Store(true)
		p.stopped.Store(true)
		p.cmd.Process.Kill()
		<-p.done
		return fmt.Errorf("start worker: %w", err)
	}
	_, err = in.await(ctx, p, ready, false)
	return err
}

// adopt gives the pre-started worker sp this instance's socket path and
// spec. adopted is false when sp could not start the flat for reasons of its
// own (it had exited, the socket could not be moved, the spec could not be
// written, or it exited before acknowledging the spec); sp is then gone and
// the caller starts a new process. Once sp acknowledged the spec, the flat's
// startup has begun in it, and its outcome is the flat's, as for a new
// process.
func (in *instance) adopt(ctx context.Context, sp *spare) (adopted bool, err error) {
	p := sp.p
	select {
	case <-p.done:
		os.Remove(sp.sock)
		return false, nil
	default:
	}
	// The bound socket follows its file: a rename moves the worker's listener
	// to this instance's path.
	if err := os.Rename(sp.sock, in.sock); err != nil {
		sp.stop()
		return false, nil
	}
	ready := make(chan error, 1)
	if !p.assign(in, ready) { // it exited while being moved
		os.Remove(in.sock)
		return false, nil
	}
	b, _ := json.Marshal(in.ws)
	if _, err := p.stdin.Write(append(b, '\n')); err != nil {
		p.stopped.Store(true)
		p.cmd.Process.Kill()
		<-p.done
		os.Remove(in.sock)
		return false, nil
	}
	adopted, err = in.await(ctx, p, ready, true)
	if err != nil || !adopted {
		os.Remove(in.sock)
	}
	return adopted, err
}

// await waits until p, which received its spec, reports ready. For an
// adopted worker an exit before it acknowledged the spec is not the flat's
// failure: adopted is false and the caller starts a new process instead.
func (in *instance) await(ctx context.Context, p *proc, ready <-chan error, isAdopted bool) (adopted bool, err error) {
	timeout := in.m.StartTimeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-ready:
		if err != nil {
			p.failed.Store(true)
			p.stopped.Store(true)
			p.cmd.Process.Kill()
			<-p.done
			return true, err
		}
	case <-p.done:
		if isAdopted {
			select {
			case err := <-ready:
				if err != nil { // a startup error, then the exit
					return true, err
				}
			default:
			}
			if !p.specAcked.Load() { // readStderr finished before done closed
				return false, nil
			}
		}
		msg := strings.TrimSpace(p.tail.String())
		if msg == "" {
			msg = fmt.Sprint(p.err)
		}
		return true, fmt.Errorf("server flat worker exited during startup: %s", msg)
	case <-timer.C:
		p.failed.Store(true)
		p.stopped.Store(true)
		p.cmd.Process.Kill()
		<-p.done
		return true, fmt.Errorf("server flat did not become ready within %s", timeout)
	case <-ctx.Done():
		p.stopped.Store(true)
		p.cmd.Process.Kill()
		<-p.done
		return true, ctx.Err()
	}
	in.mu.Lock()
	in.p = p
	in.mu.Unlock()
	select {
	case <-p.done: // died between "ready" and now: exited() ignored it
		in.mu.Lock()
		if in.p == p {
			in.p = nil
		}
		in.mu.Unlock()
		return true, fmt.Errorf("server flat worker exited right after starting: %v", p.err)
	default:
	}
	return true, nil
}

// readStderr forwards the worker's log lines and reports readiness. Lines of
// a pre-started worker that has no instance yet go to the Manager log.
func (p *proc) readStderr(m *Manager, r io.Reader) {
	defer close(p.rdone)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	sentReady := false
	for sc.Scan() {
		line := sc.Text()
		in, ready := p.assigned()
		logf := func(level, msg string) {
			if in != nil {
				in.spec.Log(level, msg)
			} else {
				m.logf("pre-started worker %d: %s: %s", p.cmd.Process.Pid, level, msg)
			}
		}
		var msg childMsg
		if strings.HasPrefix(line, "{") && json.Unmarshal([]byte(line), &msg) == nil && msg.T != "" {
			switch msg.T {
			case "warm": // a pre-started worker finished preparing
				p.warmAt.Store(time.Now().UnixNano())
			case "spec": // the worker received its spec
				if msg.CPU != nil && *msg.CPU >= 0 {
					p.baseCPU = *msg.CPU
				}
				p.specAcked.Store(true)
			case "usage":
				if in != nil {
					in.usage(p, msg)
				}
			case "ready":
				if in != nil && !sentReady {
					sentReady = true
					ready <- nil
				}
			case "fatal":
				if in != nil && !sentReady {
					sentReady = true
					ready <- fmt.Errorf("server flat failed to start: %s", msg.Msg)
				}
				logf("error", msg.Msg)
			default:
				level := msg.Level
				if level == "" {
					level = "info"
				}
				logf(level, msg.Msg)
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		p.tail.add(line)
		logf("error", line)
	}
	io.Copy(io.Discard, r)
}

// finalCPU is the flat's share of the process's CPU once it exited: its
// whole CPU since the spec. A pre-started worker that never acknowledged a
// spec ran nothing of the flat. Called by the Wait goroutine after rdone.
func (p *proc) finalCPU() int64 {
	if !p.exec && !p.specAcked.Load() {
		return 0
	}
	cpu := p.lastCPU
	if ps := p.cmd.ProcessState; ps != nil {
		cpu = max(cpu, (ps.UserTime()+ps.SystemTime()).Nanoseconds()-p.baseCPU)
	}
	return cpu
}

// unexpectedExit reports whether the exit was neither asked for nor part of
// a pre-started worker's life before it took a flat's spec. A startup the
// host killed because it failed is unexpected.
func (p *proc) unexpectedExit() bool {
	// stopped is read first: startup paths store failed before stopped, so
	// seeing stopped also guarantees seeing failed.
	stopped := p.stopped.Load()
	if p.failed.Load() {
		return true // the flat's startup failed, also in an adopted worker
	}
	if !p.exec && !p.specAcked.Load() {
		return false // the spare's own exit; spawn starts a new process
	}
	return !stopped
}

// usage forwards one valid usage sample, as CPU since the spec: a CPU time
// that is present, non-negative and not below the process's previous
// sample, and a non-negative memory figure when one is present. An invalid
// sample is dropped and logged once per process.
func (in *instance) usage(p *proc, msg childMsg) {
	var cpu int64 = -1
	if msg.CPU != nil {
		cpu = *msg.CPU - p.baseCPU
	}
	if cpu < 0 || cpu < p.lastCPU || (msg.Mem != nil && *msg.Mem < 0) {
		if !p.badUsageSeen {
			p.badUsageSeen = true
			in.m.logf("flat %s v%d: worker %d sent an invalid usage sample; dropped", in.spec.Flat, in.spec.Version, p.id)
		}
		return
	}
	p.lastCPU = cpu
	if u := in.spec.Usage; u != nil {
		var mem int64
		if msg.Mem != nil {
			mem = *msg.Mem
		}
		u.Sample(p.id, cpu, mem, msg.Mem != nil)
	}
}

// WorkerState implements core.StateReporter from the instance's own state,
// without waking or messaging the worker.
func (in *instance) WorkerState() core.WorkerState {
	in.mu.Lock()
	defer in.mu.Unlock()
	switch {
	case in.stopped:
		return core.WorkerStopped
	case in.p != nil:
		return core.WorkerRunning
	case in.starting != nil:
		return core.WorkerStarting
	}
	// The worker exited or failed to restart; the next request restarts it,
	// after its backoff.
	return core.WorkerBackoff
}

var _ core.StateReporter = (*instance)(nil)

// exited is called when a worker process ends.
func (in *instance) exited(p *proc) {
	in.mu.Lock()
	if in.p != p {
		in.mu.Unlock()
		return
	}
	in.p = nil
	in.tr.CloseIdleConnections()
	if p.stopped.Load() || in.stopped {
		in.mu.Unlock()
		return
	}
	if time.Since(p.started) > time.Minute {
		in.failures = 0
	}
	in.failures++
	in.retryAt = time.Now().Add(backoff(in.failures - 1))
	msg := fmt.Sprint(p.err)
	if t := strings.TrimSpace(p.tail.String()); t != "" {
		msg += ": " + lastLines(t, 5)
	}
	in.mu.Unlock()
	in.spec.Log("error", fmt.Sprintf("worker exited unexpectedly (%s); it restarts on the next request", msg))
}

func lastLines(s string, n int) string {
	l := strings.Split(s, "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, " | ")
}

func backoff(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	d := 250 * time.Millisecond << min(n-1, 8)
	return min(d, 30*time.Second)
}

var errStopped = errors.New("server flat is stopped")

// maxRestartWait: a request waits for a restart backoff up to this long;
// longer backoffs answer 503 immediately.
const maxRestartWait = 2 * time.Second

// ensure makes sure a worker is running, restarting a dead one (with
// backoff). Concurrent callers wait for the same restart.
func (in *instance) ensure(ctx context.Context) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	for {
		if in.stopped {
			return errStopped
		}
		if in.p != nil {
			return nil
		}
		if in.starting == nil {
			wait := time.Until(in.retryAt)
			if wait <= 0 {
				break
			}
			if wait > maxRestartWait {
				return fmt.Errorf("server flat is restarting (retry in %s)", wait.Round(100*time.Millisecond))
			}
			in.mu.Unlock()
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				in.mu.Lock()
				return ctx.Err()
			}
			in.mu.Lock()
			continue
		}
		ch := in.starting
		in.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			in.mu.Lock()
			return ctx.Err()
		}
		in.mu.Lock()
		if in.p == nil && !in.stopped && in.lastErr != nil {
			return in.lastErr
		}
	}
	done := make(chan struct{})
	in.starting = done
	in.mu.Unlock()
	t0 := time.Now()
	// Not the request's context: a restart serves every waiting request.
	var err error
	if in.spec.NextGeneration != nil {
		in.ws.Generation, err = in.spec.NextGeneration(context.Background())
	}
	if err == nil {
		err = in.spawn(context.Background())
	}
	in.mu.Lock()
	in.lastErr = err
	if in.stopped {
		p := in.p
		in.p = nil
		in.mu.Unlock()
		if p != nil {
			in.terminate(p)
		}
		os.Remove(in.sock) // Stop may have removed it before spawn re-created it
		in.mu.Lock()
		in.starting = nil
		close(done) // Stop waits for termination, not just the spawn result.
		return errStopped
	}
	in.starting = nil
	close(done)
	if err != nil {
		in.failures++
		in.retryAt = time.Now().Add(backoff(in.failures))
		in.mu.Unlock()
		in.spec.Log("error", fmt.Sprintf("worker restart failed: %v", err))
		in.mu.Lock()
		return err
	}
	in.mu.Unlock()
	in.spec.Log("info", fmt.Sprintf("worker restarted in %dms", time.Since(t0).Milliseconds()))
	in.mu.Lock()
	return nil
}

func (in *instance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := in.ensure(r.Context()); err != nil {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Service Unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	in.serveChild(w, r)
}

// serveChild never restarts: managed docs leases target this exact socket.
func (in *instance) serveChild(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength != 0 && r.Body != nil && r.Body != http.NoBody {
		// The worker's own body deadline only ends its side of the proxy;
		// the transport would still wait for a stalled visitor. Apply the
		// same minimum upload rate to the visitor's connection.
		b := &visitorBody{body: r.Body, rc: http.NewResponseController(w), start: time.Now()}
		r = r.WithContext(context.WithValue(r.Context(), visitorBodyKey{}, b))
		r.Body = b
	}
	in.proxy.ServeHTTP(w, r)
}

type visitorBodyKey struct{}

// visitorBody bounds a visitor's request body to the minimum upload rate
// with read deadlines on the visitor's connection.
type visitorBody struct {
	body     io.ReadCloser
	rc       *http.ResponseController
	start    time.Time
	n        int64 // bytes read; only Read uses it
	timedOut atomic.Bool

	mu      sync.Mutex
	noLimit bool // the connection does not support read deadlines
	eof     bool
	closed  bool
}

func (b *visitorBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if !b.eof && !b.closed && !b.noLimit && b.rc.SetReadDeadline(bodyDeadline(b.start, b.n)) != nil {
		b.noLimit = true
	}
	b.mu.Unlock()
	n, err := b.body.Read(p)
	b.n += int64(n)
	b.mu.Lock()
	defer b.mu.Unlock()
	if err == io.EOF && !b.eof && !b.closed && !b.noLimit {
		// A deadline left on the connection would end the request's
		// context while the response is copied.
		b.rc.SetReadDeadline(time.Time{})
	}
	b.eof = b.eof || err == io.EOF
	var ne net.Error
	if !b.closed && errors.As(err, &ne) && ne.Timeout() {
		b.timedOut.Store(true)
	}
	return n, err
}

// finished reports whether the whole body was read.
func (b *visitorBody) finished() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.eof
}

// Close ends a read that still waits for the visitor: the proxy is done with
// the body, and the connection is not reused after an unfinished body.
func (b *visitorBody) Close() error {
	b.mu.Lock()
	if !b.eof && !b.closed && !b.noLimit {
		b.rc.SetReadDeadline(time.Now())
	}
	b.closed = true
	b.mu.Unlock()
	return b.body.Close()
}

// Stop stops the worker (in-flight requests get a short grace period) and
// removes its socket.
func (in *instance) Stop() {
	in.mu.Lock()
	in.stopped = true
	p := in.p
	in.p = nil
	starting := in.starting
	in.mu.Unlock()
	if p != nil {
		in.terminate(p)
	}
	if starting != nil {
		<-starting // A concurrent restart may spawn another worker.
	}
	in.tr.CloseIdleConnections()
	os.Remove(in.sock)
	if in.untrack != nil {
		in.untrack()
	}
}

func (in *instance) terminate(p *proc) {
	p.stopped.Store(true)
	p.stdin.Close() // the worker shuts down gracefully on stdin EOF
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		p.cmd.Process.Kill()
		<-p.done
	}
}

// pid returns the current worker's process id (tests).
func (in *instance) pid() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.p == nil {
		return 0
	}
	return in.p.cmd.Process.Pid
}
