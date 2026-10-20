package metrics

import (
	"regexp"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/gosuda/flats/internal/buildinfo"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/slug"
)

var flatLabels = []string{"flat", "type"}

func desc(name, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc(name, help, labels, nil)
}

var (
	descBuildInfo     = desc("flats_build_info", "The running build of Flats; always 1.", "version", "commit")
	descDBBytes       = desc("flats_db_bytes", "Size of flats.db and its write-ahead log, in bytes, at the last host sweep.")
	descFlats         = desc("flats_flats", "Number of flats by type, visibility and whether they are running, at the last host sweep.", "type", "visibility", "running")
	descRequests      = desc("flats_requests_total", "Requests to a flat's live route by route kind and status class (closed: the visitor left before the response).", "flat", "type", "route", "code")
	descRateLimited   = desc("flats_rate_limited_total", "Requests to a flat refused by the rate limit.", flatLabels...)
	descRespBytes     = desc("flats_response_bytes_total", "Response body bytes a flat's live route sent.", flatLabels...)
	descOpenStreams   = desc("flats_open_streams", "Open WebSocket and event-stream connections of a flat.", flatLabels...)
	descDuration      = desc("flats_request_duration_seconds", "Response time of a flat's live route, without WebSocket and event-stream connections.", flatLabels...)
	descWorkerCPU     = desc("flats_worker_cpu_seconds_total", "CPU time (user and system) of a flat's worker processes by role: live, preview or check.", "flat", "type", "role")
	descWorkerMem     = desc("flats_worker_memory_bytes", "Private resident memory of a flat's running live workers (Linux).", flatLabels...)
	descWorkerMemAt   = desc("flats_worker_memory_sampled_timestamp_seconds", "When flats_worker_memory_bytes was sampled, in Unix seconds.", flatLabels...)
	descWorkerState   = desc("flats_worker_state", "State of the worker serving a flat's current version; 1 for that state.", "flat", "type", "state")
	descWorkerExits   = desc("flats_worker_unexpected_exits_total", "Worker processes of a flat that exited without being asked to.", flatLabels...)
	descDiskBytes     = desc("flats_disk_bytes", "Disk usage of a flat in bytes when it was last measured.", flatLabels...)
	descDiskAt        = desc("flats_disk_measured_timestamp_seconds", "When flats_disk_bytes was measured, in Unix seconds.", flatLabels...)
	descEndpoints     = desc("flats_route_endpoints", "Observed route endpoints by network provider and state.", "provider", "state")
	descApprovals     = desc("flats_pending_approvals", "Approvals waiting for the operator, at the last host sweep.")
	descSnapshotAt    = desc("flats_snapshot_timestamp_seconds", "When the last host sweep refreshed the host-wide figures, in Unix seconds.")
	descManagementReq = desc("flats_management_requests_total", "Requests to the management server by surface and status class.", "surface", "code")
	descBoardBatches  = desc("flats_board_batches_total", "People's edit batches on boards by result: applied, rejected or retry (a transient refusal the canvas resends).", "result")
	descBoardConns    = desc("flats_board_connections", "Open board WebSocket connections, Private and Public.")
	descBoardImport   = desc("flats_board_import_seconds", "Time to import board revisions from a board's outbox into the revision store.")

	allDescs = []*prometheus.Desc{descBuildInfo, descDBBytes, descFlats, descRequests, descRateLimited, descRespBytes,
		descOpenStreams, descDuration, descWorkerCPU, descWorkerMem, descWorkerMemAt, descWorkerState, descWorkerExits,
		descDiskBytes, descDiskAt, descEndpoints, descApprovals, descSnapshotAt, descManagementReq,
		descBoardBatches, descBoardConns, descBoardImport}
)

// Label values reach the exposition only from fixed enums, slugs, provider
// IDs and the build strings (spec rule 6). Values that come from core but are
// not slugs (type, visibility) must look like an enum word; anything else is
// reported as "other" rather than passed through.
var enumWord = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func enumLabel(v string) string {
	if enumWord.MatchString(v) {
		return v
	}
	return "other"
}

var routeKinds = []string{core.RoutePrivate, core.RoutePublic}

var workerStates = map[core.WorkerState]bool{
	core.WorkerRunning: true, core.WorkerStarting: true, core.WorkerQuiescing: true,
	core.WorkerSleeping: true, core.WorkerBackoff: true, core.WorkerStopped: true,
}

// endpointStates are the states network backends report; others are "other".
var endpointStates = map[string]bool{
	"ready": true, "starting": true, "needs-login": true, "key-expiring": true,
	"error": true, "idle": true, "stopped": true, "unavailable": true,
}

// buildLabel keeps a build string to the characters of versions and commits.
var buildChars = regexp.MustCompile(`[^0-9A-Za-z.+_-]`)

func buildLabel(v string) string {
	if len(v) > 64 {
		v = v[:64]
	}
	return buildChars.ReplaceAllString(v, "_")
}

type collector struct{ m *Metrics }

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range allDescs {
		ch <- d
	}
}

