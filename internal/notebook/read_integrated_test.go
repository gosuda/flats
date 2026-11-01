package notebook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestIntegratedNotebookReadMarkdownPage(t *testing.T) {
	t.Parallel()
	f := liveReadFixture(t)
	created := liveReadUpdate(t, f, "markdown-read",
		MarkdownToBlocksOp{Place: Placement{Page: Target{Ref: &f.page}}, Markdown: "# Heading\n\n**Bold** and [link](https://example.com).\n\n- parent\n  - child\n\n| Name | Note |\n| --- | --- |\n| Alice | original |"},
	)
	_, result := liveReadQuery(t, f.in, map[string]any{"page": f.page.ID, "format": "markdown", "limit": 2}, false)
	state := *result.State
	var text strings.Builder
	refs := map[ID]AssignedRef{}
	var rows, blocks, properties int
	for {
		if result.Markdown == nil || result.Offset == nil || *result.Offset != int64(text.Len()) || *result.State != state {
			t.Fatalf("Markdown capture metadata: %+v", result)
		}
		text.WriteString(*result.Markdown)
		for id, ref := range result.References {
			if id != ref.Ref.ID || ref.Ref.Inc == 0 || ref.Ver == 0 {
				t.Fatalf("not mutation-ready: %q %+v", id, ref)
			}
			refs[id] = ref
		}
		if result.Complete {
			if result.Next != nil {
				t.Fatal("complete Markdown read has a cursor")
			}
			break
		}
		cursor := *result.Next
		body, next := liveReadQuery(t, f.in, map[string]any{"cursor": cursor}, false)
		replay, _ := liveReadQuery(t, f.in, map[string]any{"cursor": cursor}, false)
		if !bytes.Equal(body, replay) {
			t.Fatal("Markdown cursor replay changed")
		}
		result = next
	}
	for id := range refs {
		switch id.Kind() {
		case RowKind:
			rows++
		case BlockKind:
			blocks++
		case PropertyKind:
			properties++
		}
	}
	if rows != 1 || properties != 2 || blocks != 5 || refs[f.page.ID].Ref != f.page {
		t.Fatalf("incomplete Markdown sidecar: rows=%d properties=%d blocks=%d refs=%+v", rows, properties, blocks, refs)
	}
	for _, fragment := range []string{"# Heading", "**Bold**", "  - child", "| Alice | original |", "<!-- flats:block "} {
		if !strings.Contains(text.String(), fragment) {
			t.Fatalf("Markdown missing %q: %s", fragment, text.String())
		}
	}
	// Every assigned reference, including implicit table items, is exposed.
	for _, assigned := range created.Refs {
		if got, ok := refs[assigned.Ref.ID]; !ok || got.Ref != assigned.Ref {
			t.Fatalf("missing created reference: %+v", assigned)
		}
	}
}

func TestIntegratedNotebookReadMarkdownSnapshotAndPublic(t *testing.T) {
	t.Parallel()
	f := liveReadFixture(t)
	created := liveReadUpdate(t, f, "markdown-snapshot",
		CreateBlockOp{CreateFields: CreateFields{Ref: "first"}, Place: Placement{Page: Target{Ref: &f.page}}, Content: Content{Kind: Paragraph, Text: RichText{{Text: "before", Marks: []Mark{}}}}},
		CreateBlockOp{CreateFields: CreateFields{Ref: "last"}, Place: Placement{Page: Target{Ref: &f.page}}, Content: Content{Kind: Toggle, Text: RichText{{Text: "toggle", Marks: []Mark{}}}}},
	)
	_, first := liveReadQuery(t, f.in, map[string]any{"page": f.page, "format": "markdown", "limit": 1}, false)
	ref := created.Refs["first"].Ref
	liveReadUpdate(t, f, "change-after-markdown-capture", SetBlockOp{Block: Target{Ref: &ref}, Content: Content{Kind: Paragraph, Text: RichText{{Text: "after", Marks: []Mark{}}}}})
	var oldText strings.Builder
	warnings := []Warning{}
	result := first
	for {
		oldText.WriteString(*result.Markdown)
		warnings = append(warnings, result.Warnings...)
		if result.State == nil || *result.State != *first.State {
			t.Fatal("Markdown snapshot changed state after mutation")
		}
		if result.Complete {
			break
		}
		_, result = liveReadQuery(t, f.in, map[string]any{"cursor": *result.Next}, false)
	}
	if !strings.Contains(oldText.String(), "before") || strings.Contains(oldText.String(), "after") || len(warnings) != 1 || warnings[0].Code != "toggle" {
		t.Fatalf("snapshot or fallback warnings changed: %s %+v", oldText.String(), warnings)
	}
	body, public := liveReadQuery(t, f.in, map[string]any{"page": f.page.ID, "format": "markdown"}, true)
	if !public.Complete || public.Markdown == nil || !strings.Contains(*public.Markdown, "after") {
		t.Fatalf("Public current Markdown: %s", body)
	}
	for _, forbidden := range []string{`"inc":`, `"ver":`, `"state":`, `"revision":`, `"pending":`, `"references":`} {
		if bytes.Contains(body, []byte(forbidden)) {
			t.Fatalf("Public Markdown leaked %s", forbidden)
		}
	}
	_, err := f.in.Read(t.Context(), []byte(fmt.Sprintf(`{"cursor":%q}`, *first.Next)), true)
	liveReadError(t, err, "conflict", "read_expired")
}

