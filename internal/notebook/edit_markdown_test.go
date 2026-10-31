package notebook

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func runEditMarkdown(t *testing.T, f *undoTestStore, browser bool, ops []Operation, fn func(*edit) error) (*edit, error) {
	t.Helper()
	f.seq++
	var e *edit
	err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		e = &edit{ctx: t.Context(), tx: tx, seq: f.seq, browser: browser, session: "a", refs: map[string]Ref{}}
		if err := e.reserveMarkdownNames(ops); err != nil {
			return err
		}
		return fn(e)
	})
	return e, err
}

func TestEditMarkdownCurrentDestinationDepth(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	page := f.record(vocabID(PageKind, 1)).Item.Ref
	var ancestor *ID
	var ancestorRef Ref
	for n := byte(1); n <= 4; n++ {
		ancestorRef = f.block("b", n, ancestor, Bullet)
		id := ancestorRef.ID
		ancestor = &id
	}
	destination := f.block("b", 5, nil, Toggle)
	e, err := runEditMarkdown(t, f, false, nil, func(e *edit) error {
		if err := e.apply(&MoveBlocksOp{Blocks: []Target{editTarget(destination)}, Place: Placement{Page: editTarget(page), Parent: &Target{Ref: &ancestorRef}}}); err != nil {
			return err
		}
		e.refs["destination"] = destination
		if err := e.markdownToBlocks(&MarkdownToBlocksOp{Place: Placement{Page: editTarget(page), Parent: &Target{Tmp: "destination"}}, Markdown: "- first\n  - second\n    - third"}); err != nil {
			return err
		}
		return e.validateStructure()
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.result.Warnings) != 2 {
		t.Fatalf("warnings=%+v", e.result.Warnings)
	}
	for _, warning := range e.result.Warnings {
		if warning.Code != "nesting_flattened" {
			t.Fatalf("warning=%+v", warning)
		}
	}
	for i, text := range []string{"first", "second", "third"} {
		ref := e.refs[fmt.Sprintf("$md%d", i+1)]
		block := f.record(ref.ID).Item.Data.(Block)
		if !sameParent(block.Parent, &destination.ID) || markdownPlain(block.Content.Text) != text {
			t.Fatalf("block=%+v want parent=%s text=%q", block, destination.ID, text)
		}
	}
}

func TestEditMarkdownAssets(t *testing.T) {
	t.Parallel()
	hash := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name    string
		path    string
		aliases []AssetInput
		known   bool
		want    string
	}{
		{name: "export alias", path: "assets/" + hash, known: true},
		{name: "bare hash", path: hash, known: true},
		{name: "named alias", path: "photo.png", aliases: []AssetInput{{Path: "photo.png", Asset: AssetTarget{Hash: hash}}}, known: true},
		{name: "missing asset", path: "assets/" + hash, want: "markdown"},
		{name: "unknown alias", path: "photo.png", known: true, want: "markdown"},
		{name: "external", path: "https://example.com/photo.png", known: true, want: "markdown"},
		{name: "unprepared", path: "photo.png", aliases: []AssetInput{{Path: "photo.png", Asset: AssetTarget{Tmp: "upload"}}}, want: "assets_unprepared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUndoTestStore(t)
			page := f.record(vocabID(PageKind, 1)).Item.Ref
			if tc.known {
				if _, err := f.s.db.ExecContext(t.Context(), `INSERT INTO assets(hash,mime,width,height,bytes,live_refs,deleted_refs,protected_until) VALUES(?,'image/png',1,1,1,0,0,0)`, hash); err != nil {
					t.Fatal(err)
				}
			}
			e, err := runEditMarkdown(t, f, false, nil, func(e *edit) error {
				return e.markdownToBlocks(&MarkdownToBlocksOp{Place: Placement{Page: editTarget(page)}, Markdown: "before ![image](" + tc.path + ") after", AssetRefs: tc.aliases})
			})
			if tc.want != "" {
				var failure *editFailure
				if !errors.As(err, &failure) || failure.Reason != tc.want || !strings.Contains(failure.Detail, tc.path) {
					t.Fatalf("error=%v detail=%+v", err, failure)
				}
				var count int
				if err := f.s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM blocks`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("failed conversion left %d blocks: %v", count, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			image := f.record(e.refs["$md2"].ID).Item.Data.(Block).Content
			if len(e.refs) != 3 || image.Kind != Image || image.Asset != hash {
				t.Fatalf("refs=%+v image=%+v", e.refs, image)
			}
		})
	}
}

func TestEditMarkdownCallWideNamesAndTable(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	page := f.record(vocabID(PageKind, 1)).Item.Ref
	reserved := &CreateBlockOp{CreateFields: CreateFields{Ref: "$md1"}}
	reservedTitle := &CreateBlockOp{CreateFields: CreateFields{Ref: "$md3.title"}}
	e, err := runEditMarkdown(t, f, false, []Operation{reserved, reservedTitle}, func(e *edit) error {
		first := &MarkdownToBlocksOp{Place: Placement{Page: editTarget(page)}, Markdown: "first"}
		if err := e.markdownToBlocks(first); err != nil {
			return err
		}
		firstRef := e.refs["$md2"]
		e.index = 3
		return e.markdownToBlocks(&MarkdownToBlocksOp{Place: Placement{Page: editTarget(page), After: &Target{Ref: &firstRef}}, Markdown: "| Name | Note |\n| --- | --- |\n| Alice | hello |"})
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Kind{"$md2": BlockKind, "$md4": DatabaseKind, "$md5": BlockKind, "$md4.title": PropertyKind, "$md4.view": ViewKind, "$md6": PropertyKind, "$md7": RowKind}
	if len(e.refs) != len(want) {
		t.Fatalf("refs=%+v", e.refs)
	}
	for name, kind := range want {
		if e.refs[name].ID.Kind() != kind {
			t.Fatalf("%s=%+v want kind=%s", name, e.refs[name], kind)
		}
	}
	for i, create := range e.result.MarkdownCreates {
		wantIndex := 3
		if i == 0 {
			wantIndex = 0
		}
		if create.Index != wantIndex || e.refs[create.Name].ID == "" {
			t.Fatalf("creation=%+v want index=%d", create, wantIndex)
		}
	}
	if len(e.result.MarkdownCreates) != len(want) {
		t.Fatalf("creations=%+v", e.result.MarkdownCreates)
	}
	database := f.record(e.refs["$md4"].ID).Item.Data.(Database)
	if database.Block != e.refs["$md5"].ID || !reflect.DeepEqual(database.PropertyOrder, []ID{e.refs["$md4.title"].ID, e.refs["$md6"].ID}) {
		t.Fatalf("database=%+v", database)
	}
	var value string
	if err := f.s.db.QueryRowContext(t.Context(), `SELECT value FROM cells WHERE row_id=? AND property_id=?`, e.refs["$md7"].ID, e.refs["$md6"].ID).Scan(&value); err != nil || !strings.Contains(value, "hello") {
		t.Fatalf("cell=%s err=%v", value, err)
	}
}

func TestEditMarkdownBrowserProposedIDs(t *testing.T) {
	t.Parallel()
	for _, tc := range []string{"exact", "missing", "extra", "wrong kind", "duplicate id", "wrong name", "allocated"} {
		t.Run(tc, func(t *testing.T) {
			t.Parallel()
			f := newUndoTestStore(t)
			page := f.record(vocabID(PageKind, 1)).Item.Ref
			proposed := []ProposedItem{
				{Ref: "$md1", ProposedID: vocabID(DatabaseKind, 40)},
				{Ref: "$md2", ProposedID: vocabID(BlockKind, 40)},
				{Ref: "$md1.title", ProposedID: vocabID(PropertyKind, 40)},
				{Ref: "$md1.view", ProposedID: vocabID(ViewKind, 40)},
				{Ref: "$md3", ProposedID: vocabID(PropertyKind, 41)},
				{Ref: "$md4", ProposedID: vocabID(RowKind, 40)},
			}
			switch tc {
			case "missing":
				proposed = proposed[:len(proposed)-1]
			case "extra":
				proposed = append(proposed, ProposedItem{Ref: "$md5", ProposedID: vocabID(BlockKind, 41)})
			case "wrong kind":
				proposed[4].ProposedID = vocabID(RowKind, 41)
			case "duplicate id":
				proposed[4].ProposedID = proposed[2].ProposedID
			case "wrong name":
				proposed[5].Ref = "$md99"
			case "allocated":
				if _, err := f.s.db.ExecContext(t.Context(), `INSERT INTO allocated_ids(id,kind) VALUES(?,'row')`, proposed[5].ProposedID); err != nil {
					t.Fatal(err)
				}
			}
			e, err := runEditMarkdown(t, f, true, nil, func(e *edit) error {
				if err := e.markdownToBlocks(&MarkdownToBlocksOp{Place: Placement{Page: editTarget(page)}, Markdown: "| Name | Note |\n| --- | --- |\n| Alice | hello |", ProposedIDs: proposed}); err != nil {
					return err
				}
				return e.validateStructure()
			})
			if tc != "exact" {
				if err == nil {
					t.Fatal("invalid proposals accepted")
				}
				var count int
				if err := f.s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM items WHERE kind<>'page'`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("partial table: count=%d err=%v", count, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range proposed {
				if e.refs[item.Ref].ID != item.ProposedID {
					t.Fatalf("%s=%+v want=%s", item.Ref, e.refs[item.Ref], item.ProposedID)
				}
				var inc string
				if err := f.s.db.QueryRowContext(t.Context(), `SELECT inc FROM created WHERE session='a' AND id=?`, item.ProposedID).Scan(&inc); err != nil || inc != sequenceString(f.seq) {
					t.Fatalf("created inc=%s err=%v", inc, err)
				}
			}
		})
	}
}

func TestEditMarkdownBrowserCallWideNames(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	page := f.record(vocabID(PageKind, 1)).Item.Ref
	firstID, secondID := vocabID(BlockKind, 40), vocabID(BlockKind, 41)
	e, err := runEditMarkdown(t, f, true, nil, func(e *edit) error {
		if err := e.markdownToBlocks(&MarkdownToBlocksOp{Place: Placement{Page: editTarget(page)}, Markdown: "first", ProposedIDs: []ProposedItem{{Ref: "$md1", ProposedID: firstID}}}); err != nil {
			return err
		}
		e.index++
		return e.markdownToBlocks(&MarkdownToBlocksOp{Place: Placement{Page: editTarget(page), After: &Target{Tmp: "$md1"}}, Markdown: "second", ProposedIDs: []ProposedItem{{Ref: "$md2", ProposedID: secondID}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.refs["$md1"].ID != firstID || e.refs["$md2"].ID != secondID || len(e.result.MarkdownCreates) != 2 || e.result.MarkdownCreates[1].Index != 1 {
		t.Fatalf("refs=%+v creates=%+v", e.refs, e.result.MarkdownCreates)
	}
}

func TestEditMarkdownInvalidDestinations(t *testing.T) {
	t.Parallel()
	for _, tc := range []string{"too deep", "stale parent", "leaf parent", "wrong page", "wrong anchor"} {
		t.Run(tc, func(t *testing.T) {
			t.Parallel()
			f := newUndoTestStore(t)
			page := f.record(vocabID(PageKind, 1)).Item.Ref
			parent := f.block("b", 1, nil, Bullet)
			place := Placement{Page: editTarget(page), Parent: &Target{Ref: &parent}}
			switch tc {
			case "too deep":
				for n := byte(2); n <= MaxBlockDepth; n++ {
					parent = f.block("b", n, &parent.ID, Bullet)
				}
			case "stale parent":
				parent.Inc++
			case "leaf parent":
				parent = f.block("b", 2, nil, Paragraph)
			case "wrong page":
				place.Page = editTarget(f.record(vocabID(PageKind, 2)).Item.Ref)
			case "wrong anchor":
				anchor := f.block("b", 2, nil, Paragraph)
				place.Before = &Target{Ref: &anchor}
			}
			before := f.record(page.ID).Item.Ver
			e, err := runEditMarkdown(t, f, false, nil, func(e *edit) error {
				return e.markdownToBlocks(&MarkdownToBlocksOp{Place: place, Markdown: "new"})
			})
			var failure *editFailure
			if !errors.As(err, &failure) || len(e.refs) != 0 || f.record(page.ID).Item.Ver != before {
				t.Fatalf("error=%v refs=%+v", err, e.refs)
			}
		})
	}
}

func TestEditMarkdownResolvedPlacementDoesNotCaptureParserNames(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	page := f.record(vocabID(PageKind, 1)).Item.Ref
	parent := f.block("b", 1, nil, Bullet)
	anchor := f.block("b", 2, &parent.ID, Paragraph)
	e, err := runEditMarkdown(t, f, false, nil, func(e *edit) error {
		e.refs["$md1"] = parent
		e.refs["$md2"] = anchor
		return e.markdownToBlocks(&MarkdownToBlocksOp{Place: Placement{Page: editTarget(page), Parent: &Target{Tmp: "$md1"}, Before: &Target{Tmp: "$md2"}}, Markdown: "- inserted\n  - nested"})
	})
	if err != nil {
		t.Fatal(err)
	}
	inserted := f.record(e.refs["$md3"].ID).Item.Data.(Block)
	nested := f.record(e.refs["$md4"].ID).Item.Data.(Block)
	if !sameParent(inserted.Parent, &parent.ID) || !sameParent(nested.Parent, idPointer(e.refs["$md3"].ID)) || inserted.Position >= f.record(anchor.ID).Item.Data.(Block).Position {
		t.Fatalf("inserted=%+v nested=%+v", inserted, nested)
	}
}
