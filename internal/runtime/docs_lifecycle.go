package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/core"
)

// docsInstance retains deployment ownership while its physical child sleeps.
// Each spawn has a fresh socket and transport, so an old lease cannot reach a
// replacement child even if its request races a process crash and recovery.
type docsInstance struct {
	m              *Manager
	spec           core.RuntimeSpec
	mu             sync.Mutex
	child          *instance
	ready          bool
	retired        bool
	stopped        bool
	leases         int
	lastForeground time.Time
	childStarted   time.Time
	transition     chan struct{}
	cancel         context.CancelFunc
	quiescing      bool
	quiescence     *docsQuiescence
	clean          bool
	cleanEpoch     uint64
	failures       int
	retryAt        time.Time
	idleRetryAt    time.Time
	idleFailures   int
}

var _ core.ManagedDocsInstance = (*docsInstance)(nil)

func (m *Manager) startDocs(ctx context.Context, spec core.RuntimeSpec) (core.Instance, error) {
	d := &docsInstance{m: m, spec: spec}
	if !spec.Lazy {
		child, err := d.spawn(ctx)
		if err != nil {
			return nil, err
		}
		d.child = child // deliberately unready until committed core preparation
	}
	return d, nil
}

func (d *docsInstance) spawn(ctx context.Context) (*instance, error) {
	spec := d.spec
	spec.ManagedDocs, spec.Lazy = false, false
	if spec.NextGeneration != nil {
		generation, err := spec.NextGeneration(ctx)
		if err != nil {
			return nil, err
		}
		spec.Generation = generation
	}
	return d.m.startChild(ctx, spec)
}

func (d *docsInstance) now() time.Time {
	if d.spec.Now != nil {
		return d.spec.Now()
	}
	return time.Now()
}

// childAlive is called with the logical controller locked. Child code never
// takes that lock, and its process-exit callbacks run outside its own mutex.
func childAlive(child *instance) bool {
	if child == nil {
		return false
	}
	child.mu.Lock()
	defer child.mu.Unlock()
	if child.p == nil || child.stopped {
		return false
	}
	select {
	case <-child.p.done:
		return false
	default:
		return true
	}
}

type docsLease struct {
	owner      *docsInstance
	child      *instance
	foreground bool
	once       sync.Once
}

func (l *docsLease) ServeHTTP(w http.ResponseWriter, r *http.Request) { l.child.serveChild(w, r) }
func (l *docsLease) Release() {
	l.once.Do(func() {
		d := l.owner
		now := d.now()
		d.mu.Lock()
		d.leases--
		if l.foreground {
			d.lastForeground = now
			d.idleFailures = 0
			d.idleRetryAt = time.Time{}
		}
		d.mu.Unlock()
	})
}

func (d *docsInstance) leaseLocked(foreground bool) core.RuntimeLease {
	d.leases++
	return &docsLease{owner: d, child: d.child, foreground: foreground}
}

func (d *docsInstance) TryAcquire(foreground bool) (core.RuntimeLease, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped || d.retired || !d.ready || d.transition != nil || !childAlive(d.child) {
		return nil, false
	}
	return d.leaseLocked(foreground), true
}

// ServeHTTP permits already-ready requests only. Recovery must pass through
// core's storage-locked Wake preparation, never the generic child ensure path.
func (d *docsInstance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	lease, ok := d.TryAcquire(true)
	if !ok {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Service Unavailable: document worker is not ready", http.StatusServiceUnavailable)
		return
	}
	defer lease.Release()
	lease.ServeHTTP(w, r)
}

