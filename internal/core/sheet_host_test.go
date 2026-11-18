package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/sheet"
	"github.com/gosuda/flats/internal/store"
)

func TestSheetHostVersionRefusals(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newSheet(t, s, "budget")
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"list versions", func() error { _, err := s.ListVersions(ctx, "budget"); return err }},
		{"get version", func() error { _, err := s.GetVersion(ctx, "budget", 1); return err }},
		{"version file", func() error { _, err := s.VersionFile("budget", 1, "index.html"); return err }},
		{"deployment history", func() error { _, err := s.Deployments(ctx, "budget"); return err }},
		{"list previews", func() error { _, err := s.ListPreviews(ctx, "budget"); return err }},
		{"save draft", func() error {
			_, err := s.SaveDraft(ctx, "budget", []bundle.File{{Path: bundle.ManifestName, Data: []byte(`{"type":"sheet"}`)}}, SaveMeta{}, 0, ViaMCP)
			return err
		}},
		{"request create version", func() error { _, err := s.RequestCreateVersion(ctx, "budget", 0, "", ViaMCP); return err }},
		{"request publish", func() error { _, err := s.RequestPublish(ctx, "budget", 0, "", ViaMCP); return err }},
		{"rollback with data", func() error { _, err := s.RollbackWithData(ctx, "budget", 1, true, ViaMCP); return err }},
		{"get draft", func() error { _, err := s.GetDraft(ctx, "budget"); return err }},
		{"preview", func() error { _, err := s.OpenPreview(ctx, "budget", 0); return err }},
		{"create version", func() error { _, err := s.CreateVersion(ctx, "budget", 0, "", ViaMCP); return err }},
		{"publish", func() error { _, err := s.Publish(ctx, "budget", 0, "", ViaMCP); return err }},
		{"deploy", func() error { _, err := s.Deploy(ctx, "budget", 1, ViaMCP); return err }},
		{"version rollback", func() error { _, err := s.Rollback(ctx, "budget", 1, ViaMCP); return err }},
		{"version quota", func() error { return s.SetVersionQuotaOverride(ctx, "budget", nil) }},
		{"delete version", func() error { _, err := s.DeleteVersion(ctx, "budget", 1, ViaMCP); return err }},
		{"request delete version", func() error {
			_, err := s.RequestDeleteVersion(ctx, "budget", 1, ViaMCP)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if !errors.Is(err, ErrSheetUnversioned) || ErrorCategory(err) != "invalid" {
				t.Fatalf("refusal = %v (%s)", err, ErrorCategory(err))
			}
			if !strings.Contains(err.Error(), "update_sheet") || !strings.Contains(err.Error(), "revision") {
				t.Fatalf("refusal lacks sheet and history guidance: %v", err)
			}
		})
	}
	if _, err := s.SaveVersion(ctx, "fresh", []bundle.File{
		{Path: bundle.ManifestName, Data: []byte(`{"type":"sheet"}`)},
	}, SaveMeta{}, ViaMCP); !errors.Is(err, ErrSheetUnversioned) {
		t.Fatalf("sheet manifest on bundle path: %v", err)
	}
	if _, err := s.st.GetFlat(ctx, "fresh"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("refused bundle created a flat: %v", err)
	}
	if _, err := s.SaveVersion(ctx, "budget", []bundle.File{
		{Path: "index.html", Data: []byte("website")},
	}, SaveMeta{}, ViaMCP); !errors.Is(err, ErrTypeChange) || !strings.Contains(err.Error(), "update_sheet") {
		t.Fatalf("website into sheet: %v", err)
	}
	if got := sheetRead(t, s, "budget", 0, `{}`); len(got["tabs"].([]any)) != 1 {
		t.Fatalf("a refused version call changed the sheet: %+v", got)
	}
}

