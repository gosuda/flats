package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/store"
)

func sheetHost(t *testing.T) (*httptest.Server, *core.Service) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	priv, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := core.New(context.Background(), core.Config{DataDir: filepath.Join(dir, "data"), Store: st, Private: priv, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&api.Server{Svc: svc}).Handler())
	t.Cleanup(func() { srv.Close(); svc.Close(); priv.Close(); st.Close() })
	return srv, svc
}

func sheetOps(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ops.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSheetCommandsAgainstAHost(t *testing.T) {
	srv, svc := sheetHost(t)
	rows := `{"op":"write","tab":"Sheet1","at":"A1","rows":[["Item","Amount"],["Venue",1200],["Food, drinks",900],["Total","=SUM(B2:B3)"],["Bad","=B2/0"]]}`
	ops := sheetOps(t, `{"ops":[`+rows+`]}`)
	r := run(t, srv.URL, "", "sheet", "apply", "budget", ops, "--op-id", "first", "--message", "Budget")
	if r.code != 0 || !strings.Contains(r.stdout, "Created sheet budget: revision 2, 10 cells written, 2 formulas, 1 formula errors") ||
		!strings.Contains(r.stdout, "Sheet1!B5 shows #DIV/0!") || !strings.Contains(r.stdout, "state: s_") || !strings.Contains(r.stderr, "op_id: first") {
		t.Fatalf("apply: %+v", r)
	}
	// The apply that creates the sheet prints where it opens.
	if view, err := svc.GetFlat(t.Context(), "budget"); err != nil || view.PrivateURL == "" || !strings.Contains(r.stdout, "  private: "+view.PrivateURL+"\n") {
		t.Fatalf("the creating apply prints no address: %+v (%+v, %v)", r, view, err)
	}
	// After a timeout the same file with the same id applies nothing again.
	r = run(t, srv.URL, "", "sheet", "apply", "budget", ops, "--op-id", "first", "--message", "Budget")
	if r.code != 0 || !strings.Contains(r.stdout, "already applied") || !strings.Contains(r.stdout, "revision 2") || strings.Contains(r.stdout, "private:") {
		t.Fatalf("replay: %+v", r)
	}
	if page, err := svc.ListRevisions(t.Context(), "budget", 0, 10); err != nil || len(page.Revisions) != 2 || page.Revisions[0].Message != "Budget" {
		t.Fatalf("revisions = %+v, %v", page, err)
	}

	format := sheetOps(t, `{"ops":[{"op":"format","tab":"Sheet1","ranges":["B2:B4"],"format":{"number":{"kind":"currency","symbol":"$","decimals":2}}}]}`)
	r = run(t, srv.URL, "", "sheet", "apply", "budget", format)
	if r.code != ExitError || !strings.Contains(r.stderr, "state_required") {
		t.Fatalf("apply without state: %+v", r)
	}
	r = run(t, srv.URL, "", "--json", "sheet", "get", "budget")
	var first struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &first); r.code != 0 || err != nil || first.State == "" {
		t.Fatalf("state read: %+v, %v", r, err)
	}
	r = run(t, srv.URL, "", "sheet", "apply", "budget", format, "--state", first.State)
	if r.code != 0 || !strings.Contains(r.stdout, "Applied sheet budget: revision 3, 0 cells written") {
		t.Fatalf("format: %+v", r)
	}

	r = run(t, srv.URL, "", "sheet", "get", "budget")
	var tabs struct {
		State string           `json:"state"`
		Tabs  []map[string]any `json:"tabs"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &tabs); r.code != 0 || err != nil || tabs.State == "" || len(tabs.Tabs) != 1 || tabs.Tabs[0]["used"] != "A1:B5" {
		t.Fatalf("tab list: %+v, %v", r, err)
	}
	// Two cells a page: the command follows every cursor and prints one result.
	r = run(t, srv.URL, "", "--json", "sheet", "get", "budget", "--tab", "Sheet1", "--include", "shown,formats", "--limit", "3")
	var read struct {
		Range   string            `json:"range"`
		Start   string            `json:"start"`
		Rows    [][]any           `json:"rows"`
		Shown   [][]string        `json:"shown"`
		Formats []json.RawMessage `json:"formats"`
		Next    any               `json:"next"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &read); r.code != 0 || err != nil {
		t.Fatalf("paged read: %+v, %v", r, err)
	}
	if read.Range != "A1:B5" || read.Start != "A1" || read.Next != nil || len(read.Rows) != 5 || len(read.Shown) != 5 || len(read.Formats) == 0 {
		t.Fatalf("merged read = %+v", read)
	}
	for i, row := range read.Rows {
		if len(row) != 2 || len(read.Shown[i]) != 2 {
			t.Fatalf("row %d was not joined: %v / %v", i+1, row, read.Shown[i])
		}
	}
	if read.Shown[3][1] != "$2,100.00" || read.Rows[3][1].(map[string]any)["value"] != float64(2100) || read.Shown[4][1] != "#DIV/0!" {
		t.Fatalf("values = %v / %v", read.Rows, read.Shown)
	}

	// A CSV field is the cell's value, not the text its format shows (spec
	// rule 60): the amounts are written as plain numbers.
	want := "\xef\xbb\xbfItem,Amount\r\nVenue,1200\r\n\"Food, drinks\",900\r\nTotal,2100\r\nBad,#DIV/0!\r\n"
	r = run(t, srv.URL, "", "sheet", "get", "budget", "--tab", "Sheet1", "--format", "csv", "--limit", "3")
	if r.code != 0 || r.stdout != want {
		t.Fatalf("csv to standard output: %q", r.stdout)
	}
	file := filepath.Join(t.TempDir(), "budget.csv")
	r = run(t, srv.URL, "", "sheet", "get", "budget", "--tab", "Sheet1", "--format", "csv", "--output", file)
	if got, err := os.ReadFile(file); r.code != 0 || err != nil || string(got) != want || !strings.Contains(r.stdout, "Wrote "+file) {
		t.Fatalf("csv file: %+v, %v, %q", r, err, got)
	}

	next := sheetOps(t, `{"state":"stale","ops":[{"op":"write","tab":"Sheet1","at":"B2","rows":[[1500]]}]}`)
	r = run(t, srv.URL, "", "sheet", "apply", "budget", next)
	if r.code != ExitError || !strings.Contains(r.stderr, "state_unknown") {
		t.Fatalf("stale state: %+v", r)
	}
	r = run(t, srv.URL, "", "sheet", "apply", "budget", next, "--state", tabs.State)
	if r.code != 0 || !strings.Contains(r.stdout, "Applied sheet budget: revision 4, 1 cells written") {
		t.Fatalf("apply with --state: %+v", r)
	}
	r = run(t, srv.URL, "", "--json", "sheet", "get", "budget", "--tab", "Sheet1", "--range", "B2:B4", "--revision", "2")
	if r.code != 0 || !strings.Contains(r.stdout, `"revision":2`) || !strings.Contains(r.stdout, `[1200]`) {
		t.Fatalf("revision read: %+v", r)
	}
	r = run(t, srv.URL, "", "sheet", "get", "budget", "--tab", "Gone")
	if r.code != ExitError || !strings.Contains(r.stderr, "not_found") {
		t.Fatalf("missing tab: %+v", r)
	}
}

