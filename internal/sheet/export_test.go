package sheet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadExport writes the export fixture of the formula package into a sheet
// through agent operations, as a real caller would.
func loadExport(t *testing.T) (*env, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("formula", "testdata", "export", "cells.json"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("formula", "testdata", "export", "expected.csv"))
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Tab     string              `json:"tab"`
		Date    string              `json:"date"`
		Rows    [][]json.RawMessage `json:"rows"`
		Formats []struct {
			Range  string          `json:"range"`
			Format json.RawMessage `json:"format"`
		} `json:"formats"`
		Widths []struct {
			Cols  string `json:"cols"`
			Width int    `json:"width"`
		} `json:"widths"`
	}
	if err := strictJSON(raw, &fx); err != nil {
		t.Fatal(err)
	}
	e := start(t)
	if got := e.clock.Now().Format("2006-01-02"); got != fx.Date {
		t.Fatalf("the fixture is for %s and the test clock says %s", fx.Date, got)
	}
	rows, _ := json.Marshal(fx.Rows)
	ops := []string{fmt.Sprintf(`{"op":"write","tab":%q,"at":"A1","rows":%s}`, fx.Tab, rows)}
	for _, f := range fx.Formats {
		ops = append(ops, fmt.Sprintf(`{"op":"format","tab":%q,"ranges":[%q],"format":%s}`, fx.Tab, f.Range, f.Format))
	}
	for _, w := range fx.Widths {
		ops = append(ops, fmt.Sprintf(`{"op":"widths","tab":%q,"cols":%q,"width":%d}`, fx.Tab, w.Cols, w.Width))
	}
	e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
	return e, want
}

// TestExportBytes covers the export cases E-01 to E-86: the CSV of the
// fixture tab is the exact expected bytes, as get_sheet returns it in pages
// and as the download writes it after the byte order mark.
func TestExportBytes(t *testing.T) {
	e, want := loadExport(t)
	var got strings.Builder
	q := ReadQuery{Tab: "Sheet1", Format: "csv", Limit: 40}
	pages := 0
	for {
		out := e.read(q)
		got.WriteString(out["csv"].(string))
		pages++
		next, ok := out["next"].(map[string]any)
		if !ok {
			break
		}
		q = ReadQuery{Cursor: next["cursor"].(string)}
	}
	if got.String() != string(want) {
		t.Errorf("get_sheet csv over %d pages differs from expected.csv:\n%s", pages, firstDiff(got.String(), string(want)))
	}
	if strings.HasPrefix(got.String(), csvBOM) {
		t.Error("E-86: get_sheet returns the byte order mark")
	}
	if pages < 2 {
		t.Errorf("the fixture was read in %d page", pages)
	}

	// E-86: the download starts with the mark and holds the same bytes.
	for _, access := range []string{"private", ""} {
		r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/_sheet/csv?tab=%d", e.tabID("Sheet1")), nil)
		if access != "" {
			r.Header.Set("X-Flats-Access", access)
		}
		w := httptest.NewRecorder()
		e.in.ServeHTTP(w, r)
		body := w.Body.Bytes()
		if w.Code != http.StatusOK || !bytes.HasPrefix(body, []byte(csvBOM)) {
			t.Fatalf("download: status %d, starts with %x", w.Code, body[:min(3, len(body))])
		}
		if !bytes.Equal(body[len(csvBOM):], want) {
			t.Errorf("download (%q access) differs from expected.csv:\n%s", access, firstDiff(string(body[len(csvBOM):]), string(want)))
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
			t.Errorf("content type %q", ct)
		}
	}
}

func firstDiff(got, want string) string {
	gl, wl := strings.Split(got, "\r\n"), strings.Split(want, "\r\n")
	for i := 0; i < len(gl) || i < len(wl); i++ {
		var g, w string
		if i < len(gl) {
			g = gl[i]
		}
		if i < len(wl) {
			w = wl[i]
		}
		if g != w {
			return fmt.Sprintf("line %d:\n got  %q\n want %q", i+1, g, w)
		}
	}
	return "same lines"
}

// TestReadWriteBack is contract case C-11: every value a read returns can
// be written back unchanged.
func TestReadWriteBack(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[
		["'=not a formula","''tis","=B9/2","=1/0","=SUM(B3",{"date":"2026-03-15"},{"time":"14:30:05"},{"datetime":"2026-03-15T14:30:00"},true,null,12.5,"plain"]]}]}`)
	before := e.read(ReadQuery{Tab: "Sheet1", Range: "A1:L1"})
	rows, _ := json.Marshal(before["rows"])
	want := `[["'=not a formula","''tis",{"formula":"=B9/2","value":0},{"error":"#DIV/0!","formula":"=1/0"},{"error":"#ERROR!","formula":"=SUM(B3"},{"date":"2026-03-15"},{"time":"14:30:05"},{"datetime":"2026-03-15T14:30:00"},true,null,12.5,"plain"]]`
	if string(rows) != want {
		t.Fatalf("read = %s\nwant   %s", rows, want)
	}
	e.wantText("A1", "'=not a formula")
	e.want("A1", "=not a formula")
	e.want("B1", "'tis")
	e.mustAgent(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A1:L1"]},{"op":"write","tab":"Sheet1","at":"A1","rows":` + string(rows) + `}]}`)
	after := e.read(ReadQuery{Tab: "Sheet1", Range: "A1:L1"})
	again, _ := json.Marshal(after["rows"])
	if string(again) != string(rows) {
		t.Fatalf("after writing back = %s", again)
	}
}

