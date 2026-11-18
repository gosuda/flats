package core

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosuda/flats/internal/sheet"
)

// SheetImportBuckets are the upper bounds, in seconds, of
// flats_sheet_import_seconds.
var SheetImportBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// SheetRecalcBuckets are the upper bounds, in seconds, of
// flats_sheet_recalculation_seconds. An edit's recalculation is refused at
// 10 seconds; a load has no such limit.
var SheetRecalcBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// SheetRecalcStarts are the values of the start label of
// flats_sheet_recalculation_seconds: an edit (a person's action, an agent
// call or a tab recovery) or anything else (a load, a date change, a
// rollback or a revision view).
var SheetRecalcStarts = []string{"edit", "other"}

// SheetRecalc is the recalculation times of one start. Counts has one
// noncumulative count per SheetRecalcBuckets bound plus one above the last.
type SheetRecalc struct {
	Count  uint64
	Sum    float64
	Counts []uint64
}

// SheetMetrics are the host-wide sheet figures /metrics exposes.
type SheetMetrics struct {
	// Batches counts the writes to sheets (people's edit batches, agent
	// calls and rollbacks) by applied or rejected result.
	Batches map[string]int64
	// Connections is the number of open sheet WebSockets.
	Connections int64
	// ImportCounts has one noncumulative count per SheetImportBuckets bound
	// plus one above the last. Only imports recording revisions count.
	ImportCount  uint64
	ImportSum    float64
	ImportCounts []uint64
	// Recalc has the recalculation times by SheetRecalcStarts value.
	Recalc map[string]SheetRecalc
}

// sheetRecalcStats are the recalculation times of one start. They belong
// to the host, not to an instance, so an unload takes nothing from them.
// The instance reports from its writer, which must not wait: the fields are
// atomic.
type sheetRecalcStats struct {
	nanos  atomic.Int64
	counts [15]atomic.Uint64 // len(SheetRecalcBuckets) + 1
}

type sheetStats struct {
	mu     sync.Mutex
	count  uint64
	sum    float64
	counts [12]uint64 // len(SheetImportBuckets) + 1
	// open holds the instances that count their own batches. applied and
	// rejected are the batches of the instances that stopped. An instance
	// leaves open in the step that adds its counts to them, so a scrape
	// that checks open in the step that reads them sees every batch once.
	open              map[*sheet.Instance]struct{}
	applied, rejected int64
	// scraping, when set by a test, runs in a scrape between its read of
	// the open instances and its read of the stopped ones.
	scraping func()
	// recalc is indexed like SheetRecalcStarts.
	recalc [2]sheetRecalcStats
}

// observeRecalc records one recalculation. Instances call it through
// Options.OnRecalc.
func (n *sheetStats) observeRecalc(edit bool, took time.Duration) {
	r := &n.recalc[1]
	if edit {
		r = &n.recalc[0]
	}
	i, _ := slices.BinarySearch(SheetRecalcBuckets, took.Seconds())
	r.counts[i].Add(1)
	r.nanos.Add(int64(took))
}

func (n *sheetStats) observeImport(d time.Duration) {
	sec := d.Seconds()
	i, _ := slices.BinarySearch(SheetImportBuckets, sec)
	n.mu.Lock()
	n.count++
	n.sum += sec
	n.counts[i]++
	n.mu.Unlock()
}

// track counts the batches of an instance that opened.
func (n *sheetStats) track(in *sheet.Instance) {
	n.mu.Lock()
	if n.open == nil {
		n.open = map[*sheet.Instance]struct{}{}
	}
	n.open[in] = struct{}{}
	n.mu.Unlock()
}

// retire keeps the batch counts of an instance that stopped, so the host
// totals never go down. The caller has stopped the instance.
func (n *sheetStats) retire(in *sheet.Instance) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.open[in]; !ok {
		return
	}
	delete(n.open, in)
	st := in.Stats()
	n.applied += st.Applied
	n.rejected += st.Rejected
}

func (s *Service) sheetMetrics() SheetMetrics {
	m := SheetMetrics{Connections: sheet.HostConnections()}
	n := &s.sheetStats
	// The instances are read outside the lock, so that a scrape never holds
	// it across a call into a sheet.
	n.mu.Lock()
	read := make(map[*sheet.Instance]sheet.Stats, len(n.open))
	for in := range n.open {
		read[in] = sheet.Stats{}
	}
	scraping := n.scraping
	n.mu.Unlock()
	for in := range read {
		read[in] = in.Stats()
	}
	if scraping != nil {
		scraping()
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	var applied, rejected int64
	for in, st := range read {
		// An instance that stopped since is in the stopped totals.
		if _, ok := n.open[in]; ok {
			applied += st.Applied
			rejected += st.Rejected
		}
	}
	m.Batches = map[string]int64{"applied": applied + n.applied, "rejected": rejected + n.rejected}
	m.ImportCount, m.ImportSum, m.ImportCounts = n.count, n.sum, slices.Clone(n.counts[:])
	m.Recalc = make(map[string]SheetRecalc, len(SheetRecalcStarts))
	for i, start := range SheetRecalcStarts {
		r := SheetRecalc{Sum: time.Duration(n.recalc[i].nanos.Load()).Seconds(), Counts: make([]uint64, len(n.recalc[i].counts))}
		for j := range r.Counts {
			r.Counts[j] = n.recalc[i].counts[j].Load()
			r.Count += r.Counts[j]
		}
		m.Recalc[start] = r
	}
	return m
}
