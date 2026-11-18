//go:build unix

package app

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
)

// The fixture's cleanup must not return while a pre-started worker still
// runs: it prepares under the data directory, which t.TempDir removes next.
// The worker is held after it received its prewarm message, so it is still
// starting when the cleanup stops the last instance.
func TestDocsRuntimeCleanupWaitsForPreStartedWorker(t *testing.T) {
	hold := t.TempDir()
	release := func() { os.WriteFile(filepath.Join(hold, "release"), nil, 0o600) }
	t.Cleanup(release)
	f := newDocsRuntimeFixture(t, spareHoldEnv+"="+hold)
	files := []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte("# Held")}}
	if _, err := f.svc.SaveVersion(t.Context(), "documents", files, core.SaveMeta{}, core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	pid := heldWorker(t, hold)
	f.cleanup()
	running := syscall.Kill(pid, 0)
	release()
	if running == nil || errors.Is(running, syscall.EPERM) {
		t.Fatalf("cleanup returned while pre-started worker %d still runs", pid)
	}
}

// heldWorker waits until a pre-started worker is held in dir and returns its
// process id.
func heldWorker(t *testing.T, dir string) int {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if pid, err := strconv.Atoi(e.Name()); err == nil {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no pre-started worker started")
	return 0
}
