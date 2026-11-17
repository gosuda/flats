package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/durable"
	"github.com/gosuda/flats/internal/store"
)

// `flats secret rotate-key` (docs/spec/secret-key-rotation.md). The command makes a new host key and seals every
// stored value again under it, in the order of rule 7. Nothing before the first of the four steps of rule 16 writes
// a file or a row. This file holds the order of the steps. The file writes, the classification of the pending key
// and the re-seal are in internal/core.

// The points of a rotation that rotateHook hears about, in the order of the steps. At a point that is named for an
// operation that has not run yet (it starts with a verb), an error of the hook stands for the failure of that
// operation. At every other point an error stops the command there, as a stop of the process would.
const (
	rotateWriteNewKey    = "write-new-key"   // step 1, after the removal of a pending key that was left over: a stop
	rotateNewKeyWritten  = "new-key-written" // step 1 is durable: the pending key, or FILE, holds the new key
	rotateCommitted      = "committed"       // the transaction of step 2 committed
	rotateCheckpoint     = "checkpoint"      // an error is the failure of the checkpoint of step 2
	rotateCheckpointed   = "checkpointed"    // the re-seal is durable
	rotateKeyWriting     = "key-writing"     // inside step 3: the key file is written and synced, and not read again yet
	rotateKeyWritten     = "key-written"     // the key file holds the new key
	rotateRemovePending  = "remove-pending"  // an error is the failure of the removal of the pending key
	rotatePendingRemoved = "pending-removed" // step 3 is complete
	rotateWriteCopy      = "write-copy"      // an error is the failure of the write of the recovery copy
	rotateCopyWritten    = "copy-written"    // between the two writes of step 4
	rotateWriteCopyID    = "write-copy-id"   // an error is the failure of the write of the ID file
)

// rotateHook is nil in production. A test sets it to stop or to fail the command at a point.
var rotateHook func(point string) error

// reached tells the hook that the command is at point.
func reached(point string) error {
	if rotateHook == nil {
		return nil
	}
	return rotateHook(point)
}

// hooked runs op, unless the hook fails the operation that point names.
func hooked(point string, op func() error) error {
	if err := reached(point); err != nil {
		return err
	}
	return op()
}

// rotation is one run of the command after rule 6.
type rotation struct {
	*secretKeyCmd
	st           *store.Store
	dataDir      string
	keyFile      core.KeyFile
	passwordFile string
	// write is the context of every write after the first one began: an interrupt does not stop it (rule 16).
	write context.Context
}

