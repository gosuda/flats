package mcpx

import (
	"context"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

func TestEnvTools(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.CreateFlat(context.Background(), "envapp", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetSecret(context.Background(), "envapp", "TOKEN", "test-secret", core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	var change EnvChangeOut
	if text, failed := call(t, e.remote, "set_env", map[string]any{"slug": "envapp", "name": "MODE", "value": "staging"}, &change); failed || change.Status != "stored" || strings.Contains(text, "staging") || !describesEnvActivation(change.Note) {
		t.Fatalf("set: %s %+v", text, change)
	}
	var listed EnvOut
	if text, failed := call(t, e.local, "list_env", map[string]any{"slug": "envapp"}, &listed); failed || len(listed.Env) != 1 || listed.Env[0].Value != "staging" || strings.Contains(text, "test-secret") || !describesEnvActivation(listed.Note) {
		t.Fatalf("list: %s %+v", text, listed)
	}
	for _, name := range []string{"TOKEN", "DB", "bad-name"} {
		if text, failed := call(t, e.local, "set_env", map[string]any{"slug": "envapp", "name": name, "value": "ordinary"}, nil); !failed || strings.Contains(text, "test-secret") {
			t.Fatalf("invalid %s: %s", name, text)
		}
	}
	if text, failed := call(t, e.local, "set_env", map[string]any{"slug": "envapp", "name": "MODE", "value": ""}, &change); failed {
		t.Fatalf("empty: %s", text)
	}
	if text, failed := call(t, e.remote, "delete_env", map[string]any{"slug": "envapp", "name": "MODE"}, &change); failed || change.Status != "deleted" || !describesEnvActivation(change.Note) {
		t.Fatalf("delete: %s %+v", text, change)
	}
	if text, failed := call(t, e.local, "list_env", map[string]any{"slug": "envapp"}, &listed); failed || len(listed.Env) != 0 {
		t.Fatalf("empty list: %s %+v", text, listed)
	}
	if text, failed := call(t, e.local, "list_env", map[string]any{"slug": "missing"}, nil); !failed {
		t.Fatalf("missing: %s", text)
	}
}

func describesEnvActivation(note string) bool {
	return strings.Contains(note, "after any required approval") && strings.Contains(note, "rollback or data restoration") && strings.Contains(note, "Flats host restart") && strings.Contains(note, "New previews capture current settings") && strings.Contains(note, "automatic worker restarts reuse their captured settings")
}
