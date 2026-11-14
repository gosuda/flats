package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/ostest"
)

// docs/spec/secret-key-path.md: host.secret_key_path keeps the host key outside the data directory.

// keyOutside moves the key of the stopped host in dir out of the data directory and sets host.secret_key_path to
// its new place, as the docs describe. It returns the new path.
func keyOutside(t *testing.T, dir, config string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "flats.key")
	if err := os.Rename(filepath.Join(dir, core.SecretKeyFile), path); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := ConfigCommand(context.Background(), []string{"set", "host.secret_key_path", path, "--config", config}, &out); err != nil {
		t.Fatal(err, out.String())
	}
	return path
}

// listed returns the names in a folder, sorted, or nil when the folder is not there.
func listed(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return out
}

// noTemporaryKeyFile fails the test when a synced temporary key file is left in any of the folders.
func noTemporaryKeyFile(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		for _, n := range listed(t, d) {
			if strings.HasPrefix(n, ".secret-key-") {
				t.Errorf("%s holds the temporary key file %s", d, n)
			}
		}
	}
}

func TestHostStartsWithAConfiguredKeyAndCreatesNothing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	config, key, h := hostWithSecrets(t, dir)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	keyPath := keyOutside(t, dir, config)
	keyDir := filepath.Dir(keyPath)
	h2, err := Start(ctx, configModeOptions(config))
	if err != nil {
		t.Fatalf("the host does not start with the key outside the data directory: %v", err)
	}
	defer h2.Close()
	if check, err := core.CheckKey(ctx, h2.Store, key); err != nil || !check.All() || check.Total != 2 {
		t.Errorf("the key opens %+v, %v; want the flat secret and the operator key", check, err)
	}
	if v, _, err := core.StoredHostKey(ctx, h2.keyFile(), h2.Store, core.HostKeyZrok); err != nil || v != "zrok-token" {
		t.Errorf("the stored operator key = %q, %v", v, err)
	}
	if !h2.keyFile().Configured() || h2.keyFile().Path() != keyPath {
		t.Errorf("the host's key file is %+v", h2.keyFile())
	}
	if _, err := os.Lstat(filepath.Join(dir, core.SecretKeyFile)); !os.IsNotExist(err) {
		t.Errorf("a key was created in the data directory: %v", err)
	}
	if got := listed(t, keyDir); !slices.Equal(got, []string{"flats.key"}) {
		t.Errorf("the key folder holds %v", got)
	}
	noTemporaryKeyFile(t, dir, keyDir)
}

