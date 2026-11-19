package formula

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const referenceDir = "testdata/reference"

// referenceRows is the number of cases the technical design, section 9.6,
// and the README of the table state.
const referenceRows = 2521

// knownFailures lists rows that cannot pass without contradicting another
// row or the spec. It is empty: every row of the table passes.
var knownFailures = map[string]string{}

type refRow struct {
	File    string
	ID      string                                `json:"id"`
	Fn      string                                `json:"fn"`
	Class   string                                `json:"class"`
	Date    string                                `json:"date"`
	Cells   map[string]json.RawMessage            `json:"cells"`
	Tabs    map[string]map[string]json.RawMessage `json:"tabs"`
	Formula string                                `json:"formula"`
	At      string                                `json:"at"`
	Format  json.RawMessage                       `json:"format"`
	Value   json.RawMessage                       `json:"value"`
	Shown   string                                `json:"shown"`
	CSV     string                                `json:"csv"`
	Note    string                                `json:"note"`
	Differs string                                `json:"differs"`
	Typed   *string                               `json:"typed"`
	Sets    json.RawMessage                       `json:"sets"`
}

func loadReference(t testing.TB) []refRow {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(referenceDir, "*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no reference files in %s: %v", referenceDir, err)
	}
	sort.Strings(files)
	var rows []refRow
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for line := 1; sc.Scan(); line++ {
			row := refRow{File: filepath.Base(file)}
			dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&row); err != nil {
				t.Fatalf("%s:%d: %v", file, line, err)
			}
			rows = append(rows, row)
		}
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	return rows
}

// The test sheet is a small engine: tabs of cells, formulas calculated on
// demand, and #CYCLE! for every formula on a cycle of static references.

type testCell struct {
	content Content
	f       *Formula
	done    bool
	val     Value
}

// showKind is the content kind Show takes for the cell: a formula whose
// outermost function gives a date shows as one.
func (c *testCell) showKind() ContentKind {
	switch {
	case c.f == nil:
		return c.content.Kind
	case c.f.DateResult():
		return ContentDate
	}
	return ContentFormula
}

type testTab struct {
	name       string
	cells      map[Pos]*testCell
	rows, cols int32
}

type cellKey struct {
	tab TabRef
	pos Pos
}

type testSheet struct {
	tabs   []*testTab
	today  Date
	cyclic map[cellKey]bool
	budget *Budget
}

func (s *testSheet) Tab(name string) (TabRef, bool) {
	key := FoldName(name)
	for i, tab := range s.tabs {
		if FoldName(tab.name) == key {
			return TabRef(i), true
		}
	}
	return 0, false
}

func (s *testSheet) Dims(tab TabRef) (rows, cols int32) {
	return s.tabs[tab].rows, s.tabs[tab].cols
}

func (s *testSheet) Filled(tab TabRef, p Pos) bool {
	_, ok := s.tabs[tab].cells[p]
	return ok
}

func (s *testSheet) Value(tab TabRef, p Pos) Value {
	c := s.tabs[tab].cells[p]
	switch {
	case c == nil:
		return Value{}
	case c.f == nil:
		return c.content.Value()
	case c.done:
		return c.val
	}
	c.done = true // a formula on a cycle is never evaluated, so this is not read early
	if s.cyclic[cellKey{tab, p}] {
		c.val = ErrorValue(ErrCycle)
	} else {
		c.val = c.f.Eval(&Env{Grid: s, Tab: tab, At: p, Today: s.today, Budget: s.budget})
	}
	return c.val
}

// findCycles marks every formula that reaches itself through static
// references.
func (s *testSheet) findCycles() {
	type edgeList []cellKey
	edges := map[cellKey]edgeList{}
	var formulas []cellKey
	for ti, tab := range s.tabs {
		for p, c := range tab.cells {
			if c.f != nil {
				formulas = append(formulas, cellKey{TabRef(ti), p})
			}
		}
	}
	for _, k := range formulas {
		c := s.tabs[k.tab].cells[k.pos]
		c.f.Refs(k.pos, func(r Ref) {
			target := k.tab
			if r.Tab != "" {
				var ok bool
				if target, ok = s.Tab(r.Tab); !ok {
					return
				}
			}
			for _, q := range formulas {
				if q.tab == target && r.Contains(q.pos) {
					edges[k] = append(edges[k], q)
				}
			}
		})
	}
	s.cyclic = map[cellKey]bool{}
	for _, start := range formulas {
		seen := map[cellKey]bool{}
		stack := append(edgeList(nil), edges[start]...)
		for len(stack) > 0 {
			q := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if q == start {
				s.cyclic[start] = true
				break
			}
			if !seen[q] {
				seen[q] = true
				stack = append(stack, edges[q]...)
			}
		}
	}
}

