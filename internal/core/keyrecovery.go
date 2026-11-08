package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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

// ReadSecretKey reads dataDir's secret.key with the checks the host makes at
// start.
func ReadSecretKey(dataDir string) ([]byte, error) {
	return readSecretKey(filepath.Join(dataDir, SecretKeyFile))
}

// KeyID identifies a key without revealing anything that helps open it.
func KeyID(key []byte) string {
	h := sha256.New()
	h.Write([]byte("flats secret key id\x00"))
	h.Write(key)
	return hex.EncodeToString(h.Sum(nil)[:16])
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
// wrong password, or a file with no passphrase recipient, wraps
// ErrRecoveryPassword.
func OpenKeyCopy(data []byte, password string) ([]byte, error) {
	id, err := age.NewScryptIdentity(password)
	if err != nil {
		return nil, err
	}
	var src io.Reader = bytes.NewReader(data)
	if bytes.HasPrefix(bytes.TrimLeft(data, " \t\r\n"), []byte(armor.Header)) {
		src = armor.NewReader(src)
	}
	r, err := age.Decrypt(src, id)
	if err != nil {
		var none *age.NoIdentityMatchError
		if errors.As(err, &none) {
			return nil, fmt.Errorf("%w: %v", ErrRecoveryPassword, err)
		}
		return nil, fmt.Errorf("not a readable recovery file: %w", err)
	}
	key, err := io.ReadAll(io.LimitReader(r, 33))
	if err != nil {
		return nil, fmt.Errorf("the recovery file is damaged: %w", err)
	}
	if len(key) != 32 {
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

// RestoreSecretKey writes key as dataDir's secret.key. It never replaces
// anything there: os.IsExist reports that the name is taken.
func RestoreSecretKey(dataDir string, key []byte) error {
	if err := publishKey(filepath.Join(dataDir, SecretKeyFile), key); err != nil {
		return err
	}
	return durable.SyncDir(dataDir)
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
	b, err := os.ReadFile(filepath.Join(dataDir, RecoveryKeyIDFile))
	if err == nil && strings.TrimSpace(string(b)) == KeyID(key) {
		return ""
	}
	return fmt.Sprintf("%s may hold another key than %s, so it may not restore this host: run `flats secret backup-key` to write a current copy", copyPath, SecretKeyFile)
}
