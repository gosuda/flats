package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// docs/spec/event-webhook.md, rules 22 to 25: the reads of `flats audit export`.

// exportBase is the time that the fixtures of these tests count from. Every time is fixed, so no test reads a clock.
var exportBase = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

// exportReader opens a reader of s that the test closes.
func exportReader(t *testing.T, s *Store) *AuditReader {
	t.Helper()
	rd, err := OpenAuditReader(s.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rd.Close() })
	return rd
}

// approvalAt inserts an approval that was requested at the given time, with the given status. A zero decided leaves
// decided_at NULL.
func approvalAt(t *testing.T, s *Store, id, status string, requested, decided time.Time) {
	t.Helper()
	var dec any
	if !decided.IsZero() {
		dec = unix(decided)
	}
	if _, err := s.db.Exec(`INSERT INTO approvals(id,flat,action,params,status,via,requested_at,decided_at) VALUES(?,?,?,?,?,?,?,?)`,
		id, "app", "delete", "{}", status, "mcp", unix(requested), dec); err != nil {
		t.Fatal(err)
	}
}

// walkExport reads a stream to its end in pages of limit rows and returns the key of every row, in the order read.
// It fails when a page is larger than the limit.
func walkExport[R any](t *testing.T, limit int, read func(after *R, limit int) ([]R, error), key func(R) string) []string {
	t.Helper()
	var out []string
	var after *R
	for pages := 0; ; pages++ {
		if pages > 1000 {
			t.Fatal("the stream does not end")
		}
		rows, err := read(after, limit)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > limit {
			t.Fatalf("a page of %d rows, more than the limit %d", len(rows), limit)
		}
		for _, r := range rows {
			out = append(out, key(r))
		}
		if len(rows) < limit {
			return out
		}
		last := rows[len(rows)-1]
		after = &last
	}
}

func auditStream(rd *AuditReader, rng ExportRange) func(*AuditEvent, int) ([]AuditEvent, error) {
	return func(after *AuditEvent, limit int) ([]AuditEvent, error) {
		return rd.ExportAudit(context.Background(), rng, after, limit)
	}
}

func createdStream(rd *AuditReader, rng ExportRange) func(*ExportApproval, int) ([]ExportApproval, error) {
	return func(after *ExportApproval, limit int) ([]ExportApproval, error) {
		return rd.ExportCreated(context.Background(), rng, after, limit)
	}
}

func decidedStream(rd *AuditReader, rng ExportRange) func(*ExportApproval, int) ([]ExportApproval, error) {
	return func(after *ExportApproval, limit int) ([]ExportApproval, error) {
		return rd.ExportDecided(context.Background(), rng, after, limit)
	}
}

func auditName(e AuditEvent) string        { return e.Name }
func approvalKey(a ExportApproval) string  { return a.ID }
func approvalTime(a ExportApproval) string { return a.ID + "@" + a.At.Format("15:04:05.000") }

