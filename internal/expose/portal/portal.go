// Package portal exposes public flats through Portal relays
// (github.com/gosuda/portal-tunnel). Each served slug gets one Portal
// exposure under a persisted identity named after the slug. Relays report a
// canonical hostname bound to the identity address, which stays stable
// across restarts: <slug>-<address>.<relay host>, or
// <name hash>--<address>.<relay host> for slugs longer than 22 characters.
// Relays also grant the friendly <slug>.<relay host> alias while no other
// identity holds it; Flats shows that URL when granted, else the canonical
// one, and warns when an alias it showed is no longer granted (see the
// alias memory comment above aliasPath).
package portal

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosuda/portal-tunnel/v2/portal/identity"
	"github.com/gosuda/portal-tunnel/v2/sdk"
	"github.com/gosuda/portal-tunnel/v2/types"
	"github.com/gosuda/portal-tunnel/v2/utils"

	"github.com/gosuda/flats/internal/core"
)

// DefaultMaxActiveRelays matches the Portal CLI's --max-active-relays default.
const DefaultMaxActiveRelays = 3

// ConflictHint is shown when a relay rejects the flat's hostname.
const ConflictHint = "another Portal user owns this name on that relay; rename the flat"

// Config configures a Net.
type Config struct {
	// ShutdownTimeout bounds HTTP drain, SDK close and listener-pump waits (default 30s).
	ShutdownTimeout time.Duration
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
	AddRelay(relayURL string) error
	RemoveRelay(relayURL string) error
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

	mu        sync.RWMutex
	closeOnce sync.Once
	closeErr  error
	ops       sync.WaitGroup // admitted operations; Add is serialized with closing
	closed    bool
	entries   map[string]*entry
	locks     map[string]*sync.Mutex // per-slug Serve/Stop/SetHidden serialization
	leaseTTL  time.Duration          // tests shorten it; zero means sdkLeaseTTL
}

// sdkLeaseTTL is the lease lifetime the Portal SDK requests and renews
// (portal-tunnel v2.6.3 sdk/listener.go defaultLeaseTTL). Once SDK Close has
// returned, nothing renews the lease, so a relay that missed the unregister
// request drops it within this time.
const sdkLeaseTTL = 2 * time.Minute

func (n *Net) leaseExpiry() time.Duration {
	if n.leaseTTL > 0 {
		return n.leaseTTL
	}
	return sdkLeaseTTL
}

var _ core.PublicNet = (*Net)(nil)

type entry struct {
	slug    string
	exp     exposure
	ln      *drainListener // what RunHTTP serves; closing it leaves exp registered
	handler *handlerBox
	cancel  context.CancelFunc // stops RunHTTP and the watcher
	httpErr error              // read only after done closes
	done    chan struct{}      // closed when RunHTTP and the watcher returned

	// The slug lock owns teardown state; e.mu guards stopping for status readers.
	stopping    bool
	closeResult <-chan closeResult // one SDK Close attempt, shared across retries
	closeDone   bool
	closeErr    error     // the first Close error; SDK Close uses sync.Once
	closedAt    time.Time // when that Close returned
	httpSeen    bool      // HTTP shutdown error already reported
	pumpSeen    bool      // listener pump error already reported
	stopErr     error     // latest unconfirmed teardown failure; guarded by Net.mu

	mu      sync.Mutex
	hidden  bool
	pinned  string                     // discovered relay kept as explicit so the URL stays (see pin)
	downAt  time.Time                  // when the pinned relay stopped being ready; zero while ready
	primary string                     // discovered relay whose URL URL() keeps returning while it is ready
	sticky  map[string]sdk.RelayStatus // terminal failures, kept after deselection
	logged  map[string]string          // relayURL -> last logged state
	aliases map[string]string          // relayURL -> alias URL last shown (see aliasPath)
}

type handlerBox struct {
	mu     sync.RWMutex
	h      http.Handler
	active atomic.Int64 // requests inside publicHandler
}

