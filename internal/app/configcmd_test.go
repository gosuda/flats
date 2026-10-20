package app

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/expose/provider"
)

func runConfig(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := ConfigCommand(context.Background(), args, &out)
	return out.String(), err
}

func TestConfigShowValidateSetUnset(t *testing.T) {
	dir := t.TempDir()
	path := initConfig(t, dir)
	out, err := runConfig(t, "show", "--config", path)
	if err != nil || !strings.Contains(out, `"schema_version": 5`) || !strings.Contains(out, `system.version_quota               20 (default)`) ||
		!strings.Contains(out, `host.data_dir                      "`+dir+`" (file)`) {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if out, err := runConfig(t, "validate", "--data", dir); err != nil || !strings.Contains(out, "is valid") {
		t.Fatalf("validate: %v %s", err, out)
	}
	if _, err := runConfig(t, "set", "system.version_quota", "3", "--config", path); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); !bytes.Contains(raw, []byte(`"version_quota": 3`)) {
		t.Fatalf("set did not write:\n%s", raw)
	}
	if _, err := runConfig(t, "set", "--config", path, "--", "system.version_quota", "-1"); exitCode(err) != ExitConfig {
		t.Fatalf("invalid value: %v", err)
	}
	if _, err := runConfig(t, "set", "host.instance_id", config.NewInstanceID(), "--config", path); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "rebind") {
		t.Fatalf("instance id: %v", err)
	}
	if _, err := runConfig(t, "unset", "system.version_quota", "--config", path); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); bytes.Contains(raw, []byte(`version_quota`)) {
		t.Fatalf("unset did not remove the key:\n%s", raw)
	}
	if _, err := runConfig(t, "set", "system.version_quota", "--config", path); exitCode(err) != 2 {
		t.Fatalf("missing value: %v", err)
	}

	// Offline commands refuse while a host holds the data lock.
	h, err := Start(context.Background(), configModeOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runConfig(t, "set", "system.version_quota", "4", "--config", path); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "stop the running") {
		t.Fatalf("set while running: %v", err)
	}
	// show and validate only read.
	if _, err := runConfig(t, "show", "--config", path); err != nil {
		t.Fatal(err)
	}
	h.Close()

	raw, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(raw[:len(raw)-2], []byte(`, "extra": {}}`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runConfig(t, "validate", "--config", path); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "extra") {
		t.Fatalf("validate of an invalid file: %v", err)
	}
}

func TestConfigSetDataDirRequiresTheSameInstance(t *testing.T) {
	old := t.TempDir()
	path := initConfig(t, old)
	moved := filepath.Join(t.TempDir(), "moved")
	if _, err := runConfig(t, "set", "host.data_dir", moved, "--config", path); exitCode(err) != ExitConfig {
		t.Fatalf("missing directory: %v", err)
	}
	other := t.TempDir()
	initConfig(t, other)
	if _, err := runConfig(t, "set", "host.data_dir", other, "--config", path); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "does not hold") {
		t.Fatalf("another host's data: %v", err)
	}
	// Moving the data itself is accepted.
	if err := os.Rename(old, moved); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(moved, "config.json")
	if _, err := runConfig(t, "set", "host.data_dir", moved, "--config", path); err != nil {
		t.Fatal(err)
	}
	h, err := Start(context.Background(), configModeOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
}