func (d *docsInstance) Wake(ctx context.Context, prepare func(context.Context, http.Handler) error) (core.RuntimeLease, error) {
	waitDeadline := time.Now().Add(4 * time.Second)
	for {
		d.mu.Lock()
		if d.stopped || d.retired {
			d.mu.Unlock()
			return nil, errStopped
		}
		if d.transition != nil {
			done := d.transition
			quiescing := d.quiescing
			d.mu.Unlock()
			waitCtx := ctx
			cancel := func() {}
			if quiescing {
				waitCtx, cancel = context.WithDeadline(ctx, waitDeadline)
			}
			select {
			case <-done:
				cancel()
				continue
			case <-waitCtx.Done():
				err := waitCtx.Err()
				cancel()
				return nil, err
			}
		}
		if d.ready && childAlive(d.child) {
			lease := d.leaseLocked(true)
			d.mu.Unlock()
			return lease, nil
		}
		if d.ready { // The previously published child has exited.
			d.ready = false
			d.clean = false
			if time.Since(d.childStarted) > time.Minute {
				d.failures = 0
			}
			d.failures++
			d.retryAt = time.Now().Add(backoff(d.failures - 1))
		}
		if wait := time.Until(d.retryAt); wait > 0 {
			d.mu.Unlock()
			if wait > maxRestartWait {
				return nil, fmt.Errorf("document worker retry in %s", wait.Round(time.Millisecond))
			}
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
				continue
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			}
		}
		if err := ctx.Err(); err != nil {
			d.mu.Unlock()
			return nil, err
		}
		done := make(chan struct{})
		d.transition = done
		d.ready = false
		d.clean = false
		child := d.child
		timeout := d.m.StartTimeout
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		// Preparation belongs to the shared deployment, not one visitor. Stop and
		// Retire cancel it; its timeout bounds the entire start/readiness operation.
		workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		d.cancel = cancel
		d.mu.Unlock()

		var err error
		if !childAlive(child) {
			if child != nil {
				child.Stop()
			}
			child, err = d.spawn(workCtx)
		}
		if err == nil {
			if prepare == nil {
				err = errors.New("document preparation is required")
			} else {
				err = d.prepare(workCtx, prepare, child, done, cancel)
			}
		}
		now := d.now()
		d.mu.Lock()
		d.child = child
		if err == nil && (d.stopped || d.retired || workCtx.Err() != nil || !childAlive(child)) {
			err = errors.New("document worker lost during preparation")
		}
		cancel()
		d.cancel = nil
		if err != nil {
			d.failures++
			d.retryAt = time.Now().Add(backoff(d.failures - 1))
			// Transfer the sole termination responsibility to the transition owner.
			// Neither a new Wake nor Stop can terminate/spawn over it.
			d.quiescing = true
			epoch := d.cleanEpoch
			d.mu.Unlock()
			go d.finishStop(child, done, false, epoch)
			d.log("error", fmt.Sprintf("document worker preparation failed: %v", err))
			return nil, err
		}
		d.ready = true
		d.lastForeground = now
		child.mu.Lock()
		// Physical uptime, unaffected by visitor releases or the idle clock.
		if child.p != nil {
			d.childStarted = child.p.started
		}
		child.mu.Unlock()
		lease := d.leaseLocked(true)
		d.transition = nil
		close(done)
		d.mu.Unlock()
		return lease, nil
	}
}

// prepare transfers process cleanup before propagating a callback panic. The
// outer HTTP recovery must not leave Stop blocked on an orphaned transition.
func (d *docsInstance) prepare(ctx context.Context, callback func(context.Context, http.Handler) error, child *instance, done chan struct{}, cancel context.CancelFunc) (err error) {
	defer func() {
		if value := recover(); value != nil {
			cancel()
			d.mu.Lock()
			d.child = child
			d.cancel = nil
			d.quiescing = true
			d.failures++
			d.retryAt = time.Now().Add(backoff(d.failures - 1))
			epoch := d.cleanEpoch
			d.mu.Unlock()
			go d.finishStop(child, done, false, epoch)
			panic(value)
		}
	}()
	return callback(ctx, http.HandlerFunc(child.serveChild))
}

func (d *docsInstance) log(level, msg string) {
	if d.spec.Log != nil {
		d.spec.Log(level, msg)
	} else {
		d.m.logf("flat %s v%d: %s: %s", d.spec.Flat, d.spec.Version, level, msg)
	}
}

