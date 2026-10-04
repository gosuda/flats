package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/expose/portal"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/store"
)

// legacyDir builds a data directory as the release before config.json left
// it: the real v5 schema with its sample rows, console settings, a host
// file with a Portal grant and a historical tailnet, and the secret key.
func legacyDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ddl, err := os.ReadFile(filepath.Join("..", "store", "testdata", "schema", "v5-80b206b.sql"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "flats.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		core.SetKeepVersions:    "20",
		core.SetRateLimit:       "50", // equal to the default: not written
		core.SetUploadMaxBytes:  "lots",
		core.SetPortalRelays:    "https://rly.best/",
		core.SetPortalDiscover:  "false",
		core.SetPortalMaxRelay:  "5",
		core.SetDiskQuotaBytes:  "0",
		core.SetPreviewTTL:      "3600",
		core.SetEventsKeep:      "5000",
		core.SetRedirectDays:    "14",
		"unrelated_legacy_note": "kept in the table only",
	} {
		if _, err := db.Exec(`INSERT INTO settings(key,value) VALUES(?,?)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "tsnet", "blog"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(dir, provider.File{Version: 1, Permitted: []provider.ID{provider.Portal}, PrivateBackend: "local",
		Migration: provider.Migration{HistoricalTSNet: true}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.key"), bytes.Repeat([]byte{7}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// legacyFlags are the service flags of the legacy host in legacyDir.
func legacyFlags(t *testing.T, dir string, args ...string) Options {
	t.Helper()
	o, err := ParseServeFlags(append([]string{"--data", dir}, args...))
	if err != nil {
		t.Fatal(err)
	}
	o.Overrides = map[string]string{"host.management_addr": "127.0.0.1:0", "host.local_addr": "127.0.0.1:0", "host.server_runtime": "false"}
	return o
}

func hashFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func readBinding(t *testing.T, dir string) *store.HostBinding {
	t.Helper()
	info, err := store.Inspect(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	return info.Binding
}

// legacyPolicyToken is the exposure policy token the release before
// config.json computed for this host: the host file as its flags saved it,
// and the Portal configuration read from the settings table.
func legacyPolicyToken(t *testing.T, file provider.File, cfg portal.Config) string {
	t.Helper()
	dir := t.TempDir()
	if err := provider.Save(dir, file); err != nil {
		t.Fatal(err)
	}
	loop, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer loop.Close()
	cfg.Dir = filepath.Join(dir, "portal")
	pn, err := portal.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pn.Close()
	m, err := provider.New(dir, provider.Options{Local: loop, Portal: pn, Configuration: providerConfiguration(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	token, err := m.ExposurePolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestLegacyUpgradeEndToEnd(t *testing.T) {
	dir := legacyDir(t)
	credential := filepath.Join(t.TempDir(), "operator")
	if err := os.WriteFile(credential, []byte(strings.Repeat("c", 40)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	flags := []string{"--network", "local", "--console-host", "console", "--operator-credential-file", credential}
	before := legacyPolicyToken(t, provider.File{Version: 1, Permitted: []provider.ID{provider.Portal}, PrivateBackend: "local"},
		portal.Config{Relays: []string{"https://rly.best"}, Discovery: false, MaxActiveRelays: 5})

	h, err := Start(context.Background(), legacyFlags(t, dir, flags...))
	if err != nil {
		t.Fatal(err)
	}
	instance := h.Config.Host.InstanceID
	want := `{
  "schema_version": 1,
  "host": {
    "instance_id": "` + instance + `",
    "data_dir": "` + dir + `",
    "console_host": "console"
  },
  "network": {
    "permitted": ["portal"],
    "private_backend": "local"
  },
  "system": {
    "keep_versions": 20,
    "disk_quota_bytes": 0,
    "preview_ttl_seconds": 3600,
    "redirect_days": 14
  },
  "portal": {
    "relays": ["https://rly.best"],
    "discovery": false,
    "max_active_relays": 5
  },
  "credentials": {
    "operator_file": "` + credential + `"
  }
}
`
	if got, _ := os.ReadFile(filepath.Join(dir, "config.json")); string(got) != want {
		t.Errorf("config.json =\n%s\nwant\n%s", got, want)
	}
	if b := readBinding(t, dir); b == nil || b.InstanceID != instance || b.MigratedAt == nil || b.ConfigPath != filepath.Join(dir, "config.json") {
		t.Errorf("binding = %+v", b)
	}
	// Effective values equal what serve applied before the upgrade.
	settings, _ := h.Svc.Settings(context.Background())
	for k, v := range map[string]string{core.SetKeepVersions: "20", core.SetUploadMaxBytes: core.Defaults[core.SetUploadMaxBytes],
		core.SetPortalRelays: "https://rly.best", core.SetPortalDiscover: "false", core.SetPortalMaxRelay: "5", core.SetDiskQuotaBytes: "0"} {
		if settings[k] != v {
			t.Errorf("setting %s = %q, want %q", k, settings[k], v)
		}
	}
	if !slices.Equal(h.Config.Network.Permitted, []string{"portal"}) || h.Public == nil || h.tsNet != nil || h.Operator == nil {
		t.Errorf("grants %+v public=%t tailscale=%t operator=%t", h.Config.Network, h.Public != nil, h.tsNet != nil, h.Operator != nil)
	}
	after, err := h.Providers.ExposurePolicy(context.Background())
	if err != nil || after != before {
		t.Errorf("exposure policy token changed by the migration: %s -> %s (%v)", before, after, err)
	}
	// The legacy Private Tailscale choice was recorded (--network local).
	if pending, err := h.Store.LegacyPrivateUpgradePending(context.Background()); err != nil || pending {
		t.Errorf("legacy choice pending=%t %v", pending, err)
	}
	if ok, _ := h.Store.ProviderPermitted(context.Background(), "docs", store.ProviderTailscale); ok {
		t.Error("declined legacy flat kept Tailscale")
	}
	if ok, _ := h.Store.ProviderPermitted(context.Background(), "blog", store.ProviderTailscale); !ok {
		t.Error("per-flat permission was lost")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, provider.FileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("host file not archived: %v", err)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "backups", "*"))
	var dbBackup, fileBackup bool
	for _, b := range backups {
		dbBackup = dbBackup || strings.HasPrefix(filepath.Base(b), "flats.v5.")
		fileBackup = fileBackup || strings.HasPrefix(filepath.Base(b), "network-provider.")
	}
	if !dbBackup || !fileBackup {
		t.Errorf("backups = %q", backups)
	}

	// The same service flags start again; the config is not rewritten.
	sum := hashFile(t, filepath.Join(dir, "config.json"))
	h, err = Start(context.Background(), legacyFlags(t, dir, flags...))
	if err != nil {
		t.Fatal(err)
	}
	if h.Config.Host.InstanceID != instance {
		t.Errorf("instance changed: %s", h.Config.Host.InstanceID)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	// A changed flag is refused instead of overriding config.json.
	_, err = Start(context.Background(), legacyFlags(t, dir, append(flags, "--relays", "https://other.example")...))
	if exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "--relays") {
		t.Fatalf("changed flag: %v", err)
	}
	if hashFile(t, filepath.Join(dir, "config.json")) != sum {
		t.Error("config.json changed")
	}
	// Config mode runs from the file alone.
	o := Options{ConfigPath: filepath.Join(dir, "config.json"), Overrides: legacyFlags(t, dir).Overrides}
	h, err = Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if after, _ := h.Providers.ExposurePolicy(context.Background()); after != before {
		t.Errorf("config mode policy token %s, want %s", after, before)
	}
	h.Close()
}

func TestLegacyUpgradeUndecidableWritesNothing(t *testing.T) {
	dir := legacyDir(t)
	db := hashFile(t, filepath.Join(dir, "flats.db"))
	hostFile := hashFile(t, filepath.Join(dir, provider.FileName))
	_, err := Start(context.Background(), legacyFlags(t, dir))
	if exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "explicit upgrade choice") {
		t.Fatalf("undecidable legacy choice: %v", err)
	}
	if hashFile(t, filepath.Join(dir, "flats.db")) != db || hashFile(t, filepath.Join(dir, provider.FileName)) != hostFile {
		t.Error("files changed")
	}
	for _, name := range []string{"config.json", "backups"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s was written: %v", name, err)
		}
	}
	// The dry run reports the same refusal and still writes nothing.
	if err := ConfigCommand(context.Background(), []string{"migrate", "--data", dir, "--dry-run"}, &bytes.Buffer{}); exitCode(err) != ExitConfig {
		t.Fatalf("dry run: %v", err)
	}
}

// Each step of the migration can be interrupted; the next start resumes
// as the storage design's interruption table says.
func TestLegacyUpgradeResumesAfterInterruption(t *testing.T) {
	flags := []string{"--network", "local", "--console-host", "resumed"}
	fail := func(t *testing.T, at string) {
		t.Helper()
		interrupt = func(step string) error {
			if step == at {
				return errors.New("interrupted at " + step)
			}
			return nil
		}
		t.Cleanup(func() { interrupt = nil })
	}
	resume := func(t *testing.T, dir string, args ...string) *Host {
		t.Helper()
		interrupt = nil
		h, err := Start(context.Background(), legacyFlags(t, dir, args...))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { h.Close() })
		if b := readBinding(t, dir); b == nil || b.InstanceID != h.Config.Host.InstanceID || b.MigratedAt == nil {
			t.Fatalf("binding %+v, instance %s", b, h.Config.Host.InstanceID)
		}
		return h
	}
	t.Run("after database", func(t *testing.T) {
		dir := legacyDir(t)
		fail(t, "db")
		if _, err := Start(context.Background(), legacyFlags(t, dir, flags...)); err == nil {
			t.Fatal("interrupted start succeeded")
		}
		if _, err := os.Stat(filepath.Join(dir, "config.json")); !errors.Is(err, fs.ErrNotExist) || readBinding(t, dir) != nil {
			t.Fatal("wrote past the interruption")
		}
		h := resume(t, dir, flags...)
		if !slices.Equal(h.Config.Network.Permitted, []string{"portal"}) || h.Config.Host.ConsoleHost != "resumed" {
			t.Fatalf("config = %+v %+v", h.Config.Host, h.Config.Network)
		}
	})
	t.Run("after config", func(t *testing.T) {
		dir := legacyDir(t)
		fail(t, "config")
		if _, err := Start(context.Background(), legacyFlags(t, dir, flags...)); err == nil {
			t.Fatal("interrupted start succeeded")
		}
		l, err := config.Load(filepath.Join(dir, "config.json"))
		if err != nil || readBinding(t, dir) != nil {
			t.Fatalf("state after config: %v binding=%v", err, readBinding(t, dir))
		}
		written, _ := l.Doc.Effective(nil)
		// A different flag cannot finish the migration with this config.
		interrupt = nil
		_, err = Start(context.Background(), legacyFlags(t, dir, "--network", "local"))
		if exitCode(err) != ExitConfig || !strings.Contains(err.Error(), `- "console_host": "resumed"`) {
			t.Fatalf("resume with other flags: %v", err)
		}
		if readBinding(t, dir) != nil {
			t.Fatal("bound with other flags")
		}
		h := resume(t, dir, flags...)
		if h.Config.Host.InstanceID != written.Host.InstanceID {
			t.Fatal("resume did not reuse the instance id")
		}
	})
	t.Run("after binding", func(t *testing.T) {
		dir := legacyDir(t)
		fail(t, "bind")
		if _, err := Start(context.Background(), legacyFlags(t, dir, flags...)); err == nil {
			t.Fatal("interrupted start succeeded")
		}
		if readBinding(t, dir) == nil {
			t.Fatal("not bound")
		}
		if _, err := os.Stat(filepath.Join(dir, provider.FileName)); err != nil {
			t.Fatal("host file moved before housekeeping")
		}
		resume(t, dir, flags...)
		if _, err := os.Stat(filepath.Join(dir, provider.FileName)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("housekeeping did not run on the next start")
		}
	})
	t.Run("housekeeping fails", func(t *testing.T) {
		dir := legacyDir(t)
		fail(t, "archive")
		h, err := Start(context.Background(), legacyFlags(t, dir, flags...))
		if err != nil {
			t.Fatalf("housekeeping failure stopped the host: %v", err)
		}
		h.Close()
		if _, err := os.Stat(filepath.Join(dir, provider.FileName)); err != nil {
			t.Fatal("host file moved although housekeeping failed")
		}
		resume(t, dir, flags...)
		if _, err := os.Stat(filepath.Join(dir, provider.FileName)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("housekeeping was not retried")
		}
	})
}

func TestSecretKeyMissingWithSecretsRefused(t *testing.T) {
	dir := legacyDir(t)
	if err := os.Remove(filepath.Join(dir, "secret.key")); err != nil {
		t.Fatal(err)
	}
	_, err := Start(context.Background(), legacyFlags(t, dir, "--network", "local"))
	if exitCode(err) != ExitConfig || !errors.Is(err, core.ErrSecretKeyMissing) {
		t.Fatalf("missing key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "secret.key")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a new secret key was created over existing secrets")
	}
}

// initConfig creates a host with `flats config init` and returns its config.
func initConfig(t *testing.T, dir string, args ...string) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	var out bytes.Buffer
	if err := ConfigCommand(context.Background(), append([]string{"init", "--data", dir}, args...), &out); err != nil {
		t.Fatal(err, out.String())
	}
	return path
}

func configModeOptions(path string) Options {
	return Options{ConfigPath: path, Overrides: map[string]string{"host.management_addr": "127.0.0.1:0", "host.local_addr": "127.0.0.1:0", "host.server_runtime": "false"}}
}

func TestConfigModeRefusals(t *testing.T) {
	ctx := context.Background()
	refused := func(t *testing.T, o Options, want string) {
		t.Helper()
		h, err := Start(ctx, o)
		if err == nil {
			h.Close()
			t.Fatalf("started; want refusal %q", want)
		}
		if exitCode(err) != ExitConfig || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want exit 78 with %q", err, want)
		}
	}
	t.Run("missing config", func(t *testing.T) {
		dir := t.TempDir()
		refused(t, configModeOptions(filepath.Join(dir, "config.json")), "does not exist")
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatal("wrote into the directory")
		}
	})
	t.Run("config without database", func(t *testing.T) {
		dir := t.TempDir()
		doc, _ := config.New(config.NewInstanceID(), dir)
		path := filepath.Join(dir, "config.json")
		if _, err := config.Create(path, doc); err != nil {
			t.Fatal(err)
		}
		refused(t, configModeOptions(path), "config init")
		if _, err := os.Stat(filepath.Join(dir, "flats.db")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("config mode created a database")
		}
	})
	t.Run("unbound legacy database", func(t *testing.T) {
		dir := legacyDir(t)
		doc, _ := config.New(config.NewInstanceID(), dir)
		path := filepath.Join(t.TempDir(), "config.json")
		if _, err := config.Create(path, doc); err != nil {
			t.Fatal(err)
		}
		db := hashFile(t, filepath.Join(dir, "flats.db"))
		refused(t, configModeOptions(path), "config migrate")
		if hashFile(t, filepath.Join(dir, "flats.db")) != db {
			t.Fatal("database changed")
		}
	})
	t.Run("binding mismatch", func(t *testing.T) {
		a, b := t.TempDir(), t.TempDir()
		pathA := initConfig(t, a)
		initConfig(t, b)
		// Point a's config at b's data, as a copied or mistyped path would.
		if _, err := config.NewWriter(pathA).Save(hashFile(t, pathA), func(d *config.Document) error { return d.Set("host.data_dir", b) }); err != nil {
			t.Fatal(err)
		}
		refused(t, configModeOptions(pathA), "rebind")
		// Legacy mode finds the same config and refuses the same way.
		refused(t, Options{DataDir: a, Overrides: configModeOptions("").Overrides}, "rebind")
	})
	t.Run("too new", func(t *testing.T) {
		dir := t.TempDir()
		path := initConfig(t, dir)
		raw, _ := os.ReadFile(path)
		raw = bytes.Replace(raw, []byte(`"schema_version": 1`), []byte(`"schema_version": 2`), 1)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		refused(t, configModeOptions(path), "newer")
		if got, _ := os.ReadFile(path); !bytes.Equal(got, raw) {
			t.Fatal("too-new config was rewritten")
		}
	})
	t.Run("legacy flags", func(t *testing.T) {
		path := initConfig(t, t.TempDir())
		for _, flags := range [][]string{{"--permit", "portal"}, {"--network", "local"}, {"--portal=false"}, {"--relays", "https://rly.best"},
			{"--authkey-file", "/k"}, {"--operator-credential-file", "/c"}, {"--data", "/d"}} {
			o, err := ParseServeFlags(append([]string{"--config", path}, flags...))
			if err != nil {
				t.Fatal(err)
			}
			o.Overrides = configModeOptions("").Overrides
			name, _, _ := strings.Cut(flags[0], "=")
			refused(t, o, name)
		}
		// The four host flags override the file for one run.
		o, err := ParseServeFlags([]string{"--config", path, "--listen", "127.0.0.1:0", "--local-addr", "127.0.0.1:0", "--console-host", "gate", "--runtime=false"})
		if err != nil {
			t.Fatal(err)
		}
		h, err := Start(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		if h.Config.Host.ConsoleHost != "gate" || h.Config.Host.ServerRuntime {
			t.Fatalf("host = %+v", h.Config.Host)
		}
		if raw, _ := os.ReadFile(path); bytes.Contains(raw, []byte("gate")) {
			t.Fatal("a per-run override was stored")
		}
	})
	t.Run("tailscale environment", func(t *testing.T) {
		dir := t.TempDir()
		path := initConfig(t, dir)
		var out bytes.Buffer
		if err := ConfigCommand(ctx, []string{"set", "network.permitted", "tailscale", "--config", path}, &out); err != nil {
			t.Fatal(err)
		}
		for _, name := range tailscaleEnv {
			t.Run(name, func(t *testing.T) {
				t.Setenv(name, "from-the-shell")
				refused(t, configModeOptions(path), name)
				if _, err := os.Stat(filepath.Join(dir, "tsnet")); !errors.Is(err, fs.ErrNotExist) {
					t.Fatal("tsnet started")
				}
			})
		}
	})
}

func TestConfigInitIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := initConfig(t, dir, "--console-host", "console")
	first := hashFile(t, path)
	b := readBinding(t, dir)
	var out bytes.Buffer
	if err := ConfigCommand(context.Background(), []string{"init", "--data", dir, "--console-host", "console"}, &out); err != nil || !strings.Contains(out.String(), "already initialized") {
		t.Fatalf("second init: %v %s", err, out.String())
	}
	if hashFile(t, path) != first || *readBinding(t, dir) != *b {
		t.Fatal("second init changed the host")
	}
	if err := ConfigCommand(context.Background(), []string{"init", "--data", dir, "--console-host", "other"}, &out); exitCode(err) != ExitConfig {
		t.Fatalf("init with other flags: %v", err)
	}
	// Init refuses a data directory that already holds a host.
	legacy := legacyDir(t)
	if err := ConfigCommand(context.Background(), []string{"init", "--data", legacy}, &out); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "config migrate") {
		t.Fatalf("init over a legacy host: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "config.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("init wrote a config over a legacy host")
	}

	// An init interrupted after writing config.json resumes with its id.
	dir = t.TempDir()
	interrupt = func(step string) error {
		if step == "config" {
			return errors.New("interrupted")
		}
		return nil
	}
	err := ConfigCommand(context.Background(), []string{"init", "--data", dir}, &out)
	interrupt = nil
	if err == nil {
		t.Fatal("interrupted init succeeded")
	}
	l, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	written, _ := l.Doc.Effective(nil)
	initConfig(t, dir)
	if b := readBinding(t, dir); b == nil || b.InstanceID != written.Host.InstanceID {
		t.Fatalf("resumed binding %+v, config %s", b, written.Host.InstanceID)
	}
}

// A first legacy start with no config and no database initializes the
// host from its flags, as the tailnet gate script does.
func TestLegacyFreshStartInitializes(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(t.TempDir(), "authkey")
	o := legacyFlags(t, dir, "--listen", "127.0.0.1:27999", "--console-host", "gate-x", "--portal=false", "--authkey-file", key, "--permit", "portal")
	o.Overrides = map[string]string{"host.management_addr": "127.0.0.1:0", "host.local_addr": "127.0.0.1:0"}
	// Stop before networking: only the config is under test here.
	interrupt = func(step string) error {
		if step == "bind" {
			return errors.New("stop")
		}
		return nil
	}
	defer func() { interrupt = nil }()
	if _, err := Start(context.Background(), o); err == nil {
		t.Fatal("expected the injected stop")
	}
	l, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := l.Doc.Effective(nil)
	if c.Host.ManagementAddr != "127.0.0.1:27999" || c.Host.ConsoleHost != "gate-x" || len(c.Network.Permitted) != 0 ||
		c.Credentials.TailscaleAuthKeyFile != key || c.Host.DataDir != dir {
		t.Fatalf("config = %+v %+v %+v", c.Host, c.Network, c.Credentials)
	}
	if b := readBinding(t, dir); b == nil || b.MigratedAt != nil {
		t.Fatalf("binding = %+v", b)
	}
}

func TestSettingsSaveToConfigAndDetectEdits(t *testing.T) {
	h, client := operatorHost(t)
	path := h.ConfigPath
	if code := operatorCall(t, h, client, "PUT", "/console/api/settings", strings.NewReader(`{"keep_versions":"3","portal_relays":"https://rly.best"}`), nil); code != 200 {
		t.Fatalf("PUT settings = %d", code)
	}
	raw, _ := os.ReadFile(path)
	if !bytes.Contains(raw, []byte(`"keep_versions": 3`)) || !bytes.Contains(raw, []byte(`"relays": ["https://rly.best"]`)) {
		t.Fatalf("config.json =\n%s", raw)
	}
	if hist, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "config-history", "config.v1.*.json")); len(hist) != 1 {
		t.Fatalf("history = %q", hist)
	}
	if h.Svc.UploadLimit() != 20<<20 {
		t.Fatal("upload limit changed")
	}
	// An out-of-range value is invalid, not a server error.
	if code := operatorCall(t, h, client, "PUT", "/console/api/settings", strings.NewReader(`{"keep_versions":"9007199254740992"}`), nil); code != 400 {
		t.Fatalf("out of range = %d", code)
	}
	// An edit on disk while the host runs makes console saves conflict.
	edited := bytes.Replace(raw, []byte(`"keep_versions": 3`), []byte(`"keep_versions": 4`), 1)
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if code := operatorCall(t, h, client, "PUT", "/console/api/settings", strings.NewReader(`{"keep_versions":"5"}`), &out); code != 409 || !strings.Contains(out["error"].(string), "restart") {
		t.Fatalf("conflicting save = %d %v", code, out)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, edited) {
		t.Fatal("conflicting save overwrote the edit")
	}
	if _, err := h.Svc.UpdateSettings(context.Background(), map[string]string{core.SetKeepVersions: "6"}); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("direct save after edit: %v", err)
	}
}

// A legacy service started with --relays compares the flag with the config
// on every start, so the console cannot change the relays under it. Other
// settings, and the same relays, still save. Config mode has no such pin.
func TestConsoleCannotChangeFlagPinnedRelays(t *testing.T) {
	dir := t.TempDir()
	h, client := operatorHostWith(t, dir, func(o *Options) {
		o.Relays, o.Set = []string{"https://rly.best"}, map[string]bool{"relays": true}
	})
	put := func(body string) (int, map[string]any) {
		t.Helper()
		var out map[string]any
		code := operatorCall(t, h, client, "PUT", "/console/api/settings", strings.NewReader(body), &out)
		return code, out
	}
	path := h.ConfigPath
	before := hashFile(t, path)
	code, out := put(`{"portal_relays":"https://other.example"}`)
	if code != 409 || out["category"] != "config_overridden" || !strings.Contains(fmt.Sprint(out["error"]), "--relays flag; reinstall the service with `flats install`") {
		t.Fatalf("pinned relays = %d %v", code, out)
	}
	if hashFile(t, path) != before {
		t.Fatal("rejected change was saved")
	}
	if code, out := put(`{"portal_relays":"https://rly.best/","keep_versions":"4"}`); code != 200 {
		t.Fatalf("unchanged relays with another setting = %d %v", code, out)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	// The same flags still start.
	o := localOptions(dir)
	o.Relays, o.Set = []string{"https://rly.best"}, map[string]bool{"relays": true}
	h2, err := Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	h2.Close()

	h3, err := Start(context.Background(), configModeOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	defer h3.Close()
	if _, err := h3.Svc.UpdateSettings(context.Background(), map[string]string{core.SetPortalRelays: "https://other.example"}); err != nil {
		t.Fatalf("config mode: %v", err)
	}
}

// With no Portal settings stored, the release before config.json read the
// defaults table (3 active relays, discovery on). The migrated host must
// produce the same exposure policy token, or pending approvals go stale.
func TestLegacyUpgradeKeepsDefaultPortalPolicyToken(t *testing.T) {
	dir := t.TempDir()
	ddl, err := os.ReadFile(filepath.Join("..", "store", "testdata", "schema", "v5-80b206b.sql"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "flats.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM settings`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	file := provider.File{Version: 1, Permitted: []provider.ID{provider.Portal}, PrivateBackend: "local"}
	if err := provider.Save(dir, file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.key"), bytes.Repeat([]byte{7}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	before := legacyPolicyToken(t, file, portal.Config{Discovery: true, MaxActiveRelays: 3})
	if legacyPolicyToken(t, file, portal.Config{Discovery: true}) == before {
		t.Fatal("the token does not depend on the relay limit, so this test cannot tell 0 from 3")
	}

	h, err := Start(context.Background(), legacyFlags(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	after, err := h.Providers.ExposurePolicy(context.Background())
	if err != nil || after != before {
		t.Errorf("default Portal policy token changed by the migration: %s -> %s (%v)", before, after, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "config.json")); strings.Contains(string(got), `"portal": {`) {
		t.Errorf("defaults were written into config.json:\n%s", got)
	}
}
