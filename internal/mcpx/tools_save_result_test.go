package mcpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func publishedSaveArgs(t *testing.T, name, content string) map[string]any {
	t.Helper()
	args := map[string]any{"slug": "published"}
	switch name {
	case "save_draft":
		args["files"] = []any{file("index.html", content, "")}
	case "save_draft_from_dir":
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		args["dir"] = dir
	case "save_page":
		args["html"] = content
	}
	return args
}

func TestPublishedSaveResults(t *testing.T) {
	for _, name := range []string{"save_draft", "save_draft_from_dir", "save_page"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			publicInstanceOn(t, e)
			for n := 1; n <= 2; n++ {
				var out SaveOut
				text, failed := call(t, e.local, name, publishedSaveArgs(t, name, fmt.Sprintf("<title>Save %d</title>", n)), &out)
				if failed || out.Version != n || out.Draft.Revision != n || out.Flat == nil || out.Flat.LiveVersion != n || out.Flat.PublicURL == "" || out.Flat.PublicNotice != core.PublicAccessNotice {
					t.Fatalf("published receipt: %s %+v", text, out)
				}
				summary := strings.Split(text, "\n{")[0]
				for _, want := range []string{fmt.Sprintf("published as version %d", n), out.Flat.PublicURL, out.Flat.PublicNotice} {
					if !strings.Contains(summary, want) {
						t.Fatalf("missing %q: %s", want, summary)
					}
				}
				if strings.Contains(summary, "Next:") || strings.Contains(summary, "Current version is unchanged") || strings.Contains(summary, "Private") {
					t.Fatal(summary)
				}
			}
			one := 2
			if err := e.svc.SetVersionQuotaOverride(t.Context(), "published", &one); err != nil {
				t.Fatal(err)
			}
			text, failed := call(t, e.local, name, publishedSaveArgs(t, name, "<title>Kept</title>"), nil)
			if !failed || !strings.Contains(text, "Draft revision 3 was saved but not published") || !strings.Contains(text, "version quota") || strings.Contains(text, "no Draft was saved") {
				t.Fatalf("failed save: %s", text)
			}
			f, err := e.svc.GetFlat(t.Context(), "published")
			if err != nil || f.LiveVersion != 2 || f.Versions != 2 || f.Draft.Revision != 3 {
				t.Fatalf("lost Draft or changed live version: %+v %v", f, err)
			}
			if _, err := e.svc.SetPublicInstance(t.Context(), false, "", "test"); err != nil {
				t.Fatal(err)
			}
			var out SaveOut
			text, failed = call(t, e.local, name, publishedSaveArgs(t, name, "<title>Draft only</title>"), &out)
			want := fmt.Sprintf("Saved Private Draft revision 4 of published (%d files, %d bytes). Current version is unchanged (live v2). Next: open_preview with version 0, or create_version to request operator approval.", out.Draft.Files, out.Draft.Size) + warningsText(out.Warnings)
			if failed || out.Version != 0 || out.Flat != nil || strings.Split(text, "\n{")[0] != want || strings.Contains(text, `"version":`) || strings.Contains(text, `"flat":{`) {
				t.Fatalf("off text changed: %s\nwant %s", text, want)
			}
			approvals, err := e.svc.ListApprovals(t.Context(), "pending")
			if err != nil || len(approvals) != 0 {
				t.Fatalf("save requested approvals: %v %v", approvals, err)
			}
		})
	}
}

func TestSavedDraftFailureHints(t *testing.T) {
	e := newEnv(t)
	tool := &tools{svc: e.svc}
	for _, tc := range []struct {
		name     string
		cause    error
		category string
	}{
		{"disk quota", fmt.Errorf("%w: %w", core.ErrConflict, core.ErrDiskQuota), "conflict"},
		{"health", &core.DeployError{Health: core.HealthResult{Status: 500, BodyHead: "not ready"}}, "health_check_failed"},
		{"start", &core.DeployError{Cause: errors.New("worker could not start")}, "runtime_start_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := tool.saved(t.Context(), "missing", nil, store.Version{Revision: 9}, &core.SavedDraftError{Revision: 9, Cause: tc.cause}, false)
			if err == nil || !strings.Contains(err.Error(), "Draft revision 9 was saved but not published") || !strings.Contains(err.Error(), `"category":"`+tc.category+`"`) || strings.Contains(err.Error(), "no Draft was saved") {
				t.Fatalf("contradictory failure hint: %v", err)
			}
		})
	}
}

func TestDraftOnlySaveOutBytes(t *testing.T) {
	b, err := json.Marshal(SaveOut{})
	if err != nil || string(b) != "{}" {
		t.Fatalf("optional result fields leaked: %s %v", b, err)
	}
}

func TestLegacySaveResultInPublicInstanceMode(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.CreateFlat(t.Context(), "published", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	publicInstanceOn(t, e)
	var out SaveOut
	text, failed := call(t, e.local, "save_draft", publishedSaveArgs(t, "save_draft", "<title>Legacy</title>"), &out)
	want := fmt.Sprintf("Saved Draft revision 1 of published (%d files, %d bytes). Current version is unchanged (not deployed). Private previews are disabled. Next: create_version to request approval, then deploy that version.", out.Draft.Files, out.Draft.Size) + warningsText(out.Warnings)
	if failed || out.Version != 0 || out.Flat != nil || strings.Split(text, "\n{")[0] != want || strings.Contains(text, `"version":`) || strings.Contains(text, `"flat":{`) {
		t.Fatalf("legacy text changed: %s\nwant %s", text, want)
	}
}

func TestPublishedSaveResultWithoutPublicLink(t *testing.T) {
	e := newEnv(t)
	publicInstanceOn(t, e)
	files := []bundle.File{{Path: "index.html", Data: []byte("<title>Published</title>")}}
	v, err := e.svc.SaveVersion(t.Context(), "published", files, core.SaveMeta{}, core.ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetProviderPermission(t.Context(), "published", "portal", false); err != nil {
		t.Fatal(err)
	}
	res, out, err := (&tools{svc: e.svc}).saved(t.Context(), "published", files, v, nil, false)
	if err != nil || out.Flat == nil || out.Version != 1 || out.Flat.PublicURL != "" {
		t.Fatalf("publication receipt: %+v %v", out, err)
	}
	summary := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(summary, "published as version 1") || !strings.Contains(summary, "the public link is not open yet (unavailable)") || !strings.Contains(summary, out.Flat.PublicNotice) || strings.Contains(summary, "Next:") {
		t.Fatal(summary)
	}
}