type docsQuiescence struct {
	owner *docsInstance
	child *instance
	done  chan struct{}
	epoch uint64
	once  sync.Once
}

func (q *docsQuiescence) ServeHTTP(w http.ResponseWriter, r *http.Request) { q.child.serveChild(w, r) }

func (d *docsInstance) TryQuiesce(now time.Time, idle time.Duration) (core.RuntimeQuiescence, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped || d.retired || !d.ready || d.transition != nil || d.leases != 0 || now.Sub(d.lastForeground) < idle || now.Before(d.idleRetryAt) || !childAlive(d.child) {
		return nil, false
	}
	d.ready = false
	d.quiescing = true
	d.transition = make(chan struct{})
	q := &docsQuiescence{owner: d, child: d.child, done: d.transition, epoch: d.cleanEpoch}
	d.quiescence = q
	return q, true
}

func (q *docsQuiescence) Suspend(clean bool) {
	q.once.Do(func() {
		d := q.owner
		d.mu.Lock()
		valid := d.transition == q.done && d.child == q.child
		d.mu.Unlock()
		if valid {
			go d.finishStop(q.child, q.done, clean, q.epoch)
		}
	})
}

func (q *docsQuiescence) Resume() error {
	result := errors.New("document quiescence already consumed")
	q.once.Do(func() {
		d := q.owner
		now := d.now()
		d.mu.Lock()
		if d.transition != q.done || d.child != q.child {
			d.mu.Unlock()
			result = errStopped
			return
		}
		if d.stopped || d.retired || !childAlive(q.child) {
			d.mu.Unlock()
			go d.finishStop(q.child, q.done, false, q.epoch)
			result = errStopped
			return
		}
		d.ready = true
		// Failed drains leave the child available for ordinary background
		// refresh. Retry the expensive admission-closing drain less often.
		d.idleFailures++
		delay := min(30*time.Second<<min(d.idleFailures-1, 4), 5*time.Minute)
		d.idleRetryAt = now.Add(delay)
		d.quiescing = false
		d.quiescence = nil
		d.transition = nil
		close(q.done)
		d.mu.Unlock()
		result = nil
	})
	return result
}

// finishStop owns physical termination until confirmed exit. No clock timeout
// grants permission to release ownership or start a second child.
func (d *docsInstance) finishStop(child *instance, done chan struct{}, clean bool, epoch uint64) {
	if child != nil {
		child.Stop()
	}
	d.mu.Lock()
	if d.transition == done {
		d.child = nil
		d.ready = false
		d.clean = clean && epoch == d.cleanEpoch && !d.stopped && !d.retired
		d.quiescing = false
		d.quiescence = nil
		d.transition = nil
		close(done)
	}
	d.mu.Unlock()
}

// Quiescing is an admission snapshot, used to bound foreground lock waits.
func (d *docsInstance) Quiescing() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.quiescing && d.transition != nil
}

func (d *docsInstance) CleanDormant() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.clean && d.child == nil && d.transition == nil && !d.stopped && !d.retired
}
func (d *docsInstance) InvalidateClean() {
	d.mu.Lock()
	d.clean = false
	d.cleanEpoch++
	d.mu.Unlock()
}
func (d *docsInstance) Retire() {
	d.mu.Lock()
	d.retired = true
	d.clean = false
	d.cleanEpoch++
	cancel := d.cancel
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (d *docsInstance) Stop() {
	d.mu.Lock()
	d.stopped = true
	d.ready = false
	d.clean = false
	d.cleanEpoch++
	cancel := d.cancel
	done := d.transition
	q := d.quiescence
	if done == nil {
		done = make(chan struct{})
		d.transition = done
		d.quiescing = true
		child := d.child
		epoch := d.cleanEpoch
		d.mu.Unlock()
		go d.finishStop(child, done, false, epoch)
	} else {
		d.mu.Unlock()
	}
	if cancel != nil {
		cancel()
	}
	if q != nil {
		q.Suspend(false)
	}
	<-done
}
