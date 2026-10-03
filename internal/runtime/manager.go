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

	once    sync.Once
	sockDir string
	initErr error
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
		if m.DataDir != "" && dir == filepath.Join(m.DataDir, "run") {
			// Private to this data dir: sockets left by an unclean exit are stale.
			stale, _ := filepath.Glob(filepath.Join(dir, "w*.sock"))
			for _, p := range stale {
				os.Remove(p)
			}
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
		ws: workerSpec{Flat: spec.Flat, Version: spec.Version, Dir: spec.Dir, Entry: spec.Entry,
			DataDir: spec.DataDir, Env: spec.Env, CacheDir: m.cacheDir(), TimeoutMS: m.Timeout.Milliseconds()},
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
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
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
}

type proc struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	done    chan struct{} // closed when the process exited and stderr drained
	err     error         // exit status
	started time.Time
	tail    *tailBuf
	rdone   chan struct{} // stderr fully read
	stopped atomic.Bool   // exit was requested
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

// spawn starts a worker process and waits for it to report ready.
func (in *instance) spawn(ctx context.Context) (err error) {
	os.Remove(in.sock)
	ln, err := net.Listen("unix", in.sock)
	if err != nil {
		return fmt.Errorf("worker socket: %w", err)
	}
	defer func() {
		if err != nil { // no worker serves it (e.g. a startup error)
			os.Remove(in.sock)
		}
	}()
	ul := ln.(*net.UnixListener)
	ul.SetUnlinkOnClose(false)
	lf, err := ul.File()
	ul.Close()
	if err != nil {
		os.Remove(in.sock)
		return fmt.Errorf("worker socket: %w", err)
	}
	defer lf.Close()

	cmd := in.m.command()
	cmd.ExtraFiles = []*os.File{lf} // fd 3
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	cmd.Stdout = nil // /dev/null
	if err := cmd.Start(); err != nil {
		os.Remove(in.sock)
		return fmt.Errorf("start worker: %w", err)
	}
	p := &proc{cmd: cmd, stdin: stdin, done: make(chan struct{}), started: time.Now(), tail: &tailBuf{}, rdone: make(chan struct{})}
	ready := make(chan error, 1)
	go in.readStderr(p, stderr, ready)
	go func() {
		<-p.rdone // Wait must not run before all reads from the pipe completed
		p.err = cmd.Wait()
		close(p.done)
		in.exited(p)
	}()
	b, _ := json.Marshal(in.ws)
	if _, err := stdin.Write(append(b, '\n')); err != nil {
		cmd.Process.Kill()
		<-p.done
		return fmt.Errorf("start worker: %w", err)
	}
	timeout := in.m.StartTimeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-ready:
		if err != nil {
			p.stopped.Store(true)
			cmd.Process.Kill()
			<-p.done
			return err
		}
	case <-p.done:
		msg := strings.TrimSpace(p.tail.String())
		if msg == "" {
			msg = fmt.Sprint(p.err)
		}
		return fmt.Errorf("server flat worker exited during startup: %s", msg)
	case <-timer.C:
		p.stopped.Store(true)
		cmd.Process.Kill()
		<-p.done
		return fmt.Errorf("server flat did not become ready within %s", timeout)
	case <-ctx.Done():
		p.stopped.Store(true)
		cmd.Process.Kill()
		<-p.done
		return ctx.Err()
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
		return fmt.Errorf("server flat worker exited right after starting: %v", p.err)
	default:
	}
	return nil
}

// readStderr forwards the worker's log lines and reports readiness.
func (in *instance) readStderr(p *proc, r io.Reader, ready chan<- error) {
	defer close(p.rdone)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	sentReady := false
	for sc.Scan() {
		line := sc.Text()
		var msg childMsg
		if strings.HasPrefix(line, "{") && json.Unmarshal([]byte(line), &msg) == nil && msg.T != "" {
			switch msg.T {
			case "ready":
				if !sentReady {
					sentReady = true
					ready <- nil
				}
			case "fatal":
				if !sentReady {
					sentReady = true
					ready <- fmt.Errorf("server flat failed to start: %s", msg.Msg)
				}
				in.spec.Log("error", msg.Msg)
			default:
				level := msg.Level
				if level == "" {
					level = "info"
				}
				in.spec.Log(level, msg.Msg)
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		p.tail.add(line)
		in.spec.Log("error", line)
	}
	io.Copy(io.Discard, r)
}

// exited is called when a worker process ends.
func (in *instance) exited(p *proc) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.p != p {
		return
	}
	in.p = nil
	in.tr.CloseIdleConnections()
	if p.stopped.Load() || in.stopped {
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
	err := in.spawn(context.Background())
	in.mu.Lock()
	in.starting = nil
	in.lastErr = err
	close(done)
	if in.stopped && in.p != nil {
		p := in.p
		in.p = nil
		in.mu.Unlock()
		in.terminate(p)
		os.Remove(in.sock) // Stop may have removed it before this spawn re-created it
		in.mu.Lock()
		return errStopped
	}
	if err != nil {
		in.failures++
		in.retryAt = time.Now().Add(backoff(in.failures))
		in.spec.Log("error", fmt.Sprintf("worker restart failed: %v", err))
		return err
	}
	in.spec.Log("info", fmt.Sprintf("worker restarted in %dms", time.Since(t0).Milliseconds()))
	return nil
}

func (in *instance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := in.ensure(r.Context()); err != nil {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Service Unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	in.proxy.ServeHTTP(w, r)
}

// Stop stops the worker (in-flight requests get a short grace period) and
// removes its socket.
func (in *instance) Stop() {
	in.mu.Lock()
	in.stopped = true
	p := in.p
	in.p = nil
	in.mu.Unlock()
	if p != nil {
		in.terminate(p)
	}
	in.tr.CloseIdleConnections()
	os.Remove(in.sock)
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