func TestIntegratedNotebookReadMarkdownChunkBoundsAndExporterParity(t *testing.T) {
	t.Parallel()
	f := liveReadFixture(t)
	id := spillID(BlockKind, 500)
	item := spillItem(id, Block{Page: f.page.ID, Position: "A", Content: Content{Kind: Code, Code: strings.Repeat("한\x01", readPartBytes)}})
	var page Item
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		if err := putItem(t.Context(), tx, item); err != nil {
			return err
		}
		var err error
		page, err = readSelected(t.Context(), tx, f.page.ID, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want, _, _ := ExportMarkdown(page, []Item{item}, nil)
	body, result := liveReadQuery(t, f.in, map[string]any{"page": f.page, "format": "markdown"}, false)
	var text strings.Builder
	responses := 0
	for {
		responses++
		if len(body) >= readPageBytes || *result.Offset != int64(text.Len()) || !utf8.ValidString(*result.Markdown) {
			t.Fatal("Markdown encoded bound, byte offsets or UTF-8 boundaries changed")
		}
		text.WriteString(*result.Markdown)
		if result.Complete {
			break
		}
		body, result = liveReadQuery(t, f.in, map[string]any{"cursor": *result.Next}, false)
	}
	if responses < 2 || strings.TrimSpace(text.String()) != strings.TrimSpace(want) {
		t.Fatalf("Markdown chunks differ from the exporter (responses=%d)", responses)
	}
}

func TestIntegratedNotebookReadMarkdownTinyBlocksLinearEnvelope(t *testing.T) {
	t.Parallel()
	const count = 2000
	for _, public := range []bool{false, true} {
		t.Run(fmt.Sprintf("public=%t", public), func(t *testing.T) {
			text, offset := "", int64(0)
			result := readResult{Markdown: &text, Offset: &offset, Warnings: []Warning{}}
			wantText := ""
			want := readResult{Markdown: &wantText, Offset: &offset, Warnings: []Warning{}}
			if !public {
				result.References, want.References = map[ID]AssignedRef{}, map[ID]AssignedRef{}
			}
			calls, encoded := 0, 0
			page, err := newMarkdownReadPage(&result, func(value any) ([]byte, error) {
				raw, err := json.Marshal(value)
				calls++
				encoded += len(raw)
				return raw, err
			})
			if err != nil {
				t.Fatal(err)
			}
			var expected strings.Builder
			for i := 0; i < count; i++ {
				id := spillID(BlockKind, uint64(i+1))
				unit := markdownReadUnit{
					Markdown:  fmt.Sprintf("<!-- flats:block %s -->\nx\n\n", id),
					Reference: &AssignedRef{Ref: Ref{ID: id, Inc: 7}, Ver: 7},
					Warnings:  []Warning{{Code: "tiny", Path: string(id), Message: "\"\\\x01<&>\u2028\u2029"}},
				}
				accepted, err := page.append(unit)
				if err != nil || !accepted {
					t.Fatalf("unit %d rejected: %v", i, err)
				}
				expected.WriteString(unit.Markdown)
				want.Warnings = append(want.Warnings, unit.Warnings...)
				if !public {
					want.References[id] = *unit.Reference
				}
			}
			text, wantText = page.text.String(), expected.String()
			got, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			wantJSON, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, wantJSON) || page.bytes != len(got) || len(got) >= readPageBytes {
				t.Fatalf("envelope parity/bound: measured=%d actual=%d expected=%d", page.bytes, len(got), len(wantJSON))
			}
			// Bound actual serializer work, not elapsed time. Re-encoding growing
			// Markdown, warning or reference prefixes violates this byte budget.
			if calls > 1+4*count || encoded > 2*len(wantJSON) {
				t.Fatalf("nonlinear encoding work: calls=%d encoded=%d output=%d", calls, encoded, len(wantJSON))
			}
		})
	}

	f := liveReadFixture(t)
	blocks := make([]Item, 0, count)
	var page Item
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		// Seed the read fixture in bulk; writer validation is not this oracle.
		type seededBlock struct {
			ID   ID    `json:"id"`
			Data Block `json:"data"`
		}
		seed := make([]seededBlock, 0, count)
		for i := 0; i < count; i++ {
			block := Block{Page: f.page.ID, Position: fmt.Sprintf("A%04d", i), Content: Content{Kind: Paragraph, Text: RichText{{Text: "x", Marks: []Mark{}}}}}
			item := spillItem(spillID(BlockKind, uint64(i+1)), block)
			blocks = append(blocks, item)
			seed = append(seed, seededBlock{ID: item.ID(), Data: block})
		}
		raw, err := json.Marshal(seed)
		if err != nil {
			return err
		}
		for _, query := range []string{
			`INSERT INTO allocated_ids(id,kind) SELECT json_extract(value,'$.id'),'block' FROM json_each(?)`,
			`INSERT INTO items(id,kind,inc,alive,deleted_seq,own_version,aggregate_version,data) SELECT json_extract(value,'$.id'),'block','1',1,'0','1','0',json_extract(value,'$.data') FROM json_each(?)`,
			`INSERT INTO blocks(id,page,parent,position) SELECT json_extract(value,'$.id'),json_extract(value,'$.data.page'),NULL,json_extract(value,'$.data.position') FROM json_each(?)`,
		} {
			if _, err := tx.ExecContext(t.Context(), query, string(raw)); err != nil {
				return err
			}
		}
		page, err = readSelected(t.Context(), tx, f.page.ID, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want, _, _ := ExportMarkdown(page, blocks, nil)
	body, result := liveReadQuery(t, f.in, map[string]any{"page": f.page, "format": "markdown", "limit": count}, false)
	var assembled strings.Builder
	refs := map[ID]AssignedRef{}
	responses := 0
	for {
		responses++
		if len(body) >= readPageBytes || *result.Offset != int64(assembled.Len()) {
			t.Fatal("tiny-block response bound or offset changed")
		}
		assembled.WriteString(*result.Markdown)
		for id, ref := range result.References {
			refs[id] = ref
		}
		if result.Complete {
			break
		}
		cursor := *result.Next
		body, result = liveReadQuery(t, f.in, map[string]any{"cursor": cursor}, false)
		replay, _ := liveReadQuery(t, f.in, map[string]any{"cursor": cursor}, false)
		if !bytes.Equal(body, replay) {
			t.Fatal("tiny-block cursor replay changed bytes")
		}
	}
	if responses != 2 || strings.TrimSpace(assembled.String()) != strings.TrimSpace(want) || len(refs) != count+1 {
		t.Fatalf("tiny-block export parity: responses=%d references=%d", responses, len(refs))
	}
}

func TestIntegratedNotebookReadMarkdownEscapedEnvelopeBound(t *testing.T) {
	t.Parallel()
	text, offset := "", int64(0)
	result := readResult{Markdown: &text, Offset: &offset, Warnings: []Warning{}, References: map[ID]AssignedRef{}}
	page, err := newMarkdownReadPage(&result, json.Marshal)
	if err != nil {
		t.Fatal(err)
	}
	id := spillID(BlockKind, 1)
	unit := markdownReadUnit{Markdown: "\"\\\x01<&>\u2028\u2029", Reference: &AssignedRef{Ref: Ref{ID: id, Inc: 7}, Ver: 7}, Warnings: []Warning{{Code: "escape", Path: "\"\\<&>", Message: strings.Repeat("\x01<&>\u2028\u2029", 20000)}}}
	accepted, err := page.append(unit)
	if err != nil || !accepted {
		t.Fatalf("first escaped unit rejected: %v", err)
	}
	// A duplicate id replaces its value rather than adding a map entry.
	unit.Warnings = nil
	unit.Reference = &AssignedRef{Ref: Ref{ID: id, Inc: 7000}, Ver: 7000}
	if accepted, err := page.append(unit); err != nil || !accepted {
		t.Fatalf("replacement reference rejected: %v", err)
	}
	text = page.text.String()
	before, err := json.Marshal(result)
	if err != nil || page.bytes != len(before) {
		t.Fatalf("escaped accounting mismatch: measured=%d actual=%d err=%v", page.bytes, len(before), err)
	}
	unit.Reference = &AssignedRef{Ref: Ref{ID: id, Inc: 8}, Ver: 8}
	unit.Warnings = []Warning{{Code: "overflow", Message: strings.Repeat("\x01", 100000)}}
	if accepted, err := page.append(unit); err != nil || accepted {
		t.Fatalf("escaped sidecar overflow accepted=%t err=%v", accepted, err)
	}
	text = page.text.String()
	after, err := json.Marshal(result)
	if err != nil || !bytes.Equal(before, after) || page.bytes != len(after) || len(after) >= readPageBytes {
		t.Fatal("rejected unit changed Markdown, warnings, references or byte accounting")
	}
	// Even an oversized first sidecar must not bypass the bound.
	text = ""
	empty := readResult{Markdown: &text, Offset: &offset, Warnings: []Warning{}}
	first, err := newMarkdownReadPage(&empty, json.Marshal)
	if err != nil {
		t.Fatal(err)
	}
	unit.Warnings = []Warning{{Code: "oversized", Message: strings.Repeat("\x01", readPartBytes*2)}}
	if accepted, err := first.append(unit); err != nil || accepted {
		t.Fatalf("oversized first unit accepted=%t err=%v", accepted, err)
	}
}

func TestIntegratedNotebookReadMarkdownMixedExporterParity(t *testing.T) {
	t.Parallel()
	f := liveReadFixture(t)
	rich := func(text string) RichText { return RichText{{Text: text, Marks: []Mark{}}} }
	blocks := []Item{}
	addBlock := func(n uint64, parent *ID, position string, content Content) ID {
		id := spillID(BlockKind, n)
		blocks = append(blocks, spillItem(id, Block{Page: f.page.ID, Parent: parent, Position: position, Content: content}))
		return id
	}
	bullet := addBlock(1, nil, "A", Content{Kind: Bullet, Text: rich("bullet\ncontinuation")})
	number := addBlock(2, &bullet, "A", Content{Kind: Numbered, Text: rich("numbered child")})
	addBlock(3, &number, "A", Content{Kind: Bullet, Text: rich("nested bullet")})
	rootNumber := addBlock(4, nil, "B", Content{Kind: Numbered, Text: rich("numbered root")})
	addBlock(5, &rootNumber, "A", Content{Kind: Paragraph, Text: rich("numbered paragraph\nsecond line")})
	toggle := addBlock(6, nil, "C", Content{Kind: Toggle, Text: rich("toggle summary")})
	addBlock(7, &toggle, "A", Content{Kind: Paragraph, Text: rich("toggle child")})
	addBlock(8, &toggle, "B", Content{Kind: Bullet, Text: rich("toggle bullet")})
	// The exporter can resolve its own page title; other pages are not inputs.
	addBlock(9, nil, "D", Content{Kind: PageLink, Target: f.page.ID})
	addBlock(14, nil, "D1", Content{Kind: PageLink, Target: spillID(PageKind, 99)})
	addBlock(10, nil, "E", Content{Kind: Image, Asset: strings.Repeat("a", 64), Alt: "alt [image]", Caption: rich("caption <&>\nsecond line")})
	database, emptyDB := spillID(DatabaseKind, 1), spillID(DatabaseKind, 2)
	owner := addBlock(11, nil, "F", Content{Kind: InlineDatabase, Database: database})
	emptyOwner := addBlock(12, nil, "G", Content{Kind: InlineDatabase, Database: emptyDB})
	addBlock(13, nil, "H", Content{Kind: Paragraph, Text: rich("after databases")})
	title, selected := spillID(PropertyKind, 1), spillID(PropertyKind, 2)
	option := spillID(OptionKind, 1)
	items := []Item{
		spillItem(database, Database{Block: owner, Title: "Rows", PropertyOrder: []ID{title, selected}, ViewOrder: []ID{}}),
		spillItem(title, Property{Database: database, Definition: PropertyDef{Name: "Name | title", Type: TitleType}, OptionOrder: []ID{}}),
		spillItem(selected, Property{Database: database, Definition: PropertyDef{Name: "Status", Type: SelectType}, OptionOrder: []ID{option}}),
		spillItem(option, Option{Property: selected, Name: "Ready | <&>", Color: Gray}),
		spillItem(emptyDB, Database{Block: emptyOwner, Title: "No properties", PropertyOrder: []ID{}, ViewOrder: []ID{}}),
	}
	dbs := map[ID]DatabaseSnapshot{
		database: {Database: items[0], Schema: Schema{Database: database, PropertyIDs: []ID{title, selected}, Properties: []Property{items[1].Data.(Property), items[2].Data.(Property)}, Options: map[ID][]Option{selected: {items[3].Data.(Option)}}}},
		emptyDB:  {Database: items[4], Schema: Schema{Database: emptyDB, Options: map[ID][]Option{}}},
	}
	for _, id := range []ID{database, emptyDB} {
		for i := 0; i < 3; i++ {
			rid := spillID(RowKind, uint64(len(items)+1))
			cells := map[ID]CellValue{}
			if id == database {
				cells[title] = CellValue{Type: TitleType, Value: fmt.Sprintf("row %d | line\nbreak", i)}
				cells[selected] = CellValue{Type: SelectType, Value: option}
			}
			row := Row{Database: id, Position: fmt.Sprintf("A%d", i), Cells: cells}
			items = append(items, spillItem(rid, row))
			snapshot := dbs[id]
			snapshot.Rows = append(snapshot.Rows, RowValues{Row: rid, Position: row.Position, Cells: cells})
			dbs[id] = snapshot
		}
	}
	var page Item
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		for _, item := range append(append([]Item{}, blocks...), items...) {
			if err := putItem(t.Context(), tx, item); err != nil {
				return err
			}
		}
		var err error
		page, err = readSelected(t.Context(), tx, f.page.ID, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want, exportRefs, wantWarnings := ExportMarkdown(page, blocks, dbs)
	if len(wantWarnings) != 5 {
		t.Fatalf("fixture lost fallback warnings: %+v", wantWarnings)
	}
	// Agent reads intentionally add the page, schema/options and every row,
	// including rows of propertyless databases, as mutation-ready references.
	wantRefs := map[ID]AssignedRef{page.ID(): {Ref: page.Ref, Ver: page.Ver}}
	for _, item := range append(append([]Item{}, blocks...), items...) {
		wantRefs[item.ID()] = AssignedRef{Ref: item.Ref, Ver: item.Ver}
	}
	for _, ref := range exportRefs {
		if wantRefs[ref.ID].Ref != ref {
			t.Fatalf("fixture omitted exporter reference: %+v", ref)
		}
	}
	for _, limit := range []int{2000, 2} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			body, result := liveReadQuery(t, f.in, map[string]any{"page": f.page, "format": "markdown", "limit": limit}, false)
			var text strings.Builder
			warnings := []Warning{}
			refs := map[ID]AssignedRef{}
			responses := 0
			for {
				responses++
				if len(body) >= readPageBytes || *result.Offset != int64(text.Len()) {
					t.Fatal("mixed-fixture byte bound or offset changed")
				}
				text.WriteString(*result.Markdown)
				warnings = append(warnings, result.Warnings...)
				for id, ref := range result.References {
					refs[id] = ref
				}
				if result.Complete {
					break
				}
				body, result = liveReadQuery(t, f.in, map[string]any{"cursor": *result.Next}, false)
			}
			if limit == 2000 && responses != 1 || limit == 2 && responses <= 1 {
				t.Fatalf("fixture did not exercise requested pagination: %d responses", responses)
			}
			if strings.TrimSpace(text.String()) != strings.TrimSpace(want) {
				t.Fatalf("Markdown differs from ExportMarkdown:\ngot:\n%s\nwant:\n%s", text.String(), want)
			}
			if !reflect.DeepEqual(warnings, wantWarnings) {
				t.Fatalf("warnings differ: got=%+v want=%+v", warnings, wantWarnings)
			}
			if !reflect.DeepEqual(refs, wantRefs) {
				t.Fatalf("reference superset differs: got=%+v want=%+v", refs, wantRefs)
			}
		})
	}
}