func TestSheetHostPendingInitAndShell(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	if _, err := s.CreateFlat(ctx, "budget", "Budget", ViaMCP); err != nil {
		t.Fatal(err)
	}
	if err := s.st.ContentTx(ctx, "budget", func(tx *store.ContentTx) error {
		return tx.InitState(ctx, contenttype.Sheet)
	}); err != nil {
		t.Fatal(err)
	}
	s.startPendingDesigns(ctx)
	s.startPendingDesigns(ctx)
	st, err := s.st.ContentState(ctx, "budget")
	if err != nil || st.Phase != "ready" {
		t.Fatalf("completed initialization = %+v, %v", st, err)
	}
	in := s.liveSheet("budget")
	if in == nil {
		t.Fatal("sheet did not start without a server runtime")
	}
	view, err := s.GetFlat(ctx, "budget")
	if err != nil || view.Type != contenttype.Sheet || view.Name != "Budget" {
		t.Fatalf("sheet view = %+v, %v", view, err)
	}
	if view.LiveVersion != 0 || view.Live != nil || view.Versions != 0 || view.VersionCount != 0 ||
		view.NextVersion != 0 || view.LatestVersion != 0 || view.LatestDeployed ||
		view.Draft != nil || view.Publication != "" || view.EffectiveVersionQuota != 0 || view.PublicAccess != "" {
		t.Fatalf("view exposes shell or guest-edit state: %+v", view)
	}
	f, err := s.st.GetFlat(ctx, "budget")
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.st.GetVersion(ctx, "budget", f.LiveVersion)
	if err != nil {
		t.Fatal(err)
	}
	if h, err := s.checkIsolated(ctx, f, v, nil); err != nil || !h.OK {
		t.Fatalf("isolated shell health = %+v, %v", h, err)
	}
	if s.liveSheet("budget") != in {
		t.Fatal("isolated health check replaced the live sheet")
	}
	page, err := s.ListRevisions(ctx, "budget", 0, 100)
	if err != nil || len(page.Revisions) != 1 || page.Revisions[0].Kind != "created" {
		t.Fatalf("initial history = %+v, %v", page, err)
	}
	if _, err := s.SetVisibilityAccess(ctx, "budget", store.Public, store.AccessEdit, ViaMCP, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("sheet admitted Public editing: %v", err)
	}
	if hasWorker("sheet", "server") || hasWorker("sheet", "static") {
		t.Fatal("sheet attributed to a worker")
	}
}

func TestSheetHostBlobStoreCategoriesAndPaths(t *testing.T) {
	s := &Service{cfg: Config{DataDir: t.TempDir()}}
	if got, want := s.storeDir("budget", contenttype.Sheet), filepath.Join(s.flatDir("budget"), "revisions"); got != want {
		t.Fatalf("sheet blob root = %s, want %s", got, want)
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"wrong type", ErrNotSheet, "not_sheet"},
		{"capacity", ErrSheetCapacity, "sheet_capacity"},
		{"version model", ErrSheetUnversioned, "invalid"},
		{"engine capacity", sheetErr(&sheet.WriteError{Code: "sheet_capacity", Limit: "cells"}), "sheet_capacity"},
		{"engine conflict", sheetErr(&sheet.WriteError{Code: "conflict", Reason: "tab_changed"}), "conflict"},
		{"engine unavailable", sheetErr(&sheet.WriteError{Code: "unavailable", Retry: true}), "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ErrorCategory(fmt.Errorf("wrapped: %w", tc.err)); got != tc.want {
				t.Fatalf("category = %s, want %s", got, tc.want)
			}
		})
	}
	hash := strings.Repeat("a", 64)
	for _, tc := range []struct {
		path  string
		valid bool
	}{
		{"sheet.json", true},
		{"tabs/3/axes.json", true},
		{"tabs/3/rows/0-0.json", true},
		{"tabs/12/rows/1f4-2.json", true},
		{"tabs/03/axes.json", false},
		{"tabs/3/rows/1F4-0.json", false},
		{"tabs/3/rows/0-00.json", false},
		{"tabs/3/rows/0.json", false},
		{"tabs/3/../sheet.json", false},
		{"tabs/x/axes.json", false},
		{"tabs/3/cells.json", false},
		{"notebook.json", false},
		{"sheet.json/extra", false},
		{"tabs/0/axes.json", false},
		{"tabs/9007199254740992/axes.json", false},
		{"tabs/3/rows/20000000000000-0.json", false},
	} {
		if err := sheet.CheckFile(sheet.File{Path: tc.path, Hash: hash, Size: 1}); (err == nil) != tc.valid {
			t.Errorf("content path %q: %v", tc.path, err)
		}
	}
	for _, f := range []sheet.File{{Path: "sheet.json", Hash: "bad", Size: 1}, {Path: "sheet.json", Hash: hash, Size: -1},
		{Path: "sheet.json", Hash: strings.ToUpper(hash), Size: 1}, {Path: "sheet.json", Hash: hash, Size: 1 << 30}} {
		if err := sheet.CheckFile(f); err == nil {
			t.Errorf("invalid content file accepted: %+v", f)
		}
	}
	if sheetRollbackTarget(sheetRollbackOp(7)) != 7 || sheetRollbackTarget("agent-op") != 0 || sheetRollbackTarget("sheet-rollback:0:x") != 0 {
		t.Fatal("a rollback op id does not name its target revision")
	}
}

