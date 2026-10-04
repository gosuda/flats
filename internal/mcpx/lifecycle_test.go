package mcpx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func publishedFixture(t *testing.T, e *env) {
	t.Helper()
	for _, content := range []string{"VERSION-1", "VERSION-2"} {
		var out SaveOut
		text, failed := call(t, e.local, "save_version", map[string]any{"slug": "census", "files": []any{file("index.html", content, "")}, "deploy": true}, &out)
		if failed || out.Deploy == nil || out.Deploy.Status != "pending_approval" {
			t.Fatalf("fixture pending: %s %+v", text, out)
		}
		e.approve(t, out.Deploy.ApprovalID)
	}
}

func TestEveryMCPToolPreservesPendingApprovalWithoutOperatorAuthority(t *testing.T) {
	inventory := newEnv(t)
	listed, err := inventory.local.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		t.Run(tool.Name, func(t *testing.T) {
			e := newEnv(t)
			publishedFixture(t, e)
			if code, _ := e.operatorCall(t, "POST", "/flats/census/providers", `{"provider":"portal","permitted":true}`); code != 200 {
				t.Fatalf("fixture grant: %d", code)
			}
			var saved SaveOut
			call(t, e.local, "save_draft", map[string]any{"slug": "census", "files": []any{file("index.html", "DRAFT-3", "")}}, &saved)
			var pending ActionOut
			call(t, e.local, "publish", map[string]any{"slug": "census", "revision": 3}, &pending)
			if pending.Status != "pending_approval" {
				t.Fatalf("missing census approval: %+v", pending)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("DIR-DRAFT"), 0600); err != nil {
				t.Fatal(err)
			}
			args := map[string]map[string]any{
				"list_flats": {}, "get_flat": {"slug": "census"}, "get_draft": {"slug": "census"}, "create_flat": {"slug": "separate"},
				"save_version":          {"slug": "census", "expected_revision": 3, "files": []any{file("index.html", "NEW-DRAFT", "")}, "deploy": true},
				"save_draft":            {"slug": "census", "expected_revision": 3, "files": []any{file("index.html", "NEW-DRAFT", "")}},
				"save_version_from_dir": {"slug": "census", "expected_revision": 3, "dir": dir, "deploy": true},
				"list_versions":         {"slug": "census"}, "deploy": {"slug": "census", "version": 1}, "publish": {"slug": "census", "revision": 3},
				"rollback": {"slug": "census", "version": 1}, "open_preview": {"slug": "census", "target": "draft", "version": 0},
				"set_visibility": {"slug": "census", "visibility": "public"}, "delete_flat": {"slug": "census", "reason": "census"},
				"get_logs": {"slug": "census"}, "get_approval": {"id": pending.ApprovalID}, "list_secrets": {"slug": "census"}, "get_runtime_reference": {},
			}
			input, ok := args[tool.Name]
			if !ok {
				t.Fatalf("new tool lacks authority behavior census: %s", tool.Name)
			}
			text, failed := call(t, e.local, tool.Name, input, nil)
			if failed {
				t.Fatalf("valid census operation failed: %s", text)
			}
			if strings.Contains(text, mcpOperatorCredential) || strings.Contains(text, "flats_operator") {
				t.Fatal("tool leaked operator authority")
			}
			a, err := e.svc.GetApproval(context.Background(), pending.ApprovalID)
			if err != nil || a.Status != "pending" {
				t.Fatalf("MCP changed approval state: %+v %v", a, err)
			}
			f, err := e.svc.GetFlat(context.Background(), "census")
			if err != nil || f.LiveVersion != 2 || f.Versions != 2 || string(f.Visibility) != "private" || len(f.Providers) != 2 || f.Providers[0] != "local" || f.Providers[1] != "portal" {
				t.Fatalf("MCP applied unapproved action: %+v %v", f, err)
			}
			if _, body := get(t, f.PrivateURL); string(body) != "VERSION-2" {
				t.Fatalf("MCP changed serving content: %q", body)
			}
		})
	}
}

