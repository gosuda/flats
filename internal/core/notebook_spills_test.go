package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpillServiceOwnsNotebookScratch(t *testing.T) {
	s, _ := newTestService(t)
	first := newNotebook(t, s, "first")
	second := newNotebook(t, s, "second")
	writeNotebook(t, s, "first", "extra-page", notebookCreatePage)
	writeNotebook(t, s, "second", "extra-page", notebookCreatePage)
	if _, err := first.Read(t.Context(), []byte(`{"limit":1}`), false); err != nil {
		t.Fatal(err)
	}
	out, err := second.Read(t.Context(), []byte(`{"limit":1}`), false)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Next string `json:"next"`
	}
	if err := json.Unmarshal(out, &page); err != nil || page.Next == "" {
		t.Fatalf("expected a retained second-notebook capture: %s, %v", out, err)
	}
	root := filepath.Join(s.cfg.DataDir, "cache", "notebook-spills")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("no discoverable host scratch namespace: %v", err)
	}
	owners := 0
	captures := 0
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "owner-") {
			continue
		}
		owners++
		children, err := os.ReadDir(filepath.Join(root, entry.Name(), "spills"))
		if err != nil {
			t.Fatal(err)
		}
		for _, child := range children {
			if strings.HasPrefix(child.Name(), "notebook-spill-") {
				captures++
			}
		}
	}
	if owners != 1 || captures != 2 {
		t.Fatalf("host scratch has %d owners and %d captures, want 1 and 2", owners, captures)
	}
	first.Stop()
	query, _ := json.Marshal(map[string]string{"cursor": page.Next})
	if _, err := second.Read(t.Context(), query, false); err != nil {
		t.Fatalf("stopping the first notebook invalidated the second cursor: %v", err)
	}
}
