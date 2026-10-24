package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// RuntimeLease pins one child. Dispatch never restarts a lost child.
type RuntimeLease interface {
	http.Handler
	Release()
}

// RuntimeQuiescence reserves an idle child until Suspend or Resume consumes it.
type RuntimeQuiescence interface {
	http.Handler
	Suspend(clean bool)
	Resume() error
}

// ManagedDocsInstance preserves a logical document while its child sleeps.
type ManagedDocsInstance interface {
	Instance
	TryAcquire(foreground bool) (RuntimeLease, bool)
	Wake(context.Context, func(context.Context, http.Handler) error) (RuntimeLease, error)
	TryQuiesce(time.Time, time.Duration) (RuntimeQuiescence, bool)
	CleanDormant() bool
	InvalidateClean()
	Retire()
	Quiescing() bool
}

type managedDocsRuntime struct {
	*runtimeInstance
	control ManagedDocsInstance
}

func (i *managedDocsRuntime) TryAcquire(f bool) (RuntimeLease, bool) { return i.control.TryAcquire(f) }
func (i *managedDocsRuntime) Wake(c context.Context, p func(context.Context, http.Handler) error) (RuntimeLease, error) {
	return i.control.Wake(c, p)
}
func (i *managedDocsRuntime) TryQuiesce(n time.Time, d time.Duration) (RuntimeQuiescence, bool) {
	return i.control.TryQuiesce(n, d)
}
func (i *managedDocsRuntime) CleanDormant() bool { return i.control.CleanDormant() }
func (i *managedDocsRuntime) InvalidateClean()   { i.control.InvalidateClean() }
func (i *managedDocsRuntime) Retire()            { i.control.Retire() }
func (i *managedDocsRuntime) Quiescing() bool    { return i.control.Quiescing() }

const docsIdleTimeout = 2 * time.Minute

type docsDispatchKey struct{}
type docsDispatch struct {
	instance Instance
	handler  http.Handler
}
type lazyDocsKey struct{}

// An operation-scoped receipt survives preparation importing/acknowledging its
// rollback before the caller asks the new child for the now-deleted receipt.
type docsOpCaptureKey struct{}
type docsOpCapture struct {
	mu       sync.Mutex
	op       string
	revision int
}

func (c *docsOpCapture) recorded() int { c.mu.Lock(); defer c.mu.Unlock(); return c.revision }
func captureDocsImport(ctx context.Context, res docsImport) {
	if c, ok := ctx.Value(docsOpCaptureKey{}).(*docsOpCapture); ok {
		if n := res.byOp[c.op]; n > 0 {
			c.mu.Lock()
			c.revision = n
			c.mu.Unlock()
		}
	}
}

func withDocsDispatch(ctx context.Context, d *deployed, h http.Handler) context.Context {
	return context.WithValue(ctx, docsDispatchKey{}, docsDispatch{d.inst, h})
}
func docsDirect(ctx context.Context, d *deployed) http.Handler {
	if x, ok := ctx.Value(docsDispatchKey{}).(docsDispatch); ok && x.instance == d.inst {
		return x.handler
	}
	return nil
}
func docsManaged(d *deployed) ManagedDocsInstance {
	if d == nil {
		return nil
	}
	m, _ := d.inst.(ManagedDocsInstance)
	return m
}

// acquireDocsLocked is called only with the storage lock held. The returned
// context carries a direct child, so nested host operations never reacquire it.
func (s *Service) acquireDocsLocked(ctx context.Context, slug string, d *deployed) (context.Context, func(), error) {
	noop := func() {}
	if d == nil {
		return ctx, noop, errNotRunningDocs
	}
	if docsDirect(ctx, d) != nil {
		return ctx, noop, nil
	}
	m := docsManaged(d)
	if m == nil {
		return ctx, noop, nil
	}
	if lease, ok := m.TryAcquire(true); ok {
		return withDocsDispatch(ctx, d, lease), lease.Release, nil
	}
	select {
	case <-s.stop:
		return ctx, noop, ErrUnavailable
	default:
	}
	lease, err := m.Wake(ctx, func(prep context.Context, h http.Handler) error {
		return s.prepareDocsLocked(withDocsDispatch(prep, d, h), slug, d)
	})
	if err != nil {
		// Manager logs actual start/preparation failures through the rate-limited
		// callback. Refused backoff/stopped requests must not add duplicate events.
		if ctx.Err() != nil {
			err = ctx.Err()
		} else {
			err = fmt.Errorf("%w: %s", ErrUnavailable, d.redact(err.Error()))
		}
		return ctx, noop, err
	}
	return withDocsDispatch(ctx, d, lease), lease.Release, nil
}

