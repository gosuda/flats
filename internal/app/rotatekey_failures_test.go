package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
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

// docs/spec/secret-key-rotation.md, rules 15, 16, 19 and 20: what the command does when one of its steps fails, and
// when an interrupt arrives.

// sqlExec runs statements against flats.db through a connection of its own.
func sqlExec(t *testing.T, dir string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "flats.db"))+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// putBogusSecret stores a flat secret that no key opens, with the label of the key with the ID keyID.
func putBogusSecret(t *testing.T, dir, name, keyID string) {
	t.Helper()
	st := openStore(t, dir)
	defer st.Close()
	sec := store.SealedSecret{Name: name, Nonce: make([]byte, 12), Ciphertext: make([]byte, 20), KeyID: keyID, UpdatedAt: time.Now()}
	if err := st.PutSecret(context.Background(), "app", sec, store.Audit{}); err != nil {
		t.Fatal(err)
	}
}

// failed fails the test unless err is a failure with status 1 whose message holds each of want.
func failedWith(t *testing.T, what string, err error, want ...string) {
	t.Helper()
	if err == nil || exitCode(err) != 1 {
		t.Fatalf("%s: %v (status %d), want a failure with status 1", what, err, exitCode(err))
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("%s: the message lacks %q: %v", what, w, err)
		}
	}
}

// A value that does not open inside the transaction changes nothing: every value opens with the old key, no value
// has a new key ID, no key.rotate event exists, and the pending key stays (rule 15). The next run refuses by rule
// 9 and leaves the pending key where it is (rule 21).
func TestRotateKeyAValueThatDoesNotOpenInTheTransaction(t *testing.T) {
	h := newRotHost(t)
	before := rowsText(sealedRows(t, h.dir))
	// The value appears after the check of rule 9 and before the transaction.
	out, err := rotateWith(t, h.config, func(p string) error {
		if p == rotateNewKeyWritten {
			putBogusSecret(t, h.dir, "BOGUS", core.KeyID(h.key))
		}
		return nil
	}, "--yes")
	failedWith(t, "the re-seal", err, "the re-seal failed", "secret BOGUS of app does not open with the old key", h.pending()+" stays", "flats secret rotate-key")
	if strings.Contains(out, "operator keys again") {
		t.Errorf("the output reports a re-seal:\n%s", out)
	}
	pending := mustRead(t, h.pending())
	if len(pending) != 32 {
		t.Fatalf("the pending key holds %d bytes", len(pending))
	}
	values, failed := openedWith(t, h.dir, h.key)
	if !mapsEqual(values, rotValues()) || !slices.Equal(failed, []string{"app/BOGUS"}) {
		t.Errorf("the old key opens %v and not %v", values, failed)
	}
	after := sealedRows(t, h.dir)
	for _, r := range after {
		if r.keyID == core.KeyID(pending) {
			t.Errorf("%s carries the ID of the new key", r.name)
		}
	}
	var kept []string
	for _, line := range rowsText(after) {
		if !strings.HasPrefix(line, "app/BOGUS ") {
			kept = append(kept, line)
		}
	}
	if !slices.Equal(kept, before) {
		t.Error("the failed transaction changed a row")
	}
	if n := len(eventsOf(t, h.dir, store.AuditKeyRotate)); n != 0 {
		t.Errorf("%d key.rotate events after a failed transaction", n)
	}
	if got := h.keyAt(t); !bytes.Equal(got, h.key) {
		t.Error("the key file changed")
	}

	// The next run: the key does not open every value (rule 9). The pending key, which is left over, stays.
	_, err = h.rotate(t, "--yes")
	refusedWith(t, "rotate-key over a value that does not open", err, "opens 5 of the 6 sealed values", "nothing was written",
		"It does not open secret BOGUS of app", "flats secret restore-key", "`flats key rm`", "`flats secret rm` after a start of the host")
	if strings.Contains(err.Error(), "flats secret reset") {
		t.Errorf("the refusal names reset although the key opens some values: %v", err)
	}
	if got := mustRead(t, h.pending()); !bytes.Equal(got, pending) {
		t.Error("the refused run changed the pending key")
	}
}

