//go:build unix

package devreview

import (
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
