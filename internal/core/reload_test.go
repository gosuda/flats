package core

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/store"
)

// newReloadService starts a service whose settings are saved to a config.json
// in dir, with the same file callbacks `flats serve` wires.
func newReloadService(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	doc, err := config.New(config.NewInstanceID(), dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	hash, err := config.Create(path, doc)
	if err != nil {
		t.Fatal(err)
	}
	src, err := NewSettingsSource(doc, &SettingsFile{Mode: "config", Hash: hash,
		Save: config.NewWriter(path).Save,
		DiskHash: func() (string, error) {
			b, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			return config.Hash(b), nil
		},
		Load: func() (*config.Loaded, error) { return config.Load(path) }})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(context.Background(), Config{DataDir: dir, Store: st, Settings: src,
		Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); st.Close() })
	return s, path
}

// editConfig applies mutate to the config file at path, as a text editor's
// save would leave it.
func editConfig(t *testing.T, path string, mutate func(*config.Document) error) {
	t.Helper()
	l, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.NewWriter(path).Save(l.Hash, mutate); err != nil {
		t.Fatal(err)
	}
}

// A reload adopts a valid changed file: the ETag advances to the file's hash,
// the changed-on-disk condition clears so a settings save succeeds again, and
// the changed keys are bucketed by when they apply.
func TestReloadAdoptsFileAndAdvancesETag(t *testing.T) {
	s, path := newReloadService(t)
	ctx := context.Background()
	etag0 := s.SettingsETag()
	editConfig(t, path, func(d *config.Document) error {
		for k, v := range map[string]string{
			"system.rate_limit_rps":      "7",
			"system.preview_ttl_seconds": "3600",
			"system.events_keep":         "100",
			"host.console_addr":          "127.0.0.1:9999",
			"metrics.enabled":            "false",
			"network.default_private":    "local",
		} {
			if err := d.Set(k, v); err != nil {
				return err
			}
		}
		return nil
	})
	// Until the reload, the file change blocks a settings save.
	if _, err := s.UpdateSettings(ctx, map[string]string{SetUploadMaxBytes: "4096"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("save over a changed file: %v", err)
	}
	rep, err := s.ReloadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ETag == "" || rep.ETag == etag0 || s.SettingsETag() != rep.ETag {
		t.Fatalf("ETag %q -> %q", etag0, rep.ETag)
	}
	if s.rateLimit() != 7 {
		t.Fatalf("rate limit %v", s.rateLimit())
	}
	if !slices.Equal(rep.Applied, []string{SetDefaultPrivate, SetRateLimit}) {
		t.Fatalf("applied %v", rep.Applied)
	}
	if !slices.Equal(rep.Periodic, []string{SetEventsKeep, SetPreviewTTL}) {
		t.Fatalf("periodic %v", rep.Periodic)
	}
	if !slices.Equal(rep.AwaitRestart, []string{"host.console_addr", "metrics.enabled"}) {
		t.Fatalf("await restart %v", rep.AwaitRestart)
	}
	if len(rep.Held) != 0 {
		t.Fatalf("held %v", rep.Held)
	}
	if _, err := s.UpdateSettings(ctx, map[string]string{SetUploadMaxBytes: "4096"}); err != nil {
		t.Fatalf("save after the reload: %v", err)
	}
}

// A file the host cannot parse is refused: nothing changes, including the
// ETag, and the reason is the parse error.
func TestReloadRefusesInvalidFile(t *testing.T) {
	s, path := newReloadService(t)
	etag0 := s.SettingsETag()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '}'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReloadConfig(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("invalid file: %v", err)
	}
	if s.SettingsETag() != etag0 {
		t.Fatal("a refused reload changed the ETag")
	}
	if _, err := s.UpdateSettings(context.Background(), map[string]string{SetUploadMaxBytes: "4096"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("save after a refused reload: %v", err)
	}
}

// A file from another schema version is refused in both directions: a reload
// cannot migrate a running host.
func TestReloadRefusesSchemaMismatch(t *testing.T) {
	for _, version := range []string{"4", "6"} {
		t.Run(version, func(t *testing.T) {
			s, path := newReloadService(t)
			etag0 := s.SettingsETag()
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			older := strings.Replace(string(raw), `"schema_version": 5`, `"schema_version": `+version, 1)
			if older == string(raw) {
				t.Fatal("schema_version not found")
			}
			if err := os.WriteFile(path, []byte(older), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ReloadConfig(context.Background()); err == nil {
				t.Fatal("a schema_version mismatch was reloaded")
			}
			if s.SettingsETag() != etag0 {
				t.Fatal("a refused reload changed the ETag")
			}
		})
	}
}

// host.instance_id and host.data_dir bind the file to the host: a file that
// changes either is refused.
func TestReloadRefusesHostMismatch(t *testing.T) {
	for _, tc := range []struct {
		key, value string
	}{
		{"host.instance_id", config.NewInstanceID()},
		{"host.data_dir", config.ScratchDataDir()},
	} {
		t.Run(tc.key, func(t *testing.T) {
			s, path := newReloadService(t)
			etag0 := s.SettingsETag()
			editConfig(t, path, func(d *config.Document) error { return d.Set(tc.key, tc.value) })
			_, err := s.ReloadConfig(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("host binding: %v", err)
			}
			if s.SettingsETag() != etag0 {
				t.Fatal("a refused reload changed the ETag")
			}
		})
	}
}

// A rate limit change reloads like a console save: the new rate is pushed to
// every live flat's limiter.
func TestReloadPushesRateLimit(t *testing.T) {
	s, path := newReloadService(t)
	ctx := context.Background()
	if _, err := s.CreateFlat(ctx, "app", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	editConfig(t, path, func(d *config.Document) error { return d.Set("system.rate_limit_rps", "3") })
	if _, err := s.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	lf := s.live["app"]
	s.mu.Unlock()
	lf.limiter.mu.Lock()
	rate := lf.limiter.rate
	lf.limiter.mu.Unlock()
	if rate != 3 {
		t.Fatalf("live limiter rate %v", rate)
	}
}

// A reload refuses a file that changes a value a legacy service flag pins,
// whose next start would refuse to run with it, and changes nothing; one that
// leaves the pinned value alone is adopted.
func TestReloadRefusesPinnedSettingChange(t *testing.T) {
	for _, c := range []struct {
		name, key, value, base, by string
		pin                        func(*SettingsSource)
	}{
		{"relays", "portal.relays", "https://relay.example.com", "", "the service's --relays flag",
			func(s *SettingsSource) { s.Pin(SetPortalRelays, "the service's --relays flag") }},
		{"console domains", consoleHostsKey, "ops", "", "the service's --console-host flag",
			func(s *SettingsSource) { s.PinConsoleHosts("the service's --console-host flag") }},
		{"provider", "network.permitted", "tailscale,portal", "tailscale", "the service's --portal flag",
			func(s *SettingsSource) { s.PinProvider("portal", "the service's --portal flag") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, path := newReloadService(t)
			ctx := context.Background()
			// Console domains need Tailscale permitted; it is not pinned.
			editConfig(t, path, func(d *config.Document) error { return d.Set("network.permitted", "tailscale") })
			if _, err := s.ReloadConfig(ctx); err != nil {
				t.Fatal(err)
			}
			c.pin(s.settings)
			etag, hosts, permitted := s.SettingsETag(), hostsOf(s), s.PermittedNetworks()
			editConfig(t, path, func(d *config.Document) error {
				if err := d.Set("system.rate_limit_rps", "3"); err != nil {
					return err
				}
				return d.Set(c.key, c.value)
			})
			_, err := s.ReloadConfig(ctx)
			if !errors.Is(err, ErrConfigOverridden) || !strings.Contains(err.Error(), c.by) || !strings.Contains(err.Error(), c.key) {
				t.Fatalf("reload of a changed pinned value: %v", err)
			}
			if s.SettingsETag() != etag || s.rateLimit() == 3 || !slices.Equal(hostsOf(s), hosts) || !slices.Equal(s.PermittedNetworks(), permitted) {
				t.Fatal("a refused reload changed the settings")
			}
			// The same file without the pinned change is adopted.
			editConfig(t, path, func(d *config.Document) error {
				if c.base == "" {
					return d.Unset(c.key)
				}
				return d.Set(c.key, c.base)
			})
			rep, err := s.ReloadConfig(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(rep.Applied, []string{SetRateLimit}) || len(rep.Held) != 0 {
				t.Fatalf("applied %v held %v", rep.Applied, rep.Held)
			}
		})
	}
}

// hostsOf returns the Tailscale console domains in effect.
func hostsOf(s *Service) []string {
	l, _ := s.ConsoleHostsOf(ConsoleProviderTailscale)
	return l
}

// A per-run override keeps its value across a reload that changes the file's.
func TestReloadKeepsRunOverride(t *testing.T) {
	s, path := newReloadService(t)
	s.settings.file.Overrides = map[string]string{"host.console_addr": "127.0.0.1:0"}
	editConfig(t, path, func(d *config.Document) error { return d.Set("host.console_addr", "127.0.0.1:9999") })
	rep, err := s.ReloadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Held, []ReloadHeld{{Key: "host.console_addr", By: "the service's --listen flag"}}) {
		t.Fatalf("held %v", rep.Held)
	}
	if slices.Contains(rep.AwaitRestart, "host.console_addr") {
		t.Fatalf("a held key awaits a restart: %v", rep.AwaitRestart)
	}
}

// The console domain lists are reported before and after, and the adopted
// network.permitted is in effect, so the host can reconcile them live.
func TestReloadReportsProvidersAndConsoleHosts(t *testing.T) {
	s, path := newReloadService(t)
	// netbird console hosts without a netbird grant are invalid; a raw edit
	// leaves such a file and the reload refuses it.
	l, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"network.permitted":       "tailscale",
		"tailscale.console_hosts": "flats",
		"netbird.console_hosts":   "ops",
	} {
		if err := l.Doc.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, l.Doc.Encode(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReloadConfig(context.Background()); err == nil {
		t.Fatal("an invalid cross-key file was reloaded")
	}
	if err := l.Doc.Set("network.permitted", "tailscale,netbird"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, l.Doc.Encode(), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := s.ReloadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := s.PermittedNetworks(); !slices.Equal(got, []string{"tailscale", "netbird"}) {
		t.Fatalf("permitted %v", got)
	}
	if len(rep.TailscaleHostsBefore) != 0 || !slices.Equal(rep.TailscaleHostsAfter, []string{"flats"}) {
		t.Fatalf("tailscale hosts %v -> %v", rep.TailscaleHostsBefore, rep.TailscaleHostsAfter)
	}
	if len(rep.NetbirdHostsBefore) != 0 || !slices.Equal(rep.NetbirdHostsAfter, []string{"ops"}) {
		t.Fatalf("netbird hosts %v -> %v", rep.NetbirdHostsBefore, rep.NetbirdHostsAfter)
	}
	if len(rep.Applied) != 0 || len(rep.AwaitRestart) != 0 {
		t.Fatalf("buckets %v %v", rep.Applied, rep.AwaitRestart)
	}
}

// failHiddenPublic is a PublicNet whose relay-listing update fails.
type failHiddenPublic struct {
	memPublic
	err error
}

func (n *failHiddenPublic) SetHidden(string, bool) error { return n.err }

// A side effect that fails after the file is adopted changes nothing about
// the adoption: the ETag stays at the file's hash, the new value is in
// effect, the failure is logged on the flat, and the host keeps serving.
func TestReloadSideEffectFailureKeepsAdoptedFile(t *testing.T) {
	s, path := newReloadService(t)
	ctx := context.Background()
	s.cfg.Public = &failHiddenPublic{err: errors.New("relay listing update refused")}
	if _, err := s.CreateFlat(ctx, "blog", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	// A public flat with a served route, following the host's listing default.
	f, err := s.st.GetFlat(ctx, "blog")
	if err != nil {
		t.Fatal(err)
	}
	f.Visibility = store.Public
	if err := s.st.UpdateFlat(ctx, f); err != nil {
		t.Fatal(err)
	}
	s.state("blog").publicServed = true

	etag0 := s.SettingsETag()
	editConfig(t, path, func(d *config.Document) error { return d.Set("portal.hide", "true") })
	rep, err := s.ReloadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ETag == etag0 || s.SettingsETag() != rep.ETag {
		t.Fatalf("ETag %q -> %q", etag0, rep.ETag)
	}
	if !slices.Contains(rep.Applied, SetPortalHide) {
		t.Fatalf("applied %v", rep.Applied)
	}
	if got := s.settings.get(SetPortalHide); got != "true" {
		t.Fatalf("portal.hide = %q", got)
	}
	events, err := s.st.ListEvents(ctx, "blog", "listing", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Level != "error" || !strings.Contains(events[0].Message, "relay listing update refused") {
		t.Fatalf("events %+v", events)
	}
	// The host keeps serving: a settings save against the new ETag works.
	if _, err := s.UpdateSettingsMatch(ctx, map[string]string{SetUploadMaxBytes: "4096"}, rep.ETag); err != nil {
		t.Fatalf("save after the failed side effect: %v", err)
	}
}

// A reload holds the settings lock like a console save: a save that reaches
// the file write always observes the reload's adopted values. If the reload
// did not hold the lock while it loaded, the save would run first against
// the pre-reload settings, and see their values or find the file changed on
// disk.
func TestReloadSerializesWithConsoleSave(t *testing.T) {
	s, path := newReloadService(t)
	ctx := context.Background()
	editConfig(t, path, func(d *config.Document) error {
		return d.Set("system.upload_max_bytes", "4096")
	})
	loadEntered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	origLoad := s.settings.file.Load
	s.settings.file.Load = func() (*config.Loaded, error) {
		once.Do(func() { close(loadEntered); <-release })
		return origLoad()
	}
	// commit calls Save with the settings lock held.
	staleSave := false
	origSave := s.settings.file.Save
	s.settings.file.Save = func(expected string, apply func(*config.Document) error) (string, error) {
		if s.settings.values[SetUploadMaxBytes] != "4096" {
			staleSave = true
		}
		return origSave(expected, apply)
	}
	reloaded := make(chan error, 1)
	go func() {
		_, err := s.ReloadConfig(ctx)
		reloaded <- err
	}()
	<-loadEntered // the reload holds the settings lock
	// A console provider change takes only the settings lock, unlike a
	// settings save, which also waits for the reload's guest-editing lock.
	saved := make(chan error, 1)
	go func() {
		_, err := s.SetPermittedNetworks([]string{"tailscale"}, "")
		saved <- err
	}()
	// Released only once the save waits for the settings lock the reload
	// holds; a save that does not wait finishes first and is never parked.
	waitFor(t, "the save to wait for the settings lock", func() bool {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		for _, g := range strings.Split(string(buf), "\n\n") {
			if strings.Contains(g, "SemacquireMutex") && strings.Contains(g, "(*SettingsSource).setPermitted") {
				return true
			}
		}
		return false
	})
	close(release)
	if err := <-reloaded; err != nil {
		t.Fatal(err)
	}
	if err := <-saved; err != nil {
		t.Fatalf("save after the reload: %v", err)
	}
	if staleSave {
		t.Fatal("a save committed with pre-reload values: the reload did not hold the settings lock")
	}
	if s.UploadLimit() != 4096 || !slices.Equal(s.PermittedNetworks(), []string{"tailscale"}) {
		t.Fatal("reload or save result missing")
	}
}

// A reload adopts a changed host.secret_key_path and lists it as awaiting a restart. The running host keeps the
// key that it read at start (docs/spec/secret-key-path.md, rule 8).
func TestReloadListsTheSecretKeyPathAsAwaitingARestart(t *testing.T) {
	s, path := newReloadService(t)
	before := string(s.secretKey)
	editConfig(t, path, func(d *config.Document) error {
		return d.Set("host.secret_key_path", filepath.Join(t.TempDir(), "elsewhere", "secret.key"))
	})
	rep, err := s.ReloadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.AwaitRestart, []string{"host.secret_key_path"}) {
		t.Fatalf("await restart %v", rep.AwaitRestart)
	}
	if len(rep.Applied) != 0 || len(rep.Held) != 0 {
		t.Errorf("applied %v, held %v; want none", rep.Applied, rep.Held)
	}
	if string(s.secretKey) != before {
		t.Error("the running host changed its key at a reload")
	}
}
