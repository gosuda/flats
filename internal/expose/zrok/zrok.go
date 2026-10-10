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
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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
	// Dir records the names this host reserved (one file per name), so a
	// deleted flat releases its name even after its zrok permission is gone.
	Dir string
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

	retryMin, retryMax time.Duration // bind retry backoff

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

	mu      sync.Mutex
	state   string
	detail  string
	ln      net.Listener
	servers map[*http.Server]struct{} // every server that may still hold connections
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
	if cfg.Dir == "" {
		return nil, errors.New("zrok: name directory is required")
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("zrok: name directory: %w", err)
	}
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
	return &Net{cfg: cfg, b: b, logf: logf, retryMin: 2 * time.Second, retryMax: time.Minute,
		entries: map[string]*entry{}, locks: map[string]*sync.Mutex{}}
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

	ns := n.cfg.Namespace
	rec, known, err := n.readRecord(slug)
	if err != nil {
		return "", fmt.Errorf("zrok: %s: %w", slug, err)
	}
	if known && rec.Namespace != ns {
		// zrok.namespace changed: release what this host holds in the old
		// namespace before reserving in the new one.
		if err := n.release(ctx, slug, rec); err != nil {
			return "", fmt.Errorf("zrok: %s: release the name in namespace %q first: %w", slug, rec.Namespace, err)
		}
		known = false
	}
	if !known {
		rec = record{Namespace: ns}
	}
	// Record the name before reserving it, so a reservation is never
	// untracked.
	if err := n.writeRecord(slug, rec); err != nil {
		return "", fmt.Errorf("zrok: %s: record name: %w", slug, err)
	}
	holder, created, err := n.b.ReserveName(ctx, ns, slug)
	if err != nil {
		if errors.Is(err, errNameTaken) {
			if !known {
				_ = n.dropRecord(slug)
			}
			return "", fmt.Errorf("zrok: %s: %s", slug, TakenHint)
		}
		return "", fmt.Errorf("zrok: %s: %w", slug, err)
	}
	if created {
		rec.Created = true
		if err := n.writeRecord(slug, rec); err != nil {
			return "", fmt.Errorf("zrok: %s: record name: %w", slug, err)
		}
	}
	if holder != "" {
		// Only a share this host created and could not delete (a crash, or
		// a shutdown whose unshare failed) is removed. Any other share, such
		// as one the operator runs under this name, is left alone.
		if holder != rec.Token {
			return "", fmt.Errorf("zrok: %s: the name is held by share %s, which Flats did not create; stop that share or rename the flat", slug, holder)
		}
		if err := n.b.Unshare(ctx, holder); err != nil {
			return "", fmt.Errorf("zrok: %s: remove stale share %s: %w", slug, holder, err)
		}
		n.logf("zrok: %s: removed stale share %s", slug, holder)
		rec.Token = ""
		_ = n.writeRecord(slug, rec)
	}
	token, endpoints, err := n.b.Share(ctx, ns, slug)
	if err != nil {
		return "", fmt.Errorf("zrok: %w", err)
	}
	rec.Token = token
	if err := n.writeRecord(slug, rec); err != nil {
		// An unrecorded share could never be cleaned up after a crash.
		_ = n.b.Unshare(ctx, token)
		return "", fmt.Errorf("zrok: %s: record share: %w", slug, err)
	}
	url := ""
	if len(endpoints) > 0 {
		url = absoluteURL(endpoints[0])
	}
	runCtx, cancel := context.WithCancel(context.Background())
	e = &entry{slug: slug, token: token, url: url, handler: &handlerBox{h: h},
		cancel: cancel, done: make(chan struct{}), state: stateStarting, servers: map[*http.Server]struct{}{}}
	n.mu.Lock()
	n.entries[slug] = e
	n.mu.Unlock()
	go n.run(runCtx, e)
	return url, nil
}

// run binds the share and serves it until Stop cancels ctx. A failed bind,
// or a listener the overlay closes, is retried with backoff, so a route that
// failed recovers without a new approval.
func (n *Net) run(ctx context.Context, e *entry) {
	defer close(e.done)
	delay := n.retryMin
	for {
		err := n.bindAndServe(ctx, e)
		if ctx.Err() != nil {
			return
		}
		e.mu.Lock()
		e.state, e.detail = stateError, err.Error()+fmt.Sprintf("; retrying in %s", delay)
		e.mu.Unlock()
		n.logf("zrok: %s: %v; retrying in %s", e.slug, err, delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(2*delay, n.retryMax)
	}
}

// bindAndServe binds the share once and serves it until the listener stops.
// It returns why serving ended; after Stop the error is irrelevant.
func (n *Net) bindAndServe(ctx context.Context, e *entry) error {
	ln, err := n.b.Listen(e.token)
	if err != nil {
		return fmt.Errorf("bind the share on the zrok overlay: %w", err)
	}
	srv := &http.Server{Handler: publicHandler(e.handler), ReadHeaderTimeout: 30 * time.Second}
	e.mu.Lock()
	if ctx.Err() != nil {
		e.mu.Unlock()
		ln.Close()
		return ctx.Err()
	}
	e.ln = ln
	e.servers[srv] = struct{}{}
	e.state, e.detail = stateReady, ""
	e.mu.Unlock()
	n.logf("zrok: %s: ready at %s", e.slug, e.url)
	err = srv.Serve(ln)
	ln.Close()
	// Serve returns when the listener closes, while requests already
	// accepted keep running. Drain them before binding again; Stop drains
	// every server still in the set.
	dctx, cancel := context.WithTimeout(ctx, n.cfg.ShutdownTimeout)
	derr := srv.Shutdown(dctx)
	cancel()
	e.mu.Lock()
	if e.ln == ln {
		e.ln = nil
	}
	if derr == nil {
		delete(e.servers, srv)
	}
	e.mu.Unlock()
	if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		err = errors.New("the zrok overlay closed the share's listener")
	}
	return fmt.Errorf("zrok share stopped serving: %w", err)
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
	ln := e.ln
	servers := make([]*http.Server, 0, len(e.servers))
	for srv := range e.servers {
		servers = append(servers, srv)
	}
	e.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("zrok: %s HTTP shutdown: %w", e.slug, err))
			srv.Close()
			continue
		}
		e.mu.Lock()
		delete(e.servers, srv)
		e.mu.Unlock()
	}
	select {
	case <-e.done:
	case <-ctx.Done():
		errs = append(errs, fmt.Errorf("zrok: %s overlay binding did not stop within %s", e.slug, n.cfg.ShutdownTimeout))
	}
	// Unshare even if draining failed: the share is what keeps the URL up.
	if err := n.b.Unshare(ctx, e.token); err != nil {
		errs = append(errs, fmt.Errorf("zrok: %s: %w", e.slug, err))
	} else {
		n.forgetToken(e.slug, e.token)
	}
	if len(errs) > 0 {
		e.mu.Lock()
		e.state, e.detail = stateError, errors.Join(errs...).Error()
		e.mu.Unlock()
	}
	return errors.Join(errs...)
}

