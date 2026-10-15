// Package netbird serves flats and console domains through embedded NetBird
// peers (github.com/netbirdio/netbird/client/embed, userspace networking, no
// root or TUN device), one peer per name, as tsnet runs one node per name.
//
// Each peer is named after the flat's slug or the console domain and plays up
// to two roles:
//
//   - Private (or console): plain HTTP on port 80 of the peer's overlay
//     address, reachable by the peers NetBird's access policies allow, at
//     http://<peer FQDN>/.
//   - Public: the flat is served on overlay port 8080 and published through
//     NetBird's reverse proxy ("expose"). The proxy URL is public: Flats sets
//     no Pin, Password or UserGroups, because Public means anyone on the
//     internet may open it.
//
// A peer registers with the setup key the host stores (Config.SetupKey) and
// keeps its WireGuard identity under Config.Dir/peers/<name>, so a restart
// logs in with the same identity. When NetBird has removed the peer (an
// ephemeral peer offline for ten minutes, or one deleted in the dashboard),
// the embedded client registers it again with the stored key.
package netbird

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosuda/flats/internal/core"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Config configures a Net.
type Config struct {
	// Dir holds one directory per peer under peers/. Created 0700.
	Dir string
	// ManagementURL is the NetBird management server ("" is NetBird Cloud).
	ManagementURL string
	// SetupKey returns the setup key stored now, or "". It is called at every
	// peer start, so a key registered while the host runs is used by the
	// next peer. Nil means no key.
	SetupKey func() string
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
	roleConsole = "console"

	// privatePort serves the private and console roles; publicPort is the
	// overlay port NetBird's reverse proxy forwards a public flat to.
	privatePort = 80
	publicPort  = 8080

	peersDir   = "peers"
	configFile = "netbird.json" // embed's persisted config (holds the peer private key)
	dnsFile    = "dns.json"     // embed's state file
)

var (
	// ErrNoSetupKey is returned when a peer must register and no setup key
	// is stored.
	ErrNoSetupKey = errors.New("netbird: no setup key: register a reusable NetBird setup key in Settings > Keys")
	// ErrKeyNoExtraLabels is returned when NetBird refuses to register a
	// peer because the setup key does not allow extra DNS labels.
	ErrKeyNoExtraLabels = errors.New("netbird: the setup key does not allow extra DNS labels: create a setup key with \"Allow extra DNS labels\" on (reusable, ephemeral, with an auto group) and register it in Settings > Keys")
	// ErrPeerRejected is returned when a peer restarts from its stored
	// identity without a setup key and NetBird management no longer accepts
	// that identity.
	ErrPeerRejected = errors.New("netbird: NetBird no longer recognises this peer (it may have been removed in the NetBird dashboard); register a setup key in Settings > Keys so Flats can register it again")
)

// startTimeout bounds a peer start.
var startTimeout = 60 * time.Second

// startSem serializes peer starts across every Net: embed.New sets
// NB_DNS_STATE_FILE in the process environment and the engine reads it while
// it connects, so two peers starting at once could swap state files. It is
// released only when the underlying start has ended, which may be after a
// timed-out Start returned.
var startSem = make(chan struct{}, 1)

