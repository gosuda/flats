package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// docs/spec/event-webhook.md: the migration to schema 26 (rule 28), the flat ID of an approval, and the audit kinds of
// rule 21.

// migrationsThrough is the registry as the binary of schema version v had it. The registry has no entry for the steps
// before 5, so a position in it is not a version.
func migrationsThrough(v int) []migration {
	i := slices.IndexFunc(migrations, func(m migration) bool { return m.version > v })
	if i < 0 {
		return migrations
	}
	return migrations[:i]
}

// indexesOf lists the index names of a table.
func indexesOf(t *testing.T, s *Store, table string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM pragma_index_list(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// A flats.db of version 25 migrates through 26 to the latest schema: host_keys takes the two webhook ids and keeps its
// rows, an approval gets the ID of its flat when the flat exists, the indexes exist, and an older binary refuses the result.
func TestMigrationFromVersion25AddsTheWebhookSupport(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flats.db")
	var keep Flat
	t.Run("old schema", func(t *testing.T) {
		withMigrations(t, migrationsThrough(25))
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		keep = makeFlat(t, s, "keep")
		for _, a := range []struct{ id, flat string }{{"apr-live", "keep"}, {"apr-gone", "gone"}} {
			if _, err := s.db.Exec(`INSERT INTO approvals(id,flat,action,params,status,via,requested_at) VALUES(?,?,?,?,?,?,?)`,
				a.id, a.flat, "delete", "{}", "pending", "mcp", 1000); err != nil {
				t.Fatal(err)
			}
		}
		// The schema is the one of version 25, before the key IDs, so the key is written as the code of that time wrote it.
		if _, err := s.db.Exec(`INSERT INTO host_keys(id,nonce,ciphertext,source,updated_at) VALUES('tailscale',?,?,'cli',?)`,
			bytes.Repeat([]byte{1}, 12), bytes.Repeat([]byte{2}, 30), unix(time.Now())); err != nil {
			t.Fatal(err)
		}
		// A grant and a list of origins from before the migration, written without the functions that audit now.
		for _, q := range []string{
			`INSERT INTO flat_providers(flat, provider, permitted) VALUES('keep', 'tailscale', 1)`,
			`INSERT INTO network_settings(flat, origins, updated_at) VALUES('keep', '["https://api.example.com"]', 1000)`,
		} {
			if _, err := s.db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if info, err := Inspect(path); err != nil || info.Version != 25 {
			t.Fatalf("before: %+v, %v", info, err)
		}
	})
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if info, err := Inspect(path); err != nil || info.Version != 27 || info.Version != latestVersion() {
		t.Fatalf("after: %+v, %v", info, err)
	}
	// The migration writes no row for what happened before it (rule 30): the grant and the origins of the old database
	// get none of the three new kinds.
	for _, e := range allAudit(t, s) {
		if e.Kind == AuditVisibilitySet || e.Kind == AuditNetworkSet || e.Kind == AuditProviderSet {
			t.Errorf("the migration wrote a %s row", e.Kind)
		}
	}
	if k, err := s.GetHostKey(ctx, "tailscale"); err != nil || !k.HasValue() || k.Source != "cli" {
		t.Errorf("the Tailscale key after the rebuild: %+v, %v", k, err)
	}
	for _, id := range []string{"webhook-url", "webhook-secret"} {
		k := HostKey{ID: id, Nonce: bytes.Repeat([]byte{3}, 12), Ciphertext: bytes.Repeat([]byte{4}, 30), Source: "cli", UpdatedAt: time.Now()}
		if err := s.PutHostKey(ctx, k, Audit{Via: AuditViaCommand}); err != nil {
			t.Errorf("PutHostKey(%s) = %v", id, err)
		}
	}
	if err := s.PutHostKey(ctx, HostKey{ID: "bogus", Nonce: bytes.Repeat([]byte{3}, 12), Ciphertext: bytes.Repeat([]byte{4}, 30), Source: "cli", UpdatedAt: time.Now()}, Audit{}); err == nil {
		t.Error("an id that is not a key was stored")
	}
	if a, err := s.GetApproval(ctx, "apr-live"); err != nil || a.FlatID != keep.ID {
		t.Errorf("approval of a flat that exists: flat_id %q, %v; want %q", a.FlatID, err, keep.ID)
	}
	if a, err := s.GetApproval(ctx, "apr-gone"); err != nil || a.FlatID != "" {
		t.Errorf("approval of a flat that is gone: flat_id %q, %v; want empty", a.FlatID, err)
	}
	for table, want := range map[string][]string{
		"approvals":    {"approvals_requested_at", "approvals_decided_at"},
		"audit_events": {"audit_events_at"},
	} {
		got := indexesOf(t, s, table)
		for _, idx := range want {
			if !slices.Contains(got, idx) {
				t.Errorf("%s has no index %s: %v", table, idx, got)
			}
		}
	}
	s.Close()
	t.Run("a binary of schema 25 refuses it", func(t *testing.T) {
		withMigrations(t, migrationsThrough(25))
		if _, err := Open(path); !errors.Is(err, ErrSchemaTooNew) {
			t.Errorf("Open = %v, want ErrSchemaTooNew", err)
		}
		if _, err := OpenAuditReader(path); !errors.Is(err, ErrSchemaTooNew) {
			t.Errorf("OpenAuditReader = %v, want ErrSchemaTooNew", err)
		}
	})
}

// An approval carries the ID of the flat that its slug named when it was made, so a flat that is deleted, and a flat
// that is created again under the same slug, are told apart. The ID is never part of the JSON of an approval (rule 28).
func TestAnApprovalCarriesTheIDOfItsFlat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	first := makeFlat(t, s, "app")
	now := time.Now()
	insert := func(id, flat string) {
		t.Helper()
		if err := s.InsertApproval(ctx, Approval{ID: id, Flat: flat, Action: "delete", Params: []byte(`{}`), Status: "pending", Via: "mcp", RequestedAt: now, IdempotencyKey: id}); err != nil {
			t.Fatal(err)
		}
	}
	insert("apr-1", "app")
	insert("apr-none", "nothing")
	if err := s.RenameFlat(ctx, "app", "renamed", now, now); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFlat(ctx, "renamed", Audit{}); err != nil {
		t.Fatal(err)
	}
	second := makeFlat(t, s, "app")
	insert("apr-2", "app")
	if first.ID == second.ID {
		t.Fatalf("a flat created again under a deleted slug has the same ID %q", first.ID)
	}
	for id, want := range map[string]string{"apr-1": first.ID, "apr-2": second.ID, "apr-none": ""} {
		a, err := s.GetApproval(ctx, id)
		if err != nil || a.FlatID != want {
			t.Errorf("%s: flat_id %q, %v; want %q", id, a.FlatID, err, want)
		}
	}
	a, _ := s.GetApproval(ctx, "apr-1")
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "flat_id") || strings.Contains(string(raw), first.ID) {
		t.Errorf("the JSON of an approval shows the ID of the flat: %s", raw)
	}
}

// visibilityFixture makes a Private flat "plan" with an approval that is being applied.
func visibilityFixture(t *testing.T) (*Store, Flat, Flat) {
	t.Helper()
	ctx := context.Background()
	s := open(t)
	now := time.Now()
	if err := s.CreateFlat(ctx, Flat{Slug: "plan", Name: "plan", Visibility: Private, LiveVersion: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"apr-a", "apr-b"} {
		if err := s.InsertApproval(ctx, Approval{ID: id, Flat: "plan", Action: "set_visibility", Params: []byte(`{}`), Status: "applying", RequestedAt: now, IdempotencyKey: id}); err != nil {
			t.Fatal(err)
		}
	}
	from, err := s.GetFlat(ctx, "plan")
	if err != nil {
		t.Fatal(err)
	}
	next := from
	next.Visibility, next.PublicAccess, next.UpdatedAt = Public, AccessView, now
	return s, from, next
}

// A visibility change that an approval applied writes a visibility.set row in the transaction of the change (rule 21),
// with the channel and identity of whoever decided it.
func TestCommitVisibilityWritesAVisibilityAuditRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, from, next := visibilityFixture(t)
	at := time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC)
	if err := s.CommitVisibility(ctx, next, from, "apr-a", []byte(`{}`), Audit{Time: at, Via: AuditViaConsole, Actor: "tailnet user \"ann@example.org\""}); err != nil {
		t.Fatal(err)
	}
	evs := allAudit(t, s)
	if len(evs) != 1 {
		t.Fatalf("%d audit rows, want 1: %v", len(evs), auditKinds(evs))
	}
	e := evs[0]
	if e.Kind != AuditVisibilitySet || e.Flat != "plan" || e.FlatID != from.ID || e.Name != "" || e.Via != AuditViaConsole || e.Actor != "tailnet user \"ann@example.org\"" || !e.Time.Equal(at) {
		t.Errorf("row = %+v", e)
	}
	want := map[string]any{
		"from":     map[string]any{"visibility": "private", "access": ""},
		"to":       map[string]any{"visibility": "public", "access": "view"},
		"approval": "apr-a",
	}
	if got := dataOf(t, e); !jsonEqual(got, want) {
		t.Errorf("data = %v, want %v", got, want)
	}

	// An agent's decision keeps its own channel.
	back, err := s.GetFlat(ctx, "plan")
	if err != nil {
		t.Fatal(err)
	}
	priv := back
	priv.Visibility, priv.PublicAccess = Private, ""
	if err := s.CommitVisibility(ctx, priv, back, "apr-b", []byte(`{}`), Audit{Via: AuditViaAutopilot, Actor: "an agent"}); err != nil {
		t.Fatal(err)
	}
	evs = allAudit(t, s)
	if len(evs) != 2 || evs[1].Via != AuditViaAutopilot || evs[1].Actor != "an agent" {
		t.Errorf("second row: %+v", evs)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// When the audit write fails the visibility does not change and the approval stays unfinished (rule 21).
func TestCommitVisibilityStopsWhenTheAuditWriteFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, from, next := visibilityFixture(t)
	failAudit(t, s)
	err := s.CommitVisibility(ctx, next, from, "apr-a", []byte(`{"status":"applied"}`), Audit{Via: AuditViaConsole})
	if err == nil || !strings.Contains(err.Error(), "audit write failed") {
		t.Fatalf("err = %v, want the audit failure", err)
	}
	got, err := s.GetFlat(ctx, "plan")
	if err != nil || got.Visibility != Private || got.AccessSeq != from.AccessSeq {
		t.Errorf("flat after the failed commit: %+v, %v", got, err)
	}
	if a, err := s.GetApproval(ctx, "apr-a"); err != nil || strings.Contains(string(a.ResultData), "applied") {
		t.Errorf("the receipt was written: %+v, %v", a, err)
	}
}

// A provider permission writes provider.set only when the stored permission changes, and a failed audit write leaves it as
// it was (rule 21 and rule 2).
func TestProviderPermissionAuditsOnlyAChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	f := makeFlat(t, s, "app")
	a := Audit{Via: AuditViaConsole, Actor: "console actor"}
	set := func(provider string, permitted bool) bool {
		t.Helper()
		changed, err := s.SetProviderPermission(ctx, "app", provider, permitted, a)
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	if !set(ProviderTailscale, true) {
		t.Error("a first grant reported no change")
	}
	evs := allAudit(t, s)
	if len(evs) != 1 || evs[0].Kind != AuditProviderSet || evs[0].Flat != "app" || evs[0].FlatID != f.ID || evs[0].Name != ProviderTailscale ||
		evs[0].Via != AuditViaConsole || evs[0].Actor != "console actor" || !jsonEqual(dataOf(t, evs[0]), map[string]any{"permitted": true}) {
		t.Fatalf("rows after the first grant: %+v", evs)
	}
	if set(ProviderTailscale, true) {
		t.Error("a repeated grant reported a change")
	}
	if !set(ProviderTailscale, false) {
		t.Error("a revocation reported no change")
	}
	if evs := allAudit(t, s); len(evs) != 2 || !jsonEqual(dataOf(t, evs[1]), map[string]any{"permitted": false}) {
		t.Fatalf("rows after the revocation: %+v", evs)
	}
	// A provider with no row counts as not permitted: revoking it changes nothing and writes nothing, and the write of the
	// row itself stays as it was before this change.
	if set(ProviderPortal, false) {
		t.Error("revoking a provider with no row reported a change")
	}
	if n := len(allAudit(t, s)); n != 2 {
		t.Errorf("%d rows after a no-op, want 2", n)
	}
	var rowsOf int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM flat_providers WHERE flat='app' AND provider=?`, ProviderPortal).Scan(&rowsOf); err != nil || rowsOf != 1 {
		t.Errorf("flat_providers rows for portal: %d, %v; want the row that the write always made", rowsOf, err)
	}

	failAudit(t, s)
	if _, err := s.SetProviderPermission(ctx, "app", ProviderPortal, true, a); err == nil || !strings.Contains(err.Error(), "audit write failed") {
		t.Fatalf("err = %v, want the audit failure", err)
	}
	if ok, err := s.ProviderPermitted(ctx, "app", ProviderPortal); err != nil || ok {
		t.Errorf("portal after the failed grant: %v, %v; want not permitted", ok, err)
	}
}

// The origin grants of a flat write network.set only when the list changes (rule 21 and rule 2).
func TestNetworkPolicyAuditsOnlyAChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	f := makeFlat(t, s, "app")
	a := Audit{Via: AuditViaCLI}
	set := func(origins []string) bool {
		t.Helper()
		changed, err := s.SetNetworkPolicy(ctx, "app", NetworkPolicy{Origins: origins, UpdatedAt: time.Now()}, a)
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	if set(nil) {
		t.Error("an empty list on a flat with no row reported a change")
	}
	if n := len(allAudit(t, s)); n != 0 {
		t.Fatalf("%d rows after an empty list, want 0", n)
	}
	if !set([]string{"https://api.example.com"}) {
		t.Error("a first list reported no change")
	}
	evs := allAudit(t, s)
	if len(evs) != 1 || evs[0].Kind != AuditNetworkSet || evs[0].Flat != "app" || evs[0].FlatID != f.ID || evs[0].Name != "" || evs[0].Via != AuditViaCLI ||
		!jsonEqual(dataOf(t, evs[0]), map[string]any{"count": 1, "origins": []string{"https://api.example.com"}}) {
		t.Fatalf("rows after the first list: %+v", evs)
	}
	if set([]string{"https://api.example.com"}) {
		t.Error("the same list reported a change")
	}
	if !set([]string{}) {
		t.Error("clearing the list reported no change")
	}
	if evs := allAudit(t, s); len(evs) != 2 || string(evs[1].Data) != `{"count":0,"origins":[]}` {
		t.Fatalf("rows after clearing: %+v", evs)
	}
	if got, err := s.NetworkPolicy(ctx, "app"); err != nil || len(got.Origins) != 0 {
		t.Fatalf("policy = %+v, %v", got, err)
	}

	failAudit(t, s)
	if _, err := s.SetNetworkPolicy(ctx, "app", NetworkPolicy{Origins: []string{"https://other.example.com"}, UpdatedAt: time.Now()}, a); err == nil || !strings.Contains(err.Error(), "audit write failed") {
		t.Fatalf("err = %v, want the audit failure", err)
	}
	if got, err := s.NetworkPolicy(ctx, "app"); err != nil || len(got.Origins) != 0 {
		t.Errorf("policy after the failed write = %+v, %v; want it unchanged", got, err)
	}
}

// longOrigins returns n origins of the longest form that the host accepts: a host name of 253 characters.
func longOrigins(n int) []string {
	out := make([]string, n)
	for i := range out {
		label := func(size int) string { return fmt.Sprintf("h%02d", i) + strings.Repeat("a", size-3) }
		out[i] = "https://" + label(63) + "." + label(63) + "." + label(63) + "." + label(61)
	}
	return out
}

// The longest list of origins that the host accepts is stored whole, as it was before the audit row existed. Its row
// holds the count of the whole list and the first origins that fit, and says that it is cut (rule 21).
func TestTheLongestOriginListIsStoredAndItsRowIsCut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	makeFlat(t, s, "app")
	origins := longOrigins(32)
	changed, err := s.SetNetworkPolicy(ctx, "app", NetworkPolicy{Origins: origins, UpdatedAt: time.Now()}, Audit{Via: AuditViaConsole})
	if err != nil || !changed {
		t.Fatalf("SetNetworkPolicy of the longest list = %t, %v", changed, err)
	}
	if got, err := s.NetworkPolicy(ctx, "app"); err != nil || !slices.Equal(got.Origins, origins) {
		t.Fatalf("stored %d origins, %v; want all %d", len(got.Origins), err, len(origins))
	}
	evs := allAudit(t, s)
	if len(evs) != 1 || len(evs[0].Data) > MaxAuditData {
		t.Fatalf("rows = %d, data of %d bytes; want one row within %d bytes", len(evs), len(evs[0].Data), MaxAuditData)
	}
	var data struct {
		Count     int
		Origins   []string
		Truncated bool
	}
	if err := json.Unmarshal(evs[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Count != 32 || !data.Truncated || len(data.Origins) == 0 || len(data.Origins) >= 32 || !slices.Equal(data.Origins, origins[:len(data.Origins)]) {
		t.Errorf("row data: count %d, truncated %t, %d origins; want 32, true and the first origins that fit", data.Count, data.Truncated, len(data.Origins))
	}
	// A list of ordinary length is not cut.
	if kept, truncated := FitOrigins(origins[:5]); truncated || len(kept) != 5 {
		t.Errorf("FitOrigins of 5 long origins = %d, %t", len(kept), truncated)
	}
	if kept, truncated := FitOrigins(nil); truncated || len(kept) != 0 {
		t.Errorf("FitOrigins(nil) = %v, %t", kept, truncated)
	}
}
