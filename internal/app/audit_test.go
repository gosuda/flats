package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/zrok"
	"github.com/gosuda/flats/internal/store"
)

// docs/spec/audit-events.md: the events that the operator-key paths and the key commands write.

// auditIn lists the audit events of the database in dir, oldest first.
func auditIn(t *testing.T, dir string) []store.AuditEvent {
	t.Helper()
	rd, err := store.OpenAuditReader(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	evs, err := rd.List(context.Background(), store.AuditQuery{}, 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// auditSummary is "kind name via actor data" for each event, so that a test compares a whole sequence.
func auditSummary(evs []store.AuditEvent) []string {
	var out []string
	for _, e := range evs {
		line := e.Kind + " " + e.Name + " " + e.Via
		if e.Actor != "" {
			line += " actor=" + e.Actor
		}
		if len(e.Data) > 0 {
			line += " " + string(e.Data)
		}
		out = append(out, strings.TrimSpace(strings.ReplaceAll(line, "  ", " ")))
	}
	return out
}

// newAudit returns the events that were written after the first n.
func newAudit(t *testing.T, dir string, n int) []store.AuditEvent {
	t.Helper()
	return auditIn(t, dir)[n:]
}

// breakAudit makes every insert into audit_events fail through a connection of its own, as a failure of the audit write.
func breakAudit(t *testing.T, dir string) (restore func()) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "flats.db"))+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER audit_fail BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit write failed'); END`); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := db.Exec(`DROP TRIGGER audit_fail`); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
}

// The console routes' methods write key.set and key.delete with the identity that the route put in the context (rules 10, 11).
func TestConsoleOperatorKeyEvents(t *testing.T) {
	clearKeyEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	h, _ := consoleHostWith(t, dir, nil)
	n := len(auditIn(t, dir))
	who := core.WithAuditActor(ctx, `tailnet user "ann@example.org"`)
	if err := h.SetTailscaleKey(who, keyA); err != nil {
		t.Fatal(err)
	}
	if err := h.SetTailscaleKey(who, keyB); err != nil {
		t.Fatal(err)
	}
	if err := h.DeleteTailscaleKey(who); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`key.set tailscale console actor=tailnet user "ann@example.org" {"replaced":false}`,
		`key.set tailscale console actor=tailnet user "ann@example.org" {"replaced":true}`,
		`key.delete tailscale console actor=tailnet user "ann@example.org" {"reason":"removed"}`,
	}
	if got := auditSummary(newAudit(t, dir, n)); !slices.Equal(got, want) {
		t.Fatalf("events:\n%q\nwant\n%q", got, want)
	}
	// A delete of a key that is not stored writes nothing.
	n = len(auditIn(t, dir))
	if err := h.DeleteTailscaleKey(who); err != nil {
		t.Fatal(err)
	}
	if len(auditIn(t, dir)) != n {
		t.Error("a delete of a key that is not stored wrote an event")
	}
}

// `flats key set` and `rm` write the events with the channel command and the operating-system user (rules 4, 5).
func TestKeyCommandsWriteEventsWithTheOperatingSystemUser(t *testing.T) {
	clearKeyEnv(t)
	dir := t.TempDir()
	config := initConfig(t, dir)
	run := func(args ...string) {
		t.Helper()
		var out bytes.Buffer
		if err := KeyCommand(context.Background(), append(args, "--config", config), strings.NewReader(keyA), &out); err != nil {
			t.Fatalf("key %v: %v\n%s", args, err, out.String())
		}
	}
	run("set", "tailscale")
	run("set", "tailscale")
	run("rm", "tailscale")
	me := operatorUser()
	want := []string{
		"key.set tailscale command actor=" + me + ` {"replaced":false}`,
		"key.set tailscale command actor=" + me + ` {"replaced":true}`,
		"key.delete tailscale command actor=" + me + ` {"reason":"removed"}`,
	}
	if got := auditSummary(auditIn(t, dir)); !slices.Equal(got, want) {
		t.Fatalf("events:\n%q\nwant\n%q", got, want)
	}
	if me == "" {
		t.Log("the system gave no user name; the events have no actor")
	}
}