// Retire stops slug's share and releases its name, for a deleted flat or an
// expired rename redirect. It acts on the name this host recorded, so a name
// kept while the flat was private, or after a restart, is released too. A
// name Flats did not create, or one another share now uses, is left in the
// account; only the record is dropped.
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
	rec, known, err := n.readRecord(slug)
	if err != nil || !known {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), n.cfg.ShutdownTimeout)
	defer cancel()
	if err := n.release(ctx, slug, rec); err != nil {
		return fmt.Errorf("zrok: %s: %w", slug, err)
	}
	return nil
}

// release undoes what rec says this host holds for slug and drops rec.
func (n *Net) release(ctx context.Context, slug string, rec record) error {
	holder, found, err := n.b.NameHolder(ctx, rec.Namespace, slug)
	if err != nil {
		return err
	}
	if found {
		switch {
		case holder != "" && holder == rec.Token:
			if err := n.b.Unshare(ctx, holder); err != nil {
				return fmt.Errorf("remove share %s: %w", holder, err)
			}
			if rec.Created {
				if err := n.b.ReleaseName(ctx, rec.Namespace, slug); err != nil {
					return err
				}
			}
		case holder != "":
			// Another share uses the name now; leave it.
		case rec.Created:
			if err := n.b.ReleaseName(ctx, rec.Namespace, slug); err != nil {
				return err
			}
		}
	}
	return n.dropRecord(slug)
}

// Reserved reports whether this host recorded reserving slug's name and has
// not released it yet.
func (n *Net) Reserved(slug string) bool { return HasRecord(n.cfg.Dir, slug) }

// HasRecord reports whether dir records a zrok name for slug. It reads only
// the local record, for callers without a zrok backend.
func HasRecord(dir, slug string) bool {
	path, err := recordPath(dir, slug)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// record is what this host knows about a name it reserved, kept as JSON in
// <Dir>/<slug>.
type record struct {
	Namespace string `json:"namespace"`
	Created   bool   `json:"created"`         // Flats created the name, so it may release it
	Token     string `json:"token,omitempty"` // a share Flats created and has not deleted yet
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func recordPath(dir, slug string) (string, error) {
	if dir == "" {
		return "", errors.New("no name directory")
	}
	if !nameRE.MatchString(slug) {
		return "", fmt.Errorf("invalid name %q", slug)
	}
	return filepath.Join(dir, slug), nil
}

func (n *Net) readRecord(slug string) (record, bool, error) {
	path, err := recordPath(n.cfg.Dir, slug)
	if err != nil {
		return record{}, false, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return record{}, false, nil
	}
	if err != nil {
		return record{}, false, err
	}
	var rec record
	if err := json.Unmarshal(b, &rec); err != nil || rec.Namespace == "" {
		return record{}, false, fmt.Errorf("unreadable name record %s", path)
	}
	return rec, true, nil
}

func (n *Net) writeRecord(slug string, rec record) error {
	path, err := recordPath(n.cfg.Dir, slug)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(n.cfg.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(n.cfg.Dir, "."+slug+".tmp*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(b, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp.Name(), 0o600)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

func (n *Net) dropRecord(slug string) error {
	path, err := recordPath(n.cfg.Dir, slug)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// forgetToken clears a deleted share from slug's record. A failure only
// leaves a token that no share has any more.
func (n *Net) forgetToken(slug, token string) {
	if rec, ok, err := n.readRecord(slug); err == nil && ok && rec.Token == token {
		rec.Token = ""
		_ = n.writeRecord(slug, rec)
	}
}

// absoluteURL turns a zrok frontend endpoint, which the controller returns
// as a bare host name (<name>.<namespace host>), into an https URL.
func absoluteURL(endpoint string) string {
	if endpoint == "" || strings.Contains(endpoint, "://") {
		return endpoint
	}
	return "https://" + endpoint
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
