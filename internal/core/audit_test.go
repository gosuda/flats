package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/store"
)

// docs/spec/audit-events.md: the events that the service writes for a secret and for a flat that holds secrets.

// auditOf lists the audit events of the database in dir, oldest first.
func auditOf(t *testing.T, dir string) []store.AuditEvent {
	t.Helper()
	rd, err := store.OpenAuditReader(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	evs, err := rd.List(context.Background(), store.AuditQuery{}, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func eventLines(evs []store.AuditEvent) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Kind+" "+e.Flat+"/"+e.Name+" "+e.Via)
	}
	return out
}

func TestSecretSetAndDeleteWriteAuditEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dir := newTestService(t)
	if _, err := s.CreateFlat(ctx, "app", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	f, err := s.st.GetFlat(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	const plain = "distinctive-plaintext-value-0123456789"
	const actor = `tailnet user "ann@example.org"`
	if err := s.SetSecret(WithAuditActor(ctx, actor), "app", "API_KEY", plain, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSecret(ctx, "app", "API_KEY", plain+"-2", ViaCLI); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSecret(WithAuditActor(ctx, actor), "app", "API_KEY", ViaConsole); err != nil {
		t.Fatal(err)
	}
	got := auditOf(t, dir)
	want := []string{"secret.set app/API_KEY console", "secret.set app/API_KEY cli", "secret.delete app/API_KEY console"}
	if !slices.Equal(eventLines(got), want) {
		t.Fatalf("events = %q, want %q", eventLines(got), want)
	}
	for i, e := range got {
		if e.FlatID != f.ID {
			t.Errorf("event %d: flat ID %q, want %q", i, e.FlatID, f.ID)
		}
	}
	if got[0].Actor != actor || got[1].Actor != "" || got[2].Actor != actor {
		t.Errorf("actors = %q, %q, %q", got[0].Actor, got[1].Actor, got[2].Actor)
	}
	var created, replaced map[string]any
	_ = json.Unmarshal(got[0].Data, &created)
	_ = json.Unmarshal(got[1].Data, &replaced)
	if created["created"] != true || replaced["created"] != false {
		t.Errorf("data = %v, %v", created, replaced)
	}
	if d := time.Since(got[0].Time); d < 0 || d > time.Minute {
		t.Errorf("time = %v, %v ago", got[0].Time, d)
	}

	// Nothing is written for what changes nothing: an invalid name or value, an agent, a name that does not exist.
	n := len(auditOf(t, dir))
	for name, err := range map[string]error{
		"invalid name":     s.SetSecret(ctx, "app", "lower", "v", ViaConsole),
		"reserved name":    s.SetSecret(ctx, "app", "DB", "v", ViaConsole),
		"invalid value":    s.SetSecret(ctx, "app", "OK_NAME", "bad\x00value", ViaConsole),
		"an agent sets":    s.SetSecret(ctx, "app", "OK_NAME", "v", ViaMCP),
		"missing flat":     s.SetSecret(ctx, "nope", "OK_NAME", "v", ViaConsole),
		"an agent deletes": s.DeleteSecret(ctx, "app", "OK_NAME", ViaMCP),
		"missing secret":   s.DeleteSecret(ctx, "app", "NOT_THERE", ViaConsole),
	} {
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if m := len(auditOf(t, dir)); m != n {
		t.Errorf("refused operations wrote %d events", m-n)
	}

	// No column of any audit row holds a value.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "flats.db"))+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT kind||flat||flat_id||name||via||actor||COALESCE(data,'') FROM audit_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var all string
		if err := rows.Scan(&all); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(all, "distinctive-plaintext") {
			t.Errorf("an audit row holds the value: %q", all)
		}
	}
}

// The deletion of a flat records the removal of its secrets, with the approval that deleted it, and the record
// outlives the flat and its log (rule 9).
func TestFlatDeletionRecordsItsSecretsAndKeepsThem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dir := newTestService(t)
	for _, slug := range []string{"agentdel", "consoledel"} {
		if _, err := s.CreateFlat(ctx, slug, "", ViaAPI); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"TWO", "ONE"} {
			if err := s.SetSecret(ctx, slug, name, "v", ViaConsole); err != nil {
				t.Fatal(err)
			}
		}
	}
	idAgent, err := s.st.GetFlat(ctx, "agentdel")
	if err != nil {
		t.Fatal(err)
	}
	before := len(auditOf(t, dir))
	id := resultID(t)(s.Delete(ctx, "agentdel", ViaMCP, "no longer needed"))
	if _, err := s.Decide(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	got := auditOf(t, dir)[before:]
	if want := []string{"secret.delete agentdel/ONE flat-delete", "secret.delete agentdel/TWO flat-delete"}; !slices.Equal(eventLines(got), want) {
		t.Fatalf("events = %q, want %q", eventLines(got), want)
	}
	for _, e := range got {
		var m map[string]any
		if err := json.Unmarshal(e.Data, &m); err != nil || m["approval"] != id || e.FlatID != idAgent.ID || e.Actor != "" {
			t.Errorf("event %+v data %v: want the approval %s and the flat ID", e, m, id)
		}
	}
	if _, err := s.st.GetFlat(ctx, "agentdel"); err == nil {
		t.Fatal("the flat is still there")
	}
	// The operator deletes the other flat in the console: there is no approval.
	before = len(auditOf(t, dir))
	if _, err := s.Delete(ctx, "consoledel", ViaConsole, ""); err != nil {
		t.Fatal(err)
	}
	got = auditOf(t, dir)[before:]
	if len(got) != 2 {
		t.Fatalf("console deletion: %d events", len(got))
	}
	for _, e := range got {
		if len(e.Data) != 0 || e.Via != "flat-delete" {
			t.Errorf("console deletion event %+v", e)
		}
	}
}

// The sweep trims the logs of flats and never an audit row (rule 2). The default events_keep is 5000.
func TestSweepKeepsTheAuditRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dir := newTestService(t)
	if _, err := s.CreateFlat(ctx, "app", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSecret(ctx, "app", "TOKEN", "v", ViaConsole); err != nil {
		t.Fatal(err)
	}
	for range 5100 {
		if _, err := s.st.AddEvent(ctx, store.Event{Flat: "app", Time: time.Now(), Level: "info", Kind: "runtime", Message: "line"}); err != nil {
			t.Fatal(err)
		}
	}
	s.Sweep(ctx)
	evs, err := s.st.ListEvents(ctx, "app", "", 0, 10000)
	if err != nil || len(evs) > 5000 {
		t.Fatalf("the sweep left %d events of the flat, %v; want it trimmed to 5000", len(evs), err)
	}
	if got := eventLines(auditOf(t, dir)); !slices.Equal(got, []string{"secret.set app/TOKEN console"}) {
		t.Errorf("audit rows after the sweep = %q", got)
	}
}
