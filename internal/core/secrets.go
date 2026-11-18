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

	"github.com/gosuda/flats/internal/egress"
	"github.com/gosuda/flats/internal/store"
)

// Secrets are encrypted with AES-256-GCM under a host key stored in
// <data>/secret.key (mode 0600). Values never leave the process except as
// environment of the flat's runtime; APIs only return names.

var secretName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

// ErrSecretKeyMissing reports that secret.key is gone while the database
// still holds secrets sealed with it. A new key could not open them.
var ErrSecretKeyMissing = errors.New("secret.key is missing but the database holds encrypted secrets")

// ErrInvalidSecretKey reports a file at secret.key that is no key: not a
// regular file, or not 32 bytes.
var ErrInvalidSecretKey = errors.New("not a valid secret key")

type invalidKeyError string

func (e invalidKeyError) Error() string      { return string(e) }
func (invalidKeyError) Is(target error) bool { return target == ErrInvalidSecretKey }

// CheckSecretKey refuses to let a new key be created over existing secrets:
// restore the key together with flats.db from the same backup. A configured key
// (host.secret_key_path) is read in full, so every way that it can be unusable is a
// refusal here, with or without sealed values: Flats never creates it.
//
// Before those checks it makes the check of a rotation that stopped (CheckRotation),
// whatever the key path holds, so that a host never seals a new value under a key
// that does not open the others (docs/spec/secret-key-rotation.md, rule 22). The
// refusal of a missing key names the key ID of the sealed values when they all
// carry one (rule 23).
func CheckSecretKey(ctx context.Context, key KeyFile, st *store.Store) error {
	ids, err := st.SealedKeyIDs(ctx)
	if err != nil {
		return err
	}
	if err := CheckRotation(key, ids); err != nil {
		return err
	}
	path := key.Path()
	_, lerr := os.Lstat(path)
	missing := errors.Is(lerr, os.ErrNotExist)
	if key.Configured() {
		// The two steps of key.Read, apart, so that only the refusal of the missing file gets the key ID.
		if err := key.Check(); err != nil {
			return err
		}
		_, err := key.readConfigured()
		if hint := keyOfTheValues(ids); err != nil && missing && hint != "" {
			return fmt.Errorf("%w%s", err, hint)
		}
		return err
	}
	if !missing {
		return nil
	}
	has, err := st.HasSecrets(ctx)
	if err != nil {
		return err
	}
	if has {
		return fmt.Errorf("%w: restore %s with `flats secret restore-key` or from the backup of this database, or, with neither the key nor its recovery password left, delete the stored values with `flats secret reset`; Flats will not create a new key%s", ErrSecretKeyMissing, path, keyOfTheValues(ids))
	}
	if has, err = st.HasSealedHostKeys(ctx); err != nil {
		return err
	}
	if has {
		return fmt.Errorf("%w: restore %s with `flats secret restore-key` or from the backup of this database, or remove the stored operator keys with `flats key rm` and register them again; Flats will not create a new key%s", ErrSecretKeyMissing, path, keyOfTheValues(ids))
	}
	return nil
}

// keyOfTheValues is the sentence that the refusal of a missing key ends with when every sealed value carries one
// key ID, and "" otherwise (rule 23 of docs/spec/secret-key-rotation.md).
func keyOfTheValues(ids store.KeyIDCounts) string {
	id, ok := ids.Single()
	if !ok {
		return ""
	}
	return fmt.Sprintf(". The stored values carry the key ID %s: a key file with that ID opens them", id)
}

// sealedByAnotherKey is the part of the error of a value that did not open that names both key IDs: the one that
// the value carries and the one of the key that was tried. It is "" for a value without a label and for one that
// carries the ID of the key, whose error stays as it was (rule 3 of docs/spec/secret-key-rotation.md).
func sealedByAnotherKey(valueID, keyID string) string {
	if valueID == "" || valueID == keyID {
		return ""
	}
	return fmt.Sprintf(" (the value carries the key ID %s, and the key in use has the ID %s)", valueID, keyID)
}

