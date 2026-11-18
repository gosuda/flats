package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// docs/spec/injected-fetch-secrets.md: the bindings in the secrets table.

var bindA = Binding{Origin: "https://api.example.com", Header: "Authorization"}

func putChecked(t *testing.T, s *Store, flat, name string, text string, put SecretPut) (Binding, error) {
	t.Helper()
	return s.PutSecretChecked(context.Background(), flat, SealedSecret{Name: name, Nonce: fill(1, 12), Ciphertext: []byte(text), KeyID: labelA, UpdatedAt: time.UnixMilli(1000)},
		put, Audit{Via: AuditViaCLI})
}

func secretRow(t *testing.T, s *Store, flat, name string) (SealedSecret, bool) {
	t.Helper()
	secs, err := s.ListSecrets(context.Background(), flat)
	if err != nil {
		t.Fatal(err)
	}
	for _, sec := range secs {
		if sec.Name == name {
			return sec, true
		}
	}
	return SealedSecret{}, false
}

func TestPutSecretCheckedKeepsTheBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	makeFlat(t, s, "app")
	b := bindA
	got, err := putChecked(t, s, "app", "TOKEN", "first", SecretPut{Inject: &b, HeaderValue: true})
	if err != nil || got != bindA {
		t.Fatalf("new injected secret: %+v, %v", got, err)
	}
	// A new value without a binding keeps the binding of the row.
	if got, err := putChecked(t, s, "app", "TOKEN", "second", SecretPut{HeaderValue: true}); err != nil || got != bindA {
		t.Fatalf("new value: %+v, %v", got, err)
	}
	// The same binding with another case of the header is the same binding.
	same := Binding{Origin: bindA.Origin, Header: "authorization"}
	if got, err := putChecked(t, s, "app", "TOKEN", "third", SecretPut{Inject: &same, HeaderValue: true}); err != nil || got != bindA {
		t.Fatalf("the same binding: %+v, %v", got, err)
	}
	row, ok := secretRow(t, s, "app", "TOKEN")
	if !ok || row.Inject != bindA || string(row.Ciphertext) != "third" {
		t.Fatalf("ListSecrets: %+v", row)
	}
	all, err := s.ListAllSecrets(ctx)
	if err != nil || len(all) != 1 || all[0].Inject != bindA {
		t.Fatalf("ListAllSecrets: %+v, %v", all, err)
	}
	_, secs, _, err := s.RuntimeSettingsSnapshot(ctx, "app")
	if err != nil || len(secs) != 1 || secs[0].Inject != bindA {
		t.Fatalf("RuntimeSettingsSnapshot: %+v, %v", secs, err)
	}
	evs := allAudit(t, s)
	if len(evs) != 3 {
		t.Fatalf("audit: %v", auditKinds(evs))
	}
	for i, e := range evs {
		var data map[string]any
		if err := json.Unmarshal(e.Data, &data); err != nil {
			t.Fatal(err)
		}
		if e.Kind != AuditSecretSet || data["created"] != (i == 0) || data["origin"] != bindA.Origin || data["header"] != bindA.Header || len(data) != 3 {
			t.Errorf("event %d: %s %s", i, e.Kind, e.Data)
		}
	}
	// A readable secret keeps the data of today.
	if _, err := putChecked(t, s, "app", "PLAIN", "x", SecretPut{}); err != nil {
		t.Fatal(err)
	}
	evs = allAudit(t, s)
	if last := evs[len(evs)-1]; string(last.Data) != `{"created":true}` {
		t.Errorf("readable secret.set data: %s", last.Data)
	}
	if row, _ := secretRow(t, s, "app", "PLAIN"); row.Inject.Injected() {
		t.Errorf("readable row: %+v", row)
	}
}

