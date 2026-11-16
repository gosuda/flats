package webhook

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func eventID(n int) string { return fmt.Sprintf("evt_%03d", n) }

// TestNotifyBeforeRun shows that an event handed over before Run waits in the
// queue and that Run delivers it.
func TestNotifyBeforeRun(t *testing.T) {
	t.Parallel()
	srv, capt := statusServer(t, http.StatusNoContent)
	client, _ := newCountingClient(t)
	h := newHarness(t, Config{Keys: keysFor(srv.URL), Client: client, Waits: []time.Duration{}, Now: fixedClock})

	within(t, "Notify before Run", func() { h.sender.Notify(sampleEvent("evt_early")) })
	if n := capt.count(); n != 0 {
		t.Fatalf("%d requests before Run started", n)
	}
	h.start(t.Context())
	if got := h.wait(1); got[0] != "evt_early" {
		t.Fatalf("finished %v, want evt_early", got)
	}
	reqs := capt.all()
	if len(reqs) != 1 || reqs[0].header.Get("X-Flats-Event-Id") != "evt_early" {
		t.Fatalf("receiver saw %d requests, want the one of evt_early", len(reqs))
	}
}

// TestDeliveriesStartInOrder uses one worker, so the order at the receiver is
// the order in which the deliveries started: the order of the events.
func TestDeliveriesStartInOrder(t *testing.T) {
	t.Parallel()
	const total = 40
	srv, capt := statusServer(t, http.StatusOK)
	client, _ := newCountingClient(t)
	h := newHarness(t, Config{Keys: keysFor(srv.URL), Client: client, Workers: 1, Waits: []time.Duration{}, Now: fixedClock})
	for i := 1; i <= total; i++ {
		h.sender.Notify(sampleEvent(eventID(i)))
	}
	h.start(t.Context())
	h.wait(total)

	var got []string
	for _, r := range capt.all() {
		got = append(got, r.header.Get("X-Flats-Event-Id"))
	}
	var want []string
	for i := 1; i <= total; i++ {
		want = append(want, eventID(i))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("order at the receiver:\n%v\nwant\n%v", got, want)
	}
}

// TestFullQueueDropsTheOldest fills a small queue before Run: the sender keeps
// the newest events and logs the IDs of the dropped ones in order.
func TestFullQueueDropsTheOldest(t *testing.T) {
	t.Parallel()
	srv, capt := statusServer(t, http.StatusOK)
	client, _ := newCountingClient(t)
	h := newHarness(t, Config{
		Keys: keysFor(srv.URL), Client: client, Workers: 1, QueueSize: 4, Waits: []time.Duration{}, Now: fixedClock,
	})
	for i := 1; i <= 10; i++ {
		h.sender.Notify(sampleEvent(eventID(i)))
	}
	// Notify writes no log line itself: a worker logs the drops, so that a log that blocks cannot block an operation.
	if lines := h.log.snapshot(); len(lines) != 0 {
		t.Fatalf("Notify wrote %d log lines %q", len(lines), lines)
	}
	h.start(t.Context())
	h.wait(4)
	lines := h.log.snapshot()
	if len(lines) != 6 {
		t.Fatalf("%d log lines %q, want 6 for the 6 dropped events", len(lines), lines)
	}
	for i, line := range lines {
		if !strings.Contains(line, eventID(i+1)) || !strings.Contains(line, "approval.created") {
			t.Errorf("log line %d = %q, want the id %s and the type", i, line, eventID(i+1))
		}
	}
	var got []string
	for _, r := range capt.all() {
		got = append(got, r.header.Get("X-Flats-Event-Id"))
	}
	if want := []string{eventID(7), eventID(8), eventID(9), eventID(10)}; !slices.Equal(got, want) {
		t.Fatalf("delivered %v, want %v", got, want)
	}
}

