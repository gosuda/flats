package core

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/durable"
	"github.com/gosuda/flats/internal/store"
)

// The rotation of the host key (docs/spec/secret-key-rotation.md). This file holds what the command, the start and
// the other key commands share: the pending key file and what it is, the check of a rotation that stopped, the file
// writes of the rotation, and the re-seal. The order of the steps is the command's.

// PendingKeyFile is the name of the pending key file in the data directory, for the default key path.
const PendingKeyFile = SecretKeyFile + pendingSuffix

const pendingSuffix = ".next"

// ErrPendingKey reports something at the name of the pending key file that no reader can classify: it is no regular
// file, or it cannot be read. It can be a pending key that waits, so every reader stops on it (rule 17).
var ErrPendingKey = errors.New("the pending key file of a rotation cannot be used")

// ErrRotationStopped reports the first state of rule 22: a pending key waits. It holds the key that opens the sealed
// values, and the key path does not hold it.
var ErrRotationStopped = errors.New("a rotation of the secret key stopped before the new key was in place")

// ErrKeySealedNothing reports the second state of rule 22: the key path holds a valid key, every sealed value carries
// a key ID, and none carries the ID of that key.
var ErrKeySealedNothing = errors.New("the secret key sealed none of the stored values")

// PendingState says what the pending key file is (rule 17).
type PendingState int

const (
	// PendingNone: nothing is at the name.
	PendingNone PendingState = iota
	// PendingWaits: a valid key whose ID a sealed value carries, and the key path does not hold the same key. A
	// rotation stopped after its re-seal, and this file holds the key that opens the values.
	PendingWaits
	// PendingDuplicate: a valid key, and the key path holds the same 32 bytes. The key is in place.
	PendingDuplicate
	// PendingLeftOver: every other regular file: a valid key whose ID no sealed value carries, or a file that is not
	// 32 bytes. It is never the only file with the key of a value.
	PendingLeftOver
)

// PendingKey is the pending key file as Pending found it. Key and ID are set when the file holds a valid key.
type PendingKey struct {
	State PendingState
	Key   []byte
	ID    string
}

// PendingPath is the pending key file of a rotation: the key path with ".next" after it, which is
// <data>/secret.key.next for the default path (rule 13).
func (k KeyFile) PendingPath() string { return k.path + pendingSuffix }

// Pending classifies the pending key file by the key IDs that the sealed values carry (rule 17). It looks at the name
// without following a link. Something there that is no regular file, or a file that cannot be read, is an error that
// wraps ErrPendingKey, and never a file that is left over. A key file that is missing or cannot be read counts as
// another key than the pending one.
func (k KeyFile) Pending(ids store.KeyIDCounts) (PendingKey, error) {
	return k.pending(ids, true)
}

// pending is Pending. With compareAlways false it reads the key file only for a pending key whose ID a sealed value
// carries, and then reports a duplicate whose ID no value carries as left over. That is what a caller needs that
// must not read the key without a reason and only asks whether a key waits (rule 26).
func (k KeyFile) pending(ids store.KeyIDCounts, compareAlways bool) (PendingKey, error) {
	data, found, err := readPending(k.PendingPath())
	if err != nil || !found {
		return PendingKey{}, err
	}
	if err := k.keyPathLeadsToPending(); err != nil {
		return PendingKey{}, err
	}
	if len(data) != 32 {
		return PendingKey{State: PendingLeftOver}, nil
	}
	p := PendingKey{State: PendingLeftOver, Key: data, ID: KeyID(data)}
	carried := ids.Carries(p.ID)
	if !carried && !compareAlways {
		return p, nil
	}
	current, err := readSecretKey(k.path)
	switch {
	case err == nil && subtle.ConstantTimeCompare(current, data) == 1:
		p.State = PendingDuplicate
	case carried:
		p.State = PendingWaits
	}
	return p, nil
}

