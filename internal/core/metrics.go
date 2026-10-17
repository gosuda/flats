package core

import (
	"context"
	"math"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/contenttype"
)

// Host metrics (docs/spec/metrics.md): Sweep refreshes the host-wide figures
// into a hostSnapshot; Metrics combines it with the live flats' in-memory
// counters and never reads SQLite, walks files or touches workers or
// providers.

type hostSnapshot struct {
	at      time.Time
	dbBytes int64
	pending int
	flats   []snapshotFlat
}

type snapshotFlat struct {
	slug, uid, visibility, typ, kind string
}

type diskSample struct {
	bytes int64
	at    time.Time
}

// diskEvery is how old a flat's disk figure may get; Sweep spreads the walks
// over the sweeps of that period.
const diskEvery = 5 * time.Minute

func (s *Service) liveFlats() map[string]*liveFlat {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*liveFlat, len(s.live))
	for k, v := range s.live {
		out[k] = v
	}
	return out
}

// flatTypes reads every flat's ID, visibility and type in one query (nil when
// it fails) and binds the IDs of live flats that have none yet. A state is
// bound only while it is still the one serving that slug: a delete removes it
// from s.live before the slug can be created again.
func (s *Service) flatTypes(ctx context.Context, live map[string]*liveFlat) []snapshotFlat {
	rows, err := s.st.FlatTypes(ctx)
	if err != nil {
		s.logf("metrics snapshot: %v", err)
		return nil
	}
	out := make([]snapshotFlat, 0, len(rows))
	for _, ft := range rows {
		typ := contenttype.Flat
		if len(ft.Manifest) > 0 {
			typ = contenttype.FromManifest(ft.Manifest)
		}
		out = append(out, snapshotFlat{slug: ft.Slug, uid: ft.UID, visibility: string(ft.Visibility.Canonical()), typ: typ, kind: ft.Kind})
		if lf := live[ft.Slug]; lf != nil && lf.uidOf() == "" && ft.UID != "" {
			s.mu.Lock()
			if s.live[ft.Slug] == lf {
				lf.bindUID(ft.UID)
			}
			s.mu.Unlock()
		}
	}
	return out
}

// storeSnapshot publishes the host-wide figures of this sweep. A failed flat
// query keeps the previous snapshot, so its time shows the stalled refresh.
func (s *Service) storeSnapshot(ctx context.Context, flats []snapshotFlat) {
	if flats == nil {
		return
	}
	prev := s.snapshot.Load()
	snap := &hostSnapshot{flats: flats, dbBytes: s.st.Size()}
	if n, err := s.st.PendingApprovals(ctx); err == nil {
		snap.pending = n
	} else if prev != nil {
		snap.pending = prev.pending
	}
	uids := make(map[string]bool, len(flats))
	for _, f := range flats {
		uids[f.uid] = true
	}
	s.diskMu.Lock()
	for uid := range s.disk {
		if !uids[uid] {
			delete(s.disk, uid)
		}
	}
	s.diskMu.Unlock()
	snap.at = s.now()
	s.snapshot.Store(snap)
}

func (s *Service) recordDisk(uid string, bytes int64) {
	if uid == "" {
		return
	}
	s.diskMu.Lock()
	defer s.diskMu.Unlock()
	if s.disk == nil {
		s.disk = map[string]diskSample{}
	}
	s.disk[uid] = diskSample{bytes: bytes, at: s.now()}
}

func (s *Service) diskOf(uid string) diskSample {
	s.diskMu.Lock()
	defer s.diskMu.Unlock()
	return s.disk[uid]
}

// walkDisks measures the flats whose disk figure is missing or close to
// diskEvery old, oldest first, at most a ninth of all flats per sweep, so
// every flat is walked at most once per period and the walks are spread over
// its sweeps.
func (s *Service) walkDisks(flats []snapshotFlat) {
	if len(flats) == 0 {
		return
	}
	now := s.now()
	type due struct {
		f  snapshotFlat
		at time.Time
	}
	var todo []due
	s.diskMu.Lock()
	for _, f := range flats {
		if f.uid == "" {
			continue
		}
		d, ok := s.disk[f.uid]
		if !ok || now.Sub(d.at) >= diskEvery-diskEvery/10 {
			todo = append(todo, due{f, d.at})
		}
	}
	s.diskMu.Unlock()
	slices.SortFunc(todo, func(a, b due) int { return a.at.Compare(b.at) })
	budget := max(1, (len(flats)+8)/9)
	for _, d := range todo[:min(len(todo), budget)] {
		s.recordDisk(d.f.uid, dirSize(s.flatDir(d.f.slug)))
	}
}

