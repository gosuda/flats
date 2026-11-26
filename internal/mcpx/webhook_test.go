package mcpx

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

// docs/spec/event-webhook.md, rule 9: an agent cannot read or change the webhook. No tool has webhook in its name.
func TestNoAgentToolIsAboutTheWebhook(t *testing.T) {
	e := newEnv(t)
	names := toolNames(t, e.local)
	if len(names) == 0 {
		t.Fatal("the tool list is empty")
	}
	for _, name := range names {
		if strings.Contains(strings.ToLower(name), "webhook") {
			t.Errorf("the tool %q is about the webhook", name)
		}
	}
}

// envDataDir returns the data directory of the environment of this test. newEnv keeps no name of it, so the test finds
// it beside a temporary directory of its own: the temporary directories of one test share a parent, and only the data
// directory holds flats.db.
func envDataDir(t *testing.T) string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(filepath.Dir(t.TempDir()), "*", "flats.db"))
	if err != nil || len(found) != 1 {
		t.Fatalf("flats.db of the environment: found %v, %v; want one database", found, err)
	}
	return filepath.Dir(found[0])
}

// docs/spec/event-webhook.md, rule 9: with both webhook keys stored, nothing that an agent reads holds the URL or the
// secret, and no tool that lists or describes the host and its flats says that a webhook exists.
func TestNoAgentToolShowsTheWebhook(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	const (
		token  = "CANARY-url-token-5c1e"
		secret = "whsec_CANARY-secret-77aa0123456789"
	)
	// The values are sealed with the key of the environment, so its service could open them.
	keyFile := core.NewKeyFile(envDataDir(t), "")
	if _, err := os.Stat(keyFile.Path()); err != nil {
		t.Fatalf("the key of the environment: %v", err)
	}
	for id, value := range map[string]string{core.HostKeyWebhookURL: "https://hooks.example.com/in?token=" + token, core.HostKeyWebhookSecret: secret} {
		if err := core.PutHostKeyValue(ctx, keyFile, e.st, id, value, core.KeySourceCLI, time.Now(), store.Audit{Via: store.AuditViaCommand}); err != nil {
			t.Fatalf("store the %s key: %v", id, err)
		}
		if got, _, err := core.StoredHostKey(ctx, keyFile, e.st, id); err != nil || got != value {
			t.Fatalf("the stored %s key opens to %q, %v; the test needs the value in the store", id, got, err)
		}
	}

	// A flat with a draft, a log, a grant, a secret and a pending approval, so that every tool has something to show.
	if _, err := e.svc.SaveVersion(ctx, "app", []bundle.File{{Path: "index.html", Data: []byte("x")}}, core.SaveMeta{}, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetNetworkPolicy(ctx, "app", []string{"https://api.example.com"}, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetSecret(ctx, "app", "API_KEY", "a-secret-value", core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	del, err := e.svc.Delete(ctx, "app", core.ViaMCP, "")
	if err != nil || del.Approval == nil {
		t.Fatalf("Delete = %+v, %v", del, err)
	}

	calls := []struct {
		name string
		args map[string]any
	}{
		{"list_flats", map[string]any{}},
		{"get_flat", map[string]any{"slug": "app"}},
		{"get_logs", map[string]any{"slug": "app", "limit": 500}},
		{"get_network", map[string]any{"slug": "app"}},
		{"list_env", map[string]any{"slug": "app"}},
		{"list_secrets", map[string]any{"slug": "app"}},
		{"list_versions", map[string]any{"slug": "app"}},
		{"get_draft", map[string]any{"slug": "app"}},
		{"get_approval", map[string]any{"id": del.Approval.ID}},
		{"get_runtime_reference", map[string]any{}},
		{"get_content_types", map[string]any{}},
		{"get_client_skill", map[string]any{}},
		{"check_client", map[string]any{"cli_present": false, "guide_revision": strings.Repeat("0", 64)}},
		{"guide", map[string]any{}},
	}
	// These tools read the content of a flat of another type and nothing of the host.
	contentReaders := []string{"get_board", "get_board_share", "get_design", "get_document", "get_notebook", "list_revisions", "get_revision"}
	names := toolNames(t, e.local)
	called := map[string]bool{}
	for _, c := range calls {
		called[c.name] = true
		if !slices.Contains(names, c.name) {
			t.Errorf("the tool list has no %s; name the tool that replaced it here", c.name)
		}
	}
	for _, name := range names {
		reads := strings.HasPrefix(name, "get_") || strings.HasPrefix(name, "list_") || strings.HasPrefix(name, "check_") || name == "guide"
		if reads && !called[name] && !slices.Contains(contentReaders, name) {
			t.Errorf("the tool %s reads something and this test does not call it; add it to the calls", name)
		}
	}

	for session, cs := range map[string]*mcp.ClientSession{"loopback": e.local, "tailnet": e.remote} {
		for _, c := range calls {
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: c.name, Arguments: c.args})
			if err != nil || res.IsError {
				t.Fatalf("%s from %s: %v %v", c.name, session, err, res)
			}
			raw, err := json.Marshal(res)
			if err != nil {
				t.Fatal(err)
			}
			text := resultText(res) + "\n" + string(raw)
			if strings.Contains(text, token) || strings.Contains(text, secret) {
				t.Errorf("%s from %s returned a webhook key value: %s", c.name, session, text)
			}
			if strings.Contains(strings.ToLower(text), "webhook") {
				t.Errorf("%s from %s mentions the webhook: %s", c.name, session, text)
			}
		}
	}

	// Everything else that an agent reads without a change: the instructions, the tool list with its descriptions,
	// every guide item, the resources, the llms documents and GET /api/client.
	for what, text := range agentSurface(t, e, del.Approval.ID) {
		if strings.Contains(text, token) || strings.Contains(text, secret) {
			t.Errorf("%s holds a webhook key value: %s", what, text)
		}
	}
}
