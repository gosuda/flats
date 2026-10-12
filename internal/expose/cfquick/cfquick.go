// Package cfquick serves public flats through Cloudflare quick tunnels: an
// anonymous tunnel on a random *.trycloudflare.com hostname, with no
// Cloudflare account and no domain. The tunnel runs in this process through
// libtunnel (an in-process cloudflared), so no cloudflared binary is needed.
//
// Each served slug gets its own tunnel and loopback origin. The tunnel's
// credentials are recorded under Config.Dir so a restart or a later Public
// transition replays the same tunnel, and its hostname, while Cloudflare
// still has it. Cloudflare reaps idle quick tunnels; a reaped tunnel is
// replaced by a new one on a new hostname.
package cfquick

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/core"
	"github.com/oesni/libtunnel"
	v1 "github.com/oesni/libtunnel/v1"
)

// Kind is the NetStatus kind of this backend.
const Kind = "cloudflare-quick"

// Provider is the quick-tunnel mint endpoint, Cloudflare's own.
const Provider = "api.trycloudflare.com"

// Detail describes the service's limits for the console.
const Detail = "anonymous Cloudflare quick tunnels on trycloudflare.com: no account or domain, a random hostname, no uptime guarantee, up to 200 concurrent requests per flat and no server-sent events"

const (
	defaultShutdownTimeout = 30 * time.Second
	defaultMintTimeout     = 90 * time.Second
)

// Backoff between attempts to replace a failed tunnel; vars for tests.
var (
	minBackoff = 2 * time.Second
	maxBackoff = time.Minute
)

// Config configures Net.
type Config struct {
	// Dir holds the tunnel records, one <slug>.json per served slug (mode
	// 0600). It is created with mode 0700.
	Dir string
	// Logf receives operational messages. Nil is silent.
	Logf func(string, ...any)
	// ShutdownTimeout bounds stopping one tunnel (default 30s).
	ShutdownTimeout time.Duration
	// MintTimeout bounds Serve's wait for a tunnel hostname (default 90s).
	MintTimeout time.Duration
}

// session is one tunnel attempt. The real one wraps a libtunnel tunnel.
type session interface {
	// Hostname resolves the tunnel's spec, minting or replaying it, and
	// blocks until then. Empty means it failed; Err says why.
	Hostname() string
	// Serialize is the resolved spec, credentials included.
	Serialize() string
	// Start connects the tunnel to origin and reports its events.
	Start(origin *url.URL, events func(v1.Event))
	Err() error
	// Done closes once the tunnel ended and let go of the edge.
	Done() <-chan struct{}
	// Cancel ends the tunnel deliberately.
	Cancel()
}

// opener starts an unresolved session that replays spec ("" mints fresh).
type opener func(spec string, log *slog.Logger) session

// Net serves public flats through quick tunnels.
type Net struct {
	cfg  Config
	logf func(string, ...any)
	open opener

	mu      sync.Mutex
	closed  bool
	entries map[string]*entry
	locks   map[string]*sync.Mutex
	ops     sync.WaitGroup
}

type entry struct {
	slug    string
	handler *handlerBox
	ln      net.Listener
	srv     *http.Server
	origin  *url.URL
	stop    chan struct{} // closed by Stop
	done    chan struct{} // closed when supervise returned

	mu       sync.Mutex
	sess     session
	hostname string
	state    string // starting | ready | error
	detail   string
	gone     bool // the edge said the tunnel no longer exists
	stopping bool
}

type handlerBox struct {
	mu sync.RWMutex
	h  http.Handler
}

func (b *handlerBox) get() http.Handler {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.h
}

func (b *handlerBox) set(h http.Handler) {
	b.mu.Lock()
	b.h = h
	b.mu.Unlock()
}