func TestPutSecretCheckedRefusals(t *testing.T) {
	t.Parallel()
	s := open(t)
	makeFlat(t, s, "app")
	b := bindA
	other := Binding{Origin: "https://other.example.com", Header: "Authorization"}
	if _, err := putChecked(t, s, "app", "PLAIN", "plain", SecretPut{HeaderValue: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := putChecked(t, s, "app", "TOKEN", "token", SecretPut{Inject: &b, HeaderValue: true}); err != nil {
		t.Fatal(err)
	}
	before := len(allAudit(t, s))
	for name, tc := range map[string]struct {
		secret string
		put    SecretPut
		want   InjectReason
	}{
		"a binding for a readable secret":   {"PLAIN", SecretPut{Inject: &b, HeaderValue: true}, InjectReadable},
		"another binding":                   {"TOKEN", SecretPut{Inject: &other, HeaderValue: true}, InjectOtherBinding},
		"a binding that another secret has": {"NEW", SecretPut{Inject: &b, HeaderValue: true}, InjectTaken},
		"a value that cannot be a header":   {"TOKEN", SecretPut{}, InjectValue},
		"a new secret with a bad value":     {"NEW", SecretPut{Inject: &other}, InjectValue},
	} {
		_, err := putChecked(t, s, "app", tc.secret, "refused", tc.put)
		var r *InjectRefusal
		if !errors.As(err, &r) || r.Reason != tc.want {
			t.Errorf("%s: %v, want reason %d", name, err, tc.want)
			continue
		}
		if tc.want == InjectTaken && r.Secret != "TOKEN" {
			t.Errorf("%s: the other secret is %q", name, r.Secret)
		}
		if tc.want == InjectOtherBinding && r.Binding != bindA {
			t.Errorf("%s: the stored binding is %+v", name, r.Binding)
		}
	}
	// PutSecret does not check a plain value, so it never replaces an injected value.
	if err := s.PutSecret(context.Background(), "app", SealedSecret{Name: "TOKEN", Nonce: fill(1, 12), Ciphertext: []byte("refused")}, Audit{}); err == nil {
		t.Error("PutSecret replaced an injected value")
	}
	for _, name := range []string{"PLAIN", "TOKEN"} {
		if row, _ := secretRow(t, s, "app", name); string(row.Ciphertext) == "refused" {
			t.Errorf("a refused set changed %s", name)
		}
	}
	if _, ok := secretRow(t, s, "app", "NEW"); ok {
		t.Error("a refused set made a row")
	}
	if n := len(allAudit(t, s)); n != before {
		t.Errorf("refused sets wrote %d events", n-before)
	}
}

func TestPutSecretCheckedLimit(t *testing.T) {
	t.Parallel()
	s := open(t)
	makeFlat(t, s, "app")
	for i := range MaxInjectedSecrets {
		b := Binding{Origin: "https://api.example.com", Header: fmt.Sprintf("X-Key-%d", i)}
		if _, err := putChecked(t, s, "app", fmt.Sprintf("K%d", i), "v", SecretPut{Inject: &b, HeaderValue: true}); err != nil {
			t.Fatal(err)
		}
	}
	b := Binding{Origin: "https://api.example.com", Header: "X-One-More"}
	var r *InjectRefusal
	if _, err := putChecked(t, s, "app", "MORE", "v", SecretPut{Inject: &b, HeaderValue: true}); !errors.As(err, &r) || r.Reason != InjectLimit {
		t.Fatalf("secret %d: %v", MaxInjectedSecrets+1, err)
	}
	// A new value of an injected secret and a readable secret are not counted against the limit.
	if _, err := putChecked(t, s, "app", "K0", "w", SecretPut{HeaderValue: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := putChecked(t, s, "app", "READABLE", "w", SecretPut{}); err != nil {
		t.Fatal(err)
	}
}

// Two sets that run at the same time give the result of one of their two orders (rule 9).
func TestConcurrentSetsOfOneNewName(t *testing.T) {
	t.Parallel()
	s := open(t)
	other := Binding{Origin: "https://other.example.com", Header: "X-Api-Key"}
	for i := range 40 {
		flat := fmt.Sprintf("race-%d", i)
		makeFlat(t, s, flat)
		name := "RACE"
		b := bindA
		b.Header = fmt.Sprintf("X-Race-%d", i)
		var wg sync.WaitGroup
		var plainErr, boundErr error
		start := make(chan struct{})
		wg.Add(2)
		// A: no binding, a value that only a readable secret may have.
		go func() { defer wg.Done(); <-start; _, plainErr = putChecked(t, s, flat, name, "a\nb", SecretPut{}) }()
		// B: a binding, with a value that fits a header.
		go func() {
			defer wg.Done()
			<-start
			_, boundErr = putChecked(t, s, flat, name, "bound", SecretPut{Inject: &b, HeaderValue: true})
		}()
		close(start)
		wg.Wait()
		row, ok := secretRow(t, s, flat, name)
		switch {
		case !ok:
			t.Fatalf("%s: no row (%v, %v)", name, plainErr, boundErr)
		case row.Inject.Injected() && string(row.Ciphertext) != "bound":
			t.Fatalf("%s: an injected row holds the value of the set without a binding", name)
		case row.Inject.Injected() && plainErr == nil, !row.Inject.Injected() && boundErr == nil:
			t.Fatalf("%s: both sets passed: %+v", name, row)
		}
		// Two bindings for one new name: one of them, with its own value.
		name2 := "PAIR"
		b2 := other
		b2.Header = fmt.Sprintf("X-Pair-%d", i)
		b1 := bindA
		b1.Header = fmt.Sprintf("X-Pair-%d", i)
		var err1, err2 error
		start2 := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start2
			_, err1 = putChecked(t, s, flat, name2, "one", SecretPut{Inject: &b1, HeaderValue: true})
		}()
		go func() {
			defer wg.Done()
			<-start2
			_, err2 = putChecked(t, s, flat, name2, "two", SecretPut{Inject: &b2, HeaderValue: true})
		}()
		close(start2)
		wg.Wait()
		row, _ = secretRow(t, s, flat, name2)
		if (err1 == nil) == (err2 == nil) || row.Inject == b1 && string(row.Ciphertext) != "one" || row.Inject == b2 && string(row.Ciphertext) != "two" {
			t.Fatalf("%s: %+v (%v, %v)", name2, row, err1, err2)
		}
	}
}

func TestUniqueIndexRefusesASecondRowWithOneBinding(t *testing.T) {
	t.Parallel()
	s := open(t)
	makeFlat(t, s, "app")
	b := bindA
	if _, err := putChecked(t, s, "app", "TOKEN", "v", SecretPut{Inject: &b, HeaderValue: true}); err != nil {
		t.Fatal(err)
	}
	_, err := s.db.Exec(`INSERT INTO secrets(flat,name,nonce,ciphertext,updated_at,fetch_origin,fetch_header) VALUES('app','COPY',X'00',X'00',1,?,?)`,
		bindA.Origin, "AUTHORIZATION")
	if err == nil {
		t.Fatal("a second row with the same binding was stored")
	}
	// Readable rows have no binding and are not held by the index.
	for _, name := range []string{"R1", "R2"} {
		if _, err := s.db.Exec(`INSERT INTO secrets(flat,name,nonce,ciphertext,updated_at) VALUES('app',?,X'00',X'00',1)`, name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResealKeepsTheBinding(t *testing.T) {
	t.Parallel()
	s := open(t)
	makeFlat(t, s, "app")
	b := bindA
	if _, err := putChecked(t, s, "app", "TOKEN", "v", SecretPut{Inject: &b, HeaderValue: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Reseal(context.Background(), Reseal{From: labelA, To: labelB, Audit: Audit{Via: AuditViaCommand},
		Seal: func(v SealedValue) ([]byte, []byte, error) { return fill(9, 12), []byte("resealed"), nil }}); err != nil {
		t.Fatal(err)
	}
	row, _ := secretRow(t, s, "app", "TOKEN")
	if row.Inject != bindA || string(row.Ciphertext) != "resealed" || row.KeyID != labelB {
		t.Fatalf("after the re-seal: %+v", row)
	}
}

// A flats.db of schema 27 migrates to 28: every secret stays readable and keeps its bytes (rule 32).
func TestMigrationFromVersion27KeepsSecretsReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flats.db")
	t.Run("build schema 27", func(t *testing.T) {
		withMigrations(t, migrationsThrough(27))
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		makeFlat(t, s, "app")
		if _, err := s.db.Exec(`INSERT INTO secrets(flat,name,nonce,ciphertext,updated_at,key_id) VALUES('app','ONE',?,?,1000,?)`, fill(1, 12), fill(2, 40), labelA); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if info, err := Inspect(path); err != nil || info.Version != 27 {
			t.Fatalf("before: %+v, %v", info, err)
		}
	})
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if info, err := Inspect(path); err != nil || info.Version != 28 {
		t.Fatalf("after: %+v, %v", info, err)
	}
	row, ok := secretRow(t, s, "app", "ONE")
	if !ok || row.Inject.Injected() || !bytes.Equal(row.Nonce, fill(1, 12)) || !bytes.Equal(row.Ciphertext, fill(2, 40)) || row.KeyID != labelA {
		t.Fatalf("the row after the migration: %+v", row)
	}
	if got := allAudit(t, s); len(got) != 0 {
		t.Errorf("the migration wrote events: %v", auditKinds(got))
	}
}
