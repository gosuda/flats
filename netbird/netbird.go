// Package netbird serves flats through an embedded NetBird peer
// (github.com/netbirdio/netbird/client/embed, userspace networking, no
// root or TUN device).
//
// Two roles share one peer per host:
//
//   - Private: the flat is served as plain HTTP on a persisted TCP port of
//     the peer's overlay address, reachable by the other peers NetBird's
//     access policies allow. One listener per host; the Host header does
//     not route.
//   - Public: the flat is served on a separate overlay port and published
//     through NetBird's reverse proxy ("expose"). The proxy URL is public:
//     Flats sets no Pin, Password or UserGroups, because Public means anyone
//     on the internet may open it.
//
// The peer is enrolled once with a setup key (Enroll). Afterwards it
// restarts from the identity persisted under Config.Dir; no secret other
// than NetBird's own 0600 config file is stored.
package netbird

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosuda/flats/internal/core"
)

// Config configures a Net.
type Config struct {
	// Dir holds NetBird's config (peer identity), account.json and the
	// port records. Created 0700.
	Dir string
	// ManagementURL is informational; an enrolled account's own URL wins.
	ManagementURL string
	// DeviceName is the peer name used when Enroll is not given one.
	DeviceName string
	// Instance is the Flats host instance id.
	Instance string
	// ShutdownTimeout bounds session termination and HTTP drain
	// (default 30s).
	ShutdownTimeout time.Duration
	// Logf receives state changes and NetBird warnings. Nil discards them.
	Logf func(string, ...any)
}

const (
	stateStarting = "starting"
	stateReady    = "ready"
	stateError    = "error"

	rolePrivate = "private"
	rolePublic  = "public"
)

// startTimeout bounds the lazy client start on the first Serve.
var startTimeout = 60 * time.Second

// Net serves flats over an embedded NetBird peer.
type Net struct {
	cfg  Config
	logf func(string, ...any)

	retryMin, retryMax time.Duration

	cmu  sync.Mutex // guards c, acct and serializes client start
	c    client
	acct Account

	mu      sync.Mutex
	closed  bool
	entries map[string]*entry
	locks   map[string]*sync.Mutex
	ops     sync.WaitGroup
}

type entry struct {
	key, role, name string
	port            int
	c               client
	handler         *handlerBox
	cancel          context.CancelFunc
	done            chan struct{}
	stopping        atomic.Bool

	first     chan error // first outcome, buffered
	firstOnce sync.Once

	mu     sync.Mutex
	url    string
	state  string
	detail string
}

func (e *entry) signalFirst(err error) { e.firstOnce.Do(func() { e.first <- err }) }

func (e *entry) set(state, detail string) {
	e.mu.Lock()
	e.state, e.detail = state, detail
	e.mu.Unlock()
}

func (e *entry) getURL() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.url
}

type handlerBox struct {
	mu sync.RWMutex
	h  http.Handler
}

func (b *handlerBox) get() http.Handler  { b.mu.RLock(); defer b.mu.RUnlock(); return b.h }
func (b *handlerBox) set(h http.Handler) { b.mu.Lock(); b.h = h; b.mu.Unlock() }

