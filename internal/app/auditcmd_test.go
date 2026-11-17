package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/ostest"
	"github.com/gosuda/flats/internal/store"
)

// docs/spec/audit-events.md, rules 20 to 23: `flats audit ls`.

func runAudit(t *testing.T, config string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := AuditCommand(context.Background(), append([]string{"ls"}, append(args, "--config", config)...), &out)
	return out.String(), err
}

// auditHost starts a host with events of every kind that a listing can filter on and returns it, with its config.
func auditHost(t *testing.T) (dir, config string, h *Host) {
	t.Helper()
	dir = t.TempDir()
	config, _, h = hostWithSecrets(t, dir)
	t.Cleanup(func() { h.Close() })
	return dir, config, h
}

func jsonLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		rows = append(rows, m)
	}
	return rows
}

func TestAuditLsListsTheLogAsTextAndAsJSONLines(t *testing.T) {
	dir, config, h := auditHost(t)
	ctx := core.WithAuditActor(context.Background(), `tailnet user "ann@example.org"`)
	if err := h.Svc.SetSecret(ctx, "app", "TOKEN", "v", core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	flat, err := h.Store.GetFlat(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	out, err := runAudit(t, config)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 { // the CLI set of hostWithSecrets, the zrok key of hostWithSecrets, the console set
		t.Fatalf("%d lines:\n%s", len(lines), out)
	}
	last := lines[2]
	for _, want := range []string{"secret.set", "app/TOKEN", "via=console", `actor="tailnet user \"ann@example.org\""`, "flat_id=" + flat.ID, `data={"created":true}`} {
		if !strings.Contains(last, want) {
			t.Errorf("the line lacks %q: %s", want, last)
		}
	}
	if !strings.Contains(lines[1], "key.set") || !strings.Contains(lines[1], " zrok ") && !strings.Contains(lines[1], "zrok") {
		t.Errorf("the key line = %s", lines[1])
	}
	jout, err := runAudit(t, config, "--json")
	if err != nil {
		t.Fatal(err)
	}
	rows := jsonLines(t, jout)
	if len(rows) != 3 {
		t.Fatalf("%d JSON lines", len(rows))
	}
	r := rows[2]
	if r["kind"] != "secret.set" || r["flat"] != "app" || r["flat_id"] != flat.ID || r["name"] != "TOKEN" || r["via"] != "console" ||
		r["actor"] != `tailnet user "ann@example.org"` || r["id"] == nil || r["data"].(map[string]any)["created"] != true {
		t.Errorf("JSON line = %v", r)
	}
	at, err := time.Parse(time.RFC3339Nano, r["at"].(string))
	if err != nil || at.Location() != time.UTC || time.Since(at) > time.Minute || !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`).MatchString(r["at"].(string)) {
		t.Errorf("at = %v, %v; want RFC 3339 in UTC with exactly three fraction digits", r["at"], err)
	}
	_ = dir
}

func TestAuditLsFilters(t *testing.T) {
	dir, config, h := auditHost(t)
	ctx := context.Background()
	if _, err := h.Svc.CreateFlat(ctx, "second", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ONE", "TWO"} {
		if err := h.Svc.SetSecret(ctx, "second", name, "v", core.ViaCLI); err != nil {
			t.Fatal(err)
		}
	}
	// An event in the past, written directly, for the --since forms.
	old := time.Now().Add(-72 * time.Hour)
	if err := h.Store.AppendAudit(ctx, store.AuditEvent{Time: old, Kind: store.AuditKeyBackup, Via: store.AuditViaCommand}); err != nil {
		t.Fatal(err)
	}
	count := func(args ...string) int {
		t.Helper()
		out, err := runAudit(t, config, append(args, "--json")...)
		if err != nil {
			t.Fatalf("audit ls %v: %v", args, err)
		}
		return len(jsonLines(t, out))
	}
	// All: the CLI set of app, the zrok key, two sets of second and the old backup event.
	if n := count(); n != 5 {
		t.Fatalf("all = %d, want 5", n)
	}
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"--kind", "secret.set"}, 3},
		{[]string{"--kind", "key.backup"}, 1},
		{[]string{"--kind", "no.such.kind"}, 0},
		{[]string{"--flat", "second"}, 2},
		{[]string{"--flat", "app"}, 1},
		{[]string{"--since", "24h"}, 4},
		{[]string{"--since", old.Add(-time.Minute).UTC().Format(time.RFC3339)}, 5},
		{[]string{"--since", time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, 0},
		{[]string{"--limit", "2"}, 2},
		{[]string{"--limit", "0"}, 5},
		{[]string{"--kind", "secret.set", "--flat", "second", "--limit", "1"}, 1},
	} {
		if n := count(tc.args...); n != tc.want {
			t.Errorf("%v: %d events, want %d", tc.args, n, tc.want)
		}
	}
	// --limit lists the newest events, oldest of them first.
	out, err := runAudit(t, config, "--limit", "2", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if rows := jsonLines(t, out); len(rows) != 2 || rows[0]["kind"] != "secret.set" || rows[1]["name"] != "TWO" && rows[1]["kind"] != "key.backup" {
		t.Errorf("newest two = %v", rows)
	}
	f, err := h.Store.GetFlat(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	if n := count("--flat-id", f.ID); n != 2 {
		t.Errorf("--flat-id: %d events, want 2", n)
	}
	// The default limit is 100: of 130 events the newest 100 are listed, and --limit 0 lists all.
	for i := range 125 {
		if err := h.Store.AppendAudit(ctx, store.AuditEvent{Kind: store.AuditKeyBackup, Via: store.AuditViaCommand, Name: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(); n != 100 {
		t.Errorf("default limit: %d events, want 100", n)
	}
	if n := count("--limit", "0"); n != 130 {
		t.Errorf("--limit 0: %d events, want 130 (more than one page)", n)
	}
	_ = dir
}

// A renamed flat keeps its history under --flat of the new slug, and the events of a deleted flat are found by its old slug.
func TestAuditLsFollowsARenameAndFindsADeletedFlat(t *testing.T) {
	_, config, h := auditHost(t)
	ctx := context.Background()
	if _, err := h.Svc.CreateFlat(ctx, "before", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := h.Svc.SetSecret(ctx, "before", "ONE", "v", core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Svc.RenameSlug(ctx, "before", "after", core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if err := h.Svc.SetSecret(ctx, "after", "TWO", "v", core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	out, err := runAudit(t, config, "--flat", "after", "--json")
	if err != nil {
		t.Fatal(err)
	}
	rows := jsonLines(t, out)
	if len(rows) != 2 || rows[0]["flat"] != "before" || rows[1]["flat"] != "after" || rows[0]["flat_id"] != rows[1]["flat_id"] {
		t.Fatalf("history across the rename = %v", rows)
	}
	if _, err := h.Svc.Delete(ctx, "after", core.ViaConsole, ""); err != nil {
		t.Fatal(err)
	}
	// No flat has the slug now: the rows that were written under it are listed.
	out, err = runAudit(t, config, "--flat", "after", "--json")
	if err != nil {
		t.Fatal(err)
	}
	rows = jsonLines(t, out)
	var kinds []string
	for _, r := range rows {
		kinds = append(kinds, r["kind"].(string)+" "+r["name"].(string)+" "+r["via"].(string))
	}
	if want := []string{"secret.set TWO cli", "secret.delete ONE flat-delete", "secret.delete TWO flat-delete"}; !slices.Equal(kinds, want) {
		t.Errorf("rows of a deleted flat under its last slug = %q, want %q", kinds, want)
	}
}

func TestAuditLsUsageMistakesAndRefusals(t *testing.T) {
	dir := t.TempDir()
	config := initConfig(t, dir)
	for name, args := range map[string][]string{
		"no subcommand":          nil,
		"another subcommand":     {"export"},
		"a bad --since":          {"ls", "--since", "yesterday"},
		"a negative duration":    {"ls", "--since", "-5h"},
		"a negative --limit":     {"ls", "--limit", "-1"},
		"a --limit that is text": {"ls", "--limit", "many"},
		"both flat flags":        {"ls", "--flat", "a", "--flat-id", "b"},
		"an argument":            {"ls", "extra"},
		"an unknown flag":        {"ls", "--nope"},
	} {
		var out bytes.Buffer
		err := AuditCommand(context.Background(), append(args, "--config", config), &out)
		if err == nil || exitCode(err) != 2 {
			t.Errorf("%s: err = %v (status %d), want a usage mistake with status 2", name, err, exitCode(err))
		}
	}
	if err := os.Remove(filepath.Join(dir, "flats.db")); err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"-wal", "-shm"} {
		os.Remove(filepath.Join(dir, "flats.db"+side))
	}
	if _, err := runAudit(t, config); err == nil || exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "no flats.db") {
		t.Errorf("no database: %v (status %d)", err, exitCode(err))
	}
	if _, err := os.Stat(filepath.Join(dir, "flats.db")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the command created flats.db: %v", err)
	}
}

func TestAuditLsRefusesAnOlderAndANewerDatabase(t *testing.T) {
	dir := t.TempDir()
	config, _, h := hostWithSecrets(t, dir)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	setVersion := func(v int) {
		t.Helper()
		db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "flats.db")))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
			t.Fatal(err)
		}
	}
	setVersion(24)
	_, err := runAudit(t, config)
	if err == nil || exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "older schema") || !strings.Contains(err.Error(), "start the host once") {
		t.Errorf("older: %v (status %d)", err, exitCode(err))
	}
	setVersion(99)
	_, err = runAudit(t, config)
	if err == nil || exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "older than the database") {
		t.Errorf("newer: %v (status %d)", err, exitCode(err))
	}
	setVersion(25)
}

// snapshotDir returns each entry of dir with the bytes of its file, leaving the bytes of the files named in skip empty.
func snapshotDir(t *testing.T, dir string, skip ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if d.IsDir() {
			out[rel+"/"] = ""
			return nil
		}
		if slices.ContainsFunc(skip, func(s string) bool { return strings.HasSuffix(rel, s) }) {
			out[rel] = "(skipped)"
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// For a host that runs, and for one that stopped cleanly, the command writes nothing (rule 22 and its criterion).
func TestAuditLsWritesNothingForARunningAndAStoppedHost(t *testing.T) {
	dir, config, h := auditHost(t)
	before := snapshotDir(t, dir, "flats.db-wal", "flats.db-shm", "flats.lock", "flats.db")
	if _, err := runAudit(t, config); err != nil {
		t.Fatal(err)
	}
	if after := snapshotDir(t, dir, "flats.db-wal", "flats.db-shm", "flats.lock", "flats.db"); !mapsEqual(before, after) {
		t.Errorf("a running host's directory changed:\n%v\n%v", before, after)
	}
	h.Close()
	before = snapshotDir(t, dir)
	if _, err := runAudit(t, config); err != nil {
		t.Fatal(err)
	}
	if after := snapshotDir(t, dir); !mapsEqual(before, after) {
		t.Errorf("a cleanly stopped host's directory changed:\n%v\n%v", before, after)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}

// A stopped host that left a -wal file and no -shm gets an empty -shm and nothing else (rule 22); a folder that cannot be
// written gives status 1.
func TestAuditLsOfAStoppedHostThatLeftAWalFile(t *testing.T) {
	// Build the source while a store holds it open, so that its commits are in the -wal file, and copy both files.
	srcDir := t.TempDir()
	st, err := store.Open(filepath.Join(srcDir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.AppendAudit(context.Background(), store.AuditEvent{Kind: store.AuditKeyBackup, Via: store.AuditViaCommand, Name: "in-the-wal"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(srcDir, "flats.db-wal")); err != nil || fi.Size() == 0 {
		t.Skipf("the store keeps no commits in a -wal file here: %v", err)
	}
	stoppedHost := func() (dir, config string) {
		dir = t.TempDir()
		config = initConfig(t, dir)
		for _, f := range []string{"flats.db", "flats.db-wal", "flats.db-shm"} {
			os.Remove(filepath.Join(dir, f))
		}
		for _, f := range []string{"flats.db", "flats.db-wal"} {
			b, err := os.ReadFile(filepath.Join(srcDir, f))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, f), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir, config
	}
	dir, config := stoppedHost()
	before := snapshotDir(t, dir)
	out, err := runAudit(t, config)
	if err != nil || !strings.Contains(out, "in-the-wal") {
		t.Fatalf("listing: %q, %v; want the event that only the -wal file holds", out, err)
	}
	after := snapshotDir(t, dir, "flats.db-shm")
	if _, ok := after["flats.db-shm"]; !ok {
		t.Error("the open created no -shm index: the test does not show the case of the spec")
	}
	delete(after, "flats.db-shm")
	if !mapsEqual(before, after) {
		t.Errorf("more than an empty -shm changed:\n%v\n%v", before, after)
	}
	// A folder without write permission: the open cannot create the index.
	t.Run("a folder that cannot be written", func(t *testing.T) {
		if ostest.Windows || runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs Unix modes and a user that is not the superuser")
		}
		dir, config := stoppedHost()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o700) })
		if _, err := runAudit(t, config); err == nil || exitCode(err) != 1 {
			t.Errorf("an unwritable folder: %v (status %d), want status 1", err, exitCode(err))
		}
	})
}

// blockingWriter holds the command's first write until it is released, as a reader of a pipe that does not read would.
type blockingWriter struct {
	once    sync.Once
	reached chan struct{}
	release chan struct{}
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	b.once.Do(func() { close(b.reached) })
	<-b.release
	return len(p), nil
}

// A listing whose output is stalled holds no read transaction, so the checkpoints of the host succeed (rule 22).
func TestAuditLsDoesNotBlockTheHostsCheckpointsWhileItsOutputIsStalled(t *testing.T) {
	_, config, h := auditHost(t)
	ctx := context.Background()
	for i := range 40 {
		if err := h.Store.AppendAudit(ctx, store.AuditEvent{Kind: store.AuditKeyBackup, Via: store.AuditViaCommand, Name: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	w := &blockingWriter{reached: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- AuditCommand(ctx, []string{"ls", "--limit", "0", "--config", config}, w)
	}()
	select {
	case <-w.reached:
	case <-time.After(20 * time.Second):
		t.Fatal("the command never wrote")
	}
	synced := make(chan error, 1)
	go func() {
		// A write first, so that the checkpoint has frames to move.
		if err := h.Store.AppendAudit(ctx, store.AuditEvent{Kind: store.AuditKeyBackup, Via: store.AuditViaCommand}); err != nil {
			synced <- err
			return
		}
		synced <- h.Store.Sync(ctx)
	}()
	select {
	case err := <-synced:
		if err != nil {
			t.Errorf("the host's checkpoint failed while the listing was stalled: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Error("the host's checkpoint did not finish while the listing was stalled")
	}
	close(w.release)
	if err := <-done; err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("the command: %v", err)
	}
}

// The listing sees what a host that runs commits while the command is paging through the log (rule 22).
func TestAuditLsSeesACommitThatTheHostWritesBetweenTwoRuns(t *testing.T) {
	_, config, h := auditHost(t)
	first, err := runAudit(t, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Svc.SetSecret(context.Background(), "app", "LATE", "v", core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	second, err := runAudit(t, config)
	if err != nil || strings.Count(second, "\n") != strings.Count(first, "\n")+1 || !strings.Contains(second, "app/LATE") {
		t.Errorf("second listing = %q after %q (err %v); want the commit of the host in the write-ahead log", second, first, err)
	}
}

// Nothing in the agent and console surfaces lists the audit rows (rule 24): the flat's log has none of them, and
// no route with audit in its path exists.
func TestAuditRowsAreNotReachableThroughTheHTTPSurfaces(t *testing.T) {
	clearKeyEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	h, client := consoleHostWith(t, dir, nil)
	if _, err := h.Svc.CreateFlat(ctx, "app", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	// Set a secret through the console route: the audit event names the caller of the request.
	if code := consoleCall(t, h, client, "PUT", "/console/api/flats/app/secrets/API_KEY", strings.NewReader(`{"value":"v"}`), nil); code != 200 {
		t.Fatalf("PUT secret: %d", code)
	}
	evs := auditIn(t, dir)
	if len(evs) != 1 || evs[0].Kind != "secret.set" || evs[0].Via != "console" || evs[0].Actor == "" {
		t.Fatalf("the console route wrote %+v; want secret.set via console with an actor", evs)
	}
	var logs map[string]any
	if code := consoleCall(t, h, client, "GET", "/console/api/flats/app/logs", nil, &logs); code != 200 {
		t.Fatalf("GET logs: %d", code)
	}
	raw, _ := json.Marshal(logs)
	for _, bad := range []string{"secret.set", "secret.delete", "flat_id", "audit"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("the log of the flat holds %q: %s", bad, raw)
		}
	}
	for _, path := range []string{"/api/audit", "/console/api/audit", "/api/audit-events", "/console/api/audit/events", "/api/flats/app/audit", "/console/api/flats/app/audit"} {
		if code := consoleCall(t, h, client, "GET", path, nil, nil); code != 404 {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}
}

// seedAudit inserts n key.backup events, named n0000 and up, with a connection of its own, in one transaction.
func seedAudit(t *testing.T, dir string, n int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "flats.db"))+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if _, err := tx.Exec(`INSERT INTO audit_events(at,kind,name,via) VALUES(?,?,?,?)`, time.Now().UnixMilli(), "key.backup", fmt.Sprintf("n%04d", i), "command"); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// More rows than one page (500): the command lists every one once, in order (rule 22).
func TestAuditLsListsMoreRowsThanOnePageOnceAndInOrder(t *testing.T) {
	dir, config, _ := auditHost(t)
	before := len(auditIn(t, dir))
	seedAudit(t, dir, 1203)
	out, err := runAudit(t, config, "--limit", "0", "--json")
	if err != nil {
		t.Fatal(err)
	}
	rows := jsonLines(t, out)
	if len(rows) != before+1203 {
		t.Fatalf("%d rows, want %d", len(rows), before+1203)
	}
	var last float64
	seen := map[string]bool{}
	for i, r := range rows {
		id := r["id"].(float64)
		if id <= last {
			t.Fatalf("row %d: id %v after %v, want ascending ids", i, id, last)
		}
		last = id
		if name := r["name"].(string); strings.HasPrefix(name, "n0") || strings.HasPrefix(name, "n1") {
			if seen[name] {
				t.Fatalf("%s is listed twice", name)
			}
			seen[name] = true
		}
	}
	if len(seen) != 1203 {
		t.Errorf("%d seeded rows listed, want 1203", len(seen))
	}
	// --limit takes the newest across the page size too.
	out, err = runAudit(t, config, "--limit", "700", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if rows := jsonLines(t, out); len(rows) != 700 || rows[699]["name"] != "n1202" || rows[0]["name"] != "n0503" {
		t.Errorf("newest 700: %d rows, first %v, last %v", len(rows), rows[0]["name"], rows[len(rows)-1]["name"])
	}
}

// stallingWriter holds its first write until it is released, and keeps what it was given.
type stallingWriter struct {
	once    sync.Once
	reached chan struct{}
	release chan struct{}
	mu      sync.Mutex
	buf     bytes.Buffer
}

func (w *stallingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.reached) })
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *stallingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// A row that the host commits while the command waits on its output, between the first page and the next, shows in a later
// page (rule 22 and its criterion).
func TestAuditLsShowsARowCommittedBetweenTwoPages(t *testing.T) {
	dir, config, h := auditHost(t)
	seedAudit(t, dir, 1203)
	w := &stallingWriter{reached: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- AuditCommand(context.Background(), []string{"ls", "--limit", "0", "--json", "--config", config}, w)
	}()
	select {
	case <-w.reached:
	case <-time.After(20 * time.Second):
		t.Fatal("the command never wrote")
	}
	if err := h.Store.AppendAudit(context.Background(), store.AuditEvent{Kind: store.AuditKeyBackup, Via: store.AuditViaCommand, Name: "late"}); err != nil {
		t.Fatal(err)
	}
	close(w.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	rows := jsonLines(t, w.String())
	if len(rows) == 0 {
		t.Fatal("the command listed nothing")
	}
	if last := rows[len(rows)-1]["name"]; last != "late" {
		t.Errorf("the last of %d rows is %v; want the row that the host committed while the command was between pages", len(rows), last)
	}
}

// The reserved kind key.rotate is listed as it is, with its fields, and --kind selects it (rule 15).
func TestAuditLsListsTheReservedKeyRotateKind(t *testing.T) {
	dir, config, h := auditHost(t)
	if err := h.Store.AppendAudit(context.Background(), store.AuditEvent{Kind: store.AuditKeyRotate, Via: store.AuditViaCommand,
		Data: store.AuditData(map[string]any{"from": "aaaa", "to": "bbbb"})}); err != nil {
		t.Fatal(err)
	}
	out, err := runAudit(t, config, "--kind", "key.rotate", "--json")
	if err != nil {
		t.Fatal(err)
	}
	rows := jsonLines(t, out)
	if len(rows) != 1 || rows[0]["kind"] != "key.rotate" || rows[0]["data"].(map[string]any)["to"] != "bbbb" {
		t.Errorf("rows = %v", rows)
	}
	_ = dir
}