func (k *secretKeyCmd) rotate(args []string) error {
	f := k.flags("rotate-key")
	outFlag := f.String("out", "", "write the new key to this new file and leave the key path alone")
	passwordFile := f.String("password-file", "", "read the recovery password from this file")
	yes := f.Bool("yes", false, "rotate without asking")
	if err := k.parseFlags(args); err != nil {
		return err
	}
	// Point 1 of rule 7: the host lock, no running host, and the database of this configuration (rule 6).
	dataDir, keyFile, st, done, err := k.openOffline()
	if err != nil {
		return err
	}
	defer done()
	r := &rotation{secretKeyCmd: k, st: st, dataDir: dataDir, keyFile: keyFile, passwordFile: *passwordFile, write: context.WithoutCancel(k.ctx)}
	keyPath, pendingPath := keyFile.Path(), keyFile.PendingPath()
	// The checks of rule 3 of the key path spec, for a new rotation (rule 8) and for a finish (rule 19).
	if err := keyFile.Check(); err != nil {
		return &ActionError{err}
	}
	ids, err := st.SealedKeyIDs(k.ctx)
	if err != nil {
		return err
	}

	// Point 2: a rotation that stopped is finished, and the points below do not apply (rule 19).
	pending, err := keyFile.Pending(ids)
	if err != nil {
		return rotationError(err)
	}
	if pending.State == core.PendingWaits || pending.State == core.PendingDuplicate {
		if *outFlag != "" {
			return actionf("%s is the pending key of a rotation that stopped, and --out starts no second one: run `flats secret rotate-key` without --out, which finishes that rotation", pendingPath)
		}
		if pending.State == core.PendingWaits {
			return r.finishWaiting(pending)
		}
		return r.finishDuplicate(pending)
	}
	var out outTarget
	if *outFlag != "" {
		if out, err = lookAtOut(*outFlag, dataDir, keyFile, ids); err != nil {
			return err
		}
		if out.state == outFinishes {
			return r.finishOut(out)
		}
	}

	// Point 3: the key path holds a valid key (rule 8), which opens every sealed value (rule 9).
	key, err := r.hostKey()
	if err != nil {
		return err
	}
	check, err := core.CheckKey(k.ctx, st, key)
	if err != nil {
		return err
	}
	if !check.All() {
		return unopenedRefusal("the key at "+keyPath, check)
	}

	// Point 4: the place of the new key.
	target := "into " + keyPath
	if *outFlag != "" {
		if out.refusal != nil {
			return out.refusal
		}
		target = "to the new file " + out.path
	} else {
		if err := keyFile.CheckWritable(); err != nil {
			return actionf("cannot open the key file %s for writing: %v; nothing was written. Make the file writable for the time of the rotation, or write the new key to a new file with --out FILE and put that file at the key path", keyPath, err)
		}
		if err := keyFile.CheckPendingPlace(); err != nil {
			return actionf("%v; nothing was written. Write the new key to a new file with --out FILE, and put that file at the key path", err)
		}
	}

	// Point 5: the recovery copy and its password (rule 20).
	password, hasCopy, err := r.recoveryPassword()
	if err != nil {
		return err
	}

	// Point 6: the confirmation (rule 10).
	k.printf("This seals the %s in flats.db again under a new key, and writes the new key %s.\n", counted(check.Total, "sealed value", "sealed values"), target)
	k.printf("It cannot be reversed: Flats does not keep the old key, and after the rotation the old key opens nothing in flats.db.\n")
	if pending.State == core.PendingLeftOver {
		k.printf("%s is left over from a rotation that stopped before it changed a value. This rotation removes it.\n", pendingPath)
	}
	k.printf("%s\n", oldKeyNoteBefore)
	if !*yes {
		p := openTerminal(k.ctx, k.in, k.out)
		if p == nil {
			return usagef("no terminal to confirm on: run again with --yes to rotate the key")
		}
		answer, err := p.line("Type rotate to go on: ")
		if err != nil {
			return err
		}
		if strings.TrimSpace(answer) != "rotate" {
			k.printf("Nothing was changed.\n")
			return nil
		}
	}

	// Point 7: the new key, and the new recovery copy, encrypted in memory.
	newKey, err := core.NewSecretKey()
	if err != nil {
		return err
	}
	copyData, err := sealCopy(newKey, password, hasCopy)
	if err != nil {
		return err
	}
	if err := k.ctx.Err(); err != nil {
		return interrupted(err)
	}

	// Point 8, step 1: the new key is durable in the pending key file, or in FILE. From here the command completes,
	// also when an interrupt arrives.
	if pending.State == core.PendingLeftOver {
		if err := keyFile.RemovePending(); err != nil {
			return fmt.Errorf("remove the pending key %s, which is left over from a rotation that stopped: %w; nothing else was changed", pendingPath, err)
		}
		k.printf("removed %s, which was left over from a rotation that stopped before it changed a value\n", pendingPath)
	}
	if err := reached(rotateWriteNewKey); err != nil {
		return err
	}
	holder, again := pendingPath, "`flats secret rotate-key`"
	if *outFlag != "" {
		holder, again = out.path, fmt.Sprintf("`flats secret rotate-key --out %s`, with the same file,", out.path)
		if err := writeKeyOut(out.path, newKey); err != nil {
			return err
		}
	} else if err := keyFile.WritePending(newKey); err != nil {
		return actionf("cannot create the pending key %s: %v; no value was changed. Write the new key to a new file with --out FILE, and put that file at the key path", pendingPath, err)
	}
	if err := reached(rotateNewKeyWritten); err != nil {
		return err
	}

	// Step 2: the re-seal in one transaction, and the checkpoint that makes it durable. A transaction that fails
	// removes neither file, because the process cannot always know whether a commit exists (rule 15).
	secrets, keys, err := core.Reseal(r.write, st, key, newKey, *outFlag != "", commandAudit())
	if err != nil {
		return fmt.Errorf("the re-seal failed: %w. %s stays and holds the new key. Run %s again: it reads the key IDs of the values to tell whether the re-seal took place", err, holder, again)
	}
	if err := reached(rotateCommitted); err != nil {
		return err
	}
	k.printf("sealed %s and %s again: the old key has the key ID %s, and the new key has the key ID %s\n",
		counted(secrets, "flat secret", "flat secrets"), counted(keys, "operator key", "operator keys"), core.KeyID(key), core.KeyID(newKey))
	if err := r.checkpoint(); err != nil {
		if *outFlag != "" {
			return r.notDurableOut(err, out.path)
		}
		return fmt.Errorf("the values are sealed under the new key, but %w, so the key file was not written. Run `flats secret rotate-key` again, which finishes the rotation", err)
	}

	if *outFlag != "" {
		// With --out there is no step 3: the operator puts the key in place.
		k.printf("wrote the new key to %s; on Unix the file has mode 0600 and belongs to the user that ran this command\n", out.path)
		k.printf("%s\n", putInPlace(out.path, keyPath))
		err := r.writeCopy(newKey, copyData, true)
		k.printf("%s\n", oldKeyNoteAfter)
		return err
	}
	// Steps 3 and 4.
	return r.placeAndCopy(newKey, copyData, true)
}

