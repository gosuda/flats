package core

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
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
