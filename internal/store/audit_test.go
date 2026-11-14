package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// docs/spec/audit-events.md: the audit log in flats.db.

// allAudit returns every audit event of s, oldest first, through a reader of its own, as `flats audit ls` reads.
func allAudit(t *testing.T, s *Store) []AuditEvent {
	t.Helper()
	rd, err := OpenAuditReader(s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	out, err := rd.List(context.Background(), AuditQuery{}, 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// auditKinds lists the kinds of events, in order.
func auditKinds(evs []AuditEvent) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

// makeFlat creates a flat and returns it with its ID.
func makeFlat(t *testing.T, s *Store, slug string) Flat {
	t.Helper()
	now := time.Now()
	if err := s.CreateFlat(context.Background(), Flat{Slug: slug, Name: slug, Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	f, err := s.GetFlat(context.Background(), slug)
	if err != nil || f.ID == "" {
		t.Fatalf("GetFlat(%s) = %+v, %v", slug, f, err)
	}
	return f
}

// failAudit makes every insert into audit_events fail, which is how the tests inject a failure of the audit write.
func failAudit(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`CREATE TRIGGER audit_fail BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit write failed'); END`); err != nil {
		t.Fatal(err)
	}
}

func sealed(name string, fill byte) SealedSecret {
	return SealedSecret{Name: name, Nonce: bytes.Repeat([]byte{fill}, 12), Ciphertext: bytes.Repeat([]byte{fill + 1}, 32), UpdatedAt: time.Now()}
}

func dataOf(t *testing.T, e AuditEvent) map[string]any {
	t.Helper()
	var m map[string]any
	if len(e.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(e.Data, &m); err != nil {
		t.Fatalf("data %q: %v", e.Data, err)
	}
	return m
}

func TestAuditTableHasTheColumnsAndIndexesOfTheSpec(t *testing.T) {
	t.Parallel()
	s := open(t)
	var cols []string
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('audit_events') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, n)
	}
	rows.Close()
	if want := []string{"id", "at", "kind", "flat", "flat_id", "name", "via", "actor", "data"}; !slices.Equal(cols, want) {
		t.Errorf("columns = %v, want %v", cols, want)
	}
	for _, idx := range []string{"audit_events_kind", "audit_events_flat", "audit_events_flat_id"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_index_list('audit_events') WHERE name=?`, idx).Scan(&n); err != nil || n != 1 {
			t.Errorf("index %s: count %d, %v", idx, n, err)
		}
	}
	var fks int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_list('audit_events')`).Scan(&fks); err != nil || fks != 0 {
		t.Errorf("audit_events has %d foreign keys, %v", fks, err)
	}
	if err := s.AppendAudit(context.Background(), AuditEvent{Kind: AuditKeyBackup, Via: AuditViaCommand}); err != nil {
		t.Fatal(err)
	}
	var ms int64
	if err := s.db.QueryRow(`SELECT at FROM audit_events`).Scan(&ms); err != nil || ms < time.Now().Add(-time.Minute).UnixMilli() || ms > time.Now().Add(time.Minute).UnixMilli() {
		t.Errorf("at = %d, %v; want milliseconds as the other tables store them", ms, err)
	}
}

// A flats.db of version 24 migrates to 25 and keeps what it held (rule 25).
func TestMigrationFromVersion24AddsTheAuditLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flats.db")
	t.Run("old schema", func(t *testing.T) {
		withMigrations(t, migrations[:len(migrations)-1])
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		makeFlat(t, s, "keep")
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if info, err := Inspect(path); err != nil || info.Version != 24 {
			t.Fatalf("before: %+v, %v", info, err)
		}
	})
	if _, err := OpenAuditReader(path); !errors.Is(err, ErrAuditSchema) {
		t.Fatalf("OpenAuditReader of version 24 = %v, want ErrAuditSchema", err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if info, err := Inspect(path); err != nil || info.Version != 25 || info.Version != latestVersion() {
		t.Fatalf("after: %+v, %v", info, err)
	}
	if _, err := s.GetFlat(context.Background(), "keep"); err != nil {
		t.Errorf("the flat is gone: %v", err)
	}
	if got := allAudit(t, s); len(got) != 0 {
		t.Errorf("a migration wrote events: %v", auditKinds(got))
	}
}

// A database with an audit row holds data, as one with a row of autopilot_changes does (rule 26).
func TestAnAuditRowIsData(t *testing.T) {
	t.Parallel()
	s := open(t)
	if info, err := Inspect(s.path); err != nil || info.HasData {
		t.Fatalf("empty host: %+v, %v", info, err)
	}
	if err := s.AppendAudit(context.Background(), AuditEvent{Kind: AuditKeyRestore, Via: AuditViaCommand}); err != nil {
		t.Fatal(err)
	}
	if info, err := Inspect(s.path); err != nil || !info.HasData {
		t.Fatalf("with an audit row: %+v, %v", info, err)
	}
}

func TestSecretEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	f := makeFlat(t, s, "app")
	at := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	a := Audit{Time: at, Via: AuditViaConsole, Actor: "tailnet user \"ann@example.org\""}
	if err := s.PutSecret(ctx, "app", sealed("TOKEN", 1), a); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSecret(ctx, "app", sealed("TOKEN", 3), Audit{Via: AuditViaCLI}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSecret(ctx, "app", "TOKEN", Audit{Via: AuditViaCLI}); err != nil {
		t.Fatal(err)
	}
	got := allAudit(t, s)
	if want := []string{AuditSecretSet, AuditSecretSet, AuditSecretDelete}; !slices.Equal(auditKinds(got), want) {
		t.Fatalf("kinds = %v, want %v", auditKinds(got), want)
	}
	first := got[0]
	if first.Flat != "app" || first.FlatID != f.ID || first.Name != "TOKEN" || first.Via != "console" || first.Actor != a.Actor || !first.Time.Equal(at) {
		t.Errorf("first event = %+v", first)
	}
	if m := dataOf(t, first); m["created"] != true {
		t.Errorf("first set: data = %v, want created true", m)
	}
	if m := dataOf(t, got[1]); m["created"] != false || got[1].Via != "cli" || got[1].Actor != "" {
		t.Errorf("replace: %+v data %v, want created false, via cli, no actor", got[1], m)
	}
	if d := got[2]; d.Name != "TOKEN" || d.Flat != "app" || d.FlatID != f.ID || d.Via != "cli" || len(d.Data) != 0 {
		t.Errorf("delete = %+v", d)
	}
	// A delete of a name that does not exist changes nothing and writes nothing.
	if err := s.DeleteSecret(ctx, "app", "TOKEN", Audit{Via: AuditViaCLI}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete of a missing secret = %v, want ErrNotFound", err)
	}
	if n := len(allAudit(t, s)); n != 3 {
		t.Errorf("a delete of a missing name wrote an event: %d events", n)
	}
	// A secret that collides with an environment variable is refused and writes nothing.
	if err := s.PutEnv(ctx, "app", EnvVar{Name: "PLAIN", Value: "v", UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSecret(ctx, "app", sealed("PLAIN", 5), Audit{Via: AuditViaConsole}); !errors.Is(err, ErrEnvCollision) {
		t.Fatalf("collision = %v, want ErrEnvCollision", err)
	}
	if n := len(allAudit(t, s)); n != 3 {
		t.Errorf("a refused set wrote an event: %d events", n)
	}
}

// No audit row holds the sealed bytes of a secret or of a key (rule 3).
func TestNoAuditRowHoldsASealedValue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	makeFlat(t, s, "app")
	sec := sealed("TOKEN", 0x41)
	if err := s.PutSecret(ctx, "app", sec, Audit{Via: AuditViaConsole}); err != nil {
		t.Fatal(err)
	}
	key := HostKey{ID: "tailscale", Nonce: bytes.Repeat([]byte{0x51}, 12), Ciphertext: bytes.Repeat([]byte{0x52}, 40), Source: "console", UpdatedAt: time.Now()}
	if err := s.PutHostKey(ctx, key, Audit{Via: AuditViaConsole}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query(`SELECT id,at,kind,flat,flat_id,name,via,actor,COALESCE(data,'') FROM audit_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
		var cols [9]sql.RawBytes
		dest := make([]any, 9)
		for i := range cols {
			dest[i] = &cols[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		for _, c := range cols {
			for _, secret := range [][]byte{sec.Nonce, sec.Ciphertext, key.Nonce, key.Ciphertext} {
				if bytes.Contains(c, secret) {
					t.Errorf("an audit row holds sealed bytes: %q", c)
				}
			}
		}
	}
	if n != 2 {
		t.Errorf("%d audit rows, want 2", n)
	}
}

func TestFlatDeletionWritesTheEventsOfItsSecretsAndKeepsThem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	f := makeFlat(t, s, "old")
	other := makeFlat(t, s, "other")
	for _, n := range []string{"B_TWO", "A_ONE"} {
		if err := s.PutSecret(ctx, "old", sealed(n, 7), Audit{Via: AuditViaConsole}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutSecret(ctx, "other", sealed("KEEP", 9), Audit{Via: AuditViaConsole}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddEvent(ctx, Event{Flat: "old", Time: time.Now(), Level: "info", Kind: "secret", Message: "a flat event"}); err != nil {
		t.Fatal(err)
	}
	before := len(allAudit(t, s))
	if err := s.DeleteFlat(ctx, "old", Audit{Approval: "ap_123"}); err != nil {
		t.Fatal(err)
	}
	got := allAudit(t, s)[before:]
	if len(got) != 2 {
		t.Fatalf("%d new events, want 2: %+v", len(got), got)
	}
	for i, name := range []string{"A_ONE", "B_TWO"} {
		e := got[i]
		if e.Kind != AuditSecretDelete || e.Name != name || e.Flat != "old" || e.FlatID != f.ID || e.Via != AuditViaFlatDelete || e.Actor != "" {
			t.Errorf("event %d = %+v", i, e)
		}
		if m := dataOf(t, e); m["approval"] != "ap_123" {
			t.Errorf("event %d data = %v, want the approval", i, m)
		}
	}
	if evs, err := s.ListEvents(ctx, "old", "", 0, 10); err != nil || len(evs) != 0 {
		t.Errorf("the flat's own log = %v, %v; want it gone", evs, err)
	}
	if err := s.DeleteFlat(ctx, "other", Audit{}); err != nil {
		t.Fatal(err)
	}
	last := allAudit(t, s)
	if e := last[len(last)-1]; e.Name != "KEEP" || e.FlatID != other.ID || dataOf(t, e) != nil {
		t.Errorf("a deletion without an approval: %+v", e)
	}
	// A flat without secrets writes nothing, and a missing one is an error that writes nothing.
	makeFlat(t, s, "empty")
	n := len(allAudit(t, s))
	if err := s.DeleteFlat(ctx, "empty", Audit{}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFlat(ctx, "missing", Audit{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete of a missing flat = %v", err)
	}
	if len(allAudit(t, s)) != n {
		t.Error("a flat without secrets wrote an event")
	}
}

func TestRenameKeepsTheRowsUnderTheOldSlugAndTheSameFlatID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	f := makeFlat(t, s, "first")
	if err := s.PutSecret(ctx, "first", sealed("TOKEN", 1), Audit{Via: AuditViaConsole}); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameFlat(ctx, "first", "second", time.Time{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSecret(ctx, "second", sealed("TOKEN", 2), Audit{Via: AuditViaConsole}); err != nil {
		t.Fatal(err)
	}
	got := allAudit(t, s)
	if len(got) != 2 || got[0].Flat != "first" || got[1].Flat != "second" || got[0].FlatID != f.ID || got[1].FlatID != f.ID {
		t.Fatalf("rows = %+v; want the old slug and the new one under one flat ID", got)
	}
	rd, err := OpenAuditReader(s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	id, found, err := rd.FlatID(ctx, "second")
	if err != nil || !found || id != f.ID {
		t.Fatalf("FlatID(second) = %q, %v, %v", id, found, err)
	}
	byID, err := rd.List(ctx, AuditQuery{FlatID: id}, 0, 10)
	if err != nil || len(byID) != 2 {
		t.Fatalf("by ID: %d rows, %v; want the history from before the rename", len(byID), err)
	}
	bySlug, err := rd.List(ctx, AuditQuery{Flat: "first"}, 0, 10)
	if err != nil || len(bySlug) != 1 {
		t.Fatalf("by the old slug: %d rows, %v", len(bySlug), err)
	}
	if _, found, _ := rd.FlatID(ctx, "first"); found {
		t.Error("the old slug resolves to a flat")
	}
}

// A flat created again under a slug that a deleted flat used has another ID (rule 20).
func TestARecreatedSlugIsAnotherFlat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	old := makeFlat(t, s, "again")
	if err := s.PutSecret(ctx, "again", sealed("TOKEN", 1), Audit{Via: AuditViaConsole}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFlat(ctx, "again", Audit{}); err != nil {
		t.Fatal(err)
	}
	fresh := makeFlat(t, s, "again")
	if err := s.PutSecret(ctx, "again", sealed("TOKEN", 2), Audit{Via: AuditViaConsole}); err != nil {
		t.Fatal(err)
	}
	if fresh.ID == old.ID {
		t.Fatal("the flat created again has the old ID")
	}
	rd, err := OpenAuditReader(s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	for id, want := range map[string]int{old.ID: 2, fresh.ID: 1} {
		rows, err := rd.List(ctx, AuditQuery{FlatID: id}, 0, 10)
		if err != nil || len(rows) != want {
			t.Errorf("flat %s: %d rows, %v; want %d", id, len(rows), err, want)
		}
	}
	// Without an ID the slug selects the rows that were written under it, of both flats.
	if rows, err := rd.List(ctx, AuditQuery{Flat: "again"}, 0, 10); err != nil || len(rows) != 3 {
		t.Errorf("by slug: %d rows, %v; want the rows of both flats", len(rows), err)
	}
}

// The trimming of a flat's log and the sweep never touch audit rows (rule 2).
func TestTrimmingAFlatLogKeepsTheAuditRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	makeFlat(t, s, "app")
	if err := s.PutSecret(ctx, "app", sealed("TOKEN", 1), Audit{Via: AuditViaConsole}); err != nil {
		t.Fatal(err)
	}
	for i := range 30 {
		if _, err := s.AddEvent(ctx, Event{Flat: "app", Time: time.Now(), Level: "info", Kind: "runtime", Message: fmt.Sprint("line ", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.PruneEvents(ctx, "app", 3); err != nil || n != 27 {
		t.Fatalf("PruneEvents = %d, %v", n, err)
	}
	if n := len(allAudit(t, s)); n != 1 {
		t.Errorf("%d audit rows after the trimming, want 1", n)
	}
}

func TestHostKeyEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	value := func(id, source string) HostKey {
		return HostKey{ID: id, Nonce: bytes.Repeat([]byte{1}, 12), Ciphertext: bytes.Repeat([]byte{2}, 30), Source: source, UpdatedAt: time.Now()}
	}
	steps := []struct {
		run  func() error
		want []string // kind and the fields of data that the test checks, as "kind via data-json"
	}{
		{func() error {
			return s.PutHostKey(ctx, value("tailscale", "flag"), Audit{Via: AuditViaStart, From: "flag"})
		}, []string{`key.set start {"from":"flag","replaced":false}`}},
		{func() error {
			return s.PutHostKey(ctx, value("tailscale", "console"), Audit{Via: AuditViaConsole, Actor: "x"})
		}, []string{`key.set console {"replaced":true}`}},
		{func() error { return s.PutHostKey(ctx, value("zrok", "cli"), Audit{Via: AuditViaCommand}) }, []string{`key.set command {"replaced":false}`}},
		// The enrolment consumes the waiting token: the record stays and holds no value.
		{func() error {
			return s.PutHostKey(ctx, HostKey{ID: "zrok", Source: "console", UpdatedAt: time.Now()}, Audit{Via: AuditViaStart, Reason: "connected"})
		}, []string{`key.delete start {"reason":"connected"}`}},
		// A record without a value over one without writes nothing, and nor does its removal.
		{func() error {
			return s.PutHostKey(ctx, HostKey{ID: "zrok", Source: "console", UpdatedAt: time.Now()}, Audit{Via: AuditViaConsole})
		}, nil},
		{func() error { return s.DeleteHostKey(ctx, "zrok", Audit{Via: AuditViaConsole}) }, nil},
		{func() error { return s.DeleteHostKey(ctx, "netbird", Audit{Via: AuditViaConsole}) }, nil}, // no record at all
		{func() error { return s.DeleteHostKey(ctx, "tailscale", Audit{Via: AuditViaCommand}) }, []string{`key.delete command {"reason":"removed"}`}},
	}
	seen := 0
	for i, st := range steps {
		if err := st.run(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		got := allAudit(t, s)[seen:]
		var lines []string
		for _, e := range got {
			lines = append(lines, fmt.Sprintf("%s %s %s", e.Kind, e.Via, e.Data))
		}
		if !slices.Equal(lines, st.want) {
			t.Fatalf("step %d wrote %q, want %q", i, lines, st.want)
		}
		seen += len(got)
	}
	// The names of the events are the keys.
	names := []string{}
	for _, e := range allAudit(t, s) {
		names = append(names, e.Name)
	}
	if want := []string{"tailscale", "tailscale", "zrok", "zrok", "tailscale"}; !slices.Equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
}

func TestResetWritesAnEventForEachValueAndOneForTheReset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	a, b := makeFlat(t, s, "alpha"), makeFlat(t, s, "bravo")
	for _, p := range []struct{ flat, name string }{{"bravo", "TWO"}, {"alpha", "ONE"}} {
		if err := s.PutSecret(ctx, p.flat, sealed(p.name, 1), Audit{Via: AuditViaConsole}); err != nil {
			t.Fatal(err)
		}
	}
	k := HostKey{ID: "netbird", Nonce: bytes.Repeat([]byte{1}, 12), Ciphertext: bytes.Repeat([]byte{2}, 30), Source: "cli", UpdatedAt: time.Now()}
	if err := s.PutHostKey(ctx, k, Audit{Via: AuditViaCommand}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutHostKey(ctx, HostKey{ID: "zrok", Source: "console", UpdatedAt: time.Now()}, Audit{}); err != nil { // a record without a value
		t.Fatal(err)
	}
	before := len(allAudit(t, s))
	ev := Event{Flat: "alpha", Time: time.Now(), Level: "warn", Kind: "secret", Message: "reset"}
	if err := s.DeleteSealedValues(ctx, []Event{ev}, Audit{Via: AuditViaCommand, Actor: "operator"}); err != nil {
		t.Fatal(err)
	}
	got := allAudit(t, s)[before:]
	var lines []string
	for _, e := range got {
		lines = append(lines, fmt.Sprintf("%s %s/%s %s %s", e.Kind, e.Flat, e.Name, e.Via, e.Actor))
	}
	want := []string{
		"secret.delete alpha/ONE command operator",
		"secret.delete bravo/TWO command operator",
		"key.delete /netbird command operator",
		"key.reset / command operator",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("events = %q, want %q", lines, want)
	}
	if got[0].FlatID != a.ID || got[1].FlatID != b.ID {
		t.Errorf("flat IDs = %q, %q", got[0].FlatID, got[1].FlatID)
	}
	if m := dataOf(t, got[3]); m["secrets"] != float64(2) || m["keys"] != float64(1) {
		t.Errorf("key.reset data = %v", m)
	}
	if _, err := s.GetHostKey(ctx, "zrok"); err != nil {
		t.Errorf("the record without a value is gone: %v", err)
	}
}

// With the audit write failing, each operation fails and leaves the old state (rule 16).
func TestAFailedAuditWriteLeavesTheOldState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	makeFlat(t, s, "app")
	makeFlat(t, s, "doomed")
	if err := s.PutSecret(ctx, "app", sealed("OLD", 1), Audit{Via: AuditViaConsole}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSecret(ctx, "doomed", sealed("KEPT", 1), Audit{Via: AuditViaConsole}); err != nil {
		t.Fatal(err)
	}
	k := HostKey{ID: "tailscale", Nonce: bytes.Repeat([]byte{1}, 12), Ciphertext: bytes.Repeat([]byte{2}, 30), Source: "console", UpdatedAt: time.Now()}
	if err := s.PutHostKey(ctx, k, Audit{Via: AuditViaConsole}); err != nil {
		t.Fatal(err)
	}
	failAudit(t, s)
	changed := k
	changed.Ciphertext = bytes.Repeat([]byte{9}, 30)
	for name, run := range map[string]func() error{
		"set a new secret": func() error { return s.PutSecret(ctx, "app", sealed("NEW", 3), Audit{Via: AuditViaConsole}) },
		"replace a secret": func() error { return s.PutSecret(ctx, "app", sealed("OLD", 5), Audit{Via: AuditViaConsole}) },
		"delete a secret":  func() error { return s.DeleteSecret(ctx, "app", "OLD", Audit{Via: AuditViaConsole}) },
		"store a key":      func() error { return s.PutHostKey(ctx, changed, Audit{Via: AuditViaConsole}) },
		"remove a key value": func() error {
			return s.PutHostKey(ctx, HostKey{ID: "tailscale", Source: "console", UpdatedAt: time.Now()}, Audit{})
		},
		"delete a key":         func() error { return s.DeleteHostKey(ctx, "tailscale", Audit{Via: AuditViaConsole}) },
		"reset":                func() error { return s.DeleteSealedValues(ctx, nil, Audit{Via: AuditViaCommand}) },
		"delete a flat":        func() error { return s.DeleteFlat(ctx, "doomed", Audit{}) },
		"append a plain event": func() error { return s.AppendAudit(ctx, AuditEvent{Kind: AuditKeyBackup}) },
	} {
		err := run()
		if err == nil || !strings.Contains(err.Error(), "audit write failed") {
			t.Errorf("%s: err = %v, want the failure of the audit write", name, err)
		}
	}
	secrets, err := s.ListSecrets(ctx, "app")
	if err != nil || len(secrets) != 1 || secrets[0].Name != "OLD" || !bytes.Equal(secrets[0].Ciphertext, sealed("OLD", 1).Ciphertext) {
		t.Errorf("secrets of app = %+v, %v; want the old one only", secrets, err)
	}
	if got, err := s.GetHostKey(ctx, "tailscale"); err != nil || !bytes.Equal(got.Ciphertext, k.Ciphertext) {
		t.Errorf("the key = %+v, %v; want the old value", got, err)
	}
	if _, err := s.GetFlat(ctx, "doomed"); err != nil {
		t.Errorf("the flat is gone: %v", err)
	}
	if n, err := s.ListSecrets(ctx, "doomed"); err != nil || len(n) != 1 {
		t.Errorf("the secrets of the flat = %+v, %v", n, err)
	}
}

func TestTheActorIsCutAndTheDataIsBounded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	long := strings.Repeat("a", 511) + "é" + strings.Repeat("b", 100) // the two bytes of é straddle byte 512
	if err := s.AppendAudit(ctx, AuditEvent{Kind: AuditKeyBackup, Via: AuditViaCommand, Actor: long}); err != nil {
		t.Fatal(err)
	}
	got := allAudit(t, s)[0].Actor
	if len(got) != 511 || strings.Contains(got, "é") || len(long) < 600 {
		t.Errorf("actor of %d bytes stored as %d bytes", len(long), len(got))
	}
	if err := s.AppendAudit(ctx, AuditEvent{Kind: AuditKeyBackup, Data: json.RawMessage(`"` + strings.Repeat("x", MaxAuditData) + `"`)}); err == nil {
		t.Error("an event with more than 4 KiB of data was written")
	}
	if n := len(allAudit(t, s)); n != 1 {
		t.Errorf("%d events", n)
	}
}

func TestReaderFiltersAndPages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	base := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	for i := range 25 {
		kind := AuditKeyBackup
		if i%5 == 0 {
			kind = AuditKeyRestore
		}
		if err := s.AppendAudit(ctx, AuditEvent{Time: base.Add(time.Duration(i) * time.Minute), Kind: kind, Via: AuditViaCommand, Name: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	rd, err := OpenAuditReader(s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	names := func(q AuditQuery, after int64, limit int) []string {
		t.Helper()
		evs, err := rd.List(ctx, q, after, limit)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range evs {
			out = append(out, e.Name)
		}
		return out
	}
	if got := names(AuditQuery{Kind: AuditKeyRestore}, 0, 100); !slices.Equal(got, []string{"0", "5", "10", "15", "20"}) {
		t.Errorf("by kind = %v", got)
	}
	if got := names(AuditQuery{Since: base.Add(22 * time.Minute)}, 0, 100); !slices.Equal(got, []string{"22", "23", "24"}) {
		t.Errorf("since = %v", got)
	}
	// Pages of 10 by id join into the whole log, with no row twice and none lost.
	var all []string
	var after int64
	for {
		evs, err := rd.List(ctx, AuditQuery{}, after, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			all = append(all, e.Name)
			after = e.ID
		}
		if len(evs) < 10 {
			break
		}
	}
	if len(all) != 25 || all[0] != "0" || all[24] != "24" {
		t.Errorf("paged listing = %v", all)
	}
	// Start gives the id before the newest N matches, and 0 when there are no more matches than N or N is 0.
	for _, tc := range []struct {
		q     AuditQuery
		limit int
		want  []string
	}{
		{AuditQuery{}, 3, []string{"22", "23", "24"}},
		{AuditQuery{Kind: AuditKeyRestore}, 2, []string{"15", "20"}},
		{AuditQuery{}, 100, nil}, // more than there are: all of them
		{AuditQuery{}, 0, nil},   // 0 lists everything
	} {
		start, err := rd.Start(ctx, tc.q, tc.limit)
		if err != nil {
			t.Fatal(err)
		}
		got := names(tc.q, start, 100)
		if tc.want == nil {
			if len(got) != 25 {
				t.Errorf("limit %d: %d events, want all 25", tc.limit, len(got))
			}
			continue
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("limit %d of %+v = %v, want %v", tc.limit, tc.q, got, tc.want)
		}
	}
}

func TestReaderSeesACommitOfAHostThatRunsAndRefusesOtherDatabases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	rd, err := OpenAuditReader(s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	if evs, err := rd.List(ctx, AuditQuery{}, 0, 10); err != nil || len(evs) != 0 {
		t.Fatalf("empty log: %v, %v", evs, err)
	}
	// The host writes after the reader opened. A plain read-only open sees the write-ahead log.
	if err := s.AppendAudit(ctx, AuditEvent{Kind: AuditKeyBackup, Via: AuditViaCommand}); err != nil {
		t.Fatal(err)
	}
	if evs, err := rd.List(ctx, AuditQuery{}, 0, 10); err != nil || len(evs) != 1 {
		t.Fatalf("after a commit of the host: %v, %v; the reader missed it", evs, err)
	}
	// A missing database is fs.ErrNotExist and the reader creates nothing.
	dir := t.TempDir()
	if _, err := OpenAuditReader(filepath.Join(dir, "flats.db")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing database: %v, want fs.ErrNotExist", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("the reader created %d files", len(ents))
	}
}

// A database that the host closed cleanly has no -wal file; the open is then immutable and creates nothing (rule 22).
func TestReaderOfACleanlyStoppedHostCreatesNoFile(t *testing.T) {
	t.Parallel()
	s := open(t)
	if err := s.AppendAudit(context.Background(), AuditEvent{Kind: AuditKeyBackup, Via: AuditViaCommand}); err != nil {
		t.Fatal(err)
	}
	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	list := func() []string {
		ents, err := os.ReadDir(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range ents {
			out = append(out, e.Name())
		}
		return out
	}
	before := list()
	rd, err := OpenAuditReader(path)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := rd.List(context.Background(), AuditQuery{}, 0, 10)
	rd.Close()
	if err != nil || len(evs) != 1 {
		t.Fatalf("List = %v, %v", evs, err)
	}
	if after := list(); !slices.Equal(before, after) {
		t.Errorf("the directory changed: %v -> %v", before, after)
	}
}

// The open is plain whenever a -wal or -journal file exists, also one of 0 bytes, which a host that started and has not
// written yet has (rule 22).
func TestReaderOpenIsNeverImmutableWhileAHostMayWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "flats.db")
	if q := auditReadQuery(path); q != "?mode=ro&immutable=1" {
		t.Errorf("no sidecar: %q, want an immutable open", q)
	}
	for _, side := range []string{"-wal", "-journal"} {
		if err := os.WriteFile(path+side, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if q := auditReadQuery(path); q != "?mode=ro" {
			t.Errorf("%s of 0 bytes: %q, want a plain open", side, q)
		}
		if err := os.Remove(path + side); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAppendAuditBeside(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	// While a host holds the database, appends from other connections succeed, one after the other and together.
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := range 20 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs <- AppendAuditBeside(ctx, s.path, AuditEvent{Kind: AuditKeyBackup, Via: AuditViaCommand, Name: fmt.Sprint("beside", i)})
		}()
		go func() {
			defer wg.Done()
			errs <- s.PutSecret(ctx, "missing-flat-is-fine-here", sealed("X", 1), Audit{}) // a write of the host; it fails on the flat, not on the lock
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !strings.Contains(err.Error(), "FOREIGN KEY") {
			t.Errorf("a concurrent write failed: %v", err)
		}
	}
	if n := len(allAudit(t, s)); n != 20 {
		t.Errorf("%d events, want 20", n)
	}
	// A missing database is not created.
	dir := t.TempDir()
	if err := AppendAuditBeside(ctx, filepath.Join(dir, "flats.db"), AuditEvent{Kind: AuditKeyBackup}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing database: %v, want fs.ErrNotExist", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("created %d files", len(ents))
	}
}

func TestAppendAuditBesideRefusesAnotherSchemaAndWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flats.db")
	t.Run("old schema", func(t *testing.T) {
		withMigrations(t, migrations[:len(migrations)-1])
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	})
	err := AppendAuditBeside(context.Background(), path, AuditEvent{Kind: AuditKeyBackup})
	if !errors.Is(err, ErrAuditSchema) {
		t.Fatalf("version 24: err = %v, want ErrAuditSchema", err)
	}
	// The database was not migrated by the attempt.
	if info, err := Inspect(path); err != nil || info.Version != 24 {
		t.Fatalf("after the refusal: %+v, %v", info, err)
	}
}

// A -wal file of 0 bytes, which a host that started and has not written yet leaves, does not stop the reader, and the version
// is read through the same plain open (rule 22).
func TestReaderOpensADatabaseWhoseWalFileIsEmpty(t *testing.T) {
	t.Parallel()
	s := open(t)
	if err := s.AppendAudit(context.Background(), AuditEvent{Kind: AuditKeyBackup, Via: AuditViaCommand}); err != nil {
		t.Fatal(err)
	}
	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-wal", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rd, err := OpenAuditReader(path)
	if err != nil {
		t.Fatalf("OpenAuditReader with an empty -wal file: %v", err)
	}
	defer rd.Close()
	if evs, err := rd.List(context.Background(), AuditQuery{}, 0, 10); err != nil || len(evs) != 1 {
		t.Errorf("List = %v, %v", evs, err)
	}
	// An empty database file with no -wal file was never made; a file that is no SQLite file is refused.
	dir := t.TempDir()
	empty := filepath.Join(dir, "flats.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAuditReader(empty); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("empty file: %v, want fs.ErrNotExist", err)
	}
	junk := filepath.Join(dir, "junk.db")
	if err := os.WriteFile(junk, []byte(strings.Repeat("not sqlite ", 20)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAuditReader(junk); !errors.Is(err, ErrNotFlats) {
		t.Errorf("junk file: %v, want ErrNotFlats", err)
	}
}

// An older binary refuses a database of schema 25, as Open does, and so does its reader (rule 25).
func TestAnOlderBinaryRefusesAVersion25Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flats.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	t.Run("a registry that ends at 24", func(t *testing.T) {
		withMigrations(t, migrations[:len(migrations)-1])
		if _, err := Open(path); !errors.Is(err, ErrSchemaTooNew) {
			t.Errorf("Open = %v, want ErrSchemaTooNew", err)
		}
		if _, err := OpenAuditReader(path); !errors.Is(err, ErrSchemaTooNew) {
			t.Errorf("OpenAuditReader = %v, want ErrSchemaTooNew", err)
		}
	})
}

// The reserved kind key.rotate is stored and listed like the others (rule 15).
func TestTheReservedKeyRotateKindIsListed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	if err := s.AppendAudit(ctx, AuditEvent{Kind: AuditKeyRotate, Via: AuditViaCommand, Data: AuditData(map[string]any{"to": "bbbb"})}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAudit(ctx, AuditEvent{Kind: AuditKeyBackup, Via: AuditViaCommand}); err != nil {
		t.Fatal(err)
	}
	rd, err := OpenAuditReader(s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	evs, err := rd.List(ctx, AuditQuery{Kind: AuditKeyRotate}, 0, 10)
	if err != nil || len(evs) != 1 || string(evs[0].Data) != `{"to":"bbbb"}` {
		t.Errorf("List(key.rotate) = %+v, %v", evs, err)
	}
}

// No code but the audit code touches audit_events, and that code only creates, inserts and selects (rule 2). The scan covers
// every path that could change a row: retention, the sweep, a flat's deletion or rename, a restore of a flat's data, and the
// reset (a migration may create the table).
func TestNothingUpdatesOrDeletesAnAuditRow(t *testing.T) {
	t.Parallel()
	write := regexp.MustCompile(`(?is)\b(update|delete\s+from|drop\s+table|drop\s+index|alter\s+table|insert\s+or\s+replace|replace\s+into)\s+"?audit_events`)
	var touching []string
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !bytes.Contains(b, []byte("audit_events")) {
				return nil
			}
			touching = append(touching, filepath.ToSlash(path))
			if m := write.Find(b); m != nil {
				t.Errorf("%s changes or deletes audit rows: %q", path, m)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"../../internal/store/audit.go"}; !slices.Equal(touching, want) {
		t.Errorf("files that name audit_events: %v, want only %v", touching, want)
	}
}