// TestNotifyNeverBlocks is the queue rule of the specification with a receiver
// that does not answer: 300 events, 4 in flight, 256 waiting, 40 dropped.
func TestNotifyNeverBlocks(t *testing.T) {
	const (
		total   = 300
		workers = 4
		queue   = 256
	)
	// The deadline of the whole test. A blocked Notify or Run would hang here
	// and not in a sleep.
	ctx, cancel := context.WithTimeout(t.Context(), 2*watchdog)
	defer cancel()

	var (
		release     = make(chan struct{})
		inflight    atomic.Int32
		maxInflight atomic.Int32
		mu          sync.Mutex
		started     []string
		fourIn      = make(chan struct{})
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			old := maxInflight.Load()
			if cur <= old || maxInflight.CompareAndSwap(old, cur) {
				break
			}
		}
		mu.Lock()
		started = append(started, r.Header.Get("X-Flats-Event-Id"))
		if len(started) == workers {
			close(fourIn)
		}
		mu.Unlock()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	// Release the receiver before the server closes, whatever the test does.
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)

	client, _ := newCountingClient(t)
	h := newHarness(t, Config{Keys: keysFor(srv.URL), Client: client, Waits: []time.Duration{}, Now: fixedClock})
	h.start(ctx)

	for i := 1; i <= workers; i++ {
		h.sender.Notify(sampleEvent(eventID(i)))
	}
	recv(t, fourIn, "the four receivers to be entered")

	burst := make(chan struct{})
	go func() {
		defer close(burst)
		for i := workers + 1; i <= total; i++ {
			h.sender.Notify(sampleEvent(eventID(i)))
		}
	}()
	select {
	case <-burst:
	case <-ctx.Done():
		t.Fatal("Notify blocked while every worker waited for the receiver")
	}

	if n := inflight.Load(); n != workers {
		t.Errorf("%d deliveries in flight, want %d", n, workers)
	}
	h.sender.mu.Lock()
	waiting := h.sender.q.len()
	h.sender.mu.Unlock()
	if waiting != queue {
		t.Errorf("%d events wait, want %d", waiting, queue)
	}
	// The oldest waiting events were dropped: the ones after the four in flight.
	dropped := total - workers - queue
	// Notify wrote no log line itself, and the receiver holds every worker, so no drop is logged yet.
	if lines := h.log.snapshot(); len(lines) != 0 {
		t.Fatalf("%d log lines while every worker waits; Notify must write none", len(lines))
	}

	releaseAll()
	h.wait(workers + queue)

	mu.Lock()
	order := slices.Clone(started)
	mu.Unlock()
	firstFour := slices.Clone(order[:workers])
	slices.Sort(firstFour)
	if want := []string{eventID(1), eventID(2), eventID(3), eventID(4)}; !slices.Equal(firstFour, want) {
		t.Errorf("the first four deliveries were %v, want the events 1 to 4", firstFour)
	}
	var want []string
	for i := 1; i <= workers; i++ {
		want = append(want, eventID(i))
	}
	for i := workers + dropped + 1; i <= total; i++ {
		want = append(want, eventID(i))
	}
	slices.Sort(order)
	if !slices.Equal(order, want) {
		t.Errorf("delivered %d events, want exactly the %d that were not dropped\ngot  %v\nwant %v", len(order), len(want), order, want)
	}
	if got := maxInflight.Load(); got != workers {
		t.Errorf("at most %d deliveries at once, want exactly %d", got, workers)
	}
	// The log is read after Run returned: a worker that took the notes may not have written every line yet.
	h.stop()
	lines := h.log.snapshot()
	if len(lines) != dropped {
		t.Fatalf("%d log lines at the end, want only the %d for dropped events", len(lines), dropped)
	}
	for i, line := range lines {
		id := eventID(workers + 1 + i)
		if !strings.Contains(line, id) || !strings.Contains(line, "approval.created") {
			t.Errorf("log line %d = %q, want the id %s and the type", i, line, id)
		}
	}
}

func TestRunOnce(t *testing.T) {
	t.Parallel()
	s := New(Config{Now: fixedClock})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	within(t, "a Run with a context that is done", func() { s.Run(ctx) })
	within(t, "a second Run", func() { s.Run(context.Background()) })
}

// TestStopCancelsAttemptsAndDropsTheQueue stops a sender with attempts in
// flight and events waiting, and then looks for what must not exist after it:
// a call of the receiver, a delivery of a later event, a goroutine of the
// sender.
func TestStopCancelsAttemptsAndDropsTheQueue(t *testing.T) {
	// Not parallel: it counts the goroutines of the whole process.
	stopScenario(t) // The first run starts what the runtime starts once.
	before := runtime.NumGoroutine()
	stopScenario(t)
	deadline := time.Now().Add(watchdog)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines after the sender stopped, %d before", runtime.NumGoroutine(), before)
		}
		time.Sleep(time.Millisecond)
	}
}

