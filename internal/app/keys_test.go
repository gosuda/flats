package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

const (
	keyA = "tskey-auth-aaaaaaaaaaaa-AAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	keyB = "tskey-auth-bbbbbbbbbbbb-BBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	keyC = "tskey-auth-cccccccccccc-CCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
)

func writeKey(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func clearKeyEnv(t *testing.T) {
	t.Helper()
	for _, e := range keyEnv {
		t.Setenv(e.value, "")
		t.Setenv(e.file, "")
	}
	for _, n := range legacyTailscaleEnv {
		t.Setenv(n, "")
	}
}

func TestReadKeyInputsPriority(t *testing.T) {
	flagFile := writeKey(t, keyA)
	envFile := writeKey(t, keyB)
	cfgFile := writeKey(t, keyC)
	set := func(names ...string) map[string]bool {
		m := map[string]bool{}
		for _, n := range names {
			m[n] = true
		}
		return m
	}
	tests := []struct {
		name       string
		opts       Options
		env        map[string]string
		legacy     bool
		cfg        string
		want, from string
	}{
		{"flag wins", Options{TailscaleKeyFile: flagFile, Set: set("tailscale-authkey-file")}, map[string]string{"FLATS_TAILSCALE_AUTHKEY": keyB}, false, cfgFile, keyA, "--tailscale-authkey-file"},
		{"legacy flag", Options{AuthKeyFile: flagFile, Set: set("authkey-file")}, nil, true, "", keyA, "--authkey-file"},
		{"env value", Options{}, map[string]string{"FLATS_TAILSCALE_AUTHKEY": keyB}, false, cfgFile, keyB, "FLATS_TAILSCALE_AUTHKEY"},
		{"env file", Options{}, map[string]string{"FLATS_TAILSCALE_AUTHKEY_FILE": envFile}, false, "", keyB, "FLATS_TAILSCALE_AUTHKEY_FILE"},
		{"legacy TS_AUTHKEY", Options{}, map[string]string{"TS_AUTHKEY": keyB}, true, "", keyB, "TS_AUTHKEY"},
		{"TS_AUTHKEY ignored in config mode", Options{}, map[string]string{"TS_AUTHKEY": keyB}, false, "", "", ""},
		{"deprecated config key", Options{}, nil, false, cfgFile, keyC, "credentials.tailscale_authkey_file"},
		{"nothing", Options{}, nil, false, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearKeyEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			in, err := readKeyInputs(tt.opts, tt.legacy, tt.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if in.tailscale.value != tt.want || in.tailscale.from != tt.from {
				t.Fatalf("tailscale = %q from %q", in.tailscale.value, in.tailscale.from)
			}
			// Flats' variables are gone after reading; so are tsnet's in legacy mode.
			for _, e := range keyEnv {
				if os.Getenv(e.value) != "" || os.Getenv(e.file) != "" {
					t.Fatalf("%s left in the environment", e.value)
				}
			}
			if tt.legacy && os.Getenv("TS_AUTHKEY") != "" {
				t.Fatal("TS_AUTHKEY left in the environment")
			}
		})
	}
}

func TestReadKeyInputsRefusals(t *testing.T) {
	good := writeKey(t, keyA)
	empty := writeKey(t, "")
	client := writeKey(t, "tskey-client-xxxx")
	missing := filepath.Join(t.TempDir(), "missing")
	tests := []struct {
		name string
		opts Options
		env  map[string]string
		cfg  string
	}{
		{"both env forms", Options{}, map[string]string{"FLATS_TAILSCALE_AUTHKEY": keyA, "FLATS_TAILSCALE_AUTHKEY_FILE": good}, ""},
		{"missing flag file", Options{TailscaleKeyFile: missing, Set: map[string]bool{"tailscale-authkey-file": true}}, nil, ""},
		{"empty file", Options{TailscaleKeyFile: empty, Set: map[string]bool{"tailscale-authkey-file": true}}, nil, ""},
		{"oauth client secret", Options{}, map[string]string{"FLATS_TAILSCALE_AUTHKEY_FILE": client}, ""},
		// A broken lower-priority input still stops the start.
		{"unreadable deprecated file under a flag", Options{TailscaleKeyFile: good, Set: map[string]bool{"tailscale-authkey-file": true}}, nil, missing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearKeyEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			if _, err := readKeyInputs(tt.opts, false, tt.cfg); exitCode(err) != ExitConfig {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func openKeyStore(t *testing.T) (string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return dir, st
}

// External values seed flats.db; a stored key always wins.
func TestResolveTailscaleKeySeeds(t *testing.T) {
	dir, st := openKeyStore(t)
	ctx := context.Background()
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	key, differs, err := resolveTailscaleKey(ctx, dir, st, keyInput{}, time.Now(), logf)
	if err != nil || key != "" || differs {
		t.Fatalf("empty: %q %v %v", key, differs, err)
	}
	key, differs, err = resolveTailscaleKey(ctx, dir, st, keyInput{keyA, core.KeySourceEnv, "FLATS_TAILSCALE_AUTHKEY"}, time.Now(), logf)
	if err != nil || key != keyA || differs {
		t.Fatalf("seed: %q %v %v", key, differs, err)
	}
	if k, _ := st.GetHostKey(ctx, core.HostKeyTailscale); k.Source != core.KeySourceEnv {
		t.Fatalf("source = %q", k.Source)
	}
	// The console replaced it; the unchanged env value no longer wins.
	if err := core.PutHostKeyValue(ctx, dir, st, core.HostKeyTailscale, keyB, core.KeySourceConsole, time.Now()); err != nil {
		t.Fatal(err)
	}
	key, differs, err = resolveTailscaleKey(ctx, dir, st, keyInput{keyA, core.KeySourceEnv, "FLATS_TAILSCALE_AUTHKEY"}, time.Now(), logf)
	if err != nil || key != keyB || !differs {
		t.Fatalf("stored wins: %q %v %v", key, differs, err)
	}
	if k, _ := st.GetHostKey(ctx, core.HostKeyTailscale); k.Source != core.KeySourceConsole {
		t.Fatalf("the external value overwrote the stored key: %q", k.Source)
	}
	key, differs, _ = resolveTailscaleKey(ctx, dir, st, keyInput{keyB, core.KeySourceFlag, "--tailscale-authkey-file"}, time.Now(), logf)
	if key != keyB || differs {
		t.Fatalf("same value: %q %v", key, differs)
	}
	if len(logs) == 0 {
		t.Fatal("nothing was logged")
	}
	for _, l := range logs {
		for _, k := range []string{keyA, keyB} {
			if strings.Contains(l, k) {
				t.Fatalf("a key reached the log: %q", l)
			}
		}
	}
}

func TestConsoleKeyAPI(t *testing.T) {
	h, client := consoleHost(t)
	var out struct{ Keys []KeyInfo }
	if code := consoleCall(t, h, client, "GET", "/console/api/keys", nil, &out); code != 200 || len(out.Keys) != 3 || out.Keys[0].ID != "tailscale" || out.Keys[0].Set {
		t.Fatalf("keys: %d %+v", code, out.Keys)
	}
	if code := consoleCall(t, h, client, "PUT", "/console/api/keys/tailscale", strings.NewReader(`{"key": "tskey-api-nope"}`), nil); code != 400 {
		t.Fatalf("an API token was accepted: %d", code)
	}
	if code := consoleCall(t, h, client, "PUT", "/console/api/keys/tailscale", strings.NewReader(`{"key": "`+keyA+`"}`), &out); code != 200 || !out.Keys[0].Set || out.Keys[0].Source != "console" {
		t.Fatalf("set: %d %+v", code, out.Keys)
	}
	if v, err := h.currentTailscaleKey(context.Background()); err != nil || v != keyA {
		t.Fatalf("stored = %q %v", v, err)
	}
	// The response never carries the value.
	raw := consoleRaw(t, h, client, "/console/api/keys")
	if strings.Contains(raw, "tskey") {
		t.Fatalf("a key value was returned: %s", raw)
	}
	if code := consoleCall(t, h, client, "DELETE", "/console/api/keys/tailscale", nil, &out); code != 200 || out.Keys[0].Set {
		t.Fatalf("delete: %d %+v", code, out.Keys)
	}
	// Agents and the CLI cannot reach these routes.
	for _, path := range []string{"/console/api/keys", "/api/keys", "/console/api/console-domains", "/api/console-domains"} {
		if code := call(t, "GET", "http://"+h.Addr()+path, nil, nil); code == 200 {
			t.Errorf("GET %s without the console headers: %d", path, code)
		}
	}
}

func consoleRaw(t *testing.T, h *Host, client *http.Client, path string) string {
	t.Helper()
	base := "http://" + h.Addr()
	req, _ := http.NewRequest("GET", base+path, nil)
	req.Header.Set("X-Flats-Console", "1")
	req.Header.Set("Origin", base)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestKeyCommandOffline(t *testing.T) {
	dir := t.TempDir()
	path := initConfig(t, dir)
	run := func(stdin string, args ...string) (string, error) {
		var out bytes.Buffer
		err := KeyCommand(context.Background(), append(args, "--config", path), strings.NewReader(stdin), &out)
		return out.String(), err
	}
	if _, err := run("", "set", "tailscale"); exitCode(err) != 2 {
		t.Fatalf("empty stdin: %v", err)
	}
	if _, err := run("tskey-client-x", "set", "tailscale"); exitCode(err) != ExitConfig {
		t.Fatalf("client secret: %v", err)
	}
	if _, err := run(keyA+"\n", "set", "tailscale"); err != nil {
		t.Fatal(err)
	}
	if _, err := run("zrok-token", "set", "zrok"); err != nil {
		t.Fatal(err)
	}
	out, err := run("", "ls")
	if err != nil || !strings.Contains(out, "tailscale  stored (from cli") || !strings.Contains(out, "zrok       waiting") || strings.Contains(out, "tskey") {
		t.Fatalf("ls: %q %v", out, err)
	}
	if _, err := run("", "set", "portal"); exitCode(err) != 2 {
		t.Fatalf("unknown key: %v", err)
	}
	if _, err := run("", "rm", "zrok"); err != nil {
		t.Fatal(err)
	}

	// The host stores nothing new and uses the key from flats.db.
	h, err := Start(context.Background(), configModeOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	v, err := h.currentTailscaleKey(context.Background())
	if err != nil || v != keyA {
		t.Fatalf("host key = %q %v", v, err)
	}
	// While the host runs, offline commands refuse.
	if _, err := run("", "ls"); exitCode(err) != ExitConfig {
		t.Fatalf("while running: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := run("", "rm", "tailscale"); err != nil {
		t.Fatal(err)
	}
	out, _ = run("", "ls")
	if !strings.Contains(out, "tailscale  none") {
		t.Fatalf("after rm: %q", out)
	}
}

// A config with the deprecated key file path: the first start stores the key
// in flats.db and removes the path.
func TestDeprecatedAuthKeyFileImported(t *testing.T) {
	clearKeyEnv(t)
	dir := t.TempDir()
	path := initConfig(t, dir)
	keyFile := writeKey(t, keyA)
	if _, err := config.NewWriter(path).Save(hashFile(t, path), func(d *config.Document) error {
		return d.SetDeprecated("credentials.tailscale_authkey_file", keyFile)
	}); err != nil {
		t.Fatal(err)
	}
	h, err := Start(context.Background(), configModeOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	k, err := h.Store.GetHostKey(context.Background(), core.HostKeyTailscale)
	if err != nil || k.Source != core.KeySourceConfig {
		t.Fatalf("stored = %+v %v", k, err)
	}
	if raw := mustRead(t, path); bytes.Contains(raw, []byte("tailscale_authkey_file")) {
		t.Fatalf("path kept:\n%s", raw)
	}
	if _, err := os.Stat(keyFile); err != nil {
		t.Fatalf("the key file was touched: %v", err)
	}
}

func TestDeprecatedAuthKeyFileUnreadable(t *testing.T) {
	clearKeyEnv(t)
	dir := t.TempDir()
	path := initConfig(t, dir)
	if _, err := config.NewWriter(path).Save(hashFile(t, path), func(d *config.Document) error {
		return d.SetDeprecated("credentials.tailscale_authkey_file", filepath.Join(dir, "missing"))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(context.Background(), configModeOptions(path)); exitCode(err) != ExitConfig {
		t.Fatalf("unreadable key file: %v", err)
	}
	if raw := mustRead(t, path); !bytes.Contains(raw, []byte("tailscale_authkey_file")) {
		t.Fatal("the path was removed although its key was not stored")
	}
}

// A missing secret.key with stored keys stops the start; `flats key rm`
// without secret.key is the way out.
func TestSecretKeyMissingWithHostKeys(t *testing.T) {
	clearKeyEnv(t)
	dir := t.TempDir()
	path := initConfig(t, dir)
	var out bytes.Buffer
	if err := KeyCommand(context.Background(), []string{"set", "tailscale", "--config", path}, strings.NewReader(keyA), &out); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "secret.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(context.Background(), configModeOptions(path)); exitCode(err) != ExitConfig || !errors.Is(err, core.ErrSecretKeyMissing) {
		t.Fatalf("start: %v", err)
	}
	if err := KeyCommand(context.Background(), []string{"rm", "tailscale", "--config", path}, strings.NewReader(""), io.Discard); err != nil {
		t.Fatal(err)
	}
	h, err := Start(context.Background(), configModeOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
}

// storedTailscaleKey reads the Tailscale key flats.db in dir holds.
func storedTailscaleKey(t *testing.T, dir string) string {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	v, _, err := core.StoredHostKey(context.Background(), dir, st, core.HostKeyTailscale)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