func TestSheetWrongTypeAndRefusedFirstWrite(t *testing.T) {
	ctx := context.Background()
	s, dir := newTestService(t)
	for _, tc := range []struct{ name, raw string }{
		{"non object", `null`},
		{"unknown field", `{"ops":[],"unknown":true}`},
		{"bad operation", `{"ops":[{"op":"missing"}]}`},
		{"slug injection", `{"slug":"other","ops":[]}`},
		{"unknown tab", `{"ops":[{"op":"write","tab":"Missing","at":"A1","rows":[[1]]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.UpdateSheet(ctx, "fresh", []byte(tc.raw), ViaMCP)
			if err == nil || ErrorCategory(err) == "" || ErrorCategory(err) == "internal" {
				t.Fatalf("refused first write: %v (%s)", err, ErrorCategory(err))
			}
			if _, err := s.st.GetFlat(ctx, "fresh"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("a refused first write left a flat: %v", err)
			}
			if exists, err := pathExists(filepath.Join(dir, "flats", "fresh", "sheet")); err != nil || exists {
				t.Fatalf("a refused first write left a database: %t, %v", exists, err)
			}
		})
	}
	out, err := s.UpdateSheet(ctx, "fresh", []byte(`{"op_id":"first","ops":[`+sheetBudgetRows+`]}`), ViaMCP)
	if err != nil || !out.Created || out.Revision != 2 || out.Pending {
		t.Fatalf("creating write = %+v, %v", out, err)
	}
	if view, err := s.GetFlat(ctx, "fresh"); err != nil || view.Type != "sheet" || !view.Running {
		t.Fatalf("created sheet = %+v, %v", view, err)
	}
	if _, err := s.CreateNotebook(ctx, "notes", "Notes", ViaMCP); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSheet(ctx, "notes", []byte(`{"ops":[]}`), ViaMCP); !errors.Is(err, ErrNotSheet) || ErrorCategory(err) != "not_sheet" || !strings.Contains(err.Error(), "notebook") {
		t.Fatalf("update_sheet on a notebook = %v", err)
	}
	if _, err := s.GetSheet(ctx, "notes", 0, nil); !errors.Is(err, ErrNotSheet) {
		t.Fatalf("get_sheet on a notebook = %v", err)
	}
	if _, err := s.UpdateNotebook(ctx, "fresh", notebookPayload("wrong-type", notebookCreatePage), ViaMCP); !errors.Is(err, ErrNotNotebook) {
		t.Fatalf("update_notebook on a sheet = %v", err)
	}
	if _, err := s.GetSheet(ctx, "missing", 0, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get_sheet on a missing flat = %v", err)
	}
	if _, err := s.GetSheet(ctx, "fresh", 0, []byte(`{"tab":"Missing"}`)); ErrorCategory(err) != "not_found" {
		t.Fatalf("get_sheet of a missing tab = %v", err)
	}
	if _, err := s.UpdateSheet(ctx, "fresh", []byte(`{"ops":[`+sheetBudgetRows+`]}`), ViaMCP); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "state_required") {
		t.Fatalf("write without state = %v", err)
	}
}

func TestSheetRevisionsRollbackAndPin(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newSheet(t, s, "budget")
	writeSheet(t, s, "budget", "rows", sheetBudgetRows+`,`+sheetCurrencyFormat)
	writeSheet(t, s, "budget", "tabs", `{"op":"rename_tab","tab":"Sheet1","name":"Budget"},{"op":"add_tab","name":"Notes"}`)
	out, _ := writeSheet(t, s, "budget", "change", `{"op":"write","tab":"Budget","at":"B2","rows":[[5000]]},{"op":"delete_tab","tab":"Notes"}`)
	if out.Revision != 4 {
		t.Fatalf("revision = %+v", out)
	}
	page, err := s.ListRevisions(ctx, "budget", 0, 10)
	if err != nil || len(page.Revisions) != 4 {
		t.Fatalf("revisions = %+v, %v", page, err)
	}
	want := map[int][]store.SheetTabChange{
		4: {{Tab: 1, Change: "changed", NameBefore: "Budget", NameAfter: "Budget"}, {Tab: 2, Change: "removed", NameBefore: "Notes"}},
		3: {{Tab: 1, Change: "changed", NameBefore: "Sheet1", NameAfter: "Budget"}, {Tab: 2, Change: "added", NameAfter: "Notes"}},
		2: {{Tab: 1, Change: "changed", NameBefore: "Sheet1", NameAfter: "Sheet1"}},
		1: {{Tab: 1, Change: "added", NameAfter: "Sheet1"}},
	}
	for _, r := range page.Revisions {
		if !slices.Equal(r.SheetTabs, want[r.Number]) {
			t.Errorf("tabs of revision %d = %+v, want %+v", r.Number, r.SheetTabs, want[r.Number])
		}
		if r.Number > 1 && r.Kind != "agent" {
			t.Errorf("revision %d has kind %s", r.Number, r.Kind)
		}
	}
	// Revision 2 reads as it was, with the date it closed with.
	old := sheetRead(t, s, "budget", 2, `{"tab":"Sheet1","range":"B2:B4","include":["shown","formats"]}`)
	rows := old["rows"].([]any)
	if old["revision"] != float64(2) || old["state"] != nil || rows[0].([]any)[0] != float64(1200) || rows[2].([]any)[0].(map[string]any)["value"] != float64(2100) {
		t.Fatalf("revision read = %+v", old)
	}
	if shown := old["shown"].([]any)[2].([]any)[0]; shown != "$2,100.00" || len(old["formats"].([]any)) != 1 {
		t.Fatalf("revision read lost the number format: %+v", old)
	}
	if date, ok, err := s.st.SheetRevisionDate(ctx, "budget", 2); err != nil || !ok || old["date"] != date {
		t.Fatalf("revision date = %q, %t, %v; read %v", date, ok, err, old["date"])
	}
	if tabs := sheetRead(t, s, "budget", 3, `{}`)["tabs"].([]any); len(tabs) != 2 {
		t.Fatalf("tabs of revision 3 = %+v", tabs)
	}
	if _, err := s.GetSheet(ctx, "budget", 9, []byte(`{}`)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("read of a missing revision = %v", err)
	}

	state := sheetState(t, s, "budget")
	res, err := s.RollbackRevisionResult(ctx, "budget", 3, "agent", ViaMCP)
	if err != nil || res.Revision != 5 || res.Warning != "" {
		t.Fatalf("rollback = %+v, %v", res, err)
	}
	r, files, err := s.GetRevision(ctx, "budget", 5)
	if err != nil || r.Kind != "rollback" || r.RollbackOf != 3 {
		t.Fatalf("rollback revision = %+v, %v", r, err)
	}
	_, target, err := s.GetRevision(ctx, "budget", 3)
	if err != nil || !slices.Equal(files, target) {
		t.Fatalf("a rollback revision does not share the files of its target: %v", err)
	}
	if holds, err := s.st.Holds(ctx, "budget"); err != nil || len(holds) != 0 {
		t.Fatalf("holds after the rollback = %+v, %v", holds, err)
	}
	now := sheetRead(t, s, "budget", 0, `{}`)
	if tabs := now["tabs"].([]any); len(tabs) != 2 || tabs[0].(map[string]any)["name"] != "Budget" {
		t.Fatalf("sheet after the rollback = %+v", now)
	}
	if got := sheetRead(t, s, "budget", 0, `{"tab":"Budget","range":"B2"}`)["rows"].([]any)[0].([]any)[0]; got != float64(1200) {
		t.Fatalf("B2 after the rollback = %v", got)
	}
	stale := fmt.Sprintf(`{"state":%q,"ops":[{"op":"write","tab":"Budget","at":"A1","rows":[["late"]]}]}`, state)
	if _, err := s.UpdateSheet(ctx, "budget", []byte(stale), ViaMCP); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "rolled_back") {
		t.Fatalf("write based on a state before the rollback = %v", err)
	}
	if _, err := s.RollbackRevisionResult(ctx, "budget", 9, "agent", ViaMCP); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rollback to a missing revision = %v", err)
	}
	if holds, err := s.st.Holds(ctx, "budget"); err != nil || len(holds) != 0 {
		t.Fatalf("a refused rollback left a hold: %+v, %v", holds, err)
	}

	if err := s.PinRevision(ctx, "budget", 2, true); err != nil {
		t.Fatal(err)
	}
	if r, err := s.st.Revision(ctx, "budget", 2); err != nil || !r.Pinned {
		t.Fatalf("pinned revision = %+v, %v", r, err)
	}
	if m := s.sheetMetrics(); m.ImportCount < 5 || len(m.ImportCounts) != len(SheetImportBuckets)+1 {
		t.Fatalf("sheet import metrics = %+v", m)
	}
}

// directSheetWrite commits a write in the instance without core's import,
// as when the host stops between the two.
func directSheetWrite(t *testing.T, in *sheet.Instance, state, opID, ops string) sheet.WriteResult {
	t.Helper()
	u, err := sheet.DecodeUpdate([]byte(fmt.Sprintf(`{"state":%q,"op_id":%q,"ops":[%s]}`, state, opID, ops)))
	if err != nil {
		t.Fatal(err)
	}
	res, err := in.Update(context.Background(), u, 0)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestSheetCatchUpAfterRestart(t *testing.T) {
	ctx := context.Background()
	s, dir := newTestService(t)
	in := newSheet(t, s, "budget")
	state := sheetState(t, s, "budget")
	unlock := sync.OnceFunc(s.lock("budget"))
	defer unlock()
	res := directSheetWrite(t, in, state, "lost-import", sheetBudgetRows)
	if got := storedRevisions(t, s, "budget"); !slices.Equal(got, []int{1}) {
		t.Fatalf("revisions before the import = %v", got)
	}
	// The host stops here: the write is durable in sheet.db and flats.db
	// has no revision for it.
	image := crashImage(t, dir)
	unlock()

	s = restartFrom(t, image)
	page, err := s.ListRevisions(ctx, "budget", 0, 10)
	if err != nil || len(page.Revisions) != 2 || page.Revisions[0].Kind != "agent" {
		t.Fatalf("revisions after the restart = %+v, %v", page, err)
	}
	reopened := s.liveSheet("budget")
	if reopened == nil || reopened.StoreID() != in.StoreID() {
		t.Fatal("the restart replaced the sheet's store")
	}
	if n, ok, err := s.st.SheetImportRevision(ctx, "budget", reopened.StoreID(), res.OutboxSeq); err != nil || !ok || n != 2 {
		t.Fatalf("imported entry = %d, %t, %v", n, ok, err)
	}
	if entries, err := reopened.Outbox(ctx, 0, 10); err != nil || len(entries) != 0 {
		t.Fatalf("outbox after the import = %+v, %v", entries, err)
	}
	// The write was committed: its op id replays, and the values are there.
	body := fmt.Sprintf(`{"state":%q,"op_id":"lost-import","ops":[%s]}`, state, sheetBudgetRows)
	replay, err := s.UpdateSheet(ctx, "budget", []byte(body), ViaMCP)
	if err != nil || replay.Revision != 2 || !sheetResult(t, replay).Replay {
		t.Fatalf("replay after the restart = %+v, %v", replay, err)
	}
	if got := sheetRead(t, s, "budget", 0, `{"tab":"Sheet1","range":"B4"}`)["rows"].([]any)[0].([]any)[0].(map[string]any)["value"]; got != float64(2100) {
		t.Fatalf("total after the restart = %v", got)
	}
}

func TestSheetImportFailureKeepsTheWrite(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	in := newSheet(t, s, "budget")
	boardExecDB(t, s.sheetDB("budget"), `CREATE TRIGGER fail_ack BEFORE DELETE ON outbox BEGIN SELECT RAISE(ABORT, 'injected acknowledgement failure'); END`)
	out, result := writeSheet(t, s, "budget", "kept", sheetBudgetRows)
	if out.Revision != 2 || out.Pending || result.CellsWritten != 8 {
		t.Fatalf("write with a failed acknowledgement = %+v / %+v", out, result)
	}
	if entries, err := in.Outbox(ctx, 0, 10); err != nil || len(entries) != 1 {
		t.Fatalf("unacknowledged entries = %+v, %v", entries, err)
	}
	boardExecDB(t, s.sheetDB("budget"), `DROP TRIGGER fail_ack`)
	s.sheetRetry.Delete("budget")
	s.TickSheets(ctx)
	if entries, err := in.Outbox(ctx, 0, 10); err != nil || len(entries) != 0 {
		t.Fatalf("entries after the retry = %+v, %v", entries, err)
	}
	if got := storedRevisions(t, s, "budget"); !slices.Equal(got, []int{2, 1}) {
		t.Fatalf("the retry duplicated revisions: %v", got)
	}
}

func TestSheetPublicRouteEpochAndWithdrawal(t *testing.T) {
	s, _ := newTestService(t)
	in := newSheet(t, s, "budget")
	f, err := s.st.GetFlat(t.Context(), "budget")
	if err != nil {
		t.Fatal(err)
	}
	f.Visibility, f.PublicAccess = store.Public, store.AccessView
	if err := s.st.UpdateFlat(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	request := func(h http.Handler, path string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("GET", "http://budget.test"+path, nil)
		r.Header.Set(AccessEpochHeader, "forged")
		r.Header.Set("X-Flats-Access", "private")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	public := s.siteHandler("budget", true)
	if w := request(public, "/"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<html") {
		t.Fatalf("Public page = %d %q", w.Code, w.Body.String())
	}
	// A Public request cannot reach History, whatever access it claims.
	if w := request(public, "/_sheet/api/revisions"); w.Code == http.StatusOK {
		t.Fatalf("Public route served History: %d %s", w.Code, w.Body.String())
	}
	if w := request(s.siteHandler("budget", false), "/_sheet/api/revisions"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"revisions"`) {
		t.Fatalf("Private History = %d %s", w.Code, w.Body.String())
	}
	d := *s.currentDeployment("budget")
	d.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Observed-Epoch", r.Header.Get(AccessEpochHeader))
		w.Header().Set("Observed-Access", r.Header.Get("X-Flats-Access"))
	})
	s.state("budget").cur.Store(&d)
	old := s.siteHandler("budget", true)
	first := request(old, "/")
	epoch := first.Header().Get("Observed-Epoch")
	if epoch == "" || epoch == "forged" || first.Header().Get("Observed-Access") == "private" {
		t.Fatalf("Public route did not supply trusted headers: %v", first.Header())
	}
	if err := s.stopPublicRoutes(t.Context(), "budget"); err != nil {
		t.Fatal(err)
	}
	if w := request(old, "/"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("withdrawn handler returned %d", w.Code)
	}
	if next := request(s.siteHandler("budget", true), "/").Header().Get("Observed-Epoch"); next == "" || next == epoch {
		t.Fatalf("re-exposure reused epoch %q", next)
	}
	if s.liveSheet("budget") != in {
		t.Fatal("withdrawal replaced the sheet")
	}
}

