//go:build unix

package devreview

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO is refused by name, without opening it (which would block).
func TestSnapshotRefusesSpecialFiles(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{"a.txt": "a\n"})
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := Snapshot(root, "", Options{}); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "pipe") {
			t.Fatalf("err = %v, want a refusal naming the FIFO", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("snapshot hung on a FIFO")
	}

	// An ignored or excluded special file is not candidate content.
	write(t, root, map[string]string{".gitignore": "pipe\n"})
	if _, err := Snapshot(root, "", Options{}); err != nil {
		t.Errorf("ignored FIFO: %v", err)
	}
	write(t, root, map[string]string{".gitignore": ""})
	if _, err := Snapshot(root, "", Options{}); err == nil {
		t.Error("the FIFO is no longer ignored but was accepted")
	}
	if _, err := Snapshot(root, "", Options{Exclude: []string{"pipe"}}); err != nil {
		t.Errorf("excluded FIFO: %v", err)
	}
}

// Ignored and excluded directories are not entered: an unreadable cache
// does not break a snapshot, while an unreadable candidate directory does.
func TestSpecialFileScanSkipsIgnoredTrees(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable directories")
	}
	root := repo(t, map[string]string{"a.txt": "a\n", ".gitignore": "cache/\n"})
	write(t, root, map[string]string{"cache/x": "x\n", "excluded/y": "y\n", "a.txt": "b\n"})
	for _, d := range []string{"cache", "excluded"} {
		p := filepath.Join(root, d)
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(p, 0o755) })
	}
	m, err := Snapshot(root, "", Options{Exclude: []string{"excluded/"}})
	if err != nil {
		t.Fatalf("snapshot with unreadable ignored and excluded directories: %v", err)
	}
	if d, err := Verify(root, m); err != nil || !d.None() {
		t.Errorf("verify: %v, %v", d, err)
	}
	if _, err := Snapshot(root, "", Options{}); err == nil {
		t.Error("an unreadable directory that is neither ignored nor excluded was accepted")
	}
}

// A pattern without a trailing slash matches paths, not directories, so it
// does not hide a FIFO beneath a directory of the same name.
func TestSpecialFileScanKeepsPathPatterns(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{"a.txt": "a\n"})
	write(t, root, map[string]string{"cache/keep.txt": "k\n"})
	if err := syscall.Mkfifo(filepath.Join(root, "cache", "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Snapshot(root, "", Options{Exclude: []string{"cache"}}); err == nil || !strings.Contains(err.Error(), "cache/pipe") {
		t.Errorf("err = %v, want cache/pipe refused", err)
	}
	if _, err := Snapshot(root, "", Options{Exclude: []string{"cache/"}}); err != nil {
		t.Errorf("a directory prefix exclusion: %v", err)
	}
}
