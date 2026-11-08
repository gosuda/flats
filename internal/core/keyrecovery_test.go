package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/ostest"
	"github.com/gosuda/flats/internal/store"
)

const (
	testPassword = "correct horse battery staple"
	// testWorkFactor keeps each derivation cheap; RecoveryWorkFactor costs
	// a second, and ten under -race.
	testWorkFactor = 10
)

// A copy records the work factor it was written at, and the commands use
// age's default. Reading the header needs no derivation at 2^18.
func TestKeyCopyWorkFactor(t *testing.T) {
	if RecoveryWorkFactor != 18 {
		t.Fatalf("RecoveryWorkFactor = %d", RecoveryWorkFactor)
	}
	data, err := SealKeyCopy(make([]byte, 32), testPassword, testWorkFactor)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitN(string(data), "\n", 3)
	if len(lines) < 3 || lines[0] != "age-encryption.org/v1" || !strings.HasPrefix(lines[1], "-> scrypt ") || !strings.HasSuffix(lines[1], " 10") {
		t.Fatalf("header %q", lines[:2])
	}
}

func TestKeyCopyRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	data, err := SealKeyCopy(key, testPassword, testWorkFactor)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("age-encryption.org/v1\n-> scrypt ")) {
		t.Fatalf("not a binary age file with a passphrase recipient: %q", data[:40])
	}
	got, err := OpenKeyCopy(data, testPassword)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("open = %x, %v", got, err)
	}
	if _, err := OpenKeyCopy(data, testPassword+"!"); !errors.Is(err, ErrRecoveryPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	damaged := bytes.Clone(data)
	damaged[len(damaged)-1] ^= 1
	if _, err := OpenKeyCopy(damaged, testPassword); err == nil || errors.Is(err, ErrRecoveryPassword) {
		t.Fatalf("damaged payload: %v", err)
	}
	if _, err := OpenKeyCopy([]byte("not age"), testPassword); err == nil || errors.Is(err, ErrRecoveryPassword) {
		t.Fatalf("not an age file: %v", err)
	}

	// The armored form opens too.
	var armored bytes.Buffer
	aw := armor.NewWriter(&armored)
	aw.Write(data)
	aw.Close()
	if got, err := OpenKeyCopy(armored.Bytes(), testPassword); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("armored: %v", err)
	}

	// A file for a key pair has no passphrase recipient.
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, _ := age.Encrypt(&buf, id.Recipient())
	w.Write(key)
	w.Close()
	if _, err := OpenKeyCopy(buf.Bytes(), testPassword); !errors.Is(err, ErrRecoveryPassword) {
		t.Fatalf("no passphrase recipient: %v", err)
	}

	// The plaintext must be a whole key.
	short, err := SealKeyCopy(key[:31], testPassword, testWorkFactor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenKeyCopy(short, testPassword); err == nil || !strings.Contains(err.Error(), "31 bytes") {
		t.Fatalf("short key: %v", err)
	}
}

