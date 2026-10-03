// Package tsnet serves flats on the operator's tailnet. Every host (flat,
// preview or the console) gets its own tsnet node, so each one has its own
// MagicDNS name, TLS certificate and browser origin.
package tsnet

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"tailscale.com/client/local"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/envknob"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/ipn/store/mem"
	ts "tailscale.com/tsnet"

	"github.com/gosuda/flats/internal/core"
)

// Config configures a Net.
type Config struct {
	// Dir is the base state directory; each node keeps its state in Dir/<host>.
	Dir string
	// AuthKey is a reusable, untagged auth key for new nodes. It may be empty,
	// in which case each new node waits for an interactive login.
	AuthKey string
	// ControlURL overrides the coordination server (tests).
	ControlURL string
	// Logf receives operational messages. Nil discards them.
	Logf func(string, ...any)
	// GetCertificate overrides the Tailscale certificate source. It is used by
	// disposable testcontrol environments, which do not run ACME.
	GetCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)
}

const (
	// HTTPSUnavailable is the host detail when the tailnet cannot issue
	// certificates and the node serves plain HTTP instead.
	HTTPSUnavailable = "HTTPS certificates are not enabled in this tailnet; serving plain HTTP inside the tailnet"

	keyExpiringWindow = 14 * 24 * time.Hour
	defaultPoll       = 5 * time.Minute
	logoutTimeout     = 10 * time.Second
	// certWarmTimeout bounds one certificate request. Tailscale gets the
	// certificate from Let's Encrypt with a DNS-01 challenge; on a real
	// tailnet that takes about 80 seconds and sometimes several minutes.
	certWarmTimeout = 5 * time.Minute
	// CertPending is the host detail while HTTPS waits for its certificate.
	CertPending = "waiting for its HTTPS certificate from Let's Encrypt (a new host usually needs 1-2 minutes)"
)

// Retry backoff for certificate fetches and the longest Stop and Close wait
// for a node to shut down; variables so tests can shorten them.
var (
	certRetryMin = 30 * time.Second
	certRetryMax = 10 * time.Minute
	stopWait     = 30 * time.Second
)

// Host states reported in core.HostInfo.State.
const (
	StateStarting    = "starting"
	StateReady       = "ready"
	StateNeedsLogin  = "needs-login"
	StateKeyExpiring = "key-expiring"
	StateError       = "error"
	// StateIdle means the node is on the tailnet and private HTTP was not requested.
	StateIdle = "idle"
)

var hostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Net implements core.PrivateNet with one tsnet.Server per host.
type Net struct {
	cfg  Config
	logf func(string, ...any)

	// Test hooks: pollEvery overrides the status poll interval, getCert
	// replaces the LocalAPI (ACME) certificate source and warmCert replaces
	// the certificate fetch done when a node starts serving HTTPS.
	// listenFunnel, when set, replaces Server.ListenFunnel. afterFunnelListen
	// runs after a listener exists; a non-nil error closes that listener
	// (which drops the AllowFunnel entry ListenFunnel added) and fails the call.
	pollEvery           time.Duration
	getCert             func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	warmCert            func(ctx context.Context, domain string) error
	listenFunnel        func(srv *ts.Server, opts []ts.FunnelOption) (net.Listener, error)
	afterFunnelListen   func() error
	beforePrivateListen func()
	logoutNode          func(context.Context, *node) error

	acmeMu sync.Mutex // serializes creating the shared ACME account key

	mu            sync.Mutex
	nodes         map[string]*node
	stopping      map[string]chan struct{} // host -> closed when any teardown attempt finishes
	retiring      map[string]*retirement   // logout retirements, including terminal failures
	funnelReports map[string]FunnelReport  // terminal setup reports after a minted node is retired
	suffix        string                   // MagicDNS suffix, once any node learns it
	plain         bool                     // some node found HTTPS unavailable
	closed        bool
	changed       chan struct{} // closed and replaced on every state change
}

type retirement struct {
	host      string
	ephemeral bool
	store     ipn.StateStore
	persisted bool // discovered from tailscaled.state without a live node
	attempt   *retirementAttempt
}

type retirementAttempt struct {
	done chan struct{}
	err  error // written before done closes
}

type node struct {
	privateMu      sync.Mutex // serializes private listener setup and confirmed stop
	funnelMu       sync.Mutex // serializes Funnel listener setup and stop after node boot
	host           string
	ephemeral      bool
	dir            string
	handler        atomic.Pointer[http.Handler]
	ctx            context.Context
	cancel         context.CancelFunc
	started        chan struct{} // closed once srv.Start has returned (Close is then safe)
	done           chan struct{} // closed when the lifecycle goroutine returns
	prev           chan struct{} // teardown of the previous node with this host, or nil
	srv            *ts.Server
	backendStarted bool  // published before started closes
	stopErr        error // set by the retiring goroutine before it closes its done channel
	store          ipn.StateStore

	// Guarded by Net.mu.
	backend       string // ipn.State string
	authURL       string
	keyExpiry     *time.Time
	dnsName       string // FQDN without trailing dot
	private       bool   // tailnet HTTP was requested; funnel-only nodes stay false
	listenStarted bool
	serving       bool
	plain         bool
	certOK        bool   // the HTTPS certificate has been obtained at least once
	certErr       string // last certificate fetch error while !certOK
	err           error
	loginKick     bool   // StartLoginInteractive already requested in this needs-login episode
	wasUp         bool   // reached Running at least once (later NeedsLogin means re-auth)
	identityState []byte // last persisted state observed while Running, for failure-atomic logout
	https         []*http.Server

	booted       chan struct{} // closed once Up has finished or the node has given up
	bootSignaled bool
	bootOK       bool

	funnelH       atomic.Pointer[http.Handler]
	funnelLn      net.Listener
	funnelSrv     *http.Server
	funnelState   string
	funnelDetail  string
	funnelURL     string
	funnelOpening bool
	funnelCleanup bool
}

