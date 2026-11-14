package core

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/ostest"
	"github.com/gosuda/flats/internal/store"
)

// listDir lists the entries of a folder, to show that a refusal or a read changed nothing.
func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// needsAnUnprivilegedUnixUser skips a test that depends on a mode the superuser ignores, or on a mode that Windows
// does not have.
func needsAnUnprivilegedUnixUser(t *testing.T) {
	t.Helper()
	if ostest.Windows || os.Geteuid() == 0 {
		t.Skip("needs Unix modes and a user that is not the superuser")
	}
}

func goodKey() []byte { return bytes.Repeat([]byte{7}, 32) }

func TestDefaultKeyFileKeepsTheOldBehavior(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	k := NewKeyFile(dir, "")
	if k.Configured() || k.Path() != filepath.Join(dir, SecretKeyFile) || k.DataDir() != dir {
		t.Fatalf("KeyFile = %+v", k)
	}
	if err := k.Check(); err != nil {
		t.Errorf("Check of a default key = %v, want nil", err)
	}
	if _, err := k.Read(); !errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrKeyLocation) {
		t.Errorf("Read of a missing default key = %v, want a plain not-exist error", err)
	}
	key, err := k.LoadOrCreate()
	if err != nil || len(key) != 32 {
		t.Fatalf("LoadOrCreate = %v, %v; want a new key", len(key), err)
	}
	if again, err := k.LoadOrCreate(); err != nil || !bytes.Equal(again, key) {
		t.Errorf("the second LoadOrCreate = %v; want the same key", err)
	}
}

// With host.secret_key_path the host reads the file and never creates it (rules 5 to 7 of the spec).
func TestConfiguredKeyIsNeverCreated(t *testing.T) {
	t.Parallel()
	data, keyDir := t.TempDir(), t.TempDir()
	path := filepath.Join(keyDir, "secret.key")
	k := NewKeyFile(data, path)
	if !k.Configured() || k.Path() != path {
		t.Fatalf("KeyFile = %+v", k)
	}
	for name, call := range map[string]func() error{
		"LoadOrCreate": func() error { _, err := k.LoadOrCreate(); return err },
		"Read":         func() error { _, err := k.Read(); return err },
		"SealHostKey": func() error {
			_, err := SealHostKey(k, HostKeyTailscale, "tskey-auth-x", KeySourceCLI, time.Now())
			return err
		},
	} {
		err := call()
		if !errors.Is(err, ErrKeyLocation) {
			t.Errorf("%s: err = %v, want ErrKeyLocation", name, err)
			continue
		}
		wants := []string{path, KeySetting, "never creates", "flats secret restore-key", "flats config unset"}
		if runtime.GOOS != "windows" {
			wants = append(wants, "head -c 32 /dev/urandom") // Windows gets no shell line (the docs give a PowerShell one)
		}
		for _, want := range wants {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the message lacks %q: %v", name, want, err)
			}
		}
	}
	if got := listDir(t, keyDir); len(got) != 0 {
		t.Errorf("the key folder holds %v after the refusals", got)
	}
	if got := listDir(t, data); len(got) != 0 {
		t.Errorf("the data directory holds %v after the refusals", got)
	}
}

func TestConfiguredKeyThatIsNotAKey(t *testing.T) {
	t.Parallel()
	for name, prepare := range map[string]func(t *testing.T, path string){
		"a directory": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"31 bytes": func(t *testing.T, path string) { putFile(t, path, bytes.Repeat([]byte{1}, 31)) },
		"33 bytes": func(t *testing.T, path string) { putFile(t, path, bytes.Repeat([]byte{1}, 33)) },
		"a newline after the key": func(t *testing.T, path string) {
			putFile(t, path, append(goodKey(), '\n'))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data, keyDir := t.TempDir(), t.TempDir()
			path := filepath.Join(keyDir, "secret.key")
			prepare(t, path)
			_, err := NewKeyFile(data, path).Read()
			if !errors.Is(err, ErrKeyLocation) || !errors.Is(err, ErrInvalidSecretKey) {
				t.Fatalf("err = %v, want ErrKeyLocation and ErrInvalidSecretKey", err)
			}
			if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), KeySetting) {
				t.Errorf("the message does not name the path and the setting: %v", err)
			}
		})
	}
}

func putFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestConfiguredKeyThatIsALinkToNothing(t *testing.T) {
	t.Parallel()
	data, keyDir := t.TempDir(), t.TempDir()
	path := filepath.Join(keyDir, "secret.key")
	if err := ostest.Symlink(t, filepath.Join(keyDir, "not-mounted"), path); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	_, err := NewKeyFile(data, path).Read()
	if !errors.Is(err, ErrKeyLocation) {
		t.Fatalf("err = %v, want ErrKeyLocation", err)
	}
	if !strings.Contains(err.Error(), "link to nothing") {
		t.Errorf("the message does not say that the path is a link to nothing: %v", err)
	}
	if strings.Contains(err.Error(), "restore-key") {
		t.Errorf("the message offers restore-key, which refuses anything at the path: %v", err)
	}
}

