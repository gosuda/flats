package mcpx

import (
	"context"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/clientstate"
)

func TestClientTools(t *testing.T) {
	e := newEnv(t)
	tools, err := e.remote.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, tool := range tools.Tools {
		if tool.Name == "check_client" || tool.Name == "get_client_skill" {
			count++
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
				t.Fatal("client tools must be read-only")
			}
		}
	}
	if count != 2 {
		t.Fatal("client tools missing")
	}
	var out clientstate.CheckResult
	text, failed := call(t, e.remote, "check_client", map[string]any{"cli_present": false, "bootstrap_present": false}, &out)
	if failed {
		t.Fatal(text)
	}
	if out.CLIStatus != "not_applicable" || out.BootstrapStatus != "not_applicable" || out.GuideStatus != "unknown" || out.Skill != "" {
		t.Fatalf("bad direct HTTP result: %+v", out)
	}
	if !strings.HasPrefix(out.ServerURL, "http://") {
		t.Fatalf("no request origin: %q", out.ServerURL)
	}
	var skill clientstate.Manifest
	text, failed = call(t, e.remote, "get_client_skill", map[string]any{}, &skill)
	if failed {
		t.Fatal(text)
	}
	if err := clientstate.ValidateManifest(skill); err != nil {
		t.Fatal(err)
	}
	if skill.BootstrapSHA256 != out.BootstrapSHA256 || skill.GuideRevision != out.GuideRevision {
		t.Fatal("tools disagree on manifest")
	}
	var guideOut GuideOut
	text, failed = call(t, e.remote, "guide", map[string]any{"items": []string{"topic.client-update"}}, &guideOut)
	if failed || guideOut.GuideRevision != out.GuideRevision || !strings.Contains(text, out.GuideRevision) {
		t.Fatalf("guide revision mismatch: %s", text)
	}
	text, failed = call(t, e.remote, "check_client", map[string]any{"cli_present": false, "client_contract": 1}, &out)
	if !failed {
		t.Fatalf("accepted conflicting identity: %s", text)
	}
}
