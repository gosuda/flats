package app

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/ostest"
	"github.com/gosuda/flats/internal/store"
)

// docs/spec/secret-key-rotation.md: the refusals of rules 6, 8, 9, 12, 13, 14, 17, 19 and 20. Each creates no
// pending key and no FILE, writes none of the things that rule 11 of the recovery spec excludes, and leaves a
// pending key or a FILE that existed the same, byte for byte.

// fileState is what is at a path: the bytes of a file, or what else is there.
func fileState(path string) string {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "nothing"
	case err != nil:
		return "error: " + err.Error()
	case fi.Mode().IsRegular():
		b, err := os.ReadFile(path)
		if err != nil {
			return "unreadable file"
		}
		return "file: " + string(b)
	}
	return fi.Mode().Type().String()
}

// frozen records everything that a refusal must not change: the rows of the sealed values, the audit log, the names
// in the data directory and in the folder of the key, and what is at the key path, at the pending name, at the
// recovery copy and at each of files. The function that it returns fails the test when one of them changed.
func frozen(t *testing.T, h *rotHost, files ...string) func(when string) {
	t.Helper()
	files = append(files, h.keyPath, h.pending(), filepath.Join(h.dir, core.RecoveryKeyFile), filepath.Join(h.dir, core.RecoveryKeyIDFile))
	read := func() []string {
		t.Helper()
		got := append(rowsText(sealedRows(t, h.dir)), "events: "+strings.Join(auditSummary(auditIn(t, h.dir)), " | "))
		got = append(got, "data: "+strings.Join(listed(t, h.dir), " "), "key folder: "+strings.Join(listed(t, filepath.Dir(h.keyPath)), " "))
		for _, f := range files {
			got = append(got, f+" = "+fileState(f))
		}
		return got
	}
	before := read()
	return func(when string) {
		t.Helper()
		after := read()
		for i := range before {
			if i >= len(after) || after[i] != before[i] {
				t.Fatalf("%s changed the host:\n was %q\n  is %q", when, before[i], after[min(i, len(after)-1)])
			}
		}
		if len(after) != len(before) {
			t.Fatalf("%s changed the number of rows: %d -> %d", when, len(before), len(after))
		}
	}
}

