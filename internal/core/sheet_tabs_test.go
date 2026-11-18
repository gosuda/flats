package core

import (
	"context"
	"slices"
	"testing"

	"github.com/gosuda/flats/internal/store"
)

// TestSheetRevisionsReportMovedTabs covers a revision that only changes the
// order of the tabs: History and list_revisions name the tab that moved,
// also when a rollback brings an earlier order back.
func TestSheetRevisionsReportMovedTabs(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newSheet(t, s, "budget")
	writeSheet(t, s, "budget", "tabs", `{"op":"rename_tab","tab":"Sheet1","name":"Budget"},{"op":"add_tab","name":"Notes"},{"op":"add_tab","name":"Plan"}`)
	out, _ := writeSheet(t, s, "budget", "move", `{"op":"move_tab","tab":"Budget","to":3}`)
	if out.Revision != 3 {
		t.Fatalf("move = %+v", out)
	}
	names := func(rev int) []string {
		t.Helper()
		var out []string
		for _, tab := range sheetRead(t, s, "budget", rev, `{}`)["tabs"].([]any) {
			out = append(out, tab.(map[string]any)["name"].(string))
		}
		return out
	}
	// The summary is read from the manifests of the revisions, so the
	// revision of the move has to hold the new order.
	if got := names(3); !slices.Equal(got, []string{"Notes", "Plan", "Budget"}) {
		t.Fatalf("the revision of a move holds the tab order %v: the sheet did not record the move in its manifest", got)
	}
	res, err := s.RollbackRevisionResult(ctx, "budget", 2, "agent", ViaMCP)
	if err != nil || res.Revision != 4 {
		t.Fatalf("rollback = %+v, %v", res, err)
	}
	if got := names(4); !slices.Equal(got, []string{"Budget", "Notes", "Plan"}) {
		t.Fatalf("tabs after the rollback = %v", got)
	}
	page, err := s.ListRevisions(ctx, "budget", 0, 10)
	if err != nil || len(page.Revisions) != 4 {
		t.Fatalf("revisions = %+v, %v", page, err)
	}
	moved := []store.SheetTabChange{{Tab: 1, Change: "changed", NameBefore: "Budget", NameAfter: "Budget"}}
	for _, r := range page.Revisions[:2] {
		if !slices.Equal(r.SheetTabs, moved) {
			t.Errorf("tabs of revision %d (%s) = %+v, want %+v", r.Number, r.Kind, r.SheetTabs, moved)
		}
	}
	// History reads the same summary through the sheet's backend.
	history, err := sheetBackend{s: s, slug: "budget"}.Revisions(ctx, 0, 10)
	if err != nil || len(history.Revisions) != 4 {
		t.Fatalf("history = %+v, %v", history, err)
	}
	for _, r := range history.Revisions[:2] {
		if len(r.Tabs) != 1 || r.Tabs[0].ID != 1 || r.Tabs[0].Name != "Budget" || r.Tabs[0].Change != "changed" {
			t.Errorf("history tabs of revision %d = %+v", r.Number, r.Tabs)
		}
	}
}

// TestSheetRevisionsReportAMoveBesideAnotherChange covers a move in a
// revision that also renames another tab: the moved tab is named beside the
// renamed one, although its manifest entry and its files stayed.
func TestSheetRevisionsReportAMoveBesideAnotherChange(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newSheet(t, s, "budget")
	writeSheet(t, s, "budget", "tabs", `{"op":"rename_tab","tab":"Sheet1","name":"Budget"},{"op":"add_tab","name":"Notes"},{"op":"add_tab","name":"Plan"}`)
	writeSheet(t, s, "budget", "move", `{"op":"move_tab","tab":"Budget","to":3},{"op":"rename_tab","tab":"Notes","name":"Memo"}`)
	if res, err := s.RollbackRevisionResult(ctx, "budget", 2, "agent", ViaMCP); err != nil || res.Revision != 4 {
		t.Fatalf("rollback = %+v, %v", res, err)
	}
	page, err := s.ListRevisions(ctx, "budget", 0, 10)
	if err != nil || len(page.Revisions) != 4 {
		t.Fatalf("revisions = %+v, %v", page, err)
	}
	want := map[int][]store.SheetTabChange{
		4: {{Tab: 1, Change: "changed", NameBefore: "Budget", NameAfter: "Budget"}, {Tab: 2, Change: "changed", NameBefore: "Memo", NameAfter: "Notes"}},
		3: {{Tab: 1, Change: "changed", NameBefore: "Budget", NameAfter: "Budget"}, {Tab: 2, Change: "changed", NameBefore: "Notes", NameAfter: "Memo"}},
	}
	for _, r := range page.Revisions[:2] {
		if !slices.Equal(r.SheetTabs, want[r.Number]) {
			t.Errorf("tabs of revision %d = %+v, want %+v", r.Number, r.SheetTabs, want[r.Number])
		}
	}
}

func TestSheetTabChangesByOrder(t *testing.T) {
	state := func(ids ...uint64) map[uint64]*sheetTabState {
		out := map[uint64]*sheetTabState{}
		for pos, id := range ids {
			out[id] = &sheetTabState{name: "t", pos: pos, entry: []byte{byte(id)}, files: map[string]string{}}
		}
		return out
	}
	for _, tc := range []struct {
		name          string
		before, after []uint64
		want          []uint64
	}{
		{"same order", []uint64{1, 2, 3}, []uint64{1, 2, 3}, nil},
		{"first to last", []uint64{1, 2, 3, 4}, []uint64{2, 3, 4, 1}, []uint64{1}},
		{"last to first", []uint64{1, 2, 3, 4}, []uint64{4, 1, 2, 3}, []uint64{4}},
		{"two moved", []uint64{1, 2, 3, 4, 5}, []uint64{5, 1, 4, 2, 3}, []uint64{4, 5}},
		{"removal shifts the rest", []uint64{1, 2, 3}, []uint64{2, 3}, []uint64{1}},
		{"addition in front shifts the rest", []uint64{1, 2}, []uint64{9, 1, 2}, []uint64{9}},
		{"move beside an addition", []uint64{1, 2, 3}, []uint64{3, 9, 1, 2}, []uint64{3, 9}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []uint64
			for _, change := range sheetTabChanges(state(tc.before...), state(tc.after...)) {
				got = append(got, change.Tab)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("changed tabs = %v, want %v", got, tc.want)
			}
		})
	}
}