// sealCopy encrypts key under the recovery password at the work factor of `flats secret backup-key`, before the
// first write of the command. Without a recovery copy it returns nil.
func sealCopy(key []byte, password string, hasCopy bool) ([]byte, error) {
	if !hasCopy {
		return nil, nil
	}
	return core.SealKeyCopy(key, password, recoveryWorkFactor)
}

// checkpoint makes every commit of flats.db durable (step 2 of rule 16).
func (r *rotation) checkpoint() error {
	if err := hooked(rotateCheckpoint, func() error { return r.st.Sync(r.write) }); err != nil {
		return err
	}
	return reached(rotateCheckpointed)
}

// notDurableOut is the failure of the checkpoint with --out (step 2 of rule 16, and the third case of rule 19).
func (r *rotation) notDurableOut(err error, outPath string) error {
	return fmt.Errorf("the values are sealed under the new key, but %w, so the re-seal is not durable yet. Run `flats secret rotate-key --out %s` again with the same file, and keep the old key at %s until that run succeeds, because a loss of power can still take the re-seal back", err, outPath, r.keyFile.Path())
}

// hostKey reads the key at the key path, which must hold a valid key (rule 8).
func (r *rotation) hostKey() ([]byte, error) {
	keyPath := r.keyFile.Path()
	if _, err := os.Lstat(keyPath); errors.Is(err, fs.ErrNotExist) {
		// The start's own message for a missing key, with the key ID of the values when they carry one (rule 23).
		err := core.CheckSecretKey(r.ctx, r.keyFile, r.st)
		switch {
		case err == nil:
			return nil, actionf("%s is missing, and flats.db holds no sealed value: there is no key to rotate yet, and the next start creates one", keyPath)
		case errors.Is(err, core.ErrKeyLocation) && r.keyFile.Configured():
			return nil, actionf("%v. A key that only the service process sees, such as a systemd credential or a Docker secret in the service container, is missing for this command too: \"Rotating the key\" in docs/configuration.md gives the steps for such a key", err)
		case errors.Is(err, core.ErrSecretKeyMissing), errors.Is(err, core.ErrKeyLocation):
			return nil, &ActionError{err}
		}
		return nil, rotationError(err)
	}
	key, err := r.keyFile.ReadUnchecked()
	switch {
	case err == nil:
		return key, nil
	case keyUnusable(err, keyPath):
		return nil, actionf("%v: move %s away first, because the next start would stop on it", err, keyPath)
	}
	return nil, err // any other failure to read the key: status 1
}

