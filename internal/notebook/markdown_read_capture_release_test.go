package notebook

import (
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMarkdownCaptureReleasesOptions(t *testing.T) {
	f, created := readerQueryFixture(t)
	property := created.Refs["tags"].Ref
	const optionCount = 40000
	ids := make([]ID, 0, optionCount)
	for n := 0; n < optionCount; n++ {
		ids = append(ids, spillID(OptionKind, uint64(n+100)))
	}
	name := func(i int) string { return fmt.Sprintf("capture%06d", i) }
	order := make([]ID, 0, optionCount+2)
	order = append(order, created.Refs["first"].Ref.ID)
	order = append(order, ids...)
	order = append(order, created.Refs["second"].Ref.ID)
	if err := f.in.db.transaction(t.Context(), func(tx *sql.Tx) error {
		if err := spillOptions(t, tx, property.ID, ids, name); err != nil {
			return err
		}
		if err := putOrder(t.Context(), tx, property.ID, OptionOrderGroup, order); err != nil {
			return err
		}
		selected := []ID{ids[0], ids[optionCount/2]}
		cell, err := EncodeCellValue(CellValue{Type: MultiSelectType, Value: selected})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(t.Context(), `INSERT INTO cells(row_id,property_id,value) VALUES(?,?,?)`, created.Refs["big"].Ref.ID, property.ID, string(cell))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	spills := f.in.spills
	spillUsed := func() int64 {
		spills.mu.Lock()
		defer spills.mu.Unlock()
		return spills.used
	}
	optionBytes := func() (ranks, vectors int64) {
		_ = filepath.WalkDir(f.in.spillDir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return nil
			}
			base := filepath.Base(path)
			if !strings.HasPrefix(base, "query-ranks") && !strings.HasPrefix(base, "markdown-options-") {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return nil
			}
			if strings.HasPrefix(base, "query-ranks") {
				ranks += info.Size()
			} else {
				vectors += info.Size()
			}
			return nil
		})
		return ranks, vectors
	}
	baseUsed := spillUsed()
	baseRanks, baseVectors := optionBytes()
	var peakUsed, peakRanks, peakVectors atomic.Int64
	var mu sync.Mutex
	sample := func() {
		mu.Lock()
		defer mu.Unlock()
		if used := spillUsed(); used > peakUsed.Load() {
			peakUsed.Store(used)
		}
		ranks, vectors := optionBytes()
		if ranks > peakRanks.Load() {
			peakRanks.Store(ranks)
		}
		if vectors > peakVectors.Load() {
			peakVectors.Store(vectors)
		}
	}
	sample()
	stop, joined := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(joined)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				sample()
			}
		}
	}()
	body, _ := liveReadQuery(t, f.in, map[string]any{"page": f.page.ID, "format": "markdown", "limit": 500}, false)
	continueMarkdownRead(t, f.in, body)
	sample()
	close(stop)
	<-joined

	heldQuota := peakUsed.Load() - baseUsed
	t.Logf("capture quota: baseline=%d peak=%d held=%d", baseUsed, peakUsed.Load(), heldQuota)
	t.Logf("rank index files: baseline=%d peak=%d", baseRanks, peakRanks.Load())
	t.Logf("id vector files: baseline=%d peak=%d", baseVectors, peakVectors.Load())
	if heldQuota <= 4<<20 {
		t.Fatalf("capture under test holds only %d quota bytes, want over 4 MiB", heldQuota)
	}
	if peakRanks.Load() == 0 {
		t.Fatalf("no rank index file observed during the read")
	}
	if peakVectors.Load() == 0 {
		t.Fatalf("no id vector file observed during the read")
	}
	if ranks, vectors := optionBytes(); ranks != 0 || vectors != 0 {
		t.Errorf("capture files not released: ranks=%d vectors=%d", ranks, vectors)
	}
}
