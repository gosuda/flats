// Package zrok exposes public flats through zrok public shares
// (github.com/openziti/zrok). Each served slug reserves the name <slug> in
// the configured namespace of the operator's zrok account, so its public
// hostname stays stable across restarts, and binds an ephemeral public
// share under that name while the flat is public.
//
// Flats does not hold the zrok account token: the operator enables a zrok
// environment (`zrok2 enable`) for the user that runs flats, and Flats reads
// that environment.
package zrok

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/core"
)

// DefaultNamespace is the zrok namespace shares use when none is configured.
const DefaultNamespace = "public"

// TakenHint is shown when another zrok account owns the flat's name.
const TakenHint = "another zrok account owns this name in the namespace; rename the flat"

// Config configures a Net.
type Config struct {
	// Environment is the zrok environment directory. "" is the zrok
	// default, ~/.zrok2 of the user that runs flats.
	Environment string
	// Namespace is the zrok namespace for names (default "public").
	Namespace string
	// ShutdownTimeout bounds HTTP drain and unsharing (default 30s).
	ShutdownTimeout time.Duration
	// Logf receives share state changes. Nil discards them.
	Logf func(string, ...any)
}

const (
	stateStarting = "starting"
	stateReady    = "ready"
	stateError    = "error"
)

// Net serves flats as zrok public shares. It satisfies the provider
// manager's zrok backend interface.
type Net struct {
	cfg  Config
	b    backend
	logf func(string, ...any)

	mu      sync.Mutex
	closed  bool
	entries map[string]*entry
	locks   map[string]*sync.Mutex // per-slug Serve/Stop/Retire serialization
	ops     sync.WaitGroup
}

type entry struct {
	slug    string
	token   string
	url     string
	handler *handlerBox
	cancel  context.CancelFunc
	done    chan struct{} // closed when the listener goroutine returned

	mu     sync.Mutex
	state  string
	detail string
	ln     net.Listener
	srv    *http.Server
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

// New reads the zrok environment. It does not contact the zrok controller
// or overlay; that happens on the first Serve.
func New(cfg Config) (*Net, error) {
	cfg = withDefaults(cfg)
	b, err := newSDKBackend(cfg)
	if err != nil {
		return nil, fmt.Errorf("zrok: %w", err)
	}
	return newNet(cfg, b), nil
}

func withDefaults(cfg Config) Config {
	if cfg.Namespace == "" {
		cfg.Namespace = DefaultNamespace
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 30 * time.Second
	}
	return cfg
}

func newNet(cfg Config, b backend) *Net {
	cfg = withDefaults(cfg)
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Net{cfg: cfg, b: b, logf: logf, entries: map[string]*entry{}, locks: map[string]*sync.Mutex{}}
}

func (n *Net) slugLock(slug string) *sync.Mutex {
	n.mu.Lock()
	defer n.mu.Unlock()
	l := n.locks[slug]
	if l == nil {
		l = &sync.Mutex{}
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

// Serve reserves slug's name, creates its public share and starts binding
// it on the overlay. It returns the share's URL without waiting for the
// binding; Status reports starting until it is ready. Serving a slug that
// is already served replaces its handler.
func (n *Net) Serve(ctx context.Context, slug string, h http.Handler) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if h == nil {
		return "", errors.New("zrok: handler is nil")
	}
	if !n.beginOp() {
		return "", errors.New("zrok: closed")
	}
	defer n.ops.Done()
	l := n.slugLock(slug)
	l.Lock()
	defer l.Unlock()

	n.mu.Lock()
	e := n.entries[slug]
	n.mu.Unlock()
	if e != nil {
		e.handler.set(h)
		return e.url, nil
	}

	holder, err := n.b.ReserveName(ctx, slug)
	if err != nil {
		if errors.Is(err, errNameTaken) {
			return "", fmt.Errorf("zrok: %s: %s", slug, TakenHint)
		}
		return "", fmt.Errorf("zrok: %s: %w", slug, err)
	}
	if holder != "" {
		// A share this environment left behind (a crash, or a shutdown that
		// could not unshare) still holds the name. Deleting a share of
		// another environment fails, so this never takes a name over.
		if err := n.b.Unshare(ctx, holder); err != nil {
			return "", fmt.Errorf("zrok: %s: the name is held by share %s, which could not be removed: %w", slug, holder, err)
		}
		n.logf("zrok: %s: removed stale share %s", slug, holder)
	}
	token, endpoints, err := n.b.Share(ctx, slug)
	if err != nil {
		return "", fmt.Errorf("zrok: %w", err)
	}
	url := ""
	if len(endpoints) > 0 {
		url = endpoints[0]
	}
	runCtx, cancel := context.WithCancel(context.Background())
	e = &entry{slug: slug, token: token, url: url, handler: &handlerBox{h: h},
		cancel: cancel, done: make(chan struct{}), state: stateStarting}
	n.mu.Lock()
	n.entries[slug] = e
	n.mu.Unlock()
	go n.run(runCtx, e)
	return url, nil
}

// run binds the share and serves it until Stop cancels ctx.
func (n *Net) run(ctx context.Context, e *entry) {
	defer close(e.done)
	ln, err := n.b.Listen(e.token)
	if err != nil {
		e.mu.Lock()
		e.state, e.detail = stateError, "bind the share on the zrok overlay: "+err.Error()
		e.mu.Unlock()
		n.logf("zrok: %s: %v", e.slug, err)
		return
	}
	srv := &http.Server{Handler: publicHandler(e.handler), ReadHeaderTimeout: 30 * time.Second}
	e.mu.Lock()
	if ctx.Err() != nil {
		e.mu.Unlock()
		ln.Close()
		return
	}
	e.ln, e.srv = ln, srv
	e.state, e.detail = stateReady, ""
	e.mu.Unlock()
	n.logf("zrok: %s: ready at %s", e.slug, e.url)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) && ctx.Err() == nil {
		e.mu.Lock()
		e.state, e.detail = stateError, "zrok share stopped serving: "+err.Error()
		e.mu.Unlock()
		n.logf("zrok: %s: http server stopped: %v", e.slug, err)
	}
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

// Stop shuts slug's share down and deletes it. The name stays reserved so
// a later Serve gets the same URL. Unknown slugs are a no-op. A failure
// leaves the share registered so a later Stop can retry.
func (n *Net) Stop(slug string) error {
	if !n.beginOp() {
		return nil // Close owns remaining entries.
	}
	defer n.ops.Done()
	l := n.slugLock(slug)
	l.Lock()
	defer l.Unlock()
	return n.stop(slug)
}

// stop runs under the slug lock.
func (n *Net) stop(slug string) error {
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

func (n *Net) stopEntry(e *entry) error {
	ctx, cancel := context.WithTimeout(context.Background(), n.cfg.ShutdownTimeout)
	defer cancel()
	var errs []error
	e.cancel()
	e.mu.Lock()
	srv, ln := e.srv, e.ln
	e.mu.Unlock()
	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("zrok: %s HTTP shutdown: %w", e.slug, err))
			srv.Close()
		}
	}
	if ln != nil {
		ln.Close()
	}
	select {
	case <-e.done:
	case <-ctx.Done():
		errs = append(errs, fmt.Errorf("zrok: %s overlay binding did not stop within %s", e.slug, n.cfg.ShutdownTimeout))
	}
	// Unshare even if draining failed: the share is what keeps the URL up.
	if err := n.b.Unshare(ctx, e.token); err != nil {
		errs = append(errs, fmt.Errorf("zrok: %s: %w", e.slug, err))
	}
	if len(errs) > 0 {
		e.mu.Lock()
		e.state, e.detail = stateError, errors.Join(errs...).Error()
		e.mu.Unlock()
	}
	return errors.Join(errs...)
}

