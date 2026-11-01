package notebook

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"slices"
	"testing"
)

func TestWriterMeasureDirtyAndUnimported(t *testing.T) {
	t.Parallel()
	s := writerOutboxStore(t)
	var before, after contentMeasure
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
		if err := syncContentFiles(t.Context(), tx, 1); err != nil {
			return err
		}
		var err error
		before, err = measureContent(t.Context(), tx)
		if err != nil {
			return err
		}
		files, err := currentContentFiles(t.Context(), tx)
		if err != nil {
			return err
		}
		var encoded bytes.Buffer
		if err := writeContentFileList(&encoded, files); err != nil {
			return err
		}
		if before.FullListBytes != int64(encoded.Len()) {
			return fmt.Errorf("file-list reservation %d != actual %d", before.FullListBytes, encoded.Len())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var dirtyBytes int64
	for _, size := range before.DirtySizes {
		dirtyBytes += size
	}
	if len(before.DirtySizes) == 0 || before.UnimportedBytes != 0 || before.DirtyUniqueBytes != dirtyBytes {
		t.Fatalf("dirty measurement = %+v", before)
	}
	writerOutboxRecord(t, s, 1, false)
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
		var err error
		after, err = measureContent(t.Context(), tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(after.DirtySizes) != 0 || after.DirtyUniqueBytes != 0 || after.UnimportedBytes != before.DirtyUniqueBytes || after.FullListBytes != before.FullListBytes {
		t.Fatalf("record measurement: before=%+v after=%+v", before, after)
	}
}

func TestWriterMeasureDistinctHashesAndLargeFileList(t *testing.T) {
	t.Parallel()
	s := writerOutboxStore(t)
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
		shared := contentHash([]byte("shared"))
		unimported := contentHash([]byte("pending"))
		for i := 0; i < 900; i++ {
			hash, size := shared, int64(6)
			if i == 2 {
				hash, size = unimported, 7
			}
			var raw [16]byte
			binary.BigEndian.PutUint64(raw[8:], uint64(i))
			path := "pages/p_" + idEncoding.EncodeToString(raw[:]) + "/page.json"
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO files(path,page,bytes,dirty_seq,hash) VALUES(?,NULL,?,'1',?)`, path, size, hash); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO snapshots(hash,data,refs) VALUES(?,?,9)`, unimported, []byte("pending")); err != nil {
			return err
		}
		m, err := measureContent(t.Context(), tx)
		if err != nil {
			return err
		}
		if len(m.DirtySizes) != 900 || m.DirtyUniqueBytes != 6 || m.UnimportedBytes != 7 {
			return fmt.Errorf("duplicate hash measurement: %+v", m)
		}
		files, err := currentContentFiles(t.Context(), tx)
		if err != nil {
			return err
		}
		encoded, err := CanonicalJSON(files)
		if err != nil {
			return err
		}
		if m.FullListBytes != int64(len(encoded)) {
			return fmt.Errorf("large file list: reserved=%d actual=%d", m.FullListBytes, len(encoded))
		}
		prior, err := CanonicalJSON([]File{{Path: "old.json", Hash: shared, Size: 6}})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(t.Context(), `UPDATE meta SET files=? WHERE singleton=1`, string(prior)); err != nil {
			return err
		}
		m, err = measureContent(t.Context(), tx)
		if err != nil {
			return err
		}
		if m.DirtyUniqueBytes != 0 || !slices.Contains(m.DirtySizes, 7) {
			return fmt.Errorf("reused imported hashes were charged again: %+v", m)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWriterMeasureReservationsAndImportCompletion(t *testing.T) {
	f := newWriterFixture(t)
	f.b.other.Store(100)
	if err := f.in.Remeasure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.in.measure.reserve(50); err != nil {
		t.Fatal(err)
	}
	f.in.measure.add(25)
	if n, ok := f.in.measure.get(); !ok || n != 175 {
		t.Fatalf("usage=%d valid=%t", n, ok)
	}
	if n, err := f.in.measure.agentOther(100); err != nil || n != 175 {
		t.Fatalf("agent usage=%d %v", n, err)
	}
	if err := f.in.measure.reserve(-51); err == nil {
		t.Fatal("reservation underflow accepted")
	}
	if err := f.in.measure.reserve(-50); err != nil {
		t.Fatal(err)
	}
	if err := f.in.Acknowledge(t.Context(), 1, 500); err != nil {
		t.Fatal(err)
	}
	if n, ok := f.in.measure.get(); !ok || n < 625 {
		t.Fatalf("imported bytes undercounted: %d %t", n, ok)
	}
	before, _ := f.in.measure.get()
	if err := f.in.Acknowledge(t.Context(), 1, 500); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.in.measure.get(); n != before {
		t.Fatalf("repeat acknowledgement charged twice: %d -> %d", before, n)
	}
	f.b.other.Store(625)
	if err := f.in.Remeasure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n, ok := f.in.measure.get(); !ok || n != 625 {
		t.Fatalf("fresh measurement=%d %t", n, ok)
	}
}

func TestWriterMeasureOverflowAndNegative(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		total int64
		add   int64
	}{
		{"overflow", math.MaxInt64, 1},
		{"negative", 0, -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			total := tt.total
			if err := addContentBytes(&total, tt.add); err == nil || total != tt.total {
				t.Fatalf("unsafe addition changed total to %d: %v", total, err)
			}
		})
	}
	counter := contentByteCounter(math.MaxInt64)
	if _, err := counter.Write([]byte("x")); err == nil {
		t.Fatal("file-list counter overflow accepted")
	}
	s := writerOutboxStore(t)
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO files(path,page,bytes,dirty_seq,hash) VALUES('notebook.json',NULL,-1,'1',?)`, contentHash(nil))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
		_, err := measureContent(t.Context(), tx)
		return err
	}); err == nil {
		t.Fatal("negative encoded size accepted")
	}
}