func stopScenario(t *testing.T) {
	t.Helper()
	const workers = 2
	var (
		entered   atomic.Int32
		enteredCh = make(chan struct{}, 64)
		release   = make(chan struct{})
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered.Add(1)
		select {
		case enteredCh <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	tr := &http.Transport{DisableKeepAlives: true}
	client := &http.Client{Transport: tr}

	log := &recorder{}
	s := New(Config{
		Keys: keysFor(srv.URL), Client: client, Workers: workers, QueueSize: 8,
		Waits: []time.Duration{0, 0, 0}, Now: fixedClock, Logf: log.logf,
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	for i := 1; i <= 6; i++ {
		s.Notify(sampleEvent(eventID(i)))
	}
	for range workers {
		recv(t, enteredCh, "the receiver to be entered")
	}
	cancel()
	recv(t, done, "Run to return after its context ended")

	// Run waited for its goroutines, so nothing of the sender runs now.
	if n := countSenderGoroutines(); n != 0 {
		t.Errorf("%d goroutines of the sender after Run returned", n)
	}
	calls := entered.Load()
	if calls != workers {
		t.Errorf("the receiver was entered %d times, want %d: one for each worker, none for the queue", calls, workers)
	}
	s.mu.Lock()
	state, waiting := s.state, s.q.len()
	s.mu.Unlock()
	if state != stateStopped || waiting != 0 {
		t.Errorf("state %d with %d waiting events after Run, want stopped and none", state, waiting)
	}

	within(t, "Notify after Run returned", func() {
		for i := 100; i < 110; i++ {
			s.Notify(sampleEvent(eventID(i)))
		}
	})
	s.mu.Lock()
	waiting = s.q.len()
	s.mu.Unlock()
	if waiting != 0 {
		t.Errorf("%d events queued after Run returned, want them dropped", waiting)
	}

	// Close the receiver: it waits for every call that it still serves.
	close(release)
	srv.Close()
	tr.CloseIdleConnections()
	if got := entered.Load(); got != calls {
		t.Errorf("the receiver was called %d times after Run returned", got-calls)
	}
	if lines := log.snapshot(); len(lines) != 0 {
		t.Errorf("a stop wrote log lines: %q", lines)
	}
}

// countSenderGoroutines counts the goroutines that run a method of the
// sender. The workers end when the WaitGroup of Run is done, a moment before
// the goroutines are gone, so it polls with a deadline.
func countSenderGoroutines() int {
	deadline := time.Now().Add(watchdog)
	for {
		buf := make([]byte, 1<<16)
		for {
			n := runtime.Stack(buf, true)
			if n < len(buf) {
				buf = buf[:n]
				break
			}
			buf = make([]byte, 2*len(buf))
		}
		count := 0
		for _, g := range strings.Split(string(buf), "\n\n") {
			if strings.Contains(g, "webhook.(*Sender).") {
				count++
			}
		}
		if count == 0 || time.Now().After(deadline) {
			return count
		}
		time.Sleep(time.Millisecond)
	}
}

// TestStopEndsRetryWaits shows that a worker in a retry wait does not keep Run
// from returning: the waits here are an hour long.
func TestStopEndsRetryWaits(t *testing.T) {
	// Not parallel: it looks for goroutines of any sender in the process.
	var capt capture
	failed := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capt.record(r)
		w.WriteHeader(http.StatusInternalServerError)
		select {
		case failed <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(srv.Close)
	client, _ := newCountingClient(t)
	h := newHarness(t, Config{
		Keys: keysFor(srv.URL), Client: client, Workers: 2,
		Waits: []time.Duration{time.Hour, time.Hour, time.Hour}, Now: fixedClock,
	})
	h.start(t.Context())
	h.sender.Notify(sampleEvent("evt_a"))
	h.sender.Notify(sampleEvent("evt_b"))
	h.sender.Notify(sampleEvent("evt_c"))
	recv(t, failed, "the first failed attempt")
	recv(t, failed, "the second failed attempt")

	h.cancel()
	recv(t, h.done, "Run to return while workers wait to retry")
	if n := countSenderGoroutines(); n != 0 {
		t.Errorf("%d goroutines of the sender after Run returned", n)
	}
	if n := capt.count(); n != 2 {
		t.Errorf("%d attempts, want 2: the retries and the third event never started", n)
	}
}

// TestNotifyFromManyGoroutines hands events over from many goroutines while
// the sender delivers, which the race detector checks where it is available.
// The count shows that nothing is lost or duplicated while the queue has room.
func TestNotifyFromManyGoroutines(t *testing.T) {
	t.Parallel()
	const (
		goroutines = 8
		each       = 25
	)
	var capt capture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capt.record(r)
	}))
	t.Cleanup(srv.Close)
	client, _ := newCountingClient(t)
	h := newHarness(t, Config{Keys: keysFor(srv.URL), Client: client, Waits: []time.Duration{}, Now: fixedClock})
	h.start(t.Context())

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range each {
				h.sender.Notify(sampleEvent(eventID(g*each + i)))
			}
		})
	}
	wg.Wait()
	h.wait(goroutines * each)

	var got []string
	for _, r := range capt.all() {
		got = append(got, r.header.Get("X-Flats-Event-Id"))
	}
	slices.Sort(got)
	var want []string
	for i := range goroutines * each {
		want = append(want, eventID(i))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("delivered %d events, want each of the %d once", len(got), len(want))
	}
}