// unopenedRefusal is the refusal of rule 9 for a key that does not open every sealed value. subject names the key.
func unopenedRefusal(subject string, c core.KeyCheck) error {
	var keys, secrets []string
	for _, n := range c.Unopened {
		if n.Flat == "" {
			keys = append(keys, n.Name)
		} else {
			secrets = append(secrets, n.String())
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s opens %d of the %d sealed values in flats.db, and a rotation would leave the others under a key that no file holds; nothing was written", subject, c.Opened, c.Total)
	if len(keys) > 0 {
		fmt.Fprintf(&b, ". It does not open the operator keys %s", strings.Join(keys, ", "))
	}
	if len(secrets) > 0 {
		fmt.Fprintf(&b, ". It does not open %s", strings.Join(secrets, ", "))
	}
	b.WriteString(". Ways out: put the right key in place with `flats secret restore-key`")
	if c.Opened > 0 {
		b.WriteString("; or remove the values that it does not open, an operator key with `flats key rm`, and a flat secret with `flats secret rm` after a start of the host")
	} else {
		b.WriteString("; or, with no such key left, delete the stored values with `flats secret reset`")
	}
	if n := len(c.UnopenedKeyIDs); n > 0 {
		carry := "the key ID " + c.UnopenedKeyIDs[0]
		if n > 1 {
			carry = "the key IDs " + strings.Join(c.UnopenedKeyIDs, ", ")
		}
		fmt.Fprintf(&b, ". The values that do not open carry %s: the file that an earlier `flats secret rotate-key --out` wrote can hold that key", carry)
	}
	return actionf("%s", b.String())
}

// counted is "1 thing" or "N things".
func counted(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// The note of rule 29, before and after the rotation.
const (
	oldKeyNoteBefore = "A copy of flats.db from before the rotation opens only with the old key. If such a copy must stay readable without a key of its own, copy the old key away first. Make a real copy and not a link, because every name of the key file gets the new key."
	oldKeyNoteAfter  = "A copy of flats.db from before this rotation opens only with the old key, which Flats did not keep. Such a copy stays readable with a key of its own, as in a backup with its secret.key, or with a copy of the old key that you made before the rotation: a real copy and not a link, because every name of the key file got the new key."
)

// outState says what the file that --out names is.
type outState int

const (
	outNew      outState = iota // nothing is there: a new rotation creates it
	outFinishes                 // it holds the key of every sealed value, and the key path does not: rule 19
	outRefused                  // anything else: the refusal of rule 14
)

// outTarget is the file of --out as lookAtOut found it. key is set when the file finishes a rotation. refusal is
// the refusal of rule 14, which belongs to point 4 of rule 7, after the checks of the key.
type outTarget struct {
	path    string
	state   outState
	key     []byte
	refusal error
}

// lookAtOut looks at the file that --out names and writes nothing. A file that does not exist gets the checks of
// `flats secret restore-key --out` (rule 14). A file that exists finishes a rotation when it is a valid key, is not
// the key at the key path, and every sealed value carries its key ID (rule 19). Every other file that exists is a
// refusal, which says which case it is.
func lookAtOut(flag, dataDir string, keyFile core.KeyFile, ids store.KeyIDCounts) (outTarget, error) {
	path, err := cleanPath(flag)
	if err != nil {
		return outTarget{}, actionf("--out: %v", err)
	}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		if _, err := keyOutFile(flag, dataDir); err != nil {
			return outTarget{path: path, state: outRefused, refusal: err}, nil
		}
		return outTarget{path: path, state: outNew}, nil
	}
	other := actionf("--out: %s exists, and it is not the file of a rotation that this command can finish: it is no valid key, or its key sealed none or not all of the stored values. Remove it, or name another file", path)
	fileKey, ok := readKeyAt(path)
	if !ok {
		return outTarget{path: path, state: outRefused, refusal: other}, nil
	}
	if current, err := keyFile.ReadUnchecked(); err == nil && subtle.ConstantTimeCompare(current, fileKey) == 1 {
		return outTarget{path: path, state: outRefused,
			refusal: actionf("--out: %s holds the key that %s holds too, so the key is in place: remove %s", path, keyFile.Path(), path)}, nil
	}
	if id, single := ids.Single(); single && id == core.KeyID(fileKey) {
		return outTarget{path: path, state: outFinishes, key: fileKey}, nil
	}
	return outTarget{path: path, state: outRefused, refusal: other}, nil
}

// readKeyAt reads the file at path when it is a regular file of 32 bytes, following a link as a read of the host
// key does. ok is false for anything else, and for a file that cannot be read.
func readKeyAt(path string) (key []byte, ok bool) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil || len(b) != 32 {
		return nil, false
	}
	return b, true
}

// writeKeyOut creates the file of --out with key (rule 14): a create that fails when the name exists, mode 0600,
// the 32 bytes, a sync of the file and then of its folder. A failure before the file is complete removes it.
func writeKeyOut(path string, key []byte) error {
	if err := writeNewFile(path, key); err != nil {
		if os.IsExist(err) {
			return actionf("--out: %s appeared while the command ran; no value was changed. Name a new file", path)
		}
		return fmt.Errorf("--out %s: %w; no value was changed", path, err)
	}
	if err := durable.SyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("--out %s: sync its folder: %w; no value was changed. The file stays and holds a key that sealed nothing: remove it before the next run", path, err)
	}
	return nil
}

