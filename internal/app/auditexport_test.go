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
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gosuda/flats/internal/cli"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/ostest"
	"github.com/gosuda/flats/internal/store"
)

// docs/spec/event-webhook.md, rules 22 to 26: `flats audit export`.
//
// These tests replace the clock and the standard error of the command, which are package variables, so none of them
// runs in parallel. Every row has a fixed time and the command has a fixed clock: no test reads the clock of the machine.

// exportT0 is the time that the fixtures count from.
var exportT0 = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

// exportAt is the time s seconds after exportT0.
func exportAt(s int) time.Time { return exportT0.Add(time.Duration(s) * time.Second) }

// exportLate is a clock that every fixture time is before the cutoff of.
var exportLate = exportT0.Add(24 * time.Hour)

// exportData makes a data directory with `flats config init` and opens its flats.db as a host does.
func exportData(t *testing.T) (dir, config string, st *store.Store) {
	t.Helper()
	dir = t.TempDir()
	config = initConfig(t, dir)
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return dir, config, st
}

// exportFlat creates a flat and returns it with its ID.
func exportFlat(t *testing.T, st *store.Store, slug string) store.Flat {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateFlat(ctx, store.Flat{Slug: slug, Name: slug, Visibility: store.Private, CreatedAt: exportT0, UpdatedAt: exportT0}); err != nil {
		t.Fatal(err)
	}
	f, err := st.GetFlat(ctx, slug)
	if err != nil || f.ID == "" {
		t.Fatalf("GetFlat(%s) = %+v, %v", slug, f, err)
	}
	return f
}

// exportSecret stores a secret of flat, which writes a secret.set audit row with the time at.
func exportSecret(t *testing.T, st *store.Store, flat, name string, at time.Time) {
	t.Helper()
	sec := store.SealedSecret{Name: name, Nonce: bytes.Repeat([]byte{1}, 12), Ciphertext: bytes.Repeat([]byte{2}, 32), UpdatedAt: at}
	if err := st.PutSecret(context.Background(), flat, sec, store.Audit{Time: at, Via: store.AuditViaCLI}); err != nil {
		t.Fatal(err)
	}
}

// exportRequest inserts a pending approval as core does for a request.
func exportRequest(t *testing.T, st *store.Store, id, flat, action, reason string, at time.Time) {
	t.Helper()
	err := st.InsertApproval(context.Background(), store.Approval{ID: id, Flat: flat, Action: action, Params: json.RawMessage(`{"canary":"in-params"}`),
		Status: "pending", Via: "mcp", Reason: reason, RequestedAt: at, IdempotencyKey: "idem-" + id})
	if err != nil {
		t.Fatal(err)
	}
}

// exportDecision claims a pending approval for actor and finishes it, as core does for a decision that is applied.
func exportDecision(t *testing.T, st *store.Store, id, actor, status, result string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := st.ClaimApprovalAuthorized(ctx, id, actor, at); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishApprovalData(ctx, id, status, result, json.RawMessage(`{"canary":"in-result-data"}`), at); err != nil {
		t.Fatal(err)
	}
}

// exportAuditRow appends an audit row with a fixed time.
func exportAuditRow(t *testing.T, st *store.Store, name string, at time.Time) {
	t.Helper()
	if err := st.AppendAudit(context.Background(), store.AuditEvent{Time: at, Kind: store.AuditKeyBackup, Via: store.AuditViaCommand, Name: name}); err != nil {
		t.Fatal(err)
	}
}

// exportSQL runs fn in one transaction on the flats.db of dir, with a connection of its own.
func exportSQL(t *testing.T, dir string, fn func(tx *sql.Tx) error) {
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
	if err := fn(tx); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// exportClock sets the clock and the standard error of the export until the test ends.
func exportClock(t *testing.T, now time.Time, stderr io.Writer) {
	t.Helper()
	oldNow, oldErr := exportNow, exportStderr
	exportNow, exportStderr = func() time.Time { return now }, stderr
	t.Cleanup(func() { exportNow, exportStderr = oldNow, oldErr })
}

// runExport runs `flats audit export` with the clock at now, and returns its standard output, its standard error
// and its error.
func runExport(t *testing.T, config string, now time.Time, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	exportClock(t, now, &errOut)
	err = AuditCommand(context.Background(), append([]string{"export"}, append(args, "--config", config)...), &out)
	return out.String(), errOut.String(), err
}

var (
	exportTimeForm = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)
	exportSummary  = regexp.MustCompile(`^exported (\d+) lines through (\S+)\n$`)
)

// checkExport checks what holds for every export and returns its lines and the time of its summary. The standard
// output is JSON lines and nothing else, each with a record and a time in UTC with three fraction digits, in rising
// order of time. The standard error is the one summary line, with the number of lines and the cutoff of the clock.
func checkExport(t *testing.T, stdout, stderr string, now time.Time) (lines []map[string]any, through string) {
	t.Helper()
	if stdout != "" {
		if !strings.HasSuffix(stdout, "\n") {
			t.Errorf("the standard output does not end with a newline: %q", stdout[max(0, len(stdout)-80):])
		}
		last := ""
		for i, text := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
			var m map[string]any
			if err := json.Unmarshal([]byte(text), &m); err != nil {
				t.Fatalf("line %d of the standard output is not JSON: %q: %v", i+1, text, err)
			}
			if !strings.HasPrefix(text, `{"record":"audit","id":`) && !strings.HasPrefix(text, `{"record":"approval","event":"created","at":`) &&
				!strings.HasPrefix(text, `{"record":"approval","event":"decided","at":`) {
				t.Errorf("line %d starts with neither kind of record: %q", i+1, text)
			}
			at, _ := m["at"].(string)
			if !exportTimeForm.MatchString(at) {
				t.Errorf("line %d: at = %q, want RFC 3339 in UTC with three fraction digits", i+1, at)
			}
			if at < last { // the form is fixed, so the text orders as the time does
				t.Errorf("line %d: at %s comes after %s", i+1, at, last)
			}
			last = at
			lines = append(lines, m)
		}
	}
	m := exportSummary.FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("the standard error is %q, want the one summary line", stderr)
	}
	if m[1] != strconv.Itoa(len(lines)) {
		t.Errorf("the summary counts %s lines and the output has %d", m[1], len(lines))
	}
	if want := now.Add(-30 * time.Second).UTC().Format("2006-01-02T15:04:05.000Z"); m[2] != want {
		t.Errorf("the summary names %s, want the cutoff %s", m[2], want)
	}
	for i, l := range lines {
		if at := l["at"].(string); at > m[2] {
			t.Errorf("line %d: at %s is later than the cutoff %s", i+1, at, m[2])
		}
	}
	return lines, m[2]
}