// keyPathLeadsToPending refuses a key path that is a symbolic link which leads to the pending key file (rule 17).
// The two names then hold the same bytes, as a duplicate does, but the pending key file is the only file with them:
// its removal would leave a link to nothing. So no reader classifies the pending key, and none removes it. A hard
// link is not refused: it is a second name of the file, which keeps the key when the pending name goes.
func (k KeyFile) keyPathLeadsToPending() error {
	link, err := os.Lstat(k.path)
	if err != nil || link.Mode()&fs.ModeSymlink == 0 {
		return nil
	}
	target, err := os.Stat(k.path)
	if err != nil {
		return nil // a link to nothing: the read of the key refuses it
	}
	pending, err := os.Lstat(k.PendingPath())
	if err != nil || !os.SameFile(target, pending) {
		return nil
	}
	return fmt.Errorf("%w: the key path %s is a link that leads to the pending key file %s, which is then the only file with the key. Flats does not go on, because the end of a rotation removes the pending key file: put a real copy of that file at the key path in place of the link, and run `flats secret rotate-key`",
		ErrPendingKey, k.path, k.PendingPath())
}

// readPending reads the file at the pending name without following a link. found is false when nothing is there. It
// reads one byte more than a key has, which is enough to tell a key from a file of another size.
func readPending(path string) (data []byte, found bool, err error) {
	li, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return nil, false, nil // also when the folder of the key is missing: no file can be at the name
	}
	if err != nil {
		return nil, false, pendingKeyError(path, "cannot be looked at: "+err.Error())
	}
	if !li.Mode().IsRegular() {
		return nil, false, pendingKeyError(path, "is no regular file ("+fileKind(li.Mode())+")")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false, pendingKeyError(path, "cannot be read: "+err.Error())
	}
	defer f.Close()
	// The name can change between the two calls. Only the file that Lstat saw is read, never one behind a link.
	if fi, err := f.Stat(); err != nil || !os.SameFile(li, fi) {
		return nil, false, pendingKeyError(path, "changed while it was read")
	}
	data, err = io.ReadAll(io.LimitReader(f, 33))
	if err != nil {
		return nil, false, pendingKeyError(path, "cannot be read: "+err.Error())
	}
	return data, true, nil
}

func fileKind(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "a folder"
	case m&fs.ModeSymlink != 0:
		return "a link"
	}
	return "a special file"
}

// pendingKeyError is the refusal of rule 17 for a file that no reader can classify. It names the file and both ways
// out.
func pendingKeyError(path, what string) error {
	return fmt.Errorf("%w: %s %s. It can hold the key of a rotation that stopped, so Flats does not go on: move it away, or make it a file that the user who runs Flats can read",
		ErrPendingKey, path, what)
}

// CheckRotation is the check of rule 22, which the start, `flats key set`, `flats secret backup-key` and
// `flats backup --out` share. ids are the key IDs that the sealed values carry. It refuses in two states, and in no
// other:
//
//   - ErrRotationStopped: a pending key waits, whatever the key path holds.
//   - ErrKeySealedNothing: no pending key waits, the key path holds a valid key that the host can use, there is a
//     sealed value, every sealed value carries a key ID, and none carries the ID of that key.
//
// A host in one of them would seal new values under a key that does not open the others. A pending key file that
// cannot be classified is the error of Pending. It writes nothing.
func CheckRotation(k KeyFile, ids store.KeyIDCounts) error {
	if err := CheckStoppedRotation(k, ids); err != nil {
		return err
	}
	if !ids.AllLabelled() {
		return nil
	}
	key, err := k.Read()
	if err != nil {
		return nil // no key that the host can use: the caller's own read of the key refuses, with its own message
	}
	id := KeyID(key)
	if ids.Carries(id) {
		return nil
	}
	of := ids.IDs()
	carried := "the key ID " + of[0]
	if len(of) > 1 {
		carried = "the key IDs " + strings.Join(of, ", ")
	}
	return fmt.Errorf("%w: the key at %s has the key ID %s, and the sealed values in flats.db carry %s. After a rotation with `--out`, run `flats secret rotate-key --out FILE` again with the same file, and put the key of that file at %s only after that run reported the re-seal durable. Else restore the key that sealed the values with `flats secret restore-key`, or, with no such key left, delete the stored values with `flats secret reset`",
		ErrKeySealedNothing, k.path, id, carried, k.path)
}