// hasWorker reports whether a flat of this type and kind runs in its own
// worker process: server flats and documents. Websites, pages and designs run
// inside the host process.
func hasWorker(typ, kind string) bool {
	return kind == "server" && typ != contenttype.Design
}

func workerStateOf(d *deployed) WorkerState {
	if d == nil || d.inst == nil {
		return WorkerStopped
	}
	if r, ok := d.inst.(StateReporter); ok {
		if st := r.WorkerState(); st != "" {
			return st
		}
	}
	return WorkerRunning
}

// memoryMeasured: worker memory (RssAnon) is measured on Linux only.
var memoryMeasured = runtime.GOOS == "linux"

// Metrics returns everything /metrics exposes from core, from memory only.
// Per-flat series exist for the flats of the last snapshot and for live flats
// with a bound ID and a current version; the empty state s.state can create
// for any slug never becomes a series.
func (s *Service) Metrics() MetricsSnapshot {
	var out MetricsSnapshot
	snap := s.snapshot.Load()
	if snap == nil {
		snap = &hostSnapshot{}
	}
	out.SnapshotAt, out.DBBytes, out.PendingApprovals = snap.at, snap.dbBytes, snap.pending
	live := s.liveFlats()
	bySlug := make(map[string]snapshotFlat, len(snap.flats))
	for _, f := range snap.flats {
		bySlug[f.slug] = f
	}
	emitted := map[string]bool{} // flat IDs
	running := map[string]bool{} // slugs of included live flats with a current version
	for slug, lf := range live {
		cur := lf.cur.Load()
		sf, inSnap := bySlug[slug]
		uid := lf.uidOf()
		if !(inSnap && (uid == "" || uid == sf.uid)) && !(uid != "" && cur != nil) {
			continue
		}
		if uid == "" {
			uid = sf.uid
		}
		typ, kind := sf.typ, sf.kind
		if cur != nil {
			typ, kind = contenttype.FromManifest(cur.version.Manifest), cur.version.Kind
			running[slug] = true
		} else if !inSnap {
			typ = contenttype.Flat
		}
		out.Flats = append(out.Flats, s.flatMetrics(slug, typ, kind, uid, lf, cur))
		emitted[uid] = true
		for _, role := range []struct {
			name string
			w    *workerUsage
		}{{UsageRolePreview, lf.previewCPU}, {UsageRoleCheck, lf.checkCPU}} {
			if role.w.used() {
				out.RoleCPU = append(out.RoleCPU, RoleCPU{Slug: slug, Type: typ, Role: role.name, CPUSeconds: float64(role.w.cpuNS()) / 1e9})
			}
		}
	}
	counts := map[FlatCount]int{}
	for _, f := range snap.flats {
		typ := f.typ
		isRunning := running[f.slug]
		if lf := live[f.slug]; isRunning && lf != nil {
			if cur := lf.cur.Load(); cur != nil {
				typ = contenttype.FromManifest(cur.version.Manifest)
			}
		}
		counts[FlatCount{Type: typ, Visibility: f.visibility, Running: isRunning}]++
		if emitted[f.uid] {
			continue
		}
		// No live state (a rename in progress, a flat just deleted): only its
		// disk figures are known.
		d := s.diskOf(f.uid)
		out.Flats = append(out.Flats, FlatMetrics{Slug: f.slug, Type: f.typ, DiskBytes: d.bytes, DiskMeasuredAt: d.at})
	}
	for c, n := range counts {
		c.N = n
		out.Counts = append(out.Counts, c)
	}
	slices.SortFunc(out.Counts, func(a, b FlatCount) int {
		if c := strings.Compare(a.Type, b.Type); c != 0 {
			return c
		}
		if c := strings.Compare(a.Visibility, b.Visibility); c != 0 {
			return c
		}
		return boolCompare(a.Running, b.Running)
	})
	slices.SortFunc(out.Flats, func(a, b FlatMetrics) int { return strings.Compare(a.Slug, b.Slug) })
	slices.SortFunc(out.RoleCPU, func(a, b RoleCPU) int {
		if c := strings.Compare(a.Slug, b.Slug); c != 0 {
			return c
		}
		return strings.Compare(a.Role, b.Role)
	})
	return out
}

func boolCompare(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	}
	return 1
}

