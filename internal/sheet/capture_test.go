package sheet

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/gosuda/flats/internal/sheet/formula"
	"github.com/gosuda/flats/internal/wsconn"
)

// fillCaptures leaves free bytes of the host-wide capture budget, as other
// sheets' captures would, and returns what gives the rest back.
func fillCaptures(t testing.TB, free int64) (release func()) {
	t.Helper()
	held := captureBudget - free - captureBytes.Load()
	if held <= 0 {
		t.Fatalf("the capture budget already holds %d bytes", captureBytes.Load())
	}
	holder := &capture{}
	if !holder.reserve(held) {
		t.Fatal("the budget did not take the filler")
	}
	var once sync.Once
	release = func() { once.Do(holder.release) }
	t.Cleanup(release)
	return release
}

// clear ends every kept capture, as their expiry would.
func (cs *captureSet) clear() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, c := range cs.list {
		c.release()
	}
	cs.list = nil
}

// grid writes rows by cols numbers into Sheet1 from A1.
func (e *env) grid(rows, cols int) {
	e.t.Helper()
	row := "[" + strings.TrimSuffix(strings.Repeat("1,", cols), ",") + "]"
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + strings.TrimSuffix(strings.Repeat(row+",", rows), ",") + `]}]}`)
}

// TestCaptureBudgetAdmission reads a tab from many requests at once while
// the capture budget is nearly full. Every request is refused, and together
// they allocate far less than one copy of the tab: a capture is charged
// while it is built, not after.
func TestCaptureBudgetAdmission(t *testing.T) {
	e := start(t)
	e.grid(1000, 26)
	s := serve(e)
	tab := e.tabID("Sheet1")
	base := captureBytes.Load()

	// One whole capture, to know its size and that it is given back.
	if _, err := e.in.Read(context.Background(), ReadQuery{Tab: "Sheet1", Limit: 10}); err != nil {
		t.Fatal(err)
	}
	whole := captureBytes.Load() - base
	if whole < 26000*64 {
		t.Fatalf("a kept capture of 26,000 cells counts %d bytes", whole)
	}
	// The counted sizes cover the records as they are allocated.
	if cell, row, format := unsafe.Sizeof(capCell{}), unsafe.Sizeof(capRow{}), unsafe.Sizeof(formula.Format{}); cell > capCellBytes || row > captureRowCost || format > capFormatBytes {
		t.Fatalf("a cell record takes %d bytes, a row %d and a format %d; they count %d, %d and %d", cell, row, format, capCellBytes, captureRowCost, capFormatBytes)
	}
	e.in.captures.clear()
	if got := captureBytes.Load(); got != base {
		t.Fatalf("cleared captures leave %d bytes charged", got-base)
	}

	release := fillCaptures(t, whole/8)
	full := captureBytes.Load()
	const readers = 24
	var wg sync.WaitGroup
	errs := make([]error, readers)
	codes := make([]int, readers)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				_, errs[i] = e.in.Read(context.Background(), ReadQuery{Tab: "Sheet1"})
				return
			}
			res, err := http.Get(fmt.Sprintf("%s/public/_sheet/csv?tab=%d", s.server.URL, tab))
			if err != nil {
				errs[i] = err
				return
			}
			res.Body.Close()
			codes[i] = res.StatusCode
		}()
	}
	wg.Wait()
	runtime.ReadMemStats(&after)
	for i := 0; i < readers; i++ {
		if i%2 == 0 {
			e.wantRefusal(errs[i], "unavailable", "capture_budget")
		} else if errs[i] != nil || codes[i] != http.StatusServiceUnavailable {
			t.Errorf("CSV download %d: status %d, %v", i, codes[i], errs[i])
		}
	}
	if got := captureBytes.Load(); got != full {
		t.Fatalf("refused reads leave %d bytes charged", got-full)
	}
	// Each refused request may copy up to the free part of the budget and
	// costs an HTTP exchange; none may copy the tab.
	if spent := int64(after.TotalAlloc - before.TotalAlloc); spent > readers*whole/4 {
		t.Fatalf("%d refused reads allocated %d bytes; one capture of the tab counts %d", readers, spent, whole)
	}

	// With the budget free again the same reads succeed.
	release()
	if out := e.read(ReadQuery{Tab: "Sheet1"}); out["range"] != "A1:Z1000" {
		t.Fatalf("read after the budget was freed: %v", out["range"])
	}
	e.in.captures.clear()
	if got := captureBytes.Load(); got != base {
		t.Fatalf("a finished read leaves %d bytes charged", got-base)
	}
}