// Net serves flats and console domains over embedded NetBird peers.
type Net struct {
	cfg  Config
	logf func(string, ...any)

	retryMin, retryMax time.Duration

	mu      sync.Mutex
	closed  bool
	peers   map[string]client // started peers by name
	entries map[string]*entry // role/name
	locks   map[string]*sync.Mutex
	ops     sync.WaitGroup
	// stopping holds, by name, peers whose Stop failed: their teardown may
	// still run, so the name is neither started again nor its identity
	// deleted until a later Stop succeeds.
	stopping map[string]client
	// starting holds, by name, starts that went on after their caller gave
	// up; the channel closes once that start (and, if it connected late, its
	// teardown) has ended. Until then the name is not started again and its
	// identity is not deleted.
	starting map[string]chan struct{}
	// keyRefused records, by name, the setup key a late start found unable
	// to register (ErrKeyNoExtraLabels), so the next start with the same key
	// fails at once instead of waiting for NetBird's retries again.
	keyRefused map[string]string
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

// New prepares a Net. It never contacts NetBird: a peer starts on the first
// Serve of its name.
func New(cfg Config) (*Net, error) {
	if cfg.Dir == "" {
		return nil, errors.New("netbird: state directory is required")
	}
	if err := validateManagementURL(cfg.ManagementURL); err != nil {
		return nil, fmt.Errorf("netbird: %w", err)
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 30 * time.Second
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Net{cfg: cfg, logf: logf, retryMin: 2 * time.Second, retryMax: time.Minute,
		peers: map[string]client{}, entries: map[string]*entry{}, locks: map[string]*sync.Mutex{}, stopping: map[string]client{}, starting: map[string]chan struct{}{}, keyRefused: map[string]string{}}, nil
}

type logWriter struct{ logf func(string, ...any) }

func (w logWriter) Write(p []byte) (int, error) {
	if s := strings.TrimSpace(string(p)); s != "" {
		w.logf("netbird: %s", s)
	}
	return len(p), nil
}

// lockFor serializes every role of one name, so a peer is never started by
// one role while another stops it.
func (n *Net) lockFor(name string) *sync.Mutex {
	n.mu.Lock()
	defer n.mu.Unlock()
	l := n.locks[name]
	if l == nil {
		l = &sync.Mutex{}
		n.locks[name] = l
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

func (n *Net) setupKey() string {
	if n.cfg.SetupKey == nil {
		return ""
	}
	return strings.TrimSpace(n.cfg.SetupKey())
}

// peerDir is the directory of name's peer. Callers validate name as a DNS
// label; cleaning it as an absolute path also keeps any other input inside
// peers/ (the form static analysis recognizes as free of ".." traversal).
func peerDir(dir, name string) string {
	return filepath.Join(dir, peersDir, filepath.Clean("/"+name))
}

// validName reports whether name can be a peer directory and NetBird host
// name: a lowercase DNS label.
func validName(name string) bool {
	if len(name) == 0 || len(name) > 63 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// ensurePeer starts name's peer once. It runs under name's lock. A failed
// start is retried by the next Serve.
func (n *Net) ensurePeer(ctx context.Context, name string) (client, error) {
	n.mu.Lock()
	c := n.peers[name]
	n.mu.Unlock()
	if c != nil {
		return c, nil
	}
	if err := n.startPending(name); err != nil {
		return nil, err
	}
	if err := n.finishStop(name); err != nil {
		return nil, err
	}
	dir := peerDir(n.cfg.Dir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("netbird: peer directory: %w", err)
	}
	cfgPath := filepath.Join(dir, configFile)
	key := n.setupKey()
	if key == "" {
		if _, err := storedPrivateKey(cfgPath); err != nil {
			return nil, ErrNoSetupKey
		}
	}
	n.mu.Lock()
	refused := key != "" && n.keyRefused[name] == key
	n.mu.Unlock()
	if refused {
		return nil, ErrKeyNoExtraLabels
	}
	var out io.Writer = io.Discard
	if n.cfg.Logf != nil {
		out = logWriter{n.logf}
	}
	scrub := func(err error) error {
		if key == "" || err == nil {
			return err
		}
		return errors.New(strings.ReplaceAll(err.Error(), key, "***"))
	}
	sctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	select {
	case startSem <- struct{}{}:
	case <-sctx.Done():
		return nil, fmt.Errorf("netbird: %s: another NetBird peer is still starting: %w", name, sctx.Err())
	}
	c, err := newClient(clientOptions{
		DeviceName: name, ManagementURL: n.cfg.ManagementURL, SetupKey: key, DNSLabels: []string{name},
		ConfigPath: cfgPath, StatePath: filepath.Join(dir, dnsFile), LogOutput: out,
	})
	if err != nil {
		<-startSem
		return nil, fmt.Errorf("netbird: %s: %w", name, scrub(err))
	}
	final := make(chan error, 1)
	err = c.Start(sctx, func(err error) {
		<-startSem
		final <- err
	})
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		// The underlying start goes on; report how it ends, and keep the
		// name busy until it has.
		pending := make(chan struct{})
		n.mu.Lock()
		n.starting[name] = pending
		n.mu.Unlock()
		go func() {
			ferr := <-final
			n.mu.Lock()
			if n.starting[name] == pending {
				delete(n.starting, name)
			}
			n.mu.Unlock()
			close(pending)
			if ferr == nil {
				n.logf("netbird: %s: the abandoned start connected late; it was stopped, and the next route retries", name)
				return
			}
			if isKeyNoExtraLabels(ferr) {
				n.mu.Lock()
				n.keyRefused[name] = key
				n.mu.Unlock()
				ferr = ErrKeyNoExtraLabels
			}
			n.logf("netbird: %s: the abandoned start failed: %v", name, scrub(ferr))
		}()
	}
	if err != nil {
		switch {
		case key == "" && isPeerRejected(err):
			n.logf("netbird: %s: NetBird management refused the stored peer identity: %v", name, err)
			return nil, ErrPeerRejected
		case isKeyNoExtraLabels(err):
			n.mu.Lock()
			n.keyRefused[name] = key
			n.mu.Unlock()
			return nil, ErrKeyNoExtraLabels
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("netbird: %s: NetBird did not connect within %s; it keeps trying in the background, and the log shows the outcome", name, startTimeout)
		}
		return nil, fmt.Errorf("netbird: %s: connect to NetBird: %w", name, scrub(err))
	}
	_ = os.Chmod(cfgPath, 0o600)
	n.mu.Lock()
	n.peers[name] = c
	n.mu.Unlock()
	return c, nil
}

// registrationFallbackMsg is the error NetBird v0.80.0's auth.Login reports
// when management answers PermissionDenied to the login with the stored key
// pair and it falls back to registering with the (empty) setup key:
// registerPeer parses "" as a UUID and fails before contacting management.
// The original PermissionDenied reason is lost.
const registrationFallbackMsg = "invalid setup-key or no sso information provided"

// isPeerRejected reports whether err from a start without a setup key means
// management refused the stored peer identity.
func isPeerRejected(err error) bool {
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.InvalidArgument && strings.Contains(st.Message(), registrationFallbackMsg)
}

// startPending refuses while a start of name abandoned by its caller still
// runs.
func (n *Net) startPending(name string) error {
	n.mu.Lock()
	_, busy := n.starting[name]
	n.mu.Unlock()
	if busy {
		return fmt.Errorf("netbird: %s: an earlier start of this peer is still running; retry when it has ended (the log shows it)", name)
	}
	return nil
}

// finishStop stops name's peer left as stopping, if any. It runs under name's
// lock.
func (n *Net) finishStop(name string) error {
	n.mu.Lock()
	c := n.stopping[name]
	n.mu.Unlock()
	if c == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), n.cfg.ShutdownTimeout)
	err := c.Stop(ctx)
	cancel()
	var torn teardownError
	if errors.As(err, &torn) {
		// The peer no longer runs; only its teardown reported a problem.
		n.logf("netbird: %s: %v", name, err)
		err = nil
	}
	if err != nil {
		n.logf("netbird: %s: stop the peer: %v", name, err)
		return fmt.Errorf("netbird: %s: the peer did not stop: %w; retried by the next change of this name", name, err)
	}
	n.mu.Lock()
	if n.stopping[name] == c {
		delete(n.stopping, name)
	}
	n.mu.Unlock()
	return nil
}

// isKeyNoExtraLabels reports whether err is NetBird's refusal to register a
// peer that asks for extra DNS labels with a key that does not allow them.
func isKeyNoExtraLabels(err error) bool {
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.FailedPrecondition && strings.Contains(st.Message(), "doesn't allow extra DNS labels")
}

// releasePeer stops name's peer once no role uses it, and with retire deletes
// its identity. It runs under name's lock. A peer whose Stop fails is kept as
// stopping, and its identity stays, until a later call stops it.
func (n *Net) releasePeer(name string, retire bool) error {
	n.mu.Lock()
	for _, e := range n.entries {
		if e.name == name {
			n.mu.Unlock()
			return nil
		}
	}
	if c := n.peers[name]; c != nil {
		delete(n.peers, name)
		n.stopping[name] = c
	}
	n.mu.Unlock()
	if err := n.finishStop(name); err != nil {
		return err
	}
	if retire {
		if err := n.startPending(name); err != nil {
			return err
		}
		if err := os.RemoveAll(peerDir(n.cfg.Dir, name)); err != nil {
			return fmt.Errorf("netbird: %s: delete the peer identity: %w", name, err)
		}
	}
	return nil
}

func key(role, name string) string { return role + "/" + name }

// ServePrivate serves h as plain HTTP on port 80 of host's own peer and
// returns its URL (peer FQDN, or the overlay IP). Serving a host that is
// already served replaces its handler.
func (n *Net) ServePrivate(ctx context.Context, host string, h http.Handler) (string, error) {
	return n.serve(ctx, rolePrivate, host, h)
}

// ServePublic serves h on slug's peer and publishes it through NetBird's
// reverse proxy. It returns the public URL once the proxy accepted the
// service. Public routes carry no Pin or Password. If the keep-alive or the
// listener fails, the service is exposed again with backoff, which may assign
// a different URL (PublicURL reports the current one).
func (n *Net) ServePublic(ctx context.Context, slug string, h http.Handler) (string, error) {
	return n.serve(ctx, rolePublic, slug, h)
}

// ServeConsole serves the console handler h on port 80 of name's own peer
// and returns its URL.
func (n *Net) ServeConsole(ctx context.Context, name string, h http.Handler) (string, error) {
	return n.serve(ctx, roleConsole, name, h)
}

func (n *Net) serve(ctx context.Context, role, name string, h http.Handler) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if h == nil {
		return "", errors.New("netbird: handler is nil")
	}
	if !validName(name) {
		return "", fmt.Errorf("netbird: %q is not a lowercase DNS label", name)
	}
	if !n.beginOp() {
		return "", errors.New("netbird: closed")
	}
	defer n.ops.Done()
	k := key(role, name)
	l := n.lockFor(name)
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
	c, err := n.ensurePeer(ctx, name)
	if err != nil {
		return "", err
	}
	port := privatePort
	if role == rolePublic {
		port = publicPort
	}
	e = &entry{key: k, role: role, name: name, port: port, c: c, handler: &handlerBox{h: h},
		done: make(chan struct{}), first: make(chan error, 1), state: stateStarting}
	if role != rolePublic {
		u, err := peerURL(c, name)
		if err != nil {
			_ = n.releasePeer(name, false)
			return "", fmt.Errorf("netbird: %s: %w", name, err)
		}
		e.url = u
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
			_ = n.releasePeer(name, false)
			return "", fmt.Errorf("netbird: %s: %w", name, err)
		}
		return e.getURL(), nil
	case <-ctx.Done():
		_ = n.stop(k)
		_ = n.releasePeer(name, false)
		return "", ctx.Err()
	}
}

