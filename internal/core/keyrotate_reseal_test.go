package core

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/store"
)

// docs/spec/secret-key-rotation.md: the key ID on each value that the host seals (rules 1 to 3), and the re-seal of
// rule 15.

// rotationHost is a service with values of two flats and two operator keys, and a record without a value.
type rotationHost struct {
	s      *Service
	dir    string
	key    []byte
	plain  map[SealedName]string
	record store.HostKey // the record without a value
}

func newRotationHost(t *testing.T) rotationHost {
	t.Helper()
	ctx := context.Background()
	s, dir := newTestService(t)
	h := rotationHost{s: s, dir: dir, plain: map[SealedName]string{
		{Flat: "alpha", Name: "FIRST"}:  "the first value",
		{Flat: "alpha", Name: "SECOND"}: "the second value",
		{Flat: "bravo", Name: "FIRST"}:  "a value of the other flat",
		{Name: HostKeyZrok}:             "value-for-zrok",
		{Name: HostKeyWebhookSecret}:    "value-for-the-webhook",
	}}
	for _, flat := range []string{"alpha", "bravo"} {
		if _, err := s.CreateFlat(ctx, flat, "", ViaAPI); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range h.plain {
		var err error
		if name.Flat == "" {
			err = PutHostKeyValue(ctx, NewKeyFile(dir, ""), s.st, name.Name, value, KeySourceCLI, time.Now(), store.Audit{})
		} else {
			err = s.SetSecret(ctx, name.Flat, name.Name, value, ViaCLI)
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := s.st.PutHostKey(ctx, store.HostKey{ID: HostKeyNetbird, Source: KeySourceConsole, UpdatedAt: time.Now()}, store.Audit{}); err != nil {
		t.Fatal(err)
	}
	var err error
	if h.record, err = s.st.GetHostKey(ctx, HostKeyNetbird); err != nil {
		t.Fatal(err)
	}
	if h.key, err = NewKeyFile(dir, "").Read(); err != nil {
		t.Fatal(err)
	}
	return h
}

// opened opens every sealed value of the store with key, as the host does, and returns the plain values. A value that
// does not open is left out.
func (h rotationHost) opened(t *testing.T, key []byte) map[SealedName]string {
	t.Helper()
	ctx := context.Background()
	g, err := hostKeyAEAD(key)
	if err != nil {
		t.Fatal(err)
	}
	out := map[SealedName]string{}
	secs, err := h.s.st.ListAllSecrets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sec := range secs {
		if pt, err := g.Open(nil, sec.Nonce, sec.Ciphertext, []byte(sec.Name)); err == nil {
			out[SealedName{sec.Flat, sec.Name}] = string(pt)
		}
	}
	recs, err := h.s.st.ListHostKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if !rec.HasValue() {
			continue
		}
		if pt, err := g.Open(nil, rec.Nonce, rec.Ciphertext, hostKeyAAD(rec.ID)); err == nil {
			out[SealedName{Name: rec.ID}] = string(pt)
		}
	}
	return out
}

// rows returns every row that holds or can hold a sealed value, to compare the state before and after.
func (h rotationHost) rows(t *testing.T) ([]store.FlatSecret, []store.HostKey) {
	t.Helper()
	secs, err := h.s.st.ListAllSecrets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	recs, err := h.s.st.ListHostKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return secs, recs
}

// rotateEvents returns the data of the key.rotate events in the audit log.
func (h rotationHost) rotateEvents(t *testing.T) []map[string]any {
	t.Helper()
	rd, err := store.OpenAuditReader(filepath.Join(h.dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	evs, err := rd.List(context.Background(), store.AuditQuery{Kind: store.AuditKeyRotate}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, e := range evs {
		if e.Flat != "" || e.Name != "" || e.Via != store.AuditViaCommand {
			t.Errorf("event = %+v; want no flat, no name and the channel of a command", e)
		}
		var data map[string]any
		if err := json.Unmarshal(e.Data, &data); err != nil {
			t.Fatal(err)
		}
		out = append(out, data)
	}
	return out
}

// Each writer of a sealed value stores the key ID of the host key in the row (rule 2).
func TestSealedValuesCarryTheKeyIDOfTheHostKey(t *testing.T) {
	h := newRotationHost(t)
	id := KeyID(h.key)
	if h.s.secretKeyID != id {
		t.Errorf("the service holds the key ID %q, want %q", h.s.secretKeyID, id)
	}
	secs, recs := h.rows(t)
	if len(secs) != 3 {
		t.Fatalf("%d flat secrets, want 3", len(secs))
	}
	for _, sec := range secs {
		if sec.KeyID != id {
			t.Errorf("secret %s of %s carries the key ID %q, want %q", sec.Name, sec.Flat, sec.KeyID, id)
		}
	}
	for _, rec := range recs {
		if want := map[bool]string{true: id, false: ""}[rec.HasValue()]; rec.KeyID != want {
			t.Errorf("operator key %s carries the key ID %q, want %q", rec.ID, rec.KeyID, want)
		}
	}
	ids, err := h.s.st.SealedKeyIDs(context.Background())
	if single, ok := ids.Single(); err != nil || !ok || single != id || ids.Total() != 5 {
		t.Errorf("SealedKeyIDs = %v, %v; want five values under %s", ids, err, id)
	}
}

// A value from before the key IDs has no label and opens as before: the host tries the key (rule 3).
func TestAValueWithoutAKeyIDStillOpens(t *testing.T) {
	ctx := context.Background()
	h := newRotationHost(t)
	secs, recs := h.rows(t)
	for _, sec := range secs {
		sec.KeyID = ""
		if err := h.s.st.PutSecret(ctx, sec.Flat, sec.SealedSecret, store.Audit{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, rec := range recs {
		rec.KeyID = ""
		if err := h.s.st.PutHostKey(ctx, rec, store.Audit{}); err != nil {
			t.Fatal(err)
		}
	}
	if ids, err := h.s.st.SealedKeyIDs(ctx); err != nil || ids[""] != 5 || ids.Total() != 5 {
		t.Fatalf("SealedKeyIDs = %v, %v; want five values without a label", ids, err)
	}
	alpha, err := h.s.st.ListSecrets(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	env, err := h.s.decryptSecrets(alpha)
	if err != nil || env["FIRST"] != "the first value" || env["SECOND"] != "the second value" {
		t.Errorf("the flat secrets without a label: %v, %v", len(env), err)
	}
	if v, _, err := StoredHostKey(ctx, NewKeyFile(h.dir, ""), h.s.st, HostKeyZrok); err != nil || v != "value-for-zrok" {
		t.Errorf("the operator key without a label: %v", err)
	}
	// The start check has nothing to say about values without a label, also with another key at the key path: such
	// a value fails when it is used, as before.
	if err := CheckSecretKey(ctx, NewKeyFile(h.dir, ""), h.s.st); err != nil {
		t.Errorf("CheckSecretKey = %v", err)
	}
	elsewhere := t.TempDir()
	putFile(t, filepath.Join(elsewhere, SecretKeyFile), keyOf(8))
	if err := CheckSecretKey(ctx, NewKeyFile(elsewhere, ""), h.s.st); err != nil {
		t.Errorf("CheckSecretKey with another key = %v", err)
	}
	if _, _, err := StoredHostKey(ctx, NewKeyFile(elsewhere, ""), h.s.st, HostKeyZrok); err == nil {
		t.Error("another key opened the value")
	}
}

// When a value does not open and carries the ID of another key, the error names both IDs. A value with the ID of the
// key, and one without a label, keep the error from before (rule 3).
func TestAValueThatDoesNotOpenNamesBothKeyIDs(t *testing.T) {
	ctx := context.Background()
	h := newRotationHost(t)
	other := keyOf(8)
	sealedBy, tried := KeyID(h.key), KeyID(other)
	both := func(err error) bool {
		return err != nil && strings.Contains(err.Error(), sealedBy) && strings.Contains(err.Error(), tried)
	}
	neither := func(err error) bool {
		return err != nil && !strings.Contains(err.Error(), sealedBy) && !strings.Contains(err.Error(), tried) && !strings.Contains(err.Error(), "key ID")
	}

	// A flat secret, with another key in the service.
	alpha, err := h.s.st.ListSecrets(ctx, "alpha")
	if err != nil || len(alpha) != 2 {
		t.Fatalf("ListSecrets = %v, %v", len(alpha), err)
	}
	own, ownID := h.s.secretKey, h.s.secretKeyID
	h.s.secretKey, h.s.secretKeyID = other, tried
	if _, err := h.s.decryptSecrets(alpha); !both(err) || !strings.Contains(err.Error(), "decrypt secret FIRST") {
		t.Errorf("a flat secret under another key: err = %v, want both key IDs", err)
	}
	unlabelled := []store.SealedSecret{alpha[0]}
	unlabelled[0].KeyID = ""
	if _, err := h.s.decryptSecrets(unlabelled); !neither(err) {
		t.Errorf("a flat secret without a label: err = %v, want the error from before", err)
	}
	h.s.secretKey, h.s.secretKeyID = own, ownID
	damaged := []store.SealedSecret{alpha[0]}
	damaged[0].Ciphertext = append(bytes.Clone(alpha[0].Ciphertext), 0)
	if _, err := h.s.decryptSecrets(damaged); !neither(err) {
		t.Errorf("a damaged flat secret with the ID of the key: err = %v, want the error from before", err)
	}

	// An operator key, with another key at the key path.
	rec, err := h.s.st.GetHostKey(ctx, HostKeyZrok)
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	putFile(t, filepath.Join(elsewhere, SecretKeyFile), other)
	_, err = OpenHostKey(NewKeyFile(elsewhere, ""), rec)
	if !both(err) {
		t.Errorf("an operator key under another key: err = %v, want both key IDs", err)
	}
	// The advice of the message stays.
	for _, part := range []string{"decrypt the stored zrok key", "does not match flats.db", "flats secret restore-key", "flats key rm"} {
		if err == nil || !strings.Contains(err.Error(), part) {
			t.Errorf("the message lacks %q: %v", part, err)
		}
	}
	bare := rec
	bare.KeyID = ""
	if _, err := OpenHostKey(NewKeyFile(elsewhere, ""), bare); !neither(err) || !strings.Contains(err.Error(), "does not match flats.db") {
		t.Errorf("an operator key without a label: err = %v, want the error from before", err)
	}
	broken := rec
	broken.Ciphertext = append(bytes.Clone(rec.Ciphertext), 0)
	if _, err := OpenHostKey(NewKeyFile(h.dir, ""), broken); !neither(err) {
		t.Errorf("a damaged operator key with the ID of the key: err = %v, want the error from before", err)
	}
	// A label never stops an attempt to open: a value with a wrong label opens when the key is right.
	wrong := rec
	wrong.KeyID = tried
	if v, err := OpenHostKey(NewKeyFile(h.dir, ""), wrong); err != nil || v != "value-for-zrok" {
		t.Errorf("a value with a wrong label did not open with the right key: %v", err)
	}
}

// The re-seal: every value opens with the new key alone, holds the same plain value under a new nonce and carries
// the ID of the new key. Nothing else of a row changes (rule 15).
func TestResealMovesEveryValueToTheNewKey(t *testing.T) {
	ctx := context.Background()
	h := newRotationHost(t)
	secsBefore, recsBefore := h.rows(t)
	next, err := NewSecretKey()
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	secrets, keys, err := Reseal(ctx, h.s.st, h.key, next, true, store.Audit{Time: at, Via: store.AuditViaCommand})
	if err != nil || secrets != 3 || keys != 2 {
		t.Fatalf("Reseal = %d, %d, %v; want 3 flat secrets and 2 operator keys", secrets, keys, err)
	}
	if got := h.opened(t, next); !reflect.DeepEqual(got, h.plain) {
		t.Errorf("the new key opens %d values that are the same as before, want all %d", len(got), len(h.plain))
	}
	if got := h.opened(t, h.key); len(got) != 0 {
		t.Errorf("the old key still opens %d values", len(got))
	}
	if c, err := CheckKey(ctx, h.s.st, next); err != nil || !c.All() || c.Total != 5 {
		t.Errorf("CheckKey of the new key = %+v, %v", c, err)
	}
	secsAfter, recsAfter := h.rows(t)
	if len(secsAfter) != len(secsBefore) || len(recsAfter) != len(recsBefore) {
		t.Fatalf("rows: %d and %d after, %d and %d before", len(secsAfter), len(recsAfter), len(secsBefore), len(recsBefore))
	}
	for i, after := range secsAfter {
		before := secsBefore[i]
		if after.KeyID != KeyID(next) || bytes.Equal(after.Nonce, before.Nonce) || bytes.Equal(after.Ciphertext, before.Ciphertext) {
			t.Errorf("secret %s of %s was not sealed again under the new key", after.Name, after.Flat)
		}
		if after.Flat != before.Flat || after.Name != before.Name || !after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Errorf("secret %s of %s changed more than its seal: %v -> %v", before.Name, before.Flat, before.UpdatedAt, after.UpdatedAt)
		}
	}
	for i, after := range recsAfter {
		before := recsBefore[i]
		if !before.HasValue() {
			if !reflect.DeepEqual(after, before) || !reflect.DeepEqual(after, h.record) {
				t.Errorf("the record without a value changed: %+v -> %+v", before, after)
			}
			continue
		}
		if after.KeyID != KeyID(next) || bytes.Equal(after.Nonce, before.Nonce) || bytes.Equal(after.Ciphertext, before.Ciphertext) {
			t.Errorf("operator key %s was not sealed again under the new key", after.ID)
		}
		if after.ID != before.ID || after.Source != before.Source || after.LastError != before.LastError || !after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Errorf("operator key %s changed more than its seal", before.ID)
		}
	}
	evs := h.rotateEvents(t)
	want := map[string]any{"from": KeyID(h.key), "to": KeyID(next), "secrets": float64(3), "keys": float64(2), "out": true}
	if len(evs) != 1 || !reflect.DeepEqual(evs[0], want) {
		t.Errorf("key.rotate events = %v, want one with %v", evs, want)
	}

	// With the new key at the key path, the host opens the values, and its check passes.
	putFile(t, filepath.Join(h.dir, SecretKeyFile), next)
	if v, _, err := StoredHostKey(ctx, NewKeyFile(h.dir, ""), h.s.st, HostKeyZrok); err != nil || v != "value-for-zrok" {
		t.Errorf("the operator key with the new key in place: %v", err)
	}
	if err := CheckSecretKey(ctx, NewKeyFile(h.dir, ""), h.s.st); err != nil {
		t.Errorf("CheckSecretKey with the new key in place: %v", err)
	}
}

// A value that the old key does not open fails the re-seal, and nothing changes: every value opens with the old key,
// none has a new key ID, and no key.rotate event exists (rule 15).
func TestResealWithAValueThatDoesNotOpenChangesNothing(t *testing.T) {
	ctx := context.Background()
	h := newRotationHost(t)
	// The last operator key by id, so that every other value is sealed again before the failure.
	foreign, err := SealHostKey(NewKeyFile(t.TempDir(), ""), HostKeyZrok, "sealed by another host", KeySourceCLI, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.st.PutHostKey(ctx, foreign, store.Audit{}); err != nil {
		t.Fatal(err)
	}
	secsBefore, recsBefore := h.rows(t)
	openedBefore := h.opened(t, h.key)
	if len(openedBefore) != 4 {
		t.Fatalf("the old key opens %d values before, want 4", len(openedBefore))
	}
	next := keyOf(8)
	// The check of a key says which key IDs the values carry that it does not open (rule 9).
	if c, err := CheckKey(ctx, h.s.st, h.key); err != nil || c.Opened != 4 || c.Total != 5 || !slices.Equal(c.UnopenedKeyIDs, []string{foreign.KeyID}) {
		t.Errorf("CheckKey of the old key = %+v, %v; want the key ID of the one value that does not open", c, err)
	}
	wantIDs := []string{KeyID(h.key), foreign.KeyID}
	slices.Sort(wantIDs)
	if c, err := CheckKey(ctx, h.s.st, next); err != nil || c.Opened != 0 || !slices.Equal(c.UnopenedKeyIDs, wantIDs) {
		t.Errorf("CheckKey of a key that opens nothing = %+v, %v; want the two key IDs", c, err)
	}
	secrets, keys, err := Reseal(ctx, h.s.st, h.key, next, false, store.Audit{Via: store.AuditViaCommand})
	if err == nil || secrets != 0 || keys != 0 || !strings.Contains(err.Error(), "operator key zrok") || !strings.Contains(err.Error(), "does not open with the old key") {
		t.Fatalf("Reseal = %d, %d, %v; want an error that names the value", secrets, keys, err)
	}
	secsAfter, recsAfter := h.rows(t)
	if !reflect.DeepEqual(secsAfter, secsBefore) || !reflect.DeepEqual(recsAfter, recsBefore) {
		t.Error("the failed re-seal changed a row")
	}
	if got := h.opened(t, h.key); !reflect.DeepEqual(got, openedBefore) {
		t.Errorf("the old key opens %d values after the failure, want the %d from before", len(got), len(openedBefore))
	}
	if got := h.opened(t, next); len(got) != 0 {
		t.Errorf("the new key opens %d values after the failure", len(got))
	}
	if ids, err := h.s.st.SealedKeyIDs(ctx); err != nil || ids.Carries(KeyID(next)) {
		t.Errorf("a value carries the ID of the new key after the failure: %v, %v", ids, err)
	}
	if evs := h.rotateEvents(t); len(evs) != 0 {
		t.Errorf("the failed re-seal wrote %d key.rotate events", len(evs))
	}

	// A row with a nonce of the wrong size is a value that does not open, and not a panic.
	if err := h.s.st.PutHostKey(ctx, store.HostKey{ID: HostKeyZrok, Nonce: []byte{1}, Ciphertext: []byte{2}, Source: KeySourceCLI, UpdatedAt: time.Now()}, store.Audit{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Reseal(ctx, h.s.st, h.key, next, false, store.Audit{}); err == nil || !strings.Contains(err.Error(), "operator key zrok") {
		t.Errorf("a row with a damaged nonce: err = %v, want an error that names the value", err)
	}
	// A key of the wrong size is refused before the transaction.
	if _, _, err := Reseal(ctx, h.s.st, h.key, next[:31], false, store.Audit{}); err == nil {
		t.Error("Reseal took a new key of 31 bytes")
	}
	if _, err := os.Stat(NewKeyFile(h.dir, "").PendingPath()); err == nil {
		t.Error("the re-seal wrote a pending key: the files are the command's")
	}
}