func parseA1Cell(t testing.TB, a1 string) Pos {
	t.Helper()
	r, err := ParseRange(a1)
	if err != nil || !r.Single() || r.Tab != "" {
		t.Fatalf("%q is no cell address", a1)
	}
	return r.First
}

func clock(t testing.TB, s string) float64 {
	t.Helper()
	var h, m, sec int
	if _, err := fmt.Sscanf(s, "%d:%d:%d", &h, &m, &sec); err != nil {
		t.Fatalf("time %q: %v", s, err)
	}
	return float64(h*3600+m*60+sec) / 86400
}

func day(t testing.TB, s string) float64 {
	t.Helper()
	d, ok := ParseDate(s)
	if !ok {
		t.Fatalf("date %q", s)
	}
	return float64(d)
}

// cellFromJSON reads a cell by the JSON typing of the write operation
// (spec rule 43).
func cellFromJSON(t testing.TB, raw json.RawMessage, at Pos) *testCell {
	t.Helper()
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	formula := func(text string) *testCell {
		f, err := Parse(text, at, Limits{})
		if err != nil {
			t.Fatalf("Parse(%q): %v", text, err)
		}
		return &testCell{f: f}
	}
	switch v := v.(type) {
	case nil:
		return nil
	case bool:
		c := Content{Kind: ContentBool}
		if v {
			c.Num = 1
		}
		return &testCell{content: c}
	case json.Number:
		x, err := v.Float64()
		if err != nil {
			t.Fatal(err)
		}
		return &testCell{content: Content{Kind: ContentNumber, Num: x}}
	case string:
		switch {
		case strings.HasPrefix(v, "=") && len(v) > 1:
			return formula(v)
		case strings.HasPrefix(v, "'"):
			return &testCell{content: Content{Kind: ContentText, Text: v[1:]}}
		}
		return &testCell{content: Content{Kind: ContentText, Text: v}}
	case map[string]any:
		if text, ok := v["formula"].(string); ok {
			for k := range v {
				if k != "formula" && k != "value" && k != "error" {
					t.Fatalf("formula object with the field %q", k)
				}
			}
			return formula(text)
		}
		if len(v) == 1 {
			for k, s := range v {
				s, _ := s.(string)
				switch k {
				case "date":
					return &testCell{content: Content{Kind: ContentDate, Num: day(t, s)}}
				case "time":
					return &testCell{content: Content{Kind: ContentTime, Num: clock(t, s)}}
				case "datetime":
					d, c, _ := strings.Cut(s, "T")
					return &testCell{content: Content{Kind: ContentDateTime, Num: day(t, d) + clock(t, c)}}
				}
			}
		}
	}
	t.Fatalf("cell value %s", raw)
	return nil
}

// newTestSheet builds the tab Sheet1 from cells, and the other tabs, with
// the JSON typing of the write operation. Call measure when every cell is
// in place.
func newTestSheet(t testing.TB, cells map[string]json.RawMessage, tabs map[string]map[string]json.RawMessage, today Date) *testSheet {
	t.Helper()
	s := &testSheet{today: today, budget: NewBudget(nil)}
	addTab := func(name string, cells map[string]json.RawMessage) {
		tab := &testTab{name: name, cells: map[Pos]*testCell{}}
		for a1, raw := range cells {
			p := parseA1Cell(t, a1)
			if c := cellFromJSON(t, raw, p); c != nil {
				tab.cells[p] = c
			}
		}
		s.tabs = append(s.tabs, tab)
	}
	addTab("Sheet1", cells)
	names := make([]string, 0, len(tabs))
	for name := range tabs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		addTab(name, tabs[name])
	}
	return s
}

// measure gives every tab the size of a new tab, or more when a cell lies
// past it, and finds the cycles.
func (s *testSheet) measure() {
	for _, tab := range s.tabs {
		tab.rows, tab.cols = 1000, 26
		for p := range tab.cells {
			tab.rows, tab.cols = max(tab.rows, p.Row+1), max(tab.cols, p.Col+1)
		}
	}
	s.findCycles()
}

// evalIn writes a formula into a cell of Sheet1 and calculates the sheet.
func evalIn(t testing.TB, row *refRow, formula string, today Date) *testCell {
	t.Helper()
	s := newTestSheet(t, row.Cells, row.Tabs, today)
	at := Pos{Row: 0, Col: 25}
	if row.At != "" {
		at = parseA1Cell(t, row.At)
	}
	f, err := Parse(formula, at, Limits{})
	if err != nil {
		t.Fatalf("Parse(%q): %v", formula, err)
	}
	cell := &testCell{f: f}
	s.tabs[0].cells[at] = cell
	s.measure()
	s.Value(0, at)
	return cell
}

