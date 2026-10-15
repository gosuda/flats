package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestVersionToolModel(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"save_version", "save_version_from_dir", "publish"} {
		res, err := e.local.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		if err == nil && !res.IsError {
			t.Fatalf("removed tool %s still succeeds", name)
		}
	}
	var saved saveFixtureOut
	text, failed := call(t, e.local, "save_draft", map[string]any{"slug": "model", "files": []any{file("index.html", "one", "")}}, &saved)
	if failed || saved.Deploy != nil || saved.Draft.Revision != 1 {
		t.Fatalf("save: %s %+v", text, saved)
	}
	approvals, err := e.svc.ListApprovals(context.Background(), "pending")
	if err != nil || len(approvals) != 0 {
		t.Fatalf("save requested approval: %+v %v", approvals, err)
	}
	var action ActionOut
	text, failed = call(t, e.local, "create_version", map[string]any{"slug": "model", "revision": 1, "hash": saved.Draft.Hash}, &action)
	if failed || action.Approval == nil || action.Approval.Action != "create_version" {
		t.Fatalf("create: %s %+v", text, action)
	}
	e.approve(t, action.Approval.ID)
	text, failed = call(t, e.local, "delete_version", map[string]any{"slug": "model", "version": 1}, &action)
	if failed || action.Approval == nil || action.Approval.Action != "delete_version" {
		t.Fatalf("delete: %s %+v", text, action)
	}
	e.approve(t, action.Approval.ID)
	versions, err := e.svc.ListVersions(context.Background(), "model")
	if err != nil || len(versions) != 0 {
		t.Fatalf("retained versions: %+v %v", versions, err)
	}
}

func TestSaveDocumentBundleRoute(t *testing.T) {
	e := newEnvRuntime(t)
	args := map[string]any{"slug": "bundle", "replace_bundle": true, "files": []any{file("index.md", "# First", ""), file("other.md", "# Other", ""), file("flats.json", `{"type":"docs"}`, "")}}
	var out saveFixtureOut
	text, failed := call(t, e.local, "save_document", args, &out)
	if failed || out.Flat == nil || out.Flat.Type != "docs" {
		t.Fatalf("bundle: %s %+v", text, out)
	}
	text, failed = call(t, e.local, "save_draft", map[string]any{"slug": "bundle", "files": args["files"]}, nil)
	if !failed || !strings.Contains(text, "save_document") {
		t.Fatalf("Draft tool accepted document: %s", text)
	}
	delete(args, "replace_bundle")
	text, failed = call(t, e.local, "save_document", args, nil)
	if !failed || !strings.Contains(text, "replace_bundle") {
		t.Fatalf("implicit replacement: %s", text)
	}
}

func TestConcurrentMCPDraftReceiptsKeepOwnContent(t *testing.T) {
	e := newEnv(t)
	call(t, e.local, "save_draft", map[string]any{"slug": "receipts", "files": []any{file("index.html", "seed", "")}}, nil)
	type receipt struct {
		want   string
		draft  api.SavedDraft
		err    error
		failed bool
	}
	start := make(chan struct{})
	results := make(chan receipt, 8)
	for i := range 8 {
		content := fmt.Sprintf("content-%d", i)
		go func() {
			<-start
			want := bundle.Hash([]bundle.File{{Path: "index.html", Data: []byte(content)}})
			response, err := e.local.CallTool(context.Background(), &mcp.CallToolParams{Name: "save_draft", Arguments: map[string]any{"slug": "receipts", "files": []any{file("index.html", content, "")}}})
			if err != nil {
				results <- receipt{err: err}
				return
			}
			b, err := json.Marshal(response.StructuredContent)
			var out SaveOut
			if err == nil {
				err = json.Unmarshal(b, &out)
			}
			results <- receipt{want: want, draft: out.Draft, err: err, failed: response.IsError}
		}()
	}
	close(start)
	seen := map[int]bool{}
	for range 8 {
		got := <-results
		if got.err != nil || got.failed || got.draft.Hash != got.want || got.draft.Revision < 2 || got.draft.SavedAt.IsZero() {
			t.Fatalf("save receipt mismatch: %+v", got)
		}
		if seen[got.draft.Revision] {
			t.Fatalf("two saves returned revision %d", got.draft.Revision)
		}
		seen[got.draft.Revision] = true
	}
}

// These are real SDK calls. The lifecycle fixture helper intentionally splits
// save-and-create for old tests, so it cannot prove schema rejection here.
func TestDraftToolsRejectDeployArgument(t *testing.T) {
	for _, name := range []string{"save_draft", "save_draft_from_dir", "save_page"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			args := map[string]any{"slug": "hidden", "deploy": true}
			switch name {
			case "save_draft":
				args["files"] = []any{file("index.html", "must not save", "")}
			case "save_draft_from_dir":
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("must not save"), 0600); err != nil {
					t.Fatal(err)
				}
				args["dir"] = dir
			case "save_page":
				args["html"] = "<title>must not save</title>"
			}
			response, err := e.local.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
			if err == nil && !response.IsError {
				t.Fatal("hidden deploy argument accepted")
			}
			flats, err := e.svc.ListFlats(context.Background())
			if err != nil || len(flats) != 0 {
				t.Fatalf("schema refusal saved content: %+v %v", flats, err)
			}
			approvals, err := e.svc.ListApprovals(context.Background(), "pending")
			if err != nil || len(approvals) != 0 {
				t.Fatalf("schema refusal requested approval: %+v %v", approvals, err)
			}
		})
	}
}

func TestDeletionCleanupPendingReceipt(t *testing.T) {
	const message = "Version 2 was deleted. Storage cleanup is pending; repair storage and restart Flats. Do not repeat deletion."
	out := actionOut(core.ActionResult{Status: "cleanup_pending", CleanupPending: true, Version: 2, Message: message})
	if !out.CleanupPending || out.Status != "cleanup_pending" || !strings.Contains(actionText(out), "Do not repeat deletion") {
		t.Fatalf("cleanup receipt: %+v", out)
	}
	execution, _ := json.Marshal(core.ApprovalExecution{Status: "approved", CleanupPending: true, Version: 2})
	approval := approvalOut(store.Approval{ID: "apr-cleanup", Action: "delete_version", Status: "approved", Result: message, ResultData: execution})
	if approval.ResultData == nil || !approval.ResultData.CleanupPending {
		t.Fatalf("approved deletion lost cleanup state: %+v", approval)
	}
}
