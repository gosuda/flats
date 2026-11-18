package sheet

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// retarget replaces the position of a cursor and keeps its capture.
func retarget(t testing.TB, cursor string, row, col, skip any) string {
	t.Helper()
	parts := strings.Split(cursor, "_")
	if len(parts) != 5 {
		t.Fatalf("cursor %q has %d parts", cursor, len(parts))
	}
	return fmt.Sprintf("c_%s_%v_%v_%v", parts[1], row, col, skip)
}

func cursorOf(t testing.TB, out map[string]any) string {
	t.Helper()
	next, ok := out["next"].(map[string]any)
	if !ok {
		t.Fatalf("the read has no next page: %v", out)
	}
	return next["cursor"].(string)
}

// TestReadCursorBounds reads with cursors whose position a client changed.
// A position outside the capture is refused; it never reaches a slice.
func TestReadCursorBounds(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["héllo","b"],["c","d"]]}]}`)
	for _, format := range []string{"csv", "json"} {
		cursor := cursorOf(t, e.read(ReadQuery{Tab: "Sheet1", Format: format, Limit: 1}))
		// The cursor as issued reads on.
		if _, err := e.in.Read(context.Background(), ReadQuery{Cursor: cursor}); err != nil {
			t.Fatalf("%s: the issued cursor: %v", format, err)
		}
		for _, bad := range []string{
			retarget(t, cursor, 0, 0, 2147483647),
			retarget(t, cursor, 0, 0, 99),
			retarget(t, cursor, 0, 0, 2), // inside the two bytes of é
			retarget(t, cursor, 2, 0, 0),
			retarget(t, cursor, 0, 2, 0),
			retarget(t, cursor, 2147483647, 2147483647, 0),
			retarget(t, cursor, -1, 0, 0),
			retarget(t, cursor, 0, 0, -1),
			retarget(t, cursor, "x", 0, 0),
			retarget(t, cursor, 0, 0, "9223372036854775808"),
			cursor + "_0",
			strings.TrimPrefix(cursor, "c_"),
		} {
			_, err := e.in.Read(context.Background(), ReadQuery{Cursor: bad})
			if werr := refusal(err); werr == nil || werr.Code != "invalid" || werr.Reason != "cursor" {
				t.Errorf("%s: cursor %q: %v", format, bad, err)
			}
		}
	}
	// A byte offset belongs to a CSV page only.
	cursor := cursorOf(t, e.read(ReadQuery{Tab: "Sheet1", Limit: 1}))
	if _, err := e.in.Read(context.Background(), ReadQuery{Cursor: retarget(t, cursor, 0, 0, 1)}); refusal(err) == nil {
		t.Errorf("a byte offset on a JSON page: %v", err)
	}
	// An offset at a code point inside the field reads the rest of it.
	cursor = cursorOf(t, e.read(ReadQuery{Tab: "Sheet1", Format: "csv", Limit: 1}))
	out, err := e.in.Read(context.Background(), ReadQuery{Cursor: retarget(t, cursor, 0, 0, 3)})
	if err != nil || !strings.Contains(string(out), `"csv":"llo,"`) {
		t.Errorf("an offset after é: %s, %v", out, err)
	}
}

// TestRevisionCursorBounds does the same for a read of a revision.
func TestRevisionCursorBounds(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["a","b"],["c","d"]]}]}`)
	n := e.b.importAll(t)
	files, date, release, err := e.b.RevisionFiles(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	cursor := cursorOf(t, e.readRevision(n, ReadQuery{Tab: "Sheet1", Format: "csv", Limit: 1}))
	if _, err := e.in.ReadRevision(context.Background(), n, files, date, ReadQuery{Cursor: cursor}); err != nil {
		t.Fatalf("the issued cursor: %v", err)
	}
	for _, bad := range []string{retarget(t, cursor, 0, 0, 2147483647), retarget(t, cursor, 5, 0, 0), retarget(t, cursor, 1, 9, 0)} {
		_, err := e.in.ReadRevision(context.Background(), n, files, date, ReadQuery{Cursor: bad})
		if werr := refusal(err); werr == nil || werr.Code != "invalid" || werr.Reason != "cursor" {
			t.Errorf("cursor %q: %v", bad, err)
		}
	}
}