// digits15 rounds a number to 15 significant digits, half away from zero,
// from 41 correctly rounded digits. It shares no code with the engine.
func digits15(x float64) string {
	if x == 0 {
		return "0"
	}
	s := strconv.FormatFloat(x, 'e', 40, 64)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	mant, exp, _ := strings.Cut(s, "e")
	d := []byte(strings.Replace(mant, ".", "", 1))
	e, _ := strconv.Atoi(exp)
	up := d[15] >= '5'
	d = d[:15]
	for i := 14; up && i >= 0; i-- {
		if d[i] == '9' {
			d[i] = '0'
			continue
		}
		d[i]++
		up = false
	}
	if up {
		d = append([]byte{'1'}, d[:14]...)
		e++
	}
	out := string(d) + "e" + strconv.Itoa(e)
	if neg {
		out = "-" + out
	}
	return out
}

func agree15(a, b float64) bool { return digits15(a) == digits15(b) }

// jsonOf returns a value in the typing of get_sheet's value and error
// fields.
func jsonOf(v Value) any {
	switch v.Kind() {
	case KindNumber:
		return v.Number()
	case KindText:
		return v.Text()
	case KindBool:
		return v.Bool()
	case KindError:
		return map[string]any{"error": v.Err().String()}
	}
	return nil
}

// sameJSON compares an expected JSON value with a calculated one: numbers
// at 15 significant digits, everything else exactly.
func sameJSON(t testing.TB, want json.RawMessage, got any) bool {
	t.Helper()
	var w any
	dec := json.NewDecoder(bytes.NewReader(want))
	dec.UseNumber()
	if err := dec.Decode(&w); err != nil {
		t.Fatal(err)
	}
	switch w := w.(type) {
	case json.Number:
		g, ok := got.(float64)
		x, err := w.Float64()
		return ok && err == nil && agree15(x, g)
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for k, v := range w {
			if g[k] != v {
				return false
			}
		}
		return true
	}
	return w == got
}

func formatOf(t testing.TB, raw json.RawMessage) *NumberFormat {
	t.Helper()
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var nf NumberFormat
	if err := json.Unmarshal(raw, &nf); err != nil {
		t.Fatalf("number format %s: %v", raw, err)
	}
	return &nf
}

type refResult struct {
	value any
	shown string
	csv   string
	sets  *NumberFormat
}

func formulaResult(t testing.TB, row *refRow, formula string, today Date, nf *NumberFormat) refResult {
	t.Helper()
	cell := evalIn(t, row, formula, today)
	var format NumberFormat
	if nf != nil {
		format = *nf
	}
	shown := Show(cell.val, cell.showKind(), format)
	return refResult{value: jsonOf(cell.val), shown: shown, csv: string(AppendCSVField(nil, cell.val, cell.showKind(), format))}
}

func typedResult(t testing.TB, typed string) refResult {
	t.Helper()
	content, sets := ParseTyped(typed)
	if content.Kind == ContentFormula {
		today, _ := ParseDate("2026-11-13")
		res := formulaResult(t, &refRow{}, content.Text, today, nil)
		return res
	}
	var format NumberFormat
	if sets != nil {
		format = *sets
	}
	shown := Show(content.Value(), content.Kind, format)
	res := refResult{shown: shown, csv: string(AppendCSVField(nil, content.Value(), content.Kind, format)), sets: sets}
	iso := NumberFormat{Kind: NumberDate}
	hms := NumberFormat{Kind: NumberTime, Time: TimeHMS}
	switch content.Kind {
	case ContentDate:
		res.value = map[string]any{"date": Show(content.Value(), content.Kind, iso)}
	case ContentTime:
		res.value = map[string]any{"time": Show(content.Value(), content.Kind, hms)}
	case ContentDateTime:
		res.value = map[string]any{"datetime": Show(content.Value(), content.Kind, iso) + "T" + Show(content.Value(), content.Kind, hms)}
	case ContentText:
		res.value = content.Text
		if strings.HasPrefix(content.Text, "=") || strings.HasPrefix(content.Text, "'") {
			res.value = "'" + content.Text // as a read returns such text
		}
	default:
		res.value = jsonOf(content.Value())
	}
	return res
}