func TestCheckKey(t *testing.T) {
	ctx := context.Background()
	s, dir := newTestService(t)
	if _, err := s.SaveVersion(ctx, "app", []bundle.File{{Path: "index.html", Data: []byte("x")}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"API_KEY", "DB_URL"} {
		if err := s.SetSecret(ctx, "app", name, "v-"+name, ViaCLI); err != nil {
			t.Fatal(err)
		}
	}
	if err := PutHostKeyValue(ctx, dir, s.st, HostKeyZrok, "zrok-token", KeySourceCLI, time.Now()); err != nil {
		t.Fatal(err)
	}
	// A record of a connection holds no value and is not counted.
	if err := s.st.PutHostKey(ctx, store.HostKey{ID: HostKeyNetbird, Source: KeySourceCLI, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	key, err := ReadSecretKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := CheckKey(ctx, s.st, key); err != nil || c.Opened != 3 || c.Total != 3 || !c.All() || len(c.Unopened) != 0 {
		t.Fatalf("own key: %+v %v", c, err)
	}
	other := make([]byte, 32)
	rand.Read(other)
	c, err := CheckKey(ctx, s.st, other)
	if err != nil || c.Opened != 0 || c.Total != 3 || c.All() {
		t.Fatalf("other key: %+v %v", c, err)
	}
	var names []string
	for _, n := range c.Unopened {
		names = append(names, n.String())
	}
	if got := strings.Join(names, ", "); got != "secret API_KEY of app, secret DB_URL of app, operator key zrok" {
		t.Fatalf("unopened: %s", got)
	}
	// A row whose nonce has the wrong size opens nothing and does not panic.
	if err := s.st.PutSecret(ctx, "app", store.SealedSecret{Name: "BROKEN", Nonce: []byte{1}, Ciphertext: []byte{2}, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if c, err := CheckKey(ctx, s.st, key); err != nil || c.Opened != 3 || c.Total != 4 || len(c.Unopened) != 1 || c.Unopened[0].Name != "BROKEN" {
		t.Fatalf("damaged row: %+v %v", c, err)
	}
}

func TestRestoreSecretKeyNeverReplaces(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	dir := t.TempDir()
	path := filepath.Join(dir, SecretKeyFile)
	if err := RestoreSecretKey(dir, key); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("restored %x %v", got, err)
	}
	if fi, err := os.Stat(path); err != nil || ostest.WrongMode(fi, 0o600) {
		t.Fatalf("mode: %v %v", fi, err)
	}
	other := make([]byte, 32)
	rand.Read(other)
	if err := RestoreSecretKey(dir, other); !os.IsExist(err) {
		t.Fatalf("over a key: %v", err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, key) {
		t.Fatal("the existing key changed")
	}

	dangling := t.TempDir()
	if err := ostest.Symlink(t, filepath.Join(dangling, "gone"), filepath.Join(dangling, SecretKeyFile)); err != nil {
		t.Fatal(err)
	}
	if err := RestoreSecretKey(dangling, key); !os.IsExist(err) {
		t.Fatalf("over a dangling link: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dangling, "gone")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("restore wrote through the link")
	}
	for _, d := range []string{dir, dangling} {
		names, _ := os.ReadDir(d)
		for _, n := range names {
			if strings.HasPrefix(n.Name(), ".secret-key-") {
				t.Fatalf("temporary file left in %s: %s", d, n.Name())
			}
		}
	}
}

func TestWriteKeyCopy(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	rand.Read(key)
	if replaced, err := WriteKeyCopy(dir, []byte("one"), key); err != nil || replaced {
		t.Fatalf("first: %v %v", replaced, err)
	}
	if replaced, err := WriteKeyCopy(dir, []byte("two"), key); err != nil || !replaced {
		t.Fatalf("second: %v %v", replaced, err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, RecoveryKeyFile)); err != nil || string(got) != "two" {
		t.Fatalf("content %q %v", got, err)
	}
	id, err := os.ReadFile(filepath.Join(dir, RecoveryKeyIDFile))
	if err != nil || string(id) != KeyID(key)+"\n" || len(KeyID(key)) != 32 {
		t.Fatalf("key ID %q %v", id, err)
	}
	for _, name := range []string{RecoveryKeyFile, RecoveryKeyIDFile} {
		if fi, err := os.Stat(filepath.Join(dir, name)); err != nil || ostest.WrongMode(fi, 0o600) {
			t.Fatalf("mode of %s: %v %v", name, fi, err)
		}
	}
	names, _ := os.ReadDir(dir)
	if len(names) != 2 {
		t.Fatalf("files left: %v", names)
	}
}

func TestRecoveryCopyWarning(t *testing.T) {
	dir := t.TempDir()
	key, other := make([]byte, 32), make([]byte, 32)
	rand.Read(key)
	rand.Read(other)
	if w := RecoveryCopyWarning(dir, key); w != "" {
		t.Fatalf("no copy: %q", w)
	}
	if _, err := WriteKeyCopy(dir, []byte("copy"), key); err != nil {
		t.Fatal(err)
	}
	if w := RecoveryCopyWarning(dir, key); w != "" {
		t.Fatalf("matching copy: %q", w)
	}
	if w := RecoveryCopyWarning(dir, other); !strings.Contains(w, "flats secret backup-key") {
		t.Fatalf("another key: %q", w)
	}
	if err := os.Remove(filepath.Join(dir, RecoveryKeyIDFile)); err != nil {
		t.Fatal(err)
	}
	if w := RecoveryCopyWarning(dir, key); !strings.Contains(w, "flats secret backup-key") {
		t.Fatalf("no key ID: %q", w)
	}
}

// The start names the commands that bring the host back.
func TestMissingKeyRefusalNamesRecovery(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	if _, err := s.SaveVersion(ctx, "app", []bundle.File{{Path: "index.html", Data: []byte("x")}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSecret(ctx, "app", "API_KEY", "v", ViaCLI); err != nil {
		t.Fatal(err)
	}
	err := CheckSecretKey(ctx, t.TempDir(), s.st)
	if !errors.Is(err, ErrSecretKeyMissing) || !strings.Contains(err.Error(), "flats secret restore-key") || !strings.Contains(err.Error(), "flats secret reset") {
		t.Fatalf("refusal: %v", err)
	}
}

func TestInvalidSecretKeyError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, SecretKeyFile), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecretKey(dir); !errors.Is(err, ErrInvalidSecretKey) || !strings.Contains(err.Error(), "must be 32 bytes") {
		t.Fatalf("short key: %v", err)
	}
	if _, err := ReadSecretKey(t.TempDir()); errors.Is(err, ErrInvalidSecretKey) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing key: %v", err)
	}
}

// A stored operator key that the key does not open names the way out.
func TestUnopenedHostKeyNamesRecovery(t *testing.T) {
	dir := t.TempDir()
	k, err := SealHostKey(dir, HostKeyTailscale, "tskey-auth-x", KeySourceCLI, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, SecretKeyFile), bytes.Repeat([]byte{1}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = OpenHostKey(dir, k)
	if err == nil || !strings.Contains(err.Error(), "does not match flats.db") || !strings.Contains(err.Error(), "flats secret restore-key") || !strings.Contains(err.Error(), "flats key rm") {
		t.Fatalf("open: %v", err)
	}
}
