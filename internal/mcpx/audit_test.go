package mcpx

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gosuda/flats/internal/core"
)

// docs/spec/audit-events.md, rule 24: an agent cannot read the audit log. No tool has audit in its name, and get_logs of a
// flat that has secrets returns the flat's own log and none of the audit rows.
func TestNoAgentToolReadsTheAuditLog(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	for _, name := range toolNames(t, e.local) {
		if strings.Contains(strings.ToLower(name), "audit") {
			t.Errorf("the tool %q reads the audit log", name)
		}
	}
	if _, err := e.svc.CreateFlat(ctx, "app", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetSecret(ctx, "app", "API_KEY", "distinctive-secret-value", core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteSecret(ctx, "app", "API_KEY", core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"get_logs", map[string]any{"slug": "app", "limit": 500}},
		{"list_secrets", map[string]any{"slug": "app"}},
		{"get_flat", map[string]any{"slug": "app"}},
	} {
		res, err := e.local.CallTool(ctx, &mcp.CallToolParams{Name: call.name, Arguments: call.args})
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %v", call.name, err, res)
		}
		text := resultText(res)
		b, _ := json.Marshal(res.StructuredContent)
		raw := string(b)
		for _, bad := range []string{"secret.set", "secret.delete", "flat_id", "flat-delete", "audit"} {
			if strings.Contains(text, bad) || strings.Contains(raw, bad) {
				t.Errorf("%s returned %q: %s %s", call.name, bad, text, raw)
			}
		}
	}
}