func TestIntegratedNotebookReadMarkdownSelectionValidation(t *testing.T) {
	t.Parallel()
	f := liveReadFixture(t)
	for _, tc := range []struct{ name, query, reason string }{
		{"tree", `{"format":"markdown"}`, "markdown_page"},
		{"database", fmt.Sprintf(`{"database":%q,"format":"markdown"}`, spillID(DatabaseKind, 1)), "markdown_page"},
		{"mixed", fmt.Sprintf(`{"page":%q,"database":%q,"format":"markdown"}`, f.page.ID, spillID(DatabaseKind, 1)), "markdown_page"},
		{"view", fmt.Sprintf(`{"page":%q,"view":%q,"format":"markdown"}`, f.page.ID, spillID(ViewKind, 1)), "database_query"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.in.Read(t.Context(), []byte(tc.query), false)
			liveReadError(t, err, "invalid", tc.reason)
		})
	}
}

func TestIntegratedNotebookReadPendingWithoutRevisionBackend(t *testing.T) {
	t.Parallel()
	f := liveReadFixture(t)
	_, initial := liveReadQuery(t, f.in, map[string]any{}, false)
	if initial.Pending == nil || !*initial.Pending || string(initial.Revision) != "null" {
		t.Fatalf("unacknowledged initial boundary: %+v", initial)
	}
	if err := f.in.Acknowledge(t.Context(), 1, 0); err != nil {
		t.Fatal(err)
	}
	_, acked := liveReadQuery(t, f.in, map[string]any{}, false)
	if *acked.Pending || string(acked.Revision) != "null" {
		t.Fatalf("clean acknowledged content should not be pending: %+v", acked)
	}
	if _, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, writerAction(f.page, "person edit")); err != nil {
		t.Fatal(err)
	}
	_, dirty := liveReadQuery(t, f.in, map[string]any{}, false)
	if !*dirty.Pending || string(dirty.Revision) != "null" {
		t.Fatalf("unrevisioned person edit should be pending: %+v", dirty)
	}
}

