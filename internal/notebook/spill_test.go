package notebook

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type spillClock struct {
	mu sync.Mutex
	at time.Time
}

func newSpillClock() *spillClock              { return &spillClock{at: time.Unix(1, 0)} }
func (c *spillClock) now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *spillClock) advance(d time.Duration) { c.mu.Lock(); c.at = c.at.Add(d); c.mu.Unlock() }
func spillManager(t *testing.T, budget int64, clock *spillClock) *SpillManager {
	t.Helper()
	m := NewSpillManager(t.TempDir(), budget, clock.now)
	t.Cleanup(func() { m.CancelScope(func(Scope) bool { return true }) })
	return m
}
func spillPending(t *testing.T, m *SpillManager, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		got := len(m.queue)
		m.mu.Unlock()
		if got == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("waiter did not enter queue")
}
func spillLease(t *testing.T, m *SpillManager, size int64, parts int) *Spill {
	t.Helper()
	s, err := m.acquire(t.Context(), size, Scope{Tree: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"data", "records", "parts"} {
		if err := os.WriteFile(filepath.Join(s.dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	m.mu.Lock()
	s.capturing = false
	s.ended = m.now()
	s.last = s.ended
	s.parts = parts
	m.mu.Unlock()
	return s
}
func TestSpillAdmissionFIFOAndFitting(t *testing.T) {
	clock := newSpillClock()
	m := spillManager(t, 100, clock)
	active := spillLease(t, m, 101, 3)
	small := spillLease(t, m, 40, 1)
	if _, ok := m.Lookup(small.Cursor()); !ok {
		t.Fatal("fitting spill blocked by oversized spill")
	}
	type result struct {
		s   *Spill
		err error
	}
	first := make(chan result, 1)
	second := make(chan result, 1)
	go func() { s, e := m.acquire(t.Context(), 101, Scope{Page: vocabID(PageKind, 1)}); first <- result{s, e} }()
	spillPending(t, m, 1)
	go func() { s, e := m.acquire(t.Context(), 102, Scope{Page: vocabID(PageKind, 2)}); second <- result{s, e} }()
	spillPending(t, m, 2)
	extra := spillLease(t, m, 60, 1)
	clock.advance(10 * time.Second)
	if _, ok := m.Lookup(active.Cursor()); ok {
		t.Fatal("replaying reader retained contended slot")
	}
	var a result
	select {
	case a = <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("first waiter stuck")
	}
	if a.err != nil {
		t.Fatal(a.err)
	}
	select {
	case <-second:
		t.Fatal("second waiter bypassed first")
	default:
	}
	m.CancelScope(func(s Scope) bool { return s.Page == vocabID(PageKind, 1) })
	select {
	case b := <-second:
		if b.err != nil {
			t.Fatal(b.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second waiter stuck")
	}
	if _, ok := m.Lookup(extra.Cursor()); !ok {
		t.Fatal("fitting spill reclaimed by contention")
	}
	if _, err := os.Stat(active.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired files remain: %v", err)
	}
}

func TestSpillCaptureProgressFloor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		bytes   int
		step    time.Duration
		wantErr bool
	}{{"below_floor", 64 << 10, time.Second, true}, {"above_floor_over_five_minutes", 64 << 10, 500 * time.Millisecond, false}} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newSpillClock()
			m := spillManager(t, 1, clock)
			s, err := m.acquire(t.Context(), 2, Scope{Tree: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(s.dir, 0700); err != nil {
				t.Fatal(err)
			}
			f, err := os.Create(filepath.Join(s.dir, "data"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			start := clock.now()
			writer := captureWriter{s: s, out: spillThrottle{out: f, clock: clock, step: tc.step}}
			block := make([]byte, tc.bytes)
			for i := 0; i < 650; i++ {
				_, err = writer.Write(block)
				if err != nil {
					break
				}
			}
			if tc.wantErr {
				if !errors.Is(err, errSpillUnavailable) {
					t.Fatalf("below floor error: %v", err)
				}
				if _, e := os.Stat(s.dir); !errors.Is(e, os.ErrNotExist) {
					t.Fatalf("cancelled scratch survives: %v", e)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if clock.now().Sub(start) <= 5*time.Minute {
					t.Fatal("capture did not cross five minutes")
				}
				m.mu.Lock()
				s.capturing = false
				s.last = m.now()
				s.ended = s.last
				m.mu.Unlock()
				if _, ok := m.Lookup(s.Cursor()); !ok {
					t.Fatal("progressing capture expired")
				}
			}
		})
	}
}

type spillThrottle struct {
	out   io.Writer
	clock *spillClock
	step  time.Duration
}

func (w spillThrottle) Write(p []byte) (int, error) { w.clock.advance(w.step); return w.out.Write(p) }

func TestSpillStallCancellationAndStartupCleanup(t *testing.T) {
	clock := newSpillClock()
	m := spillManager(t, 1, clock)
	s := spillLease(t, m, 2, 1)
	m.mu.Lock()
	s.capturing = true
	s.progressAt = clock.now()
	m.mu.Unlock()
	clock.advance(10 * time.Second)
	if _, ok := m.Lookup(s.Cursor()); ok {
		t.Fatal("stalled capture remains")
	}
	if _, err := os.Stat(s.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stalled files survive: %v", err)
	}
	dir := t.TempDir()
	orphan := filepath.Join(dir, "notebook-spill-"+strings.Repeat("a", 32))
	if err := os.Mkdir(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "unrelated")
	if err := os.WriteFile(keep, nil, 0600); err != nil {
		t.Fatal(err)
	}
	cleaned := NewSpillManager(dir, 0, clock.now)
	if cleaned.initErr != nil {
		t.Fatal(cleaned.initErr)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("orphan remains")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("unrelated file removed")
	}
}

func TestSpillQueuedReaderUsesFreedFittingBudget(t *testing.T) {
	clock := newSpillClock()
	m := spillManager(t, 100, clock)
	keep := spillLease(t, m, 40, 1)
	released := spillLease(t, m, 50, 1)
	over := spillLease(t, m, 101, 3)
	done := make(chan *Spill, 1)
	failed := make(chan error, 1)
	go func() {
		s, err := m.acquire(t.Context(), 30, Scope{Page: vocabID(PageKind, 1)})
		if err != nil {
			failed <- err
		} else {
			done <- s
		}
	}()
	spillPending(t, m, 1)
	if err := released.Close(); err != nil {
		t.Fatal(err)
	}
	var admitted *Spill
	select {
	case admitted = <-done:
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("fitting waiter remained blocked by oversize slot")
	}
	if admitted.over {
		t.Fatal("fitting waiter consumed oversized slot")
	}
	if _, ok := m.Lookup(over.Cursor()); !ok {
		t.Fatal("fitting waiter reclaimed oversize reader")
	}
	extra := spillLease(t, m, 30, 1)
	m.mu.Lock()
	used := m.used
	m.mu.Unlock()
	if used != 100 {
		t.Fatalf("fitting reservation not conserved: %d", used)
	}
	if _, ok := m.Lookup(keep.Cursor()); !ok {
		t.Fatal("fitting spill lost")
	}
	if _, ok := m.Lookup(extra.Cursor()); !ok {
		t.Fatal("new fitting spill lost")
	}
}

func TestSpillCloseIsIndividual(t *testing.T) {
	m := spillManager(t, 100, newSpillClock())
	first := spillLease(t, m, 20, 1)
	second := spillLease(t, m, 20, 1)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Lookup(first.Cursor()); ok {
		t.Fatal("closed spill remains")
	}
	if _, ok := m.Lookup(second.Cursor()); !ok {
		t.Fatal("closing one spill cancelled another")
	}
}

func TestSpillContentionStartsAfterCapture(t *testing.T) {
	clock := newSpillClock()
	m := spillManager(t, 1, clock)
	s := spillLease(t, m, 2, 3)
	m.mu.Lock()
	s.capturing = true
	s.progressAt = clock.now()
	m.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, err := m.acquire(t.Context(), 2, Scope{Page: vocabID(PageKind, 1)}); done <- err }()
	spillPending(t, m, 1)
	clock.advance(9 * time.Second)
	if err := s.progress(1 << 20); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	s.capturing = false
	s.ended = clock.now()
	s.last = s.ended
	m.mu.Unlock()
	clock.advance(9 * time.Second)
	if _, ok := m.Lookup(s.Cursor()); !ok {
		t.Fatal("contention consumed the capture's delivery interval")
	}
	clock.advance(time.Second)
	if _, ok := m.Lookup(s.Cursor()); ok {
		t.Fatal("non-advancing reader kept its slot")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiter stuck")
	}
}

func TestSpillCancelQueuedScope(t *testing.T) {
	m := spillManager(t, 1, newSpillClock())
	spillLease(t, m, 2, 1)
	done := make(chan error, 1)
	go func() { _, err := m.acquire(context.Background(), 2, Scope{Public: true, Tree: true}); done <- err }()
	spillPending(t, m, 1)
	m.CancelScope(func(s Scope) bool { return s.Public })
	select {
	case err := <-done:
		if !errors.Is(err, errSpillExpired) {
			t.Fatalf("queued cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued cancellation stuck")
	}
}