// New prepares the record directory and makes libtunnel ignore the process
// environment, so an inherited LIBTUNNEL_* variable cannot redirect a tunnel
// and no tunnel secret is exported to processes Flats starts. It does not
// contact Cloudflare.
func New(cfg Config) (*Net, error) {
	if cfg.Dir == "" {
		return nil, errors.New("cloudflare-quick: record directory is required")
	}
	// The directory is <data dir>/cloudflare-quick. Accept only a clean
	// absolute path, so nothing that reaches the host's configuration can
	// point the tunnel credentials elsewhere through a relative or ".."
	// path.
	cfg.Dir = filepath.Clean(cfg.Dir)
	if !filepath.IsAbs(cfg.Dir) || strings.Contains(cfg.Dir, "..") {
		return nil, fmt.Errorf("cloudflare-quick: record directory %q must be an absolute path without ..", cfg.Dir)
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("cloudflare-quick: record directory: %w", err)
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = defaultShutdownTimeout
	}
	if cfg.MintTimeout <= 0 {
		cfg.MintTimeout = defaultMintTimeout
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	v1.SetEnvironment(v1.Isolated)
	return &Net{cfg: cfg, logf: logf, open: libtunnelOpener, entries: map[string]*entry{}, locks: map[string]*sync.Mutex{}}, nil
}

var slugRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validSlug(slug string) error {
	if !slugRE.MatchString(slug) {
		return fmt.Errorf("cloudflare-quick: invalid slug %q", slug)
	}
	return nil
}

func (n *Net) slugLock(slug string) *sync.Mutex {
	n.mu.Lock()
	defer n.mu.Unlock()
	l := n.locks[slug]
	if l == nil {
		l = new(sync.Mutex)
		n.locks[slug] = l
	}
	return l
}

func (n *Net) beginOp() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return false
	}
	n.ops.Add(1)
	return true
}

// Serve serves h on slug's quick tunnel and returns its public URL. A slug
// already served keeps its tunnel and serves h from now on. A new tunnel's
// hostname is known when Serve returns; its edge connection comes up in the
// background (starting, then ready).
func (n *Net) Serve(ctx context.Context, slug string, h http.Handler) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if h == nil {
		return "", errors.New("cloudflare-quick: handler is nil")
	}
	if err := validSlug(slug); err != nil {
		return "", err
	}
	if !n.beginOp() {
		return "", errors.New("cloudflare-quick: closed")
	}
	defer n.ops.Done()
	l := n.slugLock(slug)
	l.Lock()
	defer l.Unlock()

	n.mu.Lock()
	e := n.entries[slug]
	n.mu.Unlock()
	if e != nil {
		e.mu.Lock()
		stopping := e.stopping
		e.mu.Unlock()
		if !stopping {
			e.handler.set(h)
			return n.URL(slug), nil
		}
		// A Stop that could not confirm the tunnel closed left the entry for
		// a retry. Finish it before opening a new tunnel.
		if err := n.stopEntry(e); err != nil {
			return "", err
		}
		n.mu.Lock()
		delete(n.entries, slug)
		n.mu.Unlock()
	}

	spec, err := n.loadRecord(slug)
	if err != nil {
		return "", err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("cloudflare-quick: origin listener: %w", err)
	}
	box := &handlerBox{h: h}
	e = &entry{
		slug:    slug,
		handler: box,
		ln:      ln,
		srv:     &http.Server{Handler: publicHandler(box), ReadHeaderTimeout: 30 * time.Second},
		origin:  &url.URL{Scheme: "http", Host: ln.Addr().String()},
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		state:   "starting",
	}
	go func() {
		if err := e.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			n.logf("cloudflare-quick: %s: origin server stopped: %v", slug, err)
		}
	}()

	mintCtx, cancel := context.WithTimeout(ctx, n.cfg.MintTimeout)
	defer cancel()
	sess, host, err := n.resolve(mintCtx, e, spec)
	if err != nil && spec != "" && !errors.Is(err, context.Canceled) && mintCtx.Err() == nil {
		// The recorded tunnel could not be replayed; mint a new one.
		n.logf("cloudflare-quick: %s: recorded tunnel not usable, minting a new one: %v", slug, err)
		sess, host, err = n.resolve(mintCtx, e, "")
	}
	if err != nil {
		shutdownServer(e.srv, n.cfg.ShutdownTimeout)
		return "", fmt.Errorf("cloudflare-quick: %s: %w", slug, err)
	}
	n.recordSpec(slug, spec, sess)

	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		sess.Cancel()
		shutdownServer(e.srv, n.cfg.ShutdownTimeout)
		return "", errors.New("cloudflare-quick: closed")
	}
	n.entries[slug] = e
	n.mu.Unlock()

	n.start(e, sess, host) // e is new and unstopped; the slug lock excludes Stop
	go n.supervise(e)
	return "https://" + host, nil
}

