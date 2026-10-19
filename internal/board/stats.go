package board

import "sync/atomic"

// Host-wide counters for /metrics (docs/internal/metrics.md).
var hostStats struct {
	applied, rejected, retried atomic.Int64 // people's batches by result
	conns                      atomic.Int64 // admitted WebSockets
}

// Stats are the host-wide board counters.
type Stats struct {
	Applied, Rejected, Retried int64 // people's batches by result
	Connections                int64 // open WebSockets of every board
}

// HostStats returns the host-wide board counters.
func HostStats() Stats {
	return Stats{Applied: hostStats.applied.Load(), Rejected: hostStats.rejected.Load(),
		Retried: hostStats.retried.Load(), Connections: hostStats.conns.Load()}
}