// FuzzReadCursor reads with arbitrary cursor text against live captures of
// both page forms. A read answers with a page or a refusal and never fails
// another way.
func FuzzReadCursor(f *testing.F) {
	e := start(f)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["héllo \"x\"","b",3],["c","=A1&B1",null],["","中文",true]]}]}`)
	tokens := map[bool]string{}
	for _, csv := range []bool{true, false} {
		format := "json"
		if csv {
			format = "csv"
		}
		cursor := cursorOf(f, e.read(ReadQuery{Tab: "Sheet1", Format: format, Limit: 1}))
		tokens[csv] = strings.Split(cursor, "_")[1]
	}
	for _, seed := range []string{"0_1_0", "0_0_0", "0_0_3", "0_0_2", "0_0_2147483647", "2_2_0", "3_0_0", "0_3_0", "-1_0_0", "1_1_99", "0_0", "0_0_0_0", "", "a_b_c", "0_0_9223372036854775808", "+1_+1_+0", "2147483647_2147483647_2147483647"} {
		f.Add(true, seed)
		f.Add(false, seed)
	}
	f.Fuzz(func(t *testing.T, csv bool, tail string) {
		for _, cursor := range []string{"c_" + tokens[csv] + "_" + tail, tail} {
			out, err := e.in.Read(context.Background(), ReadQuery{Cursor: cursor})
			if err != nil {
				if refusal(err) == nil {
					t.Fatalf("cursor %q: %v", cursor, err)
				}
				continue
			}
			var page map[string]any
			if json.Unmarshal(out, &page) != nil || !utf8.Valid(out) {
				t.Fatalf("cursor %q: the page is not JSON text: %s", cursor, out)
			}
		}
	})
}

// TestReadPageCountsFormats reads cells whose formats take as much room as
// their content: the whole result stays inside a page, and the pages join
// to every cell and every format.
func TestReadPageCountsFormats(t *testing.T) {
	e := start(t)
	const rows = 10000
	e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":` + strconv.Itoa(rows-newTabRows) + `}]}`)
	text := strings.Repeat("x", 400)
	for from := 0; from < rows; from += 2500 {
		var b strings.Builder
		for i := 0; i < 2500; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `["%s%05d"]`, text, from+i)
		}
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, from+1, b.String()))
	}
	var odd, even []string
	for i := 1; i <= rows; i++ {
		if i%2 == 1 {
			odd = append(odd, fmt.Sprintf(`"A%d"`, i))
		} else {
			even = append(even, fmt.Sprintf(`"A%d"`, i))
		}
	}
	e.mustAgent(`{"ops":[{"op":"format","tab":"Sheet1","ranges":[` + strings.Join(odd, ",") + `],"format":{"bold":true}},` +
		`{"op":"format","tab":"Sheet1","ranges":[` + strings.Join(even, ",") + `],"format":{"italic":true}},` +
		`{"op":"widths","tab":"Sheet1","cols":"A:A","width":300}]}`)
	for _, include := range [][]string{{"formats"}, {"shown", "formats", "widths"}, {"formats", "widths"}} {
		q := ReadQuery{Tab: "Sheet1", Include: include}
		seenRows, seenFormats, pages := 0, 0, 0
		for {
			data, err := e.in.Read(context.Background(), q)
			if err != nil {
				t.Fatalf("include %v, page %d: %v", include, pages, err)
			}
			if len(data) > maxPageBytes {
				t.Fatalf("include %v: page %d has %d bytes, more than %d", include, pages, len(data), maxPageBytes)
			}
			var page struct {
				Start   string `json:"start"`
				Rows    [][]any
				Shown   [][]string
				Formats []struct {
					Range  string
					Format map[string]any
				}
				Widths []map[string]any
				Next   *struct{ Cursor, Start string }
			}
			if err := json.Unmarshal(data, &page); err != nil {
				t.Fatal(err)
			}
			if want := fmt.Sprintf("A%d", seenRows+1); page.Start != want {
				t.Fatalf("include %v: page %d starts at %s, want %s", include, pages, page.Start, want)
			}
			for i, row := range page.Rows {
				if want := fmt.Sprintf("%s%05d", text, seenRows+i); len(row) != 1 || row[0] != want {
					t.Fatalf("include %v: row %d of page %d is not the written text", include, i, pages)
				}
			}
			for _, f := range page.Formats {
				n, err := strconv.Atoi(strings.TrimPrefix(f.Range, "A"))
				if err != nil || n <= seenRows || n > seenRows+len(page.Rows) {
					t.Fatalf("include %v: page %d names the format of %s outside its rows", include, pages, f.Range)
				}
				if key := map[bool]string{true: "bold", false: "italic"}[n%2 == 1]; f.Format[key] != true || len(f.Format) != 1 {
					t.Fatalf("include %v: %s has the format %v", include, f.Range, f.Format)
				}
			}
			if slices.Contains(include, "shown") && len(page.Shown) != len(page.Rows) {
				t.Fatalf("include %v: page %d has %d shown rows for %d rows", include, pages, len(page.Shown), len(page.Rows))
			}
			if hasWidths := len(page.Widths) > 0; hasWidths != (pages == 0 && slices.Contains(include, "widths")) {
				t.Fatalf("include %v: page %d has widths %v", include, pages, page.Widths)
			}
			seenRows, seenFormats, pages = seenRows+len(page.Rows), seenFormats+len(page.Formats), pages+1
			if page.Next == nil {
				break
			}
			q = ReadQuery{Cursor: page.Next.Cursor}
		}
		if seenRows != rows || seenFormats != rows {
			t.Fatalf("include %v: %d pages hold %d rows and %d formats, want %d of each", include, pages, seenRows, seenFormats, rows)
		}
		if pages < 2 {
			t.Fatalf("include %v: the read took one page", include)
		}
	}
}