// recoveryPassword is point 5 of rule 7 (rule 20). When a recovery copy is in the data directory, it reads the copy,
// asks for the recovery password once and opens the copy with it. The password is used as it is: the operator chose
// it for the copy that exists. hasCopy is false when nothing is at the name of the copy.
func (r *rotation) recoveryPassword() (password string, hasCopy bool, err error) {
	copyPath := filepath.Join(r.dataDir, core.RecoveryKeyFile)
	if _, err := os.Lstat(copyPath); errors.Is(err, fs.ErrNotExist) {
		if r.passwordFile != "" {
			return "", false, actionf("--password-file: there is no recovery copy at %s to make current, so the command takes no password. Run it without --password-file, and run `flats secret backup-key` afterwards to make a copy", copyPath)
		}
		return "", false, nil
	}
	lost := fmt.Sprintf("If the recovery password is lost, remove %s, which opens nothing without its password, rotate, and then run `flats secret backup-key`", copyPath)
	data, err := readLimited(copyPath, core.MaxRecoveryFileSize)
	if err != nil {
		return "", false, actionf("read the recovery copy: %v; nothing was written. %s", err, lost)
	}
	if password, err = r.password(r.passwordFile, false); err != nil {
		return "", false, err
	}
	if _, err := core.OpenKeyCopy(data, password); err != nil {
		return "", false, actionf("%s: %v; nothing was written. %s", copyPath, err, lost)
	}
	return password, true, nil
}

// writeCopy is step 4 of rule 16: it writes the recovery copy of key and then its ID file, and appends the
// key.backup event (rules 20 and 31). copyData is nil when the data directory holds no recovery copy. The rotation
// is complete when it runs, so each failure says what is true then, and none of them undoes anything.
func (r *rotation) writeCopy(key, copyData []byte, out bool) error {
	when := ""
	if out {
		when = " after the new key is at the key path, because it refuses before"
	}
	copyPath := filepath.Join(r.dataDir, core.RecoveryKeyFile)
	if copyData == nil {
		r.printf("there is no recovery copy in %s: run `flats secret backup-key`%s to make one for the new key\n", r.dataDir, when)
		return nil
	}
	err := hooked(rotateWriteCopy, func() error {
		_, err := core.WriteKeyCopyFile(r.dataDir, copyData)
		return err
	})
	if err != nil {
		return fmt.Errorf("the rotation is complete, but the recovery copy %s was not written and still holds the old key: %w. Run `flats secret backup-key`%s: it writes both files", copyPath, err, when)
	}
	if err := reached(rotateCopyWritten); err != nil {
		return err
	}
	if err := hooked(rotateWriteCopyID, func() error { return core.WriteKeyCopyID(r.dataDir, key) }); err != nil {
		return fmt.Errorf("the rotation is complete, and the recovery copy %s holds the new key, but its ID file %s was not written and is out of date: %w. Run `flats secret backup-key`%s: it writes both files", copyPath, core.RecoveryKeyIDFile, err, when)
	}
	r.printf("wrote %s and its key ID in %s again: the recovery password opens the new key\n", copyPath, core.RecoveryKeyIDFile)
	// The event follows the work, and the work stays when the event cannot be written (rule 17 of the audit spec).
	ev := commandEvent(store.AuditKeyBackup, map[string]any{"key_id": core.KeyID(key), "out": false})
	if err := r.st.AppendAudit(r.write, ev); err != nil {
		return fmt.Errorf("the recovery copy %s was written, and the audit event %s was not recorded: %w", copyPath, ev.Kind, err)
	}
	return nil
}