// Rule 6: the command refuses while a host runs, and for a database that does not belong to the configuration.
func TestRotateKeyNeedsTheStoppedHostOfTheConfiguration(t *testing.T) {
	t.Run("a running host", func(t *testing.T) {
		dir := t.TempDir()
		config, key, h := hostWithSecrets(t, dir)
		defer h.Close()
		_, err := runSecretKey(t, config, "rotate-key", "--yes")
		refusedWith(t, "rotate-key beside a running host", err, "stop the running Flats host")
		if got := mustRead(t, filepath.Join(dir, core.SecretKeyFile)); !bytes.Equal(got, key) {
			t.Error("the key changed beside a running host")
		}
		mustNotBeThere(t, filepath.Join(dir, core.PendingKeyFile))
		if c, err := core.CheckKey(context.Background(), h.Store, key); err != nil || !c.All() {
			t.Errorf("the values after the refusal: %+v, %v", c, err)
		}
	})
	t.Run("the database of another host", func(t *testing.T) {
		h, other := newRotHost(t), newRotHost(t)
		// The data directory of h gets the database of the other host.
		for _, name := range []string{"flats.db-wal", "flats.db-shm"} {
			os.Remove(filepath.Join(h.dir, name))
		}
		if err := os.WriteFile(filepath.Join(h.dir, "flats.db"), mustRead(t, filepath.Join(other.dir, "flats.db")), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := h.rotate(t, "--yes")
		refusedWith(t, "rotate-key over the database of another host", err, "is not the database of "+h.config)
		if got := h.keyAt(t); !bytes.Equal(got, h.key) {
			t.Error("the key changed")
		}
		mustNotBeThere(t, h.pending())
	})
}

// Rule 8: the key path must hold a valid key.
func TestRotateKeyNeedsAValidKey(t *testing.T) {
	t.Run("the default path, nothing there, no sealed value", func(t *testing.T) {
		dir := t.TempDir()
		config := initConfig(t, dir) // never started, so no key exists yet
		_, err := rotateWith(t, config, nil, "--yes")
		refusedWith(t, "rotate-key without a key", err, filepath.Join(dir, core.SecretKeyFile)+" is missing", "there is no key to rotate yet", "the next start creates one")
		if got := listed(t, dir); slices.Contains(got, core.SecretKeyFile) || slices.Contains(got, core.PendingKeyFile) {
			t.Errorf("the refusal created a key file: %v", got)
		}
	})
	t.Run("the default path, nothing there, sealed values", func(t *testing.T) {
		h := newRotHost(t)
		if err := os.Remove(h.keyPath); err != nil {
			t.Fatal(err)
		}
		check := frozen(t, h)
		_, err := h.rotate(t, "--yes")
		// The message of the start for a missing key, with the key ID of the values (rule 23).
		refusedWith(t, "rotate-key without the key of the values", err, core.ErrSecretKeyMissing.Error(), "flats secret restore-key", "flats secret reset",
			"The stored values carry the key ID "+core.KeyID(h.key))
		if _, serr := startLogs(t, h.config); serr == nil || serr.Error() != err.Error() {
			t.Errorf("the refusal is not the one of the start:\n command: %v\n   start: %v", err, serr)
		}
		check("the refusal")
	})
	t.Run("a configured path, nothing there", func(t *testing.T) {
		h := newRotHost(t).outside(t)
		if err := os.Remove(h.keyPath); err != nil {
			t.Fatal(err)
		}
		check := frozen(t, h)
		_, err := h.rotate(t, "--yes")
		refusedWith(t, "rotate-key without the configured key", err, core.KeySetting, h.keyPath, "never creates a key at a configured path",
			"A key that only the service process sees", "a systemd credential", "a Docker secret", "docs/configuration.md",
			"The stored values carry the key ID "+core.KeyID(h.key))
		for _, wrong := range []string{"no key exists", "there is no key"} {
			if strings.Contains(err.Error(), wrong) {
				t.Errorf("the refusal says %q: %v", wrong, err)
			}
		}
		check("the refusal")
	})
	for name, breakIt := range map[string]func(t *testing.T, keyPath string){
		"31 bytes": func(t *testing.T, keyPath string) { mustWrite(t, keyPath, bytes.Repeat([]byte{1}, 31)) },
		"33 bytes": func(t *testing.T, keyPath string) { mustWrite(t, keyPath, bytes.Repeat([]byte{1}, 33)) },
		"a folder": func(t *testing.T, keyPath string) {
			if err := os.Remove(keyPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(keyPath, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	} {
		for kind, place := range map[string]func(t *testing.T, h *rotHost) *rotHost{
			"the default path":  func(t *testing.T, h *rotHost) *rotHost { return h },
			"a configured path": func(t *testing.T, h *rotHost) *rotHost { return h.outside(t) },
		} {
			t.Run(kind+", "+name, func(t *testing.T) {
				h := place(t, newRotHost(t))
				breakIt(t, h.keyPath)
				check := frozen(t, h)
				_, err := h.rotate(t, "--yes")
				refusedWith(t, "rotate-key over something that is no key", err, "move "+h.keyPath+" away first")
				check("the refusal")
			})
		}
	}
	t.Run("a second key in the data directory", func(t *testing.T) {
		h := newRotHost(t).outside(t)
		mustWrite(t, filepath.Join(h.dir, core.SecretKeyFile), h.key)
		check := frozen(t, h, filepath.Join(h.dir, core.SecretKeyFile))
		_, err := h.rotate(t, "--yes")
		refusedWith(t, "rotate-key with two keys", err, "move that file away")
		check("the refusal")
	})
	t.Run("a key that cannot be read", func(t *testing.T) {
		if ostest.Windows {
			t.Skip("Windows: no mode that keeps the owner from reading a file")
		}
		if os.Geteuid() == 0 {
			t.Skip("the user root reads a file of any mode")
		}
		h := newRotHost(t)
		if err := os.Chmod(h.keyPath, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(h.keyPath, 0o600) })
		_, err := h.rotate(t, "--yes")
		// Another failure to read the key: status 1, and no refusal.
		if exitCode(err) != 1 || !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("rotate-key over an unreadable key: %v (status %d)", err, exitCode(err))
		}
		mustNotBeThere(t, h.pending())
	})
}

// Rule 9: the key must open every sealed value. The refusal gives the numbers, lists the values that do not open
// without a value, operator keys apart from flat secrets, and names the ways out that work in that state.
func TestRotateKeyNeedsAKeyThatOpensEveryValue(t *testing.T) {
	t.Run("the key opens some values", func(t *testing.T) {
		h := newRotHost(t)
		putBogusSecret(t, h.dir, "BOGUS", "")
		st := openStore(t, h.dir)
		err := st.PutHostKey(context.Background(), store.HostKey{ID: core.HostKeyWebhookSecret, Nonce: make([]byte, 12), Ciphertext: make([]byte, 20),
			Source: core.KeySourceCLI, UpdatedAt: time.Now()}, store.Audit{})
		st.Close()
		if err != nil {
			t.Fatal(err)
		}
		check := frozen(t, h)
		_, err = h.rotate(t, "--yes")
		refusedWith(t, "rotate-key", err, "the key at "+h.keyPath+" opens 5 of the 7 sealed values", "nothing was written",
			"It does not open the operator keys webhook-secret", "It does not open secret BOGUS of app",
			"`flats secret restore-key`", "an operator key with `flats key rm`", "a flat secret with `flats secret rm` after a start of the host")
		for _, wrong := range []string{"flats secret reset", "carry the key ID"} {
			if strings.Contains(err.Error(), wrong) {
				t.Errorf("the refusal says %q: %v", wrong, err)
			}
		}
		if what := leaks(err.Error(), h.key); what != "" {
			t.Errorf("the refusal shows %s: %v", what, err)
		}
		check("the refusal")
	})
	t.Run("the key opens no value, and the values carry a key ID", func(t *testing.T) {
		h := newRotHost(t)
		other := bytes.Repeat([]byte{7}, 32)
		mustWrite(t, h.keyPath, other)
		check := frozen(t, h)
		_, err := h.rotate(t, "--yes")
		refusedWith(t, "rotate-key", err, "opens 0 of the 5 sealed values", "`flats secret restore-key`", "`flats secret reset`",
			"The values that do not open carry the key ID "+core.KeyID(h.key), "the file that an earlier `flats secret rotate-key --out` wrote can hold that key")
		if strings.Contains(err.Error(), "flats key rm") {
			t.Errorf("the refusal names `flats key rm` although the key opens no value: %v", err)
		}
		check("the refusal")
	})
	t.Run("the key opens no value, and the values carry no key ID", func(t *testing.T) {
		h := newRotHost(t)
		sqlExec(t, h.dir, `UPDATE secrets SET key_id=''`, `UPDATE host_keys SET key_id=''`)
		mustWrite(t, h.keyPath, bytes.Repeat([]byte{7}, 32))
		check := frozen(t, h)
		_, err := h.rotate(t, "--yes")
		refusedWith(t, "rotate-key", err, "opens 0 of the 5 sealed values", "`flats secret reset`")
		if strings.Contains(err.Error(), "carry the key ID") {
			t.Errorf("the refusal names a key ID that no value carries: %v", err)
		}
		check("the refusal")
	})
}

// Rule 12: before its first write the command opens the key file for writing as a check, and refuses and names --out
// when that fails. With --out it does not open the key path for writing, so such a key is rotated (rule 14).
func TestRotateKeyWithAKeyFileThatCannotBeWritten(t *testing.T) {
	if !ostest.Windows && os.Geteuid() == 0 {
		t.Skip("the user root writes a file of any mode")
	}
	for name, place := range map[string]func(t *testing.T, h *rotHost) *rotHost{
		"the default path":  func(t *testing.T, h *rotHost) *rotHost { return h },
		"a configured path": func(t *testing.T, h *rotHost) *rotHost { return h.outside(t) },
	} {
		t.Run(name, func(t *testing.T) {
			h := place(t, newRotHost(t))
			// Mode 0400 on Unix, and the read-only attribute on Windows.
			if err := os.Chmod(h.keyPath, 0o400); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(h.keyPath, 0o600) })
			check := frozen(t, h)
			_, err := h.rotate(t, "--yes")
			refusedWith(t, "rotate-key over a key that cannot be written", err, "cannot open the key file "+h.keyPath+" for writing", "nothing was written", "--out FILE")
			check("the refusal")

			file := filepath.Join(t.TempDir(), "new.key")
			h.mustRotate(t, "--yes", "--out", file)
			if got := h.keyAt(t); !bytes.Equal(got, h.key) {
				t.Fatal("the key path changed")
			}
			mustHoldValues(t, h.dir, mustRead(t, file))
			mustNotBeThere(t, h.pending())
		})
	}
}