// resolve opens a session replaying spec and waits for its hostname under
// ctx. On failure the session is cancelled.
func (n *Net) resolve(ctx context.Context, e *entry, spec string) (session, string, error) {
	sess := n.open(spec, n.tunnelLogger(e.slug))
	// A spec that could not even be parsed yields a tunnel that has already
	// failed: report that instead of resolving it.
	if err := sess.Err(); err != nil {
		sess.Cancel()
		return nil, "", err
	}
	type result struct {
		host string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		// A panic here would take the whole host down; make it this
		// tunnel's failure instead.
		defer func() {
			if r := recover(); r != nil {
				got <- result{err: fmt.Errorf("tunnel failed: %v", r)}
			}
		}()
		got <- result{host: sess.Hostname()}
	}()
	select {
	case r := <-got:
		if r.host == "" {
			err := r.err
			if err == nil {
				err = sess.Err()
			}
			if err == nil {
				err = errors.New("no hostname")
			}
			sess.Cancel()
			return nil, "", err
		}
		return sess, r.host, nil
	case <-ctx.Done():
		sess.Cancel()
		return nil, "", ctx.Err()
	case <-e.stop:
		sess.Cancel()
		return nil, "", errors.New("stopped")
	}
}

// start makes sess the entry's tunnel and connects it to the origin. Once a
// Stop has begun it cancels sess instead and reports false, so a replacement
// minted while stopping never runs untracked.
func (n *Net) start(e *entry, sess session, host string) bool {
	e.mu.Lock()
	if e.stopping {
		e.mu.Unlock()
		sess.Cancel()
		return false
	}
	e.sess, e.hostname, e.gone = sess, host, false
	e.state, e.detail = "starting", "connecting to the Cloudflare edge"
	e.mu.Unlock()
	sess.Start(e.origin, func(ev v1.Event) { n.event(e, sess, ev) })
	return true
}

func (n *Net) event(e *entry, sess session, ev v1.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sess != sess {
		return // an earlier tunnel's late event
	}
	switch ev.Kind {
	case v1.EventEstablished:
		e.state, e.detail = "ready", "ready at https://"+e.hostname
	case v1.EventDisconnected:
		if !e.stopping {
			e.state, e.detail = "starting", "reconnecting to the Cloudflare edge"
		}
	case v1.EventGone:
		// Reconnecting will not bring the tunnel back; supervise mints a
		// new one when this one ends.
		e.gone = true
		e.state, e.detail = "starting", "Cloudflare no longer has this tunnel; creating a new one"
		go sess.Cancel()
	case v1.EventError:
		if ev.Err != nil {
			e.state, e.detail = "error", ev.Err.Error()
		}
	}
}

// supervise replaces a tunnel that ended without a Stop: a reaped tunnel by a
// new one, a failed one after a backoff.
func (n *Net) supervise(e *entry) {
	defer close(e.done)
	backoff := minBackoff
	for {
		e.mu.Lock()
		sess := e.sess
		e.mu.Unlock()
		select {
		case <-e.stop:
			return
		case <-sess.Done():
		}
		e.mu.Lock()
		gone, stopping := e.gone, e.stopping
		if !stopping && !gone {
			detail := "tunnel ended"
			if err := sess.Err(); err != nil && !errors.Is(err, v1.ErrClosed) {
				detail = err.Error()
			}
			e.state, e.detail = "error", detail+"; retrying"
		}
		e.mu.Unlock()
		if stopping {
			return
		}
		spec := ""
		if gone || errors.Is(sess.Err(), v1.ErrCredentialRejected) {
			n.removeRecord(e.slug)
			backoff = minBackoff
		} else {
			if err := sleep(e.stop, backoff); err != nil {
				return
			}
			backoff = min(2*backoff, maxBackoff)
			var err error
			if spec, err = n.loadRecord(e.slug); err != nil {
				n.logf("cloudflare-quick: %s: %v", e.slug, err)
				spec = ""
			}
		}
		for {
			ctx, cancel := context.WithTimeout(context.Background(), n.cfg.MintTimeout)
			next, host, err := n.resolve(ctx, e, spec)
			cancel()
			if err == nil {
				if old := e.currentHost(); old != host {
					n.logf("cloudflare-quick: %s: new public URL https://%s (was https://%s)", e.slug, host, old)
				}
				n.recordSpec(e.slug, spec, next)
				if !n.start(e, next, host) {
					return
				}
				break
			}
			e.mu.Lock()
			if e.stopping {
				e.mu.Unlock()
				return
			}
			e.state, e.detail = "error", err.Error()+"; retrying"
			e.mu.Unlock()
			spec = ""
			if err := sleep(e.stop, backoff); err != nil {
				return
			}
			backoff = min(2*backoff, maxBackoff)
		}
	}
}

