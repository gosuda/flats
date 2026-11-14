package core

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"

	"github.com/gosuda/flats/internal/durable"
	"github.com/gosuda/flats/internal/store"
)

// The recovery copy of secret.key is the key's 32 bytes in an age file with
// one passphrase (scrypt) recipient, so the reference age tool opens it
// without Flats. See docs/spec/secret-key-recovery.md.

const (
	// SecretKeyFile is the host key's name in the data directory.
	SecretKeyFile = "secret.key"
	// RecoveryKeyFile is the name of the recovery copy in the data directory.
	RecoveryKeyFile = "secret.key.recovery"
	// RecoveryKeyIDFile holds the key ID of the key in the recovery copy.
	RecoveryKeyIDFile = "secret.key.recovery.id"
	// RecoveryWorkFactor is the scrypt work factor (log2 N) of a recovery
	// copy: age's default, about a second and 256 MiB of memory.
	RecoveryWorkFactor = 18
	// MaxRecoveryFileSize bounds what a restore reads; a real copy is about
	// 200 bytes.
	MaxRecoveryFileSize = 64 << 10
)

// ErrRecoveryPassword reports that a password does not open a recovery copy.
var ErrRecoveryPassword = errors.New("the password does not open this recovery file")

// KeyID identifies a key without revealing anything that helps open it: the
// first 16 bytes of HMAC-SHA-256 over a 19-byte label, with the key as the
// HMAC key (docs/spec/secret-key-recovery.md, rule 2). The key is the HMAC
// key and not hash input, so that a code scanner does not take the ID for a
// fast hash of a password.
func KeyID(key []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("flats secret key id"))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

// SealKeyCopy encrypts key under password at the scrypt work factor
// workFactor; callers pass RecoveryWorkFactor, tests a lower one.
func SealKeyCopy(key []byte, password string, workFactor int) ([]byte, error) {
	r, err := age.NewScryptRecipient(password)
	if err != nil {
		return nil, err
	}
	r.SetWorkFactor(workFactor)
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, r)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(key); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// OpenKeyCopy decrypts a recovery copy in the binary or the armored form. A
// wrong password wraps ErrRecoveryPassword. A work factor above
// RecoveryWorkFactor is refused before any key is derived, so a crafted file
// costs no more memory than backup-key does.
func OpenKeyCopy(data []byte, password string) ([]byte, error) {
	id, err := age.NewScryptIdentity(password)
	if err != nil {
		return nil, err
	}
	id.SetMaxWorkFactor(RecoveryWorkFactor)
	var src io.Reader = bytes.NewReader(data)
	if bytes.HasPrefix(bytes.TrimLeft(data, " \t\r\n"), []byte(armor.Header)) {
		src = armor.NewReader(src)
	}
	r, err := age.Decrypt(src, id)
	if err != nil {
		var none *age.NoIdentityMatchError
		switch {
		case errors.As(err, &none) && !slices.Contains(none.StanzaTypes, "scrypt"):
			types := slices.Compact(slices.Sorted(slices.Values(none.StanzaTypes)))
			return nil, fmt.Errorf("the recovery file is encrypted to %s recipients, not with a password", strings.Join(types, ", "))
		case errors.As(err, &none):
			return nil, fmt.Errorf("%w: %v", ErrRecoveryPassword, err)
		// age has no error value for this refusal; TestKeyCopyWorkFactorCap
		// pins its text.
		case strings.Contains(err.Error(), "work factor too large"):
			return nil, fmt.Errorf("%w (the limit is %d): `age -d` opens such a copy, as the docs describe", err, RecoveryWorkFactor)
		}
		return nil, fmt.Errorf("not a readable recovery file: %w", err)
	}
	key, err := io.ReadAll(io.LimitReader(r, 33))
	if err != nil {
		return nil, fmt.Errorf("the recovery file is damaged: %w", err)
	}
	switch {
	case len(key) > 32:
		return nil, errors.New("the recovery file holds more than 32 bytes, not a 32-byte key")
	case len(key) < 32:
		return nil, fmt.Errorf("the recovery file holds %d bytes, not a 32-byte key", len(key))
	}
	return key, nil
}