// TestReadPageFormatsOfPartialRow cuts a formatted row inside a page: each
// page names the formats of its own cells, and the cursor goes on at the
// cell where the page stopped.
func TestReadPageFormatsOfPartialRow(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,2,3,4,5,6],[7,8,9,10,11,12]]},` +
		`{"op":"format","tab":"Sheet1","ranges":["A1:F2"],"format":{"bold":true}}]}`)
	type page struct {
		Start   string
		Rows    [][]float64
		Formats []struct{ Range string }
		Next    *struct{ Cursor, Start string }
	}
	read := func(q ReadQuery) page {
		var p page
		data, err := e.in.Read(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	first := read(ReadQuery{Tab: "Sheet1", Include: []string{"formats"}, Limit: 4})
	if len(first.Rows) != 1 || len(first.Rows[0]) != 4 || first.Next == nil || first.Next.Start != "E1" {
		t.Fatalf("first page: %+v", first)
	}
	if len(first.Formats) != 1 || first.Formats[0].Range != "A1:D1" {
		t.Fatalf("first page formats: %+v", first.Formats)
	}
	second := read(ReadQuery{Cursor: first.Next.Cursor})
	if second.Start != "E1" || len(second.Rows) != 1 || len(second.Rows[0]) != 2 || second.Rows[0][0] != 5 || second.Next == nil || second.Next.Start != "A2" {
		t.Fatalf("second page: %+v", second)
	}
	if len(second.Formats) != 1 || second.Formats[0].Range != "E1:F1" {
		t.Fatalf("second page formats: %+v", second.Formats)
	}
}