// notPlaced is the failure of step 3 of rule 16, and of the check of the key file before it in a finish (rule 19):
// the re-seal is durable, and the pending key holds the key of the values. It names the two ways to finish and not
// --out, which a finish refuses.
func (r *rotation) notPlaced(cause error) error {
	keyPath, pendingPath := r.keyFile.Path(), r.keyFile.PendingPath()
	return fmt.Errorf("write the new key into %s: %w. The re-seal is durable, and the pending key %s holds the key of the values. To finish, remove the cause and run `flats secret rotate-key` again. Or copy the bytes of %s into %s by hand and run the command again, which then removes the pending key",
		keyPath, cause, pendingPath, pendingPath, keyPath)
}

// placeAndCopy is steps 3 and 4 of rule 16 for the pending key. With place it writes key into the key file, where
// the file is, and creates the file when nothing is there (rule 19). It then removes the pending key and writes the
// recovery copy. A pending key that cannot be removed leaves the key in place: step 4 still runs, and the error
// names the file.
func (r *rotation) placeAndCopy(key, copyData []byte, place bool) error {
	keyPath, pendingPath := r.keyFile.Path(), r.keyFile.PendingPath()
	if place {
		_, lerr := os.Lstat(keyPath)
		created := errors.Is(lerr, fs.ErrNotExist)
		if err := r.keyFile.WriteInPlace(key, func() error { return reached(rotateKeyWriting) }); err != nil {
			return r.notPlaced(err)
		}
		if err := reached(rotateKeyWritten); err != nil {
			return err
		}
		if created {
			owner := ""
			if u := operatorUser(); u != "" {
				owner = " (" + u + ")"
			}
			r.printf("created %s with the new key and mode 0600. It belongs to the user that ran this command%s: give it the owner and the access that the host needs before you start the host\n", keyPath, owner)
		} else {
			r.printf("wrote the new key into %s, which keeps its owner and its access\n", keyPath)
		}
	}
	var late []error
	if err := hooked(rotateRemovePending, r.keyFile.RemovePending); err != nil {
		late = append(late, fmt.Errorf("%s holds the new key, but the pending key %s could not be removed: %w. Run `flats secret rotate-key` again, which removes it", keyPath, pendingPath, err))
	} else if err := reached(rotatePendingRemoved); err != nil {
		return err
	} else {
		r.printf("removed the pending key %s\n", pendingPath)
	}
	if err := r.writeCopy(key, copyData, false); err != nil {
		late = append(late, err)
	}
	r.printf("%s\n", oldKeyNoteAfter)
	return errors.Join(late...)
}

// finishInputs makes the checks that the three finishes of rule 19 share, and writes nothing: key opens every sealed
// value, and the recovery password opens the recovery copy. subject names the key in the refusal. It returns the
// recovery copy of key, encrypted in memory, which is nil without a recovery copy.
func (r *rotation) finishInputs(subject string, key []byte) (check core.KeyCheck, copyData []byte, err error) {
	if check, err = core.CheckKey(r.ctx, r.st, key); err != nil {
		return check, nil, err
	}
	if !check.All() {
		return check, nil, unopenedRefusal(subject, check)
	}
	password, hasCopy, err := r.recoveryPassword()
	if err != nil {
		return check, nil, err
	}
	if copyData, err = sealCopy(key, password, hasCopy); err != nil {
		return check, nil, err
	}
	if err := r.ctx.Err(); err != nil {
		return check, nil, interrupted(err)
	}
	return check, copyData, nil
}