func TestSheetUnloadsWhenIdleAndLoadsOnDemand(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	var offset atomic.Int64
	s.now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	in := newSheet(t, s, "budget")
	writeSheet(t, s, "budget", "rows", sheetBudgetRows+`,`+sheetCurrencyFormat)
	s.unloadIdleSheets()
	if s.liveSheet("budget") != in {
		t.Fatal("a sheet in use was unloaded")
	}
	// The sheet notes on its own tick that nobody uses it.
	awaitSheetIdle(t, in)
	offset.Store(int64(sheetIdleUnload - time.Minute))
	s.unloadIdleSheets()
	if s.liveSheet("budget") != in {
		t.Fatal("the sheet was unloaded before the idle time passed")
	}
	offset.Store(int64(sheetIdleUnload + time.Minute))
	// One pass is enough unless the flat lock is taken at that moment.
	for deadline := time.Now().Add(sheetTestWait); ; time.Sleep(10 * time.Millisecond) {
		if s.unloadIdleSheets(); s.liveSheet("budget") == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the idle sheet stayed loaded")
		}
	}
	if view, err := s.GetFlat(ctx, "budget"); err != nil || !view.Running {
		t.Fatalf("an unloaded sheet does not count as running: %+v, %v", view, err)
	}
	applied := s.sheetMetrics().Batches["applied"]

	// The next request loads it again with its content and history.
	if got := sheetRead(t, s, "budget", 0, `{"tab":"Sheet1","range":"B4"}`)["rows"].([]any)[0].([]any)[0].(map[string]any)["value"]; got != float64(2100) {
		t.Fatalf("total after the reload = %v", got)
	}
	reloaded := s.liveSheet("budget")
	if reloaded == nil || reloaded == in || reloaded.StoreID() != in.StoreID() {
		t.Fatal("the read did not load the sheet again")
	}
	if shown := sheetRead(t, s, "budget", 0, `{"tab":"Sheet1","range":"B4","include":["shown"]}`)["shown"].([]any)[0].([]any)[0]; shown != "$2,100.00" {
		t.Fatalf("the reload lost the number format: %v", shown)
	}
	out, _ := writeSheet(t, s, "budget", "after", `{"op":"write","tab":"Sheet1","at":"B2","rows":[[1300]]}`)
	if out.Revision != 3 {
		t.Fatalf("write after the reload = %+v", out)
	}
	if got := s.sheetMetrics().Batches["applied"]; got < applied {
		t.Fatalf("the batch total went down across the reload: %d < %d", got, applied)
	}
	w := httptest.NewRecorder()
	s.siteHandler("budget", false).ServeHTTP(w, httptest.NewRequest("GET", "http://budget.test/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("page after the reload = %d", w.Code)
	}
}

