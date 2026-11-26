package store

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// docs/spec/secret-key-rotation.md: the label on each sealed value (rules 1, 2 and 33) and the reads of the labels.

const (
	labelA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	labelB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// fill returns n bytes of b, which stand for a nonce or a ciphertext.
func fill(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }

// oldSchemaDB builds a flats.db as the binary of schema 26 left it: one row in secrets, one operator record with a
// value and one without. The values are bytes that stand for a nonce and a ciphertext. It returns the path.
func oldSchemaDB(t *testing.T) string {
	t.Helper()
	return oldSchemaDBWith(t, fill(1, 12), fill(2, 40), fill(3, 12), fill(4, 50))
}

// oldSchemaDBWith is oldSchemaDB with the nonce and the ciphertext of the flat secret and of the operator key.
func oldSchemaDBWith(t *testing.T, secretNonce, secretText, keyNonce, keyText []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "flats.db")
	t.Run("build schema 26", func(t *testing.T) {
		withMigrations(t, migrationsThrough(26))
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		makeFlat(t, s, "app")
		// The schema has no label column, so the rows are written as the code of that time wrote them.
		if _, err := s.db.Exec(`INSERT INTO secrets(flat,name,nonce,ciphertext,updated_at) VALUES('app','ONE',?,?,1000)`, secretNonce, secretText); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO host_keys(id,nonce,ciphertext,source,updated_at) VALUES('netbird',?,?,'cli',2000)`, keyNonce, keyText); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO host_keys(id,source,updated_at) VALUES('zrok','console',3000)`); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if info, err := Inspect(path); err != nil || info.Version != 26 {
			t.Fatalf("before: %+v, %v", info, err)
		}
	})
	return path
}