func loadOrCreateKey(path string) ([]byte, error) {
	if b, err := readSecretKey(path); err == nil {
		return b, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := NewSecretKey()
	if err != nil {
		return nil, err
	}
	if err := publishKey(path, key); err != nil {
		if os.IsExist(err) {
			// A winner must now be readable. EEXIST can also mean a dangling
			// symlink; never recurse or create more unpublished temporary keys.
			return readSecretKey(path)
		}
		return nil, err
	}
	return key, nil
}

// publishKey writes a complete, synced key to path without replacing
// anything there; os.IsExist reports that the name is taken. O_EXCL on the
// final file alone would expose an empty or partial key to readers.
func publishKey(path string, key []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".secret-key-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(key); err != nil {
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
	if err := os.Link(f.Name(), path); err != nil {
		if os.IsExist(err) {
			return err
		}
		return fmt.Errorf("publish secret key: %w", err)
	}
	return nil
}

func readSecretKey(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read secret key %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, invalidKeyError(path + ": secret key must be a regular file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read secret key %s: %w", path, err)
	}
	if len(b) != 32 {
		return nil, invalidKeyError(path + ": secret key must be 32 bytes")
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
	Name      string        `json:"name"`
	UpdatedAt time.Time     `json:"updated_at"`
	Inject    *SecretInject `json:"inject,omitempty"`
}

// SecretInject is the binding of an injected secret (docs/spec/injected-fetch-secrets.md): the host adds Header with
// the value to each fetch of the flat to Origin, and the value is not in env. The binding is not secret.
type SecretInject struct {
	Origin string `json:"origin"`
	Header string `json:"header"`
}

func secretInject(b store.Binding) *SecretInject {
	if !b.Injected() {
		return nil
	}
	return &SecretInject{Origin: b.Origin, Header: b.Header}
}

// SetSecret stores the value of a secret and names no binding. Only the operator (console or local CLI)
// may call it. Approved deployment activations (including rollback and data
// restore), new previews, and Flats host restarts capture the change. Automatic
// worker restarts retain the previously captured environment.
func (s *Service) SetSecret(ctx context.Context, slugName, name, value string, via Via) error {
	_, err := s.SetSecretBinding(ctx, slugName, name, value, nil, via)
	return err
}

// SetSecretBinding stores the value of a secret. With inject, the set names a binding: a new name becomes an injected
// secret, and a stored injected secret keeps its value only for the same binding (rules 2 and 8 of the spec). It
// returns the binding of the stored secret, or nil for a readable secret. No message holds the value.
func (s *Service) SetSecretBinding(ctx context.Context, slugName, name, value string, inject *SecretInject, via Via) (*SecretInject, error) {
	if via != ViaConsole && via != ViaCLI {
		return nil, forbiddenf("secret values are set by the operator in the web console or with `flats secret set`; agents can only list secret names")
	}
	if err := validateEnvName(name, "secret"); err != nil {
		return nil, err
	}
	if err := validateEnvValue(value, "secret"); err != nil {
		return nil, err
	}
	var want *store.Binding
	if inject != nil {
		origin, err := egress.InjectOrigin(inject.Origin)
		if err != nil {
			return nil, invalidf("secret %q: %v", name, err)
		}
		header, err := egress.InjectHeader(inject.Header)
		if err != nil {
			return nil, invalidf("secret %q: %v", name, err)
		}
		want = &store.Binding{Origin: origin, Header: header}
	}
	valueErr := egress.CheckInjectValue(value)
	if _, err := s.st.GetFlat(ctx, slugName); err != nil {
		return nil, err
	}
	g, err := s.aead()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ct := g.Seal(nil, nonce, []byte(value), []byte(name))
	sec := store.SealedSecret{Name: name, Nonce: nonce, Ciphertext: ct, KeyID: s.secretKeyID, UpdatedAt: s.now()}
	stored, err := s.st.PutSecretChecked(ctx, slugName, sec, store.SecretPut{Inject: want, HeaderValue: valueErr == nil}, s.audit(ctx, via))
	if err != nil {
		if errors.Is(err, store.ErrEnvCollision) {
			return nil, invalidf("secret %q conflicts with an existing environment variable", name)
		}
		var refused *store.InjectRefusal
		if errors.As(err, &refused) {
			return nil, injectRefusal(slugName, name, refused, valueErr)
		}
		return nil, err
	}
	how := ""
	if stored.Injected() {
		how = fmt.Sprintf(" (sent as %s to %s, not in env)", stored.Header, stored.Origin)
	}
	s.Event(ctx, slugName, "info", "secret", fmt.Sprintf("secret %s set via %s%s; redeploy to apply", name, via, how), nil)
	return secretInject(stored), nil
}

// injectRefusal is the message of a set that rules 7 and 8 refuse. Each names the next command, and none holds the
// value.
func injectRefusal(slugName, name string, r *store.InjectRefusal, valueErr error) error {
	switch r.Reason {
	case store.InjectReadable:
		return withKind(ErrConflict, fmt.Errorf("secret %q exists and flat code reads it as env.%s, so the flat may hold its value. "+
			"Remove it with `flats secret rm %s %s`, get a new credential from the service, and set it again with --origin and --header",
			name, name, slugName, name))
	case store.InjectOtherBinding:
		return withKind(ErrConflict, fmt.Errorf("secret %q is sent as %s to %s, and its binding stays until the secret is removed. "+
			"To change it, remove the secret with `flats secret rm %s %s` and set it again with its value, --origin and --header",
			name, r.Binding.Header, r.Binding.Origin, slugName, name))
	case store.InjectTaken:
		return withKind(ErrConflict, fmt.Errorf("secret %q is already sent as %s to %s, and a flat has one secret for each origin and header. "+
			"Remove it with `flats secret rm %s %s`, or give it a new value with `flats secret set %s %s` without --origin and --header",
			r.Secret, r.Binding.Header, r.Binding.Origin, slugName, r.Secret, slugName, r.Secret))
	case store.InjectLimit:
		return invalidf("flat %s has %d injected secrets, the most that a flat can have. List them with `flats secret ls %s` and remove one with `flats secret rm`",
			slugName, store.MaxInjectedSecrets, slugName)
	default:
		reason := "its value cannot be a header value"
		if valueErr != nil {
			reason = valueErr.Error()
		}
		return invalidf("the value of secret %q goes into the header %s of each fetch to %s, and %s. Nothing changed: run `flats secret set %s %s` again with a valid value",
			name, r.Binding.Header, r.Binding.Origin, reason, slugName, name)
	}
}

// DeleteSecret removes a secret.
func (s *Service) DeleteSecret(ctx context.Context, slugName, name string, via Via) error {
	if via != ViaConsole && via != ViaCLI {
		return forbiddenf("only the operator can delete secrets")
	}
	if err := s.st.DeleteSecret(ctx, slugName, name, s.audit(ctx, via)); err != nil {
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
		out = append(out, SecretInfo{Name: sec.Name, UpdatedAt: sec.UpdatedAt, Inject: secretInject(sec.Inject)})
	}
	return out, nil
}

// Stored secrets predate ordinary env validation. Preserve their names and bytes;
// each runtime retains its existing binding and value handling. New writes remain
// strictly validated. Never silently remove legacy operator configuration.
func (s *Service) decryptSecrets(secs []store.SealedSecret) (map[string]string, error) {
	g, err := s.aead()
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, sec := range secs {
		pt, err := g.Open(nil, sec.Nonce, sec.Ciphertext, []byte(sec.Name))
		if err != nil {
			return nil, fmt.Errorf("decrypt secret %s: %w%s", sec.Name, err, sealedByAnotherKey(sec.KeyID, s.secretKeyID))
		}
		env[sec.Name] = string(pt)
	}
	return env, nil
}
