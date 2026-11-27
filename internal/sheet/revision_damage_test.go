package sheet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// withFile returns the files of a revision with the file at path holding
// data, which the backend then serves.
func (e *env) withFile(files []File, path string, data string) []File {
	e.t.Helper()
	sum := sha256.Sum256([]byte(data))
	hash := hex.EncodeToString(sum[:])
	e.b.mu.Lock()
	e.b.blobs[hash] = []byte(data)
	e.b.mu.Unlock()
	out := slices.Clone(files)
	found := false
	for i := range out {
		if out[i].Path == path {
			out[i].Hash, out[i].Size, found = hash, int64(len(data)), true
		}
	}
	if !found {
		e.t.Fatalf("the revision has no file %s", path)
	}
	return out
}

// addFile returns the files of a revision with one more file at path,
// holding data.
func (e *env) addFile(files []File, path string, data string) []File {
	e.t.Helper()
	sum := sha256.Sum256([]byte(data))
	hash := hex.EncodeToString(sum[:])
	e.b.mu.Lock()
	e.b.blobs[hash] = []byte(data)
	e.b.mu.Unlock()
	return append(slices.Clone(files), File{Path: path, Hash: hash, Size: int64(len(data))})
}

// rowFile returns a row file whose row 1 holds entry in column 1.
func rowFile(entry string) string {
	return `{"rows":[{"cells":{"1":` + entry + `},"id":1}],"styles":[]}`
}

// renamed returns the files of a revision with the file at from listed
// at to.
func renamed(files []File, from, to string) []File {
	out := slices.Clone(files)
	for i := range out {
		if out[i].Path == from {
			out[i].Path = to
		}
	}
	return out
}

// viewRevision reads the first cell of a revision view of the given files
// under the number n, as often as the build answers that it calculates.
func (e *env) viewRevision(n int, files []File, date string) error {
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(time.Millisecond) {
		_, err := e.in.ReadRevision(context.Background(), n, files, date, ReadQuery{Tab: "Sheet1", Range: "A1"})
		if err == nil || refusal(err).Reason != "calculating" || time.Now().After(deadline) {
			return err
		}
	}
}

// rowFileOf returns the path of the row file of the first tab.
func rowFileOf(t *testing.T, files []File) string {
	t.Helper()
	for _, f := range files {
		if strings.HasPrefix(f.Path, "tabs/1/rows/") {
			return f.Path
		}
	}
	t.Fatal("the revision has no row file")
	return ""
}

// outboxAfter returns the outbox entries after the cursor as their
// sequences, kinds and file lists.
func (e *env) outboxAfter(cursor int64) []string {
	e.t.Helper()
	entries, err := e.in.Outbox(context.Background(), cursor, 1000)
	if err != nil {
		e.t.Fatal(err)
	}
	out := make([]string, len(entries))
	for i, entry := range entries {
		out[i] = fmt.Sprintf("%d %s %v", entry.Seq, entry.Kind, entry.Files)
	}
	return out
}

// undoDepth returns the number of entries on a person's undo stack, and
// how many of them expired.
func (p *person) undoDepth() (entries, lost int) {
	p.e.in.mu.RLock()
	defer p.e.in.mu.RUnlock()
	for _, entry := range p.s.undo.undo {
		if entry.lost {
			lost++
		}
	}
	return len(p.s.undo.undo), lost
}

