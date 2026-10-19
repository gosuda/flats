package core

import (
	"slices"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/board"
)

// BoardImportBuckets are the upper bounds, in seconds, of
// flats_board_import_seconds.
var BoardImportBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// BoardMetrics are the host-wide board figures /metrics exposes.
type BoardMetrics struct {
	// Batches counts people's batches by result: applied, rejected
	// (permanently) and retry (a transient refusal the client resends).
	Batches map[string]int64
	// Connections is the number of open board WebSockets.
	Connections int64
	// ImportCount and ImportSum describe the outbox imports that recorded
	// revisions; ImportCounts holds one count per BoardImportBuckets bound
	// plus one above the last.
	ImportCount  uint64
	ImportSum    float64
	ImportCounts []uint64
}

// boardStats accumulates the duration of board outbox imports.
type boardStats struct {
	mu     sync.Mutex
	count  uint64
	sum    float64
	counts [12]uint64 // len(BoardImportBuckets) + 1
}

func (b *boardStats) observeImport(d time.Duration) {
	sec := d.Seconds()
	i, _ := slices.BinarySearch(BoardImportBuckets, sec)
	b.mu.Lock()
	b.count++
	b.sum += sec
	b.counts[i]++
	b.mu.Unlock()
}

func (s *Service) boardMetrics() BoardMetrics {
	h := board.HostStats()
	m := BoardMetrics{Batches: map[string]int64{"applied": h.Applied, "rejected": h.Rejected, "retry": h.Retried}, Connections: h.Connections}
	s.boardStats.mu.Lock()
	m.ImportCount, m.ImportSum, m.ImportCounts = s.boardStats.count, s.boardStats.sum, slices.Clone(s.boardStats.counts[:])
	s.boardStats.mu.Unlock()
	return m
}