// TestSnapshotWaitsForCaptureBudget watches a tab while the capture budget
// is full. The browser is told to wait, and the same connection gets its
// snapshot when the budget has room, with no new watch.
func TestSnapshotWaitsForCaptureBudget(t *testing.T) {
	e := start(t)
	e.grid(50, 4)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Other"},{"op":"write","tab":"Other","at":"A1","rows":[["other"]]}]}`)
	s := serve(e)
	tab, other := e.tabID("Sheet1"), e.tabID("Other")
	base := captureBytes.Load()
	for _, route := range []string{"private", "public"} {
		release := fillCaptures(t, 0)
		c := s.dial(route, helloV1)
		c.next("welcome")
		c.send(fmt.Sprintf(`{"type":"watch","generation":1,"tab":%d}`, tab))
		if f := c.next("status", "tab_head"); f["type"] != "status" || f["calculating"] != true {
			t.Fatalf("%s: a watch under a full budget got %v", route, f)
		}
		// An edit made while the watch waits is part of the later snapshot.
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["` + route + `"]]}]}`)
		if f := c.next("delta", "tab_head"); f["type"] != "delta" {
			t.Fatalf("%s: while waiting the connection got %v", route, f)
		}
		release()
		if f := c.next("status", "tab_head"); f["type"] != "status" || f["calculating"] != false {
			t.Fatalf("%s: after the budget was freed the connection got %v", route, f)
		}
		head, cells := c.tab()
		if head["generation"] != float64(1) || head["tab"] != float64(tab) || head["seq"] != fmt.Sprint(e.seq()) {
			t.Fatalf("%s: head %v", route, head)
		}
		if cell := cells[cellKey(e, "A1")]; len(cell) < 3 || cell[2] != route {
			t.Fatalf("%s: the snapshot has A1 = %v", route, cell)
		}

		// A newer watch replaces a waiting one: only the newer tab arrives.
		// The edit drops the cached snapshot frames.
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[[2]]}]}`)
		release = fillCaptures(t, 0)
		c.send(fmt.Sprintf(`{"type":"watch","generation":2,"tab":%d}`, tab))
		c.next("status")
		c.send(fmt.Sprintf(`{"type":"watch","generation":3,"tab":%d}`, other))
		time.Sleep(50 * time.Millisecond)
		release()
		if f := c.next("status"); f["calculating"] != false {
			t.Fatalf("%s: status %v", route, f)
		}
		if head, _ := c.tab(); head["generation"] != float64(3) || head["tab"] != float64(other) {
			t.Fatalf("%s: the newer watch got the head %v", route, head)
		}
		c.ws.Close(1000, "")
	}
	// Cached frames count until the cache is dropped.
	e.in.Stop()
	if got := captureBytes.Load(); got != base {
		t.Fatalf("%d capture bytes stay charged", got-base)
	}
}