// sheetWithPendingEdit makes a sheet with a revision quota of one whose
// only revision is 2, unpinned, and a person's closed edit group in its
// outbox. The host's periodic import of the sheet waits, so the entry is
// still pending when the test's call starts. Its import would record
// revision 3 and delete revision 2.
func sheetWithPendingEdit(t *testing.T) *Service {
	t.Helper()
	ctx := context.Background()
	s, _ := newTestService(t)
	offset := sheetClock(s)
	if _, err := s.UpdateSettings(ctx, map[string]string{SetRevisionQuota: "1"}); err != nil {
		t.Fatal(err)
	}
	in := newSheet(t, s, "budget")
	writeSheet(t, s, "budget", "rows", sheetBudgetRows)
	s.sheetRetry.Store("budget", sheetBackoff{next: time.Now().Add(time.Hour)})
	browser := dialSheet(t, s, "budget")
	if got := browser.set("Sheet1", "Hall"); got != "applied" {
		t.Fatalf("the person's edit: %s", got)
	}
	offset.Add(int64(time.Minute))
	st, err := s.st.ContentState(ctx, "budget")
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(sheetTestWait); ; time.Sleep(20 * time.Millisecond) {
		entries, err := in.Outbox(ctx, st.ImportSeq, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the person's edit group never closed")
		}
	}
	if got := storedRevisions(t, s, "budget"); !slices.Equal(got, []int{2}) {
		t.Fatalf("revisions before the call = %v, want only 2 with the person's edit pending", got)
	}
	return s
}