// New creates a Net. Nodes start on the first Serve of their host.
func New(cfg Config) (*Net, error) {
	if cfg.Dir == "" {
		return nil, errors.New("tsnet: Config.Dir is required")
	}
	disableMacTokenLookup()
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	// Never upload node logs to log.tailscale.com.
	envknob.SetNoLogsNoSupport()
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Net{
		cfg:           cfg,
		logf:          logf,
		getCert:       cfg.GetCertificate,
		nodes:         map[string]*node{},
		stopping:      map[string]chan struct{}{},
		retiring:      map[string]*retirement{},
		funnelReports: map[string]FunnelReport{},
		changed:       make(chan struct{}),
	}, nil
}

// notifyLocked wakes WaitReady callers. n.mu must be held.
func (n *Net) notifyLocked() {
	close(n.changed)
	n.changed = make(chan struct{})
}

// Serve implements core.PrivateNet. It returns immediately; the node comes up
// in the background and Status reports its progress. Serving a host that is
// already served swaps its handler without restarting the node.
func (n *Net) Serve(_ context.Context, host string, h http.Handler, ephemeral bool) (string, error) {
	if !hostRE.MatchString(host) {
		return "", fmt.Errorf("tsnet: invalid host %q (want a lowercase DNS label)", host)
	}
	if h == nil {
		return "", errors.New("tsnet: nil handler")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return "", errors.New("tsnet: network is closed")
	}
	if pending, ok := n.retiring[host]; ok {
		select {
		case <-pending.attempt.done:
			if pending.attempt.err != nil {
				return "", fmt.Errorf("tsnet: %s teardown failed; retry the removal before serving it again: %w", host, pending.attempt.err)
			}
			delete(n.retiring, host)
		default:
			return "", fmt.Errorf("tsnet: %s Private teardown is still unconfirmed", host)
		}
	}
	if nd, ok := n.nodes[host]; ok {
		nd.handler.Store(&h)
		if nd.err == nil {
			if !nd.private {
				nd.private = true
				go n.ensurePrivate(nd)
			}
			return n.urlLocked(host), nil
		}
		// The node failed (start, Up or listen) and serving again is how a
		// caller retries. Shut it down without logging out, so a persistent
		// node keeps its identity, and start a fresh one after that.
		n.retireLocked(nd, false)
	}
	nd := n.newNode(host, ephemeral, true)
	nd.handler.Store(&h)
	n.nodes[host] = nd
	n.notifyLocked()
	go n.run(nd)
	return n.urlLocked(host), nil
}

// newNode builds a node that is not yet in the served set. n.mu must be held.
func (n *Net) newNode(host string, ephemeral, private bool) *node {
	ctx, cancel := context.WithCancel(context.Background())
	nd := &node{
		host:      host,
		ephemeral: ephemeral,
		private:   private,
		dir:       filepath.Join(n.cfg.Dir, host),
		ctx:       ctx,
		cancel:    cancel,
		started:   make(chan struct{}),
		done:      make(chan struct{}),
		booted:    make(chan struct{}),
		prev:      n.stopping[host],
		backend:   ipn.NoState.String(),
	}
	if ephemeral {
		nd.store = new(mem.Store)
	}
	return nd
}

// signalBoot wakes ServeFunnel. The first call wins; ok is recorded only then.
// Nodes built without a boot channel (predecessor-only teardown tests) are ignored.
func (n *Net) signalBoot(nd *node, ok bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if nd.bootSignaled || nd.booted == nil {
		return
	}
	nd.bootSignaled = true
	nd.bootOK = ok
	close(nd.booted)
}

// run owns one node's lifecycle until its context is cancelled.
func (n *Net) run(nd *node) {
	defer close(nd.done)
	defer n.signalBoot(nd, false)
	if nd.prev != nil {
		// A previous node with this host is still tearing down (and may be
		// deleting the state directory); start fresh after it.
		select {
		case <-nd.prev:
		case <-nd.ctx.Done():
			close(nd.started)
			return
		}
	}
	srv := &ts.Server{
		Dir:        nd.dir,
		Hostname:   nd.host,
		AuthKey:    n.cfg.AuthKey,
		ControlURL: n.cfg.ControlURL,
		Ephemeral:  nd.ephemeral,
		UserLogf:   n.userLogf(nd),
		Store:      nd.store,
	}
	nd.srv = srv
	err := os.MkdirAll(nd.dir, 0o700)
	if err == nil {
		if kerr := n.shareACMEKey(nd.dir); kerr != nil {
			// Not fatal: tailscaled then registers its own account.
			n.logf("tsnet %s: shared ACME account: %v", nd.host, kerr)
		}
		err = srv.Start()
	}
	nd.backendStarted = err == nil
	close(nd.started)
	if err != nil {
		n.setErr(nd, fmt.Errorf("start node: %w", err))
		return
	}
	lc, err := srv.LocalClient()
	if err != nil {
		n.setErr(nd, err)
		return
	}
	go n.watchBus(nd, lc)

	st, err := srv.Up(nd.ctx)
	if err != nil {
		if nd.ctx.Err() == nil {
			n.setErr(nd, err)
		}
		return
	}
	n.applyStatus(nd, st)
	n.signalBoot(nd, true)
	n.mu.Lock()
	wantPrivate := nd.private
	n.mu.Unlock()
	if wantPrivate {
		if err := n.listen(nd, srv, lc, st); err != nil {
			if nd.ctx.Err() == nil {
				n.setErr(nd, err)
			}
			return
		}
	}
	n.poll(nd, lc)
}

