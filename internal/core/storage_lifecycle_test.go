package core

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/store"
)

func createStorageFixture(t *testing.T, s *Service, slugName string) string {
	t.Helper()
	if _, err := s.SaveVersion(t.Context(), slugName, []bundle.File{
		{Path: "index.html", Data: []byte("owned by " + slugName)},
	}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.flatDir(slugName), "data", "files", "secret")
	if err := writeFile(path, "original secret"); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCreationRejectsUnownedStorage(t *testing.T) {
	s, _ := newTestService(t)
	path := filepath.Join(s.flatDir("orphaned"), "data", "files", "secret")
	if err := writeFile(path, "previous owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFlat(t.Context(), "orphaned", "", ViaAPI); !errors.Is(err, ErrConflict) {
		t.Fatalf("creation adopted orphan storage: %v", err)
	}
	if _, err := s.SaveVersion(t.Context(), "orphaned", []bundle.File{
		{Path: "index.html", Data: []byte("new owner")},
	}, SaveMeta{}, ViaAPI); !errors.Is(err, ErrConflict) {
		t.Fatalf("upload adopted orphan storage: %v", err)
	}
	if got, err := readFile(path); err != nil || got != "previous owner" {
		t.Fatalf("orphan data changed: %q, %v", got, err)
	}
	if _, err := s.st.GetFlat(t.Context(), "orphaned"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("orphan incorrectly acquired metadata: %v", err)
	}
}

func TestRenameRejectsOccupiedStorageDestination(t *testing.T) {
	s, _ := newTestService(t)
	createStorageFixture(t, s, "rename-source")
	target := filepath.Join(s.flatDir("rename-target"), "data", "files", "secret")
	if err := writeFile(target, "previous owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameSlug(t.Context(), "rename-source", "rename-target", ViaAPI); !errors.Is(err, ErrConflict) {
		t.Fatalf("rename overwrote an unowned target: %v", err)
	}
	if got, err := readFile(target); err != nil || got != "previous owner" {
		t.Fatalf("target storage changed: %q, %v", got, err)
	}
	if _, err := s.st.GetFlat(t.Context(), "rename-source"); err != nil {
		t.Fatalf("source metadata lost: %v", err)
	}
}

func TestRecoverInterruptedStorageRename(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "before metadata commit"
		if committed {
			name = "after metadata commit"
		}
		t.Run(name, func(t *testing.T) {
			s, _ := newTestService(t)
			createStorageFixture(t, s, "rename-old")
			op := storageOperation{Kind: "rename", From: "rename-old", To: "rename-new"}
			if err := s.beginStorageOperation(op); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(s.flatDir(op.From), s.flatDir(op.To)); err != nil {
				t.Fatal(err)
			}
			if committed {
				if err := s.st.RenameFlat(t.Context(), op.From, op.To, time.Time{}, s.now()); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.recoverStorageOperations(t.Context()); err != nil {
				t.Fatal(err)
			}
			owner, abandoned := op.From, op.To
			if committed {
				owner, abandoned = op.To, op.From
			}
			if got, err := readFile(filepath.Join(s.flatDir(owner), "data", "files", "secret")); err != nil || got != "original secret" {
				t.Fatalf("owner %q lost data: %q, %v", owner, got, err)
			}
			if exists, err := pathExists(s.flatDir(abandoned)); err != nil || exists {
				t.Fatalf("abandoned slug %q retained directory: %t, %v", abandoned, exists, err)
			}
			if _, err := s.st.GetFlat(t.Context(), owner); err != nil {
				t.Fatalf("owner metadata lost: %v", err)
			}
			if exists, err := pathExists(filepath.Join(s.storageJournalDir(), op.fileName())); err != nil || exists {
				t.Fatalf("recovery retained journal: %t, %v", exists, err)
			}
		})
	}
}

func TestRecoverInterruptedStorageDelete(t *testing.T) {
	for _, phase := range []string{"before metadata commit", "after metadata commit", "after quarantine"} {
		t.Run(phase, func(t *testing.T) {
			s, _ := newTestService(t)
			createStorageFixture(t, s, "delete-old")
			op := storageOperation{Kind: "delete", From: "delete-old"}
			if err := s.beginStorageOperation(op); err != nil {
				t.Fatal(err)
			}
			if phase != "before metadata commit" {
				if err := s.st.DeleteFlat(t.Context(), op.From); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "after quarantine" {
				if err := os.Rename(s.flatDir(op.From), filepath.Join(s.deletedStorageRoot(), op.From)); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.recoverStorageOperations(t.Context()); err != nil {
				t.Fatal(err)
			}
			exists, err := pathExists(s.flatDir(op.From))
			if err != nil {
				t.Fatal(err)
			}
			if phase == "before metadata commit" {
				if !exists {
					t.Fatal("recovery removed storage belonging to a retained flat")
				}
				if _, err := s.st.GetFlat(t.Context(), op.From); err != nil {
					t.Fatalf("retained metadata lost: %v", err)
				}
			} else {
				if exists {
					t.Fatal("deleted flat storage remained under a reusable slug")
				}
				if _, err := s.st.GetFlat(t.Context(), op.From); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("deleted flat metadata still exists: %v", err)
				}
			}
			if exists, err := pathExists(filepath.Join(s.deletedStorageRoot(), op.From)); err != nil || exists {
				t.Fatalf("recovery retained deleted storage: %t, %v", exists, err)
			}
			if exists, err := pathExists(filepath.Join(s.storageJournalDir(), op.fileName())); err != nil || exists {
				t.Fatalf("recovery retained journal: %t, %v", exists, err)
			}
		})
	}
}

func TestUnfinishedDeleteBlocksReuseEvenAfterDirectoryIsGone(t *testing.T) {
	s, _ := newTestService(t)
	createStorageFixture(t, s, "pending-delete")
	op := storageOperation{Kind: "delete", From: "pending-delete"}
	if err := s.beginStorageOperation(op); err != nil {
		t.Fatal(err)
	}
	if err := s.st.DeleteFlat(t.Context(), op.From); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(s.flatDir(op.From)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFlat(t.Context(), op.From, "", ViaAPI); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending delete permitted slug reuse: %v", err)
	}
	if err := s.recoverStorageOperations(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFlat(t.Context(), op.From, "", ViaAPI); err != nil {
		t.Fatalf("completed recovery did not release slug: %v", err)
	}
}

func TestStartupQuarantinesLegacyOrphan(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	orphan := filepath.Join(dir, "flats", "legacy-orphan", "data", "files", "secret")
	if err := writeFile(orphan, "previous owner"); err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, Config{
		DataDir: dir,
		Store:   st,
		Private: &memNet{hosts: make(map[string]http.Handler)},
		Logf:    t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	quarantined := filepath.Join(s.storageJournalDir(), "orphans", "legacy-orphan", "data", "files", "secret")
	if got, err := readFile(quarantined); err != nil || got != "previous owner" {
		t.Fatalf("legacy orphan not quarantined: %q, %v", got, err)
	}
	if _, err := s.CreateFlat(ctx, "legacy-orphan", "", ViaAPI); err != nil {
		t.Fatalf("quarantined slug should be reusable without inherited data: %v", err)
	}
	if exists, err := pathExists(s.flatDir("legacy-orphan")); err != nil || exists {
		t.Fatalf("new flat inherited a storage directory: %t, %v", exists, err)
	}
}

// Startup runs on every restart: a second orphan of the same name, or a
// stray non-journal file, must neither replace earlier quarantined data nor
// stop the host.
func TestRepeatedStartupQuarantineKeepsEarlierOrphans(t *testing.T) {
	s, _ := newTestService(t)
	if err := writeFile(filepath.Join(s.storageJournalDir(), ".DS_Store"), "finder"); err != nil {
		t.Fatal(err)
	}
	for i, body := range []string{"first", "second"} {
		if err := writeFile(filepath.Join(s.flatDir("twice-orphan"), "data", "files", "secret"), body); err != nil {
			t.Fatal(err)
		}
		if err := s.recoverStorageOperations(t.Context()); err != nil {
			t.Fatalf("startup %d: %v", i+1, err)
		}
	}
	if err := s.recoverStorageOperations(t.Context()); err != nil {
		t.Fatalf("idle startup: %v", err)
	}
	orphans := filepath.Join(s.storageJournalDir(), "orphans")
	for name, want := range map[string]string{"twice-orphan": "first", "twice-orphan.1": "second"} {
		if got, err := readFile(filepath.Join(orphans, name, "data", "files", "secret")); err != nil || got != want {
			t.Fatalf("quarantined %s = %q, %v; want %q", name, got, err, want)
		}
	}
	if exists, err := pathExists(s.flatDir("twice-orphan")); err != nil || exists {
		t.Fatalf("orphan stayed at its slug path: %t, %v", exists, err)
	}
	if _, err := s.CreateFlat(t.Context(), "twice-orphan", "", ViaAPI); err != nil {
		t.Fatalf("slug not reusable after quarantine: %v", err)
	}
}

// A committed deletion whose files cannot be removed yet must not stop the
// host. The journal stays and keeps the slug reserved until a later startup
// finishes the removal.
func TestDeleteCleanupFailureKeepsSlugReserved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s, _ := newTestService(t)
	createStorageFixture(t, s, "stuck-delete")
	op := storageOperation{Kind: "delete", From: "stuck-delete"}
	if err := s.beginStorageOperation(op); err != nil {
		t.Fatal(err)
	}
	if err := s.st.DeleteFlat(t.Context(), op.From); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(s.flatDir(op.From), "data", "files")
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Chmod(filepath.Join(filepath.Join(s.deletedStorageRoot(), op.From), "data", "files"), 0o700)
	})
	if err := s.recoverStorageOperations(t.Context()); err != nil {
		t.Fatalf("cleanup failure stopped startup: %v", err)
	}
	journal := filepath.Join(s.storageJournalDir(), op.fileName())
	if exists, err := pathExists(journal); err != nil || !exists {
		t.Fatalf("journal released before cleanup finished: %t, %v", exists, err)
	}
	if exists, err := pathExists(s.flatDir(op.From)); err != nil || exists {
		t.Fatalf("deleted storage stayed at its slug path: %t, %v", exists, err)
	}
	if _, err := s.CreateFlat(t.Context(), op.From, "", ViaAPI); !errors.Is(err, ErrConflict) {
		t.Fatalf("unfinished delete permitted slug reuse: %v", err)
	}
	if err := os.Chmod(filepath.Join(filepath.Join(s.deletedStorageRoot(), op.From), "data", "files"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverStorageOperations(t.Context()); err != nil {
		t.Fatal(err)
	}
	if exists, err := pathExists(journal); err != nil || exists {
		t.Fatalf("retry kept the journal: %t, %v", exists, err)
	}
	if exists, err := pathExists(filepath.Join(s.deletedStorageRoot(), op.From)); err != nil || exists {
		t.Fatalf("retry kept deleted storage: %t, %v", exists, err)
	}
	if _, err := s.CreateFlat(t.Context(), op.From, "", ViaAPI); err != nil {
		t.Fatalf("finished delete did not release the slug: %v", err)
	}
}

// Recovery never guesses: with a directory at both slugs it stops and
// leaves both untouched, and it never assigns storage of a flat that no
// longer exists to anyone.
func TestRenameRecoveryRefusesAmbiguousStorage(t *testing.T) {
	t.Run("both directories", func(t *testing.T) {
		s, _ := newTestService(t)
		createStorageFixture(t, s, "amb-old")
		op := storageOperation{Kind: "rename", From: "amb-old", To: "amb-new"}
		if err := s.beginStorageOperation(op); err != nil {
			t.Fatal(err)
		}
		stray := filepath.Join(s.flatDir(op.To), "data", "files", "secret")
		if err := writeFile(stray, "stray"); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if err := s.recoverStorageOperations(t.Context()); err == nil {
				t.Fatal("recovery guessed between two directories")
			}
		}
		if got, err := readFile(filepath.Join(s.flatDir(op.From), "data", "files", "secret")); err != nil || got != "original secret" {
			t.Fatalf("source changed: %q, %v", got, err)
		}
		if got, err := readFile(stray); err != nil || got != "stray" {
			t.Fatalf("destination changed: %q, %v", got, err)
		}
	})
	t.Run("no owner", func(t *testing.T) {
		s, _ := newTestService(t)
		createStorageFixture(t, s, "gone-old")
		op := storageOperation{Kind: "rename", From: "gone-old", To: "gone-new"}
		if err := s.beginStorageOperation(op); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(s.flatDir(op.From), s.flatDir(op.To)); err != nil {
			t.Fatal(err)
		}
		if err := s.st.DeleteFlat(t.Context(), op.From); err != nil {
			t.Fatal(err)
		}
		if err := s.recoverStorageOperations(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got, err := readFile(filepath.Join(s.storageJournalDir(), "orphans", op.To, "data", "files", "secret")); err != nil || got != "original secret" {
			t.Fatalf("unowned storage not preserved in quarantine: %q, %v", got, err)
		}
		for _, slugName := range []string{op.From, op.To} {
			if _, err := s.CreateFlat(t.Context(), slugName, "", ViaAPI); err != nil {
				t.Fatalf("create %s: %v", slugName, err)
			}
			if exists, err := pathExists(s.flatDir(slugName)); err != nil || exists {
				t.Fatalf("new flat %s inherited storage: %t, %v", slugName, exists, err)
			}
		}
	})
}

// A rename that fails before the metadata commit must restart the worker it
// stopped, on the old slug, and leave no journal that blocks a later retry.
func TestFailedRenameRestartsStoppedWorker(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based failure injection does not apply to root")
	}
	for _, tc := range []struct {
		name   string
		locked func(s *Service) string
	}{
		{name: "journal cannot be written", locked: (*Service).storageJournalDir},
		{name: "storage cannot be moved", locked: func(s *Service) string { return filepath.Dir(s.flatDir("origin")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			locked := tc.locked(s)
			if err := os.Chmod(locked, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(locked, 0o700) })
			if _, err := s.RenameSlug(ctx, "origin", "renamed", ViaAPI); err == nil {
				t.Fatal("rename succeeded despite the injected failure")
			}
			if err := os.Chmod(locked, 0o700); err != nil {
				t.Fatal(err)
			}
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
		})
	}
}

// A deletion whose files cannot be moved to the trash keeps them at their
// path, reserved by the journal, instead of quarantining them; otherwise the
// next startup would release the journal and keep the deleted files forever.
func TestStuckDeleteIsNotQuarantined(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s, _ := newTestService(t)
	secret := createStorageFixture(t, s, "stuck-move")
	op := storageOperation{Kind: "delete", From: "stuck-move"}
	if err := s.beginStorageOperation(op); err != nil {
		t.Fatal(err)
	}
	if err := s.st.DeleteFlat(t.Context(), op.From); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.deletedStorageRoot(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(s.deletedStorageRoot(), 0o700) })
	if err := s.recoverStorageOperations(t.Context()); err != nil {
		t.Fatalf("cleanup failure stopped startup: %v", err)
	}
	if got, err := readFile(secret); err != nil || got != "original secret" {
		t.Fatalf("pending delete storage moved: %q, %v", got, err)
	}
	if entries, err := os.ReadDir(s.orphanStorageRoot()); err != nil || len(entries) != 0 {
		t.Fatalf("pending delete storage quarantined: %v, %v", entries, err)
	}
	if _, err := s.CreateFlat(t.Context(), op.From, "", ViaAPI); !errors.Is(err, ErrConflict) {
		t.Fatalf("unfinished delete permitted slug reuse: %v", err)
	}
	if err := os.Chmod(s.deletedStorageRoot(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverStorageOperations(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{s.flatDir(op.From), filepath.Join(s.deletedStorageRoot(), op.From), filepath.Join(s.storageJournalDir(), op.fileName())} {
		if exists, err := pathExists(path); err != nil || exists {
			t.Fatalf("retry left %q: %t, %v", path, exists, err)
		}
	}
	if entries, err := os.ReadDir(s.orphanStorageRoot()); err != nil || len(entries) != 0 {
		t.Fatalf("deleted storage ended in quarantine: %v, %v", entries, err)
	}
}

// keepRenameJournal leaves the state of a rename whose rollback failed: the
// database still owns from, its files sit at to, and the journal stays for
// startup recovery.
func keepRenameJournal(t *testing.T, s *Service, from, to string) storageOperation {
	t.Helper()
	op := storageOperation{Kind: "rename", From: from, To: to}
	if err := s.beginStorageOperation(op); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(s.flatDir(from), s.flatDir(to)); err != nil {
		t.Fatal(err)
	}
	return op
}

func saveStatic(t *testing.T, s *Service, slugName, body string) error {
	t.Helper()
	_, err := s.SaveVersion(t.Context(), slugName, []bundle.File{{Path: "index.html", Data: []byte(body)}}, SaveMeta{}, ViaAPI)
	return err
}

// After a rename that could not roll back its storage, no operation may
// write below the old (or new) slug until startup recovery moves the files
// back: a recreated directory would leave recovery with two candidates and
// stop the host. Reads continue, and recovery restores normal operation.
func TestKeptRenameJournalRefusesStorageWrites(t *testing.T) {
	ctx := t.Context()
	s, _ := newTestService(t)
	for _, body := range []string{"v1", "v2"} {
		if err := saveStatic(t, s, "origin", body); err != nil {
			t.Fatal(err)
		}
		if _, err := approvedInternalDeploy(t, s, ctx, "origin", 0); err != nil {
			t.Fatal(err)
		}
	}
	// Version 3 is saved but not live; a later Draft waits for its publish.
	if err := saveStatic(t, s, "origin", "v3"); err != nil {
		t.Fatal(err)
	}
	pub, err := s.Publish(ctx, "origin", 0, "", ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, pub.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := saveStatic(t, s, "origin", "v4"); err != nil {
		t.Fatal(err)
	}
	pending := map[string]string{}
	if res, err := s.Publish(ctx, "origin", 0, "", ViaAPI); err != nil {
		t.Fatal(err)
	} else {
		pending["publish"] = res.Approval.ID
	}
	var p *PendingApproval
	if _, err := s.Deploy(ctx, "origin", 3, ViaAPI); !errors.As(err, &p) {
		t.Fatalf("deploy request: %v", err)
	}
	pending["activate"] = p.Approval.ID
	if _, err := s.Rollback(ctx, "origin", 1, ViaAPI); !errors.As(err, &p) {
		t.Fatalf("rollback request: %v", err)
	}
	pending["rollback"] = p.Approval.ID

	op := keepRenameJournal(t, s, "origin", "renamed")
	moved := filepath.Join(s.flatDir(op.To), "data", "files", "secret")
	if err := writeFile(moved, "original secret"); err != nil {
		t.Fatal(err)
	}
	refused := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrConflict) {
			t.Errorf("%s while the rename journal is kept: %v, want a conflict", what, err)
		}
		if exists, err := pathExists(s.flatDir(op.From)); err != nil || exists {
			t.Fatalf("%s recreated the old slug's storage: %t, %v", what, exists, err)
		}
	}
	// Approvals first: a changed Draft would make them stale for another reason.
	for _, action := range []string{"publish", "activate", "rollback"} {
		_, err := s.Decide(ctx, pending[action], true)
		refused(action+" approval", err)
	}
	_, err = s.OpenPreview(ctx, "origin", 1)
	refused("version preview", err)
	_, err = s.OpenPreview(ctx, "origin", 0)
	refused("draft preview", err)
	draft, err := s.GetDraft(ctx, "origin")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SaveDraft(ctx, "origin", []bundle.File{{Path: "index.html", Data: []byte("v5")}}, SaveMeta{}, draft.Revision, ViaAPI)
	refused("save draft", err)
	refused("save", saveStatic(t, s, "origin", "v5"))
	if _, err := s.CreateFlat(ctx, "renamed", "", ViaAPI); !errors.Is(err, ErrConflict) {
		t.Fatalf("new slug was reusable: %v", err)
	}
	// Reads continue.
	if f, err := s.GetFlat(ctx, "origin"); err != nil || f.LiveVersion != 2 {
		t.Fatalf("read flat: %+v, %v", f, err)
	}
	if vs, err := s.ListVersions(ctx, "origin"); err != nil || len(vs) == 0 {
		t.Fatalf("list versions: %d, %v", len(vs), err)
	}

	if err := s.recoverStorageOperations(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := readFile(filepath.Join(s.flatDir(op.From), "data", "files", "secret")); err != nil || got != "original secret" {
		t.Fatalf("recovery did not restore the storage: %q, %v", got, err)
	}
	if err := saveStatic(t, s, "origin", "v5"); err != nil {
		t.Fatalf("save after recovery: %v", err)
	}
	if _, err := s.OpenPreview(ctx, "origin", 1); err != nil {
		t.Fatalf("preview after recovery: %v", err)
	}
	if _, err := approvedInternalDeploy(t, s, ctx, "origin", 0); err != nil {
		t.Fatalf("publish and deploy after recovery: %v", err)
	}
}

// A rename journal that outlives its commit (the database could not be
// synced) names the new slug: its document is not edited or saved until
// startup recovery drops the journal.
func TestKeptRenameJournalRefusesDocumentWrites(t *testing.T) {
	ctx := t.Context()
	s, _ := newTestService(t)
	s.cfg.Runtime = &contentRuntime{}
	s.cfg.DocsApp = testDocsApp()
	saveDocs(t, s, "notes")
	with := "x"
	edit := []DocumentEditOp{{Op: "replace", Find: "ok", With: &with}}
	if _, err := s.UpdateDocument(ctx, "notes", "", edit, nil, ViaMCP); err != nil {
		t.Fatal(err)
	}
	op := storageOperation{Kind: "rename", From: "old-notes", To: "notes"}
	if err := s.beginStorageOperation(op); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDocument(ctx, "notes", "", edit, nil, ViaMCP); !errors.Is(err, ErrConflict) {
		t.Fatalf("edit while the rename journal is kept: %v", err)
	}
	if _, err := s.SaveVersion(ctx, "notes", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte("# new")}}, SaveMeta{}, ViaAPI); !errors.Is(err, ErrConflict) {
		t.Fatalf("document save while the rename journal is kept: %v", err)
	}
	if _, err := s.GetDocument(ctx, "notes", ""); err != nil {
		t.Fatalf("read while the rename journal is kept: %v", err)
	}
	if err := s.recoverStorageOperations(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDocument(ctx, "notes", "", edit, nil, ViaMCP); err != nil {
		t.Fatalf("edit after recovery: %v", err)
	}
}

func TestStoragePathAcceptsOnlySingleNames(t *testing.T) {
	s, dir := newTestService(t)
	root := filepath.Join(dir, "flats")
	if got, err := s.storagePath(); err != nil || got != root {
		t.Fatalf("root: %q, %v", got, err)
	}
	if got, err := s.storagePath("my-flat"); err != nil || got != filepath.Join(root, "my-flat") {
		t.Fatalf("slug: %q, %v", got, err)
	}
	if got, err := s.storagePath(".lifecycle", "rename.a-b.c-d.json"); err != nil || got != filepath.Join(root, ".lifecycle", "rename.a-b.c-d.json") {
		t.Fatalf("journal: %q, %v", got, err)
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "/abs", "../x"} {
		if got, err := s.storagePath(bad); err == nil {
			t.Errorf("storagePath(%q) = %q, want an error", bad, got)
		}
		if got, err := s.storagePath(".lifecycle", bad); err == nil {
			t.Errorf("storagePath(.lifecycle, %q) = %q, want an error", bad, got)
		}
	}
}