func (s *Service) flatMetrics(slug, typ, kind, uid string, lf *liveFlat, cur *deployed) FlatMetrics {
	fm := FlatMetrics{Slug: slug, Type: typ, Traffic: true}
	if u := lf.usage; u != nil {
		fm.Requests = make(map[string]map[string]uint64, len(usageRoutes))
		for r, route := range usageRoutes {
			byClass := make(map[string]uint64, classCount)
			for c, class := range UsageClasses {
				byClass[class] = u.requests[r][c].Load()
			}
			fm.Requests[route] = byClass
		}
		fm.RateLimited = u.rateLimited.Load()
		fm.ResponseBytes = u.responseBytes.Load()
		fm.OpenStreams = u.openStreams.Load()
		fm.DurationCounts = make([]uint64, usageBucketCount)
		for i := range fm.DurationCounts {
			fm.DurationCounts[i] = u.durCounts[i].Load()
		}
		fm.DurationSum = float64(u.durSumNS.Load()) / 1e9
		fm.DurationCount = u.durCount.Load()
		if hasWorker(typ, kind) {
			w := u.worker
			mem := w.memory()
			fm.HasWorker = true
			fm.CPUSeconds = float64(w.cpuNS()) / 1e9
			fm.MemoryOK = memoryMeasured
			if memoryMeasured {
				fm.MemoryBytes, fm.MemorySampledAt = mem.bytes, mem.at
			}
			fm.State = workerStateOf(cur)
			fm.UnexpectedExits = w.unexpectedExits()
		}
	}
	d := s.diskOf(uid)
	fm.DiskBytes, fm.DiskMeasuredAt = d.bytes, d.at
	return fm
}

// --- stats API ---

// UsageReport is the "usage" object of a flat's stats
// (docs/spec/metrics-TECH.md, Stats API).
type UsageReport struct {
	Days   []UsageDayView `json:"days"`
	Totals UsageTotals    `json:"totals"`
	Now    UsageNow       `json:"now"`
}

// UsageDayView is one UTC day with usage. CPU and memory are omitted for
// types that run inside the Flats process.
type UsageDayView struct {
	Day             string           `json:"day"`
	Requests        int64            `json:"requests"`
	Status          map[string]int64 `json:"status"`
	RateLimited     int64            `json:"rate_limited"`
	CPUSeconds      *float64         `json:"cpu_seconds,omitempty"`
	MemoryPeakBytes *int64           `json:"memory_peak_bytes,omitempty"`
}

// UsageTotals sums the period. ErrorRate counts 5xx only and is null without
// requests; the percentiles are null when no request was timed.
type UsageTotals struct {
	Requests        int64    `json:"requests"`
	Errors          int64    `json:"errors"`
	ErrorRate       *float64 `json:"error_rate"`
	P50MS           *float64 `json:"p50_ms"`
	P95MS           *float64 `json:"p95_ms"`
	CPUSeconds      *float64 `json:"cpu_seconds,omitempty"`
	MemoryPeakBytes *int64   `json:"memory_peak_bytes,omitempty"`
}

// Measurement states of UsageNow.CPU and UsageNow.Memory.
const (
	UsageMeasured    = "measured"
	UsageNotSeparate = "not_separate"
	UsageUnsupported = "unsupported"
)

// UsageNow is the flat's current worker figures, from memory.
type UsageNow struct {
	CPU             string      `json:"cpu"`
	Memory          string      `json:"memory"`
	State           WorkerState `json:"state,omitempty"`
	Workers         *int        `json:"workers,omitempty"`
	MemoryBytes     *int64      `json:"memory_bytes,omitempty"`
	MemorySampledAt *time.Time  `json:"memory_sampled_at,omitempty"`
	MemoryStale     *bool       `json:"memory_stale,omitempty"`
}