// ensurePrivate starts tailnet HTTP on a node that was brought up for Funnel
// only. It does nothing when private HTTP is already being served.
func (n *Net) ensurePrivate(nd *node) {
	select {
	case <-nd.booted:
	case <-nd.ctx.Done():
		return
	}
	n.mu.Lock()
	ready := nd.private && nd.bootOK && nd.err == nil && nd.srv != nil && !nd.serving && !nd.listenStarted
	n.mu.Unlock()
	if !ready {
		return
	}
	lc, err := nd.srv.LocalClient()
	if err != nil {
		n.setErr(nd, err)
		return
	}
	ctx, cancel := context.WithTimeout(nd.ctx, 10*time.Second)
	st, err := lc.Status(ctx)
	cancel()
	if err != nil {
		if nd.ctx.Err() == nil {
			n.setErr(nd, err)
		}
		return
	}
	if err := n.listen(nd, nd.srv, lc, st); err != nil && nd.ctx.Err() == nil {
		n.setErr(nd, err)
	}
}

// listen serves the handler with HTTPS on :443 (and a redirect on :80) when
// the tailnet issues certificates, else plain HTTP on :80.
func (n *Net) listen(nd *node, srv *ts.Server, lc *local.Client, st *ipnstate.Status) error {
	if n.beforePrivateListen != nil {
		n.beforePrivateListen()
	}
	nd.privateMu.Lock()
	defer nd.privateMu.Unlock()
	n.mu.Lock()
	if !nd.private || nd.ctx.Err() != nil || nd.listenStarted || nd.serving {
		n.mu.Unlock()
		return nil
	}
	nd.listenStarted = true
	n.mu.Unlock()
	app := n.identity(nd, lc.WhoIs)
	useTLS := st.CurrentTailnet != nil && st.CurrentTailnet.MagicDNSEnabled && len(st.CertDomains) > 0
	var servers []*http.Server
	start := func(ln net.Listener, h http.Handler) {
		hs := &http.Server{
			Handler:           h,
			ReadHeaderTimeout: 10 * time.Second, // also bounds the TLS handshake
			IdleTimeout:       2 * time.Minute,
			ErrorLog:          log.New(logWriter{n.logf, nd.host}, "", 0),
		}
		servers = append(servers, hs)
		go hs.Serve(ln)
	}
	if useTLS {
		base := n.getCert
		if base == nil {
			base = lc.GetCertificate
		}
		getCert := func(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			c, err := base(hi)
			if err == nil {
				n.certObtained(nd, nil)
			}
			return c, err
		}
		ln443, err := srv.Listen("tcp", ":443")
		if err != nil {
			return err
		}
		ln80, err := srv.Listen("tcp", ":80")
		if err != nil {
			ln443.Close()
			return err
		}
		canonical := st.CertDomains[0]
		start(tls.NewListener(ln443, &tls.Config{GetCertificate: getCert, NextProtos: []string{"h2", "http/1.1"}}), app)
		start(ln80, httpsRedirect(canonical))
		// Issuing a certificate can take longer than the handshake timeout,
		// which would fail the first visit; fetch it now instead.
		go n.warm(nd, lc, canonical)
	} else {
		ln80, err := srv.Listen("tcp", ":80")
		if err != nil {
			return err
		}
		start(ln80, app)
	}
	n.mu.Lock()
	nd.https = servers
	nd.serving = true
	nd.plain = !useTLS
	if !useTLS {
		n.plain = true
	}
	n.notifyLocked()
	n.mu.Unlock()
	if !useTLS {
		n.logf("tsnet %s: %s", nd.host, HTTPSUnavailable)
	}
	return nil
}

// acmeKeyName is where tailscaled keeps a node's ACME account key, inside
// <node dir>/certs (see tailscale.com/feature/acme).
const acmeKeyName = "acme-account.key.pem"

// shareACMEKey gives the node in dir the Let's Encrypt account key shared by
// all nodes of this Net, unless it already has one. Without it every node
// registers its own account, and Let's Encrypt allows only 10 new accounts
// per IP address in 3 hours: the 11th new flat or preview then waits hours
// for its certificate. The shared key lives in Config.Dir; the first time,
// an existing node's key (an already registered account) is adopted, else a
// new key is created.
func (n *Net) shareACMEKey(dir string) error {
	certs := filepath.Join(dir, "certs")
	own := filepath.Join(certs, acmeKeyName)
	if _, err := os.Stat(own); err == nil {
		return nil
	}
	key, err := n.sharedACMEKey()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(certs, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(own, key)
}

// sharedACMEKey returns the shared account key PEM, creating it if needed.
func (n *Net) sharedACMEKey() ([]byte, error) {
	n.acmeMu.Lock()
	defer n.acmeMu.Unlock()
	shared := filepath.Join(n.cfg.Dir, acmeKeyName)
	if b, err := os.ReadFile(shared); err == nil {
		return b, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	var key []byte
	entries, _ := os.ReadDir(n.cfg.Dir)
	for _, e := range entries {
		if b, err := os.ReadFile(filepath.Join(n.cfg.Dir, e.Name(), "certs", acmeKeyName)); err == nil && validACMEKey(b) {
			key = b
			break
		}
	}
	if key == nil {
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalECPrivateKey(priv)
		if err != nil {
			return nil, err
		}
		key = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	}
	if err := writeFileAtomic(shared, key); err != nil {
		return nil, err
	}
	return key, nil
}

// validACMEKey reports whether b is a PEM private key tailscaled can use.
func validACMEKey(b []byte) bool {
	blk, _ := pem.Decode(b)
	if blk == nil || !strings.Contains(blk.Type, "PRIVATE") {
		return false
	}
	if _, err := x509.ParseECPrivateKey(blk.Bytes); err == nil {
		return true
	}
	_, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	return err == nil
}

// writeFileAtomic writes a 0600 file via a temporary file and rename.
func writeFileAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	cerr := f.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp, 0o600)); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// httpsRedirect sends plain HTTP requests to the same path on https://host.
// 307 keeps the method and is not cached, so turning HTTPS off later does not
// leave browsers stuck on a dead redirect.
func httpsRedirect(host string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri := r.URL.RequestURI()
		if !strings.HasPrefix(uri, "/") {
			// Opaque or asterisk forms ("http:@evil.example", "*") would
			// otherwise be glued onto the host and change the target origin.
			uri = "/"
		}
		http.Redirect(w, r, "https://"+host+uri, http.StatusTemporaryRedirect)
	})
}

