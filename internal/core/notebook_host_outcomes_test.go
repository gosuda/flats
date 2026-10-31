package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/notebook"
)

func TestNotebookHostFirstWriteSurvivesInitializationFailure(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"new", "untyped"} {
		for _, failure := range []struct{ name, trigger string }{
			{"import", `CREATE TRIGGER fail_init BEFORE INSERT ON notebook_imports WHEN NEW.flat='notes' BEGIN SELECT RAISE(ABORT, 'injected import failure'); END`},
			{"phase", `CREATE TRIGGER fail_init BEFORE UPDATE OF phase ON content_state WHEN NEW.flat='notes' AND NEW.phase='ready' BEGIN SELECT RAISE(ABORT, 'injected phase failure'); END`},
			{"shell", `CREATE TRIGGER fail_init BEFORE INSERT ON versions WHEN NEW.flat='notes' BEGIN SELECT RAISE(ABORT, 'injected shell failure'); END`},
		} {
			t.Run(mode+"/"+failure.name, func(t *testing.T) {
				s, dir := newTestService(t)
				if mode == "untyped" {
					if _, err := s.CreateFlat(ctx, "notes", "Kept name", ViaMCP); err != nil {
						t.Fatal(err)
					}
				}
				boardExecDB(t, filepath.Join(dir, "flats.db"), failure.trigger)
				out, err := s.UpdateNotebook(ctx, "notes", notebookPayload("first-write", notebookCreatePage), ViaMCP)
				if err != nil {
					t.Fatalf("committed first write returned failure: %v", err)
				}
				result := notebookResult(t, out)
				if !out.Created || !out.Pending || out.Revision != 0 || result.Applied != 1 || result.Replay || result.Refs["page"].Ref.ID == "" {
					t.Fatalf("first write lost its outcome: %+v / %+v", out, result)
				}
				if len(result.Warnings) != 1 || result.Warnings[0].Code != "revision_pending" {
					t.Fatalf("missing pending warning: %+v", result)
				}
				st, err := s.st.ContentState(ctx, "notes")
				if err != nil || st.Type != "notebook" || st.Phase != "init" {
					t.Fatalf("initialization is not recoverable: %+v, %v", st, err)
				}
				pending := failure.name != "phase"
				revision := 0
				if !pending {
					revision = 2
				}
				for range 2 {
					replay, replayResult := writeNotebook(t, s, "notes", "first-write", notebookCreatePage)
					if replay.Created || replay.Pending != pending || replay.Revision != revision || replayResult.RevisionPending != pending || !replayResult.Replay || replayResult.Applied != result.Applied || replayResult.Refs["page"] != result.Refs["page"] || replayResult.State != result.State {
						t.Fatalf("pending replay changed the outcome: %+v / %+v", replay, replayResult)
					}
					if pending && (replayResult.Revision != nil || len(replayResult.Warnings) != 1 || replayResult.Warnings[0].Code != "revision_pending" || replayResult.Samples.Warnings.Total != 1) {
						t.Fatalf("pending replay lost its warning: %+v", replayResult)
					}
				}
				_, err = s.UpdateNotebook(ctx, "notes", notebookPayload("first-write", strings.Replace(notebookCreatePage, "Added page", "Different", 1)), ViaMCP)
				var refusal *notebook.WriteError
				if !errors.As(err, &refusal) || refusal.Code != "conflict" || refusal.Reason != "op_id_conflict" {
					t.Fatalf("pending initialization hid a conflicting replay: %v", err)
				}
				boardExecDB(t, filepath.Join(dir, "flats.db"), `DROP TRIGGER fail_init`)
				if failure.name == "shell" {
					replay, replayResult := writeNotebook(t, s, "notes", "first-write", notebookCreatePage)
					if replay.Created || replay.Pending || replay.Revision != 2 || !replayResult.Replay || replayResult.Refs["page"] != result.Refs["page"] || replayResult.State != result.State {
						t.Fatalf("replay did not recover initialization: %+v / %+v", replay, replayResult)
					}
				}
				s.startPendingDesigns(ctx)
				st, err = s.st.ContentState(ctx, "notes")
				if err != nil || st.Phase != "ready" {
					t.Fatalf("initialization did not recover: %+v, %v", st, err)
				}
				replay, replayResult := writeNotebook(t, s, "notes", "first-write", notebookCreatePage)
				if replay.Created || replay.Pending || replay.Revision != 2 || !replayResult.Replay || replayResult.Refs["page"] != result.Refs["page"] || replayResult.State != result.State {
					t.Fatalf("recovered replay lost original result: %+v / %+v", replay, replayResult)
				}
				if got := storedRevisions(t, s, "notes"); !slices.Equal(got, []int{2, 1}) {
					t.Fatalf("replay duplicated revisions: %v", got)
				}
				if got := notebookCount(t, s.liveNotebook("notes"), `SELECT count(*) FROM items WHERE kind='page' AND alive=1`); got != 2 {
					t.Fatalf("replay duplicated pages: %d", got)
				}
			})
		}
	}
}

func TestHostNotebookUnavailableRetainsRetry(t *testing.T) {
	ctx := context.Background()
	for _, failure := range []string{"stopped read", "stopped write", "measurement"} {
		t.Run(failure, func(t *testing.T) {
			s, _ := newTestService(t)
			newNotebook(t, s, "notes")
			if strings.HasPrefix(failure, "stopped") {
				s.live["notes"].cur.Swap(nil).inst.Stop()
			} else {
				blocked := filepath.Join(s.flatDir("notes"), "unmeasurable")
				if err := os.Mkdir(blocked, 0o000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
				if _, err := s.notebookOther("notes"); err == nil {
					t.Skip("filesystem permits reading mode-000 directories")
				}
			}
			var err error
			if failure == "stopped read" {
				_, err = s.GetNotebook(ctx, "notes", 0, []byte(`{}`))
			} else {
				_, err = s.UpdateNotebook(ctx, "notes", notebookPayload("unavailable-write", notebookCreatePage), ViaMCP)
			}
			var refusal *notebook.WriteError
			if !errors.Is(err, ErrUnavailable) || !errors.As(err, &refusal) || refusal.Code != "unavailable" || !refusal.Retry || refusal.Detail == "" {
				t.Fatalf("unavailable lacks retry guidance: %v (%+v)", err, refusal)
			}
		})
	}
}