// idle reports whether no request is running. A nil box has none.
func (b *handlerBox) idle() bool { return b == nil || b.active.Load() == 0 }

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
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 30 * time.Second
	}
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
	id, err := identity.LoadOrCreate(slug, path, "")
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
	if !n.beginOp() {
		return "", errors.New("portal: closed")
	}
	defer n.ops.Done()
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
		e.mu.Lock()
		stopping := e.stopping
		e.mu.Unlock()
		if stopping {
			// Never revive an exposure being torn down. Finish that teardown
			// first; a confirmed one lets this Serve register afresh.
			if err := n.finishStop(slug, e); err != nil {
				return "", fmt.Errorf("portal: %s teardown not confirmed: %w", slug, err)
			}
			e = nil
		}
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
	relays := slices.Clone(n.relays)
	pinned := n.loadPin(slug)
	if pinned != "" && !slices.Contains(relays, pinned) {
		relays = append(relays, pinned)
	}
	exp, err := n.expose(n.ctx, id, relays, maxActive, metadata(slug, hidden))
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
		pinned:  pinned,
		sticky:  map[string]sdk.RelayStatus{},
		logged:  map[string]string{},
		aliases: n.loadAliases(slug),
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		e.httpErr = n.runHTTP(runCtx, e.ln, publicHandler(e.handler))
		if err := e.httpErr; err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
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
		// Close already took its entry snapshot, but it still waits for
		// this admitted Serve. Retain failed cleanup for later inspection.
		e.mu.Lock()
		e.stopping = true
		e.mu.Unlock()
		n.entries[slug] = e
		n.mu.Unlock()
		stopErr := n.stopEntry(e)
		n.settleStop(slug, e, stopErr)
		return "", errors.Join(errors.New("portal: closed"), stopErr)
	}
	n.entries[slug] = e
	n.mu.Unlock()
	return n.URL(slug), nil
}

// publicHandler strips identity headers a visitor could forge; the public
// path never carries a Tailscale identity.
func publicHandler(b *handlerBox) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.active.Add(1)
		defer b.active.Add(-1)
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
	if !n.beginOp() {
		return errors.New("portal: closed")
	}
	defer n.ops.Done()
	l := n.slugLock(slug)
	l.Lock()
	defer l.Unlock()
	n.mu.RLock()
	e := n.entries[slug]
	n.mu.RUnlock()
	if e == nil {
		return fmt.Errorf("portal: %s is not served", slug)
	}
	e.mu.Lock()
	stopping := e.stopping
	e.mu.Unlock()
	if stopping {
		return fmt.Errorf("portal: %s teardown not confirmed", slug)
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

// Stop shuts down HTTP and unregisters slug from its relays. It forgets the
// exposure only after all teardown obligations succeed. Unknown slugs are
// a no-op.
func (n *Net) Stop(slug string) error {
	if !n.beginOp() {
		n.mu.RLock()
		e := n.entries[slug]
		var stopErr error
		if e != nil {
			stopErr = e.stopErr
		}
		n.mu.RUnlock()
		if e != nil {
			return errors.Join(fmt.Errorf("portal: %s teardown not confirmed: network closed", slug), stopErr)
		}
		return nil
	}
	defer n.ops.Done()
	return n.stop(slug)
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

func (n *Net) stop(slug string) error {
	l := n.slugLock(slug)
	l.Lock()
	defer l.Unlock()

	n.mu.RLock()
	e := n.entries[slug]
	n.mu.RUnlock()
	if e == nil {
		return nil
	}
	return n.finishStop(slug, e)
}

// finishStop tears e down, or resumes its pending teardown, and forgets it
// once confirmed. The slug lock is held.
func (n *Net) finishStop(slug string, e *entry) error {
	e.mu.Lock()
	e.stopping = true
	e.mu.Unlock()

	err := n.stopEntry(e)
	n.settleStop(slug, e, err)
	return err
}

// settleStop forgets a confirmed teardown or records why it is still
// unconfirmed. Close reports the failures of entries that remain, so a later
// retry that confirms teardown also clears an earlier failure.
func (n *Net) settleStop(slug string, e *entry, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err == nil {
		delete(n.entries, slug)
		return
	}
	e.stopErr = err
}

// stopEntry holds the slug lock. It resumes pending work without issuing
// another SDK Close: SDK v2 uses sync.Once and later calls lose the first error.
func (n *Net) stopEntry(e *entry) error {
	ctx, cancel := context.WithTimeout(context.Background(), n.cfg.ShutdownTimeout)
	defer cancel()
	var errs []error
	wait := func(done <-chan struct{}, stage string) bool {
		select {
		case <-done:
			return true
		case <-ctx.Done():
			errs = append(errs, fmt.Errorf("portal: %s %s exceeded %s: %w", e.slug, stage, n.cfg.ShutdownTimeout, ctx.Err()))
		}
		return false
	}

	e.cancel()
	if wait(e.done, "HTTP drain") {
		switch err := e.httpErr; {
		case errors.Is(err, context.DeadlineExceeded):
			// sdk.RunHTTP gives up after its own shutdown timeout and returns
			// while requests may still run. Those requests are the
			// obligation, not the historical error: wait for them.
			if !waitIdle(ctx, e.handler) {
				errs = append(errs, fmt.Errorf("portal: %s HTTP drain: requests still running after %s: %w", e.slug, n.cfg.ShutdownTimeout, err))
			}
		case err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) && !e.httpSeen:
			// A finished server leaves nothing to retry: report it once.
			e.httpSeen = true
			errs = append(errs, fmt.Errorf("portal: %s HTTP shutdown: %w", e.slug, err))
		}
	}
	// Stop accepting even if HTTP drain timed out; the pump remains an
	// obligation until the underlying exposure closes.
	e.ln.Close()
	if e.closeResult == nil {
		closed := make(chan closeResult, 1)
		e.closeResult = closed
		go func() {
			err := e.exp.Close()
			closed <- closeResult{err: err, at: time.Now()}
		}()
	}
	if !e.closeDone {
		select {
		case r := <-e.closeResult:
			e.closeDone = true
			e.closedAt = r.at
			if err := r.err; err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
				e.closeErr = fmt.Errorf("portal: close %s: %w", e.slug, err)
			}
		case <-ctx.Done():
			errs = append(errs, fmt.Errorf("portal: %s exposure close exceeded %s: %w", e.slug, n.cfg.ShutdownTimeout, ctx.Err()))
		}
	}
	// A failed unregister stays unconfirmed until the relay must have
	// dropped the unrenewed lease; calling Close again would return nil.
	if e.closeErr != nil {
		if left := n.leaseExpiry() - time.Since(e.closedAt); left > 0 {
			errs = append(errs, fmt.Errorf("%w (the relay drops the lease within %s)", e.closeErr, (left+time.Second-1).Truncate(time.Second)))
		}
	}
	if wait(e.ln.pumped, "listener pump") && e.ln.err != nil && !errors.Is(e.ln.err, net.ErrClosed) && !errors.Is(e.ln.err, context.Canceled) && !e.pumpSeen {
		e.pumpSeen = true // the pump has returned: report its error once
		errs = append(errs, fmt.Errorf("portal: %s listener pump: %w", e.slug, e.ln.err))
	}
	return errors.Join(errs...)
}