// finishWaiting finishes a rotation whose pending key waits (the first case of rule 19): the values are under the
// key of the pending file, and the key path does not hold it. It makes no new key and asks no confirmation.
func (r *rotation) finishWaiting(p core.PendingKey) error {
	keyPath, pendingPath := r.keyFile.Path(), r.keyFile.PendingPath()
	check, copyData, err := r.finishInputs("the pending key "+pendingPath, p.Key)
	if err != nil {
		return err
	}
	// The re-seal must be durable before the key file changes: an operator, or a loss of power, can have left a
	// commit that no checkpoint followed.
	if err := r.checkpoint(); err != nil {
		return fmt.Errorf("the rotation was not finished, because %w; nothing was changed. Run `flats secret rotate-key` again", err)
	}
	// The check of rule 12. Nothing at the key path is no failure: step 3 creates the file.
	if _, lerr := os.Lstat(keyPath); !errors.Is(lerr, fs.ErrNotExist) {
		if err := r.keyFile.CheckWritable(); err != nil {
			return r.notPlaced(err)
		}
	}
	if err := r.ctx.Err(); err != nil {
		return interrupted(err)
	}
	r.printf("finishing a rotation that had stopped: the %s in flats.db are under the key with the ID %s, which %s holds\n",
		counted(check.Total, "sealed value", "sealed values"), p.ID, pendingPath)
	return r.placeAndCopy(p.Key, copyData, true)
}

// finishDuplicate finishes a rotation whose key is in place and whose pending key is still there (the second case
// of rule 19). It removes the pending key and writes the recovery copy.
func (r *rotation) finishDuplicate(p core.PendingKey) error {
	keyPath, pendingPath := r.keyFile.Path(), r.keyFile.PendingPath()
	_, copyData, err := r.finishInputs("the key at "+keyPath, p.Key)
	if err != nil {
		return err
	}
	// An operator can have made the duplicate by hand before the re-seal was durable.
	if err := r.checkpoint(); err != nil {
		return fmt.Errorf("the pending key %s was not removed, because %w; nothing was changed. Run `flats secret rotate-key` again", pendingPath, err)
	}
	if err := r.ctx.Err(); err != nil {
		return interrupted(err)
	}
	r.printf("finishing a rotation that had stopped: %s holds the new key (key ID %s), and the pending key %s is a second file with it\n", keyPath, p.ID, pendingPath)
	return r.placeAndCopy(p.Key, copyData, false)
}

// finishOut finishes a rotation with --out whose file holds the key of every sealed value (the third case of rule
// 19). It makes the re-seal durable and writes the recovery copy. The operator puts the key in place.
func (r *rotation) finishOut(out outTarget) error {
	check, copyData, err := r.finishInputs("the key in "+out.path, out.key)
	if err != nil {
		return err
	}
	if err := r.checkpoint(); err != nil {
		return r.notDurableOut(err, out.path)
	}
	if err := r.ctx.Err(); err != nil {
		return interrupted(err)
	}
	r.printf("finished a rotation that had stopped: the re-seal is durable, and the %s in flats.db are under the key with the ID %s, which %s holds\n",
		counted(check.Total, "sealed value", "sealed values"), core.KeyID(out.key), out.path)
	r.printf("%s\n", putInPlace(out.path, r.keyFile.Path()))
	err = r.writeCopy(out.key, copyData, true)
	r.printf("%s\n", oldKeyNoteAfter)
	return err
}

// putInPlace says what the operator does with the file of --out (rule 28).
func putInPlace(outPath, keyPath string) string {
	return fmt.Sprintf("The new key must be put at %s: put the key of %s there, with the owner and the access that the host needs, and then remove %s. While flats.db holds sealed values, the start refuses until the new key is at %s.", keyPath, outPath, outPath, keyPath)
}
