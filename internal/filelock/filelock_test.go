package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openLock(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestTryLockRefusesASecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first, second := openLock(t, path), openLock(t, path)
	if err := TryLock(first); err != nil {
		t.Fatal(err)
	}
	if err := TryLock(second); !errors.Is(err, ErrLocked) {
		t.Fatalf("second holder: got %v, want ErrLocked", err)
	}
	if err := Unlock(first); err != nil {
		t.Fatal(err)
	}
	if err := TryLock(second); err != nil {
		t.Fatalf("after unlock: %v", err)
	}
}

func TestCloseReleasesTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first, second := openLock(t, path), openLock(t, path)
	if err := TryLock(first); err != nil {
		t.Fatal(err)
	}
	first.Close()
	if err := TryLock(second); err != nil {
		t.Fatalf("after close: %v", err)
	}
}

func TestLockWaitsForTheHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first, second := openLock(t, path), openLock(t, path)
	if err := Lock(first); err != nil {
		t.Fatal(err)
	}
	acquired := make(chan error, 1)
	go func() { acquired <- Lock(second) }()
	select {
	case err := <-acquired:
		t.Fatalf("Lock returned while the file was locked: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := Unlock(first); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Lock did not return after the holder released the lock")
	}
}

func TestALockedFileStaysReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	holder := openLock(t, path)
	if _, err := holder.WriteString("1234\n"); err != nil {
		t.Fatal(err)
	}
	if err := TryLock(holder); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read a locked file: %v", err)
	}
	if string(b) != "1234\n" {
		t.Fatalf("content %q", b)
	}
}
