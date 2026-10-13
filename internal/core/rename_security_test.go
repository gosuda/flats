package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/store"
)

// The worker intentionally opens its data directory lazily. Rename must stop
// that worker before the directory moves, even if the replacement cannot start.
type renameGuardRuntime struct {
	oldDir         string
	oldDataDir     string
	newDataDir     string
	failNew        bool
	stopped        atomic.Bool
	stopBeforeMove atomic.Bool
	rebuildCalled  atomic.Bool
}

type renameGuardInstance struct {
	http.Handler
	onStop func()
}

func (i *renameGuardInstance) Stop() {
	if i.onStop != nil {
		i.onStop()
	}
}

func (r *renameGuardRuntime) Start(_ context.Context, spec RuntimeSpec) (Instance, error) {
	if spec.DataDir == r.newDataDir {
		r.rebuildCalled.Store(true)
		if !r.stopped.Load() {
			return nil, errors.New("replacement started before old worker stopped")
		}
		if _, err := os.Lstat(r.oldDir); !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("old slug path still exists during replacement: %v", err)
		}
		if r.failNew {
			return nil, errors.New("injected replacement failure")
		}
	}
	inst := &renameGuardInstance{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ready")
	})}
	if spec.DataDir == r.oldDataDir {
		inst.onStop = func() {
			if _, err := os.Lstat(r.oldDir); err == nil {
				r.stopBeforeMove.Store(true)
			}
			r.stopped.Store(true)
		}
	}
	return inst, nil
}

func TestRenameStopsWorkerBeforeMovingData(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failNew bool
		status  int
	}{
		{name: "replacement succeeds", status: http.StatusOK},
		{name: "replacement fails closed", failNew: true, status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, _ := newTestService(t)
			r := &renameGuardRuntime{
				oldDir:     s.flatDir("origin"),
				oldDataDir: s.dataDirOf("origin"),
				newDataDir: s.dataDirOf("renamed"),
				failNew:    tc.failNew,
			}
			s.cfg.Runtime = r
			_, err := s.SaveVersion(ctx, "origin", []bundle.File{
				{Path: "flats.json", Data: []byte(`{"kind":"server"}`)},
				{Path: "server.js", Data: []byte("export default { fetch() { return new Response('ready') } }")},
			}, SaveMeta{}, ViaAPI)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := approvedInternalDeploy(t, s, ctx, "origin", 0); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RenameSlug(ctx, "origin", "renamed", ViaAPI); err != nil {
				t.Fatal(err)
			}
			if !r.stopped.Load() || !r.stopBeforeMove.Load() || !r.rebuildCalled.Load() {
				t.Fatalf("rename ordering: stopped=%t beforeMove=%t rebuilt=%t",
					r.stopped.Load(), r.stopBeforeMove.Load(), r.rebuildCalled.Load())
			}
			if _, err := os.Lstat(r.oldDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("old slug path reappeared: %v", err)
			}
			response := httptest.NewRecorder()
			s.siteHandler("renamed", false).ServeHTTP(response,
				httptest.NewRequest(http.MethodGet, "http://renamed.test/", nil))
			if response.Code != tc.status {
				t.Fatalf("renamed flat answered %d, want %d", response.Code, tc.status)
			}
			if got := s.live["renamed"].cur.Load() == nil; got != tc.failNew {
				t.Fatalf("replacement failure state: unavailable=%t failNew=%t", got, tc.failNew)
			}
		})
	}
}

func TestUnownedFlatPathCannotBeReused(t *testing.T) {
	s, dir := newTestService(t)
	ctx := context.Background()
	if _, err := s.CreateFlat(ctx, "source", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "flats", "source")
	unowned := filepath.Join(dir, "flats", "unowned")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(unowned, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(unowned, "unrelated-data")
	if err := os.WriteFile(marker, []byte("not a flat"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFlat(ctx, "unowned", "", ViaAPI); !errors.Is(err, ErrConflict) {
		t.Fatalf("creation over residual storage: %v", err)
	}
	if _, err := s.RenameSlug(ctx, "source", "unowned", ViaAPI); !errors.Is(err, ErrConflict) {
		t.Fatalf("rename over residual storage: %v", err)
	}
	if _, err := s.st.GetFlat(ctx, "source"); err != nil {
		t.Fatalf("failed rename lost the source: %v", err)
	}
	if _, err := s.st.GetFlat(ctx, "unowned"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("residual directory acquired a flat record: %v", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "not a flat" {
		t.Fatalf("unowned contents changed: %q, %v", got, err)
	}
}