func (e *entry) currentHost() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hostname
}

func sleep(stop <-chan struct{}, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-stop:
		return errors.New("stopped")
	case <-t.C:
		return nil
	}
}

// Stop closes slug's tunnel and origin. Its record is kept, so serving the
// slug again replays the same tunnel while Cloudflare has it. An error means
// the tunnel could not be confirmed closed; the entry stays for a retry.
func (n *Net) Stop(slug string) error {
	if err := validSlug(slug); err != nil {
		return err
	}
	if !n.beginOp() {
		return nil // Close owns the remaining entries.
	}
	defer n.ops.Done()
	return n.stop(slug)
}

func (n *Net) stop(slug string) error {
	l := n.slugLock(slug)
	l.Lock()
	defer l.Unlock()
	n.mu.Lock()
	e := n.entries[slug]
	n.mu.Unlock()
	if e == nil {
		return nil
	}
	if err := n.stopEntry(e); err != nil {
		return err
	}
	n.mu.Lock()
	if n.entries[slug] == e {
		delete(n.entries, slug)
	}
	n.mu.Unlock()
	return nil
}

// stopEntry ends e's tunnel, waits for it to let go of the edge, then drains
// the origin. It can be called again after a failure.
func (n *Net) stopEntry(e *entry) error {
	e.mu.Lock()
	if !e.stopping {
		e.stopping = true
		close(e.stop)
	}
	e.mu.Unlock()
	timeout := time.NewTimer(n.cfg.ShutdownTimeout)
	defer timeout.Stop()
	select {
	case <-e.done:
	case <-timeout.C:
		return fmt.Errorf("cloudflare-quick: %s: tunnel supervisor did not stop within %s", e.slug, n.cfg.ShutdownTimeout)
	}
	// Read the tunnel only after supervise returned: a replacement it
	// installed before seeing the stop is the one to close.
	e.mu.Lock()
	sess := e.sess
	e.mu.Unlock()
	if sess != nil {
		sess.Cancel()
		select {
		case <-sess.Done():
		case <-timeout.C:
			return fmt.Errorf("cloudflare-quick: %s: tunnel did not close within %s", e.slug, n.cfg.ShutdownTimeout)
		}
	}
	if err := shutdownServer(e.srv, n.cfg.ShutdownTimeout); err != nil {
		return fmt.Errorf("cloudflare-quick: %s: origin shutdown: %w", e.slug, err)
	}
	e.mu.Lock()
	e.state, e.detail = "stopped", "tunnel closed"
	e.mu.Unlock()
	return nil
}

func shutdownServer(srv *http.Server, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := srv.Shutdown(ctx)
	if err != nil {
		_ = srv.Close()
	}
	return err
}

// Retire closes slug's tunnel and deletes its record, so the slug's next
// tunnel gets a new hostname.
func (n *Net) Retire(slug string) error {
	if err := n.Stop(slug); err != nil {
		return err
	}
	return RemoveRecord(n.cfg.Dir, slug)
}

// Close stops every tunnel. Later Serve calls fail.
func (n *Net) Close() error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	n.closed = true
	slugs := make([]string, 0, len(n.entries))
	for s := range n.entries {
		slugs = append(slugs, s)
	}
	n.mu.Unlock()
	n.ops.Wait()
	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	for _, s := range slugs {
		wg.Go(func() {
			if err := n.stop(s); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// URL is slug's public URL, empty when it is not served.
func (n *Net) URL(slug string) string {
	n.mu.Lock()
	e := n.entries[slug]
	n.mu.Unlock()
	if e == nil {
		return ""
	}
	if h := e.currentHost(); h != "" {
		return "https://" + h
	}
	return ""
}

// Status reports every served slug.
func (n *Net) Status() core.NetStatus {
	ns := core.NetStatus{Kind: Kind, Enabled: true, Detail: Detail, Hosts: []core.HostInfo{}}
	n.mu.Lock()
	entries := make([]*entry, 0, len(n.entries))
	for _, e := range n.entries {
		entries = append(entries, e)
	}
	n.mu.Unlock()
	slices.SortFunc(entries, func(a, b *entry) int { return strings.Compare(a.slug, b.slug) })
	for _, e := range entries {
		e.mu.Lock()
		hi := core.HostInfo{Host: e.slug, State: e.state, Detail: e.detail}
		if e.hostname != "" {
			hi.URL = "https://" + e.hostname
		}
		e.mu.Unlock()
		ns.Hosts = append(ns.Hosts, hi)
	}
	return ns
}

// publicHandler strips identity headers a visitor could forge; the public
// path never carries a Tailscale identity.
func publicHandler(b *handlerBox) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k := range r.Header {
			if isIdentityHeader(k) {
				delete(r.Header, k)
			}
		}
		b.get().ServeHTTP(w, r)
	})
}

