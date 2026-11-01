package store

import (
	"context"
	"encoding/json"
	"testing"
)

func TestNotebookHostDerivedType(t *testing.T) {
	for _, value := range []any{Version{Manifest: json.RawMessage(`{"type":"notebook"}`)}, Draft{Manifest: json.RawMessage(`{"type":"notebook"}`)}} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var got struct{ Type string }
		if err := json.Unmarshal(raw, &got); err != nil || got.Type != "notebook" {
			t.Fatalf("derived type: %s, %v", raw, err)
		}
	}
	pages := []NotebookPageChange{{Page: "p_a", Change: "changed", TitleBefore: "old", TitleAfter: "new"}}
	raw, err := json.Marshal(Revision{Number: 2, NotebookPages: pages})
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Pages []NotebookPageChange }
	if err := json.Unmarshal(raw, &got); err != nil || len(got.Pages) != 1 || got.Pages[0] != pages[0] {
		t.Fatalf("revision summaries: %s, %v", raw, err)
	}
}

func TestNotebookHostPublicViewOnly(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.CreateFlat(ctx, Flat{Slug: "notes", Name: "Notes", Visibility: Private, LiveVersion: 1, CreatedAt: fixtureNow, UpdatedAt: fixtureNow}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertVersion(ctx, Version{Flat: "notes", Number: 1, Hash: "shell", Kind: "static", Manifest: json.RawMessage(`{"type":"notebook"}`), CreatedAt: fixtureNow, Published: true}); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"manifest", "content state"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "content state" {
				if err := s.ContentTx(ctx, "notes", func(tx *ContentTx) error { return tx.InitReadyState(ctx, "notebook") }); err != nil {
					t.Fatal(err)
				}
			}
			if ok, err := s.publicEditable(ctx, "notes"); err != nil || ok {
				t.Fatalf("notebook editable: %v, %v", ok, err)
			}
		})
	}
	for _, access := range []string{AccessEdit, AccessView} {
		t.Run(access, func(t *testing.T) {
			id := "approval-" + access
			if err := s.InsertApproval(ctx, Approval{ID: id, Flat: "notes", Action: "set_visibility", Params: json.RawMessage(`{}`), Status: "applying", RequestedAt: fixtureNow, IdempotencyKey: id}); err != nil {
				t.Fatal(err)
			}
			from, err := s.GetFlat(ctx, "notes")
			if err != nil {
				t.Fatal(err)
			}
			next := from
			next.Visibility, next.PublicAccess = Public, access
			err = s.CommitVisibility(ctx, next, from, id, json.RawMessage(`{}`))
			if access == AccessEdit {
				if err == nil {
					t.Fatal("notebook Public edit committed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.GetFlat(ctx, "notes")
			if err != nil || got.Visibility != Public || got.PublicAccess != AccessView {
				t.Fatalf("Public view: %+v, %v", got, err)
			}
		})
	}
}
