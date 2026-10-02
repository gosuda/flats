// Package portal exposes public flats through Portal relays
// (github.com/gosuda/portal-tunnel). Each served slug gets one Portal
// exposure under a persisted identity named after the slug, so its public
// hostname (<slug>.<relay host>) stays stable across restarts.
package portal

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gosuda/portal-tunnel/v2/portal/identity"
	"github.com/gosuda/portal-tunnel/v2/sdk"
	"github.com/gosuda/portal-tunnel/v2/types"
	"github.com/gosuda/portal-tunnel/v2/utils"

	"github.com/oesni/flats/internal/core"
)

// DefaultMaxActiveRelays matches the Portal CLI's --max-active-relays default.
const DefaultMaxActiveRelays = 3

// ConflictHint is shown when a relay rejects the flat's hostname.
const ConflictHint = "another Portal user owns this name on that relay; rename the flat"

// Config configures a Net.
type Config struct {
	// Dir holds one identity file per slug (<Dir>/<slug>.json, mode 0600).
	Dir string
	// Relays lists explicit relays. Empty means the Portal CLI default:
	// discovery over the bootstrap relays only.
	Relays []string
	// Discovery lets Portal add discovered bootstrap relays next to the
	// explicit ones, like the Portal CLI's --discovery (its default is on).
	// With discovery off, at least one explicit relay is required.
	Discovery bool
	// MaxActiveRelays caps the relays discovery adds (default 3).
	MaxActiveRelays int
	// Logf receives SDK warnings and relay state changes. Nil discards them.
	Logf func(string, ...any)
}

// exposure is the part of *sdk.Exposure Net uses; tests substitute a fake.
type exposure interface {
	net.Listener
	Relays() []sdk.RelayStatus
	Updates() <-chan sdk.RelayStatus
	UpdateMetadata(types.LeaseMetadata) error
	WaitReady(ctx context.Context) ([]sdk.RelayStatus, error)
}

// exposeFunc creates an exposure; maxActive <= 0 turns discovery off.
type exposeFunc func(ctx context.Context, id types.Identity, relays []string, maxActive int, meta types.LeaseMetadata) (exposure, error)

func sdkExpose(ctx context.Context, id types.Identity, relays []string, maxActive int, meta types.LeaseMetadata) (exposure, error) {
	opts := []sdk.Option{sdk.WithMetadata(meta)}
	if maxActive > 0 {
		opts = append(opts, sdk.WithDiscovery(maxActive))
	}
	e, err := sdk.Expose(ctx, id, relays, opts...)
	if err != nil {
		return nil, err
	}
	return e, nil
}

// Net implements core.PublicNet with Portal.
type Net struct {
	cfg     Config
	relays  []string // normalized explicit relays, config order
	expose  exposeFunc
	runHTTP func(ctx context.Context, ln net.Listener, h http.Handler) error
	logf    func(string, ...any)

	ctx    context.Context // parent of every exposure; cancelled by Close
	cancel context.CancelFunc

	mu      sync.RWMutex
	closed  bool
	entries map[string]*entry
	locks   map[string]*sync.Mutex // per-slug Serve/Stop/SetHidden serialization
}

var _ core.PublicNet = (*Net)(nil)