// mustExport runs an export that must succeed and returns its checked lines and the time of its summary.
func mustExport(t *testing.T, config string, now time.Time, args ...string) ([]map[string]any, string) {
	t.Helper()
	stdout, stderr, err := runExport(t, config, now, args...)
	if err != nil {
		t.Fatalf("audit export %v: %v", args, err)
	}
	return checkExport(t, stdout, stderr, now)
}

// exportKeys names each line: "audit NAME", "created ID" or "decided ID".
func exportKeys(lines []map[string]any) []string {
	var out []string
	for _, l := range lines {
		if l["record"] == "audit" {
			out = append(out, "audit "+l["name"].(string))
		} else {
			out = append(out, l["event"].(string)+" "+l["id"].(string))
		}
	}
	return out
}

func exportStamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// The example of rule 26. An agent asked to delete a flat with two secrets and the operator approved. The export has
// the request and the decision of the approval, with the slug and the ID of the flat that is gone, and the two
// secret.delete rows with the approval, in the order of rule 24.
func TestAuditExportPrintsAnApprovedDeleteAfterTheFlatIsGone(t *testing.T) {
	_, config, st := exportData(t)
	ctx := context.Background()
	flat := exportFlat(t, st, "app")
	exportSecret(t, st, "app", "API_KEY", exportAt(1))
	// The second secret is set in the millisecond of the request, and the flat is deleted in the millisecond of the
	// decision, so the order among lines of one time shows.
	exportSecret(t, st, "app", "TOKEN", exportAt(10))
	exportRequest(t, st, "apr-delete", "app", "delete", "the demo is over", exportAt(10))
	const operator = `tailnet user "ann@example.org"`
	if err := st.ClaimApprovalAuthorized(ctx, "apr-delete", operator, exportAt(20)); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteFlat(ctx, "app", store.Audit{Time: exportAt(22), Via: store.AuditViaFlatDelete, Approval: "apr-delete"}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishApprovalData(ctx, "apr-delete", "approved", "deleted", json.RawMessage(`{"canary":"in-result-data"}`), exportAt(22)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetFlat(ctx, "app"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the flat after its deletion: %v", err)
	}

	stdout, stderr, err := runExport(t, config, exportLate)
	if err != nil {
		t.Fatal(err)
	}
	lines, _ := checkExport(t, stdout, stderr, exportLate)
	want := []string{"audit API_KEY", "audit TOKEN", "created apr-delete", "audit API_KEY", "audit TOKEN", "decided apr-delete"}
	if got := exportKeys(lines); !slices.Equal(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	if want := map[string]any{"record": "approval", "event": "created", "at": exportStamp(exportAt(10)), "id": "apr-delete", "flat": "app", "flat_id": flat.ID,
		"action": "delete", "via": "mcp", "reason": "the demo is over"}; !reflect.DeepEqual(lines[2], want) {
		t.Errorf("the request line = %v, want %v", lines[2], want)
	}
	if want := map[string]any{"record": "approval", "event": "decided", "at": exportStamp(exportAt(22)), "id": "apr-delete", "flat": "app", "flat_id": flat.ID,
		"action": "delete", "state": "approved", "decided_by": operator, "result": "deleted"}; !reflect.DeepEqual(lines[5], want) {
		t.Errorf("the decision line = %v, want %v", lines[5], want)
	}
	for i, name := range []string{"API_KEY", "TOKEN"} {
		l := lines[3+i]
		data, _ := l["data"].(map[string]any)
		if l["kind"] != "secret.delete" || l["name"] != name || l["flat"] != "app" || l["flat_id"] != flat.ID || l["via"] != "flat-delete" ||
			l["at"] != exportStamp(exportAt(22)) || data["approval"] != "apr-delete" {
			t.Errorf("the secret.delete line of %s = %v", name, l)
		}
	}
	// No line has a column that rule 23 leaves out.
	for _, canary := range []string{"in-params", "in-result-data", "idem-apr-delete", "params", "result_data", "authorized_at", "idempotency_key"} {
		if strings.Contains(stdout, canary) {
			t.Errorf("the export holds %q:\n%s", canary, stdout)
		}
	}
	// An audit line is the line of `flats audit ls --json` with the record in front.
	listed, err := runAudit(t, config, "--json", "--limit", "0")
	if err != nil {
		t.Fatal(err)
	}
	var fromExport []map[string]any
	for _, l := range lines {
		if l["record"] == "audit" {
			c := map[string]any{}
			for k, v := range l {
				if k != "record" {
					c[k] = v
				}
			}
			fromExport = append(fromExport, c)
		}
	}
	if fromLs := jsonLines(t, listed); !reflect.DeepEqual(fromExport, fromLs) {
		t.Errorf("the audit lines of the export =\n%v\nand the lines of ls --json =\n%v", fromExport, fromLs)
	}
}

// A delete from the console makes no approval of its own. The export shows it through the secret rows of the flat and
// through the pending approval that the deletion failed (the scope of the spec and its criterion).
func TestAuditExportPrintsTheApprovalThatAConsoleDeleteFailed(t *testing.T) {
	_, config, st := exportData(t)
	ctx := context.Background()
	flat := exportFlat(t, st, "site")
	exportSecret(t, st, "site", "API_KEY", exportAt(1))
	exportSecret(t, st, "site", "TOKEN", exportAt(2))
	exportRequest(t, st, "apr-public", "site", "set_visibility", "share it", exportAt(5))
	// As core does: the flat goes, and then every approval of it that was pending is failed.
	if err := st.DeleteFlat(ctx, "site", store.Audit{Time: exportAt(30)}); err != nil {
		t.Fatal(err)
	}
	if err := st.DecideApproval(ctx, "apr-public", "failed", "the flat was deleted", exportAt(31)); err != nil {
		t.Fatal(err)
	}
	lines, _ := mustExport(t, config, exportLate)
	want := []string{"audit API_KEY", "audit TOKEN", "created apr-public", "audit API_KEY", "audit TOKEN", "decided apr-public"}
	if got := exportKeys(lines); !slices.Equal(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	// No operator decided it, so decided_by is there and empty.
	if want := map[string]any{"record": "approval", "event": "decided", "at": exportStamp(exportAt(31)), "id": "apr-public", "flat": "site", "flat_id": flat.ID,
		"action": "set_visibility", "state": "failed", "decided_by": "", "result": "the flat was deleted"}; !reflect.DeepEqual(lines[5], want) {
		t.Errorf("the decision line = %v, want %v", lines[5], want)
	}
	for _, l := range lines[3:5] {
		if _, has := l["data"]; l["kind"] != "secret.delete" || l["via"] != "flat-delete" || l["flat_id"] != flat.ID || has {
			t.Errorf("a secret.delete line of a delete without an approval = %v", l)
		}
	}
}

// --since takes a time or a duration, cuts by the time of each line and keeps a line of exactly that time (rule 22).
func TestAuditExportSinceCutsByTheTimeOfALineAndKeepsTheBoundary(t *testing.T) {
	_, config, st := exportData(t)
	ctx := context.Background()
	boundary := exportAt(60)
	before := boundary.Add(-time.Millisecond)
	exportAuditRow(t, st, "a-before", before)
	exportAuditRow(t, st, "a-boundary", boundary)
	exportAuditRow(t, st, "a-after", exportAt(61))
	exportRequest(t, st, "p-before", "app", "delete", "", before)
	exportRequest(t, st, "p-boundary", "app", "delete", "", boundary)
	// The time of a decision counts for its line, and the time of the request for the request.
	for id, decided := range map[string]time.Time{"d-before": before, "d-boundary": boundary} {
		exportRequest(t, st, id, "app", "delete", "", exportAt(10))
		if err := st.DecideApproval(ctx, id, "rejected", "no", decided); err != nil {
			t.Fatal(err)
		}
	}
	exportRequest(t, st, "d-later", "app", "delete", "", boundary)
	if err := st.DecideApproval(ctx, "d-later", "rejected", "no", exportAt(3000)); err != nil {
		t.Fatal(err)
	}

	all := []string{"created d-before", "created d-boundary", "audit a-before", "created p-before", "decided d-before",
		"audit a-boundary", "created d-later", "created p-boundary", "decided d-boundary", "audit a-after", "decided d-later"}
	fromBoundary := all[5:]
	afterBoundary := all[9:]
	// The clock is one hour after exportT0, so 59m ago is the boundary.
	now := exportAt(3600)
	for name, tc := range map[string]struct {
		since string
		want  []string
	}{
		"no --since":                           {"", all},
		"the time of the boundary":             {"2026-11-16T09:01:00Z", fromBoundary},
		"the same time in another zone":        {"2026-11-16T18:01:00+09:00", fromBoundary},
		"the boundary with fraction digits":    {"2026-11-16T09:01:00.000Z", fromBoundary},
		"one millisecond before the boundary":  {"2026-11-16T09:00:59.999Z", all[2:]},
		"one millisecond after the boundary":   {"2026-11-16T09:01:00.001Z", afterBoundary},
		"a duration that ends at the boundary": {"59m", fromBoundary},
		"a duration one millisecond shorter":   {"58m59.999s", afterBoundary},
		"a duration of the whole log":          {"24h", all},
		"a time after every line":              {"2026-11-16T09:50:00.001Z", nil},
		"a time after the cutoff":              {"2026-11-16T12:00:00Z", nil},
	} {
		var args []string
		if tc.since != "" {
			args = []string{"--since", tc.since}
		}
		lines, _ := mustExport(t, config, now, args...)
		if got := exportKeys(lines); !slices.Equal(got, tc.want) {
			t.Errorf("%s: lines = %q, want %q", name, got, tc.want)
		}
	}
}

// Lines of one time come as rule 24 says: the audit rows by id, then the requests by approval id, then the decisions
// by approval id. A row with an earlier time comes first whatever its id is.
func TestAuditExportOrdersLinesOfOneTime(t *testing.T) {
	dir, config, st := exportData(t)
	ctx := context.Background()
	x := exportAt(100)
	exportAuditRow(t, st, "n1", x)
	exportAuditRow(t, st, "n2", x)
	exportAuditRow(t, st, "later", x.Add(time.Millisecond))
	exportAuditRow(t, st, "n3", x)
	// This row has the largest id and the earliest time.
	exportAuditRow(t, st, "earlier", x.Add(-time.Millisecond))
	// The ids of the approvals are not in the order of their insert.
	for _, id := range []string{"b", "q", "c", "a", "n"} {
		exportRequest(t, st, id, "app", "delete", "", x)
	}
	exportRequest(t, st, "z", "app", "delete", "", x.Add(-time.Second))
	exportRequest(t, st, "y", "app", "delete", "", x.Add(-time.Second))
	for _, id := range []string{"z", "c", "a"} {
		if err := st.DecideApproval(ctx, id, "rejected", "no", x); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DecideApproval(ctx, "y", "rejected", "no", x.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	// An approval that is being applied has no decision yet: b stays pending, and q is claimed and not finished.
	if err := st.ClaimApprovalAuthorized(ctx, "q", "console", x); err != nil {
		t.Fatal(err)
	}
	// A final status that was written without a time of decision, as the adoption of a legacy draft writes it: its
	// decision line uses the time of the request (rule 23), which is x.
	exportSQL(t, dir, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE approvals SET status='failed', result='legacy' WHERE id='n'`)
		return err
	})
	lines, _ := mustExport(t, config, exportLate)
	want := []string{
		"created y", "created z",
		"audit earlier",
		"audit n1", "audit n2", "audit n3",
		"created a", "created b", "created c", "created n", "created q",
		"decided a", "decided c", "decided n", "decided z",
		"audit later", "decided y",
	}
	got := exportKeys(lines)
	if !slices.Equal(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	if l := lines[slices.Index(got, "decided n")]; l["at"] != exportStamp(x) || l["state"] != "failed" || l["result"] != "legacy" {
		t.Errorf("the decision without a time = %v, want it at the time of its request", l)
	}
}

// exportBulk seeds audits audit rows and approvals approvals in one transaction. Many rows share a millisecond, the
// times do not rise with the ids, and the ids of the approvals do not rise with their insert. One approval in three
// is pending, one is approved with a time of decision, and one is failed without a time of decision. It returns the
// keys of the lines in the order of rule 24, worked out here and not by the command.
func exportBulk(t *testing.T, dir string, audits, approvals int) []string {
	t.Helper()
	type line struct {
		at   int64
		rank int // 0 audit, 1 created, 2 decided
		id   string
		key  string
	}
	var want []line
	base := exportT0.UnixMilli()
	exportSQL(t, dir, func(tx *sql.Tx) error {
		for i := range audits {
			at := base + int64((i*37)%171)
			name := fmt.Sprintf("n%05d", i)
			if _, err := tx.Exec(`INSERT INTO audit_events(at,kind,name,via) VALUES(?,?,?,?)`, at, "key.backup", name, "command"); err != nil {
				return err
			}
			// The table was empty, so the ids rise with the insert, and the name orders as the id does.
			want = append(want, line{at, 0, name, "audit " + name})
		}
		for j := range approvals {
			id := fmt.Sprintf("apr-%05d", (j*7919)%approvals)
			requested := base + int64((j*11)%86)
			status, decided := "pending", any(nil)
			switch j % 3 {
			case 1:
				status = "approved"
				at := base + 40 + int64((j*13)%97)
				decided = at
				want = append(want, line{at, 2, id, "decided " + id})
			case 2:
				status = "failed"
				want = append(want, line{requested, 2, id, "decided " + id})
			}
			if _, err := tx.Exec(`INSERT INTO approvals(id,flat,flat_id,action,params,status,via,reason,result,requested_at,decided_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
				id, "app", "flat-id", "delete", "{}", status, "mcp", "reason", "result", requested, decided); err != nil {
				return err
			}
			want = append(want, line{requested, 1, id, "created " + id})
		}
		return nil
	})
	sort.Slice(want, func(a, b int) bool {
		x, y := want[a], want[b]
		if x.at != y.at {
			return x.at < y.at
		}
		if x.rank != y.rank {
			return x.rank < y.rank
		}
		return x.id < y.id
	})
	keys := make([]string, len(want))
	for i, l := range want {
		keys[i] = l.key
	}
	return keys
}

// exportSplitStreams lists the streams of an export in which a page of auditPage rows ends inside a group of lines of
// one time: the last line of the page and the line after it have the same time. A test of the paging needs such a
// group, or a read that skipped the rest of it would go unseen.
func exportSplitStreams(lines []map[string]any) []string {
	times := map[string][]string{}
	for _, l := range lines {
		stream := "audit"
		if l["record"] != "audit" {
			stream = l["event"].(string)
		}
		times[stream] = append(times[stream], l["at"].(string))
	}
	var out []string
	for _, stream := range []string{"audit", "created", "decided"} {
		for end := auditPage; end < len(times[stream]); end += auditPage {
			if times[stream][end-1] == times[stream][end] {
				out = append(out, stream)
				break
			}
		}
	}
	return out
}

// A log of 1200 audit rows and 600 approvals, more than two pages of each of the first two streams, is printed in
// full and in order: every line once, none lost (rule 24 and its criterion).
func TestAuditExportPrintsALogOfManyPagesInFullAndInOrder(t *testing.T) {
	dir, config, _ := exportData(t)
	want := exportBulk(t, dir, 1200, 600)
	if len(want) != 1200+600+400 {
		t.Fatalf("the fixture has %d lines, want 2200", len(want))
	}
	lines, _ := mustExport(t, config, exportLate)
	got := exportKeys(lines)
	if !slices.Equal(got, want) {
		for i := range min(len(got), len(want)) {
			if got[i] != want[i] {
				t.Fatalf("%d lines, want %d; the first difference is line %d: %q, want %q", len(got), len(want), i+1, got[i], want[i])
			}
		}
		t.Fatalf("%d lines, want %d", len(got), len(want))
	}
	if split := exportSplitStreams(lines); !slices.Equal(split, []string{"audit", "created"}) {
		t.Fatalf("a page ends inside a group of one time only in the streams %q: the fixture does not test the page key", split)
	}
	// The ids of the audit rows rise with their names, so the fixture stands for the rows that were stored.
	for _, e := range auditIn(t, dir) {
		if want := fmt.Sprintf("n%05d", e.ID-1); e.Name != want {
			t.Fatalf("audit row %d is %s, want %s: the fixture does not order as the ids do", e.ID, e.Name, want)
		}
	}
	// A page boundary in the middle of an export with --since.
	since := exportT0.Add(60 * time.Millisecond)
	var wantSince []string
	for _, l := range lines {
		if l["at"].(string) >= exportStamp(since) {
			wantSince = append(wantSince, exportKeys([]map[string]any{l})[0])
		}
	}
	linesSince, _ := mustExport(t, config, exportLate, "--since", exportStamp(since))
	if got := exportKeys(linesSince); !slices.Equal(got, wantSince) || len(got) == 0 || len(got) == len(want) {
		t.Errorf("--since in the middle: %d lines, want %d of %d", len(got), len(wantSince), len(want))
	}
}

// A stream whose rows fill its last page exactly, and one with a row less or more, ends without a lost or a repeated
// line. Two approvals in three have a decision, so 750 approvals are one full page of decisions, and 1600 are more
// than two pages of them.
func TestAuditExportEndsAStreamAtAPageBoundary(t *testing.T) {
	for _, n := range []struct {
		audits, approvals int
		split             []string // the streams that must have a page that ends inside a group of one time
	}{
		{auditPage - 1, auditPage - 1, nil},
		{auditPage, auditPage, nil},
		{auditPage + 1, auditPage + 1, nil},
		{2 * auditPage, auditPage * 3 / 2, nil},
		{2*auditPage + 1, 1600, []string{"audit", "created", "decided"}},
	} {
		dir, config, _ := exportData(t)
		want := exportBulk(t, dir, n.audits, n.approvals)
		lines, _ := mustExport(t, config, exportLate)
		if got := exportKeys(lines); !slices.Equal(got, want) {
			t.Errorf("%d audit rows and %d approvals: %d lines, want %d in the order of the fixture", n.audits, n.approvals, len(got), len(want))
		}
		if split := exportSplitStreams(lines); n.split != nil && !slices.Equal(split, n.split) {
			t.Errorf("%d audit rows and %d approvals: a page ends inside a group of one time only in the streams %q", n.audits, n.approvals, split)
		}
	}
}

// An export whose output is stalled between two pages holds no read transaction: the host writes and checkpoints
// meanwhile, and the page that the command reads after the stall has the row that the host wrote (rule 22).
func TestAuditExportHoldsNoTransactionWhileItsOutputIsStalled(t *testing.T) {
	dir, config, st := exportData(t)
	ctx := context.Background()
	want := exportBulk(t, dir, 1203, 600)
	now := exportLate
	cutoff := now.Add(-30 * time.Second)

	w := &stallingWriter{reached: make(chan struct{}), release: make(chan struct{})}
	var errOut bytes.Buffer
	exportClock(t, now, &errOut)
	var cmdErr error
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		cmdErr = AuditCommand(ctx, []string{"export", "--config", config}, w)
	}()
	release := sync.OnceFunc(func() { close(w.release) })
	// The command ends before the clock and the standard error are put back, also when the test fails early.
	t.Cleanup(func() {
		release()
		select {
		case <-finished:
		case <-time.After(30 * time.Second):
			t.Error("the command did not end")
		}
	})
	// The first write of the command comes while it prints its first pages: the buffer of the output is far smaller
	// than a page, and each stream has more than one page.
	select {
	case <-w.reached:
	case <-time.After(20 * time.Second):
		t.Fatal("the command never wrote")
	}
	// The host writes a row that belongs at the end of the export, and checkpoints. The write makes the state that the
	// command read an older one, which a checkpoint cannot pass while a read transaction holds it.
	synced := make(chan error, 1)
	go func() {
		if err := st.AppendAudit(ctx, store.AuditEvent{Time: cutoff, Kind: store.AuditKeyBackup, Via: store.AuditViaCommand, Name: "late"}); err != nil {
			synced <- err
			return
		}
		synced <- st.Sync(ctx)
	}()
	select {
	case err := <-synced:
		if err != nil {
			t.Errorf("the host's write and checkpoint while the export was stalled: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Error("the host's write and checkpoint did not finish while the export was stalled")
	}
	release()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("the command did not end after its output was released")
	}
	if cmdErr != nil {
		t.Fatal(cmdErr)
	}
	lines, _ := checkExport(t, w.String(), errOut.String(), now)
	if got := exportKeys(lines); !slices.Equal(got, append(want, "audit late")) {
		t.Errorf("%d lines, the last is %q; want the %d lines of the fixture and then the row that the host wrote during the stall",
			len(got), got[len(got)-1], len(want))
	}
}

// The command stops at its cutoff, 30 seconds behind its clock, and the next export continues from the time that the
// summary names. A line at the cutoff is in both, and a line after it only in the next (rule 25).
func TestAuditExportStopsAtItsCutoffAndTheNextExportContinues(t *testing.T) {
	_, config, st := exportData(t)
	ctx := context.Background()
	now := exportAt(1000)
	cutoff := now.Add(-30 * time.Second)
	after := cutoff.Add(time.Millisecond)
	exportAuditRow(t, st, "a-before", cutoff.Add(-time.Millisecond))
	exportAuditRow(t, st, "a-cutoff", cutoff)
	exportAuditRow(t, st, "a-after", after)
	exportRequest(t, st, "p-cutoff", "app", "delete", "", cutoff)
	exportRequest(t, st, "p-after", "app", "delete", "", after)
	// Both were requested long before. One is decided at the cutoff and one a millisecond later.
	for id, decided := range map[string]time.Time{"d-cutoff": cutoff, "d-after": after} {
		exportRequest(t, st, id, "app", "delete", "", exportAt(1))
		if err := st.DecideApproval(ctx, id, "rejected", "no", decided); err != nil {
			t.Fatal(err)
		}
	}

	stdout, stderr, err := runExport(t, config, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := "exported 6 lines through " + exportStamp(cutoff) + "\n"; stderr != want || exportStamp(cutoff) != "2026-11-16T09:16:10.000Z" {
		t.Errorf("the standard error = %q, want %q", stderr, want)
	}
	first, through := checkExport(t, stdout, stderr, now)
	wantFirst := []string{"created d-after", "created d-cutoff", "audit a-before", "audit a-cutoff", "created p-cutoff", "decided d-cutoff"}
	if got := exportKeys(first); !slices.Equal(got, wantFirst) {
		t.Fatalf("the first export = %q, want %q", got, wantFirst)
	}

	// The next export starts at the printed time, a minute later on the clock.
	second, _ := mustExport(t, config, now.Add(time.Minute), "--since", through)
	wantSecond := []string{"audit a-cutoff", "created p-cutoff", "decided d-cutoff", "audit a-after", "created p-after", "decided d-after"}
	if got := exportKeys(second); !slices.Equal(got, wantSecond) {
		t.Fatalf("the next export = %q, want %q", got, wantSecond)
	}
	// Together they hold every line, and a reader that drops the repeated boundary lines has each once.
	seen := map[string]int{}
	for _, k := range append(exportKeys(first), exportKeys(second)...) {
		seen[k]++
	}
	if want := map[string]int{
		"created d-after": 1, "created d-cutoff": 1, "audit a-before": 1,
		"audit a-cutoff": 2, "created p-cutoff": 2, "decided d-cutoff": 2,
		"audit a-after": 1, "created p-after": 1, "decided d-after": 1,
	}; !reflect.DeepEqual(seen, want) {
		t.Errorf("how often each line is in the two exports = %v, want %v", seen, want)
	}

	// A clock in the sub-millisecond part of a millisecond gives the cutoff of that millisecond.
	lines, _ := mustExport(t, config, now.Add(999*time.Microsecond))
	if got := exportKeys(lines); !slices.Equal(got, wantFirst) {
		t.Errorf("with a clock inside the millisecond: %q, want %q", got, wantFirst)
	}
	// An empty span prints nothing and still names its cutoff.
	stdout, stderr, err = runExport(t, config, exportT0)
	if err != nil || stdout != "" || stderr != "exported 0 lines through 2026-11-16T08:59:30.000Z\n" {
		t.Errorf("before every row: stdout %q, stderr %q, %v", stdout, stderr, err)
	}
}

// A usage mistake is status 2, and a data directory without a database, or with one of another schema, is a refusal
// with status 78 (rule 22). None of them prints a line or a summary.
func TestAuditExportUsageMistakesAndRefusals(t *testing.T) {
	dir, config, st := exportData(t)
	exportAuditRow(t, st, "a", exportAt(1))
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"a --since that is no time":  {"--since", "yesterday"},
		"a negative duration":        {"--since", "-5h"},
		"a --since without a value":  {"--since"},
		"an argument":                {"extra"},
		"an unknown flag":            {"--nope"},
		"a limit, which it has none": {"--limit", "5"},
		"a flag of ls":               {"--kind", "secret.set"},
		"--json":                     {"--json"},
	} {
		stdout, stderr, err := runExport(t, config, exportLate, args...)
		if err == nil || exitCode(err) != 2 || stdout != "" || stderr != "" {
			t.Errorf("%s: err = %v (status %d), stdout %q, stderr %q; want a usage mistake with status 2 and no output", name, err, exitCode(err), stdout, stderr)
		}
	}
	// The command without a subcommand names both subcommands.
	var out bytes.Buffer
	if err := AuditCommand(context.Background(), []string{"--config", config}, &out); err == nil || exitCode(err) != 2 ||
		!strings.Contains(err.Error(), "flats audit ls") || !strings.Contains(err.Error(), "flats audit export [--since TIME]") {
		t.Errorf("no subcommand: %v (status %d)", err, exitCode(err))
	}

	var latest int
	exportSQL(t, dir, func(tx *sql.Tx) error { return tx.QueryRow("PRAGMA user_version").Scan(&latest) })
	setVersion := func(v int) {
		t.Helper()
		exportSQL(t, dir, func(tx *sql.Tx) error {
			_, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", v))
			return err
		})
	}
	refused := func(name, text string) {
		t.Helper()
		before := snapshotDir(t, dir)
		stdout, stderr, err := runExport(t, config, exportLate)
		if err == nil || exitCode(err) != ExitConfig || !strings.Contains(err.Error(), text) || stdout != "" || stderr != "" {
			t.Errorf("%s: err = %v (status %d), stdout %q, stderr %q; want a refusal with status 78 that says %q", name, err, exitCode(err), stdout, stderr, text)
		}
		if after := snapshotDir(t, dir); !mapsEqual(before, after) {
			t.Errorf("%s: the refusal changed the data directory", name)
		}
	}
	setVersion(latest - 1)
	refused("the schema before this one", "start the host once")
	setVersion(latest + 1)
	refused("a later schema", "older than the database")
	setVersion(latest)
	if lines, _ := mustExport(t, config, exportLate); len(lines) != 1 {
		t.Errorf("after the version is put back: %d lines, want 1", len(lines))
	}
	for _, f := range []string{"flats.db", "flats.db-wal", "flats.db-shm"} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	refused("no database", "no flats.db")
	if _, err := os.Stat(filepath.Join(dir, "flats.db")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the command created flats.db: %v", err)
	}
}

// The command writes nothing: no row and no file. For a host that stopped cleanly the data directory is the same,
// byte for byte, before and after, and for a host that runs no file is added (rule 22).
func TestAuditExportWritesNothingForARunningAndAStoppedHost(t *testing.T) {
	dir, config, st := exportData(t)
	ctx := context.Background()
	exportFlat(t, st, "app")
	exportSecret(t, st, "app", "API_KEY", exportAt(1))
	exportRequest(t, st, "apr-1", "app", "delete", "why", exportAt(2))
	if err := st.DecideApproval(ctx, "apr-1", "rejected", "no", exportAt(3)); err != nil {
		t.Fatal(err)
	}
	want := []string{"audit API_KEY", "created apr-1", "decided apr-1"}
	// While the store of the host is open, its database files can change under it, so only their names count.
	live := []string{"flats.db-wal", "flats.db-shm", "flats.db"}
	before := snapshotDir(t, dir, live...)
	lines, _ := mustExport(t, config, exportLate)
	if got := exportKeys(lines); !slices.Equal(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	if after := snapshotDir(t, dir, live...); !mapsEqual(before, after) {
		t.Errorf("the directory of a running host changed:\n%v\n%v", before, after)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	before = snapshotDir(t, dir)
	if _, left := before["flats.db-wal"]; left {
		t.Fatal("the store left a -wal file when it closed: the test does not show a cleanly stopped host")
	}
	lines, _ = mustExport(t, config, exportLate)
	if got := exportKeys(lines); !slices.Equal(got, want) {
		t.Fatalf("lines of the stopped host = %q, want %q", got, want)
	}
	if after := snapshotDir(t, dir); !mapsEqual(before, after) {
		t.Errorf("the directory of a cleanly stopped host changed:\n%v\n%v", before, after)
	}
}

// A stopped host that left a -wal file and no -shm: the export reads what only the -wal file holds, and the open makes
// an empty -shm and nothing else, as for `flats audit ls`. A folder that cannot be written gives status 1 (rule 22).
func TestAuditExportOfAStoppedHostThatLeftAWalFile(t *testing.T) {
	// The source is built while a store holds it open, so that its commits are in the -wal file, and both files are copied.
	srcDir := t.TempDir()
	src, err := store.Open(filepath.Join(srcDir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	exportAuditRow(t, src, "in-the-wal", exportAt(1))
	exportRequest(t, src, "apr-in-the-wal", "app", "delete", "", exportAt(2))
	if fi, err := os.Stat(filepath.Join(srcDir, "flats.db-wal")); err != nil || fi.Size() == 0 {
		t.Skipf("the store keeps no commits in a -wal file here: %v", err)
	}
	stoppedHost := func(t *testing.T) (dir, config string) {
		t.Helper()
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
	dir, config := stoppedHost(t)
	before := snapshotDir(t, dir)
	lines, _ := mustExport(t, config, exportLate)
	if got, want := exportKeys(lines), []string{"audit in-the-wal", "created apr-in-the-wal"}; !slices.Equal(got, want) {
		t.Fatalf("lines = %q, want %q: the rows that only the -wal file holds", got, want)
	}
	after := snapshotDir(t, dir, "flats.db-shm")
	if _, ok := after["flats.db-shm"]; !ok {
		t.Error("the open created no -shm index: the test does not show the case of the spec")
	}
	delete(after, "flats.db-shm")
	if !mapsEqual(before, after) {
		t.Errorf("more than an empty -shm changed:\n%v\n%v", before, after)
	}
	t.Run("a folder that cannot be written", func(t *testing.T) {
		if ostest.Windows || os.Geteuid() == 0 {
			t.Skip("needs Unix modes and a user that is not the superuser")
		}
		dir, config := stoppedHost(t)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o700) })
		stdout, stderr, err := runExport(t, config, exportLate)
		if err == nil || exitCode(err) != 1 || stdout != "" || stderr != "" {
			t.Errorf("an unwritable folder: %v (status %d), stdout %q, stderr %q; want status 1 and no output", err, exitCode(err), stdout, stderr)
		}
	})
}

// reason and result are cut at 1 KiB and decided_by at 512 bytes, and no character is split (rule 23).
func TestAuditExportCutsTheLongTextOfAnApprovalLine(t *testing.T) {
	_, config, st := exportData(t)
	for _, tc := range []struct {
		id, reason, result, decidedBy  string
		reasonLen, resultLen, actorLen int
	}{
		{"ascii", strings.Repeat("a", 2048), strings.Repeat("r", 2048), strings.Repeat("d", 600), 1024, 1024, 512},
		// Three bytes each: byte 1024 and byte 512 are inside a character, and the cut steps back to its start.
		{"hangul", strings.Repeat("가", 683), strings.Repeat("나", 683), strings.Repeat("다", 200), 1023, 1023, 510},
		// Two bytes each after one byte, so that byte 1024 and byte 512 are the second byte of a character.
		{"latin", "x" + strings.Repeat("é", 1024), "y" + strings.Repeat("ü", 1024), "z" + strings.Repeat("ø", 300), 1023, 1023, 511},
		// Four bytes each: the cut lands on the start of a character and keeps all before it.
		{"emoji", strings.Repeat("😀", 512), strings.Repeat("🚀", 512), strings.Repeat("🙂", 150), 1024, 1024, 512},
		{"at the limit", strings.Repeat("a", 1024), strings.Repeat("r", 1024), strings.Repeat("d", 512), 1024, 1024, 512},
		{"short", "why", "done", "console", 3, 4, 7},
		{"empty", "", "", "", 0, 0, 0},
	} {
		exportRequest(t, st, tc.id, "app", "delete", tc.reason, exportAt(1))
		exportDecision(t, st, tc.id, tc.decidedBy, "failed", tc.result, exportAt(2))
		lines, _ := mustExport(t, config, exportLate)
		var created, decided map[string]any
		for _, l := range lines {
			if l["id"] == tc.id && l["event"] == "created" {
				created = l
			}
			if l["id"] == tc.id && l["event"] == "decided" {
				decided = l
			}
		}
		if created == nil || decided == nil {
			t.Fatalf("%s: the lines of the approval are missing", tc.id)
		}
		for field, c := range map[string]struct {
			got  any
			full string
			size int
		}{
			"reason":     {created["reason"], tc.reason, tc.reasonLen},
			"result":     {decided["result"], tc.result, tc.resultLen},
			"decided_by": {decided["decided_by"], tc.decidedBy, tc.actorLen},
		} {
			got, ok := c.got.(string)
			if !ok || len(got) != c.size || !strings.HasPrefix(c.full, got) || !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) {
				t.Errorf("%s: %s has %d bytes, want the first %d bytes of the %d, whole characters only", tc.id, field, len(got), c.size, len(c.full))
			}
		}
	}
}

// Two flats that had one slug at different times, and a flat that was renamed, are told apart by flat_id in the
// approval lines, which is also the flat_id of their audit rows (rule 23).
func TestAuditExportSeparatesFlatsOfOneSlugByTheirID(t *testing.T) {
	_, config, st := exportData(t)
	ctx := context.Background()
	first := exportFlat(t, st, "app")
	exportSecret(t, st, "app", "FIRST", exportAt(1))
	exportRequest(t, st, "apr-first", "app", "delete", "", exportAt(2))
	exportDecision(t, st, "apr-first", "console", "approved", "deleted", exportAt(3))
	if err := st.DeleteFlat(ctx, "app", store.Audit{Time: exportAt(3), Approval: "apr-first"}); err != nil {
		t.Fatal(err)
	}
	second := exportFlat(t, st, "app")
	exportSecret(t, st, "app", "SECOND", exportAt(4))
	exportRequest(t, st, "apr-second", "app", "set_visibility", "", exportAt(5))
	// The second flat moves to another slug, and a third flat takes the slug.
	if err := st.RenameFlat(ctx, "app", "moved", exportAt(6), exportAt(6)); err != nil {
		t.Fatal(err)
	}
	third := exportFlat(t, st, "app")
	exportRequest(t, st, "apr-third", "app", "delete", "", exportAt(7))
	// An approval of a slug that no flat has carries no ID.
	exportRequest(t, st, "apr-nothing", "nothing", "delete", "", exportAt(8))
	if first.ID == second.ID || second.ID == third.ID || first.ID == third.ID {
		t.Fatalf("the flats share an ID: %q %q %q", first.ID, second.ID, third.ID)
	}

	lines, _ := mustExport(t, config, exportLate)
	ids, slugs := map[string]string{}, map[string]string{}
	for i, l := range lines {
		key := exportKeys(lines[i : i+1])[0]
		if l["record"] == "audit" {
			key += " " + l["kind"].(string)
		}
		ids[key], slugs[key] = l["flat_id"].(string), l["flat"].(string)
	}
	wantIDs := map[string]string{
		"audit FIRST secret.set":    first.ID,
		"created apr-first":         first.ID,
		"decided apr-first":         first.ID,
		"audit FIRST secret.delete": first.ID,
		"audit SECOND secret.set":   second.ID,
		"created apr-second":        second.ID,
		"created apr-third":         third.ID,
		"created apr-nothing":       "",
	}
	if !reflect.DeepEqual(ids, wantIDs) {
		t.Errorf("the flat_id of each line =\n%v, want\n%v", ids, wantIDs)
	}
	// An audit row keeps the slug that it was written under, and an approval line has the slug of the rename. The slug
	// of apr-first is not compared: a rename rewrites the approvals by slug, so it changed that row of the first flat
	// too, which is why the ID is what tells the flats apart.
	for key, want := range map[string]string{
		"audit FIRST secret.set": "app", "audit FIRST secret.delete": "app", "audit SECOND secret.set": "app",
		"created apr-second": "moved", "created apr-third": "app", "created apr-nothing": "nothing",
	} {
		if slugs[key] != want {
			t.Errorf("%s: flat = %q, want %q", key, slugs[key], want)
		}
	}
}

// markWriter keeps what it is given and calls seen, once, as soon as that holds mark.
type markWriter struct {
	buf  bytes.Buffer
	mark string
	seen func()
	done bool
}

func (w *markWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	if !w.done && bytes.Contains(w.buf.Bytes(), []byte(w.mark)) {
		w.done = true
		w.seen()
	}
	return len(p), nil
}

// The lines of a page reach the output before the command reads the next page, so a long export streams. The reader
// of the output adds a row when it gets the last line of the first page, and the second page has that row.
func TestAuditExportWritesAPageOutBeforeItReadsTheNext(t *testing.T) {
	dir, config, st := exportData(t)
	now := exportLate
	// One row for each millisecond, so the first page ends with row 499. The second page has three rows.
	exportSQL(t, dir, func(tx *sql.Tx) error {
		for i := range auditPage + 3 {
			if _, err := tx.Exec(`INSERT INTO audit_events(at,kind,name,via) VALUES(?,?,?,?)`, exportT0.UnixMilli()+int64(i), "key.backup", fmt.Sprintf("n%05d", i), "command"); err != nil {
				return err
			}
		}
		return nil
	})
	w := &markWriter{mark: fmt.Sprintf(`"name":"n%05d"`, auditPage-1)}
	w.seen = func() { exportAuditRow(t, st, "late", now.Add(-30*time.Second)) }
	var errOut bytes.Buffer
	exportClock(t, now, &errOut)
	if err := AuditCommand(context.Background(), []string{"export", "--config", config}, w); err != nil {
		t.Fatal(err)
	}
	lines, _ := checkExport(t, w.buf.String(), errOut.String(), now)
	if got := exportKeys(lines); len(got) != auditPage+4 || got[len(got)-1] != "audit late" {
		t.Errorf("%d lines, the last is %q; want %d and the row that was added when the first page had been written", len(got), got[len(got)-1], auditPage+4)
	}
}

// failingWriter fails every write.
type failingWriter struct {
	err    error
	writes int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.writes++
	return 0, w.err
}

// A write that fails ends the command with status 1 and without the summary, which would name a place that the
// reader did not get to.
func TestAuditExportStopsAtAFailedWrite(t *testing.T) {
	dir, config, _ := exportData(t)
	exportBulk(t, dir, 1203, 600)
	w := &failingWriter{err: errors.New("the reader went away")}
	var errOut bytes.Buffer
	exportClock(t, exportLate, &errOut)
	err := AuditCommand(context.Background(), []string{"export", "--config", config}, w)
	if !errors.Is(err, w.err) || exitCode(err) != 1 {
		t.Errorf("err = %v (status %d), want the error of the write with status 1", err, exitCode(err))
	}
	if errOut.Len() != 0 {
		t.Errorf("the standard error = %q, want no summary after a failed write", errOut.String())
	}
	if w.writes != 1 {
		t.Errorf("%d writes after the first one failed, want none", w.writes-1)
	}
}

// The output filter of the real command line passes every line of the export through as it is, and each line is JSON
// with the text that was stored. The reasons of the approvals hold every character of the first Unicode plane, among
// them DEL and the C1 controls, which the filter would otherwise turn into \x.., and the bidi overrides.
func TestAuditExportLinesPassTheFilterOfTheCommandLineUnchanged(t *testing.T) {
	_, config, st := exportData(t)
	// Reasons of at most 1000 bytes, so that none is cut. Surrogates are no characters of their own.
	var reasons []string
	var chunk strings.Builder
	for r := rune(0); r <= 0xffff; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		if chunk.Len()+utf8.RuneLen(r) > 1000 {
			reasons = append(reasons, chunk.String())
			chunk.Reset()
		}
		chunk.WriteRune(r)
	}
	// A few characters of the other planes, and bytes that are no UTF-8.
	reasons = append(reasons, chunk.String(), "😀\U000e0001\U0010ffff", "broken \xff\xc3 bytes")
	for i, reason := range reasons {
		exportRequest(t, st, fmt.Sprintf("apr-%04d", i), "app", "delete", reason, exportAt(1))
	}
	// An audit row with such characters in its text fields and in its data.
	const odd = "a\x7fb\u0085c\u202ed\x1be"
	if err := st.AppendAudit(context.Background(), store.AuditEvent{Time: exportAt(2), Kind: store.AuditKeyBackup, Via: store.AuditViaCommand, Name: odd, Actor: odd,
		Data: store.AuditData(map[string]any{"text": odd})}); err != nil {
		t.Fatal(err)
	}

	direct, stderr, err := runExport(t, config, exportLate)
	if err != nil {
		t.Fatal(err)
	}
	lines, _ := checkExport(t, direct, stderr, exportLate)
	if len(lines) != len(reasons)+1 {
		t.Fatalf("%d lines, want %d", len(lines), len(reasons)+1)
	}
	for i, reason := range reasons {
		// JSON has no bytes that are not UTF-8: the encoder writes the replacement character for each, as the
		// conversion to runes does.
		if want := string([]rune(reason)); lines[i]["reason"] != want {
			t.Errorf("the reason of approval %d came back changed: %q, want %q", i, lines[i]["reason"], want)
		}
	}
	if l := lines[len(reasons)]; l["name"] != odd || l["actor"] != odd || l["data"].(map[string]any)["text"] != odd {
		t.Errorf("the audit line = %v, want the text %q in its fields", l, odd)
	}

	// The same export through the command line, whose standard output goes through the filter.
	oldHook := cli.Audit
	cli.Audit = AuditCommand
	t.Cleanup(func() { cli.Audit = oldHook })
	var out, errOut, summary bytes.Buffer
	exportClock(t, exportLate, &summary)
	env := cli.Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut, Getenv: func(string) string { return "" }, Home: t.TempDir(), ConfigDir: t.TempDir()}
	if code := cli.Run(context.Background(), []string{"audit", "export", "--config", config}, env); code != 0 || errOut.Len() != 0 {
		t.Fatalf("flats audit export: status %d, stderr %q", code, errOut.String())
	}
	if out.String() != direct {
		t.Errorf("the filter of the command line changed the export: %d bytes became %d", len(direct), out.Len())
	}
	if summary.String() != stderr {
		t.Errorf("the summary through the command line = %q, want %q", summary.String(), stderr)
	}
}

// docs/spec/portal-sign-in.md, rule 30: no line of `flats audit export` holds the value of the sign-in client secret.
// The host sets, sets again and removes it, and applies a sign-in change, so the export has the secret.set and
// secret.delete rows of the client secret and the lines of the approval.
func TestAuditExportHoldsNoSignInClientSecret(t *testing.T) {
	dir, config, st := exportData(t)
	ctx := context.Background()
	priv, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { priv.Close() })
	svc, err := core.New(ctx, core.Config{DataDir: dir, Store: st, Private: priv, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	if _, err := svc.CreateFlat(ctx, "app", "", core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	const value = "cs-export-distinct-5Mq"
	for _, issuer := range []string{"https://idp.example.com", "https://other.example.com"} {
		if _, err := svc.SetSignInSecret(ctx, "app", value, issuer, "flats-app", core.ViaCLI); err != nil {
			t.Fatal(err)
		}
	}
	r, err := svc.RequestSignIn(ctx, "app", core.SignInRequest{Provider: "oidc", Issuer: "https://idp.example.com", ClientID: "flats-app", Allow: []string{"ann@example.com"}}, core.ViaCLI)
	if err != nil || r.Approval == nil {
		t.Fatalf("request = %+v, %v", r, err)
	}
	if a, err := svc.Decide(ctx, r.Approval.ID, true); err != nil || a.Status != "approved" {
		t.Fatalf("decide = %+v, %v", a, err)
	}
	if _, err := svc.DeleteSignInSecret(ctx, "app", core.ViaCLI); err != nil {
		t.Fatal(err)
	}

	// The rows have the time of the machine; a clock an hour later has each of them before its cutoff.
	now := time.Now().Add(time.Hour)
	stdout, stderr, err := runExport(t, config, now)
	if err != nil {
		t.Fatal(err)
	}
	lines, _ := checkExport(t, stdout, stderr, now)
	if strings.Contains(stdout+stderr, value) {
		t.Fatalf("the export holds the client secret:\n%s", stdout)
	}
	var got []string
	for _, l := range lines {
		switch {
		case l["record"] == "audit" && l["name"] == store.SignInSecretName:
			got = append(got, l["kind"].(string))
		case l["record"] == "approval" && l["action"] == "set_signin":
			got = append(got, "approval "+l["event"].(string))
		}
	}
	// Lines of one millisecond keep the order of rule 24, which the tests above check; here only the lines count.
	slices.Sort(got)
	if want := []string{"approval created", "approval decided", "secret.delete", "secret.set", "secret.set"}; !slices.Equal(got, want) {
		t.Fatalf("the lines of the client secret and the approval = %q, want %q:\n%s", got, want, stdout)
	}
}