// SealedName names one sealed value: a flat secret, or an operator key when
// Flat is "".
type SealedName struct{ Flat, Name string }

func (n SealedName) String() string {
	if n.Flat == "" {
		return "operator key " + n.Name
	}
	return "secret " + n.Name + " of " + n.Flat
}

// KeyCheck counts the sealed values in flats.db that a key opens, and names
// those it does not.
type KeyCheck struct {
	Opened, Total int
	Unopened      []SealedName
}

// All reports whether the key opens every sealed value.
func (c KeyCheck) All() bool { return c.Opened == c.Total }

// CheckKey tries key on every flat secret and every operator key record that
// holds a value.
func CheckKey(ctx context.Context, st *store.Store, key []byte) (KeyCheck, error) {
	secs, err := st.ListAllSecrets(ctx)
	if err != nil {
		return KeyCheck{}, err
	}
	keys, err := st.ListHostKeys(ctx)
	if err != nil {
		return KeyCheck{}, err
	}
	g, err := hostKeyAEAD(key)
	if err != nil {
		return KeyCheck{}, err
	}
	var c KeyCheck
	try := func(name SealedName, nonce, ciphertext, aad []byte) {
		c.Total++
		// Open panics on a nonce of the wrong size; a damaged row opens nothing.
		if len(nonce) == g.NonceSize() {
			if _, err := g.Open(nil, nonce, ciphertext, aad); err == nil {
				c.Opened++
				return
			}
		}
		c.Unopened = append(c.Unopened, name)
	}
	for _, s := range secs {
		try(SealedName{s.Flat, s.Name}, s.Nonce, s.Ciphertext, []byte(s.Name))
	}
	for _, k := range keys {
		if k.HasValue() {
			try(SealedName{Name: k.ID}, k.Nonce, k.Ciphertext, hostKeyAAD(k.ID))
		}
	}
	return c, nil
}

// WriteKeyCopy writes data, the recovery copy of key, to dataDir and then
// the key's ID beside it, each replacing an older file, and reports whether
// a copy was there. A crash between the two leaves an ID that does not match,
// which the next start reports, never one that matches an older copy.
func WriteKeyCopy(dataDir string, data, key []byte) (replaced bool, err error) {
	_, err = os.Lstat(filepath.Join(dataDir, RecoveryKeyFile))
	replaced = err == nil
	if err := replaceFile(dataDir, RecoveryKeyFile, data); err != nil {
		return false, err
	}
	return replaced, replaceFile(dataDir, RecoveryKeyIDFile, []byte(KeyID(key)+"\n"))
}

// replaceFile writes data to dir/name through a synced temporary file.
func replaceFile(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := durable.Rename(f.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	return durable.SyncDir(dir)
}

// RecoveryCopyWarning describes a recovery copy in dataDir that may hold
// another key than key, or returns "" when it matches or there is none.
func RecoveryCopyWarning(dataDir string, key []byte) string {
	copyPath := filepath.Join(dataDir, RecoveryKeyFile)
	if _, err := os.Lstat(copyPath); errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	// An ID is 32 hex digits; 4 KiB leaves room for white space around it,
	// and a large file cannot slow the start.
	b, err := readHead(filepath.Join(dataDir, RecoveryKeyIDFile), 4096)
	if err == nil && strings.TrimSpace(string(b)) == KeyID(key) {
		return ""
	}
	return fmt.Sprintf("%s may hold another key than the host key, so it may not restore this host: run `flats secret backup-key` to write a current copy", copyPath)
}

// readHead reads at most n bytes from the start of the file at path.
func readHead(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, n))
}