// CheckStoppedRotation is the first state of CheckRotation alone: it refuses with ErrRotationStopped when a pending
// key waits. A caller that reads no key for itself uses it, as `flats backup --exclude-secret-key` does (rule 26). It
// reads the key file only to compare it with a pending key whose ID a sealed value carries.
func CheckStoppedRotation(k KeyFile, ids store.KeyIDCounts) error {
	p, err := k.pending(ids, false)
	if err != nil {
		return err
	}
	if p.State != PendingWaits {
		return nil
	}
	return fmt.Errorf("%w: %s holds the key that opens the stored values (key ID %s), and %s does not hold that key. Run `flats secret rotate-key` while the host is stopped: it finishes the rotation",
		ErrRotationStopped, k.PendingPath(), p.ID, k.path)
}

// NewSecretKey returns a new host key: 32 bytes from the random source of the operating system (rule 11).
func NewSecretKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

// CheckPendingPlace refuses a configured key path whose folder, once links are resolved, lies inside the data
// directory: the pending key would be created there, and a copy of the data directory would hold the key that the
// setting keeps out (rule 13). A default key has nothing to check.
func (k KeyFile) CheckPendingPlace() error {
	if !k.configured {
		return nil
	}
	dir := filepath.Dir(k.PendingPath())
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("look at the folder of the pending key %s: %w", k.PendingPath(), err)
	}
	dataReal, err := filepath.EvalSymlinks(k.dataDir)
	if err != nil {
		return fmt.Errorf("look at the data directory %s: %w", k.dataDir, err)
	}
	if config.InsideDir(dataReal, real) {
		return fmt.Errorf("the pending key %s would lie in %s, inside the data directory %s, where a copy of the data directory would hold it",
			k.PendingPath(), real, dataReal)
	}
	return nil
}

// WritePending creates the pending key file with key and makes it durable (rule 13): a create that fails when the
// name exists, mode 0600, the 32 bytes, a sync of the file and then of its folder. It makes no temporary file and no
// hard link, and os.IsExist reports that the name is taken. A file that it created stays after a failed write or a
// failed sync, and the error then wraps ErrPendingKeyStays: the file is left over, and the next rotation removes it
// (rule 21).
func (k KeyFile) WritePending(key []byte) error {
	if len(key) != 32 {
		return errors.New("a secret key must be 32 bytes")
	}
	created, err := createKeyFile(k.PendingPath(), key)
	if err == nil {
		err = durable.SyncDir(filepath.Dir(k.PendingPath()))
	}
	if err != nil && created {
		return fmt.Errorf("%w: %w", ErrPendingKeyStays, err)
	}
	return err
}

// ErrPendingKeyStays reports a pending key file that WritePending created and could not complete or make durable.
// The file stays.
var ErrPendingKeyStays = errors.New("the pending key file was created, but its write did not complete")

// ErrRemovedNotSynced reports a pending key file that RemovePending removed, when the sync of its folder failed
// afterwards: the file is gone, and a loss of power can bring it back.
var ErrRemovedNotSynced = errors.New("the file was removed, but the sync of its folder failed")

// createKeyFile creates path with a call that fails when the name exists, writes key and syncs the file. created
// reports that the file exists because of this call, also when a later step failed.
func createKeyFile(path string, key []byte) (created bool, err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		return true, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return true, err
	}
	return true, f.Close()
}

// RemovePending removes the pending key file and syncs its folder. It removes only a regular file: anything else at
// the name is the refusal of Pending. When the file is removed and the sync fails, the error wraps
// ErrRemovedNotSynced.
func (k KeyFile) RemovePending() error {
	path := k.PendingPath()
	li, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !li.Mode().IsRegular() {
		return pendingKeyError(path, "is no regular file ("+fileKind(li.Mode())+")")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if err := durable.SyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%w: %w", ErrRemovedNotSynced, err)
	}
	return nil
}

// CheckWritable opens the key file for writing and closes it again without a change, the check of rule 12 before the
// first write of a rotation. It follows a link, as the write does. A read-only mount or attribute, a mode that does
// not let the user write and a program that holds the file fail it. So does anything that is no regular file once
// the links are followed: the open of a named pipe would wait for a reader. Nothing at the key path is
// fs.ErrNotExist.
func (k KeyFile) CheckWritable() error {
	if err := regularFile(k.path); err != nil {
		return err
	}
	f, err := os.OpenFile(k.path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	return f.Close()
}

// regularFile refuses a path that holds something other than a regular file, after it followed the links.
func regularFile(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is no regular file (%s)", path, fileKind(fi.Mode()))
	}
	return nil
}

