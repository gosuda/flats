package core

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type idleTestLease struct {
	http.Handler
	released int
}

func (l *idleTestLease) Release() { l.released++ }

type idleTestInstance struct {
	q       RuntimeQuiescence
	retired int
	stop    func()

	http.Handler
	lease            *idleTestLease
	wake             func(context.Context, func(context.Context, http.Handler) error) (RuntimeLease, error)
	foreground       []bool
	wakes            int
	clean, quiescing bool
	stopped          int
}

func (i *idleTestInstance) Stop() {
	i.stopped++
	if i.stop != nil {
		i.stop()
	}
}
func (i *idleTestInstance) TryAcquire(f bool) (RuntimeLease, bool) {
	i.foreground = append(i.foreground, f)
	return i.lease, i.lease != nil
}
func (i *idleTestInstance) Wake(c context.Context, p func(context.Context, http.Handler) error) (RuntimeLease, error) {
	i.wakes++
	return i.wake(c, p)
}
func (i *idleTestInstance) TryQuiesce(time.Time, time.Duration) (RuntimeQuiescence, bool) {
	return i.q, i.q != nil
}
func (i *idleTestInstance) CleanDormant() bool { return i.clean }
func (i *idleTestInstance) InvalidateClean()   { i.clean = false }
func (i *idleTestInstance) Retire()            { i.retired++ }
func (i *idleTestInstance) Quiescing() bool    { return i.quiescing }

func TestDocsIdleWakeErrorsAreSafe(t *testing.T) {
	for _, name := range []string{"spawn", "backoff", "late cancellation", "shutdown"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			m := &idleTestInstance{}
			m.wake = func(context.Context, func(context.Context, http.Handler) error) (RuntimeLease, error) {
				if name == "late cancellation" {
					cancel()
				}
				if name == "backoff" {
					return nil, errors.New("document worker retry in 4s")
				}
				return nil, errors.New("worker stderr: SECRET")
			}
			s := &Service{stop: make(chan struct{})}
			if name == "shutdown" {
				close(s.stop)
			}
			d := &deployed{inst: m, redact: newRedactor(map[string]string{"TOKEN": "SECRET"})}
			_, release, err := s.acquireDocsLocked(ctx, "notes", d)
			release()
			if err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("unsafe failure: %v", err)
			}
			if name == "late cancellation" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
			} else if !errors.Is(err, ErrUnavailable) || ErrorCategory(err) != "unavailable" {
				t.Fatalf("wrong classification: %v", err)
			}
			if name == "shutdown" && m.wakes != 0 {
				t.Fatal("shutdown woke worker")
			}
		})
	}
}

func TestDocsIdleLeasePinsFullHandlerAndMaintenanceNeverWakes(t *testing.T) {
	m := &idleTestInstance{}
	d := &deployed{inst: m}
	_, release, ok := withRunningDocs(t.Context(), d)
	release()
	if ok || m.wakes != 0 || len(m.foreground) != 1 || m.foreground[0] {
		t.Fatal("maintenance woke or recorded foreground")
	}
	lease := &idleTestLease{}
	lease.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lease.released != 0 {
			t.Fatal("lease released before response completed")
		}
		w.WriteHeader(204)
	})
	m.lease = lease
	s := &Service{}
	// A ready HTTP request must not acquire a storage lock held by maintenance.
	mu := &sync.Mutex{}
	mu.Lock()
	defer mu.Unlock()
	s.locks.Store("notes", mu)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.docsDemand("notes", d).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ready request blocked on storage lock")
	}
	if rec.Code != 204 || lease.released != 1 || m.wakes != 0 {
		t.Fatalf("response=%d release=%d wake=%d", rec.Code, lease.released, m.wakes)
	}
	direct := withDocsDispatch(t.Context(), d, lease)
	before := len(m.foreground)
	_, release, ok = withRunningDocs(direct, d)
	release()
	if !ok || len(m.foreground) != before || lease.released != 1 {
		t.Fatal("nested host operation reacquired or released parent lease")
	}
	if docsDirect(direct, &deployed{inst: &idleTestInstance{}}) != nil {
		t.Fatal("old lease escaped into replacement instance")
	}
}

func TestDocsIdleLockWaitHonorsQuiescenceAndCancellation(t *testing.T) {
	for _, name := range []string{"quiescing", "canceled"} {
		t.Run(name, func(t *testing.T) {
			s := &Service{stop: make(chan struct{})}
			mu := &sync.Mutex{}
			mu.Lock()
			defer mu.Unlock()
			s.locks.Store("notes", mu)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			m := &idleTestInstance{quiescing: name == "quiescing"}
			if name == "canceled" {
				cancel()
			}
			start := time.Now()
			unlock, q, err := s.docsWakeLock(ctx, "notes", m, time.Now().Add(20*time.Millisecond))
			if unlock != nil || err == nil || time.Since(start) > time.Second {
				t.Fatalf("unbounded lock wait: q=%v err=%v", q, err)
			}
			if name == "quiescing" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("wrong deadline: %v", err)
			}
			if name == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("wrong cancellation: %v", err)
			}
		})
	}
}

func TestDocsIdleImportedReceiptSurvivesPreparationContext(t *testing.T) {
	c := &docsOpCapture{op: "uncertain-rollback"}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), docsOpCaptureKey{}, c))
	cancel()
	captureDocsImport(context.WithoutCancel(ctx), docsImport{byOp: map[string]int{"unrelated": 7, "uncertain-rollback": 8}})
	captureDocsImport(ctx, docsImport{byOp: map[string]int{"unrelated": 9}})
	if c.recorded() != 8 {
		t.Fatalf("receipt lost after ACK/context cancellation: %d", c.recorded())
	}
}

func TestDocsIdleCloseDoesNotHoldServiceMutexAcrossPreparationStop(t *testing.T) {
	s := &Service{stop: make(chan struct{}), live: map[string]*liveFlat{"notes": {}}}
	started := make(chan struct{})
	terminate := make(chan struct{})
	wakeDone := make(chan struct{})
	m := &idleTestInstance{}
	d := &deployed{inst: m, redact: noRedact}
	s.live["notes"].cur.Store(d)
	m.wake = func(context.Context, func(context.Context, http.Handler) error) (RuntimeLease, error) {
		close(started)
		<-terminate
		// Finishing preparation needs the service mutex, exactly while Stop joins it.
		if s.currentDeployment("notes") != d {
			return nil, errors.New("deployment disappeared")
		}
		return nil, ErrUnavailable
	}
	m.stop = func() { close(terminate); <-wakeDone }
	go func() {
		defer close(wakeDone)
		_, release, _ := s.acquireDocsLocked(t.Context(), "notes", d)
		release()
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("preparation never started")
	}
	closed := make(chan struct{})
	go func() { defer close(closed); s.Close() }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close holds service mutex while Stop waits for preparation")
	}
}