// TestSnapshotChargeFollowsOwners follows a snapshot through the capture
// budget. While a connection still has frames to send, its capture stays
// charged; a snapshot that was sent whole counts only as the frames the
// cache keeps, and nothing once the cache drops them.
func TestSnapshotChargeFollowsOwners(t *testing.T) {
	e := start(t)
	// 6 MB of text: more than a send queue holds.
	for row := 1; row <= 200; row += 50 {
		cells := make([]string, 50)
		for i := range cells {
			cells[i] = `["` + strings.Repeat("t", 30000) + `"]`
		}
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, row, strings.Join(cells, ",")))
	}
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Other"}]}`)
	s := serve(e)
	tab := e.tabID("Sheet1")
	base, cacheBase := captureBytes.Load(), snapshotCached.Load()
	charged := func() int64 { return captureBytes.Load() - base }
	// settle waits until the count satisfies want and holds still.
	settle := func(want func(int64) bool) int64 {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for same, last := 0, int64(-1); same < 10 && time.Now().Before(deadline); {
			time.Sleep(2 * time.Millisecond)
			if now := charged(); now == last && want(now) {
				same++
			} else {
				same, last = 0, now
			}
		}
		return charged()
	}
	// The text is 6 MB. A capture counts each text once, the content and
	// the value sharing it, at what the allocator takes for it, with its
	// records; the frames carry the bytes of the text once.
	const text = 200 * 30000
	held := 200 * formula.TextBytes(30000)
	isCapture := func(n int64) bool { return n >= held }
	isFrames := func(n int64) bool { return n >= text && n < text*5/4 }

	// A connection that asks for the tab and then reads nothing: the
	// encoder stops when the send queue is full, with the capture charged.
	host := strings.TrimPrefix(s.server.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, err := wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) { return cappedDial(ctx, "tcp", host) }, host, "/public/_sheet/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close(1000, "")
	for _, frame := range []string{helloV1, fmt.Sprintf(`{"type":"watch","generation":1,"tab":%d}`, tab)} {
		if err := ws.Send(frame); err != nil {
			t.Fatal(err)
		}
	}
	if got := settle(isCapture); !isCapture(got) {
		t.Fatalf("a snapshot in flight counts %d bytes, want its capture of more than %d", got, held)
	}
	// An edit elsewhere changes nothing for the snapshot in flight.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Other","at":"A1","rows":[[1]]}]}`)
	if got := settle(isCapture); !isCapture(got) {
		t.Fatalf("after an edit the snapshot in flight counts %d bytes", got)
	}
	// When the connection is gone nothing is counted, and nothing of the
	// unfinished snapshot is in the cache.
	ws.Close(1000, "")
	if got := settle(func(n int64) bool { return n == 0 }); got != 0 || snapshotCached.Load() != cacheBase {
		t.Fatalf("%d bytes stay counted after the connection closed, %d of them as cache", got, snapshotCached.Load()-cacheBase)
	}

	// A snapshot that was sent whole counts as the frames the cache keeps.
	c := s.dial("private", helloV1)
	c.next("welcome")
	c.watch(1, tab)
	if got := settle(isFrames); !isFrames(got) || snapshotCached.Load()-cacheBase != got {
		t.Fatalf("cached frames count %d bytes in the budget and %d in the cache", got, snapshotCached.Load()-cacheBase)
	}
	// A second connection is served from the cache without a capture.
	second := s.dial("private", helloV1)
	second.next("welcome")
	if _, cells := second.watch(1, tab); len(cells) != 200 {
		t.Fatalf("the cached snapshot holds %d cells", len(cells))
	}
	if got := settle(isFrames); !isFrames(got) {
		t.Fatalf("with two readers the cached frames count %d bytes", got)
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Other","at":"A1","rows":[[2]]}]}`)
	if got := settle(func(n int64) bool { return n == 0 }); got != 0 || snapshotCached.Load() != cacheBase {
		t.Fatalf("%d bytes stay counted after the cache was dropped", got)
	}
	if n := encoderGrowths.Load(); n != 0 {
		t.Fatalf("%d encoder buffers outgrew their reservation", n)
	}
}

