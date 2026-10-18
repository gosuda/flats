package core

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosuda/flats/internal/store"
)

// Flat usage: request counts, response times and worker CPU and memory of a
// flat's live route (docs/spec/metrics.md, docs/internal/metrics.md).
// Counting a request only touches atomics and a short in-memory buffer;
// Sweep persists the buffer per UTC day, and the request path never waits for
// SQLite.

const usageDayLayout = "2006-01-02"

// usageLatencyVersion names UsageBuckets in the stored latency encoding. Rows
// of another version keep their own buckets: percentiles use only rows of
// this version, and a day stored with an older one takes this one's buckets
// on the next flush with timed requests (store.UsageDay.Add).
const usageLatencyVersion = 2

// usageBucketCount is the number of histogram buckets, +Inf included.
var usageBucketCount = len(UsageBuckets) + 1

const (
	routePrivate = 0
	routePublic  = 1
)

var usageRoutes = [2]string{RoutePrivate, RoutePublic}

// Class indexes, in UsageClasses order.
const (
	class1xx = iota
	class2xx
	class3xx
	class4xx
	class5xx
	classClosed
	classCount
)

func statusClass(code int) int {
	switch {
	case code < 200:
		return class1xx
	case code < 300:
		return class2xx
	case code < 400:
		return class3xx
	case code < 500:
		return class4xx
	}
	return class5xx
}

// usageBucket returns the histogram bucket of a response time.
func usageBucket(d time.Duration) int {
	sec := d.Seconds()
	for i, b := range UsageBuckets {
		if sec <= b {
			return i
		}
	}
	return len(UsageBuckets)
}

// usageTally is one day of a flat's usage, in memory.
type usageTally struct {
	classes     [classCount]int64
	rateLimited int64
	timed       int64
	durationNS  int64
	buckets     []int64
	cpuNS       int64
	memPeak     int64
}

func (t *usageTally) add(o *usageTally) {
	for i := range t.classes {
		t.classes[i] += o.classes[i]
	}
	t.rateLimited += o.rateLimited
	t.timed += o.timed
	t.durationNS += o.durationNS
	if len(o.buckets) > 0 {
		if t.buckets == nil {
			t.buckets = make([]int64, usageBucketCount)
		}
		for i := range o.buckets {
			t.buckets[i] += o.buckets[i]
		}
	}
	t.cpuNS += o.cpuNS
	t.memPeak = max(t.memPeak, o.memPeak)
}

func (t *usageTally) row(day string) store.UsageDay {
	d := store.UsageDay{Day: day, C1xx: t.classes[class1xx], C2xx: t.classes[class2xx], C3xx: t.classes[class3xx],
		C4xx: t.classes[class4xx], C5xx: t.classes[class5xx], Closed: t.classes[classClosed], RateLimited: t.rateLimited,
		CPUMS: t.cpuNS / int64(time.Millisecond), MemoryPeak: t.memPeak}
	if t.timed > 0 {
		d.Timed, d.DurationMS = t.timed, (t.durationNS+int64(time.Millisecond)/2)/int64(time.Millisecond)
		d.LatencyVersion, d.Latency = usageLatencyVersion, append([]int64(nil), t.buckets...)
	}
	return d
}

// flatUsage is the usage of one live flat. It hangs off liveFlat, which a
// rename keeps, so counters continue under the new slug.
type flatUsage struct {
	now func() time.Time

	// Cumulative since the host started, for /metrics.
	requests      [2][classCount]atomic.Uint64
	rateLimited   atomic.Uint64
	responseBytes atomic.Uint64
	openStreams   atomic.Int64
	durCounts     []atomic.Uint64
	durSumNS      atomic.Int64
	durCount      atomic.Uint64

	// worker is the CPU and memory of the workers serving the live route.
	worker *workerUsage

	// mu guards the day buffer only. It is held to add to it or swap it out,
	// never while SQLite is written.
	mu   sync.Mutex
	days map[string]*usageTally
	// cpuSeenNS is the part of worker CPU already moved into the day buffer.
	cpuSeenNS int64
	// peakMax is the highest memory peak moved into the day buffer for the
	// UTC day peakDay. A sample no higher changes nothing, so a worker with
	// steady memory and no CPU growth writes no row (a host restart starts
	// over and may write the day's peak once more).
	peakDay string
	peakMax int64
}

func newFlatUsage(now func() time.Time) *flatUsage {
	return &flatUsage{now: now, durCounts: make([]atomic.Uint64, usageBucketCount), worker: newWorkerUsage(now), days: map[string]*usageTally{}}
}

func (u *flatUsage) day(key string) *usageTally {
	t := u.days[key]
	if t == nil {
		t = &usageTally{}
		u.days[key] = t
	}
	return t
}

func (u *flatUsage) today() string { return u.now().UTC().Format(usageDayLayout) }

// countRequest counts one request of class on route, on day.
func (u *flatUsage) countRequest(route, class int, day string) {
	u.requests[route][class].Add(1)
	u.mu.Lock()
	u.day(day).classes[class]++
	u.mu.Unlock()
}