// Every way that the configured key can be unusable stops the start with status 78, names the path and the
// setting, and creates nothing (rules 5 to 7).
func TestStartRefusesAnUnusableConfiguredKey(t *testing.T) {
	cases := map[string]func(t *testing.T, dir, keyPath string, key []byte){
		"missing": func(t *testing.T, dir, keyPath string, key []byte) { os.Remove(keyPath) },
		"a directory": func(t *testing.T, dir, keyPath string, key []byte) {
			os.Remove(keyPath)
			if err := os.Mkdir(keyPath, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"31 bytes": func(t *testing.T, dir, keyPath string, key []byte) { os.WriteFile(keyPath, key[:31], 0o600) },
		"33 bytes": func(t *testing.T, dir, keyPath string, key []byte) {
			os.WriteFile(keyPath, append(slices.Clone(key), 0), 0o600)
		},
		"a link to nothing": func(t *testing.T, dir, keyPath string, key []byte) {
			os.Remove(keyPath)
			if err := ostest.Symlink(t, filepath.Join(filepath.Dir(keyPath), "not-mounted"), keyPath); err != nil {
				t.Skipf("no symbolic links here: %v", err)
			}
		},
		"a second key in the data directory": func(t *testing.T, dir, keyPath string, key []byte) {
			if err := os.WriteFile(filepath.Join(dir, core.SecretKeyFile), key, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"a link into the data directory": func(t *testing.T, dir, keyPath string, key []byte) {
			inner := filepath.Join(dir, "somewhere", "k")
			if err := os.MkdirAll(filepath.Dir(inner), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(inner, key, 0o600); err != nil {
				t.Fatal(err)
			}
			os.Remove(keyPath)
			if err := ostest.Symlink(t, inner, keyPath); err != nil {
				t.Skipf("no symbolic links here: %v", err)
			}
		},
		"unreadable": func(t *testing.T, dir, keyPath string, key []byte) {
			if ostest.Windows || os.Geteuid() == 0 {
				t.Skip("needs Unix modes and a user that is not the superuser")
			}
			if err := os.Chmod(keyPath, 0); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			config, key, h := hostWithSecrets(t, dir)
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
			keyPath := keyOutside(t, dir, config)
			breakIt(t, dir, keyPath, key)
			keyDir := filepath.Dir(keyPath)
			beforeKeys := listed(t, keyDir)
			h2, err := Start(context.Background(), configModeOptions(config))
			if err == nil {
				h2.Close()
				t.Fatal("the host started")
			}
			if exitCode(err) != ExitConfig {
				t.Errorf("exit status %d (%v), want %d", exitCode(err), err, ExitConfig)
			}
			if !strings.Contains(err.Error(), keyPath) && !strings.Contains(err.Error(), filepath.Join(dir, core.SecretKeyFile)) {
				t.Errorf("the message names neither the key path nor the second key: %v", err)
			}
			if !strings.Contains(err.Error(), core.KeySetting) {
				t.Errorf("the message does not name the setting: %v", err)
			}
			if got := listed(t, keyDir); !slices.Equal(got, beforeKeys) {
				t.Errorf("the key folder changed: %v -> %v", beforeKeys, got)
			}
			noTemporaryKeyFile(t, dir, keyDir)
		})
	}
}

// The refusal holds when flats.db holds no sealed value, and the host never creates the key (rule 5).
func TestStartWithAConfiguredMissingKeyOfAFreshHostCreatesNoKey(t *testing.T) {
	dir := t.TempDir()
	config := initConfig(t, dir)
	keyPath := filepath.Join(t.TempDir(), "secret.key")
	var out bytes.Buffer
	if err := ConfigCommand(context.Background(), []string{"set", "host.secret_key_path", keyPath, "--config", config}, &out); err != nil {
		t.Fatal(err, out.String())
	}
	h, err := Start(context.Background(), configModeOptions(config))
	if err == nil {
		h.Close()
		t.Fatal("the host started and made a key")
	}
	if exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "never creates a key") {
		t.Errorf("err = %v (status %d)", err, exitCode(err))
	}
	if got := listed(t, filepath.Dir(keyPath)); len(got) != 0 {
		t.Errorf("the key folder holds %v", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, core.SecretKeyFile)); !os.IsNotExist(err) {
		t.Errorf("a key was created in the data directory: %v", err)
	}
}

// `flats key set` with the setting and no file refuses and creates nothing (rule 9). `key ls` and `key rm` touch no key.
func TestKeySetWithAConfiguredMissingKeyRefuses(t *testing.T) {
	clearKeyEnv(t)
	dir := t.TempDir()
	config := initConfig(t, dir)
	keyPath := filepath.Join(t.TempDir(), "secret.key")
	if err := ConfigCommand(context.Background(), []string{"set", "host.secret_key_path", keyPath, "--config", config}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := KeyCommand(context.Background(), []string{"set", "tailscale", "--config", config}, strings.NewReader(keyA), &out)
	if err == nil || exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "never creates a key") {
		t.Fatalf("key set: err = %v (status %d)", err, exitCode(err))
	}
	if got := listed(t, filepath.Dir(keyPath)); len(got) != 0 {
		t.Errorf("the key folder holds %v", got)
	}
	for _, args := range [][]string{{"ls"}, {"rm", "tailscale"}} {
		if err := KeyCommand(context.Background(), append(args, "--config", config), strings.NewReader(""), &out); err != nil {
			t.Errorf("key %s with a missing configured key: %v", args[0], err)
		}
	}
}

// Operator keys read the key file at each use (rule 8): a replaced file changes what they open, and a second key
// that appears in the data directory makes them refuse.
func TestOperatorKeysReadTheConfiguredFileAtEachUse(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	config, key, h := hostWithSecrets(t, dir)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	keyPath := keyOutside(t, dir, config)
	h2, err := Start(ctx, configModeOptions(config))
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	open := func() (string, error) {
		v, _, err := core.StoredHostKey(ctx, h2.keyFile(), h2.Store, core.HostKeyZrok)
		return v, err
	}
	if v, err := open(); err != nil || v != "zrok-token" {
		t.Fatalf("open = %q, %v", v, err)
	}
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{5}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := open(); err == nil {
		t.Error("a replaced key file still opened the operator key")
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := open(); err != nil || v != "zrok-token" {
		t.Fatalf("open after the key came back = %q, %v", v, err)
	}
	if err := os.WriteFile(filepath.Join(dir, core.SecretKeyFile), key, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := open(); err == nil || !strings.Contains(err.Error(), "move that file away") {
		t.Errorf("a second key in the data directory: err = %v, want a refusal", err)
	}
	// Sealing a new operator key goes through the same location.
	if err := core.PutHostKeyValue(ctx, h2.keyFile(), h2.Store, core.HostKeyTailscale, keyA, core.KeySourceConsole, time.Now()); err == nil {
		t.Error("an operator key was sealed while two keys existed")
	}
}

// The check of the key comes before any listener opens (rule 5). With both addresses already taken, a start that
// reached a listener would fail with a listen error and not with the refusal of the key.
func TestStartRefusesAKeyBeforeAnyListenerOpens(t *testing.T) {
	dir := t.TempDir()
	config := initConfig(t, dir)
	keyPath := filepath.Join(t.TempDir(), "secret.key")
	if err := ConfigCommand(context.Background(), []string{"set", "host.secret_key_path", keyPath, "--config", config}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var taken []string
	for range 2 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		taken = append(taken, ln.Addr().String())
	}
	h, err := Start(context.Background(), Options{ConfigPath: config, Overrides: map[string]string{
		"host.console_addr": taken[0], "host.local_addr": taken[1], "host.server_runtime": "false"}})
	if err == nil {
		h.Close()
		t.Fatal("the host started")
	}
	if exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "never creates a key") || strings.Contains(err.Error(), "listen") {
		t.Errorf("err = %v (status %d), want the refusal of the key and no listen error", err, exitCode(err))
	}
}

// A key on a read-only mount works (rule 7): a folder without write permission, and a key with mode 0444.
func TestHostStartsWithAKeyInAReadOnlyFolder(t *testing.T) {
	if ostest.Windows || os.Geteuid() == 0 {
		t.Skip("needs Unix modes and a user that is not the superuser")
	}
	dir := t.TempDir()
	config, key, h := hostWithSecrets(t, dir)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	keyPath := keyOutside(t, dir, config)
	keyDir := filepath.Dir(keyPath)
	if err := os.Chmod(keyPath, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(keyDir, 0o700) })
	h2, err := Start(context.Background(), configModeOptions(config))
	if err != nil {
		t.Fatalf("the host does not start with a key in a read-only folder: %v", err)
	}
	defer h2.Close()
	if check, err := core.CheckKey(context.Background(), h2.Store, key); err != nil || !check.All() {
		t.Errorf("the key opens %+v, %v", check, err)
	}
	if got := listed(t, keyDir); !slices.Equal(got, []string{"flats.key"}) {
		t.Errorf("the key folder holds %v", got)
	}
}

// flats config show lists the setting (rule 1).
func TestConfigShowListsTheKeyPath(t *testing.T) {
	dir := t.TempDir()
	config := initConfig(t, dir)
	keyPath := filepath.Join(t.TempDir(), "secret.key")
	var out bytes.Buffer
	if err := ConfigCommand(context.Background(), []string{"set", "host.secret_key_path", keyPath, "--config", config}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := ConfigCommand(context.Background(), []string{"show", "--config", config}, &out); err != nil {
		t.Fatal(err)
	}
	jsonPath := strings.ReplaceAll(keyPath, `\`, `\\`) // config show prints JSON strings
	if !strings.Contains(out.String(), "host.secret_key_path") || !strings.Contains(out.String(), jsonPath) {
		t.Errorf("config show does not list the key path:\n%s", out.String())
	}
}

// Every refusal of the key exits with status 78, also the one that core.New makes after the check at the start
// (rules 5 and 8).
func TestKeyRefusalIsAnActionError(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		"a configured key that cannot be used": fmt.Errorf("%w: nothing is there", core.ErrKeyLocation),
		"a missing key over sealed values":     fmt.Errorf("%w: restore it", core.ErrSecretKeyMissing),
	} {
		if got := keyRefusal(err); exitCode(got) != ExitConfig || !errors.Is(got, err) {
			t.Errorf("%s: exit status %d (%v), want %d", name, exitCode(got), got, ExitConfig)
		}
	}
	other := errors.New("disk full")
	if got := keyRefusal(other); got != other {
		t.Errorf("another error changed: %v", got)
	}
}

// The console's methods for the operator keys seal and open under the configured file, through the key location of
// the host, and never create a key in the data directory (rules 8 and 9).
func TestConsoleOperatorKeyMethodsUseTheConfiguredFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	config, key, h := hostWithSecrets(t, dir)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	keyPath := keyOutside(t, dir, config)
	h2, err := Start(ctx, configModeOptions(config))
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	if err := h2.SetTailscaleKey(ctx, keyA); err != nil {
		t.Fatalf("SetTailscaleKey: %v", err)
	}
	if v, err := h2.currentTailscaleKey(ctx); err != nil || v != keyA {
		t.Errorf("currentTailscaleKey = %q, %v; want the key that the console stored", v, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, core.SecretKeyFile)); !os.IsNotExist(err) {
		t.Errorf("a console method created a key in the data directory: %v", err)
	}
	// The key opens only with the configured file: with another key there, the stored value does not open.
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{9}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h2.currentTailscaleKey(ctx); err == nil {
		t.Error("a replaced key file still opened the stored Tailscale key")
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := h2.currentTailscaleKey(ctx); err != nil || v != keyA {
		t.Errorf("after the key came back: %q, %v", v, err)
	}
}
