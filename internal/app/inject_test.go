package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

// The key commands treat an injected secret like any other sealed value (docs/spec/injected-fetch-secrets.md,
// rule 31): restore-key accepts a key that opens it and keeps its binding, and reset names it and removes it.
func TestKeyCommandsWithAnInjectedSecret(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path, key, h := hostWithSecrets(t, dir)
	binding := store.Binding{Origin: "https://api.example.com", Header: "Authorization"}
	if _, err := h.Svc.SetSecretBinding(ctx, "app", "TOKEN", "Bearer app-injected-value", &core.SecretInject{Origin: binding.Origin, Header: binding.Header}, core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	pw := passwordFile(t, "a long recovery password\n")
	if _, err := runSecretKey(t, path, "backup-key", "--password-file", pw); err != nil {
		t.Fatal(err)
	}
	h.Close()
	// The export holds the binding of the secret.set event and never the value (rule 23).
	// The clock is past the lag of the export, so the events of this test are inside its range.
	stdout, stderr, err := runExport(t, path, time.Now().Add(time.Hour))
	if err != nil || !strings.Contains(stdout, `"origin":"https://api.example.com"`) || strings.Contains(stdout+stderr, "app-injected-value") {
		t.Fatalf("export: %v\n%s%s", err, stdout, stderr)
	}
	keyPath := filepath.Join(dir, core.SecretKeyFile)
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	out, err := runSecretKey(t, path, "restore-key", "--password-file", pw)
	if err != nil {
		t.Fatalf("restore-key: %q %v", out, err)
	}
	if got, err := os.ReadFile(keyPath); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("the restored key differs: %v", err)
	}
	binds := func() map[string]store.Binding {
		t.Helper()
		st, err := store.Open(filepath.Join(dir, "flats.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		secs, err := st.ListSecrets(ctx, "app")
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]store.Binding{}
		for _, s := range secs {
			m[s.Name] = s.Inject
		}
		return m
	}
	if got := binds(); got["TOKEN"] != binding || got["API_KEY"].Injected() {
		t.Fatalf("bindings after restore-key: %+v", got)
	}
	// Both the key and its copy are lost: reset names the injected secret and removes it.
	for _, p := range []string{keyPath, filepath.Join(dir, core.RecoveryKeyFile), filepath.Join(dir, core.RecoveryKeyFile) + ".id"} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	out, err = runSecretKey(t, path, "reset", "--yes")
	if err != nil || !strings.Contains(out, "secret TOKEN of app (injected: sent as Authorization to https://api.example.com)") || !strings.Contains(out, "--origin <origin> --header <header>") ||
		strings.Contains(out, "app-injected-value") {
		t.Fatalf("reset: %q %v", out, err)
	}
	if got := binds(); len(got) != 0 {
		t.Fatalf("secrets after reset: %+v", got)
	}
}