// Rule 13: the folder of the pending key of a configured path must not lie inside the data directory once links
// are resolved. That is a refusal at point 4 of rule 7, which names --out and writes nothing.
func TestRotateKeyRefusesAPendingKeyInsideTheDataDirectory(t *testing.T) {
	h := newRotHost(t)
	// The key lies outside the data directory. The path that names it goes through a folder that is a link into the
	// data directory, so the pending key would be created there.
	real := filepath.Join(t.TempDir(), "real.key")
	if err := os.Rename(h.keyPath, real); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(h.dir, "keys")
	if err := os.Mkdir(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ostest.Symlink(t, real, filepath.Join(inner, "flats.key")); err != nil {
		t.Fatal(err)
	}
	door := filepath.Join(t.TempDir(), "door")
	if err := ostest.Symlink(t, inner, door); err != nil {
		t.Fatal(err)
	}
	h.keyPath = filepath.Join(door, "flats.key")
	if err := ConfigCommand(context.Background(), []string{"set", "host.secret_key_path", h.keyPath, "--config", h.config}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	check := frozen(t, h, real, filepath.Join(inner, "flats.key.next"))
	_, err := h.rotate(t, "--yes")
	refusedWith(t, "rotate-key with a pending key inside the data directory", err, "inside the data directory", "nothing was written", "--out FILE")
	check("the refusal")
	if got := listed(t, inner); !slices.Equal(got, []string{"flats.key"}) {
		t.Errorf("the folder in the data directory holds %v", got)
	}
}

// Rule 13: when the command cannot create the pending key, it refuses, names --out, and has changed no value.
func TestRotateKeyRefusesWhenItCannotCreateThePendingKey(t *testing.T) {
	ostest.SkipOnWindows(t, "no mode that keeps the owner from creating a file in a folder")
	if os.Geteuid() == 0 {
		t.Skip("the user root creates a file in a folder of any mode")
	}
	h := newRotHost(t).outside(t)
	keyDir := filepath.Dir(h.keyPath)
	if err := os.Chmod(keyDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(keyDir, 0o700) })
	check := frozen(t, h)
	_, err := h.rotate(t, "--yes")
	refusedWith(t, "rotate-key in a folder that cannot be written", err, "cannot create the pending key "+h.pending(), "no value was changed", "--out FILE")
	check("the refusal")
	// With --out the rotation needs no file in that folder.
	file := filepath.Join(t.TempDir(), "new.key")
	h.mustRotate(t, "--yes", "--out", file)
	mustHoldValues(t, h.dir, mustRead(t, file))
}

// Rule 14: FILE passes the checks of `flats secret restore-key --out`, and a FILE that exists is refused with a
// message that says which case it is. A refusal leaves a FILE that existed as it was.
func TestRotateKeyRefusesAnOutFile(t *testing.T) {
	h := newRotHost(t)
	elsewhere := t.TempDir()
	notAKey := filepath.Join(elsewhere, "not-a-key")
	mustWrite(t, notAKey, []byte("some other file"))
	sealedNothing := filepath.Join(elsewhere, "sealed-nothing.key")
	mustWrite(t, sealedNothing, bytes.Repeat([]byte{5}, 32))
	inPlace := filepath.Join(elsewhere, "in-place.key")
	mustWrite(t, inPlace, h.key)
	folder := filepath.Join(elsewhere, "folder")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	below := filepath.Join(h.dir, "below")
	if err := os.Mkdir(below, 0o700); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		file string
		want []string
	}{
		"a folder that does not exist":       {filepath.Join(elsewhere, "missing", "new.key"), []string{"--out"}},
		"a folder that is a file":            {filepath.Join(notAKey, "new.key"), []string{"--out"}},
		"the data directory":                 {filepath.Join(h.dir, "new.key"), []string{"outside the data directory"}},
		"a folder below the data directory":  {filepath.Join(below, "new.key"), []string{"outside the data directory"}},
		"a file that is no valid key":        {notAKey, []string{notAKey + " exists", "Remove it, or name another file"}},
		"a key that sealed no value":         {sealedNothing, []string{sealedNothing + " exists", "Remove it, or name another file"}},
		"a folder at the name":               {folder, []string{folder + " exists", "Remove it, or name another file"}},
		"the key that the key path holds":    {inPlace, []string{inPlace + " holds the key that " + h.keyPath + " holds too", "the key is in place", "remove " + inPlace}},
		"the key file itself":                {h.keyPath, []string{"holds the key that " + h.keyPath + " holds too"}},
		"the recovery copy's name, not used": {filepath.Join(h.dir, core.RecoveryKeyFile), []string{"outside the data directory"}},
	}
	check := frozen(t, h, notAKey, sealedNothing, inPlace, folder, filepath.Join(h.dir, "new.key"), filepath.Join(below, "new.key"), filepath.Join(elsewhere, "missing"))
	for name, tc := range cases {
		_, err := h.rotate(t, "--yes", "--out", tc.file)
		refusedWith(t, "--out "+name, err, tc.want...)
		check("--out " + name)
	}
	t.Run("through a link into the data directory", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "link")
		if err := ostest.Symlink(t, below, link); err != nil {
			t.Fatal(err)
		}
		_, err := h.rotate(t, "--yes", "--out", filepath.Join(link, "new.key"))
		refusedWith(t, "--out through a link", err, "outside the data directory")
		check("--out through a link")
	})
}

