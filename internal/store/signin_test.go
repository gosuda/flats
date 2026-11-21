package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// docs/spec/portal-sign-in.md: the setting in flats, the client secret in secrets, and their audit events.

var oidcSetting = SignIn{Provider: SignInOIDC, Issuer: "https://idp.example.com", ClientID: "flats-app", Allow: []string{"@example.org", "ann@example.com"}}

// applyingApproval stores an approval of flat that a decision claimed, so that a commit can write its receipt.
func applyingApproval(t *testing.T, s *Store, flat, id string) {
	t.Helper()
	ctx := context.Background()
	if err := s.InsertApproval(ctx, Approval{ID: id, Flat: flat, Action: "set_signin", Params: []byte(`{}`), Status: "pending", Via: "cli", RequestedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimApproval(ctx, id); err != nil {
		t.Fatal(err)
	}
}

// commitSignIn applies to as an approved change of flat at its stored sequence and grants.
func commitSignIn(t *testing.T, s *Store, flat, id string, to SignIn) (SignIn, error) {
	t.Helper()
	st, err := s.GetSignIn(context.Background(), flat)
	if err != nil {
		t.Fatal(err)
	}
	applyingApproval(t, s, flat, id)
	return s.CommitSignIn(context.Background(), SignInCommit{Flat: flat, FlatID: st.FlatID, Seq: st.Seq, Grants: st.PublicGrants, To: to,
		Approval: id, Receipt: []byte(`{"status":"applied"}`), Audit: Audit{Via: AuditViaConsole, Actor: "operator"}})
}

func auditOf(t *testing.T, s *Store, kind string) []AuditEvent {
	t.Helper()
	var out []AuditEvent
	for _, e := range allAudit(t, s) {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func conflictReason(err error) SignInReason {
	var c *SignInConflict
	if errors.As(err, &c) {
		return c.Reason
	}
	return 0
}

// A flats.db of schema 28 migrates to 29: every flat gets sign-in off and every secret an empty use, also a row with
// the name of the client secret, and an older binary refuses the result (rule 72).
func TestMigrationFromVersion28AddsSignIn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flats.db")
	var keep Flat
	t.Run("build schema 28", func(t *testing.T) {
		withMigrations(t, migrationsThrough(28))
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		keep = makeFlat(t, s, "app")
		for _, q := range []string{
			`INSERT INTO secrets(flat,name,nonce,ciphertext,updated_at,key_id) VALUES('app','ONE',X'01',X'02',1000,'')`,
			`INSERT INTO secrets(flat,name,nonce,ciphertext,updated_at,key_id) VALUES('app','oidc-client-secret',X'03',X'04',2000,'')`,
			// The baseline of this binary has the columns of flats; the binary of schema 28 had none.
			`ALTER TABLE flats DROP COLUMN signin_provider`, `ALTER TABLE flats DROP COLUMN signin_issuer`, `ALTER TABLE flats DROP COLUMN signin_client_id`,
			`ALTER TABLE flats DROP COLUMN signin_allow`, `ALTER TABLE flats DROP COLUMN signin_seq`,
		} {
			if _, err := s.db.Exec(q); err != nil {
				t.Fatal(q, err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if info, err := Inspect(path); err != nil || info.Version != 28 {
			t.Fatalf("before: %+v, %v", info, err)
		}
	})
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := Inspect(path); err != nil || info.Version != 29 || info.Version != latestVersion() {
		t.Fatalf("after: %+v, %v", info, err)
	}
	f, err := s.GetFlat(ctx, "app")
	if err != nil || f.SignInProvider != "" || f.SignInSeq != 0 || f.ID != keep.ID {
		t.Fatalf("the flat after the migration: %+v, %v", f, err)
	}
	st, err := s.GetSignIn(ctx, "app")
	if err != nil || st.SignIn.On() || st.SignIn.Allow == nil || len(st.SignIn.Allow) != 0 || st.Seq != 0 || st.Secret != nil {
		t.Fatalf("GetSignIn = %+v, %v", st, err)
	}
	secs, err := s.ListSecrets(ctx, "app")
	if err != nil || len(secs) != 2 || secs[1].Name != SignInSecretName || !bytes.Equal(secs[1].Ciphertext, []byte{4}) {
		t.Fatalf("the row named %s stays a readable secret: %+v, %v", SignInSecretName, secs, err)
	}
	all, err := s.ListAllSecrets(ctx)
	if err != nil || len(all) != 2 || all[0].Use != "" || all[1].Use != "" {
		t.Fatalf("ListAllSecrets = %+v, %v", all, err)
	}
	if _, err := s.db.Exec(`UPDATE secrets SET use='other' WHERE name='ONE'`); err == nil {
		t.Error("the check of the use took another value")
	}
	if !slices.Contains(indexesOf(t, s, "secrets"), "secrets_signin") {
		t.Errorf("secrets has no index secrets_signin: %v", indexesOf(t, s, "secrets"))
	}
	if got := allAudit(t, s); len(got) != 0 {
		t.Errorf("the migration wrote events: %v", auditKinds(got))
	}
	s.Close()
	t.Run("a binary of schema 28 refuses it", func(t *testing.T) {
		withMigrations(t, migrationsThrough(28))
		if _, err := Open(path); !errors.Is(err, ErrSchemaTooNew) {
			t.Errorf("Open = %v, want ErrSchemaTooNew", err)
		}
		if _, err := OpenAuditReader(path); !errors.Is(err, ErrSchemaTooNew) {
			t.Errorf("OpenAuditReader = %v, want ErrSchemaTooNew", err)
		}
	})
}

// A commit writes the setting, the next sequence, the receipt and one signin.set row; a change and its reverse count
// twice (rules 19 and 65).
func TestCommitSignInWritesTheChangeAndItsRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	flat := makeFlat(t, s, "app")
	from, err := commitSignIn(t, s, "app", "apr-1", oidcSetting)
	if err != nil || from.On() {
		t.Fatalf("CommitSignIn = %+v, %v", from, err)
	}
	st, err := s.GetSignIn(ctx, "app")
	if err != nil || !st.SignIn.Same(oidcSetting) || st.Seq != 1 {
		t.Fatalf("GetSignIn = %+v, %v", st, err)
	}
	if f, _ := s.GetFlat(ctx, "app"); f.SignInProvider != SignInOIDC || f.SignInSeq != 1 {
		t.Fatalf("GetFlat = %+v", f)
	}
	if a, _ := s.GetApproval(ctx, "apr-1"); string(a.ResultData) != `{"status":"applied"}` || a.Status != "applying" {
		t.Fatalf("the receipt: %+v", a)
	}
	rows := auditOf(t, s, AuditSignInSet)
	if len(rows) != 1 || rows[0].Flat != "app" || rows[0].FlatID != flat.ID || rows[0].Name != "" || rows[0].Via != AuditViaConsole || rows[0].Actor != "operator" {
		t.Fatalf("signin.set rows = %+v", rows)
	}
	want := `{"from":{"provider":"","issuer":"","client_id":"","count":0},"to":{"provider":"oidc","issuer":"https://idp.example.com","client_id":"flats-app","count":2},"added":["@example.org","ann@example.com"],"removed":[],"approval":"apr-1"}`
	if string(rows[0].Data) != want {
		t.Errorf("data = %s\nwant %s", rows[0].Data, want)
	}
	// A change of the list, then back: each counts.
	narrow := oidcSetting
	narrow.Allow = []string{"ann@example.com", "bob@example.com"}
	if _, err := commitSignIn(t, s, "app", "apr-2", narrow); err != nil {
		t.Fatal(err)
	}
	rows = auditOf(t, s, AuditSignInSet)
	var data struct{ Added, Removed []string }
	if err := json.Unmarshal(rows[1].Data, &data); err != nil || !slices.Equal(data.Added, []string{"bob@example.com"}) || !slices.Equal(data.Removed, []string{"@example.org"}) {
		t.Errorf("added and removed: %s", rows[1].Data)
	}
	if _, err := commitSignIn(t, s, "app", "apr-3", oidcSetting); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.GetSignIn(ctx, "app"); !st.SignIn.Same(oidcSetting) || st.Seq != 3 {
		t.Fatalf("after the reverse: %+v", st)
	}
	if _, err := commitSignIn(t, s, "app", "apr-4", SignIn{}); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.GetSignIn(ctx, "app"); st.SignIn.On() || st.SignIn.Allow == nil || st.Seq != 4 {
		t.Fatalf("off: %+v", st)
	}
}

// A commit applies only while the flat ID, the sequence and the public grants are the frozen ones (rule 18), and a
// refusal changes nothing.
func TestCommitSignInStale(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	makeFlat(t, s, "app")
	st, _ := s.GetSignIn(ctx, "app")
	base := SignInCommit{Flat: "app", FlatID: st.FlatID, Seq: st.Seq, Grants: st.PublicGrants, To: oidcSetting, Receipt: []byte(`{}`), Audit: Audit{Via: AuditViaConsole}}
	for name, tc := range map[string]struct {
		change func(*SignInCommit)
		want   SignInReason
	}{
		"another flat":   {func(c *SignInCommit) { c.FlatID = "other" }, SignInOtherFlat},
		"another seq":    {func(c *SignInCommit) { c.Seq = 7 }, SignInSeqChanged},
		"another grants": {func(c *SignInCommit) { c.Grants = []string{ProviderPortal} }, SignInGrantsChanged},
	} {
		c := base
		c.Approval = "apr-" + strings.ReplaceAll(name, " ", "-")
		applyingApproval(t, s, "app", c.Approval)
		tc.change(&c)
		if _, err := s.CommitSignIn(ctx, c); conflictReason(err) != tc.want {
			t.Errorf("%s: %v, want reason %d", name, err, tc.want)
		}
		if a, _ := s.GetApproval(ctx, c.Approval); len(a.ResultData) != 0 {
			t.Errorf("%s wrote a receipt", name)
		}
	}
	if after, _ := s.GetSignIn(ctx, "app"); after.SignIn.On() || after.Seq != 0 {
		t.Errorf("a stale commit changed the setting: %+v", after)
	}
	if rows := auditOf(t, s, AuditSignInSet); len(rows) != 0 {
		t.Errorf("stale commits wrote rows: %v", rows)
	}
	// A commit without an applying approval writes nothing either.
	c := base
	c.Approval = "apr-missing"
	if _, err := s.CommitSignIn(ctx, c); err == nil {
		t.Error("a commit without its approval applied")
	}
	if after, _ := s.GetSignIn(ctx, "app"); after.Seq != 0 {
		t.Errorf("the commit without its approval changed the setting: %+v", after)
	}
}

// The refusals of rule 8 in the store: a grant of another public provider while sign-in is on, sign-in on while one
// is granted, and a change to Public with both; the setting and the grants are read in the transaction that writes.
func TestSignInRefusesOtherPublicProviders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	makeFlat(t, s, "on")
	makeFlat(t, s, "granted")
	if _, err := commitSignIn(t, s, "on", "apr-on", oidcSetting); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{ProviderFunnel, ProviderZrok, ProviderCFQuick, ProviderNetbirdExpose, ProviderProxyPublic} {
		_, err := s.SetProviderPermission(ctx, "on", p, true, Audit{})
		var c *SignInConflict
		if !errors.As(err, &c) || c.Reason != SignInGrantRefused || !slices.Equal(c.Providers, []string{p}) {
			t.Errorf("grant of %s: %v", p, err)
		}
		if ok, _ := s.ProviderPermitted(ctx, "on", p); ok {
			t.Errorf("a refused grant of %s was stored", p)
		}
	}
	for _, p := range []string{ProviderPortal, ProviderTailscale, ProviderProxy, ProviderNetbird} {
		if _, err := s.SetProviderPermission(ctx, "on", p, true, Audit{}); err != nil {
			t.Errorf("grant of %s: %v", p, err)
		}
	}
	// A revocation always passes.
	if _, err := s.SetProviderPermission(ctx, "on", ProviderFunnel, false, Audit{}); err != nil {
		t.Errorf("revocation: %v", err)
	}
	if _, err := s.SetProviderPermission(ctx, "granted", ProviderFunnel, true, Audit{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetProviderPermission(ctx, "granted", ProviderZrok, true, Audit{}); err != nil {
		t.Fatal(err)
	}
	_, err := commitSignIn(t, s, "granted", "apr-granted", oidcSetting)
	var c *SignInConflict
	if !errors.As(err, &c) || c.Reason != SignInOnRefused || !slices.Equal(c.Providers, []string{ProviderFunnel, ProviderZrok}) || c.Public {
		t.Fatalf("sign-in on with grants: %v", err)
	}
	// A setting that stays off passes.
	if _, err := commitSignIn(t, s, "granted", "apr-off", SignIn{}); err != nil {
		t.Errorf("off with grants: %v", err)
	}
	// A flats.db that a hand wrote with sign-in on and a grant: the change to Public is refused.
	if _, err := s.db.Exec(`UPDATE flat_providers SET permitted=1 WHERE flat='granted'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE flats SET signin_provider='oidc', live_version=1 WHERE slug='granted'`); err != nil {
		t.Fatal(err)
	}
	from, _ := s.GetFlat(ctx, "granted")
	applyingApproval(t, s, "granted", "apr-vis")
	to := from
	to.Visibility, to.PublicAccess = Public, AccessView
	if err := s.CommitVisibility(ctx, to, from, "apr-vis", []byte(`{}`), Audit{}); conflictReason(err) != SignInPublicRefused {
		t.Fatalf("CommitVisibility = %v", err)
	}
	if f, _ := s.GetFlat(ctx, "granted"); f.Visibility != Private {
		t.Fatalf("the refused change made the flat %s", f.Visibility)
	}
}

// A grant and a sign-in apply that run at the same time, on two connections, give the result of one of the two
// orders: never both a grant of another public provider and sign-in on (rule 9). The two stores are two
// connections, so the order is the one of the SQLite write lock, not of a Go mutex.
func TestConcurrentGrantAndSignIn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flats.db")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	grants, signIns := 0, 0
	// At least 60 runs, and more until each order occurred, up to 600: a run with one order is no test of the other.
	for i := 0; i < 60 || (grants == 0 || signIns == 0) && i < 600; i++ {
		flat := fmt.Sprintf("race-%d", i)
		makeFlat(t, a, flat)
		st, err := b.GetSignIn(ctx, flat)
		if err != nil {
			t.Fatal(err)
		}
		id := "apr-" + flat
		applyingApproval(t, b, flat, id)
		var grantErr, signInErr error
		var wg sync.WaitGroup
		start := make(chan struct{})
		grant := func() {
			defer wg.Done()
			<-start
			_, grantErr = a.SetProviderPermission(ctx, flat, ProviderFunnel, true, Audit{})
		}
		signIn := func() {
			defer wg.Done()
			<-start
			_, signInErr = b.CommitSignIn(ctx, SignInCommit{Flat: flat, FlatID: st.FlatID, Seq: st.Seq, Grants: st.PublicGrants, To: oidcSetting,
				Approval: id, Receipt: []byte(`{}`)})
		}
		wg.Add(2)
		// The goroutine that starts first tends to win, so the two orders take turns.
		if i%2 == 0 {
			go grant()
			go signIn()
		} else {
			go signIn()
			go grant()
		}
		close(start)
		wg.Wait()
		granted, _ := a.ProviderPermitted(ctx, flat, ProviderFunnel)
		after, _ := a.GetSignIn(ctx, flat)
		switch {
		case granted && after.SignIn.On():
			t.Fatalf("%s: both the grant and sign-in on (%v, %v)", flat, grantErr, signInErr)
		case (grantErr == nil) == (signInErr == nil):
			t.Fatalf("%s: both or neither passed (%v, %v)", flat, grantErr, signInErr)
		case granted != (grantErr == nil), after.SignIn.On() != (signInErr == nil):
			t.Fatalf("%s: the stored state is not the one of the writes (%v, %v)", flat, grantErr, signInErr)
		}
		if granted {
			grants++
			if r := conflictReason(signInErr); r != SignInGrantsChanged && r != SignInOnRefused {
				t.Fatalf("%s: the sign-in after the grant: %v", flat, signInErr)
			}
		} else {
			signIns++
			if conflictReason(grantErr) != SignInGrantRefused {
				t.Fatalf("%s: the grant after sign-in: %v", flat, grantErr)
			}
		}
	}
	t.Logf("the grant first %d times, sign-in first %d times", grants, signIns)
	if grants == 0 || signIns == 0 {
		t.Fatalf("one order only: the grant first %d times, sign-in first %d times", grants, signIns)
	}
}

// The client secret is a row with the use signin and a binding (rules 24 to 27, 30 and 66).
func TestSignInSecretRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	flat := makeFlat(t, s, "app")
	b := SignInBinding{Issuer: "https://idp.example.com", ClientID: "flats-app"}
	put := func(text string, b SignInBinding) (bool, error) {
		return s.PutSignInSecret(ctx, "app", SealedSecret{Nonce: fill(1, 12), Ciphertext: []byte(text), KeyID: labelA, UpdatedAt: time.UnixMilli(5000)}, b, Audit{Via: AuditViaCLI})
	}
	if created, err := put("first", b); err != nil || !created {
		t.Fatalf("first set: %v, %v", created, err)
	}
	other := SignInBinding{Issuer: "https://other.example.com/realm", ClientID: "other-app"}
	if created, err := put("second", other); err != nil || created {
		t.Fatalf("second set: %v, %v", created, err)
	}
	sec, ok, err := s.SignInSecret(ctx, "app")
	if err != nil || !ok || sec.Name != SignInSecretName || sec.Use != SecretUseSignIn || sec.SignIn != other || string(sec.Ciphertext) != "second" || sec.KeyID != labelA {
		t.Fatalf("SignInSecret = %+v, %v, %v", sec, ok, err)
	}
	if st, _ := s.GetSignIn(ctx, "app"); st.Secret == nil || st.Secret.SignInBinding != other || st.Secret.UpdatedAt.UnixMilli() != 5000 {
		t.Fatalf("GetSignIn secret = %+v", st.Secret)
	}
	// Rule 30: the lists of ordinary secrets and the capture do not hold it; the commands of every value do.
	if secs, _ := s.ListSecrets(ctx, "app"); len(secs) != 0 {
		t.Errorf("ListSecrets = %+v", secs)
	}
	if _, secs, _, _ := s.RuntimeSettingsSnapshot(ctx, "app"); len(secs) != 0 {
		t.Errorf("RuntimeSettingsSnapshot = %+v", secs)
	}
	if all, _ := s.ListAllSecrets(ctx); len(all) != 1 || all[0].Use != SecretUseSignIn || all[0].SignIn != other {
		t.Errorf("ListAllSecrets = %+v", all)
	}
	if ids, _ := s.SealedKeyIDs(ctx); ids[labelA] != 1 {
		t.Errorf("SealedKeyIDs = %v", ids)
	}
	// Rule 31: the commands of ordinary secrets refuse the row.
	if err := s.DeleteSecret(ctx, "app", SignInSecretName, Audit{}); !errors.Is(err, ErrSignInSecretRow) {
		t.Errorf("DeleteSecret = %v", err)
	}
	if _, err := putChecked(t, s, "app", SignInSecretName, "x", SecretPut{}); !errors.Is(err, ErrSignInSecretRow) {
		t.Errorf("PutSecretChecked = %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO secrets(flat,name,nonce,ciphertext,updated_at,use) VALUES('app','OTHER',X'00',X'00',1,'signin')`); err == nil {
		t.Error("a second client secret of the flat was stored")
	}
	if sec, _, _ := s.SignInSecret(ctx, "app"); string(sec.Ciphertext) != "second" {
		t.Errorf("a refused write changed the value: %+v", sec)
	}
	if err := s.DeleteSignInSecret(ctx, "app", Audit{Via: AuditViaCLI}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSignInSecret(ctx, "app", Audit{Via: AuditViaCLI}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a second removal: %v", err)
	}
	var got []string
	for _, e := range allAudit(t, s) {
		if e.Flat != "app" || e.FlatID != flat.ID || e.Name != SignInSecretName || e.Via != AuditViaCLI {
			t.Errorf("row %+v", e)
		}
		got = append(got, e.Kind+" "+string(e.Data))
	}
	want := []string{
		`secret.set {"client_id":"flats-app","created":true,"issuer":"https://idp.example.com","use":"signin"}`,
		`secret.set {"client_id":"other-app","created":false,"issuer":"https://other.example.com/realm","use":"signin"}`,
		`secret.delete {"use":"signin"}`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("rows:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A readable secret with the name of the client secret, from before the name check, stays readable and refuses a set
// of the client secret (rule 31).
func TestReadableRowWithTheNameOfTheClientSecret(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	makeFlat(t, s, "app")
	if _, err := s.db.Exec(`INSERT INTO secrets(flat,name,nonce,ciphertext,updated_at) VALUES('app',?,X'01',X'02',1)`, SignInSecretName); err != nil {
		t.Fatal(err)
	}
	before := len(allAudit(t, s))
	if _, err := s.PutSignInSecret(ctx, "app", SealedSecret{Nonce: fill(1, 12), Ciphertext: []byte("v")}, SignInBinding{Issuer: "https://idp.example.com", ClientID: "c"}, Audit{}); !errors.Is(err, ErrSignInSecretName) {
		t.Fatalf("PutSignInSecret = %v", err)
	}
	if n := len(allAudit(t, s)); n != before {
		t.Errorf("the refusal wrote %d rows", n-before)
	}
	if secs, _ := s.ListSecrets(ctx, "app"); len(secs) != 1 || !bytes.Equal(secs[0].Ciphertext, []byte{2}) {
		t.Fatalf("the readable row: %+v", secs)
	}
	if err := s.DeleteSecret(ctx, "app", SignInSecretName, Audit{}); err != nil {
		t.Fatalf("the readable row is removed as a secret: %v", err)
	}
	if _, err := s.PutSignInSecret(ctx, "app", SealedSecret{Nonce: fill(1, 12), Ciphertext: []byte("v")}, SignInBinding{Issuer: "https://idp.example.com", ClientID: "c"}, Audit{}); err != nil {
		t.Fatalf("after the removal: %v", err)
	}
}

// rotate-key keeps the use and the binding, reset and the deletion of the flat write secret.delete with the use, and a
// rename keeps the row and the setting (rules 6 and 32).
func TestSignInSecretAcrossTheCommandsOfEveryValue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := SignInBinding{Issuer: "https://idp.example.com", ClientID: "flats-app"}
	setup := func(t *testing.T) *Store {
		s := open(t)
		makeFlat(t, s, "app")
		if _, err := s.PutSignInSecret(ctx, "app", SealedSecret{Nonce: fill(1, 12), Ciphertext: []byte("v"), KeyID: labelA}, b, Audit{}); err != nil {
			t.Fatal(err)
		}
		if _, err := putChecked(t, s, "app", "PLAIN", "p", SecretPut{}); err != nil {
			t.Fatal(err)
		}
		if _, err := commitSignIn(t, s, "app", "apr-on", oidcSetting); err != nil {
			t.Fatal(err)
		}
		return s
	}
	t.Run("rotate-key", func(t *testing.T) {
		s := setup(t)
		var names []string
		if n, _, err := s.Reseal(ctx, Reseal{From: labelA, To: labelB, Seal: func(v SealedValue) ([]byte, []byte, error) {
			names = append(names, v.Name)
			return fill(9, 12), []byte("resealed"), nil
		}}); err != nil || n != 2 {
			t.Fatalf("Reseal = %d, %v", n, err)
		}
		sec, _, _ := s.SignInSecret(ctx, "app")
		if !slices.Contains(names, SignInSecretName) || sec.Use != SecretUseSignIn || sec.SignIn != b || string(sec.Ciphertext) != "resealed" || sec.KeyID != labelB {
			t.Fatalf("after the re-seal: %+v (sealed %v)", sec, names)
		}
	})
	t.Run("reset", func(t *testing.T) {
		s := setup(t)
		if err := s.DeleteSealedValues(ctx, nil, Audit{Via: AuditViaCommand}); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.SignInSecret(ctx, "app"); ok {
			t.Fatal("reset kept the client secret")
		}
		var deleted []string
		for _, e := range auditOf(t, s, AuditSecretDelete) {
			deleted = append(deleted, e.Name+" "+string(e.Data))
		}
		if !slices.Equal(deleted, []string{"PLAIN ", SignInSecretName + ` {"use":"signin"}`}) {
			t.Errorf("secret.delete rows = %q", deleted)
		}
	})
	t.Run("rename and delete", func(t *testing.T) {
		s := setup(t)
		now := time.Now()
		if err := s.RenameFlat(ctx, "app", "renamed", time.Time{}, now); err != nil {
			t.Fatal(err)
		}
		st, err := s.GetSignIn(ctx, "renamed")
		if err != nil || !st.SignIn.Same(oidcSetting) || st.Seq != 1 || st.Secret == nil || st.Secret.SignInBinding != b {
			t.Fatalf("after the rename: %+v, %v", st, err)
		}
		if err := s.DeleteFlat(ctx, "renamed", Audit{Approval: "apr-del"}); err != nil {
			t.Fatal(err)
		}
		var deleted []string
		for _, e := range auditOf(t, s, AuditSecretDelete) {
			deleted = append(deleted, e.Name+" "+e.Via+" "+string(e.Data))
		}
		want := []string{`PLAIN flat-delete {"approval":"apr-del"}`, SignInSecretName + ` flat-delete {"approval":"apr-del","use":"signin"}`}
		if !slices.Equal(deleted, want) {
			t.Errorf("secret.delete rows = %q", deleted)
		}
		if _, err := s.GetSignIn(ctx, "renamed"); !errors.Is(err, ErrNotFound) {
			t.Errorf("the deletion kept the setting: %v", err)
		}
		// A new flat under the slug starts with sign-in off.
		makeFlat(t, s, "renamed")
		if st, _ := s.GetSignIn(ctx, "renamed"); st.SignIn.On() || st.Seq != 0 || st.Secret != nil {
			t.Errorf("a new flat: %+v", st)
		}
	})
}

// added and removed that do not fit into the data of a row keep their first entries and say truncated; two issuers
// of the longest form are cut (rule 65).
func TestSignInSetDataFits(t *testing.T) {
	t.Parallel()
	long := func(prefix string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%s%03d@%s.example.com", prefix, i, strings.Repeat("d", 200))
		}
		return out
	}
	from := SignIn{Provider: SignInCredential, Allow: long("a", 100)}
	to := SignIn{Provider: SignInCredential, Allow: long("b", 100)}
	var row signInSetRow
	data := signInSetData(from, to, "apr-1")
	if err := json.Unmarshal(data, &row); err != nil || len(data) > MaxAuditData || !row.Truncated || len(row.Added) == 0 || len(row.Added) == 100 || len(row.Removed) != 0 {
		t.Fatalf("%d bytes, truncated %t, %d added, %d removed, %v", len(data), row.Truncated, len(row.Added), len(row.Removed), err)
	}
	if !slices.Equal(row.Added, to.Allow[:len(row.Added)]) || row.From.Count != 100 || row.To.Count != 100 {
		t.Errorf("the row does not keep the first entries and the counts: %+v", row)
	}
	// The next entry would not have fit.
	row.Added = append(row.Added, to.Allow[len(row.Added)])
	if b := auditJSON(row); len(b) <= MaxAuditData {
		t.Error("an entry that fits was left out")
	}
	issuer := "https://idp.example.com/" + strings.Repeat("&", 2000)
	big := SignIn{Provider: SignInOIDC, Issuer: issuer, ClientID: strings.Repeat(`"`, 256), Allow: []string{"a@example.com"}}
	other := big
	other.Issuer += "x"
	data = signInSetData(big, other, "apr-2")
	var cut struct {
		From, To  struct{ Issuer string }
		Truncated bool
	}
	if err := json.Unmarshal(data, &cut); err != nil || len(data) > MaxAuditData || !cut.Truncated || cut.From.Issuer != issuer[:cutIssuer] {
		t.Fatalf("%d bytes: %+v, %v", len(data), cut, err)
	}
	// An ordinary change is whole.
	if data := signInSetData(SignIn{}, oidcSetting, "apr-3"); bytes.Contains(data, []byte("truncated")) {
		t.Errorf("an ordinary row says truncated: %s", data)
	}
}

// The kinds of rule 67 and their data, which the link and the revocation write.
func TestSignInLinkAndRevokeRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	flat := makeFlat(t, s, "app")
	at := time.Date(2026, 11, 26, 8, 30, 0, 0, time.FixedZone("x", 3600))
	if err := s.AppendSignInLink(ctx, "app", SignInLink{Subject: "guest one", Host: "abc.portal.example", ExpiresAt: at}, Audit{Via: AuditViaConsole, Actor: "op"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendSignInRevoke(ctx, "app", "", Audit{Via: AuditViaCLI}); err != nil {
		t.Fatal(err)
	}
	// A rename that failed after its new token secret writes its row outside the failed transaction.
	if err := s.AppendSignInRevoke(ctx, "app", SignInRevokeRename, Audit{Via: "api"}); err != nil {
		t.Fatal(err)
	}
	got := allAudit(t, s)
	want := []AuditEvent{
		{Kind: AuditSignInLink, Flat: "app", FlatID: flat.ID, Via: AuditViaConsole, Actor: "op", Data: json.RawMessage(`{"expires_at":"2026-11-26T07:30:00Z","host":"abc.portal.example","subject":"guest one"}`)},
		{Kind: AuditSignInRevoke, Flat: "app", FlatID: flat.ID, Via: AuditViaCLI, Data: json.RawMessage(`{}`)},
		{Kind: AuditSignInRevoke, Flat: "app", FlatID: flat.ID, Via: "api", Data: json.RawMessage(`{"cause":"rename"}`)},
	}
	if len(got) != 3 {
		t.Fatalf("rows = %v", auditKinds(got))
	}
	for i := range got {
		got[i].ID, got[i].Time = 0, time.Time{}
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("row %d = %+v (%s), want %+v (%s)", i, got[i], got[i].Data, want[i], want[i].Data)
		}
	}
	if string(SignInRevokeData(SignInRevokeRename)) != `{"cause":"rename"}` || string(SignInRevokeData(SignInRevokeDelete)) != `{"cause":"delete"}` {
		t.Error("the causes of rule 67")
	}
}

// Rules 41, 42 and 67: the signin.revoke row of a new token secret that a rename or a deletion gave is written in the
// transaction of that change, and a failing write of it leaves the change undone.
func TestSignInRevokeRowsOfARenameAndADeletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now()
	t.Run("written", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		flat := makeFlat(t, s, "app")
		if err := s.RenameFlatRevoked(ctx, "app", "moved", now.Add(time.Hour), now, &Audit{Via: AuditViaCLI, Actor: "op"}); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteFlatRevoked(ctx, "moved", Audit{}, &Audit{Via: AuditViaConsole}); err != nil {
			t.Fatal(err)
		}
		// Without a new token secret there is no row.
		makeFlat(t, s, "plain")
		if err := s.RenameFlatRevoked(ctx, "plain", "plain2", time.Time{}, now, nil); err != nil {
			t.Fatal(err)
		}
		var rows []AuditEvent
		for _, e := range allAudit(t, s) {
			if e.Kind == AuditSignInRevoke {
				e.ID, e.Time = 0, time.Time{}
				rows = append(rows, e)
			}
		}
		want := []AuditEvent{
			{Kind: AuditSignInRevoke, Flat: "app", FlatID: flat.ID, Via: AuditViaCLI, Actor: "op", Data: json.RawMessage(`{"cause":"rename"}`)},
			{Kind: AuditSignInRevoke, Flat: "moved", FlatID: flat.ID, Via: AuditViaConsole, Data: json.RawMessage(`{"cause":"delete"}`)},
		}
		if !reflect.DeepEqual(rows, want) {
			t.Fatalf("rows = %+v, want %+v", rows, want)
		}
	})
	t.Run("a failing write", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		makeFlat(t, s, "app")
		failAuditWhen(t, s, "NEW.kind='signin.revoke'")
		if err := s.RenameFlatRevoked(ctx, "app", "moved", now.Add(time.Hour), now, &Audit{Via: AuditViaCLI}); err == nil || !strings.Contains(err.Error(), "audit write failed late") {
			t.Fatalf("rename = %v", err)
		}
		if _, err := s.GetFlat(ctx, "app"); err != nil {
			t.Fatalf("the rename happened: %v", err)
		}
		if _, err := s.RedirectFor(ctx, "app", now); !errors.Is(err, ErrNotFound) {
			t.Fatalf("the rename left a redirect: %v", err)
		}
		if err := s.DeleteFlatRevoked(ctx, "app", Audit{}, &Audit{Via: AuditViaConsole}); err == nil || !strings.Contains(err.Error(), "audit write failed late") {
			t.Fatalf("delete = %v", err)
		}
		if _, err := s.GetFlat(ctx, "app"); err != nil {
			t.Fatalf("the deletion happened: %v", err)
		}
	})
}

// SignInExcludes names the public providers other than Portal.
func TestSignInExcludes(t *testing.T) {
	for p, want := range map[string]bool{
		ProviderFunnel: true, ProviderZrok: true, ProviderCFQuick: true, ProviderNetbirdExpose: true, ProviderProxyPublic: true,
		ProviderPortal: false, ProviderLocal: false, ProviderTailscale: false, ProviderNetbird: false, ProviderProxy: false,
	} {
		if SignInExcludes(p) != want {
			t.Errorf("SignInExcludes(%s) = %t", p, !want)
		}
	}
}