// A valid key works from a read-only folder, with mode 0444, and nothing is created (rule 7).
func TestConfiguredKeyInAReadOnlyPlace(t *testing.T) {
	needsAnUnprivilegedUnixUser(t)
	t.Parallel()
	data, keyDir := t.TempDir(), t.TempDir()
	path := filepath.Join(keyDir, "secret.key")
	putFile(t, path, goodKey())
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(keyDir, 0o700) })
	k := NewKeyFile(data, path)
	for i := 0; i < 2; i++ {
		got, err := k.LoadOrCreate()
		if err != nil || !bytes.Equal(got, goodKey()) {
			t.Fatalf("LoadOrCreate = %v, %v", got, err)
		}
	}
	if err := os.Chmod(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := listDir(t, keyDir); !slices.Equal(got, []string{"secret.key"}) {
		t.Errorf("the key folder holds %v", got)
	}
}

func TestConfiguredKeyThatCannotBeRead(t *testing.T) {
	needsAnUnprivilegedUnixUser(t)
	t.Parallel()
	data, keyDir := t.TempDir(), t.TempDir()
	path := filepath.Join(keyDir, "secret.key")
	putFile(t, path, goodKey())
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	_, err := NewKeyFile(data, path).Read()
	if !errors.Is(err, ErrKeyLocation) || !strings.Contains(err.Error(), path) {
		t.Errorf("err = %v, want ErrKeyLocation that names the path", err)
	}
}

