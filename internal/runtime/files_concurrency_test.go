package runtime

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func concurrently(n int, fn func(int)) {
	start := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(n)
	for i := range n {
		done.Go(func() { ready.Done(); <-start; fn(i) })
	}
	ready.Wait()
	close(start)
	done.Wait()
}

func assertFilesAccounting(t *testing.T, d *dataStore) {
	t.Helper()
	var disk int64
	err := filepath.WalkDir(filepath.Join(d.dir, "files"), func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.Type().IsRegular() {
			info, err := e.Info()
			if err != nil {
				return err
			}
			disk += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.filesUsed.Load(); got != disk {
		t.Fatalf("FILES cached %d, disk %d", got, disk)
	}
}

func TestConcurrentFilesQuota(t *testing.T) {
	d := newDataStore(t.TempDir())
	defer d.Close()
	root, err := d.filesRoot(true)
	if err != nil {
		t.Fatal(err)
	}
	f, err := root.OpenFile("sparse", os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxFilesTotal - 5); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	errs := make([]error, 2)
	concurrently(2, func(i int) { errs[i] = d.filePut(fmt.Sprintf("key%d", i), "1234") })
	successes := 0
	for _, err := range errs {
		if err == nil {
			successes++
		} else if !strings.Contains(err.Error(), "storage is full") {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("quota allowed %d concurrent writes", successes)
	}
	assertFilesAccounting(t, d)
}

func TestConcurrentFilesSameKey(t *testing.T) {
	d := newDataStore(t.TempDir())
	defer d.Close()
	if err := d.filePut("shared", "initial"); err != nil {
		t.Fatal(err)
	}
	concurrently(32, func(i int) {
		if err := d.filePut("shared", strings.Repeat("x", i+1)); err != nil {
			t.Error(err)
		}
	})
	assertFilesAccounting(t, d)
}

func TestConcurrentFilesPutDelete(t *testing.T) {
	d := newDataStore(t.TempDir())
	defer d.Close()
	if err := d.filePut("shared", "initial"); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		concurrently(32, func(i int) {
			key := fmt.Sprintf("dir/key%d", i%4)
			if i%3 == 0 {
				if _, err := d.fileDelete(key); err != nil {
					t.Error(err)
				}
			} else if err := d.filePut(key, strings.Repeat("x", i+1)); err != nil {
				t.Error(err)
			}
		})
		assertFilesAccounting(t, d)
	}
}