// TestEvictedCaptureStaysChargedWhileRead holds a page request inside a
// capture while newer reads push that capture out of the kept set, and
// while its lifetime ends. The capture stays charged until the page is
// done; only its cursor stops working.
func TestEvictedCaptureStaysChargedWhileRead(t *testing.T) {
	e := start(t)
	e.grid(200, 26)
	base := captureBytes.Load()
	charged := func() int64 { return captureBytes.Load() - base }
	cursor := cursorOf(t, e.read(ReadQuery{Tab: "Sheet1", Limit: 100}))
	one := charged()
	if one == 0 {
		t.Fatal("a kept capture counts nothing")
	}
	for _, how := range []string{"evicted", "expired"} {
		entered, proceed := make(chan struct{}), make(chan struct{})
		hookOnce(e, map[string]func(){"read_page": func() {
			close(entered)
			<-proceed
		}})
		type result struct {
			page []byte
			err  error
		}
		done := make(chan result, 1)
		go func() {
			page, err := e.in.Read(context.Background(), ReadQuery{Cursor: cursor})
			done <- result{page, err}
		}()
		<-entered
		kept := int64(1)
		if how == "evicted" {
			// Four first pages at once push the capture out of the set.
			var wg sync.WaitGroup
			for i := 0; i < maxCaptures; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := e.in.Read(context.Background(), ReadQuery{Tab: "Sheet1", Limit: 100}); err != nil {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			kept = maxCaptures
		} else {
			e.clock.Add(2 * captureTTL)
			e.tick()
			kept = 0
		}
		if got := charged(); got != (kept+1)*one {
			t.Fatalf("%s: while its page is read %d bytes are charged, want %d captures of %d", how, got, kept+1, one)
		}
		// Another request cannot start on the cursor any more.
		_, err := e.in.Read(context.Background(), ReadQuery{Cursor: cursor})
		e.wantRefusal(err, "conflict", "read_expired")
		close(proceed)
		res := <-done
		var page map[string]any
		if res.err != nil || json.Unmarshal(res.page, &page) != nil || page["start"] != "A4" {
			t.Fatalf("%s: the page under way: %v, %.80s", how, res.err, res.page)
		}
		if got := charged(); got != kept*one {
			t.Fatalf("%s: after the page %d bytes are charged, want %d captures of %d", how, got, kept, one)
		}
		if how == "evicted" {
			cursor = cursorOf(t, e.read(ReadQuery{Tab: "Sheet1", Limit: 100}))
		}
	}
	e.in.captures.clear()
	if got := charged(); got != 0 {
		t.Fatalf("%d bytes stay charged", got)
	}
}

// liveHeap returns the bytes of the heap objects that a complete
// collection started now leaves allocated. It reads what survives the
// sweep, not the collector's count of marked bytes. Two mark workers that
// reach an object at once can both count it, so large objects that several
// holders share can count several times over in that count. What is
// allocated while the collection runs counts as live, so where other
// goroutines allocate fast the reading is above what they hold (quietHeap).
func liveHeap() int64 {
	runtime.GC()
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	return int64(sample[0].Value.Uint64())
}

// heapPeak samples the live heap until stop is called and returns the most
// it grew over what it was at the start.
func heapPeak() (stop func() int64) {
	base, peak := liveHeap(), int64(0)
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			peak = max(peak, liveHeap()-base)
			select {
			case <-quit:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	return func() int64 {
		close(quit)
		<-done
		return peak
	}
}

// quietHeap returns the live heap at a moment when nothing allocates, read
// from a collection during which less than slack bytes were allocated. What is
// allocated while a collection runs counts as live in it, so a heap read
// while other goroutines allocate fast holds their garbage too; on a busy
// machine a collection runs for longer and counts more of it. The reading
// is therefore at most slack above what is reachable, never below.
func quietHeap(t *testing.T, slack uint64) int64 {
	t.Helper()
	held, noise := heapWithin(slack, 15*time.Second)
	if noise > slack {
		t.Fatalf("something still allocated %d bytes during every collection for 15 s", noise)
	}
	return held
}

// heapWithin reads the heap as quietHeap does, for at most d, and returns
// the reading of the collection during which the least was allocated, with
// that amount. Every reading is at or above what is reachable, so a noisy
// one still bounds what is held from above, but not what was freed.
func heapWithin(slack uint64, d time.Duration) (held int64, noise uint64) {
	deadline := time.Now().Add(d)
	noise = ^uint64(0)
	for {
		if h, n := heapReading(); n < noise {
			held, noise = h, n
		}
		if noise <= slack || time.Now().After(deadline) {
			return held, noise
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// heapBefore reads the heap as heapWithin does, but only from collections
// that end by the deadline, and starts none after it. ok is false when no
// collection ended by then.
func heapBefore(slack uint64, deadline time.Time) (held int64, ok bool) {
	noise := ^uint64(0)
	for time.Now().Before(deadline) {
		h, n := heapReading()
		if time.Now().After(deadline) {
			break
		}
		if n < noise {
			held, noise, ok = h, n, true
		}
		if noise <= slack {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	return held, ok
}

// heapReading runs a collection and returns the live heap after it, with
// what was allocated while it ran.
func heapReading() (held int64, noise uint64) {
	sample := []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}, {Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	before := sample[0].Value.Uint64()
	runtime.GC()
	metrics.Read(sample)
	return int64(sample[1].Value.Uint64()), sample[0].Value.Uint64() - before
}

// stallMax bounds how long the tests keep a connection's reader stopped.
// wsconn ends a connection whose single frame write does not finish within
// 10 seconds of starting, as it would for a peer that is gone. Progress
// does not renew that deadline.
const stallMax = 6 * time.Second

// catchUpMax bounds how long the first reader to stop at a point waits for
// the others, so that the measurement after it still ends within stallMax.
const catchUpMax = 3 * time.Second

// pauseAttempts is how many times a test starts its readers again after an
// attempt that could not be measured.
const pauseAttempts = 3

// pauses stops readers at the same points of their reading, so that the
// host can be measured while it holds what its readers have not taken yet.
// A reader that reaches a point stops there until the test resumes it, so
// no reader takes more, or finishes, while the host is measured.
//
// A reader stops for at most stallMax from the first stop at its point. A
// timer of the helper's own then resumes every reader, whether or not the
// test goes on, and the attempt is expired. A test that is late, or a
// measurement that is slow, therefore never keeps a connection stopped
// long enough for its sender to give it up.
//
// An attempt that cannot be measured is given up, and the test starts
// another: when a reader ended before a point it was to stop at, when the
// readers did not all stop within catchUpMax of the first, which would
// stall the first one's connection for too long, and when the attempt
// expired before its measurement was done. Giving up resumes every reader
// at once. A stopped reader shows only that it takes no more, so the test
// also checks from the host's side that the senders it measures still hold
// what they have not sent.
type pauses struct {
	at      []int // bytes read
	readers int
	resume  []chan struct{}
	off     chan struct{} // closed when the attempt is given up
	offOnce sync.Once
	expired atomic.Bool

	mu      sync.Mutex
	stopped []int         // readers stopped at each point
	first   []time.Time   // when the first of them stopped
	timers  []*time.Timer // the stall of each point
	ended   int           // readers that ended before their last point
}

func newPauses(readers int, at ...int) *pauses {
	p := &pauses{at: at, readers: readers, resume: make([]chan struct{}, len(at)), off: make(chan struct{}),
		stopped: make([]int, len(at)), first: make([]time.Time, len(at)), timers: make([]*time.Timer, len(at))}
	for i := range at {
		p.resume[i] = make(chan struct{})
	}
	return p
}

// wait returns true once every reader has stopped at point i. It returns
// false, and gives the attempt up, when
// a reader ended before point i, when the attempt expired, or when
// catchUpMax has passed since the first stop before wait saw every reader
// stopped.
func (p *pauses) wait(i int) bool {
	start := time.Now()
	for {
		p.mu.Lock()
		stopped, ended, first := p.stopped[i], p.ended, p.first[i]
		p.mu.Unlock()
		// The time since the first stop counts whether or not the others
		// have stopped, since the test itself can be late.
		switch {
		case ended > 0, p.expired.Load(),
			!first.IsZero() && time.Since(first) > catchUpMax,
			first.IsZero() && time.Since(start) > time.Minute:
			p.giveUp()
			return false
		case stopped == p.readers:
			return true
		}
		time.Sleep(time.Millisecond)
	}
}

// release lets the readers stopped at point i read on.
func (p *pauses) release(i int) {
	p.mu.Lock()
	if t := p.timers[i]; t != nil {
		t.Stop()
	}
	p.mu.Unlock()
	close(p.resume[i])
}

// giveUp ends the attempt, and every reader reads on to its end.
func (p *pauses) giveUp() {
	p.offOnce.Do(func() { close(p.off) })
	p.mu.Lock()
	for _, t := range p.timers {
		if t != nil {
			t.Stop()
		}
	}
	p.mu.Unlock()
}

// expire ends the attempt when a stall has lasted its time. The mark comes
// before the readers resume, so a measurement that finds no mark was taken
// while they were stopped.
func (p *pauses) expire() {
	p.expired.Store(true)
	p.giveUp()
}

// reader returns what a reader calls with the bytes it has read so far,
// and what it calls when it ends.
func (p *pauses) reader() (read func(got int), done func()) {
	next := 0
	read = func(got int) {
		for next < len(p.at) && got >= p.at[next] {
			p.mu.Lock()
			if p.stopped[next] == 0 {
				p.first[next] = time.Now()
				p.timers[next] = time.AfterFunc(stallMax, p.expire)
			}
			p.stopped[next]++
			p.mu.Unlock()
			select {
			case <-p.resume[next]:
			case <-p.off:
			}
			next++
		}
	}
	done = func() {
		if next < len(p.at) {
			p.mu.Lock()
			p.ended++
			p.mu.Unlock()
		}
	}
	return read, done
}

// heapWhileStopped reads the heap as quietHeap does while every reader is
// stopped at point i (wait), and then runs check, which looks from the
// host's side at what the senders still hold. It returns the reading, or
// why the attempt is given up: check failed, no collection ended before
// the stall of the point did, or the attempt expired before check was
// done, so that the readers may have read on while the host was looked at.
func (p *pauses) heapWhileStopped(i int, check func() (bool, string)) (held int64, why string) {
	p.mu.Lock()
	deadline := p.first[i].Add(stallMax)
	p.mu.Unlock()
	late := lateMeasurement.Swap(onTime)
	if late == lateBeforeReading {
		p.outlive()
	}
	held, ok := heapBefore(1<<20, deadline)
	if late == lateAfterReading {
		p.outlive()
	}
	switch {
	case !ok:
		why = "the heap could not be read before the readers' stall ended"
	case check != nil:
		if ok, failed := check(); !ok {
			why = failed
		}
	}
	if why == "" && p.expired.Load() {
		why = "the readers' stall ended before the host was looked at"
	}
	if why != "" {
		if p.expired.Load() {
			stallGiveUps.Add(1)
		}
		p.giveUp()
	}
	return held, why
}

// stallGiveUps counts the measurements given up because the readers'
// stall ended under them.
var stallGiveUps atomic.Int64

// lateMeasurement, when it is not onTime, delays the next measurement of
// paused readers, for the test of what an attempt does when its
// measurement outlives the readers' stall.
var lateMeasurement atomic.Int32

const (
	onTime = iota
	lateBeforeReading
	lateAfterReading
)

// outlive waits until the stall of the attempt has ended, as a test that
// is kept waiting does.
func (p *pauses) outlive() {
	for !p.expired.Load() {
		time.Sleep(time.Millisecond)
	}
}

// hostSends watches what the host sends while a test reads. It counts the
// bytes written to each WebSocket connection at the socket the connection
// was upgraded from, and the History tab responses still being written.
type hostSends struct {
	mu      sync.Mutex
	ws      []*atomic.Int64
	history atomic.Int64
}

func (hs *hostSends) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_sheet/ws"):
			w = &countedUpgrade{ResponseWriter: w, hs: hs}
		case strings.Contains(r.URL.Path, "/revisions/") && strings.Contains(r.URL.Path, "/tabs/"):
			hs.history.Add(1)
			defer hs.history.Add(-1)
		}
		next.ServeHTTP(w, r)
	})
}

// sockets returns the counts of the connections upgraded since the last
// call.
func (hs *hostSends) sockets() []*atomic.Int64 {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	list := hs.ws
	hs.ws = nil
	return list
}

type countedUpgrade struct {
	http.ResponseWriter
	hs *hostSends
}

func (u *countedUpgrade) Unwrap() http.ResponseWriter { return u.ResponseWriter }

func (u *countedUpgrade) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, brw, err := u.ResponseWriter.(http.Hijacker).Hijack()
	if err != nil {
		return nil, nil, err
	}
	n := new(atomic.Int64)
	u.hs.mu.Lock()
	u.hs.ws = append(u.hs.ws, n)
	u.hs.mu.Unlock()
	return countedConn{c, n}, brw, nil
}

type countedConn struct {
	net.Conn
	n *atomic.Int64
}

func (c countedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// TestSnapshotEncodingStaysInBudget loads a tab whose text takes six times
// its size as JSON, from several connections at once and through the
// History route, while the capture budget has room for their captures and
// little more. The readers stop together at three points of the snapshot
// while the host is measured, and every sender is still sending when a
// measurement ends. A connection has written too little to have queued its
// last frame, and every History response is still being written. The host
// then holds what it has built for them and not sent. That stays with what
// the budget and the send queues allow: no connection builds its frames
// before it may hold them, and History builds no response.
func TestSnapshotEncodingStaysInBudget(t *testing.T) {
	heavy(t, "encodes 18 MB of frames many times")
	e := start(t)
	// 100 cells of 30,000 control characters behind an equals sign: 3 MB
	// of text. A frame carries each as its value and as the text to type,
	// which has an apostrophe in front: 36 MB as JSON.
	const cells, chars = 100, 30000
	text := strings.Repeat(`\u0001`, chars)
	for row := 1; row <= cells; row += 25 {
		list := make([]string, 25)
		for i := range list {
			list[i] = `["'=` + text + `"]`
		}
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, row, strings.Join(list, ",")))
	}
	rev := e.b.importAll(t)
	sent := new(hostSends)
	s := serve(e, sent.wrap)
	tab := e.tabID("Sheet1")
	const frames = 2 * 6 * cells * chars
	// The revision workbook is built before anything is measured.
	historyURL := fmt.Sprintf("%s/private/_sheet/api/revisions/%d/tabs/%d", s.server.URL, rev, tab)
	// history reads the History tab, calling read with the bytes read so
	// far after each piece of the body.
	history := func(read func(int)) (int64, int) {
		for {
			res, err := cappedHTTP.Get(historyURL)
			if err != nil {
				t.Error(err)
				return 0, 0
			}
			var n int64
			for {
				m, err := io.CopyN(io.Discard, res.Body, 64<<10)
				if n += m; err != nil {
					break
				}
				read(int(n))
			}
			res.Body.Close()
			if res.StatusCode != http.StatusServiceUnavailable {
				return n, res.StatusCode
			}
			time.Sleep(5 * time.Millisecond) // calculating, or the budget is full
		}
	}
	if n, code := history(func(int) {}); code != http.StatusOK || n < frames {
		t.Fatalf("the History tab: status %d, %d bytes", code, n)
	}

	// One capture of the tab, to size the room.
	base := captureBytes.Load()
	if _, err := e.in.Read(context.Background(), ReadQuery{Tab: "Sheet1", Limit: 1}); err != nil {
		t.Fatal(err)
	}
	one := captureBytes.Load() - base
	e.in.captures.clear()
	const readers = 6
	room := (readers + 1) * (one + 2<<20)
	release := fillCaptures(t, room)
	defer release()

	// measure reads the tab from readers connections or History requests
	// at once and measures the host while they are stopped in it. It
	// reports false when the attempt could not be measured.
	measure := func(how string) bool {
		base := quietHeap(t, 1<<20)
		sent.sockets()
		// The last point leaves half of the frames unread, more than a send
		// queue and the sockets between the host and a reader take.
		stops := newPauses(readers, serverFrameMax, frames/4, frames/2)
		defer stops.giveUp()
		var wg sync.WaitGroup
		for i := 0; i < readers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				read, done := stops.reader()
				defer done()
				if how == "History" {
					if n, code := history(read); code != http.StatusOK || n < frames {
						t.Errorf("a History read: status %d, %d bytes", code, n)
					}
					return
				}
				host := strings.TrimPrefix(s.server.URL, "http://")
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				ws, err := wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) { return cappedDial(ctx, "tcp", host) }, host, "/public/_sheet/ws")
				if err != nil {
					t.Error(err)
					return
				}
				defer ws.Close(1000, "")
				_ = ws.Send(helloV1)
				_ = ws.Send(fmt.Sprintf(`{"type":"watch","generation":1,"tab":%d}`, tab))
				got := 0
				for {
					raw, err := ws.ReadMessage(64 << 20)
					if err != nil {
						t.Errorf("a connection ended after %d bytes of frames: %v", got, err)
						return
					}
					if len(raw) > serverFrameMax {
						t.Errorf("a frame of %d bytes", len(raw))
					}
					got += len(raw)
					if strings.HasPrefix(raw, `{"type":"tab_done"`) {
						break
					}
					read(got)
				}
				if got < frames {
					t.Errorf("a snapshot of %d bytes, want more than %d", got, frames)
				}
			}()
		}
		// What may be held: the budget's room, a send queue and a frame in
		// the hands of each reader, and some slack for the test itself.
		allowed := room + readers*(publicQueue+2*serverFrameMax) + 16<<20
		if how == "History" {
			allowed = room + readers*(2*serverFrameMax) + 16<<20
		}
		if int64(readers)*frames < 2*allowed {
			t.Fatalf("%s: the test allows %d bytes, too close to the %d that all frames take", how, allowed, readers*frames)
		}
		// sending reports whether the sender of every reader still has
		// frames to send. A connection that has queued its last frame has
		// written at least the snapshot less its send queue and the frame
		// its writer holds.
		var sockets []*atomic.Int64
		sending := func() (bool, string) {
			if how == "History" {
				n := sent.history.Load()
				return n == readers, fmt.Sprintf("%d of %d History responses were being written", n, readers)
			}
			if sockets == nil {
				sockets = sent.sockets()
			}
			if len(sockets) != readers {
				return false, fmt.Sprintf("%d connections were upgraded, want %d", len(sockets), readers)
			}
			for _, n := range sockets {
				if written := n.Load(); written+publicQueue+2*serverFrameMax >= frames {
					return false, fmt.Sprintf("a connection had written %d of the %d bytes of the snapshot", written, frames)
				}
			}
			return true, ""
		}
		measured := true
		for i, at := range stops.at {
			if !stops.wait(i) {
				t.Logf("%s: the readers did not all stop after %d bytes in time; the attempt is given up", how, at)
				measured = false
				break
			}
			reading, why := stops.heapWhileStopped(i, sending)
			if why != "" {
				t.Logf("%s: after %d bytes %s; the attempt is given up", how, at, why)
				measured = false
				break
			}
			held := reading - base
			t.Logf("%s: with every reader stopped after %d bytes and every sender sending the heap holds %d bytes over its start", how, at, held)
			if held > allowed {
				t.Errorf("%s: with every reader stopped after %d bytes and every sender sending the heap holds %d bytes over its start; the budget's room is %d and %d are allowed. %d readers of %d bytes of frames each", how, at, held, room, allowed, readers, frames)
			}
			stops.release(i)
		}
		wg.Wait()
		return measured
	}
	for _, how := range []string{"connections", "History"} {
		for attempt := 1; !measure(how); attempt++ {
			if attempt == pauseAttempts {
				t.Fatalf("%s: none of %d attempts could be measured", how, attempt)
			}
		}
	}
	if n := encoderGrowths.Load(); n != 0 {
		t.Fatalf("%d encoder buffers outgrew their reservation", n)
	}
}

