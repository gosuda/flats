package notebook

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestMarkdownRetainedMemory(t *testing.T) {
	for _, kind := range []string{"database_title", "row_text", "block_code", "block_rich_text", "option_name", "option_schema"} {
		t.Run(kind, func(t *testing.T) {
			sizes := []int{1 << 20}
			large := os.Getenv("NOTEBOOK_LARGE_MEMORY_TESTS") == "1"
			if kind == "option_schema" || large {
				sizes = readerMemorySizes()
			}
			if kind == "option_schema" && !large {
				// At 16 MiB, retaining every option id exceeds the growth allowance.
				sizes = append(sizes, 16<<20)
			}
			// Whole-read heap samples and the growth comparison must stay sequential.
			schemaPeaks := map[int]uint64{}
			for _, size := range sizes {
				t.Run(fmt.Sprintf("%gMiB", float64(size)/(1<<20)), func(t *testing.T) {
					started := time.Now()
					f, created := readerQueryFixture(t)
					database := created.Refs["db"].Ref
					wantRefs := map[ID]Ref{f.page.ID: f.page}
					addRefs := func(result WriteResult) {
						for _, assigned := range result.Refs {
							wantRefs[assigned.Ref.ID] = assigned.Ref
						}
					}
					addRefs(created)
					limit := 1
					optionCount := 0
					payload := kind != "database_title" && kind != "option_schema"
					var fragments []string
					id, path := created.Refs["db"].Ref.ID, "$.title"
					switch kind {
					case "row_text":
						added := liveReadUpdate(t, f, "text-property", CreatePropertyOp{CreateFields: CreateFields{Ref: "text"}, Database: Target{Ref: &database}, Definition: PropertyDef{Name: "Note", Type: TextType}})
						addRefs(added)
						cell, err := EncodeCellValue(CellValue{Type: TextType, Value: "seed"})
						if err != nil {
							t.Fatal(err)
						}
						if err := f.in.db.transaction(t.Context(), func(tx *sql.Tx) error {
							_, err := tx.ExecContext(t.Context(), `INSERT INTO cells(row_id,property_id,value) VALUES(?,?,json_set(?,'$.value',?))`, created.Refs["big"].Ref.ID, added.Refs["text"].Ref.ID, string(cell), "payloadstart"+strings.Repeat("x", size)+"payloadend")
							return err
						}); err != nil {
							t.Fatal(err)
						}
					case "block_code", "block_rich_text":
						content := Content{Kind: Code, Code: "seed", Language: "go"}
						path = "$.content.text"
						if kind == "block_rich_text" {
							content = Content{Kind: Paragraph, Text: RichText{{Text: "seed", Marks: []Mark{Bold}}}}
							path = "$.content.text[0].text"
						}
						added := liveReadUpdate(t, f, "retained-block", CreateBlockOp{CreateFields: CreateFields{Ref: "block"}, Place: Placement{Page: Target{Ref: &f.page}}, Content: content})
						addRefs(added)
						id = added.Refs["block"].Ref.ID
					case "option_name":
						first := created.Refs["first"].Ref
						row, property := created.Refs["big"].Ref, created.Refs["tags"].Ref
						liveReadUpdate(t, f, "selected-option", SetCellOp{Row: Target{Ref: &row}, Cell: CellInput{Property: Target{Ref: &property}, Value: CellInputValue{Options: []Target{{Ref: &first}}}}})
						id, path = first.ID, "$.name"
					case "option_schema":
						// Populate every ordered id; absent option ids do not exercise schema retention.
						property := created.Refs["tags"].Ref
						name := func(n int) string { return fmt.Sprintf("choice%06d", n) + strings.Repeat("x", 144) }
						data, err := json.Marshal(Option{Property: property.ID, Name: name(0), Color: Gray})
						if err != nil {
							t.Fatal(err)
						}
						optionBytes := len(data) + len(spillID(OptionKind, 100)) + 3
						count := (size + optionBytes - 1) / optionBytes
						optionCount = count
						order := make([]ID, 0, count+2)
						order = append(order, created.Refs["first"].Ref.ID)
						for n := count - 1; n >= 0; n-- {
							id := spillID(OptionKind, uint64(n+100))
							order = append(order, id)
						}
						optionName := func(i int) string { return name(count - 1 - i) }
						// Keep the default small fixtures in-process; amortize the helper build on large schemas.
						external := size >= 8<<20
						if external {
							if err := spillExternalOptions(t, f.in, property.ID, order[1:], optionName); err != nil {
								t.Fatal(err)
							}
						}
						if err := f.in.db.transaction(t.Context(), func(tx *sql.Tx) error {
							if !external {
								if err := spillOptions(t, tx, property.ID, order[1:], optionName); err != nil {
									return err
								}
							}
							order = append(order, created.Refs["second"].Ref.ID)
							if err := putOrder(t.Context(), tx, property.ID, OptionOrderGroup, order); err != nil {
								return err
							}
							selected := []ID{order[count], order[count/2], order[1]}
							order = nil
							cell, err := EncodeCellValue(CellValue{Type: MultiSelectType, Value: selected})
							if err != nil {
								return err
							}
							_, err = tx.ExecContext(t.Context(), `INSERT INTO cells(row_id,property_id,value) VALUES(?,?,?)`, created.Refs["big"].Ref.ID, property.ID, string(cell))
							return err
						}); err != nil {
							t.Fatal(err)
						}
						fragments = append(fragments, name(count-1)+", "+name(count-count/2)+", "+name(0))
						// Batch schema-only continuations rather than issue one request per option.
						limit = 500
					}
					if kind != "row_text" && kind != "option_schema" {
						if err := f.in.db.transaction(t.Context(), func(tx *sql.Tx) error {
							_, err := tx.ExecContext(t.Context(), `UPDATE items SET data=json_set(data,?,?) WHERE id=?`, path, "payloadstart"+strings.Repeat("x", size)+"payloadend", id)
							if err == nil && kind == "option_name" {
								_, err = tx.ExecContext(t.Context(), `UPDATE options SET name_key=(SELECT json_extract(data,'$.name') FROM items WHERE id=?) WHERE id=?`, id, id)
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
					raw, err := json.Marshal(map[string]any{"page": f.page.ID, "format": "markdown", "limit": limit})
					if err != nil {
						t.Fatal(err)
					}
					fixtureElapsed := time.Since(started)
					readStart := time.Now()
					body, peak, err := measureReaderHeap(t, f.in, raw)
					t.Logf("retained=%d sampled whole-read heap growth=%d response=%d fixture=%s read=%s", size, peak, len(body), fixtureElapsed, time.Since(readStart))
					if err != nil {
						t.Fatal(err)
					}
					if peak > 4<<20 {
						t.Errorf("Markdown read materialized retained %s: %d bytes exceeds 4 MiB", kind, peak)
					} else if kind == "option_schema" && !large && (size == 1<<20 || size == 16<<20) {
						schemaPeaks[size] = peak
					}
					// Delivery and its assertions are outside the whole-entry heap sample.
					text, refs, warnings := continueMarkdownRead(t, f.in, body)
					if len(refs) != len(wantRefs)+optionCount {
						t.Fatalf("captured %d references, want %d", len(refs), len(wantRefs)+optionCount)
					}
					for id, want := range wantRefs {
						if got := refs[id]; got.Ref != want || got.Ver == 0 {
							t.Fatalf("missing mutation-ready reference %s: %+v, want %+v", id, got, want)
						}
					}
					// Generate the large oracle after sampling: a live map of expected refs
					// otherwise increases the GC allowance for transient reader allocations.
					for n := 0; n < optionCount; n++ {
						id := spillID(OptionKind, uint64(n+100))
						if _, exists := wantRefs[id]; exists {
							t.Fatalf("generated option overlaps fixture reference: %s", id)
						}
						want := AssignedRef{Ref: Ref{ID: id, Inc: 7}, Ver: 7}
						if got := refs[id]; got != want {
							t.Fatalf("missing mutation-ready reference %s: %+v, want %+v", id, got, want)
						}
					}
					if payload {
						fragments = append(fragments, "payloadstart"+strings.Repeat("x", size)+"payloadend")
					}
					fragments = append(fragments, "| Name | Tags", "Second", "<!-- flats:block "+string(created.Refs["owner"].Ref.ID)+" -->")
					for _, fragment := range fragments {
						if !strings.Contains(text, fragment) {
							t.Fatalf("captured Markdown missing expected %d-byte fragment (output=%d bytes)", len(fragment), len(text))
						}
					}
					if len(warnings) != 1 || warnings[0].Code != "database_display_text" {
						t.Fatalf("database fallback warnings changed: %+v", warnings)
					}
				})
			}
			if kind == "option_schema" && !large {
				t.Run("bounded_growth", func(t *testing.T) {
					for _, size := range []int{1 << 20, 16 << 20} {
						if _, ok := schemaPeaks[size]; !ok {
							t.Fatalf("option-schema heap sample unavailable for %d MiB; run both size cases with this comparison", size>>20)
						}
					}
					small, large := schemaPeaks[1<<20], schemaPeaks[16<<20]
					delta := int64(large) - int64(small)
					t.Logf("option-schema heap growth: 1 MiB=%d 16 MiB=%d delta=%d", small, large, delta)
					if delta > 1<<20 {
						t.Errorf("Markdown read materialized retained option schema: heap growth increased by %d bytes from 1 to 16 MiB, exceeds 1 MiB", delta)
					}
				})
			}
		})
	}
}

func continueMarkdownRead(t *testing.T, in *Instance, body []byte) (string, map[ID]AssignedRef, []Warning) {
	t.Helper()
	var text strings.Builder
	refs := map[ID]AssignedRef{}
	var warnings []Warning
	var state State
	for pages := 0; ; pages++ {
		if pages > 10000 || len(body) > readPageBytes {
			t.Fatal("Markdown continuation exceeded progress or response bound")
		}
		var result readResult
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
		if pages == 0 && result.State != nil {
			state = *result.State
		}
		if result.Markdown == nil || result.Offset == nil || *result.Offset != int64(text.Len()) || result.State == nil || *result.State != state || !utf8.ValidString(*result.Markdown) {
			t.Fatal("Markdown continuation changed snapshot, byte offset or UTF-8 boundaries")
		}
		text.WriteString(*result.Markdown)
		warnings = append(warnings, result.Warnings...)
		for id, ref := range result.References {
			if old, exists := refs[id]; exists && old != ref {
				t.Fatalf("reference version changed across continuations: %s", id)
			}
			refs[id] = ref
		}
		if result.Complete {
			if result.Next != nil {
				t.Fatal("completed Markdown capture returned a cursor")
			}
			return text.String(), refs, warnings
		}
		if result.Next == nil {
			t.Fatal("incomplete Markdown capture has no cursor")
		}
		body, _ = liveReadQuery(t, in, map[string]any{"cursor": *result.Next}, false)
	}
}

func TestMarkdownContinuationSnapshotVersions(t *testing.T) {
	f, created := readerQueryFixture(t)
	row, property := created.Refs["big"].Ref, created.Refs["tags"].Ref
	title := created.Refs["db.title"].Ref
	first, second := created.Refs["first"].Ref, created.Refs["second"].Ref
	before := "beforerow"
	added := liveReadUpdate(t, f, "snapshot-content",
		SetCellOp{Row: Target{Ref: &row}, Cell: CellInput{Property: Target{Ref: &property}, Value: CellInputValue{Options: []Target{{Ref: &second}, {Ref: &first}}}}},
		SetCellOp{Row: Target{Ref: &row}, Cell: CellInput{Property: Target{Ref: &title}, Value: CellInputValue{Text: &before}}},
		CreateBlockOp{CreateFields: CreateFields{Ref: "code"}, Place: Placement{Page: Target{Ref: &f.page}}, Content: Content{Kind: Code, Code: "beforecode", Language: "go"}},
		CreateBlockOp{CreateFields: CreateFields{Ref: "rich"}, Place: Placement{Page: Target{Ref: &f.page}}, Content: Content{Kind: Paragraph, Text: RichText{{Text: "beforerich", Marks: []Mark{Bold}}}}},
	)
	if err := f.in.db.transaction(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE items SET data=json_set(data,'$.content.text',?) WHERE id=?`, strings.Repeat("beforecode\n", readPartBytes/5), added.Refs["code"].Ref.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := map[ID]AssignedRef{}
	if err := f.in.ReadTx(t.Context(), func(tx *sql.Tx, _ Sequence) error {
		ids := []ID{f.page.ID}
		for _, result := range []WriteResult{created, added} {
			for _, assigned := range result.Refs {
				ids = append(ids, assigned.Ref.ID)
			}
		}
		for _, id := range ids {
			record, err := loadRecord(t.Context(), tx, id)
			if err != nil {
				return err
			}
			want[id] = AssignedRef{Ref: record.Item.Ref, Ver: record.Item.Ver}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	body, captured := liveReadQuery(t, f.in, map[string]any{"page": f.page.ID, "format": "markdown", "limit": 1}, false)
	if captured.Complete || captured.Next == nil {
		t.Fatal("fixture did not require Markdown continuations")
	}
	code, rich := added.Refs["code"].Ref, added.Refs["rich"].Ref
	after := "afterrow"
	liveReadUpdate(t, f, "after-capture",
		SetBlockOp{Block: Target{Ref: &code}, Content: Content{Kind: Code, Code: "aftercode"}},
		SetBlockOp{Block: Target{Ref: &rich}, Content: Content{Kind: Paragraph, Text: RichText{{Text: "afterrich", Marks: []Mark{}}}}},
		SetCellOp{Row: Target{Ref: &row}, Cell: CellInput{Property: Target{Ref: &title}, Value: CellInputValue{Text: &after}}},
	)
	cursor := *captured.Next
	replay, _ := liveReadQuery(t, f.in, map[string]any{"cursor": cursor}, false)
	again, _ := liveReadQuery(t, f.in, map[string]any{"cursor": cursor}, false)
	if !bytes.Equal(replay, again) {
		t.Fatal("Markdown continuation replay changed")
	}
	text, refs, warnings := continueMarkdownRead(t, f.in, body)
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("captured reference versions changed after edits: got=%+v want=%+v", refs, want)
	}
	for _, fragment := range []string{"beforecode", "**beforerich**", "| beforerow | First, Second |"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("captured Markdown missing %q", fragment)
		}
	}
	if strings.Contains(text, "aftercode") || strings.Contains(text, "afterrich") || strings.Contains(text, "afterrow") || len(warnings) != 1 || warnings[0].Code != "database_display_text" {
		t.Fatalf("capture changed after edits: warnings=%+v", warnings)
	}
}
