package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/notebook"
	"github.com/gosuda/flats/internal/store"
)

const notebookCreatePage = `{"op":"create_page","ref":"page","parent":null,"before":null,"title":"Added page"}`

func newNotebook(t *testing.T, s *Service, slugName string) *notebook.Instance {
	t.Helper()
	if _, err := s.CreateNotebook(context.Background(), slugName, "Notes", ViaMCP); err != nil {
		t.Fatal(err)
	}
	in := s.liveNotebook(slugName)
	if in == nil {
		t.Fatal("created notebook is not running")
	}
	return in
}

func notebookPayload(opID, ops string) []byte {
	return []byte(fmt.Sprintf(`{"op_id":%q,"ops":[%s]}`, opID, ops))
}

func notebookResult(t *testing.T, out NotebookWriteResult) notebook.WriteResult {
	t.Helper()
	var result notebook.WriteResult
	if err := json.Unmarshal(out.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.Created != out.Created || result.RevisionPending != out.Pending ||
		(result.Revision == nil) != (out.Revision == 0) ||
		(result.Revision != nil && *result.Revision != out.Revision) {
		t.Fatalf("inconsistent write envelope: %+v / %+v", out, result)
	}
	return result
}

func writeNotebook(t *testing.T, s *Service, slugName, opID, ops string) (NotebookWriteResult, notebook.WriteResult) {
	t.Helper()
	out, err := s.UpdateNotebook(context.Background(), slugName, notebookPayload(opID, ops), ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	return out, notebookResult(t, out)
}

func notebookCount(t *testing.T, in *notebook.Instance, query string, args ...any) int {
	t.Helper()
	var count int
	if err := in.ReadTx(context.Background(), func(tx *sql.Tx, _ notebook.Sequence) error {
		return tx.QueryRow(query, args...).Scan(&count)
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

func notebookTitleOp(t *testing.T, ref notebook.Ref, title string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"op": "set_page_title", "page": ref, "title": title})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestNotebookCreationAdoptionAndReplay(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"create", "new write", "adopt"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := newTestService(t)
			if mode == "create" {
				view, err := s.CreateNotebook(ctx, "notes", "  Field   notes  ", ViaMCP)
				if err != nil || view.Name != "Field notes" || view.Type != "notebook" {
					t.Fatalf("create = %+v, %v", view, err)
				}
				if _, err := s.CreateNotebook(ctx, "notes", "Other", ViaMCP); !errors.Is(err, ErrConflict) {
					t.Fatalf("duplicate create = %v", err)
				}
			} else if mode == "adopt" {
				if _, err := s.CreateFlat(ctx, "notes", "Kept name", ViaMCP); err != nil {
					t.Fatal(err)
				}
			}
			out, result := writeNotebook(t, s, "notes", "first-write", notebookCreatePage)
			if out.Created != (mode != "create") || out.Pending || out.Revision != 2 || result.Applied != 1 || !result.Mapping.Complete {
				t.Fatalf("first write = %+v / %+v", out, result)
			}
			assigned, ok := result.Refs["page"]
			if !ok || assigned.Ref.ID.ValidateKind(notebook.PageKind) != nil || assigned.Ref.Inc == 0 {
				t.Fatalf("assigned page = %+v", result.Refs)
			}
			in := s.liveNotebook("notes")
			if got := notebookCount(t, in, `SELECT count(*) FROM items WHERE kind='page' AND alive=1`); got != 2 {
				t.Fatalf("live page count = %d", got)
			}
			replay, replayResult := writeNotebook(t, s, "notes", "first-write", notebookCreatePage)
			if replay.Created || !replayResult.Replay || replay.Revision != 2 || replayResult.Refs["page"] != assigned || replayResult.State != result.State {
				t.Fatalf("replay = %+v / %+v", replay, replayResult)
			}
			if got := storedRevisions(t, s, "notes"); !slices.Equal(got, []int{2, 1}) {
				t.Fatalf("replayed history = %v", got)
			}
			state, err := s.st.ContentState(ctx, "notes")
			if err != nil || state.Type != "notebook" || state.Phase != "ready" || state.ImportStore != in.StoreID() {
				t.Fatalf("content state = %+v, %v", state, err)
			}
			if mode == "adopt" {
				f, err := s.GetFlat(ctx, "notes")
				if err != nil || f.Name != "Kept name" {
					t.Fatalf("adopted name = %+v, %v", f, err)
				}
			}
			be := notebookBackend{s: s, slug: "notes"}
			src, release, err := be.RevisionSource(ctx, 2)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			found := false
			for _, f := range src.Files {
				data, err := src.Read(ctx, f.Hash)
				if err != nil || hashBytes(data) != f.Hash || int64(len(data)) != f.Size {
					t.Fatalf("historical file %s = %q, %v", f.Path, data, err)
				}
				if f.Path == "pages/"+string(assigned.Ref.ID)+"/page.json" {
					var page struct {
						Title string `json:"title"`
					}
					if err := json.Unmarshal(data, &page); err != nil || page.Title != "Added page" {
						t.Fatalf("historical page = %q, %v", data, err)
					}
					found = true
				}
			}
			if !found {
				t.Fatal("revision omitted the created page")
			}
		})
	}
}

func TestNotebookRefusedFirstWriteCleanup(t *testing.T) {
	ctx := context.Background()
	for _, adopt := range []bool{false, true} {
		t.Run(fmt.Sprintf("adopt=%t", adopt), func(t *testing.T) {
			s, _ := newTestService(t)
			var preserved string
			if adopt {
				if _, err := s.CreateFlat(ctx, "notes", "Untyped", ViaMCP); err != nil {
					t.Fatal(err)
				}
				var err error
				preserved, err = putBlob(s.docsStoreDir("notes"), []byte("pre-existing blob"))
				if err != nil {
					t.Fatal(err)
				}
			}
			payload := []byte(`{"op_id":"refused-first","guards":{"state":{"store":"wrong-store","seq":"1"}},"ops":[` + notebookCreatePage + `]}`)
			_, err := s.UpdateNotebook(ctx, "notes", payload, ViaMCP)
			var problem *notebook.WriteError
			if !errors.Is(err, ErrConflict) || !errors.As(err, &problem) || problem.Code != "conflict" {
				t.Fatalf("guard refusal = %v / %+v", err, problem)
			}
			if s.liveNotebook("notes") != nil {
				t.Fatal("refused first write left a runtime")
			}
			if _, err := os.Stat(s.notebookDir("notes")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused first write left notebook storage: %v", err)
			}
			if adopt {
				f, err := s.GetFlat(ctx, "notes")
				if err != nil || f.Type != "flat" || f.Name != "Untyped" {
					t.Fatalf("refused adoption changed flat = %+v, %v", f, err)
				}
				if data, err := readBlob(s.docsStoreDir("notes"), preserved); err != nil || string(data) != "pre-existing blob" {
					t.Fatalf("pre-existing blob = %q, %v", data, err)
				}
			} else {
				if _, err := s.st.GetFlat(ctx, "notes"); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("refused first write left metadata: %v", err)
				}
				if _, err := os.Stat(s.flatDir("notes")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("refused first write left flat storage: %v", err)
				}
			}
			out, _ := writeNotebook(t, s, "notes", "retry-valid", notebookCreatePage)
			if !out.Created || out.Revision != 2 {
				t.Fatalf("valid write after refusal = %+v", out)
			}
		})
	}
}

