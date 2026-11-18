package mcpx

import (
	"context"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

// list_secrets shows the binding of an injected secret and never its value (docs/spec/injected-fetch-secrets.md,
// rule 24), and its description names the injection.
func TestListSecretsShowsTheBinding(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.CreateFlat(ctx, "app", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetSecretBinding(ctx, "app", "TOKEN", "Bearer mcp-injected-value", &core.SecretInject{Origin: "https://api.example.com", Header: "Authorization"}, core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetSecret(ctx, "app", "PLAIN", "plain-value", core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	var out SecretsOut
	text, failed := call(t, e.local, "list_secrets", map[string]any{"slug": "app"}, &out)
	if failed || len(out.Secrets) != 2 || strings.Contains(text, "mcp-injected-value") || strings.Contains(text, "plain-value") {
		t.Fatalf("list_secrets: %s %+v", text, out)
	}
	for _, s := range out.Secrets {
		switch s.Name {
		case "TOKEN":
			if s.Inject == nil || s.Inject.Origin != "https://api.example.com" || s.Inject.Header != "Authorization" {
				t.Errorf("TOKEN: %+v", s)
			}
		case "PLAIN":
			if s.Inject != nil {
				t.Errorf("PLAIN: %+v", s)
			}
		}
	}
	if !strings.Contains(out.Note, "not in env") {
		t.Errorf("note: %s", out.Note)
	}
	tools, err := e.local.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "list_secrets" && (!strings.Contains(tool.Description, "injected secret") || !strings.Contains(tool.Description, "not in env")) {
			t.Errorf("description: %s", tool.Description)
		}
	}
}