func (u *flatUsage) countRateLimited(day string) {
	u.rateLimited.Add(1)
	u.mu.Lock()
	u.day(day).rateLimited++
	u.mu.Unlock()
}

// observe records the response time of a completed, non-streaming request.
func (u *flatUsage) observe(d time.Duration, day string) {
	d = max(d, 0)
	i := usageBucket(d)
	u.durCounts[i].Add(1)
	u.durSumNS.Add(int64(d))
	u.durCount.Add(1)
	u.mu.Lock()
	t := u.day(day)
	if t.buckets == nil {
		t.buckets = make([]int64, usageBucketCount)
	}
	t.buckets[i]++
	t.timed++
	t.durationNS += int64(d)
	u.mu.Unlock()
}

// capture moves worker CPU used since the last capture, in whole
// milliseconds, and the highest memory sampled since then, when it exceeds
// the peak already recorded for today, into today's buffer. A CPU increase
// below zero is clamped to zero.
func (u *flatUsage) capture() {
	total := u.worker.cpuNS()
	peak, peaked := u.worker.takePeak()
	u.mu.Lock()
	defer u.mu.Unlock()
	// total never decreases, but two concurrent captures can read it in one
	// order and lock in the other: an older, smaller read adds nothing
	// rather than rewinding cpuSeenNS, which would count the gap twice
	// (newCPUMS).
	ms := u.newCPUMS(total)
	day := u.today()
	if peaked && day == u.peakDay && peak <= u.peakMax {
		peaked = false
	}
	if ms <= 0 && !peaked {
		return
	}
	t := u.day(day)
	if ms > 0 {
		t.cpuNS += ms * int64(time.Millisecond)
		u.cpuSeenNS += ms * int64(time.Millisecond)
	}
	if peaked {
		t.memPeak = max(t.memPeak, peak)
		u.peakDay, u.peakMax = day, peak
	}
}

// newCPUMS is the worker CPU of total not yet moved into the day buffer, in
// whole milliseconds. The caller holds mu.
func (u *flatUsage) newCPUMS(total int64) int64 {
	if total <= u.cpuSeenNS {
		return 0
	}
	return (total - u.cpuSeenNS) / int64(time.Millisecond)
}

// pending returns a copy of the day buffer plus what a capture would add now:
// the CPU not captured yet and the memory peak not taken yet. It moves
// nothing, so reading it never changes what the next flush writes.
func (u *flatUsage) pending() map[string]*usageTally {
	total := u.worker.cpuNS()
	peak, peaked := u.worker.peekPeak()
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make(map[string]*usageTally, len(u.days)+1)
	for k, t := range u.days {
		c := *t
		c.buckets = append([]int64(nil), t.buckets...)
		out[k] = &c
	}
	ms := u.newCPUMS(total)
	if ms <= 0 && !peaked {
		return out
	}
	day := u.today()
	t := out[day]
	if t == nil {
		t = &usageTally{}
		out[day] = t
	}
	t.cpuNS += ms * int64(time.Millisecond)
	if peaked {
		t.memPeak = max(t.memPeak, peak)
	}
	return out
}

// swap takes the day buffer out.
func (u *flatUsage) swap() map[string]*usageTally {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.days) == 0 {
		return nil
	}
	days := u.days
	u.days = map[string]*usageTally{}
	return days
}

// merge puts back a buffer whose write failed.
func (u *flatUsage) merge(days map[string]*usageTally) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for k, t := range days {
		u.day(k).add(t)
	}
}

// workerUsage implements UsageSink: the CPU and memory of the worker processes
// of one flat and role. A flat's CPU total is the final CPU of exited
// processes plus the latest sample of each running one; an exiting process
// moves from one to the other in one critical section, so a reader sees each
// process exactly once and the total never decreases.
type workerUsage struct {
	now func() time.Time
	// owner is the flat whose live workers report here; nil for role sinks.
	owner *liveFlat

	mu         sync.Mutex
	running    map[uint64]*procUsage
	exitedCPU  int64
	started    uint64
	unexpected uint64
	// emptySince is when the last running process exited.
	emptySince time.Time
	// peak is the highest memory total sampled since takePeak.
	peak   int64
	peaked bool
}

type procUsage struct {
	cpu   int64
	mem   int64
	memOK bool
	// memAt is when mem was sampled, or the start before the first sample
	// with memory. A sample without memory leaves mem and memAt as they
	// were, so a failed read ages the last value instead of replacing it.
	memAt time.Time
}

var _ UsageSink = (*workerUsage)(nil)

func newWorkerUsage(now func() time.Time) *workerUsage {
	return &workerUsage{now: now, running: map[uint64]*procUsage{}}
}

func (w *workerUsage) Started(id uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.running[id]; !ok {
		w.running[id] = &procUsage{memAt: w.now()}
		w.started++
	}
}