// WriteInPlace writes key into the key file where it is, and does not replace the file, so the file keeps its owner,
// its mode and its access list, and every name of it gets the key (rule 12, and step 3 of rule 16). It follows a
// link. It writes the 32 bytes at the start, cuts the file to 32 bytes, syncs it, reads it again and compares; bytes
// that differ are an error. When nothing is at the key path, it creates the file with mode 0600 and syncs the file
// and its folder (rule 19). It makes no temporary file and no rename.
//
// written is a seam for the tests of the command and nil otherwise: it runs after the synced write and before the
// read, and its error stops the call there, as a stop of the process would.
func (k KeyFile) WriteInPlace(key []byte, written func() error) error {
	if len(key) != 32 {
		return errors.New("a secret key must be 32 bytes")
	}
	if _, err := os.Lstat(k.path); errors.Is(err, fs.ErrNotExist) {
		if _, err := createKeyFile(k.path, key); err != nil {
			return err
		}
		if err := durable.SyncDir(filepath.Dir(k.path)); err != nil {
			return err
		}
	} else if err := overwriteKeyFile(k.path, key); err != nil {
		return err
	}
	if written != nil {
		if err := written(); err != nil {
			return err
		}
	}
	got, err := readHead(k.path, 33)
	if err != nil {
		return fmt.Errorf("read %s again after the write: %w", k.path, err)
	}
	if subtle.ConstantTimeCompare(got, key) != 1 {
		return fmt.Errorf("%s does not hold the key that was written to it: the read after the write gave other bytes", k.path)
	}
	return nil
}

// overwriteKeyFile writes key over the content of the regular file at path, which exists, and syncs it.
func overwriteKeyFile(path string, key []byte) error {
	if err := regularFile(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(key, 0); err != nil {
		f.Close()
		return err
	}
	if err := f.Truncate(int64(len(key))); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Reseal seals every sealed value in st again, from oldKey to newKey, in the one transaction of store.Reseal, which
// also writes the key.rotate event of audit (rules 15 and 30). Each value is opened with oldKey and sealed with
// newKey under a new random nonce and the same associated data, and it gets the key ID of newKey. A value that oldKey
// does not open fails the call, and the transaction then changes nothing. out says that the new key went to a file
// of the operator. It returns the numbers of flat secrets and of operator key values that were sealed again.
func Reseal(ctx context.Context, st *store.Store, oldKey, newKey []byte, out bool, audit store.Audit) (secrets, keys int, err error) {
	if len(oldKey) != 32 || len(newKey) != 32 {
		return 0, 0, errors.New("a secret key must be 32 bytes")
	}
	from, err := hostKeyAEAD(oldKey)
	if err != nil {
		return 0, 0, err
	}
	to, err := hostKeyAEAD(newKey)
	if err != nil {
		return 0, 0, err
	}
	seal := func(v store.SealedValue) ([]byte, []byte, error) {
		aad := []byte(v.Name)
		if v.Flat == "" {
			aad = hostKeyAAD(v.Name)
		}
		// Open panics on a nonce of the wrong size; a damaged row opens nothing.
		if len(v.Nonce) != from.NonceSize() {
			return nil, nil, fmt.Errorf("%s does not open with the old key: its nonce has %d bytes", v, len(v.Nonce))
		}
		plain, err := from.Open(nil, v.Nonce, v.Ciphertext, aad)
		if err != nil {
			return nil, nil, fmt.Errorf("%s does not open with the old key: %w", v, err)
		}
		defer clear(plain) // the opened value does not outlive its seal
		nonce := make([]byte, to.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			return nil, nil, err
		}
		return nonce, to.Seal(nil, nonce, plain, aad), nil
	}
	return st.Reseal(ctx, store.Reseal{From: KeyID(oldKey), To: KeyID(newKey), Out: out, Seal: seal, Audit: audit})
}
