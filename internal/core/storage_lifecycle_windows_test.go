package core

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/store"
)

// Windows refuses to rename a directory while a file below it is open. That is
// the failure these paths meet there, so these tests inject it where
// TestStuckDeleteIsNotQuarantined and TestFailedRenameRestartsStoppedWorker use
// POSIX modes.
func holdOpen(t *testing.T, path string) (release func()) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { f.Close() }) }
	t.Cleanup(release)
	return release
}

func TestDeleteWithAnOpenFileKeepsItsSlugReserved(t *testing.T) {
	s, _ := newTestService(t)
	secret := createStorageFixture(t, s, "stuck-open")
	op := storageOperation{Kind: "delete", From: "stuck-open"}
	if err := s.beginStorageOperation(op); err != nil {
		t.Fatal(err)
	}
	if err := s.st.DeleteFlat(t.Context(), op.From, store.Audit{}); err != nil {
		t.Fatal(err)
	}
	release := holdOpen(t, secret)
	if err := s.recoverStorageOperations(t.Context()); err != nil {
		t.Fatalf("cleanup failure stopped startup: %v", err)
	}
	if got, err := readFile(secret); err != nil || got != "original secret" {
		t.Fatalf("pending delete storage moved: %q, %v", got, err)
	}
	if entries, err := os.ReadDir(s.orphanStorageRoot()); err != nil || len(entries) != 0 {
		t.Fatalf("pending delete storage quarantined: %v, %v", entries, err)
	}
	journal := filepath.Join(s.storageJournalDir(), op.fileName())
	if exists, err := pathExists(journal); err != nil || !exists {
		t.Fatalf("journal released before cleanup finished: %t, %v", exists, err)
	}
	if _, err := s.CreateFlat(t.Context(), op.From, "", ViaAPI); !errors.Is(err, ErrConflict) {
		t.Fatalf("unfinished delete permitted slug reuse: %v", err)
	}
	release()
	if err := s.recoverStorageOperations(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{s.flatDir(op.From), filepath.Join(s.deletedStorageRoot(), op.From), journal} {
		if exists, err := pathExists(path); err != nil || exists {
			t.Fatalf("retry left %q: %t, %v", path, exists, err)
		}
	}
}

func TestRenameWithAnOpenFileRestartsTheStoppedWorker(t *testing.T) {
	ctx := t.Context()
	s, _ := newTestService(t)
	r := &renameGuardRuntime{
		oldDir:     s.flatDir("origin"),
		oldDataDir: s.dataDirOf("origin"),
		newDataDir: s.dataDirOf("renamed"),
	}
	s.cfg.Runtime = r
	if _, err := s.SaveVersion(ctx, "origin", []bundle.File{
		{Path: "flats.json", Data: []byte(`{"kind":"server"}`)},
		{Path: "server.js", Data: []byte("export default { fetch() { return new Response('ready') } }")},
	}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := approvedInternalDeploy(t, s, ctx, "origin", 0); err != nil {
		t.Fatal(err)
	}
	held := filepath.Join(s.dataDirOf("origin"), "held")
	if err := writeFile(held, "x"); err != nil {
		t.Fatal(err)
	}
	release := holdOpen(t, held)
	if _, err := s.RenameSlug(ctx, "origin", "renamed", ViaAPI); err == nil {
		t.Fatal("rename succeeded with an open file")
	}
	release()
	if !r.stopped.Load() || r.rebuildCalled.Load() {
		t.Fatalf("worker state: stopped=%t rebuilt on new slug=%t", r.stopped.Load(), r.rebuildCalled.Load())
	}
	if _, err := s.st.GetFlat(ctx, "origin"); err != nil {
		t.Fatalf("failed rename lost the source metadata: %v", err)
	}
	response := httptest.NewRecorder()
	s.siteHandler("origin", false).ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "http://origin.test/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("flat answered %d after a failed rename, want its restarted worker", response.Code)
	}
	if err := s.checkStorageJournal("origin"); err != nil {
		t.Fatalf("failed rename left a journal: %v", err)
	}
}
