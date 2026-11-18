package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSheetMeasurementHonorsItsContext measures a sheet's directory with a
// context that has ended: the walk stops with the context's error and
// reports no total, where it reports the bytes beside sheet.db otherwise.
func TestSheetMeasurementHonorsItsContext(t *testing.T) {
	s, _ := newTestService(t)
	dir := s.flatDir("budget")
	for name, size := range map[string]int{
		"sheet/sheet.db": 4096, "sheet/sheet.db-wal": 512, "sheet/sheet.db-shm": 64,
		"revisions/blobs/aa/one": 1000, "revisions/blobs/bb/two": 234,
	} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b := sheetBackend{s: s, slug: "budget"}
	if n, err := b.MeasureOther(context.Background()); err != nil || n != 1234 {
		t.Fatalf("the measurement = %d, %v, want the 1234 bytes beside sheet.db", n, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if n, err := b.MeasureOther(cancelled); !errors.Is(err, context.Canceled) || n != 0 {
		t.Fatalf("a measurement with a cancelled context = %d, %v", n, err)
	}
	late, done := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer done()
	if n, err := b.MeasureOther(late); !errors.Is(err, context.DeadlineExceeded) || n != 0 {
		t.Fatalf("a measurement past its deadline = %d, %v", n, err)
	}
}