// TestSnapshotCacheLimitAcrossSheets has two sheets encode a snapshot at
// the same moment while the host-wide cache has room for one and a half of
// them. However the two interleave, the cache never counts more than its
// limit, and what it counts is what the sheets keep.
func TestSnapshotCacheLimitAcrossSheets(t *testing.T) {
	// The reservation itself: of many that race for the last room, exactly
	// those that fit succeed.
	base := snapshotCached.Load()
	const piece = 1 << 20
	var granted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 256; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if reserveSnapshotCache(piece) {
				granted.Add(piece)
			}
		}()
	}
	wg.Wait()
	if got, want := granted.Load(), (snapshotCacheMax-base)/piece*piece; got != want || snapshotCached.Load() != base+got {
		t.Fatalf("%d bytes were granted, want %d; the cache counts %d", got, want, snapshotCached.Load()-base)
	}
	snapshotCached.Add(-granted.Load())

	sheets := []*env{start(t), start(t)}
	var sites []*site
	for _, e := range sheets {
		for row := 1; row <= 60; row += 20 {
			list := make([]string, 20)
			for i := range list {
				list[i] = `["` + strings.Repeat("c", 30000) + `"]`
			}
			e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, row, strings.Join(list, ",")))
		}
		sites = append(sites, serve(e))
	}
	// One snapshot alone, to learn its size in the cache.
	first := sites[0].dial("private", helloV1)
	first.next("welcome")
	first.watch(1, sheets[0].tabID("Sheet1"))
	one := snapshotCached.Load() - base
	if one < 60*30000 {
		t.Fatalf("a cached snapshot counts %d bytes", one)
	}
	// Other sheets hold the cache up to one and a half snapshots below its
	// limit.
	sheets[0].mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[["drop"]]}]}`)
	if got := snapshotCached.Load(); got != base {
		t.Fatalf("after the edit the cache counts %d bytes", got-base)
	}
	filler := snapshotCacheMax - base - one*3/2
	snapshotCached.Add(filler)
	t.Cleanup(func() { snapshotCached.Add(-filler) })

	for round := 0; round < 20; round++ {
		// Both sheets get an edit, which drops their cached frames, and
		// then stop at the moment they start to encode.
		var reached sync.WaitGroup
		gate := make(chan struct{})
		reached.Add(len(sheets))
		for _, e := range sheets {
			e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[[%d]]}]}`, round))
			hookOnce(e, map[string]func(){"snapshot_stream": func() {
				reached.Done()
				<-gate
			}})
		}
		var peak atomic.Int64
		stop, sampled := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(sampled)
			for {
				peak.Store(max(peak.Load(), snapshotCached.Load()))
				select {
				case <-stop:
					return
				default:
					runtime.Gosched()
				}
			}
		}()
		var done sync.WaitGroup
		for i, e := range sheets {
			done.Add(1)
			go func() {
				defer done.Done()
				c := sites[i].dial("private", helloV1)
				c.next("welcome")
				if _, cells := c.watch(1, e.tabID("Sheet1")); len(cells) != 61 {
					t.Errorf("round %d: a snapshot of %d cells", round, len(cells))
				}
				c.ws.Close(1000, "")
			}()
		}
		reached.Wait()
		close(gate)
		done.Wait()
		close(stop)
		<-sampled
		if got := peak.Load(); got > snapshotCacheMax {
			t.Fatalf("round %d: the cache counted %d bytes, %d above its limit", round, got, got-snapshotCacheMax)
		}
		kept := int64(0)
		for _, e := range sheets {
			e.in.hub.mu.Lock()
			for _, entry := range e.in.hub.snaps {
				kept += entry.bytes
			}
			e.in.hub.mu.Unlock()
		}
		if got := snapshotCached.Load() - base - filler; got != kept || kept > one*3/2 {
			t.Fatalf("round %d: the cache counts %d bytes and the sheets keep %d", round, got, kept)
		}
	}
}

