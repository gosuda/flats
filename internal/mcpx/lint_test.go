package mcpx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Save warnings are reported in the text and the structured output and never
// change whether or what the save stores.
func TestSaveWarningsDoNotBlock(t *testing.T) {
	e := newEnvRuntime(t) // docs flats need a runtime
	sloppy := `<html><body><script src="https://unpkg.com/react@latest/umd/react.js"></script><img src="logo.png"></body></html>`
	var out SaveOut
	text, failed := call(t, e.local, "save_draft", map[string]any{"slug": "sloppy", "files": []any{file("index.html", sloppy, "")}}, &out)
	if failed || out.Draft.Revision != 1 {
		t.Fatalf("a save with warnings must succeed: %s", text)
	}
	for _, want := range []string{"no <title>", "viewport", `version "latest"`, "no file logo.png", "favicon", "no screenshot"} {
		found := false
		for _, w := range out.Warnings {
			found = found || (strings.Contains(w.Message, want) && w.Fix != "")
		}
		if !found || !strings.Contains(text, want) {
			t.Errorf("missing warning %q in structured output %+v or text:\n%s", want, out.Warnings, text)
		}
	}
	if !strings.Contains(text, "non-blocking; the Draft was saved") || !strings.Contains(text, "topic.design") {
		t.Errorf("warnings text: %s", text)
	}
	draft, err := e.svc.GetDraft(context.Background(), "sloppy")
	if err != nil || draft.Revision != 1 {
		t.Fatalf("Draft not stored: %+v %v", draft, err)
	}

	// deploy=true still requests approval and keeps the warnings.
	out = SaveOut{}
	text, failed = call(t, e.local, "save_version", map[string]any{"slug": "sloppy", "expected_revision": 1, "deploy": true, "files": []any{file("index.html", sloppy, "")}}, &out)
	if failed || out.Deploy == nil || out.Deploy.Status != "pending_approval" || len(out.Warnings) == 0 || !strings.Contains(text, "Warnings (") {
		t.Fatalf("deploy with warnings: %s %+v", text, out)
	}

	clean := `<!doctype html><html><head><meta name="viewport" content="width=device-width, initial-scale=1"><title>Lunch Poll</title><link rel="icon" href="icon.svg"></head><body>Hi</body></html>`
	out = SaveOut{}
	text, failed = call(t, e.local, "save_draft", map[string]any{"slug": "tidy", "files": []any{
		file("index.html", clean, ""), file("icon.svg", "<svg/>", ""), file("shot.png", "cG5n", "base64"), file("flats.json", `{"screenshot":"shot.png"}`, ""),
	}}, &out)
	if failed || len(out.Warnings) != 0 || strings.Contains(text, "Warnings") {
		t.Fatalf("clean bundle: %s %+v", text, out.Warnings)
	}

	// Docs bundles are not linted.
	out = SaveOut{}
	if text, failed := call(t, e.local, "save_version", map[string]any{"slug": "notes", "files": []any{file("index.md", "# Notes", "")}}, &out); failed || len(out.Warnings) != 0 {
		t.Fatalf("docs bundle: %s %+v", text, out.Warnings)
	}

	dir := filepath.Join(t.TempDir(), "dist")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(sloppy), 0o644); err != nil {
		t.Fatal(err)
	}
	out = SaveOut{}
	if text, failed := call(t, e.local, "save_version_from_dir", map[string]any{"slug": "fromdir", "dir": dir}, &out); failed || out.Draft.Revision != 1 || len(out.Warnings) == 0 {
		t.Fatalf("save_version_from_dir warnings: %s %+v", text, out)
	}
}