// warm fetches the node's certificate in the background, retrying with
// backoff until it succeeds or the node stops. Until then the host reports
// starting rather than ready, so callers do not hand out a URL whose TLS
// handshake still fails.
func (n *Net) warm(nd *node, lc *local.Client, domain string) {
	fetch := n.warmCert
	if fetch == nil {
		fetch = func(ctx context.Context, d string) error {
			_, _, err := lc.CertPair(ctx, d)
			return err
		}
	}
	wait := certRetryMin
	for {
		ctx, cancel := context.WithTimeout(nd.ctx, certWarmTimeout)
		err := fetch(ctx, domain)
		cancel()
		if nd.ctx.Err() != nil {
			return
		}
		if n.certObtained(nd, err) {
			return
		}
		n.logf("tsnet %s: certificate for %s: %v (retrying in %s)", nd.host, domain, err, wait)
		select {
		case <-nd.ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, certRetryMax)
	}
}

// certObtained records the outcome of a certificate fetch (err == nil means
// the node has its certificate) and reports whether the node has one.
func (n *Net) certObtained(nd *node, err error) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if nd.certOK {
		return true
	}
	if err == nil {
		nd.certOK, nd.certErr = true, ""
	} else {
		nd.certErr = err.Error()
	}
	n.notifyLocked()
	return nd.certOK
}

type logWriter struct {
	logf func(string, ...any)
	host string
}

func (w logWriter) Write(p []byte) (int, error) {
	w.logf("tsnet %s: %s", w.host, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

type whoIsFunc func(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error)

// isIdentityHeader reports whether a request header name could be read as a
// Tailscale-User-* identity header, ignoring case and treating "_" as "-"
// (CGI-style servers and some proxies fold the two together).
func isIdentityHeader(name string) bool {
	const prefix = "tailscale-user-"
	return len(name) >= len(prefix) && strings.EqualFold(strings.ReplaceAll(name[:len(prefix)], "_", "-"), prefix)
}

// identity strips client-sent Tailscale-User-* headers and sets the
// caller's login and display name from WhoIs, unless the peer is tagged.
func (n *Net) identity(nd *node, whoIs whoIsFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k := range r.Header {
			if isIdentityHeader(k) {
				delete(r.Header, k)
			}
		}
		if who, err := whoIs(r.Context(), r.RemoteAddr); err == nil && who.Node != nil && !who.Node.IsTagged() && who.UserProfile != nil {
			r.Header.Set("Tailscale-User-Login", headerValue(who.UserProfile.LoginName))
			r.Header.Set("Tailscale-User-Name", headerValue(who.UserProfile.DisplayName))
		}
		if handler := nd.handler.Load(); handler != nil {
			(*handler).ServeHTTP(w, r)
		} else {
			http.NotFound(w, r)
		}
	})
}

// headerValue matches tailscale serve: ASCII as is, other UTF-8 as RFC 2047
// Q-encoding, invalid UTF-8 dropped.
func headerValue(v string) string {
	if !utf8.ValidString(v) {
		return ""
	}
	return mime.QEncoding.Encode("utf-8", v)
}

// userLogf captures the auth URL tsnet prints while a node needs login.
func (n *Net) userLogf(nd *node) func(string, ...any) {
	return func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		if _, url, ok := strings.Cut(msg, "go to: "); ok {
			url = strings.TrimSpace(url)
			n.mu.Lock()
			if nd.authURL != url {
				nd.authURL = url
				n.notifyLocked()
			}
			n.mu.Unlock()
		}
		n.logf("tsnet %s: %s", nd.host, msg)
	}
}

// watchBus follows backend state and login URLs on the IPN bus.
func (n *Net) watchBus(nd *node, lc *local.Client) {
	w, err := lc.WatchIPNBus(nd.ctx, ipn.NotifyInitialState)
	if err != nil {
		return
	}
	defer w.Close()
	for {
		msg, err := w.Next()
		if err != nil {
			return
		}
		if msg.BrowseToURL != nil && *msg.BrowseToURL != "" {
			n.mu.Lock()
			nd.authURL = *msg.BrowseToURL
			n.notifyLocked()
			n.mu.Unlock()
		}
		if msg.State != nil {
			n.mu.Lock()
			nd.backend = msg.State.String()
			if *msg.State == ipn.Running {
				nd.authURL = ""
				nd.loginKick = false
				nd.wasUp = true
			}
			n.notifyLocked()
			n.mu.Unlock()
			n.refresh(nd, lc)
		}
	}
}

// poll refreshes key expiry and backend state until the node stops.
func (n *Net) poll(nd *node, lc *local.Client) {
	every := n.pollEvery
	if every <= 0 {
		every = defaultPoll
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-nd.ctx.Done():
			return
		case <-t.C:
			n.refresh(nd, lc)
		}
	}
}