type entry struct {
	slug    string
	exp     exposure
	ln      *drainListener // what RunHTTP serves; closing it leaves exp registered
	handler *handlerBox
	cancel  context.CancelFunc // stops RunHTTP and the watcher
	done    chan struct{}      // closed when RunHTTP and the watcher returned

	mu      sync.Mutex
	hidden  bool
	primary string                     // discovered relay whose URL URL() keeps returning while it is ready
	sticky  map[string]sdk.RelayStatus // terminal failures, kept after deselection
	logged  map[string]string          // relayURL -> last logged state
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

// New validates cfg and prepares the identity directory. It does not
// contact any relay.
func New(cfg Config) (*Net, error) {
	if cfg.Dir == "" {
		return nil, errors.New("portal: identity directory is required")
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("portal: identity directory: %w", err)
	}
	if cfg.MaxActiveRelays <= 0 {
		cfg.MaxActiveRelays = DefaultMaxActiveRelays
	}
	relays, err := NormalizeRelays(cfg.Relays)
	if err != nil {
		return nil, err
	}
	if !cfg.Discovery && len(relays) == 0 {
		return nil, errors.New("portal: relay discovery is off and no relays are configured")
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	installSDKLogger(cfg.Logf)
	ctx, cancel := context.WithCancel(context.Background())
	return &Net{
		cfg:     cfg,
		relays:  relays,
		expose:  sdkExpose,
		runHTTP: func(ctx context.Context, ln net.Listener, h http.Handler) error { return sdk.RunHTTP(ctx, ln, h, "") },
		logf:    logf,
		ctx:     ctx,
		cancel:  cancel,
		entries: map[string]*entry{},
		locks:   map[string]*sync.Mutex{},
	}, nil
}

// NormalizeRelays validates relay URLs the way the Portal CLI does and
// returns them normalized and deduplicated; blank entries are skipped.
func NormalizeRelays(in []string) ([]string, error) {
	var relays []string
	for _, r := range in {
		if strings.TrimSpace(r) == "" {
			continue
		}
		u, err := utils.NormalizeRelayURL(r)
		if err != nil {
			return nil, fmt.Errorf("portal: relay %q: %w", r, err)
		}
		if !slices.Contains(relays, u) {
			relays = append(relays, u)
		}
	}
	return relays, nil
}

func (n *Net) slugLock(slug string) *sync.Mutex {
	n.mu.Lock()
	defer n.mu.Unlock()
	l, ok := n.locks[slug]
	if !ok {
		l = &sync.Mutex{}
		n.locks[slug] = l
	}
	return l
}

func (n *Net) identityPath(slug string) (string, error) {
	if slug == "" || slug != filepath.Base(slug) || strings.ContainsAny(slug, `/\`) || strings.HasPrefix(slug, ".") {
		return "", fmt.Errorf("portal: invalid slug %q", slug)
	}
	return filepath.Join(n.cfg.Dir, slug+".json"), nil
}

// loadIdentity loads <Dir>/<slug>.json or creates it. An existing file wins,
// which keeps the public hostname stable.
func (n *Net) loadIdentity(slug string) (types.Identity, error) {
	path, err := n.identityPath(slug)
	if err != nil {
		return types.Identity{}, err
	}
	id, err := identity.LoadOrCreate(slug, "", path, "")
	if err != nil {
		return types.Identity{}, fmt.Errorf("portal: identity for %s: %w", slug, err)
	}
	// LoadOrCreate writes new files 0600 but leaves an existing file as is.
	if err := os.Chmod(path, 0o600); err != nil {
		return types.Identity{}, fmt.Errorf("portal: identity for %s: %w", slug, err)
	}
	return id, nil
}

func metadata(slug string, hidden bool) types.LeaseMetadata {
	return types.LeaseMetadata{Hide: hidden, Description: "Flats: " + slug}
}

// Serve exposes h for slug and returns at once; relays register in the
// background. The returned URL is empty until a relay is ready (see
// WaitReady). Serving an already-served slug swaps the handler and applies
// hidden without re-registering.
func (n *Net) Serve(ctx context.Context, slug string, h http.Handler, hidden bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if h == nil {
		return "", errors.New("portal: handler is nil")
	}
	l := n.slugLock(slug)
	l.Lock()
	defer l.Unlock()

	n.mu.RLock()
	closed, e := n.closed, n.entries[slug]
	n.mu.RUnlock()
	if closed {
		return "", errors.New("portal: closed")
	}
	if e != nil {
		e.handler.set(h)
		if err := n.setHidden(e, hidden); err != nil {
			return "", err
		}
		return n.URL(slug), nil
	}

	id, err := n.loadIdentity(slug)
	if err != nil {
		return "", err
	}
	// The exposure outlives the request that asked for it, so it hangs off
	// n.ctx rather than ctx.
	maxActive := 0 // discovery off
	if n.cfg.Discovery {
		maxActive = n.cfg.MaxActiveRelays
	}
	exp, err := n.expose(n.ctx, id, slices.Clone(n.relays), maxActive, metadata(slug, hidden))
	if err != nil {
		return "", fmt.Errorf("portal: expose %s: %w", slug, err)
	}
	runCtx, cancel := context.WithCancel(n.ctx)
	e = &entry{
		slug:    slug,
		exp:     exp,
		ln:      newDrainListener(exp),
		handler: &handlerBox{h: h},
		cancel:  cancel,
		done:    make(chan struct{}),
		hidden:  hidden,
		sticky:  map[string]sdk.RelayStatus{},
		logged:  map[string]string{},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := n.runHTTP(runCtx, e.ln, publicHandler(e.handler)); err != nil {
			n.logf("portal: %s: http server stopped: %v", slug, err)
		}
	}()
	go func() {
		defer wg.Done()
		n.watch(runCtx, e)
	}()
	go func() {
		wg.Wait()
		close(e.done)
	}()

	n.mu.Lock()
	if n.closed {
		// Close ran while this exposure was being created and did not see it.
		n.mu.Unlock()
		return "", errors.Join(errors.New("portal: closed"), n.stopEntry(e))
	}
	n.entries[slug] = e
	n.mu.Unlock()
	return n.URL(slug), nil
}

// publicHandler strips identity headers a visitor could forge; the public
// path never carries a Tailscale identity.
func publicHandler(b *handlerBox) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k := range r.Header {
			if strings.HasPrefix(k, "Tailscale-User-") {
				r.Header.Del(k)
			}
		}
		b.get().ServeHTTP(w, r)
	})
}

// SetHidden toggles relay listing. Relays pick it up at the next lease
// renewal, up to about 90 s later.
func (n *Net) SetHidden(slug string, hidden bool) error {
	l := n.slugLock(slug)
	l.Lock()
	defer l.Unlock()
	n.mu.RLock()
	e := n.entries[slug]
	n.mu.RUnlock()
	if e == nil {
		return fmt.Errorf("portal: %s is not served", slug)
	}
	return n.setHidden(e, hidden)
}

// setHidden runs under the slug lock. It does not hold e.mu across
// UpdateMetadata, which waits for the SDK's relay reconciliation (and that
// can include unregister round trips), so Status and the watcher never block
// on it.
func (n *Net) setHidden(e *entry, hidden bool) error {
	e.mu.Lock()
	same := e.hidden == hidden
	e.mu.Unlock()
	if same {
		return nil
	}
	if err := e.exp.UpdateMetadata(metadata(e.slug, hidden)); err != nil {
		return fmt.Errorf("portal: update listing for %s: %w", e.slug, err)
	}
	e.mu.Lock()
	e.hidden = hidden
	e.mu.Unlock()
	return nil
}

// Stop shuts the HTTP server down, unregisters slug from its relays and
// forgets its status. Unknown slugs are a no-op.
func (n *Net) Stop(slug string) error {
	l := n.slugLock(slug)
	l.Lock()
	defer l.Unlock()
	n.mu.Lock()
	e := n.entries[slug]
	delete(n.entries, slug)
	n.mu.Unlock()
	if e == nil {
		return nil
	}
	return n.stopEntry(e)
}

func (n *Net) stopEntry(e *entry) error {
	// Drain HTTP first, then unregister. RunHTTP's shutdown closes only the
	// drain listener, so the relays keep routing in-flight requests to us
	// until exp.Close below.
	e.cancel()
	<-e.done
	err := e.exp.Close()
	<-e.ln.pumped
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("portal: close %s: %w", e.slug, err)
	}
	return nil
}

// Close stops every exposure. Later Serve calls fail.
func (n *Net) Close() error {
	n.mu.Lock()
	n.closed = true
	slugs := make([]string, 0, len(n.entries))
	for s := range n.entries {
		slugs = append(slugs, s)
	}
	n.mu.Unlock()

	errs := make([]error, len(slugs))
	var wg sync.WaitGroup
	for i, s := range slugs {
		wg.Go(func() { errs[i] = n.Stop(s) })
	}
	wg.Wait()
	n.cancel()
	return errors.Join(errs...)
}

func (n *Net) entry(slug string) *entry {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.entries[slug]
}

// URLs returns every ready public URL of slug: explicit relays first in
// config order, then the discovered relay URL() returned before (while it
// stays ready), then the rest by relay URL.
func (n *Net) URLs(slug string) []string {
	e := n.entry(slug)
	if e == nil {
		return nil
	}
	return n.readyURLs(e, e.exp.Relays())
}

// readyURLs orders the ready relays of sts and pins the first discovered
// one as e.primary, so a relay discovery adds later cannot take over URL().
func (n *Net) readyURLs(e *entry, sts []sdk.RelayStatus) []string {
	var ready []sdk.RelayStatus
	for _, s := range sts {
		if s.State == sdk.RelayReady && !s.Deselected && s.PublicURL != "" {
			ready = append(ready, s)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	ready = n.ordered(ready, e.primary)
	if len(ready) > 0 && !slices.Contains(n.relays, ready[0].RelayURL) {
		e.primary = ready[0].RelayURL
	}
	urls := make([]string, len(ready))
	for i, s := range ready {
		urls[i] = s.PublicURL
	}
	return urls
}

// URL returns the first ready public URL, or "" while no relay is ready.
func (n *Net) URL(slug string) string {
	if urls := n.URLs(slug); len(urls) > 0 {
		return urls[0]
	}
	return ""
}

// WaitReady blocks until slug is ready on at least one relay and returns its
// public URLs.
func (n *Net) WaitReady(ctx context.Context, slug string) ([]string, error) {
	e := n.entry(slug)
	if e == nil {
		return nil, fmt.Errorf("portal: %s is not served", slug)
	}
	if _, err := e.exp.WaitReady(ctx); err != nil {
		return nil, err
	}
	for {
		if urls := n.URLs(slug); len(urls) > 0 {
			return urls, nil
		}
		// Ready but the relay has not reported a URL yet; rare, retry briefly.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
		if n.entry(slug) != e {
			return nil, fmt.Errorf("portal: %s was stopped", slug)
		}
	}
}

// ordered sorts statuses with explicit relays first (config order), then
// primary, then the rest by relay URL, so URL() stays stable as discovery
// adds relays.
func (n *Net) ordered(sts []sdk.RelayStatus, primary string) []sdk.RelayStatus {
	rank := func(u string) int {
		if i := slices.Index(n.relays, u); i >= 0 {
			return i
		}
		if u == primary {
			return len(n.relays)
		}
		return len(n.relays) + 1
	}
	out := slices.Clone(sts)
	slices.SortStableFunc(out, func(a, b sdk.RelayStatus) int {
		if ra, rb := rank(a.RelayURL), rank(b.RelayURL); ra != rb {
			return ra - rb
		}
		return strings.Compare(a.RelayURL, b.RelayURL)
	})
	return out
}

const pollInterval = 2 * time.Second

// watch records terminal failures (which can vanish from Relays() once
// discovery drops the relay) and logs relay state changes. Updates is
// best-effort, so it also polls.
func (n *Net) watch(ctx context.Context, e *entry) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	n.observe(e, e.exp.Relays()...)
	for {
		select {
		case <-ctx.Done():
			return
		case st := <-e.exp.Updates():
			n.observe(e, st)
			n.observe(e, e.exp.Relays()...)
		case <-t.C:
			n.observe(e, e.exp.Relays()...)
		}
	}
}

func (n *Net) observe(e *entry, sts ...sdk.RelayStatus) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range sts {
		if s.RelayURL == "" {
			continue
		}
		permanent := s.State == sdk.RelayFailed && (s.Failure == sdk.RelayFailureTerminal || s.Failure == sdk.RelayFailureMITM)
		switch {
		case permanent:
			e.sticky[s.RelayURL] = s
		case !s.Deselected:
			delete(e.sticky, s.RelayURL)
		}
		state := string(s.State)
		if s.Deselected {
			state = "deselected"
		}
		if e.logged[s.RelayURL] == state {
			continue
		}
		e.logged[s.RelayURL] = state
		switch {
		case s.Deselected:
			if s.PublicURL != "" {
				n.logf("portal: %s: no longer served at %s", e.slug, s.PublicURL)
			}
		case s.State == sdk.RelayReady:
			n.logf("portal: %s: ready at %s", e.slug, s.PublicURL)
		case s.State == sdk.RelayFailed:
			n.logf("portal: %s: %s", e.slug, describeFailure(s))
		}
	}
}

func isHostnameConflict(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, &types.APIRequestError{Code: types.APIErrorCodeHostnameConflict}) ||
		strings.Contains(err.Error(), types.APIErrorCodeHostnameConflict)
}

func relayHost(relayURL string) string {
	if u, err := url.Parse(relayURL); err == nil && u.Host != "" {
		return u.Host
	}
	return relayURL
}

func describeFailure(s sdk.RelayStatus) string {
	host := relayHost(s.RelayURL)
	msg := "failed"
	if s.Err != nil {
		msg = s.Err.Error()
	}
	switch {
	case isHostnameConflict(s.Err):
		return host + ": hostname conflict: " + ConflictHint
	case s.Failure == sdk.RelayFailureMITM:
		return host + ": TLS interception detected, relay blocked: " + msg
	case s.Failure == sdk.RelayFailureTerminal:
		return host + ": rejected permanently: " + msg
	default:
		return host + ": " + msg + " (retrying)"
	}
}

// Status reports one host per served slug.
func (n *Net) Status() core.NetStatus {
	ns := core.NetStatus{Kind: "portal", Enabled: true, Hosts: []core.HostInfo{}}
	hosts := make([]string, len(n.relays))
	for i, r := range n.relays {
		hosts[i] = relayHost(r)
	}
	switch {
	case len(n.relays) == 0:
		ns.Detail = fmt.Sprintf("relays: Portal defaults (discovery, up to %d active)", n.cfg.MaxActiveRelays)
	case n.cfg.Discovery:
		ns.Detail = fmt.Sprintf("relays: %s plus discovery (up to %d more)", strings.Join(hosts, ", "), n.cfg.MaxActiveRelays)
	default:
		ns.Detail = fmt.Sprintf("relays: %s only (discovery off)", strings.Join(hosts, ", "))
	}

	n.mu.RLock()
	entries := make([]*entry, 0, len(n.entries))
	for _, e := range n.entries {
		entries = append(entries, e)
	}
	n.mu.RUnlock()
	slices.SortFunc(entries, func(a, b *entry) int { return strings.Compare(a.slug, b.slug) })

	var conflicts []string
	for _, e := range entries {
		hi, conflict := n.hostInfo(e)
		if conflict {
			conflicts = append(conflicts, e.slug)
		}
		ns.Hosts = append(ns.Hosts, hi)
	}
	if len(conflicts) > 0 {
		ns.Detail += fmt.Sprintf("; hostname conflict for %s: %s", strings.Join(conflicts, ", "), ConflictHint)
	}
	return ns
}

func (n *Net) hostInfo(e *entry) (core.HostInfo, bool) {
	live := e.exp.Relays()
	e.mu.Lock()
	hidden := e.hidden
	merged := maps.Clone(e.sticky)
	e.mu.Unlock()
	for _, s := range live {
		if s.Deselected {
			continue
		}
		// A recorded terminal failure explains more than a later bare failure.
		if _, ok := merged[s.RelayURL]; ok && s.State == sdk.RelayFailed {
			continue
		}
		merged[s.RelayURL] = s
	}
	ready := n.readyURLs(e, live)
	all := n.ordered(slices.Collect(maps.Values(merged)), "")

	var connecting, failed []string
	permanent, conflict := 0, false
	for _, s := range all {
		switch s.State {
		case sdk.RelayConnecting:
			connecting = append(connecting, relayHost(s.RelayURL))
		case sdk.RelayFailed:
			failed = append(failed, describeFailure(s))
			if s.Failure == sdk.RelayFailureTerminal || s.Failure == sdk.RelayFailureMITM {
				permanent++
			}
			conflict = conflict || isHostnameConflict(s.Err)
		}
	}

	hi := core.HostInfo{Host: e.slug}
	var parts []string
	switch {
	case len(ready) > 0:
		hi.State, hi.URL = "ready", ready[0]
		parts = append(parts, "ready at "+strings.Join(ready, ", "))
	case conflict:
		hi.State = "error"
	case len(all) == 0:
		hi.State = "starting"
		parts = append(parts, "discovering relays (can take about a minute)")
	case len(connecting) == 0 && permanent == len(all):
		hi.State = "error"
	default:
		hi.State = "starting"
	}
	if len(connecting) > 0 {
		parts = append(parts, "connecting to "+strings.Join(connecting, ", "))
	}
	if len(failed) > 0 {
		parts = append(parts, "failed: "+strings.Join(failed, "; "))
	}
	if hidden {
		parts = append(parts, "unlisted")
	}
	hi.Detail = strings.Join(parts, "; ")
	return hi, conflict
}

// drainListener lets http.Server.Shutdown stop accepting and drain in-flight
// requests without closing the exposure: http.Server closes its listener
// first, and closing the exposure unregisters it from every relay.
type drainListener struct {
	net.Listener
	conns  chan net.Conn
	closed chan struct{} // closed by Close
	failed chan struct{} // closed when the exposure's Accept fails
	pumped chan struct{} // closed when the pump returned
	err    error         // Accept error; read after failed is closed
	once   sync.Once
}

func newDrainListener(ln net.Listener) *drainListener {
	d := &drainListener{
		Listener: ln,
		conns:    make(chan net.Conn),
		closed:   make(chan struct{}),
		failed:   make(chan struct{}),
		pumped:   make(chan struct{}),
	}
	go d.pump()
	return d
}

// pump accepts from the exposure until the exposure closes. Connections
// that arrive after Close are dropped.
func (d *drainListener) pump() {
	defer close(d.pumped)
	for {
		c, err := d.Listener.Accept()
		if err != nil {
			d.err = err
			close(d.failed)
			return
		}
		select {
		case d.conns <- c:
		case <-d.closed:
			c.Close()
		}
	}
}

func (d *drainListener) Accept() (net.Conn, error) {
	select {
	case <-d.closed:
		return nil, net.ErrClosed
	default:
	}
	select {
	case c := <-d.conns:
		return c, nil
	case <-d.closed:
		return nil, net.ErrClosed
	case <-d.failed:
		return nil, d.err
	}
}

// Close stops Accept only; the exposure stays open and registered.
func (d *drainListener) Close() error {
	d.once.Do(func() { close(d.closed) })
	return nil
}