// TestSheetRollbackKeepsItsTargetThroughItsImport rolls back to revision 2
// of sheetWithPendingEdit. The rollback imports the pending entry before
// it restores 2, and holds 2 first, so that import's retention keeps it.
// A rollback to the pending entry's own revision imports it before it
// holds it.
func TestSheetRollbackKeepsItsTargetThroughItsImport(t *testing.T) {
	for _, tc := range []struct {
		target int
		a1     string
	}{{2, "[[Item]]"}, {3, "[[Hall]]"}} {
		t.Run(fmt.Sprint("revision=", tc.target), func(t *testing.T) {
			ctx := context.Background()
			s := sheetWithPendingEdit(t)
			files, err := s.st.RevisionFiles(ctx, "budget", 2)
			if err != nil {
				t.Fatal(err)
			}
			res, err := s.RollbackRevisionResult(ctx, "budget", tc.target, "agent", ViaMCP)
			if err != nil || res.Revision != 4 || res.Warning != "" {
				t.Fatalf("rollback to revision %d = %+v, %v, want revision 4", tc.target, res, err)
			}
			r, restored, err := s.GetRevision(ctx, "budget", 4)
			if err != nil || r.Kind != "rollback" || r.RollbackOf != tc.target {
				t.Fatalf("rollback revision = %+v, %v", r, err)
			}
			if tc.target == 2 && !slices.Equal(restored, files) {
				t.Fatal("the rollback revision does not share the files of its target")
			}
			if got := fmt.Sprint(sheetRead(t, s, "budget", 0, `{"tab":"Sheet1","range":"A1"}`)["rows"]); got != tc.a1 {
				t.Fatalf("A1 after the rollback = %s, want %s", got, tc.a1)
			}
			if holds, err := s.st.Holds(ctx, "budget"); err != nil || len(holds) != 0 {
				t.Fatalf("holds after the rollback = %+v, %v", holds, err)
			}
			if pins, err := s.st.CountPins(ctx, "budget"); err != nil || pins != 0 {
				t.Fatalf("the rollback left %d pins, %v", pins, err)
			}
		})
	}
}

