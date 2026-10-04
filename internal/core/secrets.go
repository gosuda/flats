package core

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/gosuda/flats/internal/store"
)

// Secrets are encrypted with AES-256-GCM under a host key stored in
// <data>/secret.key (mode 0600). Values never leave the process except as
// environment of the flat's runtime; APIs only return names.

var secretName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

// ErrSecretKeyMissing reports that secret.key is gone while the database
// still holds secrets sealed with it. A new key could not open them.
var ErrSecretKeyMissing = errors.New("secret.key is missing but the database holds encrypted secrets")

// CheckSecretKey refuses to let a new key be created over existing secrets:
// restore <dataDir>/secret.key together with flats.db from the same backup.
func CheckSecretKey(ctx context.Context, dataDir string, st *store.Store) error {
	path := filepath.Join(dataDir, "secret.key")
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	has, err := st.HasSecrets(ctx)
	if err != nil {
		return err
	}
	if has {
		return fmt.Errorf("%w: restore %s from the backup of this database; Flats will not create a new key", ErrSecretKeyMissing, path)
	}
	return nil
}

func loadOrCreateKey(path string) ([]byte, error) {
	if b, err := readSecretKey(path); err == nil {
		return b, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	// Publish a complete, synced key without replacing a concurrent winner.
	// O_EXCL on the final file alone would expose an empty/partial key to readers.
	f, err := os.CreateTemp(filepath.Dir(path), ".secret-key-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(key); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := os.Link(f.Name(), path); err != nil {
		if os.IsExist(err) {
			// A winner must now be readable. EEXIST can also mean a dangling
			// symlink; never recurse or create more unpublished temporary keys.
			return readSecretKey(path)
		}
		return nil, fmt.Errorf("publish secret key: %w", err)
	}

	return key, nil
}

func readSecretKey(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read secret key %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: secret key must be a regular file", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read secret key %s: %w", path, err)
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("%s: secret key must be 32 bytes", path)
	}
	return b, nil
}

func (s *Service) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(s.secretKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SecretInfo is what APIs may reveal about a secret.
type SecretInfo struct {
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SetSecret stores a secret value. Only the operator (console or local CLI)
// may call it; the change applies on the next deploy.
func (s *Service) SetSecret(ctx context.Context, slugName, name, value string, via Via) error {
	if via != ViaConsole && via != ViaCLI {
		return forbiddenf("secret values are set by the operator in the web console or with `flats secret set`; agents can only list secret names")
	}
	if !secretName.MatchString(name) {
		return invalidf("secret name %q must match [A-Z_][A-Z0-9_]* (at most 64 characters)", name)
	}
	if len(value) > 64<<10 {
		return invalidf("secret value is larger than 64 KiB")
	}
	if _, err := s.st.GetFlat(ctx, slugName); err != nil {
		return err
	}
	g, err := s.aead()
	if err != nil {
		return err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ct := g.Seal(nil, nonce, []byte(value), []byte(name))
	if err := s.st.PutSecret(ctx, slugName, store.SealedSecret{Name: name, Nonce: nonce, Ciphertext: ct, UpdatedAt: s.now()}); err != nil {
		return err
	}
	s.Event(ctx, slugName, "info", "secret", fmt.Sprintf("secret %s set via %s; redeploy to apply", name, via), nil)
	return nil
}

// DeleteSecret removes a secret.
func (s *Service) DeleteSecret(ctx context.Context, slugName, name string, via Via) error {
	if via != ViaConsole && via != ViaCLI {
		return forbiddenf("only the operator can delete secrets")
	}
	if err := s.st.DeleteSecret(ctx, slugName, name); err != nil {
		return err
	}
	s.Event(ctx, slugName, "info", "secret", fmt.Sprintf("secret %s deleted via %s; redeploy to apply", name, via), nil)
	return nil
}

// SecretNames lists secret names (never values).
func (s *Service) SecretNames(ctx context.Context, slugName string) ([]SecretInfo, error) {
	if _, err := s.st.GetFlat(ctx, slugName); err != nil {
		return nil, err
	}
	secs, err := s.st.ListSecrets(ctx, slugName)
	if err != nil {
		return nil, err
	}
	out := make([]SecretInfo, 0, len(secs))
	for _, sec := range secs {
		out = append(out, SecretInfo{Name: sec.Name, UpdatedAt: sec.UpdatedAt})
	}
	return out, nil
}

func (s *Service) secretsFor(ctx context.Context, slugName string) (map[string]string, error) {
	secs, err := s.st.ListSecrets(ctx, slugName)
	if err != nil {
		return nil, err
	}
	g, err := s.aead()
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, sec := range secs {
		pt, err := g.Open(nil, sec.Nonce, sec.Ciphertext, []byte(sec.Name))
		if err != nil {
			return nil, fmt.Errorf("decrypt secret %s: %w", sec.Name, err)
		}
		env[sec.Name] = string(pt)
	}
	return env, nil
}