func TestFifo(t *testing.T) {
	t.Parallel()
	var f fifo
	next, pushed := 0, 0
	for i := range 1000 {
		f.push(&event{id: eventID(pushed)})
		pushed++
		// Pop in a pattern that lets the live part drift through the array.
		for f.len() > 3+i%5 {
			e := f.pop()
			if e.id != eventID(next) {
				t.Fatalf("popped %s, want %s", e.id, eventID(next))
			}
			next++
		}
	}
	if got := f.len(); got != pushed-next {
		t.Fatalf("len = %d, want %d", got, pushed-next)
	}
	if cap(f.items) > 64 {
		t.Errorf("the array grew to %d for a queue that never held more than 8", cap(f.items))
	}
	f.reset()
	if f.len() != 0 || f.pop() != nil {
		t.Error("a reset queue is not empty")
	}
}

// TestNotifyDoesNotWaitForTheLog fills the queue of a sender whose log blocks:
// Notify still returns, because a worker writes the log line of a drop
// (rule 15). The blocked log holds only that worker.
func TestNotifyDoesNotWaitForTheLog(t *testing.T) {
	t.Parallel()
	srv, _ := statusServer(t, http.StatusOK)
	client, _ := newCountingClient(t)
	blocked := make(chan struct{})
	entered := make(chan struct{}, 1)
	logf := func(string, ...any) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-blocked
	}
	s := New(Config{Keys: keysFor(srv.URL), Client: client, Workers: 1, QueueSize: 2, Waits: []time.Duration{}, Now: fixedClock, Logf: logf})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	t.Cleanup(func() {
		close(blocked)
		cancel()
		recv(t, done, "Run to return")
	})
	// More events than the queue holds, so that the sender drops some and has log lines to write.
	within(t, "Notify with a full queue and a log that blocks", func() {
		for i := 1; i <= 50; i++ {
			s.Notify(sampleEvent(eventID(i)))
		}
	})
	recv(t, entered, "the worker to start the log line of a drop")
	// The worker is held by the log now. Notify still returns.
	within(t, "Notify while the log holds the worker", func() {
		for i := 51; i <= 100; i++ {
			s.Notify(sampleEvent(eventID(i)))
		}
	})
}

// TestMoreDropsThanNotesAreCounted drops more events than the sender keeps
// log lines for: the log has the IDs of the first 64 drops and one line with
// the count of the rest (rule 16).
func TestMoreDropsThanNotesAreCounted(t *testing.T) {
	t.Parallel()
	srv, _ := statusServer(t, http.StatusOK)
	client, _ := newCountingClient(t)
	h := newHarness(t, Config{
		Keys: keysFor(srv.URL), Client: client, Workers: 1, QueueSize: 2, Waits: []time.Duration{}, Now: fixedClock,
	})
	const total = 100
	for i := 1; i <= total; i++ {
		h.sender.Notify(sampleEvent(eventID(i)))
	}
	h.start(t.Context())
	h.wait(2)
	h.stop()
	lines := h.log.snapshot()
	const dropped = total - 2
	if len(lines) != maxNotes+1 {
		t.Fatalf("%d log lines, want %d with an ID and one with the count of the rest", len(lines), maxNotes)
	}
	for i, line := range lines[:maxNotes] {
		if !strings.Contains(line, eventID(i+1)) {
			t.Errorf("log line %d = %q, want the id %s", i, line, eventID(i+1))
		}
	}
	if want := fmt.Sprintf("%d more events dropped", dropped-maxNotes); !strings.Contains(lines[maxNotes], want) {
		t.Errorf("the last line = %q, want %q", lines[maxNotes], want)
	}
}