type integratedReadBackend struct {
	*writerBackend
	resolve func(context.Context, string, int64) (*int, bool, error)
}

func (b *integratedReadBackend) NotebookReadRevision(ctx context.Context, store string, outbox int64) (*int, bool, error) {
	return b.resolve(ctx, store, outbox)
}

func TestIntegratedNotebookReadRevisionStatusAndFrozenCursor(t *testing.T) {
	t.Parallel()
	revision, imported := 19, true
	var calledStore string
	var calledOutbox int64
	b := &integratedReadBackend{writerBackend: &writerBackend{}}
	b.resolve = func(_ context.Context, store string, outbox int64) (*int, bool, error) {
		calledStore, calledOutbox = store, outbox
		if revision == 0 {
			return nil, imported, nil
		}
		n := revision
		return &n, imported, nil
	}
	in, _, err := openNotebookFixture(t, t.TempDir(), b, Options{Slug: "notes", Now: func() time.Time { return time.Unix(10000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.Stop)
	_, first := liveReadQuery(t, in, map[string]any{}, false)
	if first.Pending == nil || *first.Pending || string(first.Revision) != "19" || calledStore != in.StoreID() || calledOutbox != 1 {
		t.Fatalf("exact imported boundary not reported: %+v store=%q outbox=%d", first, calledStore, calledOutbox)
	}
	// Acknowledgement removes the outbox row, but its AUTOINCREMENT boundary persists.
	if err := in.Acknowledge(t.Context(), 1, 0); err != nil {
		t.Fatal(err)
	}
	_, acked := liveReadQuery(t, in, map[string]any{}, false)
	if *acked.Pending || string(acked.Revision) != "19" || calledOutbox != 1 {
		t.Fatalf("acknowledged boundary was lost: %+v outbox=%d", acked, calledOutbox)
	}
	revision = 0 // Imported but retention-pruned is not pending.
	_, pruned := liveReadQuery(t, in, map[string]any{}, false)
	if *pruned.Pending || string(pruned.Revision) != "null" {
		t.Fatalf("pruned revision misreported as pending: %+v", pruned)
	}
	revision, imported = 19, false
	_, pending := liveReadQuery(t, in, map[string]any{}, false)
	if !*pending.Pending || string(pending.Revision) != "null" {
		t.Fatalf("unimported boundary misreported: %+v", pending)
	}
	imported = true
	_, err = in.Update(t.Context(), Update{Slug: "notes", OpID: "two-pages", Ops: []Operation{CreatePageOp{CreateFields: CreateFields{Ref: "two"}, Title: "Second"}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, frozen := liveReadQuery(t, in, map[string]any{"limit": 1}, false)
	body, next := liveReadQuery(t, in, map[string]any{"cursor": *frozen.Next}, false)
	revision = 20
	replay, _ := liveReadQuery(t, in, map[string]any{"cursor": *frozen.Next}, false)
	if !bytes.Equal(body, replay) || string(next.Revision) != "19" {
		t.Fatal("cursor revision metadata was refreshed from the host")
	}
	if err := in.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO groups(editor,started_at,last_at,dirty,label) VALUES('person',10000,10000,1,'Person')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, dirty := liveReadQuery(t, in, map[string]any{}, false)
	if !*dirty.Pending || string(dirty.Revision) != "null" {
		t.Fatalf("dirty person state falsely matched a revision: %+v", dirty)
	}
}

func TestIntegratedNotebookReadRevisionLookupFailure(t *testing.T) {
	t.Parallel()
	b := &integratedReadBackend{writerBackend: &writerBackend{}, resolve: func(context.Context, string, int64) (*int, bool, error) {
		return nil, false, errors.New("host lookup unavailable")
	}}
	in, _, err := openNotebookFixture(t, t.TempDir(), b, Options{Slug: "notes"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.Stop)
	_, err = in.Read(t.Context(), []byte(`{}`), false)
	liveReadError(t, err, "unavailable", "snapshot")
	entries, err := os.ReadDir(in.spills.dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("lookup failure retained its capture: %v %v", entries, err)
	}
}

func TestIntegratedNotebookReadOversizedOwnershipReferences(t *testing.T) {
	t.Parallel()
	f := liveReadFixture(t)
	// Seed a retained oversized block directly; ordinary writes remain limited.
	blockID := spillID(BlockKind, 400)
	content := strings.Repeat("한", readPageBytes)
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		if err := putItem(t.Context(), tx, spillItem(blockID, Block{Page: f.page.ID, Position: "A", Content: Content{Kind: Code, Code: "seed"}})); err != nil {
			return err
		}
		payload, err := json.Marshal(Block{Page: f.page.ID, Position: "A", Content: Content{Kind: Code, Code: content}})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(t.Context(), `UPDATE items SET data=? WHERE id=?`, string(payload), blockID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, result := liveReadQuery(t, f.in, map[string]any{"page": f.page}, false)
	var assembled strings.Builder
	part := int64(0)
	for {
		if result.Continuation != nil {
			c := result.Continuation
			if c.Record != blockID || c.Part != part || !utf8.ValidString(c.Data) {
				t.Fatalf("bad ownership continuation: %+v", c)
			}
			assembled.WriteString(c.Data)
			part++
		}
		if result.Complete {
			break
		}
		_, result = liveReadQuery(t, f.in, map[string]any{"cursor": *result.Next}, false)
	}
	var got struct {
		Page    Ref     `json:"page_ref"`
		Content Content `json:"content"`
	}
	if err := json.Unmarshal([]byte(assembled.String()), &got); err != nil || got.Page != f.page || got.Content.Code != content {
		t.Fatalf("ownership conversion lost bytes or references: %v page=%+v", err, got.Page)
	}
}

func TestIntegratedNotebookReadVirtualContinuationReader(t *testing.T) {
	t.Parallel()
	file, err := os.Create(filepath.Join(t.TempDir(), "record"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := io.WriteString(file, "prefix"); err != nil {
		t.Fatal(err)
	}
	r := readAppendAt{base: file, length: 6, tail: bytes.NewReader([]byte("-tail"))}
	for _, tc := range []struct {
		name   string
		offset int64
		length int
		want   string
		eof    bool
	}{
		{"base", 0, 6, "prefix", false},
		{"crossing", 4, 5, "ix-ta", false},
		{"tail", 6, 5, "-tail", false},
		{"short", 9, 4, "il", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buffer := make([]byte, tc.length)
			n, err := r.ReadAt(buffer, tc.offset)
			if string(buffer[:n]) != tc.want || errors.Is(err, io.EOF) != tc.eof || err != nil && !tc.eof {
				t.Fatalf("ReadAt=%q %v; want %q EOF=%t", buffer[:n], err, tc.want, tc.eof)
			}
		})
	}
}
