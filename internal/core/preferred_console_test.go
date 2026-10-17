package core_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

// Under a --console-host flag, which replaces the Tailscale list for one run:
// a name only the flag lists cannot be preferred (it is not in config.json),
// and a stored preference naming a domain the flag leaves out is ignored for
// that run, with the warning, and kept in the file.
func TestPreferredConsoleUnderConsoleHostFlag(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	doc, err := config.New(config.NewInstanceID(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{{"network.permitted", "tailscale"}, {"tailscale.console_hosts", "stored,other"}, {config.PreferredHostKey, "tailscale:stored"}} {
		if err := doc.Set(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "config.json")
	hash, err := config.Create(path, doc)
	if err != nil {
		t.Fatal(err)
	}
	overrides := map[string]string{"tailscale.console_hosts": "flagonly,other"}
	src, err := core.NewSettingsSource(doc, &core.SettingsFile{Mode: "config", Hash: hash, Overrides: overrides,
		Save: config.NewWriter(path).Save, DiskHash: func() (string, error) {
			b, err := os.ReadFile(path)
			return config.Hash(b), err
		}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc, err := core.New(ctx, core.Config{DataDir: dir, Store: st, Settings: src, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	// The stored preference names a domain this run does not list: ignored.
	if v, _, _, ok := svc.ConsolePreferred(); v != "tailscale:stored" || ok {
		t.Fatalf("preference for this run: %q in effect=%v", v, ok)
	}
	c, err := doc.Effective(overrides)
	if err != nil {
		t.Fatal(err)
	}
	if c.PreferredHostProblem() == "" {
		t.Fatal("no warning for a preference the flag leaves out")
	}
	// A flag-only name is refused: config.json does not list it.
	if _, err := svc.SetConsolePreferred("tailscale:flagonly", svc.SettingsETag()); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("flag-only name: %v", err)
	}
	// A stored name the flag leaves out is refused as well: the console only
	// offers what it shows and serves this run.
	if _, err := svc.SetConsolePreferred("tailscale:stored", svc.SettingsETag()); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("stored name the flag omits: %v", err)
	}
	// A name in both lists may be preferred, and the preference is not pinned.
	if _, err := svc.SetConsolePreferred("tailscale:other", svc.SettingsETag()); err != nil {
		t.Fatal(err)
	}
	if v, n, name, ok := svc.ConsolePreferred(); v != "tailscale:other" || n != "tailscale" || name != "other" || !ok {
		t.Fatalf("after set: %q %q %q %v", v, n, name, ok)
	}
	if _, err := svc.SetConsolePreferred("local", svc.SettingsETag()); err != nil {
		t.Fatal(err)
	}
	// The list itself stays pinned by the flag.
	if _, err := svc.SetConsoleHostsOf(core.ConsoleProviderTailscale, []string{"other"}, ""); !errors.Is(err, core.ErrConfigOverridden) {
		t.Fatalf("list under the flag: %v", err)
	}
	l, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := l.Doc.Stored(config.PreferredHostKey); v != "local" {
		t.Fatalf("stored preference %v", v)
	}
	if v, _ := l.Doc.Stored("tailscale.console_hosts"); len(v.([]string)) != 2 {
		t.Fatalf("the flag's list was stored: %v", v)
	}
}