func TestMCPConflictPreservesContentAndCannotDecideOrGrant(t *testing.T) {
	e := newEnv(t)
	var saved SaveOut
	call(t, e.local, "save_draft", map[string]any{"slug": "conflict", "files": []any{file("index.html", "PRESERVED", "")}, "expected_revision": 0, "deploy": true}, &saved)
	if saved.Draft.Revision != 1 || saved.Deploy == nil {
		t.Fatalf("initial save: %+v", saved)
	}
	for _, expected := range []int{0, 99} {
		text, failed := call(t, e.local, "save_draft", map[string]any{"slug": "conflict", "files": []any{file("index.html", "OVERWRITE", "")}, "expected_revision": expected}, nil)
		if !failed || !strings.Contains(text, `"category":"conflict"`) {
			t.Fatalf("conflict cause absent: %s", text)
		}
		d, err := e.svc.GetDraft(context.Background(), "conflict")
		if err != nil || d.Revision != 1 || d.Hash != saved.Draft.Hash {
			t.Fatalf("conflict overwrote Draft: %+v %v", d, err)
		}
	}
	if _, err := e.svc.Decide(context.Background(), saved.Deploy.ApprovalID, true); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("agent context decided: %v", err)
	}
	if err := e.svc.SetProviderPermission(context.Background(), "conflict", "portal", true, core.ViaConsole); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("ViaConsole self-granted provider: %v", err)
	}
	for _, name := range []string{"approve", "decide", "reject", "set_provider_permission"} {
		res, err := e.local.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"id": saved.Deploy.ApprovalID}})
		if err == nil && !res.IsError {
			t.Fatalf("operator tool exists: %s", name)
		}
	}
	e.approve(t, saved.Deploy.ApprovalID)
	var a ApprovalOut
	call(t, e.local, "get_approval", map[string]any{"id": saved.Deploy.ApprovalID}, &a)
	if a.Status != "approved" || a.ResultData == nil || a.ResultData.Status != "approved" {
		t.Fatalf("authorized positive result DTO: %+v", a)
	}
}

func TestMCPDeployZeroRequestsCurrentDraftAndNoVersionBeforeDecision(t *testing.T) {
	e := newEnv(t)
	var saved SaveOut
	if text, failed := call(t, e.local, "save_draft", map[string]any{"slug": "zero", "expected_revision": 0, "files": []any{file("index.html", "ZERO-DRAFT", "")}}, &saved); failed {
		t.Fatal(text)
	}
	var pending DeployInfo
	text, failed := call(t, e.local, "deploy", map[string]any{"slug": "zero", "version": 0}, &pending)
	if failed || pending.Status != "pending_approval" || pending.ApprovalID == "" || pending.Approval == nil || pending.Approval.Action != "publish" || strings.Contains(text, " is live") {
		t.Fatalf("Draft deploy pending: %s %+v", text, pending)
	}
	f, _ := e.svc.GetFlat(context.Background(), "zero")
	if f.LiveVersion != 0 || f.Versions != 0 {
		t.Fatalf("premature vN: %+v", f)
	}
	e.approve(t, pending.ApprovalID)
	f, _ = e.svc.GetFlat(context.Background(), "zero")
	if f.LiveVersion != 1 || f.Versions != 1 {
		t.Fatalf("approved v1: %+v", f)
	}
	if _, body := get(t, f.PrivateURL); string(body) != "ZERO-DRAFT" {
		t.Fatalf("published bytes: %q", body)
	}
}