func TestConfigRebind(t *testing.T) {
	dir := t.TempDir()
	path := initConfig(t, dir)
	// A copy of the whole directory is the same instance until rebound.
	copyDir := t.TempDir()
	for _, name := range []string{"config.json", "flats.db"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(copyDir, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	copyPath := filepath.Join(copyDir, "config.json")
	if _, err := runConfig(t, "set", "host.data_dir", copyDir, "--config", copyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runConfig(t, "rebind", "--config", copyPath); exitCode(err) != 2 {
		t.Fatalf("rebind without --yes: %v", err)
	}
	if _, err := runConfig(t, "rebind", "--yes", "--config", copyPath); err != nil {
		t.Fatal(err)
	}
	l, _ := config.Load(copyPath)
	c, _ := l.Doc.Effective(nil)
	if b := readBinding(t, copyDir); b == nil || b.InstanceID != c.Host.InstanceID || b.InstanceID == readBinding(t, dir).InstanceID {
		t.Fatalf("binding %+v, config %s", b, c.Host.InstanceID)
	}
	for _, p := range []string{path, copyPath} {
		h, err := Start(context.Background(), configModeOptions(p))
		if err != nil {
			t.Fatal(p, err)
		}
		h.Close()
	}
}

func TestConfigMigrateCommand(t *testing.T) {
	dir := legacyDir(t)
	db := hashFile(t, filepath.Join(dir, "flats.db"))
	out, err := runConfig(t, "migrate", "--data", dir, "--dry-run", "--", "--network", "local", "--relays", "https://flag.example")
	if err != nil || !strings.Contains(out, `"relays": ["https://flag.example"]`) || !strings.Contains(out, "nothing was written") ||
		!strings.Contains(out, "lots") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if hashFile(t, filepath.Join(dir, "flats.db")) != db {
		t.Fatal("dry run changed the database")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() == "config.json" || e.Name() == "backups" || e.Name() == "flats.lock" {
			t.Fatalf("dry run wrote %s", e.Name())
		}
	}
	if out, err = runConfig(t, "migrate", "--data", dir, "--", "--network", "local"); err != nil || !strings.Contains(out, "migrated the legacy host") {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	if b := readBinding(t, dir); b == nil || b.MigratedAt == nil {
		t.Fatalf("binding %+v", b)
	}
	if _, err := os.Stat(filepath.Join(dir, provider.FileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("host file not archived")
	}
	// Running it again is a no-op; the dry run says so too.
	if out, err = runConfig(t, "migrate", "--data", dir, "--dry-run"); err != nil || !strings.Contains(out, "already bound") {
		t.Fatalf("second dry run: %v\n%s", err, out)
	}
	if _, err = runConfig(t, "migrate", "--data", t.TempDir()); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "nothing to migrate") {
		t.Fatalf("migrate an empty directory: %v", err)
	}
}

func TestEnsureConfigForInstall(t *testing.T) {
	var out bytes.Buffer
	ctx := context.Background()
	// A new host: init from the documented install.sh flags.
	dir := filepath.Join(t.TempDir(), "data")
	credential := filepath.Join(t.TempDir(), "operator")
	path, data, err := EnsureConfig(ctx, "", dir, []string{"--operator-credential-file", credential}, &out)
	if err != nil || path != filepath.Join(dir, "config.json") || data != dir {
		t.Fatalf("new host: %q %q %v", path, data, err)
	}
	l, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := l.Doc.Effective(nil); c.Credentials.OperatorFile != credential {
		t.Fatalf("credentials = %+v", c.Credentials)
	}
	// Reinstalling with the same flags, or none, keeps the config.
	sum := hashFile(t, path)
	for _, args := range [][]string{{"--operator-credential-file", credential}, nil} {
		if _, _, err := EnsureConfig(ctx, "", dir, args, &out); err != nil {
			t.Fatal(args, err)
		}
	}
	if hashFile(t, path) != sum {
		t.Fatal("reinstall rewrote the config")
	}
	// Different flags are refused.
	if _, _, err := EnsureConfig(ctx, "", dir, []string{"--permit", "portal"}, &out); exitCode(err) != ExitConfig {
		t.Fatalf("different flags: %v", err)
	}
	// Key flags the installed service would lose are refused, with the way to
	// store the key instead.
	for _, args := range [][]string{{"--tailscale-authkey-file", "/k"}, {"--authkey-file", "/k"}, {"--zrok-token-file", "/k"}, {"--netbird-setup-key-file", "/k"}} {
		if _, _, err := EnsureConfig(ctx, "", dir, args, &out); exitCode(err) != 2 || !strings.Contains(err.Error(), "flats key set") {
			t.Errorf("%q on an existing host: %v", args, err)
		}
	}
	// A new host stores the key of a Tailscale key file in flats.db; the
	// config holds no key and no path.
	fresh := filepath.Join(t.TempDir(), "data")
	if _, _, err := EnsureConfig(ctx, "", fresh, []string{"--tailscale-authkey-file", writeKey(t, keyB)}, &out); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(fresh, "config.json")); strings.Contains(string(raw), "tailscale_authkey_file") {
		t.Fatalf("config.json = %s", raw)
	}
	if got := storedTailscaleKey(t, fresh); got != keyB {
		t.Fatalf("stored key = %q", got)
	}
	// An unreadable key file stops the install before anything is written.
	broken := filepath.Join(t.TempDir(), "data")
	if _, _, err := EnsureConfig(ctx, "", broken, []string{"--authkey-file", filepath.Join(t.TempDir(), "missing")}, &out); exitCode(err) != ExitConfig {
		t.Fatalf("unreadable key file: %v", err)
	}
	// A legacy data directory is migrated, to a config path of choice.
	legacy := legacyDir(t)
	custom := filepath.Join(t.TempDir(), "etc", "flats.json")
	path, data, err = EnsureConfig(ctx, custom, legacy, []string{"--network", "local"}, &out)
	if err != nil || path != custom || data != legacy {
		t.Fatalf("legacy host: %q %q %v", path, data, err)
	}
	if b := readBinding(t, legacy); b == nil || b.ConfigPath != custom || b.MigratedAt == nil {
		t.Fatalf("binding %+v", b)
	}
	// A running host blocks a change that needs writes.
	running := t.TempDir()
	h, err := Start(ctx, localOptions(running))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, _, err := EnsureConfig(ctx, "", running, nil, &out); err != nil {
		t.Fatalf("matching install while running: %v", err)
	}
	if _, _, err := EnsureConfig(ctx, "", running, []string{"--permit", "portal"}, &out); exitCode(err) != ExitConfig {
		t.Fatalf("changing install while running: %v", err)
	}
	if _, _, err := EnsureConfig(ctx, "", dir, []string{"--config", path}, &out); exitCode(err) != 2 {
		t.Fatalf("--config as a serve flag: %v", err)
	}
}

func TestConsoleAddressConfigCLI(t *testing.T) {
	path := initConfig(t, t.TempDir())
	if _, err := runConfig(t, "set", "host.console_addr", "127.0.0.1:0", "--config", path); err != nil {
		t.Fatal(err)
	}
	out, err := runConfig(t, "show", "--config", path)
	if err != nil || !strings.Contains(out, "host.console_addr") || strings.Contains(out, "host.management_addr") {
		t.Fatalf("show: %v\n%s", err, out)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"set", "host.management_addr", "127.0.0.1:9999"}, {"unset", "host.management_addr"}} {
		if _, err := runConfig(t, append(args, "--config", path)...); exitCode(err) != ExitConfig {
			t.Fatalf("old key accepted: %q %v", args, err)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("old key changed config: %v", err)
		}
	}
	opts := configModeOptions(path)
	delete(opts.Overrides, "host.console_addr")
	h, err := Start(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if h.Config.Host.ConsoleAddr != "127.0.0.1:0" {
		t.Errorf("console address = %q", h.Config.Host.ConsoleAddr)
	}
	if _, src, _ := h.Config.Lookup("host.console_addr"); src != config.SourceFile {
		t.Errorf("source = %s", src)
	}
	for _, route := range []string{"/api/status", "/llms.txt", "/"} {
		if code, _ := fetch(t, "http://"+h.Addr()+route); code != 200 {
			t.Errorf("%s = %d", route, code)
		}
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := runConfig(t, "unset", "host.console_addr", "--config", path); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loaded.Doc.Effective(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host.ConsoleAddr != "127.0.0.1:7878" {
		t.Fatalf("unset default: %+v", cfg.Host)
	}
}
