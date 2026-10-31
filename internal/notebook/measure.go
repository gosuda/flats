package notebook

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sync"
	"time"
)

type contentMeasure struct {
	DirtySizes       []int64
	DirtyUniqueBytes int64
	FullListBytes    int64
	UnimportedBytes  int64
}

type contentByteCounter int64

func (n *contentByteCounter) Write(data []byte) (int, error) {
	if int64(len(data)) > math.MaxInt64-int64(*n) {
		return 0, fmt.Errorf("notebook content size overflow")
	}
	*n += contentByteCounter(len(data))
	return len(data), nil
}

func addContentBytes(total *int64, size int64) error {
	if size < 0 || size > math.MaxInt64-*total {
		return fmt.Errorf("notebook content size overflow")
	}
	*total += size
	return nil
}

func measureContent(ctx context.Context, tx *sql.Tx) (contentMeasure, error) {
	m := contentMeasure{DirtySizes: []int64{}}
	files, err := currentContentFiles(ctx, tx)
	if err != nil {
		return m, err
	}
	var count contentByteCounter
	if err := writeContentFileList(&count, files); err != nil {
		return m, err
	}
	m.FullListBytes = int64(count)
	newest, err := newestContentFiles(ctx, tx)
	if err != nil {
		return m, err
	}
	reserved := make(map[string]bool, len(newest))
	for _, f := range newest {
		reserved[f.Hash] = true
	}
	rows, err := tx.QueryContext(ctx, `SELECT hash,length(data) FROM snapshots WHERE refs>0 ORDER BY hash`)
	if err != nil {
		return m, err
	}
	for rows.Next() {
		var hash string
		var size int64
		if err := rows.Scan(&hash, &size); err != nil {
			rows.Close()
			return m, err
		}
		if err := addContentBytes(&m.UnimportedBytes, size); err != nil {
			rows.Close()
			return m, err
		}
		reserved[hash] = true
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return m, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT hash,bytes FROM files WHERE dirty_seq<>'0' ORDER BY path`)
	if err != nil {
		return m, err
	}
	defer rows.Close()
	for rows.Next() {
		var hash string
		var size int64
		if err := rows.Scan(&hash, &size); err != nil {
			return m, err
		}
		if size < 0 {
			return m, fmt.Errorf("negative notebook content size")
		}
		m.DirtySizes = append(m.DirtySizes, size)
		if !reserved[hash] {
			if err := addContentBytes(&m.DirtyUniqueBytes, size); err != nil {
				return m, err
			}
			reserved[hash] = true
		}
	}
	return m, rows.Err()
}

const (
	measureTTL     = 60 * time.Second
	measureRefresh = 45 * time.Second
)

// Directory measurements never run on the writer or while it owns a SQL
// transaction. Concurrent blob completions remain conservatively counted.
type directoryMeasure struct {
	in         *Instance
	mu         sync.Mutex
	other      int64
	extra      int64
	reserved   int64
	generation uint64
	at         time.Time
	valid      bool
	wake       chan struct{}
	gate       chan struct{}
}

func newDirectoryMeasure(in *Instance) *directoryMeasure {
	return &directoryMeasure{in: in, wake: make(chan struct{}, 1), gate: make(chan struct{}, 1)}
}

func (m *directoryMeasure) get() (int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.valid || m.in.now().Sub(m.at) > measureTTL {
		return 0, false
	}
	n, err := quotaAdd(m.other, m.extra, m.reserved)
	return n, err == nil
}

func (m *directoryMeasure) refresh() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *directoryMeasure) add(n int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generation++
	total, err := quotaAdd(m.extra, n)
	if err != nil {
		m.valid = false
		return
	}
	m.extra = total
}

func (m *directoryMeasure) agentOther(fresh int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return quotaAdd(fresh, m.extra, m.reserved)
}

func (m *directoryMeasure) reserve(n int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n < 0 {
		if n < -m.reserved {
			return fmt.Errorf("notebook blob reservation underflow")
		}
		m.reserved += n
		return nil
	}
	total, err := quotaAdd(m.reserved, n)
	if err == nil {
		m.reserved = total
	}
	return err
}

func (m *directoryMeasure) remeasure(ctx context.Context) error {
	select {
	case m.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-m.in.ctx.Done():
		return unavailable("stopped")
	}
	defer func() { <-m.gate }()
	m.mu.Lock()
	generation := m.generation
	m.mu.Unlock()
	other, err := m.in.b.MeasureOther(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil || other < 0 {
		m.valid = false
		if err == nil {
			err = fmt.Errorf("negative notebook directory measurement")
		}
		return err
	}
	m.other, m.at, m.valid = other, m.in.now(), true
	if generation == m.generation {
		m.extra = 0
	}
	return nil
}

func (m *directoryMeasure) run() {
	defer m.in.workers.Done()
	tick := time.NewTicker(measureRefresh)
	defer tick.Stop()
	for {
		select {
		case <-m.in.ctx.Done():
			return
		case <-tick.C:
		case <-m.wake:
		}
		ctx, cancel := context.WithTimeout(m.in.ctx, 2*time.Minute)
		err := m.remeasure(ctx)
		cancel()
		if err != nil && m.in.ctx.Err() == nil {
			m.in.log.Warn("notebook measurement", "err", err)
		}
	}
}