// A revision whose files hold an id outside 1 to 2^53-1, a run whose sum
// wraps, a run of more than two numbers, an object with a key twice, also
// under an escaped spelling of the key or in its other case, a row outside
// its chunk, a chunk from 2^53 or later or a part of a chunk without the
// parts before it, is refused as a view and as a rollback, and so is a key
// of a width or a cell with a leading zero, a row file whose rows do not
// ascend, and a stored formula or a text over the limits of a typed one.
// So is a revision with a path that the writer does not spell so, a file
// of no tab of its manifest, a file listed twice or one of another size
// than its entry says: the rollback would store the list as it is.
// Neither changes the live sheet, its sequence, its outbox or the undo
// history, and the sheet opens again as it was.
func TestDamagedRevisionLeavesTheSheet(t *testing.T) {
	ctx := context.Background()
	e := start(t)
	alice := e.person("Alice")
	alice.set("A1", "before")
	n := closeRevision(e)
	alice.set("A2", "live")
	files, date, release, err := e.b.RevisionFiles(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	rows := rowFileOf(t, files)
	cases := []struct{ name, path, data string }{
		{"a row run whose end wraps", axesPath(1), `{"cols":[[1,26]],"rows":[[1,1000],[18446744073709551615,2]],"widths":{}}`},
		{"a column run whose end wraps", axesPath(1), `{"cols":[[1,26],[18446744073709551615,2]],"rows":[[1,1000]],"widths":{}}`},
		{"a row run past the last id", axesPath(1), `{"cols":[[1,26]],"rows":[[1,1000],[9007199254740991,2]],"widths":{}}`},
		{"a column run past the last id", axesPath(1), `{"cols":[[1,26],[9007199254740991,2]],"rows":[[1,1000]],"widths":{}}`},
		{"a row run from 2^53", axesPath(1), `{"cols":[[1,26]],"rows":[[1,1000],[9007199254740992,1]],"widths":{}}`},
		{"a row run from 0", axesPath(1), `{"cols":[[1,26]],"rows":[[0,1001]],"widths":{}}`},
		{"a column run from 0", axesPath(1), `{"cols":[[0,27]],"rows":[[1,1000]],"widths":{}}`},
		{"a run of three numbers", axesPath(1), `{"cols":[[1,26]],"rows":[[1,1000,5]],"widths":{}}`},
		{"a tab name twice", manifestPath, `{"format":1,"tabs":[{"frozen_cols":0,"frozen_rows":0,"id":1,"name":"Sheet1","name":"After"}]}`},
		{"a tab name twice, escaped", manifestPath, `{"format":1,"tabs":[{"frozen_cols":0,"frozen_rows":0,"id":1,"name":"Sheet1","\u006eame":"After"}]}`},
		{"a tab id twice", manifestPath, `{"format":1,"tabs":[{"frozen_cols":0,"frozen_rows":0,"id":1,"id":1,"name":"Sheet1"}]}`},
		{"a tab key in another case", manifestPath, `{"format":1,"tabs":[{"frozen_cols":0,"frozen_rows":0,"id":1,"Name":"Sheet1"}]}`},
		{"a cell value twice", rows, `{"rows":[{"cells":{"1":{"v":1,"v":2}},"id":1}],"styles":[]}`},
		{"a cell value twice, escaped", rows, `{"rows":[{"cells":{"1":{"v":1,"\u0076":2}},"id":1}],"styles":[]}`},
		{"a cell value twice, in another case", rows, `{"rows":[{"cells":{"1":{"v":1,"V":2}},"id":1}],"styles":[]}`},
		{"a cell style twice", rows, `{"rows":[{"cells":{"1":{"s":0,"s":0,"v":1}},"id":1}],"styles":[{"bold":true}]}`},
		{"a format property twice", rows, `{"rows":[{"cells":{"1":{"s":0,"v":1}},"id":1}],"styles":[{"bold":true,"bold":false}]}`},
		{"a format property twice, escaped", rows, `{"rows":[{"cells":{"1":{"s":0,"v":1}},"id":1}],"styles":[{"bold":true,"\u0062old":false}]}`},
		{"a number format key twice", rows, `{"rows":[{"cells":{"1":{"s":0,"v":1}},"id":1}],"styles":[{"number":{"decimals":0,"decimals":2,"kind":"percent"}}]}`},
		{"a number format key twice, escaped", rows, `{"rows":[{"cells":{"1":{"s":0,"v":1}},"id":1}],"styles":[{"number":{"kind":"percent","decimals":0,"\u006bind":"number","thousands":true}}]}`},
		{"a number format key in another case", rows, `{"rows":[{"cells":{"1":{"s":0,"v":1}},"id":1}],"styles":[{"number":{"Kind":"percent","decimals":0}}]}`},
		{"a width key with a leading zero", axesPath(1), `{"cols":[[1,26]],"rows":[[1,1000]],"widths":{"02":120}}`},
		{"a column key with a leading zero", rows, `{"rows":[{"cells":{"01":{"v":1}},"id":1}],"styles":[]}`},
		{"a row twice in its file", rows, `{"rows":[{"cells":{"1":{"v":1}},"id":1},{"cells":{"2":{"v":2}},"id":1}],"styles":[]}`},
		{"rows out of order", rows, `{"rows":[{"cells":{"1":{"v":1}},"id":2},{"cells":{"1":{"v":1}},"id":1}],"styles":[]}`},
		// Stored formulas and a text that no typed one could be: the text
		// of a formula past 8,192 characters at its cell, nesting past 64
		// levels, a stored form past formula.MaxStoredBytes and a text past
		// 32,767 characters.
		{"a formula of a long literal", rows, rowFile(`{"f":` + strconv.Quote(`="`+strings.Repeat("x", 32768)+`"`) + `}`)},
		{"a formula nested 65 levels", rows, rowFile(`{"f":"=` + strings.Repeat("(", 65) + "1" + strings.Repeat(")", 65) + `"}`)},
		{"a formula of a long literal sum", rows, rowFile(`{"f":"=1` + strings.Repeat("+1", formula.MaxStoredBytes/2) + `"}`)},
		{"a formula of more tokens than a formula holds", rows, rowFile(`{"f":"=1` + strings.Repeat("+1", (formula.MaxStoredBytes-2)/2) + `"}`)},
		{"a long text", rows, rowFile(`{"v":"` + strings.Repeat("x", 32768) + `"}`)},
	}
	type damage struct {
		name  string
		files []File
	}
	// Files that do not fit their paths: the second part of a chunk
	// without its first, and a row that lies past the end of the chunk
	// whose file holds it, since another chunk begins at its id.
	damages := []damage{
		{"a part without the part before it", renamed(files, rows, "tabs/1/rows/0-1.json")},
		{"a row past its chunk", e.addFile(files, "tabs/1/rows/1-0.json", `{"rows":[{"cells":{"1":{"v":2}},"id":2}],"styles":[]}`)},
	}
	if rows != "tabs/1/rows/0-0.json" {
		t.Fatalf("the row file of the revision is %s", rows)
	}
	// Paths of the same files under spellings the writer does not make.
	for _, path := range []string{
		"tabs/1/rows/0-00.json", "tabs/1/rows/0-+0.json", "tabs/1/rows/0--0.json", "tabs/1/rows/00-0.json",
		"tabs/1/rows/+0-0.json", "tabs/1/rows/0x0-0.json", "tabs/01/rows/0-0.json", "tabs/+1/rows/0-0.json",
	} {
		damages = append(damages, damage{"the row file at " + path, renamed(files, rows, path)})
	}
	empty := `{"rows":[],"styles":[]}`
	// The chunk from row 10 on, holding the rows 10 to 265, so that with
	// the row of the chunk before it it holds more than chunkRows rows, as
	// the writer leaves two neighboring chunks.
	tail := make([]string, chunkRows)
	for i := range tail {
		tail[i] = fmt.Sprintf(`{"cells":{"1":{"v":%d}},"id":%d}`, 10+i, 10+i)
	}
	fromTen := `{"rows":[` + strings.Join(tail, ",") + `],"styles":[]}`
	resized := slices.Clone(files)
	resized[0].Size++
	damages = append(damages,
		// The chunk from row 10 on, its first id in uppercase. In lowercase
		// it is taken.
		damage{"a chunk whose first id is in uppercase", e.addFile(files, "tabs/1/rows/A-0.json", fromTen)},
		// The writer writes no file for a chunk without rows.
		damage{"a row file without rows", e.addFile(files, "tabs/1/rows/a-0.json", empty)},
		// Chunks from 2^53 and from the greatest 64-bit number, whose
		// first ids sheet.db could not hold.
		damage{"a chunk from 2^53", e.addFile(files, "tabs/1/rows/20000000000000-0.json", empty)},
		damage{"a chunk from 2^64-1", e.addFile(files, "tabs/1/rows/ffffffffffffffff-0.json", empty)},
		damage{"axes under a tab id with a leading zero", e.addFile(files, "tabs/01/axes.json", `{"cols":[[1,26]],"rows":[[1,1000]],"widths":{}}`)},
		damage{"a row file of a tab the manifest lacks", e.addFile(files, "tabs/7/rows/0-0.json", empty)},
		damage{"axes of a tab the manifest lacks", e.addFile(files, axesPath(7), `{"cols":[[1,26]],"rows":[[1,1000]],"widths":{}}`)},
		damage{"a file the writer never makes", e.addFile(files, "notes.json", `{}`)},
		damage{"a file listed twice", append(slices.Clone(files), files[len(files)-1])},
		damage{"a file of another size", resized},
	)
	for _, c := range cases {
		damages = append(damages, damage{c.name, e.withFile(files, c.path, c.data)})
	}
	seq, before := e.seq(), e.dump()
	depth, lost := alice.undoDepth()
	outbox := e.outboxAfter(0)
	for i, c := range damages {
		damaged := c.files
		if err := e.viewRevision(1000+i, damaged, date); refusal(err).Reason != "revision" {
			t.Errorf("%s: the view answered %v, want a refusal of the revision", c.name, err)
		}
		if err := e.in.Rollback(ctx, damaged, "Alice", fmt.Sprintf("damaged-%d", i)); refusal(err).Reason != "revision" {
			t.Errorf("%s: the rollback answered %v, want a refusal of the revision", c.name, err)
		}
		if got := e.seq(); got != seq {
			t.Fatalf("%s: the sheet is at %d after the refusals, was at %d", c.name, got, seq)
		}
		if after := e.dump(); after != before {
			t.Fatalf("%s: the refusals changed the sheet:\n%s", c.name, dumpDiff(before, after))
		}
		if d, l := alice.undoDepth(); d != depth || l != lost {
			t.Fatalf("%s: Alice's undo holds %d entries, %d lost, after the refusals; it held %d, %d lost", c.name, d, l, depth, lost)
		}
		if got := e.outboxAfter(0); !slices.Equal(got, outbox) {
			t.Fatalf("%s: the outbox holds %v after the refusals, held %v", c.name, got, outbox)
		}
	}
	e.restart()
	if after := e.dump(); after != before {
		t.Fatalf("the sheet opens differently after the refusals:\n%s", dumpDiff(before, after))
	}
	// The chunk from row 10 on in lowercase is a file the writer could
	// have made, and is taken.
	if err := e.in.Rollback(ctx, e.addFile(files, "tabs/1/rows/a-0.json", fromTen), "Alice", "lowercase"); err != nil {
		t.Fatalf("the rollback with the chunk in lowercase: %v", err)
	}
	e.want("A16", "16")
	// The files as the engine wrote them still roll back.
	if err := e.in.Rollback(ctx, files, "Alice", "intact"); err != nil {
		t.Fatal(err)
	}
	e.want("A1", "before")
	e.want("A2", "")
}

// Refused revision files also leave a tab recovery without effect: the
// sheet, its sequence and the person's undo stay as they were.
func TestDamagedRevisionRecoversNoTab(t *testing.T) {
	ctx := context.Background()
	e := start(t)
	alice := e.person("Alice")
	alice.set("A1", "before")
	n := closeRevision(e)
	alice.set("A2", "live")
	files, _, release, err := e.b.RevisionFiles(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	seq, before := e.seq(), e.dump()
	depth, lost := alice.undoDepth()
	for _, data := range []struct{ path, data string }{
		{axesPath(1), `{"cols":[[1,26]],"rows":[[1,1000],[18446744073709551615,2]],"widths":{}}`},
		{manifestPath, `{"format":1,"tabs":[{"frozen_cols":0,"frozen_rows":0,"id":1,"name":"Sheet1","name":"After"}]}`},
		{rowFileOf(t, files), `{"rows":[{"cells":{"1":{"v":1,"v":2}},"id":1}],"styles":[]}`},
	} {
		damaged := e.withFile(files, data.path, data.data)
		if _, err := e.in.recoverTab(ctx, alice.s, "", damaged, 1); refusal(err).Reason != "revision" {
			t.Errorf("the recovery from a damaged %s answered %v, want a refusal of the revision", data.path, err)
		}
		if got, after := e.seq(), e.dump(); got != seq || after != before {
			t.Fatalf("the refused recovery changed the sheet from %d to %d:\n%s", seq, got, dumpDiff(before, after))
		}
		if d, l := alice.undoDepth(); d != depth || l != lost {
			t.Fatalf("Alice's undo holds %d entries, %d lost, after the refused recovery; it held %d, %d lost", d, l, depth, lost)
		}
	}
}

// A chunk whose first id is the last id, 2^53-1, is taken: the view serves
// it, and a rollback to it is stored and opens again with that chunk and
// its row.
func TestRevisionChunkAtTheLastID(t *testing.T) {
	ctx := context.Background()
	e := start(t)
	e.person("Alice").set("A1", "kept")
	n := closeRevision(e)
	files, date, release, err := e.b.RevisionFiles(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	edge := e.withFile(files, axesPath(1), `{"cols":[[1,26]],"rows":[[1,1000],[9007199254740991,1]],"widths":{}}`)
	// The writer merges two neighboring chunks of at most chunkRows rows,
	// so the chunk before the last holds chunkRows rows.
	head := []string{`{"cells":{"1":{"v":"kept"}},"id":1}`}
	for id := 2; id <= chunkRows; id++ {
		head = append(head, fmt.Sprintf(`{"cells":{"1":{"v":%d}},"id":%d}`, id, id))
	}
	edge = e.withFile(edge, rowFileOf(t, files), `{"rows":[`+strings.Join(head, ",")+`],"styles":[]}`)
	edge = e.addFile(edge, "tabs/1/rows/1fffffffffffff-0.json", `{"rows":[{"cells":{"1":{"v":1}},"id":9007199254740991}],"styles":[]}`)
	if err := e.viewRevision(1000, edge, date); err != nil {
		t.Fatalf("the view: %v", err)
	}
	if err := e.in.Rollback(ctx, edge, "Alice", "edge"); err != nil {
		t.Fatalf("the rollback: %v", err)
	}
	e.restart()
	e.want("A1", "kept")
	e.want("A1001", "1")
	e.in.mu.RLock()
	chunks := slices.Clone(e.in.wb.tabs[0].chunks)
	e.in.mu.RUnlock()
	if !slices.Equal(chunks, []RowID{0, maxID - 1}) {
		t.Fatalf("the chunks of the tab are %v after the restart, want 0 and %d", chunks, uint64(maxID-1))
	}
}

// A run that ends at the last id, 2^53-1, is taken, in the rows and in the
// columns: the view serves it, and a rollback to it is stored and opens
// again with that id on its axis.
func TestRevisionIDsAtTheirLimit(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct{ name, axes string }{
		{"rows", `{"cols":[[1,26]],"rows":[[1,1000],[9007199254740991,1]],"widths":{}}`},
		{"columns", `{"cols":[[1,26],[9007199254740991,1]],"rows":[[1,1000]],"widths":{"9007199254740991":120}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			e.person("Alice").set("A1", "kept")
			n := closeRevision(e)
			files, date, release, err := e.b.RevisionFiles(ctx, n)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			edge := e.withFile(files, axesPath(1), c.axes)
			if err := e.viewRevision(1000, edge, date); err != nil {
				t.Fatalf("the view: %v", err)
			}
			if err := e.in.Rollback(ctx, edge, "Alice", "edge"); err != nil {
				t.Fatalf("the rollback: %v", err)
			}
			e.restart()
			e.want("A1", "kept")
			e.in.mu.RLock()
			tab := e.in.wb.tabs[0]
			last := map[string]uint64{"rows": tab.rows.ids[len(tab.rows.ids)-1], "columns": tab.cols.ids[len(tab.cols.ids)-1]}[c.name]
			e.in.mu.RUnlock()
			if last != maxID-1 {
				t.Fatalf("the last id of the %s is %d after the restart, want %d", c.name, last, uint64(maxID-1))
			}
		})
	}
}

// The readers of a manifest, a cell entry and a stored format refuse a
// key twice, under any spelling, and a key in another case than the
// engine's. The engine's own encoding of each reads back.
func TestRevisionObjectsRefuseRepeatedKeys(t *testing.T) {
	for _, data := range []string{
		`{"format":1,"tabs":[{"id":1,"name":"A","name":"B"}]}`,
		`{"format":1,"tabs":[{"id":1,"name":"A","\u006eame":"B"}]}`,
		`{"format":1,"tabs":[{"id":1,"ID":2,"name":"A"}]}`,
		`{"format":1,"tabs":[{"frozen_rows":1,"frozen_rows":2,"id":1,"name":"A"}]}`,
	} {
		if _, err := decodeManifest([]byte(data)); err == nil {
			t.Errorf("the manifest %s was taken", data)
		}
	}
	if m, err := decodeManifest([]byte(`{"format":1,"tabs":[{"frozen_cols":2,"frozen_rows":1,"id":3,"name":"Budget"}]}`)); err != nil ||
		len(m.Tabs) != 1 || m.Tabs[0] != (manifestTab{ID: 3, Name: "Budget", FrozenRows: 1, FrozenCols: 2}) {
		t.Errorf("the canonical manifest reads as %+v, %v", m, err)
	}
	for _, data := range []string{
		`{"v":1,"v":2}`, `{"v":1,"\u0076":2}`, `{"v":1,"V":2}`, `{"f":"=1","f":"=2"}`, `{"d":"2026-01-01","dt":"2026-01-01T00:00:00","d":"2026-01-02"}`,
		`{"v":1} {}`, `{"v":1,"w":2}`,
	} {
		if _, err := decodeCellEntry([]byte(data)); err == nil {
			t.Errorf("the cell entry %s was taken", data)
		}
	}
	for _, data := range []string{
		`{"bold":true,"bold":false}`, `{"bold":true,"\u0062old":false}`, `{"Bold":true}`,
		`{"number":{"decimals":0,"decimals":2,"kind":"percent"}}`, `{"number":{"decimals":0,"kind":"number","\u006bind":"percent"}}`,
		`{"number":{"kind":"percent","Decimals":0}}`, `{"bold":true} {}`,
	} {
		if _, err := decodeFormatPatch([]byte(data), true); err == nil {
			t.Errorf("the format %s was taken", data)
		}
	}
	for _, f := range []string{
		`{}`, `{"bold":true}`, `{"align":"center","fill":"red-600","italic":true,"number":{"decimals":2,"kind":"currency","symbol":"$"},"text_color":"blue-600","wrap":true}`,
		`{"number":{"decimals":1,"kind":"number","thousands":true}}`, `{"number":{"kind":"percent","decimals":0}}`,
	} {
		p, err := decodeFormatPatch([]byte(f), true)
		if err != nil {
			t.Errorf("the format %s: %v", f, err)
			continue
		}
		again, aerr := appendFormat(nil, p.f)
		if q, err := decodeFormatPatch(again, true); aerr != nil || err != nil || q.f != p.f {
			t.Errorf("the format %s encodes as %s, which reads as %+v: %v %v", f, again, q.f, aerr, err)
		}
	}
}