// TestSheetRevisionReadsKeepTheirTargetThroughTheirImport reads and pins
// revision 2 of sheetWithPendingEdit on every route that imports first.
// Each call's import records the person's edit, and the revision the call
// is about survives it.
func TestSheetRevisionReadsKeepTheirTargetThroughTheirImport(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		call func(*Service) error
	}{
		{"get_revision", func(s *Service) error { _, _, err := s.GetRevision(ctx, "budget", 2); return err }},
		{"sheet read of a revision", func(s *Service) error {
			_, err := s.GetSheet(ctx, "budget", 2, []byte(`{"tab":"Sheet1","range":"A1"}`))
			return err
		}},
		{"history view in the browser", func(s *Service) error {
			_, _, release, err := sheetBackend{s: s, slug: "budget"}.RevisionFiles(ctx, 2)
			release()
			return err
		}},
		{"pin", func(s *Service) error { return s.PinRevision(ctx, "budget", 2, true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := sheetWithPendingEdit(t)
			if err := tc.call(s); err != nil {
				t.Fatalf("%s of revision 2: %v", tc.name, err)
			}
			if got := storedRevisions(t, s, "budget"); !slices.Contains(got, 3) {
				t.Fatalf("revisions after the call = %v, want the person's edit imported", got)
			}
			if holds, err := s.st.Holds(ctx, "budget"); err != nil || len(holds) != 0 {
				t.Fatalf("holds after the call = %+v, %v", holds, err)
			}
			if n := syncMapLen(&s.sheetRollbacks); n != 0 {
				t.Fatalf("%d holds stay registered after the call", n)
			}
		})
	}
}

func syncMapLen(m *sync.Map) int {
	n := 0
	m.Range(func(_, _ any) bool { n++; return true })
	return n
}