// peerURL is the HTTP URL of name's peer on port 80: its extra DNS label in
// the account's NetBird domain, else its overlay IP.
func peerURL(c client, name string) (string, error) {
	fqdn, ip, err := c.Identity()
	if err != nil {
		return "", fmt.Errorf("peer address: %w", err)
	}
	if _, domain, ok := strings.Cut(fqdn, "."); ok && domain != "" {
		return "http://" + name + "." + domain + "/", nil
	}
	if ip == "" {
		return "", errors.New("the peer has no overlay address yet")
	}
	return "http://" + ip + "/", nil
}

// Hostnames returns the names name's peer answers to: its extra label and
// primary FQDN in NetBird DNS, and its overlay IP. It is empty while the peer
// is not running.
func (n *Net) Hostnames(name string) []string {
	n.mu.Lock()
	c := n.peers[name]
	n.mu.Unlock()
	if c == nil {
		return nil
	}
	fqdn, ip, err := c.Identity()
	if err != nil {
		return nil
	}
	var out []string
	if _, domain, ok := strings.Cut(fqdn, "."); ok && domain != "" && name+"."+domain != fqdn {
		out = append(out, name+"."+domain)
	}
	if fqdn != "" {
		out = append(out, fqdn)
	}
	if ip != "" {
		out = append(out, ip) // the URL when the peer has no FQDN
	}
	return out
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

// StopPrivate stops host's private route. Its peer stops once it serves
// nothing else; the identity stays.
func (n *Net) StopPrivate(host string) error { return n.stopRole(rolePrivate, host, false) }

// StopPublic terminates slug's expose session and drains its server.
func (n *Net) StopPublic(slug string) error { return n.stopRole(rolePublic, slug, false) }

// StopConsole stops console domain name's route.
func (n *Net) StopConsole(name string) error { return n.stopRole(roleConsole, name, false) }

// RetirePrivate stops host's private route and, once its peer serves nothing
// else, stops the peer and deletes its identity, for a deleted flat.
func (n *Net) RetirePrivate(host string) error { return n.stopRole(rolePrivate, host, true) }

// RetirePublic stops slug's public route and retires its peer as
// RetirePrivate does.
func (n *Net) RetirePublic(slug string) error { return n.stopRole(rolePublic, slug, true) }

// RetireConsole stops console domain name and deletes its peer identity.
func (n *Net) RetireConsole(name string) error { return n.stopRole(roleConsole, name, true) }

func (n *Net) stopRole(role, name string, retire bool) error {
	if !n.beginOp() {
		if retire {
			return errors.New("netbird: closed")
		}
		return nil // Close owns the remaining entries.
	}
	defer n.ops.Done()
	if !validName(name) {
		return nil // never served
	}
	l := n.lockFor(name)
	l.Lock()
	defer l.Unlock()
	if err := n.stop(key(role, name)); err != nil {
		return err
	}
	return n.releasePeer(name, retire)
}

// stop runs under the name lock. A failure leaves the entry registered so a
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

// PeerCount reports how many peers run.
func (n *Net) PeerCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.peers)
}