// refresh reads StatusWithoutPeers. When a node that was running needs login
// again (expired key) and control has not handed out a URL, it asks for an
// interactive login once so the console can show one. New nodes are left
// alone: control already returns a URL for them, and an extra interactive
// login could race an auth-key login.
func (n *Net) refresh(nd *node, lc *local.Client) {
	ctx, cancel := context.WithTimeout(nd.ctx, 10*time.Second)
	defer cancel()
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return
	}
	n.applyStatus(nd, st)
	n.mu.Lock()
	kick := nd.wasUp && nd.backend == ipn.NeedsLogin.String() && nd.authURL == "" && !nd.loginKick
	if kick {
		nd.loginKick = true
	}
	n.mu.Unlock()
	if kick {
		if err := lc.StartLoginInteractive(ctx); err != nil {
			n.logf("tsnet %s: start login: %v", nd.host, err)
		}
	}
}

func (n *Net) applyStatus(nd *node, st *ipnstate.Status) {
	var identityState []byte
	if st.BackendState == ipn.Running.String() {
		identityState, _ = os.ReadFile(filepath.Join(nd.dir, "tailscaled.state"))
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if st.BackendState != "" {
		nd.backend = st.BackendState
	}
	if st.AuthURL != "" {
		nd.authURL = st.AuthURL
	}
	if st.Self != nil {
		nd.keyExpiry = st.Self.KeyExpiry
		if name := strings.TrimSuffix(st.Self.DNSName, "."); name != "" {
			nd.dnsName = name
		}
	}
	if len(identityState) > 0 {
		nd.identityState = identityState
	}
	if s := strings.TrimSuffix(st.MagicDNSSuffix, "."); s != "" {
		n.suffix = s
	}
	n.notifyLocked()
}

func (n *Net) setErr(nd *node, err error) {
	n.logf("tsnet %s: %v", nd.host, err)
	n.mu.Lock()
	nd.err = err
	n.notifyLocked()
	n.mu.Unlock()
}

// StopPrivate closes only tailnet HTTP when Funnel shares the node. Without
// a Funnel request it retires the node normally, including isolated previews.
func (n *Net) StopPrivate(host string) error {
	return n.stop(host, true)
}

// Stop implements core.PrivateNet: it logs the node out, closes it and deletes
// its state. Failed retirements stay tracked, and each later Stop creates a
// fresh backend from the retained state before retrying logout.
func (n *Net) Stop(host string) error {
	return n.stop(host, false)
}

func (n *Net) stop(host string, privateOnly bool) error {
	for {
		n.mu.Lock()
		if pending := n.retiring[host]; pending != nil {
			attempt := pending.attempt
			scheduled := false
			select {
			case <-attempt.done:
				if attempt.err == nil {
					delete(n.retiring, host)
					n.mu.Unlock()
					continue
				}
				if n.closed {
					err := attempt.err
					n.mu.Unlock()
					return err
				}
				attempt = n.retryRetirementLocked(pending)
				scheduled = true
			default:
			}
			n.mu.Unlock()
			err := n.waitRetirement(host, pending, attempt)
			if err != nil && !scheduled {
				select {
				case <-attempt.done:
					// This call arrived while the prior attempt was running. It is
					// the operator's retry, so let it own the next real attempt.
					continue
				default:
				}
			}
			return err
		}
		nd := n.nodes[host]
		if nd == nil && !privateOnly {
			if prior := n.stopping[host]; prior != nil {
				n.mu.Unlock()
				select {
				case <-prior:
					continue
				case <-time.After(stopWait):
					return fmt.Errorf("tsnet: %s prior teardown is still unconfirmed", host)
				}
			}
			if n.closed {
				n.mu.Unlock()
				return errors.New("tsnet: network is closed")
			}
			hasState, err := persistedNodeState(filepath.Join(n.cfg.Dir, host))
			if err != nil {
				n.mu.Unlock()
				return fmt.Errorf("tsnet: inspect %s persisted state: %w", host, err)
			}
			if hasState {
				pending := &retirement{host: host, persisted: true}
				attempt := n.scheduleRetirementLocked(pending, func() error { return n.retryLogout(pending) })
				n.retiring[host] = pending
				n.notifyLocked()
				n.mu.Unlock()
				return n.waitRetirement(host, pending, attempt)
			}
		}
		n.mu.Unlock()
		if nd == nil {
			return nil
		}

		// Serialize listener setup and every kind of retirement, then re-check
		// identity. A concurrent preview/delete path may have retired nd while
		// this goroutine waited for privateMu.
		nd.privateMu.Lock()
		nd.funnelMu.Lock()
		n.mu.Lock()
		if n.nodes[host] != nd {
			n.mu.Unlock()
			nd.funnelMu.Unlock()
			nd.privateMu.Unlock()
			continue
		}
		nd.private = false
		nd.handler.Store(nil)
		if privateOnly && nd.funnelH.Load() != nil {
			servers := append([]*http.Server(nil), nd.https...)
			n.mu.Unlock()
			var errs []error
			for _, hs := range servers {
				if err := hs.Close(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
					errs = append(errs, err)
				}
			}
			err := errors.Join(errs...)
			n.mu.Lock()
			if err == nil && n.nodes[host] == nd {
				nd.https = nil
				nd.serving, nd.listenStarted = false, false
			}
			n.notifyLocked()
			n.mu.Unlock()
			nd.funnelMu.Unlock()
			nd.privateMu.Unlock()
			return err
		}
		pending, attempt := n.beginRetirementLocked(nd)
		n.mu.Unlock()
		nd.funnelMu.Unlock()
		nd.privateMu.Unlock()
		return n.waitRetirement(host, pending, attempt)
	}
}

func (n *Net) waitRetirement(host string, pending *retirement, attempt *retirementAttempt) error {
	select {
	case <-attempt.done:
		n.mu.Lock()
		err := attempt.err
		if err == nil {
			if current := n.retiring[host]; current == pending && current.attempt == attempt {
				delete(n.retiring, host)
			}
		}
		n.mu.Unlock()
		if err != nil {
			return err
		}
		return nil
	case <-time.After(stopWait):
		return fmt.Errorf("tsnet: %s background teardown is still unconfirmed", host)
	}
}

func (n *Net) beginRetirementLocked(nd *node) (*retirement, *retirementAttempt) {
	delete(n.nodes, nd.host)
	pending := &retirement{host: nd.host, ephemeral: nd.ephemeral, store: nd.store}
	attempt := n.scheduleRetirementLocked(pending, func() error { return n.teardown(nd, true) })
	n.retiring[nd.host] = pending
	n.notifyLocked()
	return pending, attempt
}

func (n *Net) retryRetirementLocked(pending *retirement) *retirementAttempt {
	return n.scheduleRetirementLocked(pending, func() error { return n.retryLogout(pending) })
}

// retryLogout starts a new backend over the retained state. Tailscale clears
// the running backend's node key before its logout request reaches control, so
// calling Logout on that same backend again cannot retry the request. A fresh
// backend reloads the still-persisted key and can complete the revocation.
func (n *Net) retryLogout(pending *retirement) error {
	dir := filepath.Join(n.cfg.Dir, pending.host)
	if n.logoutNode != nil {
		ctx, cancel := context.WithTimeout(context.Background(), logoutTimeout)
		defer cancel()
		nd := &node{host: pending.host, ephemeral: pending.ephemeral, dir: dir, store: pending.store}
		if err := n.logoutNode(ctx, nd); err != nil {
			return fmt.Errorf("tsnet %s logout retry: %w", pending.host, err)
		}
		return os.RemoveAll(dir)
	}
	snapshot, err := readStateSnapshot(dir)
	if err != nil {
		return fmt.Errorf("tsnet %s logout retry snapshot: %w", pending.host, err)
	}
	if pending.persisted && !snapshot.exists {
		// The deterministic host candidate has no identity. Do not start a
		// backend: that could mint a new node while trying to retire nothing.
		return os.RemoveAll(dir)
	}

	srv := &ts.Server{
		Dir:        dir,
		Hostname:   pending.host,
		ControlURL: n.cfg.ControlURL,
		Ephemeral:  pending.ephemeral,
		Store:      pending.store,
		UserLogf: func(format string, args ...any) {
			n.logf("tsnet %s logout retry: %s", pending.host, fmt.Sprintf(format, args...))
		},
	}
	if err := srv.Start(); err != nil {
		startErr := fmt.Errorf("tsnet %s logout retry start: %w", pending.host, err)
		if closeErr := srv.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) && !errors.Is(closeErr, context.Canceled) {
			startErr = errors.Join(startErr, fmt.Errorf("tsnet %s logout retry backend close: %w", pending.host, closeErr))
		}
		if restoreErr := snapshot.restore(dir); restoreErr != nil {
			startErr = errors.Join(startErr, fmt.Errorf("tsnet %s restore retry identity: %w", pending.host, restoreErr))
		}
		return startErr
	}
	var retryErr error
	ctx, cancel := context.WithTimeout(context.Background(), logoutTimeout)
	if lc, err := srv.LocalClient(); err != nil {
		retryErr = fmt.Errorf("tsnet %s logout retry client: %w", pending.host, err)
	} else if err := waitForLogoutState(ctx, lc); err != nil {
		retryErr = fmt.Errorf("tsnet %s logout retry backend: %w", pending.host, err)
	} else {
		cancel()
		ctx, cancel = context.WithTimeout(context.Background(), logoutTimeout)
		if err := lc.Logout(ctx); err != nil {
			retryErr = fmt.Errorf("tsnet %s logout retry: %w", pending.host, err)
		}
	}
	cancel()
	if err := srv.Close(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
		retryErr = errors.Join(retryErr, fmt.Errorf("tsnet %s logout retry backend close: %w", pending.host, err))
	}
	if retryErr == nil {
		retryErr = os.RemoveAll(dir)
	} else if err := snapshot.restore(dir); err != nil {
		retryErr = errors.Join(retryErr, fmt.Errorf("tsnet %s restore retry identity: %w", pending.host, err))
	}
	return retryErr
}