func (w *workerUsage) Sample(id uint64, cpuNS, mem int64, memOK bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.running[id]
	if p == nil || cpuNS < 0 || (memOK && mem < 0) {
		return
	}
	p.cpu = max(p.cpu, cpuNS)
	if memOK {
		p.mem, p.memOK, p.memAt = mem, true, w.now()
		var sum int64
		for _, q := range w.running {
			if q.memOK {
				sum += q.mem
			}
		}
		if !w.peaked || sum > w.peak {
			w.peak, w.peaked = sum, true
		}
	}
}

func (w *workerUsage) Exited(id uint64, cpuNS int64, unexpected bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	final := max(cpuNS, 0)
	if p := w.running[id]; p != nil {
		final = max(final, p.cpu)
		delete(w.running, id)
	}
	w.exitedCPU += final
	if unexpected {
		w.unexpected++
	}
	if len(w.running) == 0 {
		w.emptySince = w.now()
	}
}

// cpuNS is the total CPU time of every process of this sink.
func (w *workerUsage) cpuNS() int64 {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	n := w.exitedCPU
	for _, p := range w.running {
		n += p.cpu
	}
	return n
}

// used reports whether a process of this sink ever started.
func (w *workerUsage) used() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.started > 0
}

func (w *workerUsage) unexpectedExits() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.unexpected
}

// workerMemory is the memory the running processes hold now.
type workerMemory struct {
	bytes   int64
	workers int
	// at is the oldest latest memory sample of a running process (its start
	// if it sent none yet); with none running it is when the last one exited
	// (zero when none ever ran).
	at time.Time
}

func (w *workerUsage) memory() workerMemory {
	w.mu.Lock()
	defer w.mu.Unlock()
	m := workerMemory{workers: len(w.running), at: w.emptySince}
	first := true
	for _, p := range w.running {
		if p.memOK {
			m.bytes += p.mem
		}
		if first || p.memAt.Before(m.at) {
			m.at, first = p.memAt, false
		}
	}
	return m
}

func (w *workerUsage) takePeak() (int64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.peak, w.peaked
	w.peak, w.peaked = 0, false
	return p, ok
}

// peekPeak is takePeak without taking it.
func (w *workerUsage) peekPeak() (int64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.peak, w.peaked
}

// memoryStaleAfter: a running worker's memory sample older than this is
// reported with its time and flagged stale.
const memoryStaleAfter = 60 * time.Second

// --- liveFlat binding ---

// liveSink is the usage sink of the flat's live workers.
func (lf *liveFlat) liveSink() UsageSink {
	if lf == nil || lf.usage == nil {
		return nil
	}
	return lf.usage.worker
}

// liveFlatOf returns the flat whose live sink usage is, else nil.
func liveFlatOf(usage UsageSink) *liveFlat {
	if w, ok := usage.(*workerUsage); ok && w != nil {
		return w.owner
	}
	return nil
}

func (lf *liveFlat) previewSink() UsageSink {
	if lf == nil || lf.previewCPU == nil {
		return nil
	}
	return lf.previewCPU
}

func (lf *liveFlat) checkSink() UsageSink {
	if lf == nil || lf.checkCPU == nil {
		return nil
	}
	return lf.checkCPU
}

// bindUID records the stored flat this state serves the first time it is
// bound. Usage is flushed under that ID only.
func (lf *liveFlat) bindUID(uid string) {
	if lf == nil || uid == "" {
		return
	}
	lf.uid.CompareAndSwap(nil, &uid)
}

func (lf *liveFlat) uidOf() string {
	if lf == nil {
		return ""
	}
	if p := lf.uid.Load(); p != nil {
		return *p
	}
	return ""
}

// flushUsage persists the buffered usage of lfs under their bound flat IDs,
// in one transaction. Each buffer is swapped out before SQLite is written and
// every one is merged back if the write fails, so nothing is lost or counted
// twice; usage of a flat that no longer exists is discarded. usageMu
// serializes the flush with the reads of Usage; the request path never takes
// it.
func (s *Service) flushUsage(ctx context.Context, lfs ...*liveFlat) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	type swapped struct {
		u    *flatUsage
		days map[string]*usageTally
	}
	var batches []store.UsageBatch
	var taken []swapped
	for _, lf := range lfs {
		if lf == nil || lf.usage == nil {
			continue
		}
		uid := lf.uidOf()
		if uid == "" {
			continue
		}
		lf.usage.capture()
		days := lf.usage.swap()
		if len(days) == 0 {
			continue
		}
		rows := make([]store.UsageDay, 0, len(days))
		for day, t := range days {
			rows = append(rows, t.row(day))
		}
		batches = append(batches, store.UsageBatch{UID: uid, Days: rows})
		taken = append(taken, swapped{lf.usage, days})
	}
	if len(batches) == 0 {
		return
	}
	if _, err := s.st.AddUsageBatch(ctx, batches); err != nil {
		for _, t := range taken {
			t.u.merge(t.days)
		}
		s.logf("flush usage of %d flats: %v", len(batches), err)
	}
}