func TestMCPVisibilityWithoutProviderRemainsPending(t *testing.T) {
	e := newEnv(t)
	publishedFixture(t, e)
	var out ActionOut
	text, failed := call(t, e.local, "set_visibility", map[string]any{"slug": "census", "visibility": "public"}, &out)
	if failed || out.Status != "pending_approval" || out.ApprovalID == "" {
		t.Fatalf("provider preflight consumed request: %s %+v", text, out)
	}
	a, err := e.svc.GetApproval(context.Background(), out.ApprovalID)
	if err != nil || a.Status != "pending" {
		t.Fatalf("approval: %+v %v", a, err)
	}
	f, err := e.svc.GetFlat(context.Background(), "census")
	if err != nil || f.LiveVersion != 2 || string(f.Visibility) != "private" || len(f.Providers) != 1 || f.Providers[0] != "local" {
		t.Fatalf("request applied or granted provider: %+v %v", f, err)
	}
	if _, body := get(t, f.PrivateURL); string(body) != "VERSION-2" {
		t.Fatalf("changed current: %q", body)
	}
}

func TestMCPApplyingApprovalIsNotDescribedAsWaitingForDecision(t *testing.T) {
	e := newEnv(t)
	var saved SaveOut
	call(t, e.local, "save_draft", map[string]any{"slug": "applying", "files": []any{file("index.html", "one", "")}, "deploy": true}, &saved)
	if saved.Deploy == nil || saved.Deploy.ApprovalID == "" {
		t.Fatal("missing pending approval")
	}
	if err := e.st.ClaimApprovalAuthorized(t.Context(), saved.Deploy.ApprovalID, "operator fixture", time.Now()); err != nil {
		t.Fatal(err)
	}
	var approval ApprovalOut
	text, failed := call(t, e.local, "get_approval", map[string]any{"id": saved.Deploy.ApprovalID}, &approval)
	if failed || approval.Status != "applying" || !strings.Contains(text, "operation is still applying") || strings.Contains(text, "Waiting for the operator") {
		t.Fatalf("applying copy: %s %+v", text, approval)
	}
}

func TestGetFlatRetainsSummaryAlongsideDraftPreview(t *testing.T) {
	e := newEnv(t)
	publishedFixture(t, e)
	var saved SaveOut
	call(t, e.local, "save_draft", map[string]any{"slug": "census", "files": []any{file("index.html", "DRAFT-3", "")}}, &saved)
	if text, failed := call(t, e.local, "open_preview", map[string]any{"slug": "census", "target": "draft", "version": 0}, nil); failed {
		t.Fatal(text)
	}
	var out FlatOut
	text, failed := call(t, e.local, "get_flat", map[string]any{"slug": "census"}, &out)
	if failed || out.Flat.LiveVersion != 2 || len(out.Previews) != 1 {
		t.Fatalf("get_flat: %s %+v", text, out)
	}
	for _, want := range []string{"census (", "private", "2 version(s)", "Private URL:", "Private Draft preview of census revision 3", "Current version is unchanged"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
	if strings.Contains(text, "Preview of v0") {
		t.Errorf("Draft is mislabeled as a published version: %s", text)
	}
}

func TestToolErrorPreservesLifecycleCauseBeforeConflict(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"stale_approval", core.ErrStaleApproval},
		{"provider_not_permitted", core.ErrProviderNotPermitted},
		{"provider_not_ready", core.ErrProviderNotReady},
		{"provider_unavailable", errors.Join(core.ErrProviderNotReady, core.ErrProviderUnavailable)},
		{"unchanged_content", errors.Join(core.ErrProviderNotReady, core.ErrUnchangedContent)},
		{"provider_unavailable", core.ErrProviderUnavailable},
		{"runtime_unavailable", core.ErrRuntimeUnavailable},
		{"unavailable", core.ErrUnavailable},
		{"provider_in_use", core.ErrProviderInUse},
		{"not_deployed", core.ErrNotDeployed},
		{"runtime_start_failed", &core.DeployError{Cause: errors.New("startup failed")}},
		{"health_check_failed", &core.DeployError{}},
		{"public_stop_unconfirmed", core.ErrPublicStopUnconfirmed},
		{"unchanged_content", core.ErrUnchangedContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := toolErr(errors.Join(core.ErrConflict, tc.err), "")
			if !strings.Contains(err.Error(), `"category":"`+tc.name+`"`) {
				t.Fatalf("lost typed cause: %v", err)
			}
		})
	}
}
