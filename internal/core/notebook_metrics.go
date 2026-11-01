package core

import (
	"slices"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/notebook"
)

// NotebookImportBuckets are the upper bounds, in seconds, of
// flats_notebook_import_seconds.
var NotebookImportBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// NotebookMetrics are the host-wide notebook figures /metrics exposes.
type NotebookMetrics struct {
	// Batches counts people's batches by applied, rejected or retry result.
	Batches map[string]int64
	// Connections is the number of open notebook WebSockets.
	Connections int64
	// ImportCounts has one noncumulative count per NotebookImportBuckets
	// bound plus one above the last. Only imports recording revisions count.
	ImportCount  uint64
	ImportSum    float64
	ImportCounts []uint64
}

type notebookImportStats struct {
	mu     sync.Mutex
	count  uint64
	sum    float64
	counts [12]uint64 // len(NotebookImportBuckets) + 1
}

func (n *notebookImportStats) observeImport(d time.Duration) {
	sec := d.Seconds()
	i, _ := slices.BinarySearch(NotebookImportBuckets, sec)
	n.mu.Lock()
	n.count++
	n.sum += sec
	n.counts[i]++
	n.mu.Unlock()
}

func (s *Service) notebookMetrics() NotebookMetrics {
	h := notebook.HostStats()
	m := NotebookMetrics{Batches: map[string]int64{"applied": h.Applied, "rejected": h.Rejected, "retry": h.Retried}, Connections: h.Connections}
	s.notebookStats.mu.Lock()
	m.ImportCount, m.ImportSum, m.ImportCounts = s.notebookStats.count, s.notebookStats.sum, slices.Clone(s.notebookStats.counts[:])
	s.notebookStats.mu.Unlock()
	return m
}