// New prepares a Net. It never contacts NetBird: the peer starts on the first
// Serve. Without an enrolled account New succeeds and Serve fails with
// ErrNotConnected.
func New(cfg Config) (*Net, error) {
	if cfg.Dir == "" {
		return nil, errors.New("netbird: state directory is required")
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 30 * time.Second
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Net{cfg: cfg, logf: logf, retryMin: 2 * time.Second, retryMax: time.Minute,
		entries: map[string]*entry{}, locks: map[string]*sync.Mutex{}}, nil
}

type logWriter struct{ logf func(string, ...any) }

func (w logWriter) Write(p []byte) (int, error) {
	if s := strings.TrimSpace(string(p)); s != "" {
		w.logf("netbird: %s", s)
	}
	return len(p), nil
}

func (n *Net) lockFor(key string) *sync.Mutex {
	n.mu.Lock()
	defer n.mu.Unlock()
	l := n.locks[key]
	if l == nil {
		l = &sync.Mutex{}
		n.locks[key] = l
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

// ensureClient starts the embedded peer once. A failed start is retried by
// the next Serve.
func (n *Net) ensureClient(ctx context.Context) (client, error) {
	n.cmu.Lock()
	defer n.cmu.Unlock()
	if n.c != nil {
		return n.c, nil
	}
	acct, ok, err := LoadAccount(n.cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("netbird: %w", err)
	}
	if !ok {
		return nil, ErrNotConnected
	}
	var out io.Writer = io.Discard
	if n.cfg.Logf != nil {
		out = logWriter{n.logf}
	}
	c, err := newClient(clientOptions{
		DeviceName: acct.DeviceName, ManagementURL: acct.ManagementURL,
		ConfigPath: filepath.Join(n.cfg.Dir, configFile), StatePath: filepath.Join(n.cfg.Dir, dnsFile),
		LogOutput: out,
	})
	if err != nil {
		return nil, fmt.Errorf("netbird: %w", err)
	}
	sctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	if err := c.Start(sctx); err != nil {
		return nil, fmt.Errorf("netbird: connect to NetBird: %w", err)
	}
	n.c, n.acct = c, acct
	return c, nil
}

// Account returns the enrolled peer's fingerprint for policy tokens, or ""
// when this host is not enrolled.
func (n *Net) Account() string {
	n.cmu.Lock()
	defer n.cmu.Unlock()
	if n.c != nil {
		return n.acct.Fingerprint
	}
	if a, ok, _ := LoadAccount(n.cfg.Dir); ok {
		return a.Fingerprint
	}
	return ""
}

func key(role, name string) string { return role + "/" + name }

// ServePrivate serves h as plain HTTP on this host's overlay port for host and
// returns its URL (peer FQDN, or the overlay IP, plus the port). Serving a
// host that is already served replaces its handler.
func (n *Net) ServePrivate(ctx context.Context, host string, h http.Handler) (string, error) {
	return n.serve(ctx, rolePrivate, host, h)
}

// ServePublic serves h on an overlay port and publishes it through NetBird's
// reverse proxy. It returns the public URL once the proxy accepted the
// service. Public routes carry no Pin or Password. If the keep-alive or the
// listener fails, the service is exposed again with backoff, which may assign
// a different URL (PublicURL reports the current one).
func (n *Net) ServePublic(ctx context.Context, slug string, h http.Handler) (string, error) {
	return n.serve(ctx, rolePublic, slug, h)
}

func (n *Net) serve(ctx context.Context, role, name string, h http.Handler) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if h == nil {
		return "", errors.New("netbird: handler is nil")
	}
	if name == "" {
		return "", errors.New("netbird: name is empty")
	}
	if !n.beginOp() {
		return "", errors.New("netbird: closed")
	}
	defer n.ops.Done()
	k := key(role, name)
	l := n.lockFor(k)
	l.Lock()
	defer l.Unlock()

	n.mu.Lock()
	e := n.entries[k]
	n.mu.Unlock()
	if e != nil && !e.stopping.Load() {
		e.handler.set(h)
		return e.getURL(), nil
	}
	if e != nil {
		if err := n.stop(k); err != nil {
			return "", fmt.Errorf("netbird: %s: finish the earlier stop: %w", name, err)
		}
	}
	c, err := n.ensureClient(ctx)
	if err != nil {
		return "", err
	}
	port, err := n.reservePort(k)
	if err != nil {
		return "", fmt.Errorf("netbird: %s: %w", name, err)
	}
	e = &entry{key: k, role: role, name: name, port: port, c: c, handler: &handlerBox{h: h},
		done: make(chan struct{}), first: make(chan error, 1), state: stateStarting}
	if role == rolePrivate {
		fqdn, ip, err := c.Identity()
		if err != nil {
			return "", fmt.Errorf("netbird: %s: peer address: %w", name, err)
		}
		host := fqdn
		if host == "" {
			host = ip
		}
		if host == "" {
			return "", fmt.Errorf("netbird: %s: the peer has no overlay address yet", name)
		}
		e.url = "http://" + net.JoinHostPort(host, fmt.Sprint(port))
	}
	runCtx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	n.mu.Lock()
	n.entries[k] = e
	n.mu.Unlock()
	go n.run(runCtx, e)

	select {
	case err := <-e.first:
		if err != nil {
			_ = n.stop(k)
			return "", fmt.Errorf("netbird: %s: %w", name, err)
		}
		return e.getURL(), nil
	case <-ctx.Done():
		_ = n.stop(k)
		return "", ctx.Err()
	}
}

func (n *Net) reservePort(k string) (int, error) {
	portsMu.Lock()
	defer portsMu.Unlock()
	p, err := readPorts(n.cfg.Dir)
	if err != nil {
		return 0, err
	}
	port, added, err := p.allocate(k)
	if err != nil {
		return 0, err
	}
	if added {
		if err := writeJSON(filepath.Join(n.cfg.Dir, portsFile), p); err != nil {
			return 0, fmt.Errorf("record port: %w", err)
		}
	}
	return port, nil
}

func (n *Net) releasePort(k string) error {
	portsMu.Lock()
	defer portsMu.Unlock()
	p, err := readPorts(n.cfg.Dir)
	if err != nil {
		return err
	}
	port, ok := p[k]
	if !ok {
		return nil
	}
	delete(p, k)
	if strings.HasPrefix(k, rolePublic+"/") {
		p.quarantine(port, time.Now())
	}
	return writeJSON(filepath.Join(n.cfg.Dir, portsFile), p)
}

// run keeps the route up until Stop cancels ctx. Before the first success a
// failure ends the entry so Serve can report it; afterwards failures retry
// with exponential backoff.
func (n *Net) run(ctx context.Context, e *entry) {
	defer close(e.done)
	delay := n.retryMin
	everReady := false
	for {
		reached, err := n.cycle(ctx, e)
		if ctx.Err() != nil {
			e.signalFirst(ctx.Err())
			return
		}
		if reached {
			everReady = true
			delay = n.retryMin
		}
		if !everReady {
			// The first attempt failed: report it; Serve removes the entry.
			e.signalFirst(err)
			return
		}
		e.set(stateError, fmt.Sprintf("%v; retrying in %s", err, delay))
		n.logf("netbird: %s %s: %v; retrying in %s", e.role, e.name, err, delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(2*delay, n.retryMax)
	}
}

// cycle binds the overlay listener (and, for a public route, the expose
// session) once and serves until one of them fails or ctx ends. reached
// reports whether the route became ready.
func (n *Net) cycle(ctx context.Context, e *entry) (reached bool, err error) {
	ln, err := e.c.ListenTCP(net.JoinHostPort("", fmt.Sprint(e.port)))
	if err != nil {
		return false, fmt.Errorf("listen on the NetBird overlay port %d: %w", e.port, err)
	}
	srv := &http.Server{Handler: guardHandler(e.handler), ReadHeaderTimeout: 30 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	tctx, tcancel := context.WithCancel(context.Background())
	defer tcancel()
	drain := func() {
		dctx, c := context.WithTimeout(tctx, n.cfg.ShutdownTimeout)
		defer c()
		if err := srv.Shutdown(dctx); err != nil {
			srv.Close()
		}
		ln.Close()
	}

	var waitErr chan error
	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	if e.role == rolePublic {
		sess, err := e.c.Expose(ctx, exposeRequest{NamePrefix: namePrefix(e.name), Port: uint16(e.port)})
		if err != nil {
			drain()
			return false, fmt.Errorf("expose through NetBird: %w", err)
		}
		u := absoluteURL(sess.URL())
		waitErr = make(chan error, 1)
		go func() { waitErr <- sess.Wait(sctx) }()
		if u == "" {
			scancel() // Wait terminates the session
			<-waitErr
			drain()
			return false, errors.New("NetBird returned no public URL for the service")
		}
		e.mu.Lock()
		e.url = u
		e.mu.Unlock()
	}
	e.set(stateReady, "")
	n.logf("netbird: %s %s ready at %s", e.role, e.name, e.getURL())
	e.signalFirst(nil)

	var cause error
	select {
	case <-ctx.Done():
		cause = ctx.Err()
	case err := <-serveErr:
		cause = fmt.Errorf("the NetBird overlay closed the listener: %w", err)
	case err := <-waitErr: // nil channel for a private route blocks forever
		waitErr = nil // the session is already over
		if err == nil {
			err = errors.New("the expose session ended")
		}
		cause = fmt.Errorf("NetBird expose keep-alive failed: %w", err)
	}
	// Stop publishing first, then drain what was already accepted.
	scancel()
	if waitErr != nil {
		select {
		case <-waitErr:
		case <-time.After(n.cfg.ShutdownTimeout):
		}
	}
	drain()
	return true, cause
}

// guardHandler strips identity headers a visitor could forge; neither the
// overlay nor the NetBird proxy path carries a Tailscale identity, so a
// Tailscale-User-* header can only be spoofed.
func guardHandler(b *handlerBox) http.Handler {
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
// Tailscale-User-* identity header, ignoring case and treating "_" as "-"
// (CGI-style servers and some proxies fold the two together).
func isIdentityHeader(name string) bool {
	const prefix = "tailscale-user-"
	return len(name) >= len(prefix) && strings.EqualFold(strings.ReplaceAll(name[:len(prefix)], "_", "-"), prefix)
}

func absoluteURL(s string) string {
	if s == "" || strings.Contains(s, "://") {
		return s
	}
	return "https://" + s
}

// StopPrivate stops host's private route. The port record stays.
func (n *Net) StopPrivate(host string) error { return n.stopRole(rolePrivate, host) }

// StopPublic terminates slug's expose session and drains its server. The
// port record stays, so the next ServePublic uses the same overlay port.
func (n *Net) StopPublic(slug string) error { return n.stopRole(rolePublic, slug) }

func (n *Net) stopRole(role, name string) error {
	if !n.beginOp() {
		return nil // Close owns the remaining entries.
	}
	defer n.ops.Done()
	k := key(role, name)
	l := n.lockFor(k)
	l.Lock()
	defer l.Unlock()
	return n.stop(k)
}

// stop runs under the key lock. A failure leaves the entry registered so a
// later stop can retry.
func (n *Net) stop(k string) error {
	n.mu.Lock()
	e := n.entries[k]
	n.mu.Unlock()
	if e == nil {
		return nil
	}
	if err := n.stopEntry(e); err != nil {
		return err
	}
	n.mu.Lock()
	if n.entries[k] == e {
		delete(n.entries, k)
	}
	n.mu.Unlock()
	return nil
}

func (n *Net) stopEntry(e *entry) error {
	e.stopping.Store(true)
	e.cancel()
	select {
	case <-e.done:
		return nil
	case <-time.After(2*n.cfg.ShutdownTimeout + time.Second):
		err := fmt.Errorf("netbird: %s %s did not stop within %s", e.role, e.name, 2*n.cfg.ShutdownTimeout)
		e.set(stateError, err.Error())
		return err
	}
}

// RetirePrivate stops host's private route and releases its port, for a
// deleted flat.
func (n *Net) RetirePrivate(host string) error { return n.retire(rolePrivate, host) }

// RetirePublic stops slug's public route and releases its port.
func (n *Net) RetirePublic(slug string) error { return n.retire(rolePublic, slug) }

func (n *Net) retire(role, name string) error {
	if !n.beginOp() {
		return errors.New("netbird: closed")
	}
	defer n.ops.Done()
	k := key(role, name)
	l := n.lockFor(k)
	l.Lock()
	defer l.Unlock()
	if err := n.stop(k); err != nil {
		return err
	}
	if err := n.releasePort(k); err != nil {
		return fmt.Errorf("netbird: %s: release port: %w", name, err)
	}
	return nil
}

// PrivateURL returns host's private URL, or "" when it is not served.
func (n *Net) PrivateURL(host string) string { return n.url(rolePrivate, host) }

// PublicURL returns slug's current public URL, or "" when it is not served.
func (n *Net) PublicURL(slug string) string { return n.url(rolePublic, slug) }

func (n *Net) url(role, name string) string {
	n.mu.Lock()
	e := n.entries[key(role, name)]
	n.mu.Unlock()
	if e == nil {
		return ""
	}
	return e.getURL()
}

// PrivateStatus reports every private route for the console.
func (n *Net) PrivateStatus() core.NetStatus { return n.status(rolePrivate, "netbird") }

// PublicStatus reports every public route for the console.
func (n *Net) PublicStatus() core.NetStatus { return n.status(rolePublic, "netbird-expose") }

func (n *Net) status(role, kind string) core.NetStatus {
	n.mu.Lock()
	var entries []*entry
	for _, e := range n.entries {
		if e.role == role {
			entries = append(entries, e)
		}
	}
	closed := n.closed
	n.mu.Unlock()
	st := core.NetStatus{Kind: kind, Enabled: !closed, Hosts: []core.HostInfo{}}
	if !closed && n.Account() == "" {
		st.Detail = "not connected"
	}
	for _, e := range entries {
		e.mu.Lock()
		st.Hosts = append(st.Hosts, core.HostInfo{Host: e.name, URL: e.url, State: e.state, Detail: e.detail})
		e.mu.Unlock()
	}
	slices.SortFunc(st.Hosts, func(a, b core.HostInfo) int { return strings.Compare(a.Host, b.Host) })
	return st
}

// Close stops every route, then the peer, bounded by ShutdownTimeout per
// step. It is idempotent; later Serve calls fail.
func (n *Net) Close() error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	n.closed = true
	n.mu.Unlock()
	n.ops.Wait()
	n.mu.Lock()
	entries := make([]*entry, 0, len(n.entries))
	for _, e := range n.entries {
		entries = append(entries, e)
	}
	n.entries = map[string]*entry{}
	n.mu.Unlock()
	var errs []error
	for _, e := range entries {
		e.stopping.Store(true)
		e.cancel()
	}
	for _, e := range entries {
		if err := n.stopEntry(e); err != nil {
			errs = append(errs, err)
		}
	}
	n.cmu.Lock()
	c := n.c
	n.c = nil
	n.cmu.Unlock()
	if c != nil {
		ctx, cancel := context.WithTimeout(context.Background(), n.cfg.ShutdownTimeout)
		defer cancel()
		if err := c.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("netbird: stop the peer: %w", err))
		}
	}
	return errors.Join(errs...)
}

// maxNamePrefix is NetBird's limit for an expose name prefix.
const maxNamePrefix = 32

// namePrefix derives the expose name prefix from a slug. Flats slugs are up to
// 54 characters of lowercase letters, digits and single hyphens; NetBird takes
// at most 32 and adds its own suffix, so a long slug is cut, without leaving a
// trailing hyphen.
func namePrefix(slug string) string {
	if len(slug) > maxNamePrefix {
		slug = strings.TrimRight(slug[:maxNamePrefix], "-")
	}
	return slug
}
