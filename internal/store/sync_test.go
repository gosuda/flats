package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// After Sync a commit is in the main database file, so it no longer depends
// on write-ahead log frames that synchronous=NORMAL has not synced yet.
func TestSyncCheckpointsCommits(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.Now().UTC()
	if err := s.CreateFlat(ctx, Flat{Slug: "durable", Name: "durable", Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	// Open a copy of the main file alone, as if the log had been lost.
	raw, err := os.ReadFile(filepath.Join(dir, "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	if err := os.WriteFile(copyPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Open(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := c.GetFlat(ctx, "durable"); err != nil {
		t.Fatalf("committed flat missing from the main database file: %v", err)
	}
}
