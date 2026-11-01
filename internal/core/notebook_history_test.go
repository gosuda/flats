package core

import (
	"encoding/json"
	"testing"
)

func TestNotebookHistoryExplicitRevisionCursorSurvivesPruning(t *testing.T) {
	s, _ := newTestService(t)
	newNotebook(t, s, "notes")
	out, result := writeNotebook(t, s, "notes", "historical", notebookCreatePage)
	first, err := s.GetNotebook(t.Context(), "notes", out.Revision, []byte(`{"limit":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Next string `json:"next"`
	}
	if err := json.Unmarshal(first, &page); err != nil {
		t.Fatal(err)
	}
	if page.Next == "" {
		t.Fatal("missing historical cursor")
	}
	one := 1
	if err := s.SetRevisionQuotas(t.Context(), "notes", &one, nil); err != nil {
		t.Fatal(err)
	}
	writeNotebook(t, s, "notes", "later", notebookTitleOp(t, result.Refs["page"].Ref, "Changed"))
	if revisions := storedRevisions(t, s, "notes"); len(revisions) != 1 || revisions[0] == out.Revision {
		t.Fatalf("revision not pruned: %v", revisions)
	}
	query, _ := json.Marshal(map[string]string{"cursor": page.Next})
	if _, err := s.GetNotebook(t.Context(), "notes", out.Revision, query); err != nil {
		t.Fatalf("explicit revision cursor after pruning: %v", err)
	}
}