// A flats.db of schema 26 migrates through 27 to the latest schema. Each row keeps its bytes, its time and its
// source and has an empty label (rule 33).
func TestMigrationFromVersion26AddsTheLabels(t *testing.T) {
	ctx := context.Background()
	path := oldSchemaDB(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if info, err := Inspect(path); err != nil || info.Version != 30 || info.Version != latestVersion() {
		t.Fatalf("after: %+v, %v", info, err)
	}
	secs, err := s.ListSecrets(ctx, "app")
	if err != nil || len(secs) != 1 {
		t.Fatalf("ListSecrets = %+v, %v", secs, err)
	}
	if got := secs[0]; got.Name != "ONE" || !bytes.Equal(got.Nonce, fill(1, 12)) || !bytes.Equal(got.Ciphertext, fill(2, 40)) || got.KeyID != "" || got.UpdatedAt.UnixMilli() != 1000 {
		t.Errorf("the row of secrets after the migration: %+v", got)
	}
	k, err := s.GetHostKey(ctx, "netbird")
	if err != nil || !bytes.Equal(k.Nonce, fill(3, 12)) || !bytes.Equal(k.Ciphertext, fill(4, 50)) || k.KeyID != "" || k.Source != "cli" || k.UpdatedAt.UnixMilli() != 2000 {
		t.Errorf("the record with a value after the migration: %+v, %v", k, err)
	}
	if z, err := s.GetHostKey(ctx, "zrok"); err != nil || z.HasValue() || z.KeyID != "" || z.Source != "console" {
		t.Errorf("the record without a value after the migration: %+v, %v", z, err)
	}
	if ids, err := s.SealedKeyIDs(ctx); err != nil || !maps.Equal(ids, KeyIDCounts{"": 2}) {
		t.Errorf("SealedKeyIDs = %v, %v; want two values without a label", ids, err)
	}
	if got := allAudit(t, s); len(got) != 0 {
		t.Errorf("the migration wrote events: %v", auditKinds(got))
	}
	if bs := backupFiles(t, path); len(bs) != 1 {
		t.Errorf("backups = %v, want the one of schema 26", bs)
	}
}

// Values that a key sealed before the migration keep their bytes, have an empty label and open with that key
// afterwards (rule 33). The test makes its key, and seals as the host does: AES-256-GCM, with the name of a flat
// secret or "host-key", a zero byte and the id of an operator key as the associated data.
func TestMigrationFromVersion26KeepsValuesThatOpen(t *testing.T) {
	ctx := context.Background()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	seal := func(value, aad string) (nonce, text []byte) {
		nonce = make([]byte, gcm.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			t.Fatal(err)
		}
		return nonce, gcm.Seal(nil, nonce, []byte(value), []byte(aad))
	}
	const secretValue, keyValue, keyAAD = "the value of ONE", "the value of the netbird key", "host-key\x00netbird"
	secretNonce, secretText := seal(secretValue, "ONE")
	keyNonce, keyText := seal(keyValue, keyAAD)
	path := oldSchemaDBWith(t, secretNonce, secretText, keyNonce, keyText)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if info, err := Inspect(path); err != nil || info.Version != latestVersion() {
		t.Fatalf("after: %+v, %v", info, err)
	}
	secs, err := s.ListSecrets(ctx, "app")
	if err != nil || len(secs) != 1 {
		t.Fatalf("ListSecrets = %+v, %v", secs, err)
	}
	if got := secs[0]; !bytes.Equal(got.Nonce, secretNonce) || !bytes.Equal(got.Ciphertext, secretText) || got.KeyID != "" {
		t.Errorf("the flat secret changed in the migration, or has a label: %+v", got)
	}
	if got, err := gcm.Open(nil, secs[0].Nonce, secs[0].Ciphertext, []byte("ONE")); err != nil || string(got) != secretValue {
		t.Errorf("the flat secret after the migration opens as %q, %v", got, err)
	}
	k, err := s.GetHostKey(ctx, "netbird")
	if err != nil || !bytes.Equal(k.Nonce, keyNonce) || !bytes.Equal(k.Ciphertext, keyText) || k.KeyID != "" {
		t.Fatalf("the operator key changed in the migration, or has a label: %+v, %v", k, err)
	}
	if got, err := gcm.Open(nil, k.Nonce, k.Ciphertext, []byte(keyAAD)); err != nil || string(got) != keyValue {
		t.Errorf("the operator key after the migration opens as %q, %v", got, err)
	}
}

// The opens that demand the exact schema refuse a database of schema 26 and leave it as it is, until an Open
// migrated it (rule 33). The read of the labels beside a host makes no check there (rule 25).
func TestExactSchemaOpensRefuseSchema26(t *testing.T) {
	ctx := context.Background()
	path := oldSchemaDB(t)
	before := snapshot(t, path)
	ids, ok, err := SealedKeyIDsBeside(ctx, path)
	if err != nil || ok || ids != nil {
		t.Errorf("SealedKeyIDsBeside of schema 26 = %v, %t, %v; want no labels, not ok and no error", ids, ok, err)
	}
	if after := snapshot(t, path); after != before {
		t.Errorf("SealedKeyIDsBeside changed the database or its folder:\n before %s\n after  %s", before, after)
	}
	if st, err := OpenImmutable(path); !errors.Is(err, ErrVersionMismatch) {
		if err == nil {
			st.Close()
		}
		t.Errorf("OpenImmutable of schema 26 = %v, want ErrVersionMismatch", err)
	}
	if rd, err := OpenAuditReader(path); !errors.Is(err, ErrAuditSchema) {
		if err == nil {
			rd.Close()
		}
		t.Errorf("OpenAuditReader of schema 26 = %v, want ErrAuditSchema", err)
	}
	if err := AppendAuditBeside(ctx, path, AuditEvent{Kind: AuditKeyBackup}); !errors.Is(err, ErrAuditSchema) {
		t.Errorf("AppendAuditBeside of schema 26 = %v, want ErrAuditSchema", err)
	}
	if info, err := Inspect(path); err != nil || info.Version != 26 {
		t.Fatalf("after the refusals: %+v, %v; want schema 26 still", info, err)
	}

	// After an Open migrated it, each of them works.
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if ids, ok, err := SealedKeyIDsBeside(ctx, path); err != nil || !ok || !maps.Equal(ids, KeyIDCounts{"": 2}) {
		t.Errorf("SealedKeyIDsBeside after the migration = %v, %t, %v", ids, ok, err)
	}
	if err := AppendAuditBeside(ctx, path, AuditEvent{Kind: AuditKeyBackup}); err != nil {
		t.Errorf("AppendAuditBeside after the migration: %v", err)
	}
	rd, err := OpenAuditReader(path)
	if err != nil {
		t.Fatalf("OpenAuditReader after the migration: %v", err)
	}
	rd.Close()
	st, err := OpenImmutable(path)
	if err != nil {
		t.Fatalf("OpenImmutable after the migration: %v", err)
	}
	st.Close()
}

// Each writer stores the label in the row write, and each reader returns it (rule 2).
func TestWritersAndReadersCarryTheLabel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	makeFlat(t, s, "app")
	at := time.UnixMilli(1_700_000_000_000).UTC()
	put := func(label string) {
		t.Helper()
		sec := SealedSecret{Name: "ONE", Nonce: fill(1, 12), Ciphertext: fill(2, 40), KeyID: label, UpdatedAt: at}
		if err := s.PutSecret(ctx, "app", sec, Audit{}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want string) {
		t.Helper()
		one, err := s.ListSecrets(ctx, "app")
		if err != nil || len(one) != 1 || one[0].KeyID != want {
			t.Errorf("ListSecrets = %+v, %v; want the label %q", one, err, want)
		}
		all, err := s.ListAllSecrets(ctx)
		if err != nil || len(all) != 1 || all[0].KeyID != want || all[0].Flat != "app" {
			t.Errorf("ListAllSecrets = %+v, %v; want the label %q", all, err, want)
		}
		_, snap, _, err := s.RuntimeSettingsSnapshot(ctx, "app")
		if err != nil || len(snap) != 1 || snap[0].KeyID != want || !bytes.Equal(snap[0].Ciphertext, fill(2, 40)) {
			t.Errorf("RuntimeSettingsSnapshot = %+v, %v; want the label %q", snap, err, want)
		}
	}
	put(labelA)
	check(labelA)
	put(labelB) // a new set of the value replaces the label
	check(labelB)
	put("") // a writer that knows no label stores none
	check("")

	rec := HostKey{ID: "netbird", Nonce: fill(3, 12), Ciphertext: fill(4, 50), KeyID: labelA, Source: "cli", UpdatedAt: at}
	if err := s.PutHostKey(ctx, rec, Audit{}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetHostKey(ctx, "netbird"); err != nil || got.KeyID != labelA {
		t.Errorf("GetHostKey = %+v, %v; want the label", got, err)
	}
	if list, err := s.ListHostKeys(ctx); err != nil || len(list) != 1 || list[0].KeyID != labelA {
		t.Errorf("ListHostKeys = %+v, %v; want the label", list, err)
	}
}

// A record without a value has an empty label, also when it replaces a record that held a value and the caller
// passes the record that it read (rule 1).
func TestARecordWithoutAValueHasNoLabel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	at := time.UnixMilli(1_700_000_000_000).UTC()
	rec := HostKey{ID: "zrok", Nonce: fill(3, 12), Ciphertext: fill(4, 50), KeyID: labelA, Source: "cli", UpdatedAt: at}
	if err := s.PutHostKey(ctx, rec, Audit{}); err != nil {
		t.Fatal(err)
	}
	// The host reads the record, removes its value and writes it back.
	got, err := s.GetHostKey(ctx, "zrok")
	if err != nil || got.KeyID != labelA {
		t.Fatalf("GetHostKey = %+v, %v", got, err)
	}
	got.Nonce, got.Ciphertext = nil, nil
	if err := s.PutHostKey(ctx, got, Audit{}); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetHostKey(ctx, "zrok")
	if err != nil || after.HasValue() || after.KeyID != "" {
		t.Errorf("the record after its value was removed: %+v, %v; want no value and no label", after, err)
	}
	var column string
	if err := s.db.QueryRow(`SELECT key_id FROM host_keys WHERE id='zrok'`).Scan(&column); err != nil || column != "" {
		t.Errorf("the column of the record = %q, %v; want empty", column, err)
	}
	// A new record without a value gets none either.
	if err := s.PutHostKey(ctx, HostKey{ID: "netbird", KeyID: labelB, Source: "console", UpdatedAt: at}, Audit{}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.GetHostKey(ctx, "netbird"); err != nil || n.KeyID != "" {
		t.Errorf("a new record without a value: %+v, %v; want no label", n, err)
	}
	if ids, err := s.SealedKeyIDs(ctx); err != nil || ids.Total() != 0 {
		t.Errorf("SealedKeyIDs = %v, %v; want nothing, because no record holds a value", ids, err)
	}
}

// The methods of KeyIDCounts, also on a nil map.
func TestKeyIDCounts(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		c        KeyIDCounts
		total    int
		labelled bool
		single   string
		ids      []string
	}{
		"nil":             {nil, 0, false, "", nil},
		"empty":           {KeyIDCounts{}, 0, false, "", nil},
		"one label":       {KeyIDCounts{labelA: 3}, 3, true, labelA, []string{labelA}},
		"two labels":      {KeyIDCounts{labelB: 1, labelA: 2}, 3, true, "", []string{labelA, labelB}},
		"no label":        {KeyIDCounts{"": 2}, 2, false, "", nil},
		"mixed":           {KeyIDCounts{"": 1, labelA: 1}, 2, false, "", []string{labelA}},
		"a count of zero": {KeyIDCounts{labelA: 0, labelB: 1}, 1, true, labelB, []string{labelB}},
	} {
		if got := tc.c.Total(); got != tc.total {
			t.Errorf("%s: Total = %d, want %d", name, got, tc.total)
		}
		if got := tc.c.AllLabelled(); got != tc.labelled {
			t.Errorf("%s: AllLabelled = %t, want %t", name, got, tc.labelled)
		}
		id, ok := tc.c.Single()
		if id != tc.single || ok != (tc.single != "") {
			t.Errorf("%s: Single = %q, %t; want %q", name, id, ok, tc.single)
		}
		if got := tc.c.IDs(); !slices.Equal(got, tc.ids) {
			t.Errorf("%s: IDs = %v, want %v", name, got, tc.ids)
		}
		if tc.c.Carries("") {
			t.Errorf("%s: Carries of the empty label is true", name)
		}
		for _, id := range []string{labelA, labelB} {
			if got, want := tc.c.Carries(id), slices.Contains(tc.ids, id); got != want {
				t.Errorf("%s: Carries(%s) = %t, want %t", name, id, got, want)
			}
		}
	}
}

// SealedKeyIDs counts the rows of secrets and the records with a value by their label, and the read beside a host
// gives the same counts while a host holds the database.
func TestSealedKeyIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	makeFlat(t, s, "alpha")
	makeFlat(t, s, "bravo")
	at := time.UnixMilli(1_700_000_000_000).UTC()
	want := func(step string, counts KeyIDCounts) {
		t.Helper()
		got, err := s.SealedKeyIDs(ctx)
		if err != nil || !maps.Equal(got, counts) {
			t.Errorf("%s: SealedKeyIDs = %v, %v; want %v", step, got, err, counts)
		}
		beside, ok, err := SealedKeyIDsBeside(ctx, s.path)
		if err != nil || !ok || !maps.Equal(beside, counts) {
			t.Errorf("%s: SealedKeyIDsBeside = %v, %t, %v; want %v", step, beside, ok, err, counts)
		}
	}
	secret := func(flat, name, label string) {
		t.Helper()
		if err := s.PutSecret(ctx, flat, SealedSecret{Name: name, Nonce: fill(1, 12), Ciphertext: fill(2, 40), KeyID: label, UpdatedAt: at}, Audit{}); err != nil {
			t.Fatal(err)
		}
	}
	record := func(id, label string, value bool) {
		t.Helper()
		rec := HostKey{ID: id, KeyID: label, Source: "cli", UpdatedAt: at}
		if value {
			rec.Nonce, rec.Ciphertext = fill(3, 12), fill(4, 50)
		}
		if err := s.PutHostKey(ctx, rec, Audit{}); err != nil {
			t.Fatal(err)
		}
	}
	want("no sealed value", KeyIDCounts{})
	record("zrok", labelA, false)
	want("a record without a value", KeyIDCounts{})
	secret("alpha", "ONE", labelA)
	secret("bravo", "TWO", labelA)
	record("netbird", labelA, true)
	want("every value carries one label", KeyIDCounts{labelA: 3})
	secret("bravo", "TWO", labelB)
	want("two labels", KeyIDCounts{labelA: 2, labelB: 1})
	secret("alpha", "ONE", "")
	want("mixed", KeyIDCounts{"": 1, labelA: 1, labelB: 1})
	secret("bravo", "TWO", "")
	record("netbird", "", true)
	want("no label", KeyIDCounts{"": 3})
}

// The read beside a host creates no database, and an error tells a file that is no database from one of another schema.
func TestSealedKeyIDsBesideOfNoDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	missing := filepath.Join(dir, "flats.db")
	if ids, ok, err := SealedKeyIDsBeside(ctx, missing); err != nil || ok || ids != nil {
		t.Errorf("a missing database = %v, %t, %v; want not ok and no error", ids, ok, err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("the read created %v", names)
	}
	if err := os.WriteFile(missing, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if ids, ok, err := SealedKeyIDsBeside(ctx, missing); err != nil || ok || ids != nil {
		t.Errorf("an empty file = %v, %t, %v; want not ok and no error", ids, ok, err)
	}
	other := filepath.Join(dir, "other.db")
	if err := os.WriteFile(other, bytes.Repeat([]byte("not a database "), 20), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := SealedKeyIDsBeside(ctx, other); !errors.Is(err, ErrNotFlats) || ok {
		t.Errorf("a file that is no database = %t, %v; want ErrNotFlats", ok, err)
	}
}