// Two keys would be in use, and a copy of the data directory would hold one of them (rule 3).
func TestConfiguredKeyRefusesALeftoverKeyInTheDataDirectory(t *testing.T) {
	t.Parallel()
	for name, prepare := range map[string]func(t *testing.T, leftover string){
		"a file":      func(t *testing.T, p string) { putFile(t, p, goodKey()) },
		"a directory": func(t *testing.T, p string) { os.Mkdir(p, 0o700) },
		"a link": func(t *testing.T, p string) {
			if err := ostest.Symlink(t, filepath.Join(filepath.Dir(p), "nowhere"), p); err != nil {
				t.Skipf("no symbolic links here: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data, keyDir := t.TempDir(), t.TempDir()
			path := filepath.Join(keyDir, "secret.key")
			putFile(t, path, goodKey())
			leftover := filepath.Join(data, SecretKeyFile)
			prepare(t, leftover)
			for what, err := range map[string]error{"Check": NewKeyFile(data, path).Check(), "Read": readErr(NewKeyFile(data, path))} {
				if !errors.Is(err, ErrKeyLocation) || !strings.Contains(err.Error(), leftover) || !strings.Contains(err.Error(), "move that file away") {
					t.Errorf("%s: err = %v, want ErrKeyLocation that names %s", what, err, leftover)
				}
			}
			// With no setting the same file is the key, and nothing is refused.
			if err := NewKeyFile(data, "").Check(); err != nil {
				t.Errorf("Check of a default key: %v", err)
			}
		})
	}
}

func readErr(k KeyFile) error { _, err := k.Read(); return err }

// A key whose resolved path lies inside the resolved data directory is refused, whatever name it was given (rule 3).
func TestConfiguredKeyThatResolvesIntoTheDataDirectory(t *testing.T) {
	t.Parallel()
	t.Run("a link into the data directory", func(t *testing.T) {
		t.Parallel()
		data, keyDir := t.TempDir(), t.TempDir()
		inner := filepath.Join(data, "keys", "k")
		if err := os.MkdirAll(filepath.Dir(inner), 0o700); err != nil {
			t.Fatal(err)
		}
		putFile(t, inner, goodKey())
		path := filepath.Join(keyDir, "secret.key")
		if err := ostest.Symlink(t, inner, path); err != nil {
			t.Skipf("no symbolic links here: %v", err)
		}
		err := readErr(NewKeyFile(data, path))
		if !errors.Is(err, ErrKeyLocation) || !strings.Contains(err.Error(), "inside the data directory") {
			t.Errorf("err = %v, want a refusal that says the key resolves inside the data directory", err)
		}
	})
	t.Run("a data directory that is a link to the key's folder", func(t *testing.T) {
		t.Parallel()
		real, root := t.TempDir(), t.TempDir()
		putFile(t, filepath.Join(real, "secret.key"), goodKey())
		data := filepath.Join(root, "data")
		if err := ostest.Symlink(t, real, data); err != nil {
			t.Skipf("no symbolic links here: %v", err)
		}
		// The key is configured at its real path, which lies in what the data directory resolves to. <data>/secret.key
		// is that same file, so the refusal must not tell the operator to move "that file" away: it is the only key.
		err := readErr(NewKeyFile(data, filepath.Join(real, "secret.key")))
		if !errors.Is(err, ErrKeyLocation) || !strings.Contains(err.Error(), "the same file") || strings.Contains(err.Error(), "move that file away") {
			t.Errorf("err = %v, want a refusal that says the two paths are the same file and does not say to move it away", err)
		}
	})
	t.Run("a key outside the data directory is fine", func(t *testing.T) {
		t.Parallel()
		data, keyDir := t.TempDir(), t.TempDir()
		path := filepath.Join(keyDir, "secret.key")
		putFile(t, path, goodKey())
		if got, err := NewKeyFile(data, path).Read(); err != nil || !bytes.Equal(got, goodKey()) {
			t.Errorf("Read = %v, %v", got, err)
		}
	})
}

// CheckSecretKey of a configured key refuses with no sealed value in flats.db too, and never creates the key (rule 5).
func TestCheckSecretKeyOfAConfiguredKey(t *testing.T) {
	t.Parallel()
	data, keyDir := t.TempDir(), t.TempDir()
	st, err := store.Open(filepath.Join(data, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	path := filepath.Join(keyDir, "secret.key")
	if err := CheckSecretKey(ctx, NewKeyFile(data, path), st); !errors.Is(err, ErrKeyLocation) {
		t.Errorf("a missing configured key: err = %v, want ErrKeyLocation", err)
	}
	// The default key, with no sealed value, is left for the start to create.
	if err := CheckSecretKey(ctx, NewKeyFile(data, ""), st); err != nil {
		t.Errorf("a missing default key with no sealed value: %v", err)
	}
	putFile(t, path, goodKey())
	if err := CheckSecretKey(ctx, NewKeyFile(data, path), st); err != nil {
		t.Errorf("a valid configured key: %v", err)
	}
	if got := listDir(t, keyDir); !slices.Equal(got, []string{"secret.key"}) {
		t.Errorf("the key folder holds %v", got)
	}
}

// Restore writes beside the configured path, never replaces a file, and leaves no temporary file (rule 12).
func TestRestoreOfAConfiguredKey(t *testing.T) {
	t.Parallel()
	data, keyDir := t.TempDir(), t.TempDir()
	path := filepath.Join(keyDir, "secret.key")
	k := NewKeyFile(data, path)
	if err := k.Restore(goodKey()); err != nil {
		t.Fatal(err)
	}
	if got, err := k.Read(); err != nil || !bytes.Equal(got, goodKey()) {
		t.Fatalf("Read = %v, %v", got, err)
	}
	if err := k.Restore(bytes.Repeat([]byte{9}, 32)); !os.IsExist(err) {
		t.Errorf("a second Restore: err = %v, want os.IsExist", err)
	}
	if got := listDir(t, keyDir); !slices.Equal(got, []string{"secret.key"}) {
		t.Errorf("the key folder holds %v; want the key and no temporary file", got)
	}
	if got := listDir(t, data); len(got) != 0 {
		t.Errorf("the data directory holds %v; the temporary file lies beside the configured path", got)
	}
}

func TestRestoreOfAConfiguredKeyInAReadOnlyFolder(t *testing.T) {
	needsAnUnprivilegedUnixUser(t)
	t.Parallel()
	data, keyDir := t.TempDir(), t.TempDir()
	if err := os.Chmod(keyDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(keyDir, 0o700) })
	k := NewKeyFile(data, filepath.Join(keyDir, "secret.key"))
	if err := k.Restore(goodKey()); err == nil {
		t.Fatal("Restore wrote into a read-only folder")
	}
	if err := os.Chmod(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := listDir(t, keyDir); len(got) != 0 {
		t.Errorf("the failed Restore left %v", got)
	}
}

// ReadUnchecked does not repeat the checks of Check: a second key that appears in the data directory during a backup does
// not fail the re-read, and a missing file still does.
func TestReadUncheckedSkipsTheChecksOfCheck(t *testing.T) {
	t.Parallel()
	data, keyDir := t.TempDir(), t.TempDir()
	path := filepath.Join(keyDir, "secret.key")
	putFile(t, path, goodKey())
	k := NewKeyFile(data, path)
	putFile(t, filepath.Join(data, SecretKeyFile), goodKey())
	if _, err := k.Read(); !errors.Is(err, ErrKeyLocation) {
		t.Fatalf("Read with a second key: err = %v, want ErrKeyLocation", err)
	}
	if got, err := k.ReadUnchecked(); err != nil || !bytes.Equal(got, goodKey()) {
		t.Errorf("ReadUnchecked = %v, %v; want the key", got, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := k.ReadUnchecked(); !errors.Is(err, ErrKeyLocation) {
		t.Errorf("ReadUnchecked of a missing key: err = %v, want ErrKeyLocation", err)
	}
	// A default key is read as before.
	if got, err := NewKeyFile(data, "").ReadUnchecked(); err != nil || !bytes.Equal(got, goodKey()) {
		t.Errorf("ReadUnchecked of the default key = %v, %v", got, err)
	}
}
