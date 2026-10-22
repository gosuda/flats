package mcpx

import (
	"testing"

	"github.com/gosuda/flats/internal/core"
)

// set_visibility takes a public access level for boards and documents;
// get_flat reports it for them only.
func TestSetVisibilityAccess(t *testing.T) {
	e := newEnv(t)
	var fi FlatInfo
	if text, failed := call(t, e.local, "create_flat", map[string]any{"slug": "plan", "type": "board"}, &fi); failed {
		t.Fatal(text)
	}
	if code, _ := e.operatorCall(t, "POST", "/flats/plan/providers", `{"provider":"portal","permitted":true}`); code != 200 {
		t.Fatalf("provider grant: %d", code)
	}
	var out ActionOut
	if text, failed := call(t, e.local, "set_visibility", map[string]any{"slug": "plan", "visibility": "private", "access": "edit"}, &out); !failed {
		t.Fatalf("private with access: %s", text)
	}
	call(t, e.local, "set_visibility", map[string]any{"slug": "plan", "visibility": "public"}, &out)
	e.approve(t, out.ApprovalID)
	var got FlatOut
	call(t, e.local, "get_flat", map[string]any{"slug": "plan"}, &got)
	if got.Flat.Visibility != "public" || got.Flat.PublicAccess != "view" {
		t.Fatalf("public board: %+v", got.Flat)
	}
	text, failed := call(t, e.local, "set_visibility", map[string]any{"slug": "plan", "visibility": "public", "access": "edit", "reason": "workshop"}, &out)
	if failed || out.Status != "pending_approval" || out.Notice != core.PendingPublicEditNotice {
		t.Fatalf("edit request: %s %+v", text, out)
	}
	var a ApprovalOut
	call(t, e.local, "get_approval", map[string]any{"id": out.ApprovalID}, &a)
	if a.Params["access"] != "edit" || a.Params["from_access"] != "view" || a.Params["access_seq"] == nil {
		t.Fatalf("approval params: %+v", a.Params)
	}
	call(t, e.local, "get_flat", map[string]any{"slug": "plan"}, &got)
	if got.Flat.PublicAccess != "view" {
		t.Fatalf("edit applied before approval: %+v", got.Flat)
	}
	// Applying edit needs the board server's safe point; the core tests
	// cover it with a fake board until then.

	// A website is never editable by anyone, and reports no level.
	var saved saveFixtureOut
	call(t, e.local, "save_draft", map[string]any{"slug": "demo", "files": []any{file("index.html", "hi", "")}, "deploy": true}, &saved)
	e.approve(t, saved.Deploy.ApprovalID)
	if text, failed := call(t, e.local, "set_visibility", map[string]any{"slug": "demo", "visibility": "public", "access": "edit"}, &out); !failed {
		t.Fatalf("website edit: %s", text)
	}
}

// A document can be editable by anyone too; the request carries the
// documents' notice.
func TestSetVisibilityAccessDocuments(t *testing.T) {
	e := newEnvRuntime(t)
	var out ActionOut
	var got FlatOut
	var saved saveFixtureOut
	if text, failed := call(t, e.local, "save_document", map[string]any{"slug": "notes", "markdown": "# notes"}, &saved); failed {
		t.Fatal(text)
	}
	if code, _ := e.operatorCall(t, "POST", "/flats/notes/providers", `{"provider":"portal","permitted":true}`); code != 200 {
		t.Fatalf("provider grant: %d", code)
	}
	call(t, e.local, "set_visibility", map[string]any{"slug": "notes", "visibility": "public"}, &out)
	e.approve(t, out.ApprovalID)
	call(t, e.local, "get_flat", map[string]any{"slug": "notes"}, &got)
	if got.Flat.PublicAccess != "view" {
		t.Fatalf("public document: %+v", got.Flat)
	}
	text, failed := call(t, e.local, "set_visibility", map[string]any{"slug": "notes", "visibility": "public", "access": "edit"}, &out)
	if failed || out.Status != "pending_approval" || out.Notice != core.PendingPublicEditDocsNotice {
		t.Fatalf("document edit request: %s %+v", text, out)
	}

}
