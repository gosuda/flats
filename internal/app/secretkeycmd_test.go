package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/ostest"
	"github.com/gosuda/flats/internal/store"
)

// defaultWorkFactor is read before init lowers the factor for the tests.
var defaultWorkFactor = recoveryWorkFactor

func init() { recoveryWorkFactor = 10 }

func TestRecoveryCopiesUseAgesDefault(t *testing.T) {
	if defaultWorkFactor != core.RecoveryWorkFactor || core.RecoveryWorkFactor != 18 {
		t.Fatalf("work factor %d, core %d", defaultWorkFactor, core.RecoveryWorkFactor)
	}
}

// scripted answers a prompt with the next line of its script.
type scripted struct{ answers []string }

func (s *scripted) next() (string, error) {
	if len(s.answers) == 0 {
		return "", io.EOF
	}
	a := s.answers[0]
	s.answers = s.answers[1:]
	return a, nil
}
func (s *scripted) hidden(string) (string, error) { return s.next() }
func (s *scripted) line(string) (string, error)   { return s.next() }

// onTerminal makes the commands see a terminal that gives these answers.
func onTerminal(t *testing.T, answers ...string) {
	t.Helper()
	old := openTerminal
	openTerminal = func(context.Context, io.Reader, io.Writer) prompter { return &scripted{answers} }
	t.Cleanup(func() { openTerminal = old })
}

func runSecretKey(t *testing.T, config string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := SecretKeyCommand(context.Background(), append(args, "--config", config), strings.NewReader(""), &out)
	return out.String(), err
}