// Each stream is ordered by its time and then by the id, also when the ids were given in another order than the
// times, and pages of any size join into the whole stream: a row that shares its time with the last row of a page is
// neither skipped nor read twice (rule 24).
func TestExportStreamsAreOrderedByTimeAndIDAcrossPages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	at := func(ms int) time.Time { return exportBase.Add(time.Duration(ms) * time.Millisecond) }

	// Audit rows: the ids rise with the insert, and the times do not. Five rows share one time.
	for _, r := range []struct {
		name string
		ms   int
	}{{"c1", 20}, {"c2", 20}, {"c3", 20}, {"c4", 20}, {"c5", 20}, {"e1", 40}, {"a1", 0}, {"d1", 30}, {"d2", 30}, {"b1", 10}} {
		if err := s.AppendAudit(ctx, AuditEvent{Time: at(r.ms), Kind: AuditKeyBackup, Via: AuditViaCommand, Name: r.name}); err != nil {
			t.Fatal(err)
		}
	}
	wantAudit := []string{"a1", "b1", "c1", "c2", "c3", "c4", "c5", "d1", "d2", "e1"}

	// Approvals: ids that sort in another order than they were inserted in, several with one time of request. The
	// decisions come in yet another order. p1 is pending and q1 is applying, so neither has a decision. n1 and z1 have a
	// final status and no time of decision (NULL and 0), so they count at the time of their request.
	for _, a := range []struct {
		id, status string
		req, dec   int // dec < 0: decided_at is NULL
	}{
		{"m3", "approved", 5, 90},
		{"m1", "rejected", 5, 70},
		{"m2", "failed", 5, 70},
		{"p1", "pending", 5, -1},
		{"k9", "approved", 1, 60},
		{"q1", "applying", 6, -1},
		{"n1", "failed", 65, -1},
		{"z1", "failed", 80, 0},
	} {
		var dec time.Time
		if a.dec >= 0 {
			dec = at(a.dec)
		}
		approvalAt(t, s, a.id, a.status, at(a.req), dec)
	}
	if _, err := s.db.Exec(`UPDATE approvals SET decided_at=0 WHERE id='z1'`); err != nil {
		t.Fatal(err)
	}
	wantCreated := []string{"k9", "m1", "m2", "m3", "p1", "q1", "n1", "z1"}
	wantDecided := []string{"k9@09:00:00.060", "n1@09:00:00.065", "m1@09:00:00.070", "m2@09:00:00.070", "z1@09:00:00.080", "m3@09:00:00.090"}

	rd := exportReader(t, s)
	for _, limit := range []int{1, 2, 3, 4, 500} {
		if got := walkExport(t, limit, auditStream(rd, ExportRange{}), auditName); !slices.Equal(got, wantAudit) {
			t.Errorf("audit rows in pages of %d = %v, want %v", limit, got, wantAudit)
		}
		if got := walkExport(t, limit, createdStream(rd, ExportRange{}), approvalKey); !slices.Equal(got, wantCreated) {
			t.Errorf("requests in pages of %d = %v, want %v", limit, got, wantCreated)
		}
		if got := walkExport(t, limit, decidedStream(rd, ExportRange{}), approvalTime); !slices.Equal(got, wantDecided) {
			t.Errorf("decisions in pages of %d = %v, want %v", limit, got, wantDecided)
		}
	}
}

// Both ends of the range belong to it (rules 22 and 25), and the range is compared with the time of the stream: the
// time of the request for a request, and the time of the decision for a decision.
func TestExportRangeKeepsBothOfItsEnds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	at := func(ms int) time.Time { return exportBase.Add(time.Duration(ms) * time.Millisecond) }
	for _, ms := range []int{99, 100, 101, 199, 200, 201} {
		if err := s.AppendAudit(ctx, AuditEvent{Time: at(ms), Kind: AuditKeyBackup, Via: AuditViaCommand, Name: fmt.Sprint(ms)}); err != nil {
			t.Fatal(err)
		}
		// r<ms> is requested at ms and never decided. d<ms> was requested long before and is decided at ms.
		approvalAt(t, s, fmt.Sprintf("r%d", ms), "pending", at(ms), time.Time{})
		approvalAt(t, s, fmt.Sprintf("d%d", ms), "approved", at(-1000), at(ms))
	}
	// A final status without a time of decision falls back to the time of the request, for the range too.
	approvalAt(t, s, "n150", "failed", at(150), time.Time{})
	rd := exportReader(t, s)
	for name, tc := range map[string]struct {
		rng                    ExportRange
		audit, created, decide []string
	}{
		"open": {ExportRange{},
			[]string{"99", "100", "101", "199", "200", "201"},
			[]string{"d100", "d101", "d199", "d200", "d201", "d99", "r99", "r100", "r101", "n150", "r199", "r200", "r201"},
			[]string{"d99", "d100", "d101", "n150", "d199", "d200", "d201"}},
		"since": {ExportRange{Since: at(100)},
			[]string{"100", "101", "199", "200", "201"},
			[]string{"r100", "r101", "n150", "r199", "r200", "r201"},
			[]string{"d100", "d101", "n150", "d199", "d200", "d201"}},
		"cutoff": {ExportRange{Cutoff: at(200)},
			[]string{"99", "100", "101", "199", "200"},
			[]string{"d100", "d101", "d199", "d200", "d201", "d99", "r99", "r100", "r101", "n150", "r199", "r200"},
			[]string{"d99", "d100", "d101", "n150", "d199", "d200"}},
		"both": {ExportRange{Since: at(100), Cutoff: at(200)},
			[]string{"100", "101", "199", "200"},
			[]string{"r100", "r101", "n150", "r199", "r200"},
			[]string{"d100", "d101", "n150", "d199", "d200"}},
		"one instant": {ExportRange{Since: at(200), Cutoff: at(200)}, []string{"200"}, []string{"r200"}, []string{"d200"}},
		"nothing":     {ExportRange{Since: at(201), Cutoff: at(200)}, nil, nil, nil},
	} {
		// Pages of 2, so that the range holds across the page key too.
		if got := walkExport(t, 2, auditStream(rd, tc.rng), auditName); !slices.Equal(got, tc.audit) {
			t.Errorf("%s: audit rows = %v, want %v", name, got, tc.audit)
		}
		if got := walkExport(t, 2, createdStream(rd, tc.rng), approvalKey); !slices.Equal(got, tc.created) {
			t.Errorf("%s: requests = %v, want %v", name, got, tc.created)
		}
		if got := walkExport(t, 2, decidedStream(rd, tc.rng), approvalKey); !slices.Equal(got, tc.decide) {
			t.Errorf("%s: decisions = %v, want %v", name, got, tc.decide)
		}
	}
}

