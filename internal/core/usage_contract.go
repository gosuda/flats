package core

import "time"

// This file is the contract between core (request and worker usage), the
// runtime (worker samples and states) and the metrics endpoint. See
// docs/spec/metrics.md and docs/internal/metrics.md.

// UsageSink receives the CPU and memory of the worker processes started for
// one RuntimeSpec. id is unique per process within the host run.
type UsageSink interface {
	// Started is called once after the process started.
	Started(id uint64)
	// Sample reports the latest CPU time (user+system, ns since the process
	// started) and, when memOK, its resident private anonymous memory.
	Sample(id uint64, cpuNS int64, mem int64, memOK bool)
	// Exited is called once when the process ended, before Stop returns,
	// with its final CPU time. unexpected is false when it was asked to stop.
	Exited(id uint64, cpuNS int64, unexpected bool)
}

// WorkerState is the diagnostic state of a flat's current worker. It never
// changes whether the console and API call a flat running.
type WorkerState string

const (
	WorkerRunning   WorkerState = "running"
	WorkerStarting  WorkerState = "starting"
	WorkerQuiescing WorkerState = "quiescing"
	WorkerSleeping  WorkerState = "sleeping"
	WorkerBackoff   WorkerState = "backoff"
	WorkerStopped   WorkerState = "stopped"
)

// StateReporter is implemented by runtime instances (and forwarded by core's
// wrappers). It never wakes, starts or messages a worker.
type StateReporter interface {
	WorkerState() WorkerState
}

// Usage roles of worker CPU in host metrics.
const (
	UsageRoleLive    = "live"
	UsageRolePreview = "preview"
	UsageRoleCheck   = "check"
)

// Request classes counted per flat.
const (
	Class1xx    = "1xx"
	Class2xx    = "2xx"
	Class3xx    = "3xx"
	Class4xx    = "4xx"
	Class5xx    = "5xx"
	ClassClosed = "closed"
)

// UsageClasses lists the request classes in exposition order.
var UsageClasses = []string{Class1xx, Class2xx, Class3xx, Class4xx, Class5xx, ClassClosed}

// Route kinds counted per flat.
const (
	RoutePrivate = "private"
	RoutePublic  = "public"
)

// UsageBuckets are the upper bounds in seconds of the response-time
// histogram (+Inf implied). The 11 s bound keeps requests that end at the
// 10 s runtime deadline (10 s plus a little overhead) out of (10, 30], where
// they would inflate the upper percentiles. Version 2 of the stored latency
// encoding (version 1 had no 11 s bound).
var UsageBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 11, 30}

// MetricsSnapshot is everything /metrics exposes from core. Service.Metrics
// builds it from memory only: it never reads SQLite, walks files, or touches
// workers or providers.
type MetricsSnapshot struct {
	// SnapshotAt is when the last Sweep refreshed the host-wide figures.
	SnapshotAt       time.Time
	DBBytes          int64
	PendingApprovals int
	Counts           []FlatCount
	Flats            []FlatMetrics
	// RoleCPU is preview and check worker CPU, per flat and role.
	RoleCPU []RoleCPU
	// Board holds the host-wide board counters.
	Board BoardMetrics
}

// FlatCount is the number of flats with one type, visibility and logical
// running state.
type FlatCount struct {
	Type       string
	Visibility string
	Running    bool
	N          int
}

// FlatMetrics are one flat's series.
type FlatMetrics struct {
	Slug string
	Type string
	// Traffic is false for a flat that has no live state at this moment (a
	// rename in progress, a flat deleted since the last snapshot): only its
	// disk figures are known, and no traffic or worker series should be
	// emitted for it, so its counters never appear to reset.
	Traffic bool
	// Requests[route][class]: cumulative since the host started.
	Requests      map[string]map[string]uint64
	RateLimited   uint64
	ResponseBytes uint64
	OpenStreams   int64
	// Duration histogram: cumulative counts per UsageBuckets bound (not
	// cumulative across buckets; the last entry is +Inf), sum in seconds.
	DurationCounts []uint64
	DurationSum    float64
	DurationCount  uint64

	// Worker figures; HasWorker is false for flats without a worker process.
	HasWorker       bool
	CPUSeconds      float64
	MemoryOK        bool
	MemoryBytes     int64
	MemorySampledAt time.Time
	State           WorkerState
	UnexpectedExits uint64
	DiskBytes       int64
	DiskMeasuredAt  time.Time // zero when never measured
}

// RoleCPU is the CPU of a flat's preview or check workers.
type RoleCPU struct {
	Slug       string
	Type       string
	Role       string
	CPUSeconds float64
}