// PrivateURL returns host's private URL, or "" when it is not served.
func (n *Net) PrivateURL(host string) string { return n.url(rolePrivate, host) }

// PublicURL returns slug's current public URL, or "" when it is not served.
func (n *Net) PublicURL(slug string) string { return n.url(rolePublic, slug) }

// ConsoleURL returns console domain name's URL, or "" when it is not served.
func (n *Net) ConsoleURL(name string) string { return n.url(roleConsole, name) }

func (n *Net) url(role, name string) string {
	n.mu.Lock()
	e := n.entries[key(role, name)]
	n.mu.Unlock()
	if e == nil {
		return ""
	}
	return e.getURL()
}

// IdentityForIP returns the FQDN of the NetBird peer at ip as name's peer
// sees it, for recording who made a console decision.
func (n *Net) IdentityForIP(name, ip string) string {
	n.mu.Lock()
	c := n.peers[name]
	n.mu.Unlock()
	if c == nil {
		return ""
	}
	return c.PeerFQDN(ip)
}

// PrivateStatus reports every private route for the console.
func (n *Net) PrivateStatus() core.NetStatus { return n.status(rolePrivate, "netbird") }

// PublicStatus reports every public route for the console.
func (n *Net) PublicStatus() core.NetStatus { return n.status(rolePublic, "netbird-expose") }