// TestReferenceTableRoundTrip is the second half of C-11 and the get_sheet
// and CSV projections of the Formulas criterion: every row of the reference
// table gives its value, shown text and CSV field through the engine, and
// reads back unchanged.
func TestReferenceTableRoundTrip(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("formula", "testdata", "reference", "*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("reference table: %v", err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "typed.jsonl") {
			continue
		}
		t.Run(strings.TrimSuffix(filepath.Base(file), ".jsonl"), func(t *testing.T) {
			if (testing.Short() || raceEnabled) && !strings.Contains(file, "logic") {
				t.Skip("one file of the table in short mode and under the race detector")
			}
			runReferenceFile(t, file)
		})
	}
}

type referenceCase struct {
	ID      string                                `json:"id"`
	Date    string                                `json:"date"`
	Cells   map[string]json.RawMessage            `json:"cells"`
	Tabs    map[string]map[string]json.RawMessage `json:"tabs"`
	Formula string                                `json:"formula"`
	At      string                                `json:"at"`
	Format  json.RawMessage                       `json:"format"`
	Value   json.RawMessage                       `json:"value"`
	Shown   string                                `json:"shown"`
	CSV     string                                `json:"csv"`
}

func runReferenceFile(t *testing.T, file string) {
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	e := start(t)
	failed := 0
	for n, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var c referenceCase
		if err := json.Unmarshal(line, &c); err != nil {
			t.Fatalf("line %d: %v", n+1, err)
		}
		if c.At == "" {
			c.At = "Z1"
		}
		if n%50 == 49 {
			e.b.importAll(t)
		}
		// Each case runs on its own date and on new tabs, which replace
		// the tabs of the case before, so that cases do not meet.
		day, _ := parseDay(c.Date)
		e.clock.mu.Lock()
		e.clock.now = day
		e.clock.mu.Unlock()
		ops := []string{`{"op":"add_tab","name":"Sheet1"}`}
		write := func(tab string, cells map[string]json.RawMessage) {
			for addr, value := range cells {
				ops = append(ops, fmt.Sprintf(`{"op":"write","tab":%q,"at":%q,"rows":[[%s]]}`, tab, addr, value))
			}
		}
		write("Sheet1", c.Cells)
		for tab, cells := range c.Tabs {
			if tab != "Sheet1" {
				ops = append(ops, fmt.Sprintf(`{"op":"add_tab","name":%q}`, tab))
			}
			write(tab, cells)
		}
		formulaJSON, _ := json.Marshal(c.Formula)
		ops = append(ops, fmt.Sprintf(`{"op":"write","tab":"Sheet1","at":%q,"rows":[[%s]]}`, c.At, formulaJSON))
		if len(c.Format) > 0 && string(c.Format) != "null" {
			ops = append(ops, fmt.Sprintf(`{"op":"format","tab":"Sheet1","ranges":[%q],"format":{"number":%s}}`, c.At, c.Format))
		}
		names := e.tabNames()
		drop := make([]string, 0, len(names))
		for _, name := range names {
			drop = append(drop, fmt.Sprintf(`{"op":"rename_tab","tab":%q,"name":%q}`, name, "old "+name))
		}
		for _, name := range names {
			ops = append(ops, fmt.Sprintf(`{"op":"delete_tab","tab":%q}`, "old "+name))
		}
		if _, err := e.agent(`{"ops":[` + strings.Join(append(drop, ops...), ",") + `]}`); err != nil {
			t.Errorf("%s: write refused: %v", c.ID, err)
			failed++
			continue
		}
		out := e.read(ReadQuery{Tab: "Sheet1", Range: c.At, Include: []string{"shown"}})
		var cell map[string]json.RawMessage
		rows, _ := json.Marshal(out["rows"])
		var grid [][]json.RawMessage
		_ = json.Unmarshal(rows, &grid)
		ok := len(grid) == 1 && len(grid[0]) == 1 && json.Unmarshal(grid[0][0], &cell) == nil
		got := cell["value"]
		if errText, isErr := cell["error"]; isErr {
			got = json.RawMessage(`{"error":` + string(errText) + `}`)
		}
		shown := ""
		if s, _ := json.Marshal(out["shown"]); len(s) > 6 {
			var g [][]string
			_ = json.Unmarshal(s, &g)
			shown = g[0][0]
		}
		csv := e.read(ReadQuery{Tab: "Sheet1", Range: c.At, Format: "csv"})["csv"]
		if !ok || !sameJSON(got, c.Value) || shown != c.Shown || csv != c.CSV+"\r\n" {
			failed++
			if failed <= 10 {
				t.Errorf("%s %s: value %s shown %q csv %q, want %s %q %q", c.ID, c.Formula, got, shown, csv, c.Value, c.Shown, c.CSV)
			}
			continue
		}
		// C-11: the read value writes back to the same content.
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":%q,"rows":%s}]}`, c.At, rows))
		if again, _ := json.Marshal(e.read(ReadQuery{Tab: "Sheet1", Range: c.At})["rows"]); string(again) != string(rows) {
			t.Errorf("%s: wrote %s back and read %s", c.ID, rows, again)
		}
	}
	if failed > 10 {
		t.Errorf("%d cases failed", failed)
	}
}
