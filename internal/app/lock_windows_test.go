package app

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/winservice"
)

// A service on another data directory leaves its definition in the default
// data directory. A later host there starts as on Unix.
func TestDefaultDataDirectoryWithOnlyTheServiceDefinitionStartsAHost(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LocalAppData", root)
	dir := filepath.Join(root, "Flats")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(winservice.DefinitionPath(dir), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := Start(context.Background(), localOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := lockDataDir(dir) // what flats config init does
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestTheLockFileNamesTheRoleOfTheService(t *testing.T) {
	for _, tt := range []struct {
		byTask bool
		want   string
	}{{false, strconv.Itoa(os.Getpid())}, {true, strconv.Itoa(os.Getpid()) + " " + winservice.ServiceRole}} {
		startedByTask = tt.byTask
		dir := t.TempDir()
		f, err := lockDataDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dir, "flats.lock"))
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(b)); got != tt.want {
			t.Errorf("by task %v: lock file %q, want %q", tt.byTask, got, tt.want)
		}
	}
	startedByTask = false
}