func passwordFile(t *testing.T, password string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte(password), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// hostWithSecrets starts a host in dir with a flat secret and a stored
// operator key, and returns its config path and key bytes.
func hostWithSecrets(t *testing.T, dir string) (string, []byte, *Host) {
	t.Helper()
	ctx := context.Background()
	path := initConfig(t, dir)
	h, err := Start(ctx, configModeOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Svc.SaveVersion(ctx, "app", []bundle.File{{Path: "index.html", Data: []byte("x")}}, core.SaveMeta{}, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := h.Svc.SetSecret(ctx, "app", "API_KEY", "hunter2-value", core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	if err := core.PutHostKeyValue(ctx, dir, h.Store, core.HostKeyZrok, "zrok-token", core.KeySourceCLI, time.Now()); err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(filepath.Join(dir, core.SecretKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	return path, key, h
}

func TestSecretKeyBackupAndRestore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path, key, h := hostWithSecrets(t, dir)
	keyPath := filepath.Join(dir, core.SecretKeyFile)
	copyPath := filepath.Join(dir, core.RecoveryKeyFile)
	pw := passwordFile(t, "a long recovery password\n")

	// backup-key works beside the running host and replaces its own copy.
	if out, err := runSecretKey(t, path, "backup-key", "--password-file", pw); err != nil || !strings.Contains(out, "wrote "+copyPath) {
		t.Fatalf("backup: %q %v", out, err)
	}
	for _, p := range []string{copyPath, copyPath + ".id"} {
		if fi, err := os.Stat(p); err != nil || ostest.WrongMode(fi, 0o600) {
			t.Fatalf("mode of %s: %v %v", p, fi, err)
		}
	}
	if w := core.RecoveryCopyWarning(dir, key); w != "" {
		t.Fatalf("a fresh copy warns: %s", w)
	}
	if out, err := runSecretKey(t, path, "backup-key", "--password-file", pw); err != nil || !strings.Contains(out, "replaced "+copyPath) {
		t.Fatalf("second backup: %q %v", out, err)
	}
	data, _ := os.ReadFile(copyPath)
	outside := filepath.Join(t.TempDir(), "key.age")
	if _, err := runSecretKey(t, path, "backup-key", "--password-file", pw, "--out", outside); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(outside); err != nil || ostest.WrongMode(fi, 0o600) {
		t.Fatalf("--out mode: %v %v", fi, err)
	}
	data, _ = os.ReadFile(copyPath)
	if got, _ := os.ReadFile(outside); !bytes.Equal(got, data) {
		t.Fatal("--out holds other bytes than the data directory's copy")
	}
	// An existing --out is refused before anything is written.
	if _, err := runSecretKey(t, path, "backup-key", "--password-file", pw, "--out", outside); exitCode(err) != ExitConfig {
		t.Fatalf("existing --out: %v", err)
	}
	if got, _ := os.ReadFile(copyPath); !bytes.Equal(got, data) {
		t.Fatal("a refused backup changed the copy")
	}
	// So is an --out whose folder is missing, is a file, or is the data
	// directory, where the copy and its ID live.
	for _, bad := range []string{filepath.Join(t.TempDir(), "missing", "key.age"), filepath.Join(outside, "key.age"), filepath.Join(dir, "key.age")} {
		if _, err := runSecretKey(t, path, "backup-key", "--password-file", pw, "--out", bad); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "--out") {
			t.Fatalf("--out %s: %v", bad, err)
		}
		if got, _ := os.ReadFile(copyPath); !bytes.Equal(got, data) {
			t.Fatalf("--out %s: a refused backup changed the copy", bad)
		}
	}

	// restore-key refuses while the host runs, also with nothing at
	// secret.key, where only the lock stops it.
	if err := os.Rename(keyPath, keyPath+".held"); err != nil {
		t.Fatal(err)
	}
	if _, err := runSecretKey(t, path, "restore-key", "--password-file", pw); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "stop the running Flats host") {
		t.Fatalf("restore while running: %v", err)
	}
	if _, err := os.Lstat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("restore wrote a key beside a running host")
	}
	if err := os.Rename(keyPath+".held", keyPath); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if h, err := Start(ctx, configModeOptions(path)); !errors.Is(err, core.ErrSecretKeyMissing) {
		if h != nil {
			h.Close()
		}
		t.Fatalf("start without the key: %v", err)
	}

	// No password source is a usage error, and a directory at secret.key is
	// something there.
	if _, err := runSecretKey(t, path, "restore-key"); exitCode(err) != 2 {
		t.Fatalf("no terminal and no password file: %v", err)
	}
	if err := os.Mkdir(keyPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runSecretKey(t, path, "restore-key", "--password-file", pw); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "never replaces a key") {
		t.Fatalf("restore over a directory: %v", err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}

	// A wrong password and another host's copy write nothing.
	if _, err := runSecretKey(t, path, "restore-key", "--password-file", passwordFile(t, "not the password")); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), core.ErrRecoveryPassword.Error()) {
		t.Fatalf("wrong password: %v", err)
	}
	otherDir := t.TempDir()
	otherPath, _, other := hostWithSecrets(t, otherDir)
	other.Close()
	if _, err := runSecretKey(t, otherPath, "backup-key", "--password-file", pw); err != nil {
		t.Fatal(err)
	}
	out, err := runSecretKey(t, path, "restore-key", "--password-file", pw, "--from", filepath.Join(otherDir, core.RecoveryKeyFile))
	if exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "opens 0 of the 2 sealed values") {
		t.Fatalf("another host's copy: %q %v", out, err)
	}
	if _, err := os.Lstat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused restore wrote %s", keyPath)
	}

	// A file over 64 KiB is refused by its size before any password is
	// asked: without a terminal or a password file, a later check would be a
	// usage error instead.
	big := filepath.Join(t.TempDir(), "big.age")
	if err := os.WriteFile(big, make([]byte, core.MaxRecoveryFileSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runSecretKey(t, path, "restore-key", "--from", big); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized file: %v", err)
	}
	if _, err := runSecretKey(t, path, "restore-key", "--password-file", passwordFile(t, strings.Repeat("x", maxPasswordBytes+1))); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("long password: %v", err)
	}
	if out, err := runSecretKey(t, path, "restore-key", "--password-file", pw); err != nil || !strings.Contains(out, "restored "+keyPath) {
		t.Fatalf("restore: %q %v", out, err)
	}
	if got, _ := os.ReadFile(keyPath); !bytes.Equal(got, key) {
		t.Fatal("the restored key differs from the original")
	}
	if fi, err := os.Stat(keyPath); err != nil || ostest.WrongMode(fi, 0o600) {
		t.Fatalf("key mode: %v %v", fi, err)
	}
	if _, err := runSecretKey(t, path, "restore-key", "--password-file", pw); exitCode(err) != ExitConfig {
		t.Fatalf("restore over a key: %v", err)
	}

	h, err = Start(ctx, configModeOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if c, err := core.CheckKey(ctx, h.Store, key); err != nil || !c.All() || c.Total != 2 {
		t.Fatalf("after restore: %+v %v", c, err)
	}
}

