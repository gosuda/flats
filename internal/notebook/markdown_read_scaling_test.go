package notebook

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestMarkdownManyRichRuns(t *testing.T) {
	const count = 40000
	f := liveReadFixture(t)
	var runs strings.Builder
	runs.WriteByte('[')
	for i := 0; i < count; i++ {
		if i > 0 {
			runs.WriteByte(',')
		}
		mark := "bold"
		if i%2 == 1 {
			mark = "italic"
		}
		fmt.Fprintf(&runs, `{"text":"word","marks":[%q]}`, mark)
	}
	runs.WriteByte(']')
	if runs.Len() < 1<<20 {
		t.Fatal("fixture no longer exceeds 1 MiB")
	}
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		id := spillID(BlockKind, 900)
		if err := putItem(t.Context(), tx, spillItem(id, Block{Page: f.page.ID, Position: "A", Content: Content{Kind: Paragraph, Text: RichText{}}})); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `UPDATE items SET data=json_set(data,'$.content.text',json(?)) WHERE id=?`, runs.String(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	query, _ := json.Marshal(map[string]any{"page": f.page.ID, "format": "markdown", "limit": 1})
	body, peak, err := measureReaderHeap(t, f.in, query)
	t.Logf("runs=%d retained=%d heap=%d", count, runs.Len(), peak)
	if err != nil {
		t.Fatal(err)
	}
	if peak > 4<<20 {
		t.Fatalf("rich run capture exceeds heap bound: %d", peak)
	}
	text, _, _ := continueMarkdownRead(t, f.in, body)
	if strings.Count(text, "word") != count {
		t.Fatal("rich runs missing from capture")
	}
}

func TestMarkdownManyOptionRows(t *testing.T) {
	const options, rows = 5000, 100
	f, created := readerQueryFixture(t)
	property, database := created.Refs["tags"].Ref.ID, created.Refs["db"].Ref.ID
	order := make([]ID, options)
	for i := range order {
		order[i] = spillID(OptionKind, uint64(100+i))
	}
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		if err := spillOptions(t, tx, property, order, func(i int) string { return fmt.Sprintf("Option %d", i) }); err != nil {
			return err
		}
		if err := putOrder(t.Context(), tx, property, OptionOrderGroup, order); err != nil {
			return err
		}
		for i := 0; i < rows; i++ {
			id := spillID(RowKind, uint64(100+i))
			row := Row{Database: database, Position: fmt.Sprintf("B%03d", i), Cells: map[ID]CellValue{property: {Type: MultiSelectType, Value: []ID{order[options-1], order[0]}}}}
			if err := putItem(t.Context(), tx, spillItem(id, row)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	query, _ := json.Marshal(map[string]any{"page": f.page.ID, "format": "markdown", "limit": 64})
	body, peak, err := measureReaderHeap(t, f.in, query)
	t.Logf("options=%d rows=%d heap=%d", options, rows, peak)
	if err != nil {
		t.Fatal(err)
	}
	if peak > 4<<20 {
		t.Fatalf("option row capture exceeds heap bound: %d", peak)
	}
	text, _, _ := continueMarkdownRead(t, f.in, body)
	if strings.Count(text, "Option 0, Option 4999") != rows {
		t.Fatal("ordered selections missing")
	}
}
