package app

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/ostest"
	"github.com/gosuda/flats/internal/store"
)

// docs/spec/secret-key-rotation.md: `flats secret rotate-key`. The tests of the command set the package variables
// rotateHook and openTerminal, so none of them runs in parallel.

// errStopped is what a hook returns to stop the command at a point, as a stop of the process would.
var errStopped = errors.New("stopped by the test")

// rotateWith runs `flats secret rotate-key` with hook as rotateHook.
func rotateWith(t *testing.T, config string, hook func(point string) error, args ...string) (string, error) {
	t.Helper()
	old := rotateHook
	rotateHook = hook
	defer func() { rotateHook = old }()
	return runSecretKey(t, config, append([]string{"rotate-key"}, args...)...)
}

// stopAt is a hook that stops the command at point.
func stopAt(point string) func(string) error {
	return func(p string) error {
		if p == point {
			return errStopped
		}
		return nil
	}
}

// failAt is a hook that fails the operation that point names with err.
func failAt(point string, err error) func(string) error {
	return func(p string) error {
		if p == point {
			return err
		}
		return nil
	}
}

// rotatePrompter is a terminal that gives the recovery password and one answer, and counts the questions.
type rotatePrompter struct {
	password, answer string
	hiddenAsked      int
	lines            []string // the questions that were asked on a line
}

func (p *rotatePrompter) hidden(string) (string, error) { p.hiddenAsked++; return p.password, nil }
func (p *rotatePrompter) line(q string) (string, error) {
	p.lines = append(p.lines, q)
	return p.answer, nil
}

// useTerminal makes the commands see p as their terminal.
func useTerminal(t *testing.T, p prompter) {
	t.Helper()
	old := openTerminal
	openTerminal = func(context.Context, io.Reader, io.Writer) prompter { return p }
	t.Cleanup(func() { openTerminal = old })
}

// The values of the host of the tests.
const (
	rotSecretApp   = "hunter2-value"
	rotSecretSite  = "site-token-value"
	rotSecretOther = "site-other-value"
	rotNetbirdKey  = "8f3c2a1e-4b5d-4c6e-9f70-0123456789ab"
	rotSealed      = 5 // three flat secrets and two operator keys
)

// rotHost is a stopped host with flat secrets of two flats, two operator keys and an operator record without a
// value.
type rotHost struct {
	dir, config string
	keyPath     string
	key         []byte // the key before the rotation
}

func (h *rotHost) pending() string { return h.keyPath + ".next" }

func newRotHost(t *testing.T) *rotHost {
	t.Helper()
	clearKeyEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	config, key, h := hostWithSecrets(t, dir)
	if _, err := h.Svc.SaveVersion(ctx, "site", []bundle.File{{Path: "index.html", Data: []byte("y")}}, core.SaveMeta{}, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"SITE_TOKEN": rotSecretSite, "SITE_OTHER": rotSecretOther} {
		if err := h.Svc.SetSecret(ctx, "site", name, value, core.ViaCLI); err != nil {
			t.Fatal(err)
		}
	}
	for id, value := range map[string]string{core.HostKeyTailscale: keyA, core.HostKeyNetbird: rotNetbirdKey} {
		if err := core.PutHostKeyValue(ctx, h.keyFile(), h.Store, id, value, core.KeySourceCLI, time.Now(), store.Audit{}); err != nil {
			t.Fatal(err)
		}
	}
	// A connected account keeps a record without a value. It takes the place of the waiting zrok token of
	// hostWithSecrets, which would make every later start of the host call the zrok service.
	if err := h.Store.PutHostKey(ctx, store.HostKey{ID: core.HostKeyZrok, Source: core.KeySourceConsole, UpdatedAt: time.Now()}, store.Audit{}); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	return &rotHost{dir: dir, config: config, keyPath: filepath.Join(dir, core.SecretKeyFile), key: key}
}

// outside moves the key of the host out of the data directory and sets host.secret_key_path.
func (h *rotHost) outside(t *testing.T) *rotHost {
	t.Helper()
	h.keyPath = keyOutside(t, h.dir, h.config)
	return h
}

func (h *rotHost) rotate(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return rotateWith(t, h.config, nil, args...)
}