// closeResult is the outcome of the one SDK Close call and when it returned.
type closeResult struct {
	err error
	at  time.Time
}

// waitIdle waits until no request runs in b or ctx ends.
func waitIdle(ctx context.Context, b *handlerBox) bool {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for !b.idle() {
		select {
		case <-t.C:
		case <-ctx.Done():
			return b.idle()
		}
	}
	return true
}

// Close stops every exposure. Later Serve calls fail.
func (n *Net) Close() error {
	n.closeOnce.Do(func() {
		n.mu.Lock()
		n.closed = true
		slugs := make([]string, 0, len(n.entries))
		for s := range n.entries {
			slugs = append(slugs, s)
		}
		n.mu.Unlock()

		var wg sync.WaitGroup
		for _, s := range slugs {
			wg.Go(func() { n.stop(s) })
		}
		// The SDK closes an exposure on parent cancellation and discards the
		// first Close error. Keep that parent alive until explicit teardown
		// drains HTTP and captures unregister errors, including an admitted
		// Stop with unconfirmed teardown or a late Serve's cleanup.
		// On timeout cancellation unblocks context-aware SDK work; the caller
		// receives a deadline error even if background cleanup outlives Close.
		defer n.cancel()
		done := make(chan struct{})
		go func() { wg.Wait(); n.ops.Wait(); close(done) }()
		select {
		case <-done:
			n.mu.Lock()
			n.closeErr = n.unconfirmedErr()
			n.mu.Unlock()
		case <-time.After(n.cfg.ShutdownTimeout):
			n.closeErr = fmt.Errorf("portal: network shutdown exceeded %s: %w", n.cfg.ShutdownTimeout, context.DeadlineExceeded)
		}
	})
	return n.closeErr
}