func TestNotebookFixedTypeAndWriteErrors(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	if _, err := s.CreateBoard(ctx, "board", "Board", ViaMCP); err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct {
		name string
		run  func() error
	}{
		{"update other type", func() error {
			_, err := s.UpdateNotebook(ctx, "board", notebookPayload("wrong-type", notebookCreatePage), ViaMCP)
			return err
		}},
		{"read other type", func() error { _, err := s.GetNotebook(ctx, "board", 0, []byte(`{}`)); return err }},
	} {
		t.Run(call.name, func(t *testing.T) {
			if err := call.run(); !errors.Is(err, ErrNotNotebook) || ErrorCategory(err) != "not_notebook" {
				t.Fatalf("wrong type = %v", err)
			}
		})
	}
	newNotebook(t, s, "notes")
	if _, err := s.SaveVersion(ctx, "notes", []bundle.File{{Path: "index.html", Data: []byte("website")}}, SaveMeta{}, ViaMCP); !errors.Is(err, ErrTypeChange) {
		t.Fatalf("notebook type changed = %v", err)
	}
	writeNotebook(t, s, "notes", "same-op", notebookCreatePage)
	_, err := s.UpdateNotebook(ctx, "notes", notebookPayload("same-op", strings.ReplaceAll(notebookCreatePage, "Added page", "Different")), ViaMCP)
	var problem *notebook.WriteError
	if !errors.Is(err, ErrConflict) || !errors.As(err, &problem) || problem.Reason != "op_id_conflict" {
		t.Fatalf("conflicting replay = %v / %+v", err, problem)
	}
	for _, tc := range []struct {
		code string
		kind error
	}{
		{"invalid", ErrInvalid}, {"conflict", ErrConflict}, {"notebook_capacity", ErrNotebookCapacity},
		{"unavailable", ErrUnavailable}, {"not_found", store.ErrNotFound},
	} {
		t.Run(tc.code, func(t *testing.T) {
			original := &notebook.WriteError{Code: tc.code, Reason: "detail", Detail: "kept", Retry: tc.code == "unavailable"}
			err := notebookErr(fmt.Errorf("wrapped: %w", original))
			var got *notebook.WriteError
			if !errors.Is(err, tc.kind) || !errors.As(err, &got) || got != original || ErrorCategory(err) != tc.code {
				t.Fatalf("mapped error = %v / %+v", err, got)
			}
		})
	}
}