// TestMeasurementThatOutlivesTheStallIsGivenUp runs the tests that measure
// the host while its readers are stopped, and keeps the first measurement
// of each waiting until the readers' stall has ended: before the heap is
// read, or after the heap is read and before the host is looked at. The
// stall's end resumes the readers on its own, so no connection waits long
// enough for its sender to give it up, and the late attempt is given up
// and not reported as a failure. A later attempt is measured, and each
// test passes.
func TestMeasurementThatOutlivesTheStallIsGivenUp(t *testing.T) {
	heavy(t, "waits out the stall of three measurements")
	t.Cleanup(func() { lateMeasurement.Store(onTime) })
	for _, tc := range []struct {
		name string
		test func(*testing.T)
		when int32
	}{
		{"snapshot, late before the reading", TestSnapshotEncodingStaysInBudget, lateBeforeReading},
		{"shared delta, late after the reading", TestLargeDeltaIsSharedByItsSenders, lateAfterReading},
		{"revoked delta, late before the reading", TestHeldDeltaIsChargedAfterTheLog, lateBeforeReading},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lateMeasurement.Store(tc.when)
			given := stallGiveUps.Load()
			tc.test(t)
			if lateMeasurement.Load() != onTime {
				t.Fatal("the late measurement was not taken")
			}
			if stallGiveUps.Load() == given {
				t.Fatal("no measurement was given up when the stall ended")
			}
		})
	}
}
