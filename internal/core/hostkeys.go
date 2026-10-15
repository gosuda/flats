package core

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/store"
)

// Operator key ids (store.HostKey.ID).
const (
	HostKeyTailscale = "tailscale"
	HostKeyZrok      = "zrok"
	HostKeyNetbird   = "netbird"
)

// HostKeyIDs lists the operator keys in display order.
var HostKeyIDs = []string{HostKeyTailscale, HostKeyNetbird, HostKeyZrok}

// Where a stored key came from (store.HostKey.Source).
const (
	KeySourceConsole = "console"
	KeySourceCLI     = "cli"
	KeySourceFlag    = "flag"
	KeySourceEnv     = "env"
	KeySourceConfig  = "config"
)

// maxHostKeyLen bounds a key value; real keys are far shorter.
const maxHostKeyLen = 4096

// CheckHostKeyValue checks a key value before it is stored or used. A
// Tailscale key must be an auth key: OAuth client secrets and API keys are
// refused. Whether it is reusable cannot be told from the key.
func CheckHostKeyValue(id, v string) error {
	switch {
	case v == "":
		return invalidf("the %s key is empty", id)
	case len(v) > maxHostKeyLen:
		return invalidf("the %s key is longer than %d bytes", id, maxHostKeyLen)
	case strings.ContainsAny(v, " \t\r\n"):
		return invalidf("the %s key contains white space", id)
	}
	if id == HostKeyNetbird {
		if !setupKeyRE.MatchString(v) {
			return invalidf("a NetBird setup key is a UUID such as 8f3c2a1e-4b5d-4c6e-9f70-0123456789ab; copy it from Setup Keys in the NetBird dashboard")
		}
		return nil
	}
	if id != HostKeyTailscale {
		return nil
	}
	switch {
	case strings.HasPrefix(v, "tskey-auth-"):
		return nil
	case strings.HasPrefix(v, "tskey-client-"):
		return invalidf("this is a Tailscale OAuth client secret; create a reusable auth key under Settings > Keys in the Tailscale admin console instead")
	case strings.HasPrefix(v, "tskey-api-"):
		return invalidf("this is a Tailscale API access token; create a reusable auth key under Settings > Keys in the Tailscale admin console instead")
	default:
		return invalidf("a Tailscale auth key starts with tskey-auth-; create a reusable auth key under Settings > Keys in the Tailscale admin console")
	}
}

// setupKeyRE matches a NetBird setup key, which NetBird parses as a UUID.
var setupKeyRE = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

func hostKeyAAD(id string) []byte { return []byte("host-key\x00" + id) }

func hostKeyAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SealHostKey encrypts value for id under the data directory's secret.key,
// creating the key file when there is none yet.
func SealHostKey(dataDir, id, value string, source string, now time.Time) (store.HostKey, error) {
	key, err := loadOrCreateKey(filepath.Join(dataDir, "secret.key"))
	if err != nil {
		return store.HostKey{}, err
	}
	g, err := hostKeyAEAD(key)
	if err != nil {
		return store.HostKey{}, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return store.HostKey{}, err
	}
	return store.HostKey{ID: id, Nonce: nonce, Ciphertext: g.Seal(nil, nonce, []byte(value), hostKeyAAD(id)),
		Source: source, UpdatedAt: now}, nil
}

// OpenHostKey decrypts a stored key value.
func OpenHostKey(dataDir string, k store.HostKey) (string, error) {
	if !k.HasValue() {
		return "", errors.New("the key record holds no value")
	}
	key, err := readSecretKey(filepath.Join(dataDir, "secret.key"))
	if err != nil {
		return "", err
	}
	g, err := hostKeyAEAD(key)
	if err != nil {
		return "", err
	}
	pt, err := g.Open(nil, k.Nonce, k.Ciphertext, hostKeyAAD(k.ID))
	if err != nil {
		return "", fmt.Errorf("decrypt the stored %s key: %w; secret.key does not match flats.db", k.ID, err)
	}
	return string(pt), nil
}

// StoredHostKey returns the value stored for id, or "" with no stored value.
func StoredHostKey(ctx context.Context, dataDir string, st *store.Store, id string) (string, store.HostKey, error) {
	k, err := st.GetHostKey(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return "", store.HostKey{}, nil
	}
	if err != nil {
		return "", store.HostKey{}, err
	}
	if !k.HasValue() {
		return "", k, nil
	}
	v, err := OpenHostKey(dataDir, k)
	return v, k, err
}

// PutHostKeyValue seals and stores value for id.
func PutHostKeyValue(ctx context.Context, dataDir string, st *store.Store, id, value, source string, now time.Time) error {
	k, err := SealHostKey(dataDir, id, value, source, now)
	if err != nil {
		return err
	}
	return st.PutHostKey(ctx, k)
}

// SameKey compares two key values in constant time.
func SameKey(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