func TestNotebookQuotaRefusalIsAtomic(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	in := newNotebook(t, s, "notes")
	if _, err := s.UpdateSettings(ctx, map[string]string{SetDiskQuotaBytes: "1"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.UpdateNotebook(ctx, "notes", notebookPayload("over-quota", notebookCreatePage), ViaMCP)
	var problem *notebook.WriteError
	if !errors.Is(err, ErrNotebookCapacity) || !errors.As(err, &problem) || problem.Code != "notebook_capacity" || ErrorCategory(err) != "notebook_capacity" {
		t.Fatalf("quota refusal = %v / %+v", err, problem)
	}
	if got := notebookCount(t, in, `SELECT count(*) FROM items WHERE kind='page' AND alive=1`); got != 1 {
		t.Fatalf("quota refusal changed pages: %d", got)
	}
	if got := notebookCount(t, in, `SELECT count(*) FROM commands WHERE op_id='over-quota'`); got != 0 {
		t.Fatalf("quota refusal retained command: %d", got)
	}
	if got := storedRevisions(t, s, "notes"); !slices.Equal(got, []int{1}) {
		t.Fatalf("quota refusal changed history: %v", got)
	}
	if _, err := s.UpdateSettings(ctx, map[string]string{SetDiskQuotaBytes: "0"}); err != nil {
		t.Fatal(err)
	}
	out, _ := writeNotebook(t, s, "notes", "over-quota", notebookCreatePage)
	if out.Revision != 2 {
		t.Fatalf("same op after quota removal = %+v", out)
	}
}

// A stop observer checks storage at the lifecycle boundary without requiring
// the live transport, which is supplied by another implementation unit.
type notebookStopObserver struct {
	*notebook.Instance
	onStop func()
}

func (o *notebookStopObserver) Stop() {
	o.Instance.Stop()
	o.onStop()
}

func TestNotebookRenameDeleteStopBeforeStorage(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	in := newNotebook(t, s, "notes")
	_, result := writeNotebook(t, s, "notes", "before-rename", notebookCreatePage)
	observe := func(slugName string, in *notebook.Instance) *bool {
		unlock := s.lock(slugName)
		defer unlock()
		stopped := new(bool)
		s.mu.Lock()
		lf := s.live[slugName]
		s.mu.Unlock()
		d := *lf.cur.Load()
		d.inst = &notebookStopObserver{Instance: in, onStop: func() {
			*stopped = true
			if _, err := os.Stat(s.notebookDB(slugName)); err != nil {
				t.Errorf("notebook storage moved before Stop returned: %v", err)
			}
			if err := in.Barrier(ctx); err == nil {
				t.Error("old notebook still admits work after Stop")
			}
		}}
		lf.cur.Store(&d)
		return stopped
	}
	stopped := observe("notes", in)
	if _, err := s.RenameSlug(ctx, "notes", "renamed", ViaMCP); err != nil {
		t.Fatal(err)
	}
	if !*stopped || s.liveNotebook("renamed") == nil || s.liveNotebook("renamed") == in {
		t.Fatal("rename did not stop and replace notebook")
	}
	if _, err := os.Stat(s.flatDir("notes")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old path after rename = %v", err)
	}
	out, _ := writeNotebook(t, s, "renamed", "after-rename", notebookTitleOp(t, result.Refs["page"].Ref, "Renamed"))
	if out.Revision != 3 || !slices.Equal(storedRevisions(t, s, "renamed"), []int{3, 2, 1}) {
		t.Fatalf("history after rename = %+v", out)
	}
	stopped = observe("renamed", s.liveNotebook("renamed"))
	if result, err := s.Delete(ctx, "renamed", ViaConsole, ""); err != nil || result.Status != "done" {
		t.Fatal(err)
	}
	if !*stopped {
		t.Fatal("delete did not stop notebook")
	}
	if _, err := os.Stat(s.flatDir("renamed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted storage remains = %v", err)
	}
	if _, err := s.st.GetFlat(ctx, "renamed"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted metadata remains = %v", err)
	}
}

func TestNotebookRetentionPinsSweepAndLease(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newNotebook(t, s, "notes")
	_, result := writeNotebook(t, s, "notes", "retained-page", notebookCreatePage)
	be := notebookBackend{s: s, slug: "notes"}
	one := 1
	if err := s.SetRevisionQuotas(ctx, "notes", &one, &one); err != nil {
		t.Fatal(err)
	}
	if err := be.Pin(ctx, 2, true); err != nil {
		t.Fatal(err)
	}
	src, release, err := be.RevisionSource(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, f := range src.Files {
		if !s.leased("notes", f.Hash) {
			t.Fatalf("historical source did not lease %s", f.Path)
		}
	}
	if _, err := src.Read(ctx, strings.Repeat("0", 64)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("source read outside file list = %v", err)
	}
	writeNotebook(t, s, "notes", "changed-page", notebookTitleOp(t, result.Refs["page"].Ref, "New title"))
	if got := storedRevisions(t, s, "notes"); !slices.Equal(got, []int{3, 2}) {
		t.Fatalf("pin retention = %v", got)
	}
	if err := s.PinRevision(ctx, "notes", 3, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("pin quota refusal = %v", err)
	}
	if err := be.Pin(ctx, 2, false); err != nil {
		t.Fatal(err)
	}
	if got := storedRevisions(t, s, "notes"); !slices.Equal(got, []int{3}) {
		t.Fatalf("unpin retention = %v", got)
	}
	old := time.Now().Add(-time.Hour)
	root := s.docsStoreDir("notes")
	var oldPage notebook.File
	for _, f := range src.Files {
		if f.Path == "pages/"+string(result.Refs["page"].Ref.ID)+"/page.json" {
			oldPage = f
		}
	}
	if oldPage.Hash == "" {
		t.Fatal("source omitted old page")
	}
	if err := os.Chtimes(storedBlobPath(root, oldPage.Hash), old, old); err != nil {
		t.Fatal(err)
	}
	stray, err := putBlob(root, []byte("old orphan"))
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := putBlob(root, []byte("fresh orphan"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(storedBlobPath(root, stray), old, old); err != nil {
		t.Fatal(err)
	}
	s.sweepBlobs(ctx, "notes", "notebook")
	if _, err := src.Read(ctx, oldPage.Hash); err != nil {
		t.Fatalf("sweep removed leased historical file = %v", err)
	}
	if _, err := os.Stat(storedBlobPath(root, stray)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old orphan survived sweep = %v", err)
	}
	if _, err := os.Stat(storedBlobPath(root, fresh)); err != nil {
		t.Fatalf("fresh orphan lost grace = %v", err)
	}
	release()
	release()
	for _, f := range src.Files {
		if s.leased("notes", f.Hash) {
			t.Fatalf("lease still held for %s", f.Path)
		}
	}
	s.sweepBlobs(ctx, "notes", "notebook")
	if _, err := os.Stat(storedBlobPath(root, oldPage.Hash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unleased pruned file survived sweep = %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.notebookDir("notes"), "notebook.db")); err != nil {
		t.Fatalf("sweep removed runtime database = %v", err)
	}
}