// A start input stores a key once, with the kind of input and the channel start, and writes nothing when a key is stored
// already (rule 10). The import of a legacy key writes the channel migration.
func TestStartAndMigrationInputsWriteEvents(t *testing.T) {
	clearKeyEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	keyFile := core.NewKeyFile(dir, "")
	logf := func(string, ...any) {}
	in := keyInput{value: keyA, source: core.KeySourceEnv, from: "FLATS_TAILSCALE_AUTHKEY"}
	if _, _, err := resolveTailscaleKey(ctx, keyFile, st, in, time.Now(), logf); err != nil {
		t.Fatal(err)
	}
	// The same input again, and a different one, change nothing: the stored key wins.
	for _, again := range []keyInput{in, {value: keyB, source: core.KeySourceFlag, from: "--tailscale-authkey-file"}} {
		if _, _, err := resolveTailscaleKey(ctx, keyFile, st, again, time.Now(), logf); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{`key.set tailscale start {"from":"env","replaced":false}`}
	if got := auditSummary(auditIn(t, dir)); !slices.Equal(got, want) {
		t.Fatalf("events:\n%q\nwant\n%q", got, want)
	}

	// The import of a legacy key file stores the key with the channel migration.
	legacyDir := t.TempDir()
	lst, err := store.Open(filepath.Join(legacyDir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer lst.Close()
	file := filepath.Join(t.TempDir(), "authkey")
	if err := os.WriteFile(file, []byte(keyA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := &legacyPlan{keyFile: file, keyFlag: "authkey-file"}
	if err := importLegacyKey(ctx, legacyDir, lst, plan); err != nil {
		t.Fatal(err)
	}
	if err := importLegacyKey(ctx, legacyDir, lst, plan); err != nil { // repeating it changes nothing
		t.Fatal(err)
	}
	want = []string{`key.set tailscale migration {"from":"flag","replaced":false}`}
	if got := auditSummary(auditIn(t, legacyDir)); !slices.Equal(got, want) {
		t.Fatalf("legacy import events:\n%q\nwant\n%q", got, want)
	}
}

// fakeZrok replaces the calls that the zrok account path makes. connected says what Describe reports and is set by
// Connect, as a real account is.
type fakeZrok struct{ connected bool }

func useFakeZrok(t *testing.T, connected bool) *fakeZrok {
	t.Helper()
	f := &fakeZrok{connected: connected}
	d, c, r, x := zrokDescribe, zrokConnect, zrokReplace, zrokDisconnect
	t.Cleanup(func() { zrokDescribe, zrokConnect, zrokReplace, zrokDisconnect = d, c, r, x })
	zrokDescribe = func(string) (zrok.Account, error) { return zrok.Account{Connected: f.connected}, nil }
	zrokConnect = func(context.Context, string, string, string) error { f.connected = true; return nil }
	zrokReplace = func(context.Context, string, string, string) error { return nil }
	zrokDisconnect = func(context.Context, string, bool) error { f.connected = false; return nil }
	return f
}

// The consumption of a waiting zrok token writes key.delete with the reason connected (rule 11), from the enrolment
// at start (channel start) and from the console (channel console), and from the clean-up of a token that an account
// made unnecessary. A failure of the audit write keeps the token, and the next start removes it with its event (rule 16).
func TestZrokTokenConsumptionWritesEvents(t *testing.T) {
	clearKeyEnv(t)
	ctx := context.Background()
	f := useFakeZrok(t, false) // before the host starts, so that its own enrolment uses the fake too
	dir := t.TempDir()
	config := initConfig(t, dir)

	// 1. A waiting token that `flats key set zrok` stored is consumed by the enrolment that the host runs after its start
	// (channel start).
	var out bytes.Buffer
	if err := KeyCommand(ctx, []string{"set", "zrok", "--config", config}, strings.NewReader("zrok-token-value\n"), &out); err != nil {
		t.Fatalf("key set zrok: %v\n%s", err, out.String())
	}
	h, err := Start(ctx, configModeOptions(config))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	deadline := time.Now().Add(20 * time.Second)
	for len(auditIn(t, dir)) < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	me := operatorUser()
	want := []string{
		"key.set zrok command actor=" + me + ` {"replaced":false}`,
		`key.delete zrok start {"reason":"connected"}`,
	}
	if got := auditSummary(auditIn(t, dir)); !slices.Equal(got, want) {
		t.Fatalf("events after the start:\n%q\nwant\n%q", got, want)
	}
	kf := h.keyFile()
	putToken := func(source string) {
		t.Helper()
		if err := core.PutHostKeyValue(ctx, kf, h.Store, core.HostKeyZrok, "zrok-token-value", source, time.Now(), commandAudit()); err != nil {
			t.Fatal(err)
		}
	}
	hasValue := func() bool {
		t.Helper()
		k, err := h.Store.GetHostKey(ctx, core.HostKeyZrok)
		return err == nil && k.HasValue()
	}
	if !f.connected || hasValue() {
		t.Fatalf("connected = %v, value stored = %v; want the account connected and the token gone", f.connected, hasValue())
	}

	// 2. The console connects with a token that is stored: the stored token is replaced (channel console).
	f.connected = false
	putToken(core.KeySourceCLI)
	n := len(auditIn(t, dir))
	if err := h.connectZrok(core.WithAuditActor(ctx, "the console"), "typed-token", core.KeySourceConsole, true); err != nil {
		t.Log("the zrok backend did not start after the connection, as expected without an account:", err)
	}
	if hasValue() {
		t.Fatal("the stored token is still there after the console connected")
	}
	if got, want := auditSummary(newAudit(t, dir, n)), []string{`key.delete zrok console actor=the console {"reason":"connected"}`}; !slices.Equal(got, want) {
		t.Fatalf("console connect events: %q, want %q", got, want)
	}

	// 3. A token that a person types and that is never stored writes nothing: the record has no value before or after.
	f.connected = false
	n = len(auditIn(t, dir))
	_ = h.connectZrok(ctx, "typed-token", core.KeySourceConsole, true)
	if len(auditIn(t, dir)) != n {
		t.Error("connecting with a typed token wrote an event")
	}

	// 4. An account that is connected already makes a waiting token unnecessary: the clean-up removes it (channel start).
	f.connected = true
	putToken(core.KeySourceCLI)
	n = len(auditIn(t, dir))
	h.enrollWaiting(ctx, keyInputs{})
	if got, want := auditSummary(newAudit(t, dir, n)), []string{`key.delete zrok start {"reason":"connected"}`}; !slices.Equal(got, want) {
		t.Fatalf("clean-up events: %q, want %q", got, want)
	}

	// 5. With the audit write failing, the consumption keeps the token; the next start removes it with its event.
	f.connected = false
	putToken(core.KeySourceCLI)
	n = len(auditIn(t, dir))
	restore := breakAudit(t, dir)
	h.enrollWaiting(ctx, keyInputs{})
	if !hasValue() {
		t.Fatal("the token was removed although its event could not be written")
	}
	if !f.connected {
		t.Fatal("the account was not connected: the connection is outside flats.db and is not undone")
	}
	restore()
	if len(auditIn(t, dir)) != n {
		t.Fatal("an event was written while the audit write failed")
	}
	h.enrollWaiting(ctx, keyInputs{}) // the next start: the account is connected, so the clean-up removes the token
	if hasValue() {
		t.Error("the next start did not remove the token")
	}
	if got, want := auditSummary(newAudit(t, dir, n)), []string{`key.delete zrok start {"reason":"connected"}`}; !slices.Equal(got, want) {
		t.Errorf("retry events: %q, want %q", got, want)
	}
}

// A disconnect that removes a record with a value writes key.delete with the reason removed, and one that removes a record
// without a value writes nothing (rule 11).
func TestZrokDisconnectEvents(t *testing.T) {
	clearKeyEnv(t)
	ctx := context.Background()
	useFakeZrok(t, true)
	dir := t.TempDir()
	h, _ := consoleHostWith(t, dir, nil)
	// A record that only notes the connection.
	if err := h.Store.PutHostKey(ctx, store.HostKey{ID: core.HostKeyZrok, Source: core.KeySourceConsole, UpdatedAt: time.Now()}, store.Audit{}); err != nil {
		t.Fatal(err)
	}
	n := len(auditIn(t, dir))
	if err := h.DisconnectZrok(core.WithAuditActor(ctx, "ann"), false); err != nil {
		t.Fatal(err)
	}
	if len(auditIn(t, dir)) != n {
		t.Error("removing a record without a value wrote an event")
	}
	// A record that holds a value, which a failed clean-up can leave.
	if err := core.PutHostKeyValue(ctx, h.keyFile(), h.Store, core.HostKeyZrok, "zrok-token-value", core.KeySourceCLI, time.Now(), commandAudit()); err != nil {
		t.Fatal(err)
	}
	n = len(auditIn(t, dir))
	if err := h.DisconnectZrok(core.WithAuditActor(ctx, "ann"), false); err != nil {
		t.Fatal(err)
	}
	if got, want := auditSummary(newAudit(t, dir, n)), []string{`key.delete zrok console actor=ann {"reason":"removed"}`}; !slices.Equal(got, want) {
		t.Fatalf("events: %q, want %q", got, want)
	}
}

// `flats secret reset` writes one event for each value it deletes and one key.reset, in one transaction (rule 14).
func TestResetWritesAuditEvents(t *testing.T) {
	dir := t.TempDir()
	config, _, h := hostWithSecrets(t, dir)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, core.SecretKeyFile)); err != nil {
		t.Fatal(err)
	}
	n := len(auditIn(t, dir))
	mustRunSecretKey(t, config, "reset", "--yes")
	me := operatorUser()
	var kinds []string
	for _, e := range newAudit(t, dir, n) {
		if e.Via != "command" || e.Actor != me {
			t.Errorf("event %+v: want via command and the operating-system user", e)
		}
		kinds = append(kinds, e.Kind+" "+e.Flat+"/"+e.Name)
	}
	if want := []string{"secret.delete app/API_KEY", "key.delete /zrok", "key.reset /"}; !slices.Equal(kinds, want) {
		t.Fatalf("events = %q, want %q", kinds, want)
	}
	last := newAudit(t, dir, n)[2]
	var m map[string]any
	if err := json.Unmarshal(last.Data, &m); err != nil || m["secrets"] != float64(1) || m["keys"] != float64(1) {
		t.Errorf("key.reset data = %s", last.Data)
	}
}

// backup-key appends key.backup beside the running host, with the key ID and whether --out wrote a second copy (rules 12, 18).
func TestBackupKeyAppendsItsEventBesideTheHost(t *testing.T) {
	dir := t.TempDir()
	config, key, h := hostWithSecrets(t, dir)
	defer h.Close()
	pw := passwordFile(t, recoveryPassword)
	n := len(auditIn(t, dir))
	mustRunSecretKey(t, config, "backup-key", "--password-file", pw)
	out := filepath.Join(t.TempDir(), "kept.recovery")
	mustRunSecretKey(t, config, "backup-key", "--password-file", pw, "--out", out)
	// The host writes while the commands run beside it, and a host write still succeeds afterwards.
	if err := h.Svc.SetSecret(context.Background(), "app", "NEXT", "v", core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	got := newAudit(t, dir, n)
	if len(got) != 3 {
		t.Fatalf("%d events, want 3: %v", len(got), auditSummary(got))
	}
	id := core.KeyID(key)
	for i, wantOut := range []bool{false, true} {
		e := got[i]
		var m map[string]any
		if e.Kind != "key.backup" || e.Via != "command" || json.Unmarshal(e.Data, &m) != nil || m["key_id"] != id || m["out"] != wantOut {
			t.Errorf("event %d = %+v data %s; want key.backup with key_id %s and out %v", i, e, e.Data, id, wantOut)
		}
		if strings.Contains(string(e.Data), "recovery") {
			t.Errorf("the event holds a path: %s", e.Data)
		}
	}
	if got[2].Kind != "secret.set" {
		t.Errorf("the host's own write: %+v", got[2])
	}
}

// When the event cannot be recorded, backup-key keeps the copy and says so (rule 17): here the schema version is another one.
func TestBackupKeyKeepsTheCopyWhenTheEventCannotBeRecorded(t *testing.T) {
	dir := t.TempDir()
	config, _, h := hostWithSecrets(t, dir)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "flats.db")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 24`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = runSecretKey(t, config, "backup-key", "--password-file", passwordFile(t, recoveryPassword))
	if err == nil || exitCode(err) != 1 || !strings.Contains(err.Error(), "was written") || !strings.Contains(err.Error(), "was not recorded") {
		t.Fatalf("err = %v (status %d), want status 1 and a message that the copy was written and the event was not recorded", err, exitCode(err))
	}
	if _, err := os.Stat(filepath.Join(dir, core.RecoveryKeyFile)); err != nil {
		t.Errorf("the copy is not kept: %v", err)
	}
	// The command did not migrate the database.
	if info, err := store.Inspect(filepath.Join(dir, "flats.db")); err != nil || info.Version != 24 {
		t.Errorf("after the refusal: %+v, %v", info, err)
	}
	// A missing database is not created either.
	empty := t.TempDir()
	if err := store.AppendAuditBeside(context.Background(), filepath.Join(empty, "flats.db"), store.AuditEvent{Kind: store.AuditKeyBackup}); err == nil {
		t.Error("AppendAuditBeside created a database")
	}
	if ents, _ := os.ReadDir(empty); len(ents) != 0 {
		t.Errorf("created %d files", len(ents))
	}
}

// restore-key writes key.restore with the numbers that it checked, with and without --out (rule 13).
func TestRestoreKeyWritesItsEvent(t *testing.T) {
	dir := t.TempDir()
	config, key, h := hostWithSecrets(t, dir)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	pw := passwordFile(t, recoveryPassword)
	mustRunSecretKey(t, config, "backup-key", "--password-file", pw)
	if err := os.Remove(filepath.Join(dir, core.SecretKeyFile)); err != nil {
		t.Fatal(err)
	}
	n := len(auditIn(t, dir))
	out := filepath.Join(t.TempDir(), "recovered.key")
	mustRunSecretKey(t, config, "restore-key", "--password-file", pw, "--out", out)
	mustRunSecretKey(t, config, "restore-key", "--password-file", pw)
	got := newAudit(t, dir, n)
	if len(got) != 2 {
		t.Fatalf("%d events, want 2: %v", len(got), auditSummary(got))
	}
	for i, wantOut := range []bool{true, false} {
		var m map[string]any
		e := got[i]
		if e.Kind != "key.restore" || e.Via != "command" || e.Actor != operatorUser() || json.Unmarshal(e.Data, &m) != nil ||
			m["key_id"] != core.KeyID(key) || m["out"] != wantOut || m["opened"] != float64(2) || m["total"] != float64(2) {
			t.Errorf("event %d = %+v data %s", i, e, e.Data)
		}
	}
}