func TestSecretKeyPasswordRules(t *testing.T) {
	dir := t.TempDir()
	path, _, h := hostWithSecrets(t, dir)
	defer h.Close()
	copyPath := filepath.Join(dir, core.RecoveryKeyFile)

	if _, err := runSecretKey(t, path, "backup-key"); exitCode(err) != 2 {
		t.Fatalf("no terminal and no file: %v", err)
	}
	if _, err := runSecretKey(t, path, "backup-key", "--password-file", passwordFile(t, "short pass\n")); exitCode(err) != ExitConfig {
		t.Fatalf("short password: %v", err)
	}
	if _, err := runSecretKey(t, path, "backup-key", "--password-file", passwordFile(t, strings.Repeat("x", maxPasswordBytes+1))); exitCode(err) != ExitConfig {
		t.Fatalf("long password: %v", err)
	}
	if _, err := runSecretKey(t, path, "backup-key", "--password-file", passwordFile(t, strings.Repeat("x", 4*maxPasswordBytes+1))); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("password file over 4096 bytes: %v", err)
	}
	onTerminal(t, "first long password", "second long password")
	if _, err := runSecretKey(t, path, "backup-key"); exitCode(err) != ExitConfig {
		t.Fatalf("passwords differ: %v", err)
	}
	if _, err := os.Lstat(copyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused backup wrote the copy")
	}
	onTerminal(t, "same long password", "same long password")
	if _, err := runSecretKey(t, path, "backup-key"); err != nil {
		t.Fatal(err)
	}
	// Twelve characters, not bytes.
	onTerminal(t, "ééééééééééé", "ééééééééééé")
	if _, err := runSecretKey(t, path, "backup-key"); exitCode(err) != ExitConfig {
		t.Fatalf("11 characters in 22 bytes: %v", err)
	}
	// The password file loses only its trailing line ends.
	data, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if k, err := core.OpenKeyCopy(data, "same long password"); err != nil || len(k) != 32 {
		t.Fatalf("terminal password: %v", err)
	}
	if _, err := runSecretKey(t, path, "backup-key", "--password-file", passwordFile(t, " spaced  password \r\n\n")); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(copyPath)
	if _, err := core.OpenKeyCopy(data, " spaced  password "); err != nil {
		t.Fatalf("file password: %v", err)
	}

	// Without secret.key there is nothing to back up, and something at the
	// path that is no key must be moved away.
	h.Close()
	keyPath := filepath.Join(dir, core.SecretKeyFile)
	os.Remove(keyPath)
	good := passwordFile(t, "a long recovery password")
	if _, err := runSecretKey(t, path, "backup-key", "--password-file", good); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "start the host once") {
		t.Fatalf("no key: %v", err)
	}
	os.WriteFile(keyPath, []byte("short"), 0o600)
	if _, err := runSecretKey(t, path, "backup-key", "--password-file", good); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "must be 32 bytes") {
		t.Fatalf("short key: %v", err)
	}
}

