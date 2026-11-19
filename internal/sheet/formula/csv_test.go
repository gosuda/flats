package formula

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// exportFixture is testdata/export/cells.json: one tab as the rows of a
// write operation from A1, with the number formats and the widths of a
// read.
type exportFixture struct {
	Tab     string              `json:"tab"`
	Date    string              `json:"date"`
	Rows    [][]json.RawMessage `json:"rows"`
	Formats []struct {
		Range  string `json:"range"`
		Format struct {
			Number NumberFormat `json:"number"`
		} `json:"format"`
	} `json:"formats"`
	Widths []struct {
		Cols  string `json:"cols"`
		Width int    `json:"width"`
	} `json:"widths"`
}

// TestExportFixture checks the export cases E-01 to E-85 of the technical
// design, section 22.2: the fields of every cell of the fixture tab, row by
// row, are the bytes of expected.csv. The engine's TestExportBytes compares
// its whole export with the same file.
func TestExportFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "export", "cells.json"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "export", "expected.csv"))
	if err != nil {
		t.Fatal(err)
	}
	var fx exportFixture
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fx); err != nil {
		t.Fatal(err)
	}
	today, ok := ParseDate(fx.Date)
	if !ok {
		t.Fatalf("date %q", fx.Date)
	}
	cells := map[string]json.RawMessage{}
	width := int32(0)
	for r, row := range fx.Rows {
		for c, cell := range row {
			cells[Pos{Row: int32(r), Col: int32(c)}.A1()] = cell
		}
		width = max(width, int32(len(row)))
	}
	sheet := newTestSheet(t, cells, nil, today)
	sheet.measure()
	formats := map[Pos]NumberFormat{}
	for _, f := range fx.Formats {
		r, err := ParseRange(f.Range)
		if err != nil {
			t.Fatalf("format range %q: %v", f.Range, err)
		}
		for row := r.First.Row; row <= r.Last.Row; row++ {
			for col := r.First.Col; col <= r.Last.Col; col++ {
				formats[Pos{Row: row, Col: col}] = f.Format.Number
			}
		}
	}

	var got []byte
	for r := range fx.Rows {
		for c := int32(0); c < width; c++ {
			if c > 0 {
				got = append(got, ',')
			}
			p := Pos{Row: int32(r), Col: c}
			cell := sheet.tabs[0].cells[p]
			if cell == nil {
				continue
			}
			got = AppendCSVField(got, sheet.Value(0, p), cell.showKind(), formats[p])
		}
		got = append(got, '\r', '\n')
	}
	if bytes.Equal(got, want) {
		return
	}
	gotRows, wantRows := bytes.Split(got, []byte("\r\n")), bytes.Split(want, []byte("\r\n"))
	for i := range wantRows {
		if i >= len(gotRows) || !bytes.Equal(gotRows[i], wantRows[i]) {
			var row []byte
			if i < len(gotRows) {
				row = gotRows[i]
			}
			t.Fatalf("the export differs at CSV line %d:\n got %q\nwant %q", i+1, row, wantRows[i])
		}
	}
	t.Fatalf("the export has %d bytes, want %d", len(got), len(want))
}