// waitForLogoutState waits only until the backend has loaded enough identity
// to accept Logout. Server.Up cannot be used here: it waits exclusively for
// Running, while an expired or unapproved key deliberately enters a login
// state that must still be revocable.
func waitForLogoutState(ctx context.Context, lc *local.Client) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	last := ""
	for {
		status, err := lc.StatusWithoutPeers(ctx)
		if err == nil {
			last = status.BackendState
			switch last {
			case ipn.Running.String(), ipn.NeedsLogin.String(), ipn.NeedsMachineAuth.String(), ipn.Starting.String(), ipn.NoState.String():
				// tsnet.Start returns only after LocalBackend.Start installed the
				// control client. For an expired key, tsnet observes NeedsLogin and
				// immediately starts interactive login, so an external observer can
				// first see Starting or NoState even though Direct still holds the
				// restored node key needed by Logout.
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("last backend state %q: %w", last, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (n *Net) scheduleRetirementLocked(pending *retirement, run func() error) *retirementAttempt {
	attempt := &retirementAttempt{done: make(chan struct{})}
	pending.attempt = attempt
	n.stopping[pending.host] = attempt.done
	n.notifyLocked()
	go func() {
		attempt.err = run()
		n.mu.Lock()
		if n.stopping[pending.host] == attempt.done {
			delete(n.stopping, pending.host)
		}
		n.mu.Unlock()
		close(attempt.done)
	}()
	return attempt
}

// retireLocked removes nd from the served set and tears it down in the
// background. The returned channel is closed when the teardown has finished;
// a later node with the same host waits for it before starting. n.mu must be
// held.
func (n *Net) retireLocked(nd *node, logout bool) chan struct{} {
	delete(n.nodes, nd.host)
	done := make(chan struct{})
	n.stopping[nd.host] = done
	n.notifyLocked()
	go func() {
		nd.stopErr = n.teardown(nd, logout) // read only after done is closed
		n.mu.Lock()
		if n.stopping[nd.host] == done {
			delete(n.stopping, nd.host)
		}
		n.mu.Unlock()
		close(done)
	}()
	return done
}

// teardown stops a node. With logout it also removes the node from the
// tailnet and deletes its state directory.
func (n *Net) teardown(nd *node, logout bool) error {
	// Cancelling first unblocks a node still waiting for its predecessor or
	// for login; the LocalAPI used for Logout does not depend on nd.ctx.
	nd.cancel()
	<-nd.started
	waited := false
	if !nd.backendStarted {
		// A successor cancelled while waiting for an earlier teardown has no
		// backend of its own. Wait for both lifetimes before deciding whether
		// any persisted identity remains to retire.
		<-nd.done
		if nd.prev != nil {
			<-nd.prev
		}
		waited = true
	}
	var errs []error
	// Wait for an in-progress private listener setup to publish all of its
	// servers, then close that complete set. Release privateMu before waiting
	// for run: run itself may need this mutex before it observes cancellation.
	nd.privateMu.Lock()
	nd.funnelMu.Lock()
	if nd.srv != nil {
		n.mu.Lock()
		servers := nd.https
		funnelSrv := nd.funnelSrv
		funnelLn := nd.funnelLn
		nd.funnelSrv = nil
		nd.funnelLn = nil
		nd.funnelState = ""
		n.mu.Unlock()
		if funnelSrv != nil {
			funnelSrv.Close()
		}
		if funnelLn != nil {
			funnelLn.Close()
		}
		for _, hs := range servers {
			if err := hs.Close(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
				errs = append(errs, fmt.Errorf("tsnet %s HTTP close: %w", nd.host, err))
			}
		}
	}
	nd.funnelMu.Unlock()
	nd.privateMu.Unlock()
	logoutOK := !logout
	var snapshot stateSnapshot
	var restoreState bool
	if logout {
		if !nd.backendStarted && n.logoutNode == nil {
			hasState, err := persistedNodeState(nd.dir)
			if err != nil {
				errs = append(errs, fmt.Errorf("tsnet %s inspect persisted state: %w", nd.host, err))
			} else if hasState {
				errs = append(errs, fmt.Errorf("tsnet %s logout: backend did not start and persisted identity remains", nd.host))
			} else {
				logoutOK = true
			}
		} else {
			var err error
			snapshot, err = readStateSnapshot(nd.dir)
			n.mu.Lock()
			if len(nd.identityState) > 0 {
				snapshot = stateSnapshot{data: append([]byte(nil), nd.identityState...), exists: true}
			}
			n.mu.Unlock()
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), logoutTimeout)
				err = n.logout(ctx, nd)
				cancel()
			}
			if err != nil {
				// Access already fails closed. Keep the state, but close this backend:
				// its in-memory key is consumed and only a fresh backend can retry.
				n.logf("tsnet %s: logout: %v", nd.host, err)
				errs = append(errs, fmt.Errorf("tsnet %s logout: %w", nd.host, err))
				restoreState = true
			} else {
				logoutOK = true
			}
		}
	}
	if nd.srv != nil {
		if err := nd.srv.Close(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
			errs = append(errs, fmt.Errorf("tsnet %s backend close: %w", nd.host, err))
		}
	}
	if restoreState {
		if err := snapshot.restore(nd.dir); err != nil {
			errs = append(errs, fmt.Errorf("tsnet %s restore logout identity: %w", nd.host, err))
		}
	}
	if !waited {
		<-nd.done
	}
	if !waited && nd.prev != nil {
		// A node cancelled while waiting for its predecessor never started;
		// keep teardowns of one host in order so the predecessor cannot
		// delete a directory that a later node is already using.
		<-nd.prev
	}
	if logoutOK && (logout || nd.ephemeral) {
		if err := os.RemoveAll(nd.dir); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func persistedNodeState(dir string) (bool, error) {
	_, err := os.Stat(filepath.Join(dir, "tailscaled.state"))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

type stateSnapshot struct {
	data   []byte
	exists bool
}

func readStateSnapshot(dir string) (stateSnapshot, error) {
	data, err := os.ReadFile(filepath.Join(dir, "tailscaled.state"))
	if err == nil {
		return stateSnapshot{data: data, exists: true}, nil
	}
	if os.IsNotExist(err) {
		return stateSnapshot{}, nil
	}
	return stateSnapshot{}, err
}

func (s stateSnapshot) restore(dir string) error {
	if !s.exists {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "tailscaled.state"), s.data)
}

func (n *Net) logout(ctx context.Context, nd *node) error {
	if n.logoutNode != nil {
		return n.logoutNode(ctx, nd)
	}
	if nd.srv == nil {
		return nil
	}
	lc, err := nd.srv.LocalClient()
	if err != nil {
		return err
	}
	return lc.Logout(ctx)
}

// Close implements core.PrivateNet. Persistent nodes are shut down but not
// logged out, so they come back with the same identity after a restart.
// Ephemeral nodes (previews) cannot come back: they are logged out, so they
// leave the tailnet now instead of lingering offline, and their directories
// are removed.
func (n *Net) Close() error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	n.closed = true
	nodes := make([]*node, 0, len(n.nodes))
	for _, nd := range n.nodes {
		nodes = append(nodes, nd)
	}
	n.nodes = map[string]*node{}
	pending := make([]chan struct{}, 0, len(n.stopping))
	for _, ch := range n.stopping {
		pending = append(pending, ch)
	}
	retirements := make([]*retirement, 0, len(n.retiring))
	for _, retirement := range n.retiring {
		retirements = append(retirements, retirement)
	}
	n.notifyLocked()
	n.mu.Unlock()

	var wg sync.WaitGroup
	errs := make([]error, len(nodes))
	for i, nd := range nodes {
		wg.Go(func() { errs[i] = n.teardown(nd, nd.ephemeral) })
	}
	all := make(chan []error, 1)
	go func() {
		wg.Wait()
		for _, ch := range pending {
			<-ch
		}
		var cleanupErrs []error
		for _, retirement := range retirements {
			attempt := retirement.attempt
			<-attempt.done
			cleanupErrs = append(cleanupErrs, attempt.err)
		}
		all <- cleanupErrs
	}()
	select {
	case cleanupErrs := <-all:
		errs = append(errs, cleanupErrs...)
	case <-time.After(stopWait):
		// Shutting down must finish; a node stuck in its backend is left to
		// the process exit (an ephemeral one is then removed by control).
		return fmt.Errorf("tsnet: nodes still shutting down after %s: %w", stopWait, context.DeadlineExceeded)
	}
	return errors.Join(errs...)
}

// URL implements core.PrivateNet. Before any node has learned the tailnet's
// MagicDNS suffix it returns a placeholder https://<host>.<tailnet>.ts.net.
func (n *Net) URL(host string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.urlLocked(host)
}

func (n *Net) urlLocked(host string) string {
	scheme := "https"
	name := ""
	plain := n.plain // best guess until the node itself is serving
	if nd, ok := n.nodes[host]; ok {
		name = nd.dnsName // control may have renamed a duplicate (host-1)
		if nd.serving {
			plain = nd.plain
		}
	}
	if plain {
		scheme = "http"
	}
	if name == "" {
		suffix := n.suffix
		if suffix == "" {
			suffix = "<tailnet>.ts.net"
		}
		name = host + "." + suffix
	}
	return scheme + "://" + name
}

// Tailnet returns the MagicDNS suffix (e.g. "tail1234.ts.net") once a node
// has learned it, else "".
func (n *Net) Tailnet() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.suffix
}

// Status implements core.PrivateNet.
func (n *Net) Status() core.NetStatus {
	n.mu.Lock()
	defer n.mu.Unlock()
	st := core.NetStatus{Kind: "tailscale", Enabled: !n.closed}
	if n.suffix != "" {
		st.Detail = "tailnet " + n.suffix
	}
	now := time.Now()
	for _, nd := range n.nodes {
		st.Hosts = append(st.Hosts, n.hostInfoLocked(nd, now))
	}
	sort.Slice(st.Hosts, func(i, j int) bool { return st.Hosts[i].Host < st.Hosts[j].Host })
	return st
}

func (n *Net) hostInfoLocked(nd *node, now time.Time) core.HostInfo {
	hi := core.HostInfo{Host: nd.host, URL: n.urlLocked(nd.host), Ephemeral: nd.ephemeral, State: StateStarting}
	if nd.keyExpiry != nil {
		hi.KeyExpiry = nd.keyExpiry.UTC().Format(time.RFC3339)
	}
	switch {
	case nd.err != nil:
		hi.State, hi.Detail = StateError, nd.err.Error()
	case nd.backend == ipn.NeedsLogin.String() && !nd.wasUp && n.cfg.AuthKey != "" && nd.authURL == "":
		// A new node logging in with the auth key passes through NeedsLogin.
		hi.Detail = "joining the tailnet with the auth key"
	case nd.backend == ipn.NeedsLogin.String():
		hi.State = StateNeedsLogin
		if nd.authURL != "" {
			hi.Detail = nd.authURL
		} else if nd.keyExpiry != nil && nd.keyExpiry.Before(now) {
			hi.Detail = "node key expired; re-authenticate, or disable key expiry for this machine in the Tailscale admin console"
		} else {
			hi.Detail = "waiting for a login URL from the coordination server"
		}
	case nd.backend == ipn.NeedsMachineAuth.String():
		hi.State, hi.Detail = StateNeedsLogin, "waiting for device approval in the Tailscale admin console"
	case !nd.private && !nd.serving && nd.bootOK && nd.backend == ipn.Running.String():
		hi.State = StateIdle
		hi.Detail = "on the tailnet; private HTTP is not served"
	case nd.serving && nd.backend == ipn.Running.String() && !nd.plain && !nd.certOK:
		hi.Detail = CertPending
		if nd.certErr != "" {
			hi.Detail += "; last attempt: " + nd.certErr
		}
	case nd.serving && nd.backend == ipn.Running.String():
		hi.State = StateReady
		if nd.keyExpiry != nil && nd.keyExpiry.Sub(now) < keyExpiringWindow {
			hi.State = StateKeyExpiring
			hi.Detail = "node key expires " + nd.keyExpiry.UTC().Format(time.RFC3339) + "; re-authenticate or disable key expiry in the Tailscale admin console"
		} else if nd.plain {
			hi.Detail = HTTPSUnavailable
		}
	}
	return hi
}

// WaitReady blocks until host is serving (ready or key-expiring), its node
// fails, or ctx ends.
func (n *Net) WaitReady(ctx context.Context, host string) error {
	for {
		n.mu.Lock()
		nd, ok := n.nodes[host]
		var hi core.HostInfo
		if ok {
			hi = n.hostInfoLocked(nd, time.Now())
		}
		ch := n.changed
		n.mu.Unlock()
		if !ok {
			return fmt.Errorf("tsnet: %s is not served", host)
		}
		switch hi.State {
		case StateReady, StateKeyExpiring:
			return nil
		case StateError:
			return fmt.Errorf("tsnet: %s: %s", host, hi.Detail)
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return fmt.Errorf("tsnet: %s is %s: %w", host, hi.State, ctx.Err())
		}
	}
}

var _ core.PrivateNet = (*Net)(nil)