// An approval row has the fields of its two lines (rule 23): the slug that the row has now, the ID of the flat that
// the request named, and the text columns as empty strings when they are NULL.
func TestExportApprovalRowsHaveTheFieldsOfTheirLines(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	f := makeFlat(t, s, "app")
	req, claimed, done := exportBase, exportBase.Add(time.Second), exportBase.Add(2*time.Second)
	if err := s.InsertApproval(ctx, Approval{ID: "apr-1", Flat: "app", Action: "delete", Params: []byte(`{"secret":"in-params"}`), Status: "pending",
		Via: "mcp", Reason: "no longer used", RequestedAt: req, IdempotencyKey: "key-1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimApprovalAuthorized(ctx, "apr-1", `tailnet user "ann@example.org"`, claimed); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishApprovalData(ctx, "apr-1", "approved", "deleted", []byte(`{"receipt":"in-result-data"}`), done); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameFlat(ctx, "app", "renamed", done, done); err != nil {
		t.Fatal(err)
	}
	// A row as an older Flats wrote it: no reason, no result, no decided_by, and no flat ID.
	approvalAt(t, s, "apr-0", "rejected", req.Add(-time.Hour), req.Add(-time.Minute))

	rd := exportReader(t, s)
	created, err := rd.ExportCreated(ctx, ExportRange{}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	wantCreated := []ExportApproval{
		{At: req.Add(-time.Hour), ID: "apr-0", Flat: "app", Action: "delete", Status: "rejected", Via: "mcp"},
		{At: req, ID: "apr-1", Flat: "renamed", FlatID: f.ID, Action: "delete", Status: "approved", Via: "mcp", Reason: "no longer used",
			DecidedBy: `tailnet user "ann@example.org"`, Result: "deleted"},
	}
	if !slices.Equal(created, wantCreated) {
		t.Errorf("requests =\n%+v, want\n%+v", created, wantCreated)
	}
	decided, err := rd.ExportDecided(ctx, ExportRange{}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	wantDecided := slices.Clone(wantCreated)
	wantDecided[0].At, wantDecided[1].At = req.Add(-time.Minute), done
	if !slices.Equal(decided, wantDecided) {
		t.Errorf("decisions =\n%+v, want\n%+v", decided, wantDecided)
	}
	for _, a := range append(created, decided...) {
		if text := fmt.Sprintf("%+v", a); strings.Contains(text, "in-params") || strings.Contains(text, "in-result-data") || strings.Contains(text, "key-1") {
			t.Errorf("a row holds a column that no line has: %s", text)
		}
	}
}

// A page is closed when its read returns. The host then checkpoints with the reader still open, which a reader that
// kept a read transaction on an older state would stop, and the next page sees what the host wrote (rule 22).
func TestExportPagesHoldNoReadBetweenThem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	for i := range 6 {
		if err := s.AppendAudit(ctx, AuditEvent{Time: exportBase.Add(time.Duration(i) * time.Second), Kind: AuditKeyBackup, Via: AuditViaCommand, Name: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
		approvalAt(t, s, fmt.Sprintf("apr-%d", i), "approved", exportBase.Add(time.Duration(i)*time.Second), exportBase.Add(time.Duration(i)*time.Second))
	}
	rd := exportReader(t, s)
	first, err := rd.ExportAudit(ctx, ExportRange{}, nil, 3)
	if err != nil || len(first) != 3 {
		t.Fatalf("first page of the audit rows: %d rows, %v", len(first), err)
	}
	firstCreated, err := rd.ExportCreated(ctx, ExportRange{}, nil, 3)
	if err != nil || len(firstCreated) != 3 {
		t.Fatalf("first page of the requests: %d rows, %v", len(firstCreated), err)
	}
	firstDecided, err := rd.ExportDecided(ctx, ExportRange{}, nil, 3)
	if err != nil || len(firstDecided) != 3 {
		t.Fatalf("first page of the decisions: %d rows, %v", len(firstDecided), err)
	}
	// A write first, so that the state that the reader saw is not the newest and the checkpoint has frames to move.
	late := exportBase.Add(time.Hour)
	if err := s.AppendAudit(ctx, AuditEvent{Time: late, Kind: AuditKeyBackup, Via: AuditViaCommand, Name: "late"}); err != nil {
		t.Fatal(err)
	}
	approvalAt(t, s, "late", "approved", late, late)
	if err := s.Sync(ctx); err != nil {
		t.Fatalf("the checkpoint of the host between two pages: %v", err)
	}
	rest, err := rd.ExportAudit(ctx, ExportRange{}, &first[2], 10)
	if err != nil || len(rest) != 4 || rest[3].Name != "late" {
		t.Errorf("second page of the audit rows = %+v, %v; want three rows and the one written meanwhile", rest, err)
	}
	restCreated, err := rd.ExportCreated(ctx, ExportRange{}, &firstCreated[2], 10)
	if err != nil || len(restCreated) != 4 || restCreated[3].ID != "late" {
		t.Errorf("second page of the requests = %+v, %v", restCreated, err)
	}
	restDecided, err := rd.ExportDecided(ctx, ExportRange{}, &firstDecided[2], 10)
	if err != nil || len(restDecided) != 4 || restDecided[3].ID != "late" {
		t.Errorf("second page of the decisions = %+v, %v", restDecided, err)
	}
}

// The streams of the audit rows and of the requests are read through the indexes of schema 26, so a page costs its
// own rows and not the whole table.
func TestExportPagesOfAuditRowsAndRequestsUseTheirIndexes(t *testing.T) {
	t.Parallel()
	s := open(t)
	rng := ExportRange{Since: exportBase, Cutoff: exportBase.Add(time.Hour)}
	for _, tc := range []struct {
		name, table, at, index string
		after                  []any
	}{
		{"audit rows", `audit_events`, `at`, "audit_events_at", []any{unix(exportBase), int64(7)}},
		{"requests", `approvals`, `requested_at`, "approvals_requested_at", []any{unix(exportBase), "apr-7"}},
	} {
		name := tc.name
		query, args := exportPage(`id`, tc.table, tc.at, ``, rng, tc.after, 500)
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		if text := strings.Join(plan, "; "); !strings.Contains(text, "SEARCH") || !strings.Contains(text, tc.index) {
			t.Errorf("%s: the plan is %q; want a search of %s", name, text, tc.index)
		}
	}
}

// The reader of the export is the reader of `flats audit ls`: it refuses a database of the schema before this one and
// of a later one, and migrates neither (rule 22).
func TestExportReaderRefusesAnotherSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flats.db")
	t.Run("a database that the registry built up to version 25", func(t *testing.T) {
		withMigrations(t, migrationsThrough(25))
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := OpenAuditReader(path); !errors.Is(err, ErrAuditSchema) {
		t.Errorf("OpenAuditReader of version 25 = %v, want ErrAuditSchema", err)
	}
	if info, err := Inspect(path); err != nil || info.Version != 25 {
		t.Errorf("after the refusal: %+v, %v; want version 25 still", info, err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, latestVersion()+1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAuditReader(path); !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("OpenAuditReader of a later version = %v, want ErrSchemaTooNew", err)
	}
}
