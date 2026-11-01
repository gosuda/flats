package notebook

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type writerQuotaBackend struct {
	Backend
	quota int64
}

func (b *writerQuotaBackend) Limits() Limits { return Limits{DiskQuota: b.quota} }

func writerQuotaInstance(t *testing.T) (*Instance, *writerQuotaBackend) {
	t.Helper()
	s, _, err := openStore(filepath.Join(t.TempDir(), dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	})
	b := &writerQuotaBackend{quota: math.MaxInt64}
	return &Instance{db: s, b: b, now: func() time.Time { return time.Unix(1234, 0) }}, b
}

func writerQuotaError(t *testing.T, err error) {
	t.Helper()
	var e *WriteError
	if !errors.As(err, &e) || e.Code != "notebook_capacity" || e.Reason != "disk_quota" || e.Retry {
		t.Fatalf("expected nonretryable disk quota refusal, got %v", err)
	}
}

func TestWriterQuotaCommitFramesAreReserved(t *testing.T) {
	f := newWriterFixture(t)
	reader, err := f.in.readers.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback()
	var seq string
	if err := reader.QueryRowContext(t.Context(), `SELECT seq FROM meta`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	var before, limit int64
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		data := []byte(strings.Repeat("x", 1<<20))
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO snapshots(hash,data,refs) VALUES(?,?,1)`, contentHash(data), data); err != nil {
			return err
		}
		m, err := measureContent(t.Context(), tx)
		if err != nil {
			return err
		}
		used, err := quotaUsedPages(t.Context(), tx)
		if err != nil {
			return err
		}
		future, err := quotaFuturePages(m, f.in.db.pageSize)
		if err != nil {
			return err
		}
		wal, err := f.in.quotaWALBytes()
		if err != nil {
			return err
		}
		before = wal
		old, err := quotaUsage(m, f.in.db.pageSize, used, future, 0, 0, wal)
		if err != nil {
			return err
		}
		f.b.quota.Store(old)
		writerQuotaError(t, f.in.checkQuota(t.Context(), tx, 0, 0))
		limit = writerQuotaEstimate(t, f.in, tx, 0, 0)
		f.b.quota.Store(limit)
		return f.in.checkQuota(t.Context(), tx, 0, 0)
	}); err != nil {
		t.Fatal(err)
	}
	wal, err := f.in.quotaWALBytes()
	if err != nil {
		t.Fatal(err)
	}
	if wal <= before {
		t.Fatalf("fixture did not exercise commit-only WAL growth: %d -> %d", before, wal)
	}
	tx, err := f.in.readers.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	m, err := measureContent(t.Context(), tx)
	if err != nil {
		t.Fatal(err)
	}
	used, err := quotaUsedPages(t.Context(), tx)
	if err != nil {
		t.Fatal(err)
	}
	future, err := quotaFuturePages(m, f.in.db.pageSize)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := quotaUsage(m, f.in.db.pageSize, used, future, 0, 0, wal)
	if err != nil || actual > limit {
		t.Fatalf("post-commit usage=%d limit=%d: %v", actual, limit, err)
	}
}

func TestWriterQuotaFormula(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		m    contentMeasure
		ps   int64
	}{
		{"empty", contentMeasure{}, 4096},
		{"round each file", contentMeasure{DirtySizes: []int64{1, 1, 3225, 3226}, FullListBytes: 2765}, 4096},
		{"more than fifty files", contentMeasure{DirtySizes: make([]int64, 101), FullListBytes: 500001}, 4096},
		{"large bucket", contentMeasure{DirtySizes: []int64{64 << 20}, FullListBytes: 4097}, 512},
		{"large exact integer", contentMeasure{DirtySizes: []int64{math.MaxInt64}, FullListBytes: 0}, 65536},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ceil := func(n, d *big.Int) *big.Int {
				q, r := new(big.Int), new(big.Int)
				q.QuoRem(n, d, r)
				if r.Sign() != 0 {
					q.Add(q, big.NewInt(1))
				}
				return q
			}
			inflated := func(n, size int64) *big.Int {
				return ceil(new(big.Int).Mul(big.NewInt(n), big.NewInt(5)), new(big.Int).Mul(big.NewInt(size), big.NewInt(4)))
			}
			want := big.NewInt(8)
			want.Add(want, ceil(big.NewInt(int64(len(tc.m.DirtySizes))), big.NewInt(50)))
			want.Add(want, new(big.Int).Mul(big.NewInt(2), inflated(tc.m.FullListBytes+512, tc.ps)))
			for _, size := range tc.m.DirtySizes {
				want.Add(want, inflated(size, tc.ps-64))
			}
			got, err := quotaFuturePages(tc.m, tc.ps)
			if err != nil || got != want.Int64() {
				t.Fatalf("future pages = %d, %v; want %s", got, err, want)
			}
		})
	}
}

func TestWriterQuotaCheckedArithmetic(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		call func() (int64, error)
	}{
		{"sum overflow", func() (int64, error) { return quotaAdd(math.MaxInt64, 1) }},
		{"negative bytes", func() (int64, error) { return quotaAdd(-1) }},
		{"product overflow", func() (int64, error) { return quotaMul(math.MaxInt64, 2) }},
		{"negative product", func() (int64, error) { return quotaMul(-1, 0) }},
		{"zero divisor", func() (int64, error) { return quotaCeil(1, 0) }},
		{"invalid page size", func() (int64, error) { return quotaFuturePages(contentMeasure{}, 64) }},
		{"list overflow", func() (int64, error) {
			return quotaFuturePages(contentMeasure{FullListBytes: math.MaxInt64}, 4096)
		}},
		{"negative dirty size", func() (int64, error) {
			return quotaFuturePages(contentMeasure{DirtySizes: []int64{-1}}, 4096)
		}},
		{"usage overflow", func() (int64, error) {
			return quotaUsage(contentMeasure{}, 4096, 1, 1, math.MaxInt64, 0, 0)
		}},
		{"negative pending", func() (int64, error) {
			return quotaUsage(contentMeasure{}, 4096, 1, 1, 0, -1, 0)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.call()
			writerQuotaError(t, err)
		})
	}
	if got, err := quotaCeil(math.MaxInt64, 1); err != nil || got != math.MaxInt64 {
		t.Fatalf("representable ceiling = %d, %v", got, err)
	}
}

func writerQuotaEstimate(t *testing.T, in *Instance, tx *sql.Tx, other, pending int64) int64 {
	t.Helper()
	m, err := measureContent(t.Context(), tx)
	if err != nil {
		t.Fatal(err)
	}
	used, err := quotaUsedPages(t.Context(), tx)
	if err != nil {
		t.Fatal(err)
	}
	future, err := quotaFuturePages(m, in.db.pageSize)
	if err != nil {
		t.Fatal(err)
	}
	growth, err := in.quotaRecordedGrowth(t.Context(), tx, used)
	if err != nil {
		t.Fatal(err)
	}
	wal, err := in.quotaPendingWAL(t.Context(), tx, max(future, growth+8))
	if err != nil {
		t.Fatal(err)
	}
	usage, err := quotaUsage(m, in.db.pageSize, used, max(future, growth+8), other, pending, wal)
	if err != nil {
		t.Fatal(err)
	}
	return usage
}

func TestWriterQuotaAdmission(t *testing.T) {
	t.Parallel()
	in, b := writerQuotaInstance(t)
	if err := in.db.transaction(t.Context(), func(tx *sql.Tx) error {
		if err := syncContentFiles(t.Context(), tx, 1); err != nil {
			return err
		}
		b.quota = writerQuotaEstimate(t, in, tx, 1234, 5678)
		if err := in.checkQuota(t.Context(), tx, 1234, 5678); err != nil {
			t.Fatalf("exactly fitting reservation refused: %v", err)
		}
		writerQuotaError(t, in.checkQuota(t.Context(), tx, 1234, 5679))
		writerQuotaError(t, in.checkQuota(t.Context(), tx, math.MaxInt64, 0))
		// A pre-edit check uses the same refusal path, including for a delete.
		b.quota = 1
		writerQuotaError(t, in.checkQuota(t.Context(), tx, 0, 0))
		b.quota = 0
		if err := in.checkQuota(t.Context(), tx, math.MaxInt64, math.MaxInt64); err != nil {
			t.Fatalf("disabled quota: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func writerQuotaID(kind Kind, n uint64) ID {
	var raw [16]byte
	binary.BigEndian.PutUint64(raw[8:], n)
	return ID(kindPrefixes[kind] + idEncoding.EncodeToString(raw[:]))
}

func TestWriterQuotaRecordingBound(t *testing.T) {
	t.Parallel()
	in, b := writerQuotaInstance(t)
	if err := in.db.transaction(t.Context(), func(tx *sql.Tx) error {
		for i := 0; i < 600; i++ {
			id := writerQuotaID(PageKind, uint64(i))
			item := Item{Ref: Ref{ID: id, Inc: 1}, Ver: 1, V: map[GroupKey]Version{}, Data: Page{Title: fmt.Sprintf("page-%d", i), Position: fmt.Sprintf("a%04d", i)}}
			if err := putItem(t.Context(), tx, item); err != nil {
				return err
			}
		}
		for i, added := uint64(0), 0; added < 8; i++ {
			id := writerQuotaID(BlockKind, i)
			if contentBucket(id) != "00" {
				continue
			}
			item := Item{Ref: Ref{ID: id, Inc: 1}, Ver: 1, V: map[GroupKey]Version{}, Data: Block{Page: writerQuotaID(PageKind, 0), Position: fmt.Sprintf("a%04d", added), Content: Content{Kind: Paragraph, Text: RichText{{Text: strings.Repeat("bucket", 1300), Marks: []Mark{}}}}}}
			if err := putItem(t.Context(), tx, item); err != nil {
				return err
			}
			added++
		}
		if err := syncContentFiles(t.Context(), tx, 1); err != nil {
			return err
		}
		before, err := measureContent(t.Context(), tx)
		if err != nil {
			return err
		}
		if len(before.DirtySizes) <= 512 || before.FullListBytes < 64000 {
			t.Fatalf("fixture must exercise large full list: %d files, %d bytes", len(before.DirtySizes), before.FullListBytes)
		}
		used, err := quotaUsedPages(t.Context(), tx)
		if err != nil {
			return err
		}
		growth, err := in.quotaRecordedGrowth(t.Context(), tx, used)
		if err != nil {
			return err
		}
		if growth <= 0 {
			t.Fatal("recording did not allocate pages")
		}
		after, err := measureContent(t.Context(), tx)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatal("dry-run changed dirty-file or snapshot reservations")
		}
		var outboxes, snapshots int
		if err := tx.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM outbox),(SELECT count(*) FROM snapshots)`).Scan(&outboxes, &snapshots); err != nil {
			return err
		}
		if outboxes != 0 || snapshots != 0 {
			t.Fatalf("probe left outbox/snapshot rows: %d/%d", outboxes, snapshots)
		}
		b.quota = writerQuotaEstimate(t, in, tx, 0, 0)
		if err := in.checkQuota(t.Context(), tx, 0, 0); err != nil {
			return err
		}
		formula, err := quotaFuturePages(before, in.db.pageSize)
		if err != nil {
			return err
		}
		charged := max(formula, growth+8)
		seq, err := recordOutbox(t.Context(), tx, "person", "", "", nil, in.now(), true)
		if err != nil {
			return err
		}
		actual, err := quotaUsedPages(t.Context(), tx)
		if err != nil {
			return err
		}
		if seq != 1 || charged < actual-used || actual-used != growth {
			t.Fatalf("charge=%d, observed=%d, actual=%d, seq=%d", charged, growth, actual-used, seq)
		}
		t.Logf("files=%d list_bytes=%d formula_pages=%d charged_pages=%d actual_growth_pages=%d", len(before.DirtySizes), before.FullListBytes, formula, charged, actual-used)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWriterQuotaProbeErrorRollsBack(t *testing.T) {
	t.Parallel()
	in, b := writerQuotaInstance(t)
	if err := in.db.transaction(t.Context(), func(tx *sql.Tx) error {
		if err := putItem(t.Context(), tx, Item{Ref: Ref{ID: writerQuotaID(PageKind, 1), Inc: 1}, Ver: 1, V: map[GroupKey]Version{}, Data: Page{Title: "page", Position: "a0"}}); err != nil {
			return err
		}
		if err := syncContentFiles(t.Context(), tx, 1); err != nil {
			return err
		}
		if _, err := tx.ExecContext(t.Context(), `UPDATE files SET bytes=bytes+1 WHERE path LIKE 'pages/%'`); err != nil {
			return err
		}
		used, err := quotaUsedPages(t.Context(), tx)
		if err != nil {
			return err
		}
		m, err := measureContent(t.Context(), tx)
		if err != nil {
			return err
		}
		future, err := quotaFuturePages(m, in.db.pageSize)
		if err != nil {
			return err
		}
		wal, err := in.quotaPendingWAL(t.Context(), tx, future)
		if err != nil {
			return err
		}
		b.quota, err = quotaUsage(m, in.db.pageSize, used, future, 0, 0, wal)
		if err != nil {
			return err
		}
		if err := in.checkQuota(t.Context(), tx, 0, 0); err == nil || !strings.Contains(err.Error(), "not synchronized") {
			t.Fatalf("near-quota check must run recording and propagate its failure, got %v", err)
		}
		var snapshots, outboxes, dirty int
		if err := tx.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM snapshots),(SELECT count(*) FROM outbox),(SELECT count(*) FROM files WHERE dirty_seq<>'0')`).Scan(&snapshots, &outboxes, &dirty); err != nil {
			return err
		}
		if snapshots != 0 || outboxes != 0 || dirty != 2 {
			t.Fatalf("failed probe changed state: snapshots=%d outbox=%d dirty=%d", snapshots, outboxes, dirty)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWriterQuotaTransactionPages(t *testing.T) {
	t.Parallel()
	in, b := writerQuotaInstance(t)
	if err := in.db.transaction(t.Context(), func(tx *sql.Tx) error {
		if err := syncContentFiles(t.Context(), tx, 1); err != nil {
			return err
		}
		b.quota = writerQuotaEstimate(t, in, tx, 0, 0)
		if err := in.checkQuota(t.Context(), tx, 0, 0); err != nil {
			return err
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO commands(op_id,request_hash,outcome,mapping_cursor,state,import_status) VALUES('metadata','','','','','pending')`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(t.Context(), `UPDATE commands SET outcome=? WHERE op_id='metadata'`, strings.Repeat("x", 256<<10)); err != nil {
			return err
		}
		writerQuotaError(t, in.checkQuota(t.Context(), tx, 0, 0))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWriterQuotaPinnedWAL(t *testing.T) {
	in, b := writerQuotaInstance(t)
	if err := in.truncateWAL(); err != nil {
		t.Fatal(err)
	}
	idle, err := in.quotaWALBytes()
	if err != nil {
		t.Fatal(err)
	}
	reader, err := sql.Open("sqlite", "file:"+in.db.path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	read, err := reader.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback()
	var seq string
	if err := read.QueryRowContext(t.Context(), `SELECT seq FROM meta`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := in.db.transaction(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `INSERT INTO commands(op_id,request_hash,outcome,mapping_cursor,state,import_status) VALUES(?,'',?,'','','pending')`, fmt.Sprint(i), strings.Repeat("x", 128<<10))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	if err := in.truncateWAL(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("pinned reader blocked writer for %v", elapsed)
	}
	var timeout int
	if err := in.db.db.QueryRowContext(t.Context(), `PRAGMA busy_timeout`).Scan(&timeout); err != nil || timeout != 5000 {
		t.Fatalf("writer timeout = %d, %v", timeout, err)
	}
	info, err := os.Stat(in.db.walPath())
	if err != nil {
		t.Fatal(err)
	}
	actual, err := in.quotaWALBytes()
	if err != nil || actual != info.Size() || actual <= idle {
		t.Fatalf("WAL allowance = %d, %v; stat=%d idle=%d", actual, err, info.Size(), idle)
	}
	if err := in.db.transaction(t.Context(), func(tx *sql.Tx) error {
		m, err := measureContent(t.Context(), tx)
		if err != nil {
			return err
		}
		used, err := quotaUsedPages(t.Context(), tx)
		if err != nil {
			return err
		}
		future, err := quotaFuturePages(m, in.db.pageSize)
		if err != nil {
			return err
		}
		b.quota, err = quotaUsage(m, in.db.pageSize, used, future, 0, 0, idle)
		if err != nil {
			return err
		}
		writerQuotaError(t, in.checkQuota(t.Context(), tx, 0, 0))
		b.quota = math.MaxInt64
		return in.checkQuota(t.Context(), tx, 0, 0)
	}); err != nil {
		t.Fatal(err)
	}
	if err := read.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := in.truncateWAL(); err != nil {
		t.Fatal(err)
	}
	if got, err := in.quotaWALBytes(); err != nil || got != idle {
		t.Fatalf("released-reader WAL allowance = %d, %v; want %d", got, err, idle)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := in.db.transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE meta SET wal_bytes=0`)
		return err
	}); err != nil {
		t.Fatalf("writer unusable after checkpoint: %v", err)
	}
}
