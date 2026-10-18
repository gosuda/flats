package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
)

// imageRuntime is contentRuntime whose instances behave like a worker that
// keeps writing its document image (temporary file, then rename) until it
// is stopped.
type imageRuntime struct {
	*contentRuntime
	mu     sync.Mutex
	writes int
}

type imageInstance struct {
	Instance
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

func (rt *imageRuntime) Start(ctx context.Context, spec RuntimeSpec) (Instance, error) {
	inst, err := rt.contentRuntime.Start(ctx, spec)
	if err != nil || spec.ImagePath == "" {
		return inst, err
	}
	in := &imageInstance{Instance: inst, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(in.done)
		for {
			// A worker never creates directories.
			tmp := spec.ImagePath + ".tmp"
			if os.WriteFile(tmp, []byte("image"), 0o600) == nil && os.Rename(tmp, spec.ImagePath) == nil {
				rt.mu.Lock()
				rt.writes++
				rt.mu.Unlock()
			}
			select {
			case <-in.stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	return in, nil
}

// Stop returns after the "worker" exited, as Manager's does.
func (in *imageInstance) Stop() {
	in.once.Do(func() { close(in.stop) })
	<-in.done
	in.Instance.Stop()
}

func (rt *imageRuntime) imageWrites() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.writes
}

func docsStates(t *testing.T, dataDir string) []string {
	t.Helper()
	states, err := filepath.Glob(filepath.Join(dataDir, "runtime", "docs", "*"+docsStateSuffix))
	if err != nil {
		t.Fatal(err)
	}
	return states
}

func TestDocsImagePathLifecycle(t *testing.T) {
	s, dir := newTestService(t)
	rt := &contentRuntime{}
	s.cfg.Runtime = rt
	s.cfg.DocsApp = testDocsApp()
	saveDocs(t, s, "notes")
	var live *RuntimeSpec
	for i := range rt.specs {
		spec := &rt.specs[i]
		if spec.ManagedDocs {
			live = spec
		} else if spec.ImagePath != "" {
			t.Fatalf("scratch or trial runtime got an image path: %+v", spec)
		}
	}
	if live == nil {
		t.Fatal("no live managed document started")
	}
	state := live.Dir + docsStateSuffix
	if live.ImagePath != filepath.Join(state, docsImageName) {
		t.Fatalf("image path %q for runtime %q", live.ImagePath, live.Dir)
	}
	fi, err := os.Stat(state)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("state directory: %v %v", fi, err)
	}
	// An isolated start (a scratch trial) shares the runtime directory but
	// gets no image path.
	v, err := s.st.GetVersion(t.Context(), "notes", 1)
	if err != nil {
		t.Fatal(err)
	}
	isolated, err := s.build(t.Context(), "notes", v, filepath.Join(dir, "isolated"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec := rt.specs[len(rt.specs)-1]; spec.ImagePath != "" || spec.Dir != live.Dir {
		t.Fatalf("isolated start: %+v", spec)
	}
	// Leftovers of an interrupted image write are removed when the directory
	// is acquired with no reference, as after a host crash; the image stays.
	if err := os.WriteFile(live.ImagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "image.123.tmp"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.docsMu.Lock()
	refs := s.docsRefs[live.Dir]
	s.docsRefs[live.Dir] = 0
	s.docsMu.Unlock()
	again, err := s.build(t.Context(), "notes", v, filepath.Join(dir, "isolated2"), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.docsMu.Lock()
	s.docsRefs[live.Dir] += refs
	s.docsMu.Unlock()
	entries, err := os.ReadDir(state)
	if err != nil || len(entries) != 1 || entries[0].Name() != docsImageName {
		t.Fatalf("state after acquisition: %v %v", entries, err)
	}
	// A referenced acquisition leaves a running worker's temporary file alone.
	if err := os.WriteFile(filepath.Join(state, "image.456.tmp"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := s.build(t.Context(), "notes", v, filepath.Join(dir, "isolated3"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "image.456.tmp")); err != nil {
		t.Fatal("referenced acquisition removed a worker's temporary file")
	}
	third.inst.Stop()
	// Startup cleanup keeps the state of referenced directories and removes
	// unreferenced runtime and state directories.
	parent := filepath.Join(dir, "runtime", "docs")
	for _, stale := range []string{"stale", "stale" + docsStateSuffix, "orphan" + docsStateSuffix} {
		if err := os.MkdirAll(filepath.Join(parent, stale), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	s.cleanupDocs()
	if _, err := os.Stat(live.ImagePath); err != nil {
		t.Fatal("cleanup removed a referenced directory's image")
	}
	for _, stale := range []string{"stale", "stale" + docsStateSuffix, "orphan" + docsStateSuffix} {
		if _, err := os.Stat(filepath.Join(parent, stale)); !os.IsNotExist(err) {
			t.Fatalf("cleanup kept %s: %v", stale, err)
		}
	}
	// The last release removes the state directory with the runtime directory.
	isolated.inst.Stop()
	again.inst.Stop()
	if _, err := os.Stat(state); err != nil {
		t.Fatal("state removed while referenced")
	}
	lf := s.state("notes")
	lf.cur.Swap(nil).inst.Stop()
	if states := docsStates(t, dir); len(states) != 0 {
		t.Fatalf("state directories remain: %v", states)
	}
	if _, err := os.Stat(live.Dir); !os.IsNotExist(err) {
		t.Fatalf("runtime directory remains: %v", err)
	}
}

// Deleting or replacing a document while its worker writes the image leaves
// no state directory, and the worker cannot write one afterwards.
func TestDocsImageStateRemovedOnDeleteAndReplace(t *testing.T) {
	s, dir := newTestService(t)
	rt := &imageRuntime{contentRuntime: &contentRuntime{}}
	s.cfg.Runtime = rt
	s.cfg.DocsApp = testDocsApp()
	saveDocs(t, s, "deleted")
	saveDocs(t, s, "replaced")
	images := map[string]string{}
	for _, spec := range rt.specs {
		if spec.ImagePath != "" {
			images[spec.Flat] = spec.ImagePath
		}
	}
	if len(images) != 2 || images["deleted"] != images["replaced"] {
		// Identical content shares one runtime directory and its image.
		t.Fatalf("image paths %v", images)
	}
	deadline := time.Now().Add(10 * time.Second)
	for rt.imageWrites() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if rt.imageWrites() == 0 {
		t.Fatal("no image written")
	}
	if _, err := s.Delete(t.Context(), "deleted", ViaConsole, ""); err != nil {
		t.Fatal(err)
	}
	// The other document still holds the shared directory.
	if _, err := os.Stat(filepath.Dir(images["replaced"])); err != nil {
		t.Fatal("delete removed a directory another document uses")
	}
	if _, err := s.SaveVersion(t.Context(), "replaced", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte("# replaced")}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	old := filepath.Dir(images["replaced"])
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(old); os.IsNotExist(err) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("replaced document's state remains: %v", err)
	}
	if err := os.WriteFile(images["replaced"]+".tmp", []byte("late"), 0o600); err == nil {
		t.Fatal("a late image write succeeded after release")
	}
	var current []string
	for _, spec := range rt.specs {
		if spec.Flat == "replaced" && spec.ImagePath != "" && spec.ImagePath != images["replaced"] {
			current = append(current, filepath.Dir(spec.ImagePath))
		}
	}
	states := docsStates(t, dir)
	if len(current) == 0 || len(states) != 1 || states[0] != current[len(current)-1] {
		t.Fatalf("state directories %v, want only the replacement's %v", states, current)
	}
	if strings.Contains(states[0], filepath.Base(old)) {
		t.Fatal("replacement reuses the old directory")
	}
}