func TestSheetCommandAdmission(t *testing.T) {
	t.Parallel()
	ops := sheetOps(t, `{"ops":[]}`)
	for _, tt := range []struct {
		name string
		args []string
	}{
		{"missing subcommand", []string{"sheet"}},
		{"unknown subcommand", []string{"sheet", "import", "budget", "file.csv"}},
		{"missing slug", []string{"sheet", "get"}},
		{"limit", []string{"sheet", "get", "budget", "--tab", "Sheet1", "--limit", "0"}},
		{"range without tab", []string{"sheet", "get", "budget", "--range", "A1"}},
		{"repeated flag", []string{"sheet", "get", "budget", "--tab", "a", "--tab", "b"}},
		{"bad format", []string{"sheet", "get", "budget", "--tab", "Sheet1", "--format", "xlsx"}},
		{"output without csv", []string{"sheet", "get", "budget", "--tab", "Sheet1", "--output", "x.csv"}},
		{"missing file", []string{"sheet", "apply", "budget"}},
		{"array file", []string{"sheet", "apply", "budget", sheetOps(t, `[]`)}},
		{"slug in the file", []string{"sheet", "apply", "budget", sheetOps(t, `{"slug":"other","ops":[]}`)}},
		{"unknown operation", []string{"sheet", "apply", "budget", sheetOps(t, `{"ops":[{"op":"erase","tab":"Sheet1"}]}`)}},
		{"bad op id", []string{"sheet", "apply", "budget", ops, "--op-id", "has space"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			code := Run(context.Background(), tt.args, Env{Stdout: &out, Stderr: &errw, ConfigDir: t.TempDir()})
			if code != ExitUsage {
				t.Fatalf("code=%d out=%s err=%s", code, out.String(), errw.String())
			}
		})
	}
	if got := sheetColumn("AB12"); got != "AB" {
		t.Fatalf("column of AB12 = %q", got)
	}
}

// A page that ends inside a row continues that row on the next page.
func TestSheetGetJoinsARowSplitAcrossPages(t *testing.T) {
	t.Parallel()
	pages := map[string]string{
		"":   `{"state":"s_a.1","date":"2026-11-13","tab":"Wide","range":"A1:C2","start":"A1","rows":[[1,2]],"shown":[["1","2"]],"next":{"cursor":"c1","start":"C1"}}`,
		"c1": `{"state":"s_a.1","date":"2026-11-13","tab":"Wide","range":"A1:C2","start":"C1","rows":[[3],[4,5,6]],"shown":[["3"],["4","5","6"]]}`,
	}
	var cursors []string
	client := notebookFakeClient{invoke: func(_ context.Context, _, _ string, q url.Values, _ any) (string, error) {
		cursors = append(cursors, q.Get("cursor"))
		if q.Get("cursor") != "" && (len(q) != 2 || q.Get("revision") != "4") {
			t.Errorf("continuation query = %v", q)
		}
		return pages[q.Get("cursor")], nil
	}}
	a, out, _ := notebookTestApp()
	a.jsonOut = true
	if err := a.sheetGet(client, "wide", url.Values{"tab": {"Wide"}, "revision": {"4"}}, ""); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Rows  [][]int    `json:"rows"`
		Shown [][]string `json:"shown"`
		Next  any        `json:"next"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(cursors) != 2 || got.Next != nil || len(got.Rows) != 2 || len(got.Rows[0]) != 3 || got.Rows[0][2] != 3 || len(got.Rows[1]) != 3 || got.Shown[0][2] != "3" {
		t.Fatalf("merged pages = %s (cursors %v)", out.String(), cursors)
	}
	loop := notebookFakeClient{invoke: func(context.Context, string, string, url.Values, any) (string, error) {
		return `{"tab":"Wide","range":"A1:A2","start":"A1","rows":[[1]],"next":{"cursor":"same","start":"A2"}}`, nil
	}}
	a, _, _ = notebookTestApp()
	if err := a.sheetGet(loop, "wide", url.Values{"tab": {"Wide"}}, ""); err == nil || !strings.Contains(err.Error(), "repeated continuation cursor") {
		t.Fatalf("a repeated cursor = %v", err)
	}
}