// unconfirmedErr joins the failures of exposures whose teardown is still
// unconfirmed, in slug order. n.mu is held.
func (n *Net) unconfirmedErr() error {
	var errs []error
	for _, slug := range slices.Sorted(maps.Keys(n.entries)) {
		e := n.entries[slug]
		if err := e.stopErr; err != nil {
			errs = append(errs, err)
		} else {
			errs = append(errs, fmt.Errorf("portal: %s teardown not confirmed", slug))
		}
	}
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
		if isReady(s) {
			ready = append(ready, s)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	ready = n.ordered(ready, e.pinned, e.primary)
	if len(ready) > 0 && !slices.Contains(n.relays, ready[0].RelayURL) {
		e.primary = ready[0].RelayURL
	}
	urls := make([]string, len(ready))
	for i, s := range ready {
		urls[i] = displayURL(s)
	}
	return urls
}

// displayURL is the URL Flats shows for a relay: the friendly
// <slug>.<relay host> alias when the relay granted it to this exposure,
// else the identity-bound canonical URL. A default https port is dropped.
func displayURL(s sdk.RelayStatus) string {
	raw := cmp.Or(s.AliasURL, s.PublicURL)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Port() != "443" {
		return raw
	}
	u.Host = strings.TrimSuffix(u.Host, ":443")
	return u.String()
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
// the pinned relay, then primary, then the rest by relay URL, so URL() stays
// stable as discovery adds relays.
func (n *Net) ordered(sts []sdk.RelayStatus, pinned, primary string) []sdk.RelayStatus {
	rank := func(u string) int {
		if i := slices.Index(n.relays, u); i >= 0 {
			return i
		}
		switch u {
		case pinned:
			return len(n.relays)
		case primary:
			return len(n.relays) + 1
		}
		return len(n.relays) + 2
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
	var aliases map[string]string
	if n.record(e, sts) {
		aliases = maps.Clone(e.aliases)
	}
	old, next := n.pinChange(e, sts, time.Now())
	e.mu.Unlock()
	if aliases != nil {
		n.saveAliases(e.slug, aliases)
	}
	if next != "" {
		n.repin(e, old, next)
	}
}

// record tracks terminal failures and granted aliases and logs state
// changes. It reports whether e.aliases changed. e.mu is held.
func (n *Net) record(e *entry, sts []sdk.RelayStatus) (aliasesChanged bool) {
	for _, s := range sts {
		if s.RelayURL == "" {
			continue
		}
		if s.AliasURL != "" && isReady(s) {
			if u := displayURL(s); e.aliases[s.RelayURL] != u {
				e.aliases[s.RelayURL] = u
				aliasesChanged = true
			}
		}
		permanent := s.State == sdk.RelayFailed && (s.Failure == sdk.RelayFailureTerminal || s.Failure == sdk.RelayFailureMITM)
		switch {
		case permanent:
			e.sticky[s.RelayURL] = s
		case !s.Deselected:
			delete(e.sticky, s.RelayURL)
		}
		state := string(s.State)
		lost := lostAlias(e, s)
		switch {
		case s.Deselected:
			state = "deselected"
		case lost != "":
			state += " without " + lost
		}
		if e.logged[s.RelayURL] == state {
			continue
		}
		e.logged[s.RelayURL] = state
		switch {
		case s.Deselected:
			if s.PublicURL != "" {
				n.logf("portal: %s: no longer served at %s", e.slug, displayURL(s))
			}
		case s.State == sdk.RelayReady:
			n.logf("portal: %s: ready at %s", e.slug, displayURL(s))
			if lost != "" {
				n.logf("portal: %s: %s is no longer granted to this flat; %s", e.slug, lost, aliasLostHint)
			}
		case s.State == sdk.RelayFailed:
			n.logf("portal: %s: %s", e.slug, describeFailure(s))
		}
	}
	return aliasesChanged
}

// Alias memory. A relay grants the friendly <slug>.<relay host> alias only
// while no other Portal identity holds it, and the SDK falls back to the
// canonical hostname without an error when another identity took it, for
// example while this flat was offline. A link someone was given then opens
// that other service. Flats therefore remembers the alias it last showed
// for each relay in <Dir>/<slug>.alias and reports when a ready relay no
// longer grants it. The warning stays while that relay lacks the alias;
// deleting the file clears it.

// aliasLostHint explains a lost alias.
const aliasLostHint = "another Portal identity may hold that name now, so links to it can reach another service; share the current URL instead"

// aliasPath returns <Dir>/<slug>.alias, or "" when slug is not a plain file
// name (Serve already rejects such slugs through identityPath).
func (n *Net) aliasPath(slug string) string {
	if slug == "" || strings.Contains(slug, "..") || strings.ContainsAny(slug, `/\`) {
		return ""
	}
	return filepath.Join(n.cfg.Dir, slug+".alias")
}

// loadAliases returns slug's remembered aliases (relayURL -> alias URL).
func (n *Net) loadAliases(slug string) map[string]string {
	var aliases map[string]string
	path := n.aliasPath(slug)
	if path == "" {
		return map[string]string{}
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		n.logf("portal: %s: read aliases: %v", slug, err)
	default:
		if err := json.Unmarshal(b, &aliases); err != nil {
			n.logf("portal: %s: ignore unreadable alias file: %v", slug, err)
			aliases = nil
		}
	}
	if aliases == nil { // also a file holding JSON null
		aliases = map[string]string{}
	}
	return aliases
}

func (n *Net) saveAliases(slug string, aliases map[string]string) {
	path := n.aliasPath(slug)
	if path == "" {
		return
	}
	b, err := json.Marshal(aliases)
	if err == nil {
		err = writeFileAtomic(path, append(b, '\n'))
	}
	if err != nil {
		n.logf("portal: %s: save aliases: %v", slug, err)
	}
}

// lostAlias returns the alias Flats last showed for s's relay when s is
// ready without it, else "". e.mu is held.
func lostAlias(e *entry, s sdk.RelayStatus) string {
	if !isReady(s) || s.AliasURL != "" {
		return ""
	}
	return e.aliases[s.RelayURL]
}

func isReady(s sdk.RelayStatus) bool {
	return s.State == sdk.RelayReady && !s.Deselected && s.PublicURL != ""
}

// Relay pinning. Without explicit relays, every relay comes from discovery,
// which swaps relays freely (on a real run it replaced both relays of a
// flat within two minutes), so a public URL handed to someone stopped
// working. The first discovered relay that becomes ready is therefore
// pinned: added as an explicit relay, which discovery never drops, and
// saved to <Dir>/<slug>.relay so the URL survives restarts too. Discovery
// still adds other relays next to it. The pin moves only when the pinned
// relay fails permanently (for example it rejects the name) or has not
// been ready for repinAfter while another relay is.
var repinAfter = 24 * time.Hour

func (n *Net) pinPath(slug string) string {
	return filepath.Join(n.cfg.Dir, slug+".relay")
}

// loadPin returns slug's saved pinned relay, or "".
func (n *Net) loadPin(slug string) string {
	if len(n.relays) > 0 {
		return "" // explicit relays already keep the URL stable
	}
	b, err := os.ReadFile(n.pinPath(slug))
	if err != nil {
		return ""
	}
	u, err := utils.NormalizeRelayURL(strings.TrimSpace(string(b)))
	if err != nil {
		return ""
	}
	return u
}

// pinChange decides whether e's pinned relay should change and returns the
// old and new pin (next == "" means no change). e.mu is held.
func (n *Net) pinChange(e *entry, sts []sdk.RelayStatus, now time.Time) (old, next string) {
	if len(n.relays) > 0 {
		return "", ""
	}
	var candidate string
	pinnedReady := false
	for _, s := range sts {
		if s.RelayURL == e.pinned {
			pinnedReady = isReady(s)
		} else if candidate == "" && isReady(s) {
			candidate = s.RelayURL
		}
	}
	if e.pinned != "" {
		if pinnedReady {
			e.downAt = time.Time{}
			return "", ""
		}
		if e.downAt.IsZero() {
			e.downAt = now
		}
		_, permanent := e.sticky[e.pinned]
		if !permanent && now.Sub(e.downAt) < repinAfter {
			return "", ""
		}
	}
	if candidate == "" {
		return "", ""
	}
	return e.pinned, candidate
}

// repin makes next the explicit pinned relay of e and drops old. It calls
// into the SDK, so e.mu must not be held.
func (n *Net) repin(e *entry, old, next string) {
	if err := e.exp.AddRelay(next); err != nil {
		n.logf("portal: %s: pin relay %s: %v", e.slug, next, err)
		return
	}
	e.mu.Lock()
	e.pinned, e.downAt, e.primary = next, time.Time{}, next
	e.mu.Unlock()
	if err := writeFileAtomic(n.pinPath(e.slug), []byte(next+"\n")); err != nil {
		n.logf("portal: %s: save pinned relay: %v", e.slug, err)
	}
	if old == "" {
		n.logf("portal: %s: keeping %s as its public relay", e.slug, next)
		return
	}
	if err := e.exp.RemoveRelay(old); err != nil {
		n.logf("portal: %s: unpin relay %s: %v", e.slug, old, err)
	}
	n.logf("portal: %s: moved its public address from %s to %s (old relay unavailable)", e.slug, old, next)
}

// writeFileAtomic writes a 0600 file through a temporary file and rename.
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
	stopping := e.stopping
	merged := maps.Clone(e.sticky)
	var lost []string
	for _, s := range live {
		if a := lostAlias(e, s); a != "" {
			lost = append(lost, a)
		}
	}
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
	all := n.ordered(slices.Collect(maps.Values(merged)), "", "")

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
	if len(lost) > 0 {
		slices.Sort(lost)
		parts = append(parts, strings.Join(lost, ", ")+" no longer granted: "+aliasLostHint)
	}
	if hidden {
		parts = append(parts, "unlisted")
	}
	if stopping {
		hi.State = "error"
		parts = append(parts, "teardown pending confirmation")
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