// Retire stops slug's share and releases its name, for a deleted flat or an
// expired rename redirect. It releases the name even when no share is
// served, so a name left by an earlier process is not kept forever.
func (n *Net) Retire(slug string) error {
	if !n.beginOp() {
		return errors.New("zrok: closed")
	}
	defer n.ops.Done()
	l := n.slugLock(slug)
	l.Lock()
	defer l.Unlock()
	if err := n.stop(slug); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), n.cfg.ShutdownTimeout)
	defer cancel()
	holder, found, err := n.b.NameHolder(ctx, slug)
	if err != nil {
		return fmt.Errorf("zrok: %s: %w", slug, err)
	}
	if !found {
		return nil
	}
	if holder != "" {
		if err := n.b.Unshare(ctx, holder); err != nil {
			return fmt.Errorf("zrok: %s: remove share %s: %w", slug, holder, err)
		}
	}
	if err := n.b.ReleaseName(ctx, slug); err != nil {
		return fmt.Errorf("zrok: %s: %w", slug, err)
	}
	return nil
}

// URL returns slug's public URL, or "" when it is not served.
func (n *Net) URL(slug string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if e := n.entries[slug]; e != nil {
		return e.url
	}
	return ""
}

// Status reports every served share for the console.
func (n *Net) Status() core.NetStatus {
	n.mu.Lock()
	entries := make([]*entry, 0, len(n.entries))
	for _, e := range n.entries {
		entries = append(entries, e)
	}
	closed := n.closed
	n.mu.Unlock()
	st := core.NetStatus{Kind: "zrok", Enabled: !closed, Detail: "namespace " + n.cfg.Namespace, Hosts: []core.HostInfo{}}
	for _, e := range entries {
		e.mu.Lock()
		st.Hosts = append(st.Hosts, core.HostInfo{Host: e.slug, URL: e.url, State: e.state, Detail: e.detail})
		e.mu.Unlock()
	}
	slices.SortFunc(st.Hosts, func(a, b core.HostInfo) int { return strings.Compare(a.Host, b.Host) })
	return st
}

// Close stops and deletes every share. Names stay reserved, so the next
// process serves the same URLs. Later Serve calls fail.
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
		if err := n.stopEntry(e); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, n.b.Close())
	return errors.Join(errs...)
}
