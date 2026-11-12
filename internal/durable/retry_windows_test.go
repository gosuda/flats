package durable

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoveAllWaitsForAHolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "held"))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		f.Close()
	}()
	if err := RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("stat: %v", err)
	}
}

func TestRenameWaitsForAHolder(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "d")
	if err := os.Mkdir(from, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(from, "held"))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		f.Close()
	}()
	if err := Rename(from, filepath.Join(dir, "e")); err != nil {
		t.Fatal(err)
	}
}

func TestRetryGivesUpOnAHolderThatStays(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "held"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	start := time.Now()
	if err := RemoveAll(dir); err == nil {
		t.Fatal("no error")
	}
	if took := time.Since(start); took > retryFor+time.Second {
		t.Fatalf("took %s", took)
	}
}