// Rule 17: something at the pending name that is no regular file, or a file that cannot be read, is none of the
// three kinds of a pending key. It stops the command, the start, `flats key set`, `flats secret reset`,
// `flats secret backup-key` and `flats backup --out` with an error that names the file, because it can be a pending
// key that waits.
func TestAPendingFileThatCannotBeClassifiedStopsEveryReader(t *testing.T) {
	kinds := map[string]func(t *testing.T, h *rotHost){
		// Not named by the criteria, and it needs no privilege: it runs on every system.
		"a folder": func(t *testing.T, h *rotHost) {
			if err := os.Mkdir(h.pending(), 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"a link": func(t *testing.T, h *rotHost) {
			target := filepath.Join(t.TempDir(), "a.key")
			mustWrite(t, target, bytes.Repeat([]byte{6}, 32))
			if err := ostest.Symlink(t, target, h.pending()); err != nil {
				t.Fatal(err)
			}
		},
		"a file that the user cannot read": func(t *testing.T, h *rotHost) {
			ostest.SkipOnWindows(t, "no mode that keeps the owner from reading a file")
			if os.Geteuid() == 0 {
				t.Skip("the user root reads a file of any mode")
			}
			mustWrite(t, h.pending(), bytes.Repeat([]byte{6}, 32))
			if err := os.Chmod(h.pending(), 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(h.pending(), 0o600) })
		},
	}
	for name, put := range kinds {
		t.Run(name, func(t *testing.T) {
			h := newRotHost(t)
			put(t, h)
			rows := rowsText(sealedRows(t, h.dir))
			events := len(auditIn(t, h.dir))
			pw := passwordFile(t, recoveryPassword)
			copies := t.TempDir()
			readers := map[string]func() error{
				"flats secret rotate-key":       func() error { _, err := h.rotate(t, "--yes"); return err },
				"flats secret rotate-key --out": func() error { _, err := h.rotate(t, "--yes", "--out", filepath.Join(copies, "new.key")); return err },
				"the start":                     func() error { _, err := startLogs(t, h.config); return err },
				"flats key set":                 func() error { return keySetError(h.config, core.HostKeyNetbird, rotNetbirdKey) },
				"flats secret reset":            func() error { _, err := runSecretKey(t, h.config, "reset", "--yes"); return err },
				"flats secret backup-key":       func() error { _, err := runSecretKey(t, h.config, "backup-key", "--password-file", pw); return err },
				"flats backup --out": func() error {
					_, err := runBackup(t, "--out", filepath.Join(copies, "a"), "--config", h.config)
					return err
				},
				"flats backup --exclude-secret-key": func() error {
					_, err := runBackup(t, "--out", filepath.Join(copies, "b"), "--exclude-secret-key", "--config", h.config)
					return err
				},
			}
			for reader, run := range readers {
				err := run()
				refusedWith(t, reader, err, h.pending(), "move it away", "make it a file that the user who runs Flats can read")
				if !errors.Is(err, core.ErrPendingKey) && !strings.Contains(err.Error(), core.ErrPendingKey.Error()) {
					t.Errorf("%s: the error is not the one of a pending key that cannot be used: %v", reader, err)
				}
			}
			if got := listed(t, copies); len(got) != 0 {
				t.Errorf("a refused command wrote %v", got)
			}
			if got := rowsText(sealedRows(t, h.dir)); !slices.Equal(got, rows) {
				t.Error("a refused command changed a row")
			}
			if n := len(auditIn(t, h.dir)); n != events {
				t.Errorf("the refused commands wrote %d events", n-events)
			}
			if got := h.keyAt(t); !bytes.Equal(got, h.key) {
				t.Error("a refused command changed the key")
			}
			mustNotBeThere(t, filepath.Join(h.dir, core.RecoveryKeyFile))
		})
	}
}

// Rule 19: a finish refuses, and writes nothing, when the key that it would put in place does not open every sealed
// value.
func TestRotateKeyFinishNeedsAKeyThatOpensEveryValue(t *testing.T) {
	t.Run("a pending key that waits", func(t *testing.T) {
		h := newRotHost(t)
		pending := stateC(t, h, rotateCheckpointed, "the old key")
		putBogusSecret(t, h.dir, "BOGUS", core.KeyID(pending))
		check := frozen(t, h)
		_, err := h.rotate(t)
		refusedWith(t, "the finish", err, "the pending key "+h.pending()+" opens 5 of the 6 sealed values", "nothing was written", "secret BOGUS of app")
		check("the refused finish")
	})
	t.Run("a pending key that is a duplicate", func(t *testing.T) {
		h := newRotHost(t)
		stopRotation(t, h, rotateKeyWritten, "--yes")
		putBogusSecret(t, h.dir, "BOGUS", core.KeyID(h.keyAt(t)))
		check := frozen(t, h)
		_, err := h.rotate(t)
		refusedWith(t, "the finish", err, "the key at "+h.keyPath+" opens 5 of the 6 sealed values", "nothing was written")
		check("the refused finish")
	})
	t.Run("the FILE of --out", func(t *testing.T) {
		h := newRotHost(t)
		file := filepath.Join(t.TempDir(), "new.key")
		stopRotation(t, h, rotateCheckpointed, "--yes", "--out", file)
		putBogusSecret(t, h.dir, "BOGUS", core.KeyID(mustRead(t, file)))
		check := frozen(t, h, file)
		_, err := h.rotate(t, "--out", file)
		refusedWith(t, "the finish", err, "the key in "+file+" opens 5 of the 6 sealed values", "nothing was written")
		check("the refused finish")
	})
	t.Run("a second key in the data directory", func(t *testing.T) {
		// The checks of rule 3 of the key path spec apply to a finish at a configured path.
		h := newRotHost(t).outside(t)
		stateC(t, h, rotateCheckpointed, "the old key")
		mustWrite(t, filepath.Join(h.dir, core.SecretKeyFile), h.key)
		check := frozen(t, h, filepath.Join(h.dir, core.SecretKeyFile))
		_, err := h.rotate(t)
		refusedWith(t, "the finish", err, "move that file away")
		check("the refused finish")
	})
}

// Rule 20: the refusals of the recovery copy and its password. A wrong password, or a copy that does not open, is a
// refusal that writes nothing and says what an operator who lost the password does. Without a terminal and without
// --password-file the command stops with a usage error. --password-file without a recovery copy is a refusal.
func TestRotateKeyRecoveryCopyRefusals(t *testing.T) {
	lost := []string{"If the recovery password is lost, remove", "which opens nothing without its password", "`flats secret backup-key`"}
	t.Run("--password-file without a recovery copy", func(t *testing.T) {
		h := newRotHost(t)
		check := frozen(t, h)
		_, err := h.rotate(t, "--yes", "--password-file", passwordFile(t, recoveryPassword))
		refusedWith(t, "--password-file without a copy", err, "--password-file", "there is no recovery copy", "to make current")
		check("the refusal")
	})
	h := newRotHost(t)
	pw := passwordFile(t, recoveryPassword)
	mustRunSecretKey(t, h.config, "backup-key", "--password-file", pw)
	copyPath := filepath.Join(h.dir, core.RecoveryKeyFile)
	good := mustRead(t, copyPath)
	check := frozen(t, h)

	// No terminal and no --password-file: a usage error that names the flag.
	_, err := h.rotate(t, "--yes")
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "--password-file") {
		t.Fatalf("no terminal and no --password-file: %v (status %d)", err, exitCode(err))
	}
	check("the usage error")

	_, err = h.rotate(t, "--yes", "--password-file", passwordFile(t, "not the password"))
	refusedWith(t, "a wrong password", err, append(lost, core.ErrRecoveryPassword.Error(), "nothing was written")...)
	check("a wrong password")
	t.Run("on the terminal", func(t *testing.T) { // a subtest, so that the terminal is gone after it
		p := &rotatePrompter{password: "not the password either", answer: "rotate"}
		useTerminal(t, p)
		_, err := h.rotate(t)
		refusedWith(t, "a wrong password on the terminal", err, append(lost, core.ErrRecoveryPassword.Error())...)
		if p.hiddenAsked != 1 || len(p.lines) != 0 {
			t.Errorf("the command asked %d times for the password and then %q; want one question and no confirmation after a wrong password", p.hiddenAsked, p.lines)
		}
		check("a wrong password on the terminal")
	})
	_, err = h.rotate(t, "--yes", "--password-file", passwordFile(t, ""))
	refusedWith(t, "an empty password", err, "the password is empty")
	_, err = h.rotate(t, "--yes", "--password-file", passwordFile(t, strings.Repeat("x", maxPasswordBytes+1)))
	refusedWith(t, "a password over 1024 bytes", err, "longer than")
	_, err = h.rotate(t, "--yes", "--password-file", filepath.Join(t.TempDir(), "no-such-file"))
	refusedWith(t, "a password file that is missing", err, "--password-file")
	check("the refusals of the password")

	// A copy that does not open.
	for name, data := range map[string][]byte{
		"not a recovery file": []byte("this is no age file"),
		"larger than 64 KiB":  make([]byte, core.MaxRecoveryFileSize+1),
	} {
		mustWrite(t, copyPath, data)
		_, err = h.rotate(t, "--yes", "--password-file", pw)
		refusedWith(t, "a copy that is "+name, err, append(lost, "nothing was written")...)
		if got := mustRead(t, copyPath); !bytes.Equal(got, data) {
			t.Errorf("the refusal changed a copy that is %s", name)
		}
		if got := h.keyAt(t); !bytes.Equal(got, h.key) {
			t.Fatal("the refusal changed the key")
		}
		mustNotBeThere(t, h.pending())
	}
	mustWrite(t, copyPath, good)
	check("the refusals of the copy")

	// The same refusal stops a finish, which asks for the password as a rotation does.
	stopRotation(t, h, rotateCheckpointed, "--yes", "--password-file", pw)
	check = frozen(t, h)
	_, err = h.rotate(t, "--password-file", passwordFile(t, "not the password"))
	refusedWith(t, "a finish with a wrong password", err, append(lost, core.ErrRecoveryPassword.Error())...)
	_, err = h.rotate(t)
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "--password-file") {
		t.Fatalf("a finish without a terminal and without --password-file: %v (status %d)", err, exitCode(err))
	}
	check("the refused finish")
}
