package core

import (
	"context"
	"errors"
	"net/http"
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
				if err := os.Rename(s.flatDir(op.From), s.deletedStorageDir(op.From)); err != nil {
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
			if exists, err := pathExists(s.deletedStorageDir(op.From)); err != nil || exists {
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
		Store: st,
		Private: &memNet{hosts: make(map[string]http.Handler)},
		Logf: t.Logf,
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