func unix(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	counter := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, labels...)
	}

	bi := buildinfo.Get()
	gauge(descBuildInfo, 1, buildLabel(bi.Version), buildLabel(bi.Commit))

	for i, s := range surfaces {
		for j, class := range managementClasses {
			counter(descManagementReq, float64(c.m.mgmt[i][j].Load()), s, class)
		}
	}

	type epKey struct{ provider, state string }
	eps := map[epKey]int{}
	for _, e := range c.m.cachedEndpoints() {
		if _, err := provider.ParseID(string(e.Provider)); err != nil {
			continue
		}
		state := e.State
		if !endpointStates[state] {
			state = "other"
		}
		eps[epKey{string(e.Provider), state}] += e.N
	}
	for k, n := range eps {
		gauge(descEndpoints, float64(n), k.provider, k.state)
	}

	if c.m.src == nil {
		return
	}
	snap := c.m.src.Metrics()
	if !snap.SnapshotAt.IsZero() {
		gauge(descSnapshotAt, unix(snap.SnapshotAt))
		gauge(descDBBytes, float64(snap.DBBytes))
		gauge(descApprovals, float64(snap.PendingApprovals))
	}
	b := snap.Board
	for _, result := range []string{"applied", "rejected", "retry"} {
		counter(descBoardBatches, float64(b.Batches[result]), result)
	}
	gauge(descBoardConns, float64(b.Connections))
	buckets := make(map[float64]uint64, len(core.BoardImportBuckets))
	var cum uint64
	for i, bound := range core.BoardImportBuckets {
		if i < len(b.ImportCounts) {
			cum += b.ImportCounts[i]
		}
		buckets[bound] = cum
	}
	ch <- prometheus.MustNewConstHistogram(descBoardImport, b.ImportCount, b.ImportSum, buckets)

	type countKey struct{ typ, vis, running string }
	counts := map[countKey]int{}
	for _, fc := range snap.Counts {
		counts[countKey{enumLabel(fc.Type), enumLabel(fc.Visibility), strconv.FormatBool(fc.Running)}] += fc.N
	}
	for k, n := range counts {
		gauge(descFlats, float64(n), k.typ, k.vis, k.running)
	}

	if !c.m.perFlat {
		return
	}

	seen := map[string]bool{}
	for _, f := range snap.Flats {
		if slug.Validate(f.Slug) != nil || seen[f.Slug] {
			continue // never a label that is not a slug; never a duplicate series
		}
		seen[f.Slug] = true
		c.flat(ch, f, gauge, counter)
	}

	type roleKey struct{ slug, typ, role string }
	roles := map[roleKey]float64{}
	var order []roleKey
	for _, rc := range snap.RoleCPU {
		if (rc.Role != core.UsageRolePreview && rc.Role != core.UsageRoleCheck) || slug.Validate(rc.Slug) != nil {
			continue
		}
		k := roleKey{rc.Slug, enumLabel(rc.Type), rc.Role}
		if _, ok := roles[k]; !ok {
			order = append(order, k)
		}
		roles[k] += rc.CPUSeconds
	}
	for _, k := range order {
		counter(descWorkerCPU, roles[k], k.slug, k.typ, k.role)
	}
}

func (c *collector) flat(ch chan<- prometheus.Metric, f core.FlatMetrics,
	gauge, counter func(*prometheus.Desc, float64, ...string)) {
	s, t := f.Slug, enumLabel(f.Type)
	if !f.DiskMeasuredAt.IsZero() {
		gauge(descDiskBytes, float64(f.DiskBytes), s, t)
		gauge(descDiskAt, unix(f.DiskMeasuredAt), s, t)
	}
	// Without live state (a rename in progress, or a flat deleted since the
	// last sweep) only the disk figures are known; leaving the counters out
	// keeps them from appearing to reset.
	if !f.Traffic {
		return
	}
	for _, route := range routeKinds {
		for _, class := range core.UsageClasses {
			counter(descRequests, float64(f.Requests[route][class]), s, t, route, class)
		}
	}
	counter(descRateLimited, float64(f.RateLimited), s, t)
	counter(descRespBytes, float64(f.ResponseBytes), s, t)
	gauge(descOpenStreams, float64(f.OpenStreams), s, t)
	count, sum, buckets := histogram(f)
	ch <- prometheus.MustNewConstHistogram(descDuration, count, sum, buckets, s, t)

	if f.HasWorker {
		counter(descWorkerCPU, f.CPUSeconds, s, t, core.UsageRoleLive)
		if f.MemoryOK {
			gauge(descWorkerMem, float64(f.MemoryBytes), s, t)
			if !f.MemorySampledAt.IsZero() {
				gauge(descWorkerMemAt, unix(f.MemorySampledAt), s, t)
			}
		}
		if workerStates[f.State] {
			gauge(descWorkerState, 1, s, t, string(f.State))
		}
		counter(descWorkerExits, float64(f.UnexpectedExits), s, t)
	}
}

// histogram turns the per-bucket counts of core.UsageBuckets (the last entry
// is +Inf) into the cumulative bucket map of a Prometheus histogram.
func histogram(f core.FlatMetrics) (count uint64, sum float64, buckets map[float64]uint64) {
	buckets = make(map[float64]uint64, len(core.UsageBuckets))
	var cum uint64
	for i, bound := range core.UsageBuckets {
		if i < len(f.DurationCounts) {
			cum += f.DurationCounts[i]
		}
		buckets[bound] = cum
	}
	total := cum
	for i := len(core.UsageBuckets); i < len(f.DurationCounts); i++ {
		total += f.DurationCounts[i]
	}
	count = max(f.DurationCount, total)
	return count, f.DurationSum, buckets
}
