package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/ostest"
)

const recoveryPassword = "a long recovery password\n"

// stoppedHostWithAKeyOutside starts a host with a flat secret and an operator key, stops it, and moves its key out
// of the data directory. It returns the data directory, the configuration, the key bytes and the key path.
func stoppedHostWithAKeyOutside(t *testing.T) (dir, config string, key []byte, keyPath string) {
	t.Helper()
	dir = t.TempDir()
	var h *Host
	config, key, h = hostWithSecrets(t, dir)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, config, key, keyOutside(t, dir, config)
}

func mustRunSecretKey(t *testing.T, config string, args ...string) string {
	t.Helper()
	out, err := runSecretKey(t, config, args...)
	if err != nil {
		t.Fatalf("flats secret %s: %v\n%s", args[0], err, out)
	}
	return out
}

func TestBackupKeyReadsTheConfiguredKey(t *testing.T) {
	dir, config, key, keyPath := stoppedHostWithAKeyOutside(t)
	mustRunSecretKey(t, config, "backup-key", "--password-file", passwordFile(t, recoveryPassword))
	copyBytes, err := os.ReadFile(filepath.Join(dir, core.RecoveryKeyFile))
	if err != nil {
		t.Fatalf("the recovery copy is not in the data directory: %v", err)
	}
	if got, err := core.OpenKeyCopy(copyBytes, strings.TrimSpace(recoveryPassword)); err != nil || !bytes.Equal(got, key) {
		t.Errorf("the recovery copy opens to %x, %v; want the configured key", got, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, core.SecretKeyFile)); !os.IsNotExist(err) {
		t.Errorf("backup-key put a key in the data directory: %v", err)
	}
	// A missing configured key is a refusal that names the path and the setting.
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	_, err = runSecretKey(t, config, "backup-key", "--password-file", passwordFile(t, "another long password\n"))
	if err == nil || exitCode(err) != ExitConfig || !strings.Contains(err.Error(), keyPath) || !strings.Contains(err.Error(), core.KeySetting) {
		t.Errorf("backup-key without the key: %v (status %d)", err, exitCode(err))
	}
}

func TestRestoreKeyWritesTheConfiguredPath(t *testing.T) {
	dir, config, key, keyPath := stoppedHostWithAKeyOutside(t)
	mustRunSecretKey(t, config, "backup-key", "--password-file", passwordFile(t, recoveryPassword))
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	keyDir := filepath.Dir(keyPath)
	out := mustRunSecretKey(t, config, "restore-key", "--password-file", passwordFile(t, recoveryPassword))
	if !strings.Contains(out, keyPath) {
		t.Errorf("the output does not name the restored path: %s", out)
	}
	if got, err := os.ReadFile(keyPath); err != nil || !bytes.Equal(got, key) {
		t.Errorf("the restored key = %x, %v", got, err)
	}
	if got := listed(t, keyDir); !slices.Equal(got, []string{"flats.key"}) {
		t.Errorf("the key folder holds %v", got)
	}
	noTemporaryKeyFile(t, dir, keyDir)
	if _, err := os.Lstat(filepath.Join(dir, core.SecretKeyFile)); !os.IsNotExist(err) {
		t.Errorf("restore-key wrote into the data directory: %v", err)
	}
	// It never replaces a file.
	_, err := runSecretKey(t, config, "restore-key", "--password-file", passwordFile(t, recoveryPassword))
	if err == nil || !strings.Contains(err.Error(), "never replaces") {
		t.Errorf("a second restore-key: %v, want a refusal", err)
	}
	h, err := Start(context.Background(), configModeOptions(config))
	if err != nil {
		t.Fatalf("the host does not start with the restored key: %v", err)
	}
	h.Close()
}