// A commit that fails stops the command with status 1, and the command removes neither the pending key nor FILE,
// because the process cannot always know whether a commit exists. The message names the file that stays (rule 15).
func TestRotateKeyAFailedCommitKeepsTheFiles(t *testing.T) {
	// A deferred foreign key is checked by the COMMIT statement, so every statement before it succeeds.
	breakCommit := func(t *testing.T, dir string) (restore func()) {
		sqlExec(t, dir,
			`CREATE TABLE rotate_parent (id INTEGER PRIMARY KEY)`,
			`CREATE TABLE rotate_child (id INTEGER REFERENCES rotate_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
			`CREATE TRIGGER rotate_commit_fails AFTER INSERT ON audit_events WHEN NEW.kind='key.rotate' BEGIN INSERT INTO rotate_child VALUES (1); END`)
		return func() {
			sqlExec(t, dir, `DROP TRIGGER rotate_commit_fails`, `DROP TABLE rotate_child`, `DROP TABLE rotate_parent`)
		}
	}
	t.Run("the pending key stays", func(t *testing.T) {
		h := newRotHost(t)
		before := rowsText(sealedRows(t, h.dir))
		restore := breakCommit(t, h.dir)
		_, err := h.rotate(t, "--yes")
		restore()
		failedWith(t, "the commit", err, "the re-seal failed", "FOREIGN KEY", h.pending()+" stays")
		pending := mustRead(t, h.pending())
		if len(pending) != 32 {
			t.Fatalf("the pending key holds %d bytes", len(pending))
		}
		if got := rowsText(sealedRows(t, h.dir)); !slices.Equal(got, before) {
			t.Error("the failed commit changed a row")
		}
		if n := len(eventsOf(t, h.dir, store.AuditKeyRotate)); n != 0 {
			t.Errorf("%d key.rotate events after a failed commit", n)
		}
		mustHoldValues(t, h.dir, h.key)
		// The next run reads the key IDs: no commit exists, so the pending key is left over, and a new rotation
		// removes it.
		out := h.mustRotate(t, "--yes")
		if !strings.Contains(out, "removed "+h.pending()) {
			t.Errorf("the next run:\n%s", out)
		}
		mustHoldValues(t, h.dir, h.keyAt(t))
	})
	t.Run("FILE stays", func(t *testing.T) {
		h := newRotHost(t)
		before := rowsText(sealedRows(t, h.dir))
		file := filepath.Join(t.TempDir(), "new.key")
		restore := breakCommit(t, h.dir)
		_, err := h.rotate(t, "--yes", "--out", file)
		restore()
		failedWith(t, "the commit with --out", err, "the re-seal failed", file+" stays", "--out "+file)
		left := mustRead(t, file)
		if len(left) != 32 {
			t.Fatalf("FILE holds %d bytes", len(left))
		}
		if got := rowsText(sealedRows(t, h.dir)); !slices.Equal(got, before) {
			t.Error("the failed commit changed a row")
		}
		mustHoldValues(t, h.dir, h.key)
		// The next run with the same FILE: its key sealed nothing, so it is refused because it exists (state F).
		_, err = h.rotate(t, "--yes", "--out", file)
		refusedWith(t, "the next run with the same FILE", err, file+" exists")
		if got := mustRead(t, file); !bytes.Equal(got, left) {
			t.Error("the refusal changed FILE")
		}
	})
}

// When the checkpoint of step 2 fails, the key file is not written. Without --out the message says to run the
// command again, which finishes the rotation. With --out it says to run it again with the same FILE and to keep the
// old key at the key path, and a second run reports the re-seal durable only after its checkpoint succeeded.
func TestRotateKeyAFailedCheckpoint(t *testing.T) {
	busy := errors.New("checkpoint database: another connection kept it busy")
	t.Run("without --out", func(t *testing.T) {
		h := newRotHost(t)
		fi, err := os.Stat(h.keyPath)
		if err != nil {
			t.Fatal(err)
		}
		out, err := rotateWith(t, h.config, failAt(rotateCheckpoint, busy), "--yes")
		failedWith(t, "the checkpoint", err, "the values are sealed under the new key", busy.Error(), "the key file was not written",
			"Run `flats secret rotate-key` again, which finishes the rotation")
		if after, err := os.Stat(h.keyPath); err != nil || !after.ModTime().Equal(fi.ModTime()) || !bytes.Equal(h.keyAt(t), h.key) {
			t.Fatalf("the key file was written although the checkpoint failed: %v", err)
		}
		if strings.Contains(out, "wrote the new key") {
			t.Errorf("the output:\n%s", out)
		}
		pending := mustRead(t, h.pending())
		mustHoldValues(t, h.dir, pending) // state C
		mustRefuseStart(t, h.config, "flats secret rotate-key")
		// A finish whose checkpoint fails stops without a change.
		_, err = rotateWith(t, h.config, failAt(rotateCheckpoint, busy))
		failedWith(t, "the checkpoint of the finish", err, "the rotation was not finished", "nothing was changed")
		if !bytes.Equal(h.keyAt(t), h.key) || !bytes.Equal(mustRead(t, h.pending()), pending) {
			t.Fatal("a finish whose checkpoint failed changed a key file")
		}
		h.mustRotate(t)
		if got := h.keyAt(t); !bytes.Equal(got, pending) {
			t.Fatal("the second run did not finish the rotation")
		}
		mustNotBeThere(t, h.pending())
		mustStart(t, h, rotSealed)
	})
	t.Run("with --out", func(t *testing.T) {
		h := newRotHost(t)
		file := filepath.Join(t.TempDir(), "new.key")
		want := []string{"the values are sealed under the new key", busy.Error(), "the re-seal is not durable yet",
			"Run `flats secret rotate-key --out " + file + "` again with the same file", "keep the old key at " + h.keyPath, "a loss of power can still take the re-seal back"}
		_, err := rotateWith(t, h.config, failAt(rotateCheckpoint, busy), "--yes", "--out", file)
		failedWith(t, "the checkpoint with --out", err, want...)
		newKey := mustRead(t, file)
		mustHoldValues(t, h.dir, newKey)
		if !bytes.Equal(h.keyAt(t), h.key) {
			t.Fatal("the key path changed")
		}
		// The second run with the same FILE makes no new key. While its checkpoint fails, it says that flats.db is
		// not durable, and does not report anything durable.
		out, err := rotateWith(t, h.config, failAt(rotateCheckpoint, busy), "--out", file)
		failedWith(t, "the checkpoint of the second run", err, "the command could not make flats.db durable", busy.Error(),
			"Run `flats secret rotate-key --out "+file+"` again with the same file", "keep the key at "+h.keyPath, "a loss of power can still take a commit back")
		if strings.Contains(out, "durable") || strings.Contains(out, "must be put at") {
			t.Errorf("the second run reported the re-seal durable although its checkpoint failed:\n%s", out)
		}
		out, err = h.rotate(t, "--out", file)
		if err != nil || !strings.Contains(out, "flats.db is durable now") || !strings.Contains(out, "The key of "+file+" must be put at "+h.keyPath) {
			t.Fatalf("the third run: %v\n%s", err, out)
		}
		if n := len(eventsOf(t, h.dir, store.AuditKeyRotate)); n != 1 {
			t.Errorf("%d key.rotate events", n)
		}
	})
}

// The checkpoint of step 2 is a real complete checkpoint: a second connection that holds a read transaction keeps
// it from completing, and the command then does not write the key file. The checkpoint waits out the busy timeout
// of the store, 5 s, so this test runs in parallel. It sets no package variable.
func TestRotateKeyCheckpointHeldUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	config, key, h := hostWithSecrets(t, dir)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, core.SecretKeyFile)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "flats.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reader, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := reader.QueryRowContext(ctx, "SELECT count(*) FROM secrets").Scan(&n); err != nil || n != 1 {
		t.Fatalf("before the rotation: %d secrets, %v", n, err)
	}
	_, err = runSecretKey(t, config, "rotate-key", "--yes")
	if err == nil || exitCode(err) != 1 || !strings.Contains(err.Error(), "checkpoint database") || !strings.Contains(err.Error(), "the key file was not written") {
		t.Fatalf("a rotation under a reader: %v", err)
	}
	if got, _ := os.ReadFile(keyPath); !bytes.Equal(got, key) {
		t.Fatal("the key file was written although the checkpoint did not complete")
	}
	pending, err := os.ReadFile(keyPath + ".next")
	if err != nil || len(pending) != 32 {
		t.Fatalf("the pending key: %d bytes, %v", len(pending), err)
	}
	if err := reader.Rollback(); err != nil {
		t.Fatal(err)
	}
	if out, err := runSecretKey(t, config, "rotate-key"); err != nil {
		t.Fatalf("the finish after the reader left: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(keyPath); !bytes.Equal(got, pending) {
		t.Fatal("the finish did not put the pending key in place")
	}
	st := openStore(t, dir)
	defer st.Close()
	if c, err := core.CheckKey(ctx, st, pending); err != nil || !c.All() || c.Total != 2 {
		t.Fatalf("the new key opens %+v, %v", c, err)
	}
}

// The three failures of step 3 (rule 16). A failed write and a failed comparison stop the command with status 1 in
// state C, and the message gives the two ways to finish: a second run, or a copy by hand and a second run. A failed
// removal of the pending key after a good write leaves state D: step 4 still runs, and the status is 1.
func TestRotateKeyFailuresOfStep3(t *testing.T) {
	twoWays := func(h *rotHost) []string {
		return []string{"The re-seal is durable", "the pending key " + h.pending() + " holds the key of the values",
			"remove the cause and run `flats secret rotate-key` again", "copy the bytes of " + h.pending() + " into " + h.keyPath + " by hand"}
	}
	t.Run("a failed write, finished by a second run", func(t *testing.T) {
		h := newRotHost(t)
		// After the checkpoint, something that is no file takes the place of the key file.
		_, err := rotateWith(t, h.config, func(p string) error {
			if p == rotateCheckpointed {
				if err := os.Remove(h.keyPath); err != nil {
					return err
				}
				return os.Mkdir(h.keyPath, 0o700)
			}
			return nil
		}, "--yes")
		failedWith(t, "the write of the key file", err, twoWays(h)...)
		if strings.Contains(err.Error(), "--out") {
			t.Errorf("the message names --out: %v", err)
		}
		pending := mustRead(t, h.pending())
		mustHoldValues(t, h.dir, pending) // state C
		mustRefuseStart(t, h.config, "flats secret rotate-key")
		// While the cause is there, the finish fails in the same way and changes nothing (rule 19).
		_, err = h.rotate(t)
		failedWith(t, "the finish over a folder at the key path", err, twoWays(h)...)
		if strings.Contains(err.Error(), "--out") {
			t.Errorf("the message of the finish names --out: %v", err)
		}
		if got := mustRead(t, h.pending()); !bytes.Equal(got, pending) {
			t.Fatal("the failed finish changed the pending key")
		}
		// The cause is removed, and a second run finishes.
		if err := os.Remove(h.keyPath); err != nil {
			t.Fatal(err)
		}
		h.mustRotate(t)
		if got := h.keyAt(t); !bytes.Equal(got, pending) {
			t.Fatal("the second run did not finish the rotation")
		}
		mustNotBeThere(t, h.pending())
		mustStart(t, h, rotSealed)
	})
	t.Run("a failed comparison, finished by a copy by hand", func(t *testing.T) {
		h := newRotHost(t)
		other := bytes.Repeat([]byte{9}, 32)
		// The file holds other bytes when the command reads it again.
		_, err := rotateWith(t, h.config, func(p string) error {
			if p == rotateKeyWriting {
				return os.WriteFile(h.keyPath, other, 0o600)
			}
			return nil
		}, "--yes")
		failedWith(t, "the comparison of the key file", err, append(twoWays(h), "does not hold the key that was written to it")...)
		pending := mustRead(t, h.pending())
		if !bytes.Equal(h.keyAt(t), other) {
			t.Fatal("the test did not damage the key file")
		}
		mustHoldValues(t, h.dir, pending) // state C
		mustRefuseStart(t, h.config, "flats secret rotate-key")
		// The copy by hand makes state D, and the next run removes the pending key and makes no new key.
		mustWrite(t, h.keyPath, pending)
		mustStart(t, h, rotSealed)
		out := h.mustRotate(t)
		if !strings.Contains(out, "removed the pending key "+h.pending()) {
			t.Errorf("the run after the copy by hand:\n%s", out)
		}
		mustNotBeThere(t, h.pending())
		if got := h.keyAt(t); !bytes.Equal(got, pending) {
			t.Fatal("the run after the copy by hand changed the key file")
		}
		if n := len(eventsOf(t, h.dir, store.AuditKeyRotate)); n != 1 {
			t.Errorf("%d key.rotate events", n)
		}
	})
	t.Run("a failed removal of the pending key", func(t *testing.T) {
		h := newRotHost(t)
		pw := passwordFile(t, recoveryPassword)
		mustRunSecretKey(t, h.config, "backup-key", "--password-file", pw)
		events := len(auditIn(t, h.dir))
		denied := errors.New("remove: permission denied")
		out, err := rotateWith(t, h.config, failAt(rotateRemovePending, denied), "--yes", "--password-file", pw)
		failedWith(t, "the removal of the pending key", err, denied.Error(), h.keyPath+" holds the new key", "the pending key "+h.pending()+" could not be removed",
			"Run `flats secret rotate-key` again, which removes it")
		// State D: the key is in place, and the pending key is a duplicate.
		newKey := h.keyAt(t)
		if bytes.Equal(newKey, h.key) || !bytes.Equal(mustRead(t, h.pending()), newKey) {
			t.Fatal("the state is not D")
		}
		mustHoldValues(t, h.dir, newKey)
		// Step 4 ran: the copy holds the new key, and its event is written.
		if w := core.RecoveryCopyWarning(h.dir, newKey); w != "" {
			t.Errorf("step 4 did not run: %s", w)
		}
		if !strings.Contains(out, "wrote "+filepath.Join(h.dir, core.RecoveryKeyFile)) {
			t.Errorf("the output:\n%s", out)
		}
		if got := auditSummary(newAudit(t, h.dir, events)); len(got) != 2 || !strings.HasPrefix(got[0], "key.rotate ") || !strings.HasPrefix(got[1], "key.backup ") {
			t.Errorf("the events: %q", got)
		}
		// The next run removes the pending key.
		h.mustRotate(t, "--password-file", pw)
		mustNotBeThere(t, h.pending())
		if got := h.keyAt(t); !bytes.Equal(got, newKey) {
			t.Fatal("the next run made a new key")
		}
	})
	// The file can be gone when the removal reports an error: the sync of the folder failed after it. The message
	// then says so, and does not say that the file could not be removed.
	t.Run("a removal of the pending key that is not durable", func(t *testing.T) {
		h := newRotHost(t)
		notSynced := fmt.Errorf("%w: sync: input/output error", core.ErrRemovedNotSynced)
		_, err := rotateWith(t, h.config, failAt(rotateRemovePending, notSynced), "--yes")
		failedWith(t, "the removal of the pending key", err, "input/output error", h.keyPath+" holds the new key",
			"the pending key "+h.pending()+" was removed, but its removal is not durable", "after a loss of power")
		if strings.Contains(err.Error(), "could not be removed") {
			t.Errorf("the message says that the file could not be removed: %v", err)
		}
	})
}

// A finish with nothing at the key path creates the key file with mode 0600, and the file is there and durable
// before the pending key is removed (rule 19). It works at a configured path too, where the host never creates a
// key.
func TestRotateKeyFinishCreatesAMissingKeyFile(t *testing.T) {
	for name, place := range map[string]func(t *testing.T, h *rotHost) *rotHost{
		"the default path":  func(t *testing.T, h *rotHost) *rotHost { return h },
		"a configured path": func(t *testing.T, h *rotHost) *rotHost { return h.outside(t) },
	} {
		t.Run(name, func(t *testing.T) {
			h := place(t, newRotHost(t))
			pending := stateC(t, h, rotateCheckpointed, "nothing")
			seen := false
			out, err := rotateWith(t, h.config, func(p string) error {
				if p != rotateKeyWritten {
					return nil
				}
				seen = true
				fi, err := os.Stat(h.keyPath)
				if err != nil || ostest.WrongMode(fi, 0o600) || !bytes.Equal(mustRead(t, h.keyPath), pending) {
					t.Errorf("the key file before the removal of the pending key: %v, %v", fi, err)
				}
				if got := mustRead(t, h.pending()); !bytes.Equal(got, pending) {
					t.Error("the pending key is gone before the key file was written")
				}
				return nil
			})
			if err != nil || !seen {
				t.Fatalf("the finish: %v (the hook ran: %v)\n%s", err, seen, out)
			}
			mustNotBeThere(t, h.pending())
			if !strings.Contains(out, "created "+h.keyPath) || !strings.Contains(out, "the owner and the access that the host needs") {
				t.Errorf("the output:\n%s", out)
			}
			if got := listed(t, filepath.Dir(h.keyPath)); slices.ContainsFunc(got, func(n string) bool { return strings.HasPrefix(n, ".secret-key-") }) {
				t.Errorf("the folder of the key holds a temporary file: %v", got)
			}
			mustStart(t, h, rotSealed)
		})
	}
}

// The two failures of step 4 (rule 20). The rotation is complete, the status is 1, and the message says what is
// true: the copy was not written and holds the old key, or the copy holds the new key and its ID file is out of
// date. Both name `flats secret backup-key`. With --out the message says to run it after the key is in place.
func TestRotateKeyFailuresOfStep4(t *testing.T) {
	password := strings.TrimSpace(recoveryPassword)
	full := errors.New("no space left on device")
	for name, tc := range map[string]struct {
		hook      func(h *rotHost) func(string) error
		want      []string
		holdsNew  bool
		prepare   func(t *testing.T, h *rotHost)
		wantCause string
	}{
		"the copy is not written": {
			hook:      func(*rotHost) func(string) error { return failAt(rotateWriteCopy, full) },
			want:      []string{"the rotation is complete", "was not written and still holds the old key", "`flats secret backup-key`", "it writes both files"},
			wantCause: full.Error(),
		},
		"the ID file is not written": {
			hook:      func(*rotHost) func(string) error { return failAt(rotateWriteCopyID, full) },
			want:      []string{"the rotation is complete", "holds the new key", "its ID file " + core.RecoveryKeyIDFile + " was not written and is out of date", "`flats secret backup-key`"},
			holdsNew:  true,
			wantCause: full.Error(),
		},
		// The same failure without the hook: a folder that is not empty is at the name of the ID file, so the
		// rename of the real write fails.
		"the ID file cannot be replaced": {
			hook: func(*rotHost) func(string) error { return nil },
			prepare: func(t *testing.T, h *rotHost) {
				id := filepath.Join(h.dir, core.RecoveryKeyIDFile)
				if err := os.Remove(id); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(id, "inside"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want:     []string{"the rotation is complete", "holds the new key", "was not written and is out of date"},
			holdsNew: true,
		},
	} {
		for _, withOut := range []bool{false, true} {
			sub := name
			if withOut {
				sub += " with --out"
			}
			t.Run(sub, func(t *testing.T) {
				h := newRotHost(t)
				pw := passwordFile(t, recoveryPassword)
				mustRunSecretKey(t, h.config, "backup-key", "--password-file", pw)
				if tc.prepare != nil {
					tc.prepare(t, h)
				}
				events := len(eventsOf(t, h.dir, store.AuditKeyBackup))
				args := []string{"--yes", "--password-file", pw}
				file := filepath.Join(t.TempDir(), "new.key")
				if withOut {
					args = append(args, "--out", file)
				}
				out, err := rotateWith(t, h.config, tc.hook(h), args...)
				failedWith(t, "step 4", err, append(tc.want, tc.wantCause)...)
				if after := "after the new key is at the key path, because it refuses before"; strings.Contains(err.Error(), after) != withOut {
					t.Errorf("the message with --out = %v: %v", withOut, err)
				}
				// The rotation is complete.
				newKey := h.keyAt(t)
				if withOut {
					newKey = mustRead(t, file)
				} else {
					mustNotBeThere(t, h.pending())
				}
				mustHoldValues(t, h.dir, newKey)
				if !strings.Contains(out, "sealed 3 flat secrets and 2 operator keys again") || !strings.Contains(out, "opens only with the old key") {
					t.Errorf("the output of a rotation whose step 4 failed:\n%s", out)
				}
				holds, err := core.OpenKeyCopy(mustRead(t, filepath.Join(h.dir, core.RecoveryKeyFile)), password)
				if err != nil {
					t.Fatal(err)
				}
				if want := map[bool][]byte{false: h.key, true: newKey}[tc.holdsNew]; !bytes.Equal(holds, want) {
					t.Errorf("the recovery copy holds the key %s", core.KeyID(holds))
				}
				if w := core.RecoveryCopyWarning(h.dir, newKey); w == "" {
					t.Error("the start would not warn about the recovery copy")
				}
				if n := len(eventsOf(t, h.dir, store.AuditKeyBackup)); n != events {
					t.Errorf("a key.backup event was written although step 4 failed")
				}
			})
		}
	}
}

// A key.backup event that cannot be recorded fails the command with status 1, and the copy stays (rule 31, and rule
// 17 of the audit spec).
func TestRotateKeyKeepsTheCopyWhenItsEventCannotBeRecorded(t *testing.T) {
	h := newRotHost(t)
	pw := passwordFile(t, recoveryPassword)
	mustRunSecretKey(t, h.config, "backup-key", "--password-file", pw)
	backups := len(eventsOf(t, h.dir, store.AuditKeyBackup))
	sqlExec(t, h.dir, `CREATE TRIGGER backup_event_fails BEFORE INSERT ON audit_events WHEN NEW.kind='key.backup' BEGIN SELECT RAISE(ABORT, 'audit write failed'); END`)
	_, err := h.rotate(t, "--yes", "--password-file", pw)
	sqlExec(t, h.dir, `DROP TRIGGER backup_event_fails`)
	failedWith(t, "the append of key.backup", err, "the recovery copy "+filepath.Join(h.dir, core.RecoveryKeyFile)+" was written", "the audit event key.backup was not recorded", "audit write failed")
	newKey := h.keyAt(t)
	mustHoldValues(t, h.dir, newKey)
	if w := core.RecoveryCopyWarning(h.dir, newKey); w != "" {
		t.Errorf("the copy does not stay: %s", w)
	}
	if n := len(eventsOf(t, h.dir, store.AuditKeyRotate)); n != 1 {
		t.Errorf("%d key.rotate events", n)
	}
	if n := len(eventsOf(t, h.dir, store.AuditKeyBackup)); n != backups {
		t.Errorf("%d key.backup events, want the %d from before", n, backups)
	}
}

// cancelling answers each question and interrupts at the same time, as a Ctrl-C right after the answer does.
type cancelling struct {
	cancel context.CancelFunc
	asked  int
}

func (p *cancelling) hidden(string) (string, error) {
	p.asked++
	p.cancel()
	return strings.TrimSpace(recoveryPassword), nil
}

func (p *cancelling) line(string) (string, error) {
	p.asked++
	p.cancel()
	return "rotate", nil
}

// rotateInterrupted runs the command with a context that the terminal or the hook can cancel.
func rotateInterrupted(t *testing.T, h *rotHost, atPrompt bool, atPoint string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if atPrompt {
		old := openTerminal
		openTerminal = func(context.Context, io.Reader, io.Writer) prompter { return &cancelling{cancel: cancel} }
		defer func() { openTerminal = old }()
	}
	old := rotateHook
	interrupted := false
	rotateHook = func(p string) error {
		if p == atPoint {
			interrupted = true
			cancel()
		}
		return nil
	}
	defer func() { rotateHook = old }()
	var out bytes.Buffer
	err := SecretKeyCommand(ctx, append([]string{"rotate-key", "--config", h.config}, args...), strings.NewReader(""), &out)
	if atPoint != "" && !interrupted {
		t.Fatalf("the command did not reach the point %q, so the test made no interrupt: %v\n%s", atPoint, err, out.String())
	}
	return out.String(), err
}

// An interrupt before step 1 writes nothing, also after the password and the confirmation were given. After step 1
// has begun, the command completes its four steps (rule 16).
func TestRotateKeyAndAnInterrupt(t *testing.T) {
	h := newRotHost(t)
	pw := passwordFile(t, recoveryPassword)
	mustRunSecretKey(t, h.config, "backup-key", "--password-file", pw)
	rows := rowsText(sealedRows(t, h.dir))
	names := listed(t, h.dir)
	copyBefore := mustRead(t, filepath.Join(h.dir, core.RecoveryKeyFile))
	events := len(auditIn(t, h.dir))
	for name, args := range map[string][]string{
		"at the password":     {"--yes"},
		"at the confirmation": {"--password-file", pw},
		"at both":             nil,
	} {
		_, err := rotateInterrupted(t, h, true, "", args...)
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "nothing was written") {
			t.Fatalf("an interrupt %s: %v", name, err)
		}
		if got := rowsText(sealedRows(t, h.dir)); !slices.Equal(got, rows) {
			t.Fatalf("an interrupt %s changed a row", name)
		}
		if got := listed(t, h.dir); !slices.Equal(got, names) {
			t.Fatalf("an interrupt %s left %v in the data directory, which held %v", name, got, names)
		}
		if !bytes.Equal(h.keyAt(t), h.key) || !bytes.Equal(mustRead(t, filepath.Join(h.dir, core.RecoveryKeyFile)), copyBefore) {
			t.Fatalf("an interrupt %s changed the key or the recovery copy", name)
		}
		if n := len(auditIn(t, h.dir)); n != events {
			t.Fatalf("an interrupt %s wrote %d events", name, n-events)
		}
	}

	// After step 1 has begun, an interrupt does not stop steps 2 to 4.
	out, err := rotateInterrupted(t, h, false, rotateNewKeyWritten, "--yes", "--password-file", pw)
	if err != nil {
		t.Fatalf("an interrupt after step 1 began: %v\n%s", err, out)
	}
	newKey := h.keyAt(t)
	if bytes.Equal(newKey, h.key) {
		t.Fatal("step 3 did not run")
	}
	mustNotBeThere(t, h.pending())
	mustHoldValues(t, h.dir, newKey)
	if w := core.RecoveryCopyWarning(h.dir, newKey); w != "" {
		t.Errorf("step 4 did not run: %s", w)
	}
	if got := auditSummary(newAudit(t, h.dir, events)); len(got) != 2 || !strings.HasPrefix(got[0], "key.rotate ") || !strings.HasPrefix(got[1], "key.backup ") {
		t.Errorf("the events after the interrupt: %q", got)
	}
}

// A finish follows the same rule: an interrupt before its first write writes nothing, and after that write has
// begun the finish completes.
func TestRotateKeyFinishAndAnInterrupt(t *testing.T) {
	h := newRotHost(t)
	pw := passwordFile(t, recoveryPassword)
	mustRunSecretKey(t, h.config, "backup-key", "--password-file", pw)
	stopRotation(t, h, rotateCheckpointed, "--yes", "--password-file", pw)
	pending := mustRead(t, h.pending())
	copyBefore := mustRead(t, filepath.Join(h.dir, core.RecoveryKeyFile))
	events := len(auditIn(t, h.dir))

	// Before the first write: at the password, and at the checkpoint, which is no write of a key.
	for name, run := range map[string]func() (string, error){
		"at the password": func() (string, error) { return rotateInterrupted(t, h, true, "") },
		"at the checkpoint": func() (string, error) {
			return rotateInterrupted(t, h, false, rotateCheckpointed, "--password-file", pw)
		},
	} {
		_, err := run()
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "nothing was written") {
			t.Fatalf("an interrupt %s: %v", name, err)
		}
		if !bytes.Equal(h.keyAt(t), h.key) || !bytes.Equal(mustRead(t, h.pending()), pending) || !bytes.Equal(mustRead(t, filepath.Join(h.dir, core.RecoveryKeyFile)), copyBefore) {
			t.Fatalf("an interrupt %s changed a file", name)
		}
		if n := len(auditIn(t, h.dir)); n != events {
			t.Fatalf("an interrupt %s wrote an event", name)
		}
	}

	// After the first write has begun.
	out, err := rotateInterrupted(t, h, false, rotateKeyWriting, "--password-file", pw)
	if err != nil {
		t.Fatalf("an interrupt inside the write of the key file: %v\n%s", err, out)
	}
	if got := h.keyAt(t); !bytes.Equal(got, pending) {
		t.Fatal("the finish did not write the key file")
	}
	mustNotBeThere(t, h.pending())
	if w := core.RecoveryCopyWarning(h.dir, pending); w != "" {
		t.Errorf("step 4 did not run: %s", w)
	}
	if got := auditSummary(newAudit(t, h.dir, events)); len(got) != 1 || !strings.HasPrefix(got[0], "key.backup ") {
		t.Errorf("the events of the finish: %q", got)
	}
}