// currentDeployment never creates state for a deleted or renamed route.
func (s *Service) currentDeployment(slug string) *deployed {
	s.mu.Lock()
	lf := s.live[slug]
	s.mu.Unlock()
	if lf == nil {
		return nil
	}
	return lf.cur.Load()
}

type docsRedispatchKey struct{}

// docsWakeLock bounds quiescence admission from request entry, including the
// storage lock held by the drain. Cold-start waiters keep their caller deadline.
func (s *Service) docsWakeLock(ctx context.Context, slug string, m ManagedDocsInstance, deadline time.Time) (func(), bool, error) {
	value, _ := s.locks.LoadOrStore(slug, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	quiescing := false
	timer := time.NewTicker(2 * time.Millisecond)
	defer timer.Stop()
	for {
		quiescing = quiescing || m.Quiescing()
		if err := ctx.Err(); err != nil {
			return nil, quiescing, err
		}
		if quiescing && !time.Now().Before(deadline) {
			return nil, true, context.DeadlineExceeded
		}
		if mu.TryLock() {
			if err := s.checkStorageJournal(slug); err != nil {
				mu.Unlock()
				return nil, quiescing, err
			}
			return mu.Unlock, quiescing, nil
		}
		select {
		case <-ctx.Done():
			return nil, quiescing, ctx.Err()
		case <-s.stop:
			return nil, quiescing, ErrUnavailable
		case <-timer.C:
		}
	}
}

// docsDemand wraps live docs only. Ready traffic avoids storage locking and
// redispatch preserves all wrappers of the new version, at most once.
func (s *Service) docsDemand(slug string, d *deployed) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline := time.Now().Add(4 * time.Second)
		if h := docsDirect(r.Context(), d); h != nil {
			h.ServeHTTP(w, r)
			return
		}
		m := docsManaged(d)
		if m == nil {
			d.inst.ServeHTTP(w, r)
			return
		}
		if lease, ok := m.TryAcquire(true); ok {
			defer lease.Release()
			lease.ServeHTTP(w, r)
			return
		}
		unlock, quiescing, err := s.docsWakeLock(r.Context(), slug, m, deadline)
		if err != nil {
			docsWakeError(w)
			return
		}
		var next *deployed
		var acquired context.Context
		release := func() {}
		func() {
			defer unlock()
			next = s.currentDeployment(slug)
			if next != d {
				return
			}
			waitCtx := r.Context()
			cancel := func() {}
			if quiescing || m.Quiescing() {
				waitCtx, cancel = context.WithDeadline(waitCtx, deadline)
			}
			defer cancel()
			acquired, release, err = s.acquireDocsLocked(waitCtx, slug, d)
		}()
		if next != d {
			if retried, _ := r.Context().Value(docsRedispatchKey{}).(bool); !retried && next != nil {
				next.handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), docsRedispatchKey{}, true)))
			} else {
				docsWakeError(w)
			}
			return
		}
		if err != nil {
			docsWakeError(w)
			return
		}
		defer release()
		if h := docsDirect(acquired, d); h != nil {
			h.ServeHTTP(w, r)
		} else {
			d.inst.ServeHTTP(w, r)
		}
	})
}
func docsWakeError(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	http.Error(w, "document temporarily unavailable; retry", http.StatusServiceUnavailable)
}

// withRunningDocs never wakes and does not update foreground activity.
func withRunningDocs(ctx context.Context, d *deployed) (context.Context, func(), bool) {
	noop := func() {}
	if d == nil {
		return ctx, noop, false
	}
	if docsDirect(ctx, d) != nil {
		return ctx, noop, true
	}
	if m := docsManaged(d); m != nil {
		l, ok := m.TryAcquire(false)
		if !ok {
			return ctx, noop, false
		}
		return withDocsDispatch(ctx, d, l), l.Release, true
	}
	return ctx, noop, true
}