// isIdentityHeader reports whether a request header name could be read as a
// Tailscale-User-* identity header, ignoring case and treating "_" as "-".
func isIdentityHeader(name string) bool {
	const prefix = "tailscale-user-"
	return len(name) >= len(prefix) && strings.EqualFold(strings.ReplaceAll(name[:len(prefix)], "_", "-"), prefix)
}

// tunnelLogger forwards libtunnel's warnings and errors to Logf.
func (n *Net) tunnelLogger(slug string) *slog.Logger {
	w := logWriter{logf: n.logf, prefix: "cloudflare-quick: " + slug + ": "}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slog.LevelWarn,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
}

type logWriter struct {
	logf   func(string, ...any)
	prefix string
}

func (w logWriter) Write(p []byte) (int, error) {
	w.logf("%s%s", w.prefix, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

var _ io.Writer = logWriter{}

// recordPath is slug's record file.
func recordPath(dir, slug string) string { return filepath.Join(dir, slug+".json") }

func (n *Net) loadRecord(slug string) (string, error) {
	b, err := os.ReadFile(recordPath(n.cfg.Dir, slug))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("cloudflare-quick: %s: read the tunnel record: %w", slug, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// recordSpec saves sess's spec when it differs from the replayed one. A
// failure is logged: the tunnel works, but a restart will mint a new one.
func (n *Net) recordSpec(slug, replayed string, sess session) {
	spec := sess.Serialize()
	if spec == "" || spec == replayed {
		return
	}
	if err := writeFileAtomic(recordPath(n.cfg.Dir, slug), []byte(spec+"\n")); err != nil {
		n.logf("cloudflare-quick: %s: tunnel record not saved, a restart gets a new hostname: %v", slug, err)
	}
}

func (n *Net) removeRecord(slug string) {
	if err := RemoveRecord(n.cfg.Dir, slug); err != nil {
		n.logf("cloudflare-quick: %s: %v", slug, err)
	}
}

// RemoveRecord deletes slug's tunnel record from dir. It needs no backend, so
// deleting a flat removes the record even with the provider off.
func RemoveRecord(dir, slug string) error {
	if err := validSlug(slug); err != nil {
		return err
	}
	if err := os.Remove(recordPath(dir, slug)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cloudflare-quick: %s: remove the tunnel record: %w", slug, err)
	}
	return nil
}

func writeFileAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// libtunnelOpener opens a real tunnel through Cloudflare's own quick-tunnel
// endpoint, with network-error reports kept on this machine.
func libtunnelOpener(spec string, log *slog.Logger) session {
	backend := libtunnel.Cloudflare().WithProvider(Provider).WithReports(false)
	t := libtunnel.FromWith(spec, backend).WithLogger(log)
	return &tunnelSession{t: t}
}

type tunnelSession struct {
	t    v1.Tunnel
	once sync.Once
	done chan struct{}
}

func (s *tunnelSession) Hostname() string  { return s.t.Hostname() }
func (s *tunnelSession) Serialize() string { return s.t.Serialize() }
func (s *tunnelSession) Err() error        { return s.t.Err() }
func (s *tunnelSession) Cancel()           { s.t.Cancel() }

func (s *tunnelSession) Start(origin *url.URL, events func(v1.Event)) {
	s.t.WithEventListener(events).WithLocalURL(origin)
}

func (s *tunnelSession) Done() <-chan struct{} {
	s.once.Do(func() {
		s.done = make(chan struct{})
		go func() {
			<-s.t.Done()
			close(s.done)
		}()
	})
	return s.done
}
