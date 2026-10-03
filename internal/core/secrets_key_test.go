package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentFirstStartKeyPublication(t *testing.T) {
	for range 10 {
		dir := t.TempDir()
		path := filepath.Join(dir, "secret.key")
		start, done := make(chan struct{}), make(chan struct{})
		var wg sync.WaitGroup
		keys := make([][]byte, 32)
		errs := make([]error, len(keys))
		for i := range keys {
			wg.Go(func() { <-start; keys[i], errs[i] = loadOrCreateKey(path) })
		}
		observed := make(chan error, 1)
		go func() {
			for {
				b, err := os.ReadFile(path)
				if err != nil && !os.IsNotExist(err) {
					observed <- err
					return
				}
				if err == nil && len(b) != 32 {
					observed <- fmt.Errorf("observed secret key length %d, want 32", len(b))
					return
				}
				select {
				case <-done:
					observed <- nil
					return
				default:
				}
			}
		}()
		close(start)
		wg.Wait()
		close(done)
		if err := <-observed; err != nil {
			t.Fatal(err)
		}
		disk, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, key := range keys {
			if errs[i] != nil || len(key) != 32 || string(key) != string(disk) {
				t.Fatalf("caller %d disagreed with published key: %v", i, errs[i])
			}
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("key permissions: %v %v", info, err)
		}
		files, err := os.ReadDir(dir)
		if err != nil || len(files) != 1 {
			t.Fatalf("temporary keys remain: %v %v", files, err)
		}
	}
}

func TestInvalidExistingKeyIsPreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	if err := os.WriteFile(path, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateKey(path); err == nil {
		t.Fatal("accepted malformed key")
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "invalid" {
		t.Fatal("replaced existing key")
	}
}

// Run the dangling-path case in a disposable process: a recursion regression
// must be killed at the deadline rather than leaking goroutines/temp keys.
func TestDanglingSecretKeySymlink(t *testing.T) {
	if os.Getenv("FLATS_TEST_DANGLING_KEY") == "1" {
		dir := os.Getenv("FLATS_TEST_KEY_DIR")
		path := filepath.Join(dir, "secret.key")
		_, err := loadOrCreateKey(path)
		if err == nil || !strings.Contains(err.Error(), path) || !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("dangling key error = %v", err)
		}
		files, err := os.ReadDir(dir)
		if err != nil || len(files) != 1 {
			t.Fatalf("temporary keys leaked: %v %v", files, err)
		}
		return
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.key")
	if err := os.Symlink(filepath.Join(dir, "unmounted-key"), path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDanglingSecretKeySymlink$")
	cmd.Env = append(os.Environ(), "FLATS_TEST_DANGLING_KEY=1", "FLATS_TEST_KEY_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("key load did not fail promptly and cleanly: %v %s", err, out)
	}
	target, err := os.Readlink(path)
	if err != nil || target != filepath.Join(dir, "unmounted-key") {
		t.Fatal("dangling symlink changed")
	}
}

func TestValidExistingKeyIsPreserved(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprintf("symlink=%t", symlink), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "secret.key")
			key := []byte(strings.Repeat("k", 32))
			target := path
			if symlink {
				target = filepath.Join(dir, "mounted-key")
			}
			if err := os.WriteFile(target, key, 0600); err != nil {
				t.Fatal(err)
			}
			if symlink {
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			got, err := loadOrCreateKey(path)
			if err != nil || string(got) != string(key) {
				t.Fatalf("existing key = %q, %v", got, err)
			}
			files, err := os.ReadDir(dir)
			want := 1
			if symlink {
				want = 2
			}
			if err != nil || len(files) != want {
				t.Fatalf("existing key path changed: %v %v", files, err)
			}
		})
	}
}