// --out writes the recovered key to a new file and changes nothing at the location, which may hold a wrong key
// (rule 13).
func TestRestoreKeyOutWritesANewFile(t *testing.T) {
	dir, config, key, keyPath := stoppedHostWithAKeyOutside(t)
	mustRunSecretKey(t, config, "backup-key", "--password-file", passwordFile(t, recoveryPassword))
	wrong := bytes.Repeat([]byte{3}, 32)
	if err := os.WriteFile(keyPath, wrong, 0o600); err != nil {
		t.Fatal(err)
	}
	pw := passwordFile(t, recoveryPassword)
	outFile := filepath.Join(t.TempDir(), "recovered.key")
	out := mustRunSecretKey(t, config, "restore-key", "--out", outFile, "--password-file", pw)
	if !strings.Contains(out, outFile) || !strings.Contains(out, keyPath) {
		t.Errorf("the output names neither the file nor the key path: %s", out)
	}
	if got, err := os.ReadFile(outFile); err != nil || !bytes.Equal(got, key) {
		t.Errorf("--out file = %x, %v; want the recovered key", got, err)
	}
	if !ostest.Windows {
		if fi, err := os.Stat(outFile); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("--out file mode = %v, %v; want 0600", fi.Mode(), err)
		}
	}
	if got, err := os.ReadFile(keyPath); err != nil || !bytes.Equal(got, wrong) {
		t.Errorf("--out changed the location: %x, %v", got, err)
	}
	// It never replaces a file, and it refuses a place inside the data directory.
	if _, err := runSecretKey(t, config, "restore-key", "--out", outFile, "--password-file", pw); err == nil {
		t.Error("--out replaced a file")
	}
	inside := filepath.Join(dir, "recovered.key")
	_, err := runSecretKey(t, config, "restore-key", "--out", inside, "--password-file", pw)
	if err == nil || exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "outside the data directory") {
		t.Errorf("--out inside the data directory: %v (status %d)", err, exitCode(err))
	}
	if _, err := os.Lstat(inside); !os.IsNotExist(err) {
		t.Errorf("--out wrote inside the data directory: %v", err)
	}
	// A second key in the data directory is a refusal with or without --out (rule 3).
	if err := os.WriteFile(filepath.Join(dir, core.SecretKeyFile), key, 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other.key")
	if _, err := runSecretKey(t, config, "restore-key", "--out", other, "--password-file", pw); err == nil || !strings.Contains(err.Error(), "move that file away") {
		t.Errorf("--out with a second key in the data directory: %v", err)
	}
	if _, err := os.Lstat(other); !os.IsNotExist(err) {
		t.Errorf("--out wrote a file next to a refusal: %v", err)
	}
}

func TestRestoreKeyInAReadOnlyLocationNamesOut(t *testing.T) {
	if ostest.Windows || os.Geteuid() == 0 {
		t.Skip("needs Unix modes and a user that is not the superuser")
	}
	dir, config, _, keyPath := stoppedHostWithAKeyOutside(t)
	mustRunSecretKey(t, config, "backup-key", "--password-file", passwordFile(t, recoveryPassword))
	keyDir := filepath.Dir(keyPath)
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(keyDir, 0o700) })
	_, err := runSecretKey(t, config, "restore-key", "--password-file", passwordFile(t, recoveryPassword))
	if err == nil || !strings.Contains(err.Error(), "--out") || !strings.Contains(err.Error(), "nothing was written") {
		t.Errorf("restore-key into a read-only folder: %v", err)
	}
	os.Chmod(keyDir, 0o700)
	if got := listed(t, keyDir); len(got) != 0 {
		t.Errorf("the key folder holds %v", got)
	}
	noTemporaryKeyFile(t, dir, keyDir)
}

func TestResetWithAConfiguredKey(t *testing.T) {
	ctx := context.Background()
	dir, config, _, keyPath := stoppedHostWithAKeyOutside(t)
	// The configured key opens every value, so nothing is lost and reset refuses.
	_, err := runSecretKey(t, config, "reset", "--yes")
	if err == nil || exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "opens all") || !strings.Contains(err.Error(), keyPath) {
		t.Fatalf("reset with the key in place: %v (status %d)", err, exitCode(err))
	}
	// With the key gone, reset deletes the values and says what the next start needs.
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	out := mustRunSecretKey(t, config, "reset", "--yes")
	for _, want := range []string{"deleted 2 values", "The next start needs a key at " + keyPath, "never creates a key at a configured path"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Lstat(keyPath); !os.IsNotExist(err) {
		t.Errorf("reset created the key: %v", err)
	}
	if h, err := Start(ctx, configModeOptions(config)); err == nil {
		h.Close()
		t.Fatal("the host started without a key")
	} else if exitCode(err) != ExitConfig {
		t.Errorf("start without the key: %v (status %d)", err, exitCode(err))
	}
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{4}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := Start(ctx, configModeOptions(config))
	if err != nil {
		t.Fatalf("the host does not start with a new key: %v", err)
	}
	defer h.Close()
	if check, err := core.CheckKey(ctx, h.Store, bytes.Repeat([]byte{4}, 32)); err != nil || check.Total != 0 {
		t.Errorf("after the reset the host holds %+v, %v; want no sealed value", check, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, core.SecretKeyFile)); !os.IsNotExist(err) {
		t.Errorf("a key appeared in the data directory: %v", err)
	}
}