func TestSecretReset(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path, key, h := hostWithSecrets(t, dir)
	// Two secrets of one flat: each gets its own event.
	if err := h.Svc.SetSecret(ctx, "app", "OTHER_KEY", "other-value", core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	// A connected account keeps a record without a value.
	if err := h.Store.PutHostKey(ctx, store.HostKey{ID: core.HostKeyNetbird, Source: core.KeySourceCLI, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// reset refuses while the host runs, also with nothing at secret.key,
	// where only the lock stops it.
	keyPath := filepath.Join(dir, core.SecretKeyFile)
	if err := os.Rename(keyPath, keyPath+".held"); err != nil {
		t.Fatal(err)
	}
	if _, err := runSecretKey(t, path, "reset", "--yes"); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "stop the running Flats host") {
		t.Fatalf("reset while running: %v", err)
	}
	if names, err := h.Svc.SecretNames(ctx, "app"); err != nil || len(names) != 2 {
		t.Fatalf("a refused reset deleted secrets: %v %v", names, err)
	}
	if err := os.Rename(keyPath+".held", keyPath); err != nil {
		t.Fatal(err)
	}
	h.Close()
	stored := func() int {
		t.Helper()
		st, err := store.Open(filepath.Join(dir, "flats.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		secs, _ := st.ListAllSecrets(ctx)
		records, _ := st.ListHostKeys(ctx)
		n := len(secs)
		for _, r := range records {
			if r.HasValue() {
				n++
			}
		}
		return n
	}

	// The key opens everything: nothing is lost, and reset refuses.
	if _, err := runSecretKey(t, path, "reset", "--yes"); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "nothing is lost") {
		t.Fatalf("reset with a working key: %v", err)
	}
	// The key opens some values: reset names the others and refuses.
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	bogus := store.SealedSecret{Name: "BOGUS", Nonce: make([]byte, 12), Ciphertext: make([]byte, 20), UpdatedAt: time.Now()}
	if err := st.PutSecret(ctx, "app", bogus); err != nil {
		t.Fatal(err)
	}
	st.Close()
	_, err = runSecretKey(t, path, "reset", "--yes")
	if exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "opens 3 of the 4") || !strings.Contains(err.Error(), "remove secret BOGUS of app with `flats secret rm`") {
		t.Fatalf("reset with a partial key: %v", err)
	}
	if st, err = store.Open(filepath.Join(dir, "flats.db")); err != nil {
		t.Fatal(err)
	}
	// An operator key that does not open comes first, while stopped.
	if err := st.PutHostKey(ctx, store.HostKey{ID: core.HostKeyTailscale, Nonce: make([]byte, 12), Ciphertext: make([]byte, 20), Source: core.KeySourceCLI, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	_, err = runSecretKey(t, path, "reset", "--yes")
	if msg := fmt.Sprint(err); exitCode(err) != ExitConfig || !strings.Contains(msg, "first, with the host stopped, the operator keys tailscale with `flats key rm`") ||
		strings.Index(msg, "flats key rm") > strings.Index(msg, "then start the host and remove secret BOGUS of app") {
		t.Fatalf("reset with an operator key that does not open: %v", err)
	}
	if st, err = store.Open(filepath.Join(dir, "flats.db")); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSecret(ctx, "app", "BOGUS"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteHostKey(ctx, core.HostKeyTailscale); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// A key that cannot be read stops reset with that error, status 1, also
	// through a link.
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		unreadable := func(file string) {
			t.Helper()
			if err := os.Chmod(file, 0); err != nil {
				t.Fatal(err)
			}
			_, err := runSecretKey(t, path, "reset", "--yes")
			if err := os.Chmod(file, 0o600); err != nil {
				t.Fatal(err)
			}
			if exitCode(err) != 1 || !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("reset over an unreadable key at %s: %v", file, err)
			}
		}
		unreadable(keyPath)
		target := keyPath + ".target"
		if err := os.Rename(keyPath, target); err != nil {
			t.Fatal(err)
		}
		if err := ostest.Symlink(t, target, keyPath); err != nil {
			t.Fatal(err)
		}
		unreadable(target)
		if err := os.Remove(keyPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(target, keyPath); err != nil {
			t.Fatal(err)
		}
	}

	// Something at secret.key that is no key must be moved away first.
	if err := os.Rename(keyPath, keyPath+".bak"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runSecretKey(t, path, "reset", "--yes"); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "move "+keyPath+" away") {
		t.Fatalf("reset over a broken key: %v", err)
	}
	os.Remove(keyPath)
	if n := stored(); n != 3 {
		t.Fatalf("a refused reset deleted values: %d left", n)
	}

	// Nothing at secret.key: reset asks, and deletes only when told.
	if _, err := runSecretKey(t, path, "reset"); exitCode(err) != 2 {
		t.Fatalf("no terminal and no --yes: %v", err)
	}
	onTerminal(t, "yes")
	if out, err := runSecretKey(t, path, "reset"); err != nil || !strings.Contains(out, "Nothing was deleted") {
		t.Fatalf("declined: %q %v", out, err)
	}
	if n := stored(); n != 3 {
		t.Fatalf("a declined reset deleted values: %d left", n)
	}
	onTerminal(t, "reset\n")
	out, err := runSecretKey(t, path, "reset")
	if err != nil || !strings.Contains(out, "secret API_KEY of app") || !strings.Contains(out, "secret OTHER_KEY of app") || !strings.Contains(out, "operator key zrok") ||
		strings.Contains(out, "hunter2") || !strings.Contains(out, "deleted 3 values") || !strings.Contains(out, "The next start creates a new") {
		t.Fatalf("reset: %q %v", out, err)
	}
	// With no recovery copy, the output claims none.
	if !strings.Contains(out, "to keep a recovery copy of the new key") || strings.Contains(out, "holds the lost key") {
		t.Fatalf("reset without a copy: %q", out)
	}
	if out, err := runSecretKey(t, path, "reset", "--yes"); err != nil || !strings.Contains(out, "nothing to reset") {
		t.Fatalf("second reset: %q %v", out, err)
	}

	// The record without a value stays. (A start drops that NetBird row
	// itself, so look before starting.)
	if st, err = store.Open(filepath.Join(dir, "flats.db")); err != nil {
		t.Fatal(err)
	}
	records, err := st.ListHostKeys(ctx)
	st.Close()
	if err != nil || len(records) != 1 || records[0].ID != core.HostKeyNetbird {
		t.Fatalf("key records after reset: %+v %v", records, err)
	}

	h, err = Start(ctx, configModeOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if names, err := h.Svc.SecretNames(ctx, "app"); err != nil || len(names) != 0 {
		t.Fatalf("secrets after reset: %v %v", names, err)
	}
	if fresh, err := os.ReadFile(keyPath); err != nil || bytes.Equal(fresh, key) {
		t.Fatalf("the start made no new key: %v", err)
	}
	events, err := h.Store.ListEvents(ctx, "app", "secret", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int{}
	for _, e := range events {
		for _, name := range []string{"API_KEY", "OTHER_KEY"} {
			if strings.Contains(e.Message, "secret "+name+" deleted by `flats secret reset`") {
				found[name]++
			}
		}
	}
	if found["API_KEY"] != 1 || found["OTHER_KEY"] != 1 {
		t.Fatalf("reset events: %+v", events)
	}
}

// A valid key that opens none of the values is no help: reset deletes them
// and keeps that key.
func TestSecretResetOverAnotherKey(t *testing.T) {
	dir := t.TempDir()
	path, _, h := hostWithSecrets(t, dir)
	h.Close()
	keyPath := filepath.Join(dir, core.SecretKeyFile)
	other := bytes.Repeat([]byte{7}, 32)
	if err := os.WriteFile(keyPath, other, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runSecretKey(t, path, "reset", "--yes")
	if err != nil || !strings.Contains(out, "opens none of the 2") || !strings.Contains(out, "deleted 2 values") || strings.Contains(out, "The next start creates") {
		t.Fatalf("reset: %q %v", out, err)
	}
	if got, _ := os.ReadFile(keyPath); !bytes.Equal(got, other) {
		t.Fatal("reset changed secret.key")
	}
	// One value is one value.
	if err := os.WriteFile(keyPath, other, 0o600); err != nil {
		t.Fatal(err)
	}
	h, err = Start(context.Background(), configModeOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Svc.SetSecret(context.Background(), "app", "API_KEY", "v", core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	h.Close()
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{8}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runSecretKey(t, path, "reset", "--yes"); err != nil || !strings.Contains(out, "deleted 1 value.") {
		t.Fatalf("one value: %q %v", out, err)
	}
}

// The host warns at start while the copy holds another key or has no ID, and
// stays silent without a copy or with a matching one.
func TestStartWarnsAboutAStaleCopy(t *testing.T) {
	dir := t.TempDir()
	path, _, h := hostWithSecrets(t, dir)
	h.Close()
	start := func() string {
		t.Helper()
		var logs bytes.Buffer
		log.SetOutput(&logs)
		defer log.SetOutput(os.Stderr)
		h, err := Start(context.Background(), configModeOptions(path))
		if err != nil {
			t.Fatalf("the start failed: %v", err)
		}
		h.Close()
		return logs.String()
	}
	const warning = "run `flats secret backup-key`"
	if logs := start(); strings.Contains(logs, warning) {
		t.Fatalf("a warning without a copy: %s", logs)
	}
	pw := passwordFile(t, "a long recovery password")
	backup := func() {
		t.Helper()
		if _, err := runSecretKey(t, path, "backup-key", "--password-file", pw); err != nil {
			t.Fatal(err)
		}
	}
	backup()
	if logs := start(); strings.Contains(logs, warning) {
		t.Fatalf("a warning about a matching copy: %s", logs)
	}
	if err := os.Remove(filepath.Join(dir, core.RecoveryKeyIDFile)); err != nil {
		t.Fatal(err)
	}
	if logs := start(); !strings.Contains(logs, warning) {
		t.Fatalf("no warning without the ID file: %s", logs)
	}

	// The key is lost and reset: the next start makes a new key, which the
	// copy does not hold.
	backup()
	if err := os.Remove(filepath.Join(dir, core.SecretKeyFile)); err != nil {
		t.Fatal(err)
	}
	if out, err := runSecretKey(t, path, "reset", "--yes"); err != nil || !strings.Contains(out, "because the existing secret.key.recovery holds the lost key") {
		t.Fatalf("reset: %q %v", out, err)
	}
	if logs := start(); !strings.Contains(logs, warning) {
		t.Fatalf("no warning after a reset: %s", logs)
	}
}

// With no sealed value there is nothing to check the key against: restore
// says so and writes the key.
func TestRestoreKeyWithNoSealedValues(t *testing.T) {
	dir := t.TempDir()
	path := initConfig(t, dir)
	h, err := Start(context.Background(), configModeOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
	keyPath := filepath.Join(dir, core.SecretKeyFile)
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	pw := passwordFile(t, "a long recovery password")
	if _, err := runSecretKey(t, path, "backup-key", "--password-file", pw); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	out, err := runSecretKey(t, path, "restore-key", "--password-file", pw)
	if err != nil || !strings.Contains(out, "nothing to check the key against") {
		t.Fatalf("restore: %q %v", out, err)
	}
	if got, _ := os.ReadFile(keyPath); !bytes.Equal(got, key) {
		t.Fatal("the restored key differs")
	}
	// Without a copy in the data directory, restore points at --from.
	os.Remove(keyPath)
	os.Remove(filepath.Join(dir, core.RecoveryKeyFile))
	if _, err := runSecretKey(t, path, "restore-key", "--password-file", pw); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "--from") {
		t.Fatalf("no copy: %v", err)
	}
	// A password file may end in any run of line ends.
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("p", maxPasswordBytes)
	if _, err := runSecretKey(t, path, "backup-key", "--password-file", passwordFile(t, long+"\r\n\r\n\n")); err != nil {
		t.Fatalf("1024-byte password with line ends: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, core.RecoveryKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := core.OpenKeyCopy(data, long); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("the 1024-byte password does not open the copy: %v", err)
	}
}

// A link at secret.key that leads to no file (a missing target, a loop, a
// file on its path) is something there: each command refuses and writes
// nothing through it. On Windows without the symlink privilege this test
// alone is skipped.
func TestSecretKeyCommandsOverADanglingLink(t *testing.T) {
	dir := t.TempDir()
	path, _, h := hostWithSecrets(t, dir)
	pw := passwordFile(t, "a long recovery password")
	if _, err := runSecretKey(t, path, "backup-key", "--password-file", pw); err != nil {
		t.Fatal(err)
	}
	h.Close()
	keyPath := filepath.Join(dir, core.SecretKeyFile)
	gone := filepath.Join(dir, "gone")
	for _, target := range []string{gone, keyPath, filepath.Join(path, "key")} {
		if err := os.Remove(keyPath); err != nil {
			t.Fatal(err)
		}
		if err := ostest.Symlink(t, target, keyPath); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			args []string
			want string
		}{
			{[]string{"backup-key", "--password-file", pw}, "away"},
			{[]string{"restore-key", "--password-file", pw}, "never replaces a key"},
			{[]string{"reset", "--yes"}, "away"},
		} {
			if _, err := runSecretKey(t, path, c.args...); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s over a link to %s: %v", c.args[0], target, err)
			}
			if _, err := os.Lstat(gone); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s wrote through the link", c.args[0])
			}
		}
	}
}

// interrupting answers each prompt and interrupts at the same time, as a
// Ctrl-C right after the answer does.
type interrupting struct{ cancel context.CancelFunc }

func (p interrupting) hidden(string) (string, error) {
	p.cancel()
	return "a long recovery password", nil
}

func (p interrupting) line(string) (string, error) {
	p.cancel()
	return "reset", nil
}

// An interrupt that arrives after the answers still writes nothing.
func TestSecretKeyCommandsStopOnAnInterrupt(t *testing.T) {
	dir := t.TempDir()
	path, _, h := hostWithSecrets(t, dir)
	h.Close()
	copyPath := filepath.Join(dir, core.RecoveryKeyFile)
	keyPath := filepath.Join(dir, core.SecretKeyFile)
	run := func(args ...string) error {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		old := openTerminal
		openTerminal = func(context.Context, io.Reader, io.Writer) prompter { return interrupting{cancel} }
		defer func() { openTerminal = old }()
		return SecretKeyCommand(ctx, append(args, "--config", path), strings.NewReader(""), io.Discard)
	}
	// backup-key and reset stop at their own check before writing; restore
	// stops earlier, when CheckKey's queries see the cancelled context.
	if err := run("backup-key"); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("backup-key: %v", err)
	}
	if _, err := os.Lstat(copyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an interrupted backup-key wrote the copy")
	}
	if _, err := runSecretKey(t, path, "backup-key", "--password-file", passwordFile(t, "a long recovery password")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if err := run("restore-key"); !errors.Is(err, context.Canceled) {
		t.Fatalf("restore-key: %v", err)
	}
	if _, err := os.Lstat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an interrupted restore-key wrote the key")
	}
	if err := run("reset"); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("reset: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if secs, err := st.ListAllSecrets(context.Background()); err != nil || len(secs) != 1 {
		t.Fatalf("an interrupted reset deleted secrets: %v %v", secs, err)
	}
}

func TestReadHiddenLine(t *testing.T) {
	// A line ends at "\n" on Unix and at "\r" on Windows; "\r\n" ends it on
	// both, and a backspace drops a byte.
	for _, c := range []struct{ in, want string }{
		{"pass word\r\n", "pass word"},
		{"pa\bass\r\nnext\r\n", "pass"},
		{"no line end", "no line end"},
	} {
		if got, err := readHiddenLine(strings.NewReader(c.in)); err != nil || got != c.want {
			t.Errorf("%q: %q %v", c.in, got, err)
		}
	}
	if _, err := readHiddenLine(strings.NewReader("")); !errors.Is(err, io.EOF) {
		t.Errorf("empty input: %v", err)
	}
	// It reads no further than its line.
	r := strings.NewReader("first\r\nsecond\r\n")
	if a, _ := readHiddenLine(r); a != "first" {
		t.Fatalf("first line %q", a)
	}
	if b, _ := readHiddenLine(r); b != "second" {
		t.Fatalf("second line %q", b)
	}
}

// The data directory reached through a link is still the data directory: a
// first backup with --out there is refused before anything is written. On
// Windows without the symlink privilege this test alone is skipped.
func TestBackupKeyOutThroughALink(t *testing.T) {
	dir := t.TempDir()
	path, _, h := hostWithSecrets(t, dir)
	defer h.Close()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := ostest.Symlink(t, dir, alias); err != nil {
		t.Fatal(err)
	}
	pw := passwordFile(t, "a long recovery password")
	for _, out := range []string{filepath.Join(alias, core.RecoveryKeyFile), filepath.Join(alias, "key.age")} {
		if _, err := runSecretKey(t, path, "backup-key", "--password-file", pw, "--out", out); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "outside the data directory") {
			t.Fatalf("--out %s: %v", out, err)
		}
	}
	for _, name := range []string{core.RecoveryKeyFile, core.RecoveryKeyIDFile, "key.age"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a refused backup wrote %s", name)
		}
	}
}