// prepareDocsLocked preserves reads/recovery on import or capacity failures.
// A revoked budget blocks writes until an explicit installation succeeds.
// The stored public access level and its epoch reach the worker before any
// request does (docs-public-edit-TECH 2.4).
func (s *Service) prepareDocsLocked(ctx context.Context, slug string, d *deployed) error {
	if err := s.docsHost(ctx, slug, http.MethodPost, "budget/revoke", map[string]any{}, nil); err != nil {
		return err
	}
	s.pushDocsAccessLocked(ctx, slug, d)
	syncResult, syncErr := s.syncDocsLocked(ctx, slug)
	if syncErr != nil && !errors.Is(syncErr, ErrDocumentCapacity) {
		return syncErr
	}
	res, err := s.importDocsLocked(ctx, slug)
	captureDocsImport(ctx, res)
	if err != nil {
		s.Event(ctx, slug, "warn", "revision", "document revision import deferred: "+d.redact(err.Error()), nil)
	}
	if err == nil && syncErr == nil {
		s.settleHoldsLocked(ctx, slug, intentID(syncResult))
	}
	if berr := s.pushDocsBudgetStoredLocked(ctx, slug, res.stored); berr != nil {
		// A partial import may have installed a budget in its acknowledgement.
		if revokeErr := s.docsHost(ctx, slug, http.MethodPost, "budget/revoke", map[string]any{}, nil); revokeErr != nil {
			return revokeErr
		}
		s.Event(ctx, slug, "warn", "quota", "document budget unavailable; edits remain disabled: "+d.redact(berr.Error()), nil)
	}
	return nil
}

// pushDocsAccessLocked tells a waking worker the flat's stored public
// access level and access_seq. A worker that misses it keeps its older pair,
// which matches no request's headers, so guests cannot join until a later
// wake or level change succeeds.
func (s *Service) pushDocsAccessLocked(ctx context.Context, slug string, d *deployed) {
	f, err := s.st.GetFlat(ctx, slug)
	if err == nil {
		level := ""
		if f.Visibility.Public() {
			level = f.PublicAccess
		}
		err = docsTarget{s: s, slug: slug}.SetPublicAccess(ctx, level, f.AccessSeq)
	}
	if err != nil {
		s.Event(ctx, slug, "warn", "visibility", "the document did not take its public access level yet: "+d.redact(err.Error()), nil)
	}
}

// idleDocsLocked reserves admission before draining; physical exit happens
// asynchronously after this method releases the flat lock.
func (s *Service) idleDocsLocked(ctx context.Context, slug string, d *deployed) bool {
	m := docsManaged(d)
	if m == nil {
		return false
	}
	q, ok := m.TryQuiesce(s.now(), docsIdleTimeout)
	if !ok {
		return false
	}
	ctx = withDocsDispatch(ctx, d, q)
	clean := false
	suspend := func(clean bool) {
		// A sleeping document has no public presence.
		s.state(slug).docsPresence.Store(nil)
		q.Suspend(clean)
	}
	defer func() {
		defer func() {
			if failure := recover(); failure != nil {
				suspend(false)
				panic(failure)
			}
		}()
		if clean {
			suspend(true)
			return
		}
		if err := s.prepareDocsLocked(ctx, slug, d); err != nil {
			suspend(false)
			return
		}
		if err := q.Resume(); err != nil {
			suspend(false)
		}
	}()
	synced, err := s.syncDocsLocked(ctx, slug)
	if err != nil {
		return true
	}
	res, err := s.importDocsLocked(ctx, slug)
	if err != nil || res.more {
		return true
	}
	s.settleHoldsLocked(ctx, slug, intentID(synced))
	proof, err := s.syncDocsLocked(ctx, slug)
	if err != nil || !proof.Quiescent || proof.Ready != 0 {
		return true
	}
	holds, err := s.st.Holds(ctx, slug)
	if err != nil || len(holds) != 0 {
		return true
	}
	clean = true
	return true
}

// Compile-time forwarding guard: Stop still owns permanent directory cleanup.
var _ ManagedDocsInstance = (*managedDocsRuntime)(nil)