func TestReferenceTable(t *testing.T) {
	rows := loadReference(t)
	if len(rows) != referenceRows {
		t.Errorf("the table has %d rows, want %d", len(rows), referenceRows)
	}
	passed, excluded := 0, 0
	for i := range rows {
		row := &rows[i]
		var res refResult
		var wantSets *NumberFormat
		if row.Typed != nil {
			res = typedResult(t, *row.Typed)
			wantSets = formatOf(t, row.Sets)
		} else {
			today, ok := ParseDate(row.Date)
			if !ok {
				t.Fatalf("%s: date %q", row.ID, row.Date)
			}
			res = formulaResult(t, row, row.Formula, today, formatOf(t, row.Format))
		}
		var diffs []string
		if !sameJSON(t, row.Value, res.value) {
			diffs = append(diffs, fmt.Sprintf("value = %#v, want %s", res.value, row.Value))
		}
		if res.shown != row.Shown {
			diffs = append(diffs, fmt.Sprintf("shown = %q, want %q", res.shown, row.Shown))
		}
		if res.csv != row.CSV {
			diffs = append(diffs, fmt.Sprintf("csv = %q, want %q", res.csv, row.CSV))
		}
		if row.Typed != nil && !(wantSets == nil && res.sets == nil || wantSets != nil && res.sets != nil && *wantSets == *res.sets) {
			diffs = append(diffs, fmt.Sprintf("sets = %+v, want %s", res.sets, row.Sets))
		}
		reason, skip := knownFailures[row.ID]
		switch {
		case skip && len(diffs) == 0:
			t.Errorf("%s passes and is still listed in knownFailures (%s)", row.ID, reason)
		case skip:
			excluded++
			t.Logf("%s is excluded: %s", row.ID, reason)
		case len(diffs) > 0:
			text := row.Formula
			if row.Typed != nil {
				text = *row.Typed
			}
			t.Errorf("%s %s (%s)\n    %s", row.ID, text, row.Note, strings.Join(diffs, "\n    "))
		default:
			passed++
		}
	}
	t.Logf("reference rows passing: %d of %d (%d excluded)", passed, len(rows), excluded)
}

