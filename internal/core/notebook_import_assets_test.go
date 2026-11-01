package core

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/notebook"
	"github.com/gosuda/flats/internal/store"
)

func uploadNotebookAsset(t *testing.T, in *notebook.Instance) (string, []byte) {
	t.Helper()
	data := pngBytes(t, 3, 2)
	hashes, err := in.PrepareUploads(t.Context(), []notebook.UploadAsset{{Ref: "image", Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	return hashes["image"], data
}

func expireNotebookAsset(t *testing.T, s *Service, hash string, ageBlob bool) {
	t.Helper()
	boardExecDB(t, s.notebookDB("notes"), fmt.Sprintf(`UPDATE assets SET protected_until=0 WHERE hash='%s'`, hash))
	if ageBlob {
		old := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(storedBlobPath(s.docsStoreDir("notes"), hash), old, old); err != nil {
			t.Fatal(err)
		}
	}
}

func assertNotebookAssetStored(t *testing.T, s *Service, in *notebook.Instance, hash string, want bool) {
	t.Helper()
	if got := notebookCount(t, in, `SELECT count(*) FROM assets WHERE hash=?`, hash); (got == 1) != want {
		t.Errorf("asset metadata present = %d, want %t", got, want)
	}
	_, err := os.Stat(storedBlobPath(s.docsStoreDir("notes"), hash))
	if want && err != nil || !want && !errors.Is(err, os.ErrNotExist) {
		t.Errorf("asset blob stat = %v, want present %t", err, want)
	}
}

func TestNotebookAssetsAbandonedUploadCollected(t *testing.T) {
	s, _ := newTestService(t)
	in := newNotebook(t, s, "notes")
	hash, data := uploadNotebookAsset(t, in)
	s.sweepBlobs(t.Context(), "notes", "notebook")
	assertNotebookAssetStored(t, s, in, hash, true)

	expireNotebookAsset(t, s, hash, false)
	s.sweepBlobs(t.Context(), "notes", "notebook")
	assertNotebookAssetStored(t, s, in, hash, true)
	expireNotebookAsset(t, s, hash, true)
	before, err := s.notebookOther("notes")
	if err != nil {
		t.Fatal(err)
	}
	s.sweepBlobs(t.Context(), "notes", "notebook")
	assertNotebookAssetStored(t, s, in, hash, false)
	after, err := s.notebookOther("notes")
	if err != nil {
		t.Fatal(err)
	}
	if before-after != int64(len(data)) {
		t.Errorf("quota-counted storage recovered %d bytes, want %d", before-after, len(data))
	}
	uploadNotebookAsset(t, in)
	assertNotebookAssetStored(t, s, in, hash, true)
}

func TestNotebookAssetsSweepPreparationExcludesExpiredUnusedUpload(t *testing.T) {
	s, _ := newTestService(t)
	in := newNotebook(t, s, "notes")
	hash, _ := uploadNotebookAsset(t, in)
	expireNotebookAsset(t, s, hash, true)
	unlock := s.lock("notes")
	defer unlock()
	_, roots, ok := s.prepareNotebookSweepLocked(t.Context(), "notes")
	if !ok || slices.Contains(roots, hash) {
		t.Fatalf("sweep preparation ok=%t retains abandoned upload=%t", ok, slices.Contains(roots, hash))
	}
	if count := notebookCount(t, in, `SELECT count(*) FROM assets WHERE hash=?`, hash); count != 0 {
		t.Fatalf("expired metadata rows = %d", count)
	}
}

func TestNotebookAssetsProtectedAssetsSurviveSweep(t *testing.T) {
	for _, protection := range []string{"upload protection", "lease", "retained history", "rollback hold", "attached before sweep", "deleted payload"} {
		t.Run(protection, func(t *testing.T) {
			s, _ := newTestService(t)
			in := newNotebook(t, s, "notes")
			hash, _ := uploadNotebookAsset(t, in)
			if protection == "upload protection" {
				old := time.Now().Add(-2 * time.Hour)
				if err := os.Chtimes(storedBlobPath(s.docsStoreDir("notes"), hash), old, old); err != nil {
					t.Fatal(err)
				}
				s.sweepBlobs(t.Context(), "notes", "notebook")
				assertNotebookAssetStored(t, s, in, hash, true)
				expireNotebookAsset(t, s, hash, true)
				s.sweepBlobs(t.Context(), "notes", "notebook")
				assertNotebookAssetStored(t, s, in, hash, false)
				return
			}
			if protection == "lease" {
				release := s.lease("notes", []string{hash})
				defer release()
				expireNotebookAsset(t, s, hash, true)
				s.sweepBlobs(t.Context(), "notes", "notebook")
				assertNotebookAssetStored(t, s, in, hash, true)
				release()
				s.sweepBlobs(t.Context(), "notes", "notebook")
				assertNotebookAssetStored(t, s, in, hash, false)
				return
			}
			var page notebook.Ref
			if err := in.ReadTx(t.Context(), func(tx *sql.Tx, _ notebook.Sequence) error {
				return tx.QueryRowContext(t.Context(), `SELECT id,inc FROM items WHERE kind='page' AND alive=1 LIMIT 1`).Scan(&page.ID, &page.Inc)
			}); err != nil {
				t.Fatal(err)
			}
			if protection == "attached before sweep" {
				expireNotebookAsset(t, s, hash, true)
				session, err := in.NewSession(t.Context(), "person", "Person", true)
				if err != nil {
					t.Fatal(err)
				}
				id := notebook.ID("b_aaaaaaaaaaaaaaaaaaaaaaaaaa")
				ack, err := in.SubmitAction(t.Context(), session.Token, 1, notebook.Action{N: 1, ActionID: "attach-image", Ops: []notebook.Operation{notebook.CreateBlockOp{
					CreateFields: notebook.CreateFields{Ref: "image", ProposedID: &id},
					Place:        notebook.Placement{Page: notebook.Target{Ref: &page}},
					Content:      notebook.Content{Kind: notebook.Image, Asset: hash, Caption: notebook.RichText{}},
				}}})
				if err != nil || ack.Summary.Applied != 1 {
					t.Fatalf("attach before sweep: ack=%+v err=%#v", ack, err)
				}
				s.sweepBlobs(t.Context(), "notes", "notebook")
				assertNotebookAssetStored(t, s, in, hash, true)
				return
			}
			create := fmt.Sprintf(`{"op":"create_block","ref":"image","place":{"page":{"id":%q,"inc":"%d"},"parent":null,"before":null},"content":{"kind":"image","asset":%q,"alt":"","caption":[]}}`, page.ID, page.Inc, hash)
			_, result := writeNotebook(t, s, "notes", "attach-image", create)
			image := result.Refs["image"].Ref
			if protection == "retained history" || protection == "rollback hold" {
				replacement, err := in.PrepareUploads(t.Context(), []notebook.UploadAsset{{Ref: "replacement", Data: pngBytes(t, 4, 2)}})
				if err != nil {
					t.Fatal(err)
				}
				writeNotebook(t, s, "notes", "replace-image", fmt.Sprintf(`{"op":"set_block","block":{"id":%q,"inc":"%d"},"content":{"kind":"image","asset":%q,"alt":"","caption":[]}}`, image.ID, image.Inc, replacement["replacement"]))
				if got := notebookCount(t, in, `SELECT live_refs+deleted_refs FROM assets WHERE hash=?`, hash); got != 0 {
					t.Fatalf("historical asset still has current references: %d", got)
				}
				if protection == "rollback hold" {
					if err := s.st.ContentTx(t.Context(), "notes", func(tx *store.ContentTx) error { return tx.AddHold(t.Context(), "attach-image", 2) }); err != nil {
						t.Fatal(err)
					}
					one := 1
					if err := s.SetRevisionQuotas(t.Context(), "notes", &one, &one); err != nil {
						t.Fatal(err)
					}
					if _, err := s.st.Revision(t.Context(), "notes", 2); err != nil {
						t.Fatalf("held revision was pruned: %v", err)
					}
				}
			}
			if protection == "deleted payload" {
				writeNotebook(t, s, "notes", "delete-image", fmt.Sprintf(`{"op":"delete_blocks","blocks":[{"id":%q,"inc":"%d"}]}`, image.ID, image.Inc))
			}
			expireNotebookAsset(t, s, hash, true)
			s.sweepBlobs(t.Context(), "notes", "notebook")
			assertNotebookAssetStored(t, s, in, hash, true)
		})
	}
}

func TestNotebookAssetsFailedReferenceLookupSkipsSweep(t *testing.T) {
	s, _ := newTestService(t)
	in := newNotebook(t, s, "notes")
	hash, _ := uploadNotebookAsset(t, in)
	expireNotebookAsset(t, s, hash, true)
	boardExecDB(t, s.notebookDB("notes"),
		`INSERT INTO sessions VALUES('sweep-session','person','Person',1,'0',0)`,
		`INSERT INTO undo VALUES('sweep-session','undo',1,'broken',1)`)
	s.sweepBlobs(t.Context(), "notes", "notebook")
	assertNotebookAssetStored(t, s, in, hash, true)
	boardExecDB(t, s.notebookDB("notes"), `DELETE FROM undo WHERE session='sweep-session'`)
	s.sweepBlobs(t.Context(), "notes", "notebook")
	assertNotebookAssetStored(t, s, in, hash, false)
}