// flats backup records an external key, leaves it out and scans for it; --verify reports the note (rules 16 to 20).
func TestBackupCommandWithAConfiguredKey(t *testing.T) {
	ctx := context.Background()
	dir, config, key, keyPath := stoppedHostWithAKeyOutside(t)
	out := filepath.Join(t.TempDir(), "copy")
	text, err := runBackup(t, "--out", out, "--config", config)
	if err != nil {
		t.Fatalf("backup: %v\n%s", err, text)
	}
	if !strings.Contains(text, "the secret key is kept at "+keyPath) || !strings.Contains(text, "back it up apart from the copy") {
		t.Errorf("the output does not say where the key is kept:\n%s", text)
	}
	if _, err := os.Lstat(filepath.Join(out, "data", core.SecretKeyFile)); !os.IsNotExist(err) {
		t.Errorf("the copy holds a key file: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil || !strings.Contains(string(manifest), `"secret_key": "external"`) {
		t.Errorf("manifest = %s, %v; want secret_key external", manifest, err)
	}
	filepath.WalkDir(out, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, rerr := os.ReadFile(p); rerr == nil && bytes.Contains(b, key) {
				t.Errorf("%s holds the bytes of the key", p)
			}
		}
		return nil
	})
	verify, err := runBackup(t, "--verify", out)
	if err != nil || !strings.Contains(verify, "the key is outside the copy") || !strings.Contains(verify, "ok: ") {
		t.Errorf("verify: %v\n%s", err, verify)
	}
	// A restore needs the key put back at the configured path. Without it, the host refuses; with it, it opens every value.
	restoreFrom(t, dir, out)
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if h, err := Start(ctx, configModeOptions(config)); err == nil {
		h.Close()
		t.Fatal("the restored host started without the key")
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := Start(ctx, configModeOptions(config))
	if err != nil {
		t.Fatalf("the restored host does not start with the key put back: %v", err)
	}
	defer h.Close()
	if check, err := core.CheckKey(ctx, h.Store, key); err != nil || !check.All() || check.Total != 2 {
		t.Errorf("the key opens %+v, %v", check, err)
	}
}

func TestBackupCommandRefusesASecondKeyAndSkipsAnUnreadableOne(t *testing.T) {
	dir, config, key, keyPath := stoppedHostWithAKeyOutside(t)
	if err := os.WriteFile(filepath.Join(dir, core.SecretKeyFile), key, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "copy")
	_, err := runBackup(t, "--out", out, "--config", config)
	if err == nil || exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "move that file away") {
		t.Fatalf("backup with a second key: %v (status %d)", err, exitCode(err))
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("a refused backup left %s: %v", out, err)
	}
	if err := os.Remove(filepath.Join(dir, core.SecretKeyFile)); err != nil {
		t.Fatal(err)
	}
	// A missing key refuses the backup and names the flag. With the flag the backup does not read the key.
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	_, err = runBackup(t, "--out", out, "--config", config)
	if err == nil || exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "--exclude-secret-key") {
		t.Fatalf("backup without the key: %v (status %d)", err, exitCode(err))
	}
	if got := listed(t, filepath.Dir(out)); len(got) != 0 {
		t.Errorf("a refused backup made %v", got)
	}
	text, err := runBackup(t, "--out", out, "--exclude-secret-key", "--config", config)
	if err != nil || !strings.Contains(text, "the secret key is kept at") {
		t.Errorf("backup with --exclude-secret-key and no key: %v\n%s", err, text)
	}
}
