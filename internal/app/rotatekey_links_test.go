package app

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/ostest"
)

// docs/spec/secret-key-rotation.md: the names of one key file. A link can make the key path and another name the
// same file, and the command must never remove, or tell the operator to remove, the only file with the key.

// Rule 14: a FILE that the key path reaches through a link is the key file. The refusal does not say to remove it.
func TestRotateKeyOutThatTheKeyPathLinksTo(t *testing.T) {
	h := newRotHost(t)
	file := filepath.Join(t.TempDir(), "earlier.key")
	if err := os.Rename(h.keyPath, file); err != nil {
		t.Fatal(err)
	}
	if err := ostest.Symlink(t, file, h.keyPath); err != nil {
		t.Fatal(err)
	}
	check := frozen(t, h, file)
	_, err := h.rotate(t, "--yes", "--out", file)
	refusedWith(t, "--out with the file that the key path links to", err, file+" is the key file that the key path "+h.keyPath+" names", "do not remove "+file)
	if msg := err.Error(); strings.Contains(msg, "you can remove") || strings.Contains(msg, "Remove ") {
		t.Errorf("the refusal says to remove the key file: %v", err)
	}
	check("the refusal")
	mustStart(t, h, rotSealed)
}

// Rule 17: a key path that is a symbolic link to the pending key file holds the bytes of a duplicate, but the
// pending key file is then the only file with the key. Every reader stops, and nothing removes the file. With a real
// copy at the key path, the command finishes the rotation.
func TestAKeyPathThatLeadsToThePendingKeyStopsEveryReader(t *testing.T) {
	h := newRotHost(t)
	pending := stateC(t, h, rotateCheckpointed, "nothing")
	if err := ostest.Symlink(t, h.pending(), h.keyPath); err != nil {
		t.Fatal(err)
	}
	rows := rowsText(sealedRows(t, h.dir))
	events := len(auditIn(t, h.dir))
	pw := passwordFile(t, recoveryPassword)
	copies := t.TempDir()
	readers := map[string]func() error{
		"flats secret rotate-key": func() error { _, err := h.rotate(t); return err },
		"the start":               func() error { _, err := startLogs(t, h.config); return err },
		"flats key set":           func() error { return keySetError(h.config, core.HostKeyNetbird, rotNetbirdKey) },
		"flats secret reset":      func() error { _, err := runSecretKey(t, h.config, "reset", "--yes"); return err },
		"flats secret backup-key": func() error { _, err := runSecretKey(t, h.config, "backup-key", "--password-file", pw); return err },
		"flats backup --out": func() error {
			_, err := runBackup(t, "--out", filepath.Join(copies, "a"), "--config", h.config)
			return err
		},
	}
	for reader, run := range readers {
		err := run()
		refusedWith(t, reader, err, h.keyPath+" is a link that leads to the pending key file "+h.pending(), "put a real copy of that file at the key path")
		if !errors.Is(err, core.ErrPendingKey) && !strings.Contains(err.Error(), core.ErrPendingKey.Error()) {
			t.Errorf("%s: the error is not the one of a pending key that cannot be used: %v", reader, err)
		}
		// The only file with the key is there, with its bytes, and the link still leads to it.
		if got := mustRead(t, h.pending()); !bytes.Equal(got, pending) {
			t.Fatalf("%s changed the pending key", reader)
		}
		if fi, err := os.Lstat(h.keyPath); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			t.Fatalf("%s changed the link at the key path: %v", reader, err)
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
	mustNotBeThere(t, filepath.Join(h.dir, core.RecoveryKeyFile))

	// The way out that the refusal names: a real copy in place of the link.
	if err := os.Remove(h.keyPath); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, h.keyPath, pending)
	out := h.mustRotate(t)
	if !strings.Contains(out, "finishing a rotation that had stopped") {
		t.Errorf("the output of the finish:\n%s", out)
	}
	mustNotBeThere(t, h.pending())
	if got := h.keyAt(t); !bytes.Equal(got, pending) {
		t.Fatal("the key file does not hold the key after the finish")
	}
	mustStart(t, h, rotSealed)
	mustHoldValues(t, h.dir, pending)
}

// A hard link is a second name of the pending key file, and not a link to it: the key path keeps the key when the
// pending name goes. So it is a duplicate, and the finish removes the pending name (rule 19).
func TestAKeyPathThatIsAHardLinkOfThePendingKeyIsADuplicate(t *testing.T) {
	h := newRotHost(t)
	pending := stateC(t, h, rotateCheckpointed, "nothing")
	if err := os.Link(h.pending(), h.keyPath); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	mustStart(t, h, rotSealed)
	h.mustRotate(t)
	mustNotBeThere(t, h.pending())
	if got := h.keyAt(t); !bytes.Equal(got, pending) {
		t.Fatal("the key file does not hold the key after the removal of the pending name")
	}
	mustStart(t, h, rotSealed)
	mustHoldValues(t, h.dir, pending)
}

// Rule 25: when flats.db cannot be read as a database, `flats secret backup-key` makes no check of the key IDs and
// writes the copy, as before the key IDs. A copy of the key helps most beside a database that is damaged.
func TestBackupKeyWritesTheCopyBesideADatabaseThatCannotBeRead(t *testing.T) {
	h := newRotHost(t)
	db, err := os.OpenFile(filepath.Join(h.dir, "flats.db"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.WriteAt(bytes.Repeat([]byte{0xff}, 100), 0); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = runSecretKey(t, h.config, "backup-key", "--password-file", passwordFile(t, recoveryPassword))
	if err != nil && (exitCode(err) != 1 || strings.Contains(err.Error(), "key IDs")) {
		t.Fatalf("backup-key beside a damaged database: %v (status %d), want the copy and at most a failure of the audit event", err, exitCode(err))
	}
	got, oerr := core.OpenKeyCopy(mustRead(t, filepath.Join(h.dir, core.RecoveryKeyFile)), strings.TrimSpace(recoveryPassword))
	if oerr != nil || !bytes.Equal(got, h.key) {
		t.Errorf("the copy does not hold the key at the key path: %v", oerr)
	}
}