func (h *rotHost) mustRotate(t *testing.T, args ...string) string {
	t.Helper()
	out, err := h.rotate(t, args...)
	if err != nil {
		t.Fatalf("rotate-key %v: %v\n%s", args, err, out)
	}
	return out
}

// keyAt reads the key file of the host.
func (h *rotHost) keyAt(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(h.keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// sealedRow is one row that holds a sealed value, or an operator record without one.
type sealedRow struct {
	name              string // "app/API_KEY", or "key:zrok"
	nonce, ciphertext []byte
	keyID             string
	rest              string // what a rotation must not change: the time, and the source and the error of a record
	aad               []byte
}

func sealedRows(t *testing.T, dir string) []sealedRow {
	t.Helper()
	ctx := context.Background()
	st := openStore(t, dir)
	defer st.Close()
	secs, err := st.ListAllSecrets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := st.ListHostKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []sealedRow
	for _, s := range secs {
		out = append(out, sealedRow{name: s.Flat + "/" + s.Name, nonce: s.Nonce, ciphertext: s.Ciphertext, keyID: s.KeyID,
			rest: s.UpdatedAt.String(), aad: []byte(s.Name)})
	}
	for _, k := range keys {
		out = append(out, sealedRow{name: "key:" + k.ID, nonce: k.Nonce, ciphertext: k.Ciphertext, keyID: k.KeyID,
			rest: k.UpdatedAt.String() + " " + k.Source + " " + k.LastError, aad: []byte("host-key\x00" + k.ID)})
	}
	return out
}

// rowsText is every column of the rows, so that a test can show that nothing changed.
func rowsText(rows []sealedRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%s nonce=%x ct=%x id=%s %s", r.name, r.nonce, r.ciphertext, r.keyID, r.rest))
	}
	return out
}

// openedWith opens every sealed value of the host with key. It returns the values that opened, by name, and the
// names of those that did not.
func openedWith(t *testing.T, dir string, key []byte) (values map[string]string, failed []string) {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	values = map[string]string{}
	for _, r := range sealedRows(t, dir) {
		if r.ciphertext == nil {
			continue // a record without a value
		}
		pt, err := g.Open(nil, r.nonce, r.ciphertext, r.aad)
		if err != nil {
			failed = append(failed, r.name)
			continue
		}
		values[r.name] = string(pt)
	}
	return values, failed
}

// rotValues are the values of newRotHost as openedWith returns them.
func rotValues() map[string]string {
	return map[string]string{"app/API_KEY": rotSecretApp, "site/SITE_TOKEN": rotSecretSite, "site/SITE_OTHER": rotSecretOther,
		"key:netbird": rotNetbirdKey, "key:tailscale": keyA}
}

// mustHoldValues fails the test unless key opens every value of the host and the values are the ones of newRotHost,
// each with the key ID of key.
func mustHoldValues(t *testing.T, dir string, key []byte) {
	t.Helper()
	values, failed := openedWith(t, dir, key)
	if len(failed) > 0 {
		t.Fatalf("the key %s does not open %v", core.KeyID(key), failed)
	}
	if want := rotValues(); !mapsEqual(values, want) {
		t.Fatalf("the opened values are %v, want %v", values, want)
	}
	for _, r := range sealedRows(t, dir) {
		if r.ciphertext != nil && r.keyID != core.KeyID(key) {
			t.Fatalf("%s carries the key ID %q, want %s", r.name, r.keyID, core.KeyID(key))
		}
	}
}

// startLogs starts the host and stops it again. It returns what the start logged.
func startLogs(t *testing.T, config string) (string, error) {
	t.Helper()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	h, err := Start(context.Background(), configModeOptions(config))
	if err != nil {
		return logs.String(), err
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	return logs.String(), nil
}

// mustStart starts the host, shows that its service opens every value with the key at the key path, and stops it.
func mustStart(t *testing.T, h *rotHost, total int) string {
	t.Helper()
	ctx := context.Background()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	host, err := Start(ctx, configModeOptions(h.config))
	if err != nil {
		t.Fatalf("the host does not start: %v", err)
	}
	defer host.Close()
	if c, err := core.CheckKey(ctx, host.Store, h.keyAt(t)); err != nil || !c.All() || c.Total != total {
		t.Fatalf("the started host opens %+v, %v; want all %d values", c, err, total)
	}
	return logs.String()
}

// mustRefuseStart shows that the start refuses with status 78 and a message that holds each of want.
func mustRefuseStart(t *testing.T, config string, want ...string) error {
	t.Helper()
	_, err := startLogs(t, config)
	if err == nil {
		t.Fatal("the host started")
	}
	if exitCode(err) != ExitConfig {
		t.Fatalf("the start failed with status %d (%v), want %d", exitCode(err), err, ExitConfig)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("the refusal of the start lacks %q: %v", w, err)
		}
	}
	return err
}

// leaks names what of the keys and the values of the host shows in text, or "" when nothing does.
func leaks(text string, keys ...[]byte) string {
	for _, k := range keys {
		for _, form := range []string{string(k), hex.EncodeToString(k), strings.ToUpper(hex.EncodeToString(k)),
			base64.StdEncoding.EncodeToString(k), base64.RawStdEncoding.EncodeToString(k), base64.URLEncoding.EncodeToString(k)} {
			if strings.Contains(text, form) {
				return "a key"
			}
		}
	}
	for name, v := range rotValues() {
		if strings.Contains(text, v) {
			return "the value of " + name
		}
	}
	return ""
}

// rotateEvents returns the events of kind in the audit log of the host.
func eventsOf(t *testing.T, dir, kind string) []store.AuditEvent {
	t.Helper()
	var out []store.AuditEvent
	for _, e := range auditIn(t, dir) {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func eventData(t *testing.T, e store.AuditEvent) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Data, &m); err != nil {
		t.Fatalf("the data of %s: %v", e.Kind, err)
	}
	return m
}

// A rotation of a host with flat secrets of two flats and operator keys: the key file holds a new key and is the
// same file, every value opens with the new key alone and holds the same bytes, a record without a value is as it
// was, nothing is left beside the key, and a host that starts afterwards opens the secrets of its flats. The output
// is the one of rules 28 and 29, and the transaction wrote one key.rotate event (rule 30).
func TestRotateKeySealsEveryValueUnderANewKey(t *testing.T) {
	h := newRotHost(t)
	if !ostest.Windows {
		if err := os.Chmod(h.keyPath, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	fiBefore, err := os.Stat(h.keyPath)
	if err != nil {
		t.Fatal(err)
	}
	before := sealedRows(t, h.dir)
	names := listed(t, h.dir)
	events := len(auditIn(t, h.dir))
	mustHoldValues(t, h.dir, h.key)

	out := h.mustRotate(t, "--yes")

	newKey := h.keyAt(t)
	if len(newKey) != 32 || bytes.Equal(newKey, h.key) {
		t.Fatalf("the key file holds %d bytes, the old key: %v", len(newKey), bytes.Equal(newKey, h.key))
	}
	fiAfter, err := os.Stat(h.keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(fiBefore, fiAfter) {
		t.Error("the key file is another file after the rotation: it was replaced")
	}
	if ostest.WrongPerm(fiAfter.Mode().Perm(), fiBefore.Mode().Perm()) {
		t.Errorf("the key file has mode %v, and had %v", fiAfter.Mode().Perm(), fiBefore.Mode().Perm())
	}
	if a, b := ownerOf(fiBefore), ownerOf(fiAfter); a != b {
		t.Errorf("the key file belongs to %s, and belonged to %s", b, a)
	}

	// Every value opens with the new key and carries its ID, with the bytes that it had. None opens with the old key.
	mustHoldValues(t, h.dir, newKey)
	if values, failed := openedWith(t, h.dir, h.key); len(values) != 0 || len(failed) != rotSealed {
		t.Errorf("the old key still opens %v", values)
	}
	// The seal of each value is new, and nothing else of its row changed. The record without a value is as it was.
	after := sealedRows(t, h.dir)
	if len(after) != len(before) {
		t.Fatalf("%d rows after the rotation, %d before", len(after), len(before))
	}
	for i, b := range before {
		a := after[i]
		if a.name != b.name || a.rest != b.rest {
			t.Errorf("row %s changed apart from its seal: %q -> %q", b.name, b.rest, a.rest)
		}
		if b.ciphertext == nil {
			if a.ciphertext != nil || a.nonce != nil || a.keyID != "" || b.name != "key:zrok" {
				t.Errorf("the record without a value changed: %+v", a)
			}
			continue
		}
		if bytes.Equal(a.nonce, b.nonce) || bytes.Equal(a.ciphertext, b.ciphertext) {
			t.Errorf("%s kept its nonce or its ciphertext", b.name)
		}
	}
	// No pending key and no temporary file stays.
	if got := listed(t, h.dir); !slices.Equal(got, names) {
		t.Errorf("the data directory holds %v after the rotation, and held %v", got, names)
	}
	noTemporaryKeyFile(t, h.dir)

	// The output (rules 28 and 29).
	for _, want := range []string{
		"sealed 3 flat secrets and 2 operator keys again",
		"the old key has the key ID " + core.KeyID(h.key),
		"the new key has the key ID " + core.KeyID(newKey),
		"wrote the new key into " + h.keyPath,
		"there is no recovery copy", "`flats secret backup-key`",
		"opens only with the old key", "a real copy and not a link",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	if what := leaks(out, h.key, newKey); what != "" {
		t.Errorf("the output shows %s:\n%s", what, out)
	}

	// One key.rotate event, and no event for a single value (rule 30).
	got := newAudit(t, h.dir, events)
	if len(got) != 1 || got[0].Kind != store.AuditKeyRotate {
		t.Fatalf("the rotation wrote %v, want one key.rotate", auditSummary(got))
	}
	e := got[0]
	if e.Via != store.AuditViaCommand || e.Actor != operatorUser() || e.Flat != "" || e.Name != "" {
		t.Errorf("event = %+v; want the channel command, the operating-system user, no flat and no name", e)
	}
	m := eventData(t, e)
	if m["from"] != core.KeyID(h.key) || m["to"] != core.KeyID(newKey) || m["secrets"] != float64(3) || m["keys"] != float64(2) || m["out"] != false || len(m) != 5 {
		t.Errorf("the data of key.rotate = %s", e.Data)
	}

	// A host that starts afterwards opens the secrets of its flats, and seals a new one under the same key.
	logs := mustStart(t, h, rotSealed)
	if strings.Contains(logs, "flats secret backup-key") {
		t.Errorf("the start warns without a recovery copy:\n%s", logs)
	}
	host, err := Start(context.Background(), configModeOptions(h.config))
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	for flat, n := range map[string]int{"app": 1, "site": 2} {
		if names, err := host.Svc.SecretNames(context.Background(), flat); err != nil || len(names) != n {
			t.Errorf("the secrets of %s after the rotation: %v, %v", flat, names, err)
		}
	}
	if err := host.Svc.SetSecret(context.Background(), "app", "LATER", "later-value", core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	if c, err := core.CheckKey(context.Background(), host.Store, newKey); err != nil || !c.All() || c.Total != rotSealed+1 {
		t.Errorf("after a new secret the key opens %+v, %v", c, err)
	}
}

// With no sealed value there is nothing to check, and the command still makes a new key and writes the event (rules
// 9 and 30).
func TestRotateKeyWithNoSealedValue(t *testing.T) {
	dir := t.TempDir()
	config := initConfig(t, dir)
	if _, err := startLogs(t, config); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, core.SecretKeyFile)
	old, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	out, err := rotateWith(t, config, nil, "--yes")
	if err != nil {
		t.Fatalf("rotate-key: %v\n%s", err, out)
	}
	fresh, err := os.ReadFile(keyPath)
	if err != nil || len(fresh) != 32 || bytes.Equal(fresh, old) {
		t.Fatalf("the key file after the rotation: %d bytes, %v", len(fresh), err)
	}
	if !strings.Contains(out, "sealed 0 flat secrets and 0 operator keys again") {
		t.Errorf("the output:\n%s", out)
	}
	evs := eventsOf(t, dir, store.AuditKeyRotate)
	if len(evs) != 1 {
		t.Fatalf("%d key.rotate events, want 1", len(evs))
	}
	if m := eventData(t, evs[0]); m["secrets"] != float64(0) || m["keys"] != float64(0) || m["to"] != core.KeyID(fresh) {
		t.Errorf("the data of key.rotate = %s", evs[0].Data)
	}
	if _, err := startLogs(t, config); err != nil {
		t.Errorf("the host does not start after the rotation: %v", err)
	}
}

// The confirmation of rule 10: on a terminal the command shows what it will do and asks for the word rotate. Any
// other answer changes nothing and ends with status 0. Without a terminal it needs --yes, which also skips the
// question on a terminal.
func TestRotateKeyAsksBeforeItWrites(t *testing.T) {
	h := newRotHost(t)
	before := rowsText(sealedRows(t, h.dir))
	names := listed(t, h.dir)
	unchanged := func(when string) {
		t.Helper()
		if got := rowsText(sealedRows(t, h.dir)); !slices.Equal(got, before) {
			t.Fatalf("%s: the sealed values changed", when)
		}
		if got := h.keyAt(t); !bytes.Equal(got, h.key) {
			t.Fatalf("%s: the key file changed", when)
		}
		if got := listed(t, h.dir); !slices.Equal(got, names) {
			t.Fatalf("%s: the data directory holds %v, and held %v", when, got, names)
		}
		if n := len(eventsOf(t, h.dir, store.AuditKeyRotate)); n != 0 {
			t.Fatalf("%s: %d key.rotate events", when, n)
		}
	}

	// No terminal and no --yes: a usage error.
	out, err := h.rotate(t)
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("without a terminal and without --yes: %v (status %d)\n%s", err, exitCode(err), out)
	}
	unchanged("the missing --yes")

	// Another answer than rotate.
	for _, answer := range []string{"yes", "", "Rotate", "reset"} {
		p := &rotatePrompter{answer: answer}
		useTerminal(t, p)
		out, err := h.rotate(t)
		if err != nil || !strings.Contains(out, "Nothing was changed.") {
			t.Fatalf("the answer %q: %v\n%s", answer, err, out)
		}
		if len(p.lines) != 1 || !strings.Contains(p.lines[0], "rotate") || p.hiddenAsked != 0 {
			t.Fatalf("the answer %q: the questions were %q, and %d for a password", answer, p.lines, p.hiddenAsked)
		}
		// What the operator sees before the question: the number of values, the file, and the note of rule 29.
		for _, want := range []string{"5 sealed values", "into " + h.keyPath, "cannot be reversed", "copy the old key away first", "a real copy and not a link"} {
			if !strings.Contains(out, want) {
				t.Errorf("the confirmation lacks %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "left over") {
			t.Errorf("the confirmation names a pending key that is not there:\n%s", out)
		}
		unchanged("the answer " + answer)
	}

	// --yes skips the question on a terminal.
	p := &rotatePrompter{answer: "no"}
	useTerminal(t, p)
	if out, err := h.rotate(t, "--yes"); err != nil {
		t.Fatalf("--yes on a terminal: %v\n%s", err, out)
	}
	if len(p.lines) != 0 {
		t.Errorf("--yes still asked %q", p.lines)
	}
	second := h.keyAt(t)
	if bytes.Equal(second, h.key) {
		t.Fatal("--yes did not rotate")
	}

	// The word rotate, with white space around it.
	p = &rotatePrompter{answer: " rotate\n"}
	useTerminal(t, p)
	if out, err := h.rotate(t); err != nil {
		t.Fatalf("the answer rotate: %v\n%s", err, out)
	}
	if third := h.keyAt(t); bytes.Equal(third, second) {
		t.Fatal("the answer rotate did not rotate")
	}
	mustHoldValues(t, h.dir, h.keyAt(t))
	if n := len(eventsOf(t, h.dir, store.AuditKeyRotate)); n != 2 {
		t.Errorf("%d key.rotate events after two rotations", n)
	}
}

// A usage mistake exits with status 2 and changes nothing (rule 5).
func TestRotateKeyUsageMistakes(t *testing.T) {
	h := newRotHost(t)
	for _, args := range [][]string{{"--no-such-flag"}, {"extra"}, {"--yes", "more"}, {"--out"}} {
		_, err := h.rotate(t, args...)
		if exitCode(err) != 2 {
			t.Errorf("rotate-key %v: %v (status %d), want a usage error", args, err, exitCode(err))
		}
	}
	if got := h.keyAt(t); !bytes.Equal(got, h.key) {
		t.Error("a usage mistake changed the key")
	}
}