// ConsoleStatus reports every console domain's route.
func (n *Net) ConsoleStatus() core.NetStatus { return n.status(roleConsole, "netbird") }

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
	if !closed && n.setupKey() == "" {
		st.Detail = "no setup key"
	}
	for _, e := range entries {
		e.mu.Lock()
		hi := core.HostInfo{Host: e.name, URL: e.url, State: e.state, Detail: e.detail}
		e.mu.Unlock()
		st.Hosts = append(st.Hosts, hi)
	}
	slices.SortFunc(st.Hosts, func(a, b core.HostInfo) int { return strings.Compare(a.Host, b.Host) })
	return st
}

// Close stops every route, then every peer, bounded by ShutdownTimeout per
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
	peers := n.peers
	n.peers = map[string]client{}
	for name, c := range n.stopping {
		peers[name] = c
	}
	n.stopping = map[string]client{}
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
	for name, c := range peers {
		ctx, cancel := context.WithTimeout(context.Background(), n.cfg.ShutdownTimeout)
		err := c.Stop(ctx)
		cancel()
		var torn teardownError
		if errors.As(err, &torn) {
			n.logf("netbird: %s: %v", name, err)
		} else if err != nil {
			errs = append(errs, fmt.Errorf("netbird: %s: stop the peer: %w", name, err))
		}
	}
	// Starts their callers abandoned still own their peer's state: wait for
	// them, so the data directory is not released while they run.
	n.mu.Lock()
	pending := maps.Clone(n.starting)
	n.mu.Unlock()
	deadline := time.After(n.cfg.ShutdownTimeout)
	for name, done := range pending {
		select {
		case <-done:
		case <-deadline:
			errs = append(errs, fmt.Errorf("netbird: %s: an earlier start of the peer is still running: %w", name, context.DeadlineExceeded))
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