// Usage returns a flat's usage for the last days UTC days, today included.
// It flushes the flat's buffered usage first. It never wakes or starts a
// worker.
func (s *Service) Usage(ctx context.Context, slugName string, days int) (UsageReport, error) {
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return UsageReport{}, err
	}
	s.mu.Lock()
	lf := s.live[slugName]
	s.mu.Unlock()
	if lf != nil {
		lf.bindUID(f.ID)
		if lf.uidOf() == f.ID {
			s.flushUsage(ctx, lf)
		} else {
			lf = nil
		}
	}
	var cur *deployed
	if lf != nil {
		cur = lf.cur.Load()
	}
	var typ, kind string
	if cur != nil {
		typ, kind = contenttype.FromManifest(cur.version.Manifest), cur.version.Kind
	} else {
		ft, err := s.st.FlatTypeOf(ctx, slugName)
		if err != nil {
			return UsageReport{}, err
		}
		typ, kind = contenttype.Flat, ft.Kind
		if len(ft.Manifest) > 0 {
			typ = contenttype.FromManifest(ft.Manifest)
		}
	}
	since := s.now().UTC().AddDate(0, 0, 1-days).Format(usageDayLayout)
	rows, err := s.st.UsageSince(ctx, f.ID, since)
	if err != nil {
		return UsageReport{}, err
	}
	worker := hasWorker(typ, kind)
	rep := UsageReport{Days: make([]UsageDayView, 0, len(rows))}
	buckets := make([]int64, usageBucketCount)
	var cpuMS, peak int64
	for _, row := range rows {
		day := UsageDayView{Day: row.Day, Requests: row.Requests(), RateLimited: row.RateLimited, Status: map[string]int64{}}
		for class, n := range map[string]int64{Class1xx: row.C1xx, Class2xx: row.C2xx, Class3xx: row.C3xx, Class4xx: row.C4xx, Class5xx: row.C5xx, ClassClosed: row.Closed} {
			if n > 0 {
				day.Status[class] = n
			}
		}
		if worker {
			cpu, mem := float64(row.CPUMS)/1000, row.MemoryPeak
			day.CPUSeconds, day.MemoryPeakBytes = &cpu, &mem
		}
		rep.Days = append(rep.Days, day)
		rep.Totals.Requests += day.Requests
		rep.Totals.Errors += row.C5xx
		cpuMS += row.CPUMS
		peak = max(peak, row.MemoryPeak)
		if row.LatencyVersion == usageLatencyVersion && len(row.Latency) == usageBucketCount {
			for i, n := range row.Latency {
				buckets[i] += n
			}
		}
	}
	if rep.Totals.Requests > 0 {
		rate := math.Round(float64(rep.Totals.Errors)/float64(rep.Totals.Requests)*1e4) / 1e4
		rep.Totals.ErrorRate = &rate
	}
	if v, ok := usageQuantile(0.5, buckets); ok {
		rep.Totals.P50MS = &v
	}
	if v, ok := usageQuantile(0.95, buckets); ok {
		rep.Totals.P95MS = &v
	}
	rep.Now = UsageNow{CPU: UsageNotSeparate, Memory: UsageNotSeparate}
	if worker {
		cpu := float64(cpuMS) / 1000
		rep.Totals.CPUSeconds, rep.Totals.MemoryPeakBytes = &cpu, &peak
		rep.Now.CPU, rep.Now.Memory = UsageMeasured, UsageUnsupported
		rep.Now.State = workerStateOf(cur)
		var mem workerMemory
		if lf != nil && lf.usage != nil {
			mem = lf.usage.worker.memory()
		}
		rep.Now.Workers = &mem.workers
		if memoryMeasured {
			rep.Now.Memory = UsageMeasured
			stale := mem.workers > 0 && s.now().Sub(mem.at) > memoryStaleAfter
			rep.Now.MemoryBytes, rep.Now.MemoryStale = &mem.bytes, &stale
			if !mem.at.IsZero() {
				at := mem.at.UTC()
				rep.Now.MemorySampledAt = &at
			}
		}
	}
	return rep, nil
}

// usageQuantile interpolates quantile q in milliseconds within the buckets
// (counts per UsageBuckets bound, then +Inf), as Prometheus'
// histogram_quantile does: a quantile in the +Inf bucket is the highest
// finite bound.
func usageQuantile(q float64, counts []int64) (float64, bool) {
	var total int64
	for _, n := range counts {
		total += n
	}
	if total == 0 {
		return 0, false
	}
	rank := q * float64(total)
	var cum int64
	for i, n := range counts {
		if n > 0 && float64(cum+n) >= rank {
			var sec float64
			if i >= len(UsageBuckets) {
				sec = UsageBuckets[len(UsageBuckets)-1]
			} else {
				lower := 0.0
				if i > 0 {
					lower = UsageBuckets[i-1]
				}
				sec = lower + (UsageBuckets[i]-lower)*(rank-float64(cum))/float64(n)
			}
			return math.Round(sec*1e4) / 10, true // milliseconds, one decimal
		}
		cum += n
	}
	return 0, false
}