// TestReferenceCoverage enforces the minimum cases of the technical design,
// section 9.6: every function of rule 12 has at least 8 cases, among them a
// normal case, an error case and a call with a number of arguments the
// function does not take, and every row the README names exists.
func TestReferenceCoverage(t *testing.T) {
	rows := loadReference(t)
	type cover struct {
		rows                 int
		normal, errors, args bool
	}
	covered := map[string]*cover{}
	ids := map[string]bool{}
	perFile := map[string]int{}
	for i := range rows {
		row := &rows[i]
		if ids[row.ID] {
			t.Errorf("the id %s is used twice", row.ID)
		}
		ids[row.ID] = true
		perFile[row.File]++
		c := covered[row.Fn]
		if c == nil {
			c = &cover{}
			covered[row.Fn] = c
		}
		c.rows++
		c.normal = c.normal || row.Class == "normal"
		c.errors = c.errors || row.Class == "error"
		c.args = c.args || (strings.Contains(row.Note, "number of arguments") && string(row.Value) == `{"error":"#ERROR!"}`)
		switch row.Class {
		case "normal", "boundary", "error", "coercion":
		default:
			t.Errorf("%s: class %q", row.ID, row.Class)
		}
	}
	functions := Functions()
	if len(functions) != 104 {
		t.Fatalf("Functions() has %d functions, want the 104 of rule 12", len(functions))
	}
	groupFile := map[string]string{
		"Math": "math.jsonl", "Statistics": "statistics.jsonl", "Conditional totals": "conditional.jsonl",
		"Logic": "logic.jsonl", "Text": "text.jsonl", "Lookup": "lookup.jsonl", "Date and time": "date.jsonl",
	}
	for _, fn := range functions {
		c := covered[fn.Name]
		switch {
		case c == nil:
			t.Errorf("%s has no case", fn.Name)
			continue
		case c.rows < 8:
			t.Errorf("%s has %d cases, want at least 8", fn.Name, c.rows)
		}
		if !c.normal {
			t.Errorf("%s has no normal case", fn.Name)
		}
		if !c.errors {
			t.Errorf("%s has no error case", fn.Name)
		}
		if !c.args {
			t.Errorf("%s has no case with a number of arguments it does not take", fn.Name)
		}
		if groupFile[fn.Group] == "" {
			t.Errorf("%s has the group %q", fn.Name, fn.Group)
		}
	}
	for i := range rows {
		row := &rows[i]
		switch row.Fn {
		case "operators":
			if row.File != "operators.jsonl" {
				t.Errorf("%s is in %s", row.ID, row.File)
			}
		case "typed":
			if row.File != "typed.jsonl" {
				t.Errorf("%s is in %s", row.ID, row.File)
			}
		default:
			fn, ok := lookupFunc(row.Fn)
			if !ok {
				t.Errorf("%s: %s is no function of rule 12", row.ID, row.Fn)
			} else if want := groupFile[funcTable[fn].group]; row.File != want {
				t.Errorf("%s is in %s, want %s", row.ID, row.File, want)
			}
		}
		if !strings.HasPrefix(row.ID, strings.ToLower(row.Fn)+"-") {
			t.Errorf("%s: the id does not start with the function %s", row.ID, row.Fn)
		}
	}

	readme, err := os.ReadFile(filepath.Join(referenceDir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	// The file table of the README states the rows of each file.
	for _, m := range regexp.MustCompile("(?m)^\\| `([a-z]+\\.jsonl)` \\| ([0-9]+) \\|").FindAllStringSubmatch(string(readme), -1) {
		if want, _ := strconv.Atoi(m[2]); perFile[m[1]] != want {
			t.Errorf("%s has %d rows, the README says %d", m[1], perFile[m[1]], want)
		}
	}
	// Every row id the README names, alone or as the ends of a run, exists.
	_, chosen, ok := strings.Cut(string(readme), "## Chosen behaviors")
	if !ok {
		t.Fatal("the README has no Chosen behaviors section")
	}
	idRE := regexp.MustCompile("`([a-z0-9]+)-([0-9]{3})`( to `([a-z0-9]+)-([0-9]{3})`)?")
	named := 0
	for _, m := range idRE.FindAllStringSubmatch(chosen, -1) {
		first, _ := strconv.Atoi(m[2])
		last := first
		if m[3] != "" {
			if m[4] != m[1] {
				t.Errorf("the README names a run from %s to %s", m[1], m[4])
				continue
			}
			last, _ = strconv.Atoi(m[5])
		}
		for n := first; n <= last; n++ {
			named++
			if id := fmt.Sprintf("%s-%03d", m[1], n); !ids[id] {
				t.Errorf("the README names the row %s, which the table does not have", id)
			}
		}
	}
	if named < 200 {
		t.Errorf("found only %d row ids in the README's Chosen behaviors", named)
	}
}

// findPython3 returns the path of a Python 3 interpreter, or what it tried.
// The interpreter is "python3" or "python" on the path, whichever answers
// that its major version is 3: a system may have only one of the names,
// "python" may be Python 2, and on Windows "python3" may be a stub that
// opens the app store and fails.
func findPython3() (path, tried string) {
	var notes []string
	for _, name := range []string{"python3", "python"} {
		path, err := exec.LookPath(name)
		if err != nil {
			notes = append(notes, name+" is not on the path")
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		out, err := exec.CommandContext(ctx, path, "-c", "import sys; print(sys.version_info[0])").Output()
		cancel()
		if err == nil && strings.TrimSpace(string(out)) == "3" {
			return path, ""
		}
		notes = append(notes, fmt.Sprintf("%s does not answer as Python 3 (%v, %q)", path, err, strings.TrimSpace(string(out))))
	}
	return "", strings.Join(notes, "; ")
}

// TestReferenceChecker runs the independent checker that is kept beside
// the table. The checker is the second opinion on every row, so it has to
// run somewhere: in CI a system without Python 3 fails the test, except
// Windows, where the test is skipped with what it tried and the other
// systems run the checker. Outside CI the test is skipped without Python 3.
func TestReferenceChecker(t *testing.T) {
	python, tried := findPython3()
	if python == "" {
		switch {
		case runtime.GOOS == "windows":
			t.Skipf("no Python 3 on this Windows system, so the reference table checker does not run here: %s", tried)
		case os.Getenv("CI") != "":
			t.Fatalf("no Python 3, so the reference table checker cannot run in CI: %s", tried)
		}
		t.Skipf("no Python 3: %s", tried)
	}
	// go test caches a pass by the files this process opens; python's reads
	// are invisible to it, so read the checker's inputs here too.
	files, err := filepath.Glob(filepath.Join(referenceDir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if _, err := os.ReadFile(file); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(python, filepath.Join(referenceDir, "check_reference.py"))
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1") // no encoding of the locale, on any system
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s check_reference.py: %v\n%s", python, err, out)
	}
	if !bytes.Contains(out, []byte("RESULT: OK")) {
		t.Fatalf("check_reference.py did not report OK:\n%s", out)
	}
}
