package notebook

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func editOrder(t *testing.T, f *undoTestStore, page ID) []ID {
	t.Helper()
	var ids []ID
	rows, err := f.s.db.QueryContext(t.Context(), `SELECT b.id FROM blocks b JOIN items i ON i.id=b.id WHERE i.alive=1 AND b.page=? AND b.parent IS NULL ORDER BY b.position,b.id`, page)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id ID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestEditSelectionTupleOrder(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(fmt.Sprintf("duplicate=%t", duplicate), func(t *testing.T) {
			t.Parallel()
			f := newUndoTestStore(t)
			page := vocabID(PageKind, 1)
			destination := f.record(vocabID(PageKind, 2)).Item.Ref
			a := f.block("b", 1, nil, Paragraph)
			h := f.block("b", 2, nil, Paragraph)
			anchor := f.block("b", 3, nil, Paragraph)
			err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
				for id, pos := range map[ID]string{a.ID: "V", h.ID: "V7", anchor.ID: "z"} {
					r, err := loadRecord(t.Context(), tx, id)
					if err != nil {
						return err
					}
					b := r.Item.Data.(Block)
					b.Position = pos
					r.Item.Data = b
					if err := putRecord(t.Context(), tx, r); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			selection := []Target{editTarget(h), editTarget(a)}
			if duplicate {
				result := f.execute("a", &DuplicateBlocksOp{Blocks: selection, After: editTarget(anchor)})
				requireOutcomes(t, result, "applied")
				if len(result.Copies) != 2 || result.Copies[0].Source != a || result.Copies[1].Source != h {
					t.Fatalf("copy order=%+v", result.Copies)
				}
				got := editOrder(t, f, page)
				want := []ID{a.ID, h.ID, anchor.ID, result.Copies[0].Ref.ID, result.Copies[1].Ref.ID}
				if !slices.Equal(got, want) {
					t.Fatalf("order=%v want=%v", got, want)
				}
			} else {
				requireOutcomes(t, f.execute("a", &MoveBlocksOp{Blocks: selection, Place: Placement{Page: editTarget(destination)}}), "applied")
				got := editOrder(t, f, destination.ID)
				if !slices.Equal(got, []ID{a.ID, h.ID}) {
					t.Fatalf("selection reversed=%v", got)
				}
			}
		})
	}
}

func TestEditMoveRebalanceUndoKeepsNeighborsAndEffects(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	page := f.record(vocabID(PageKind, 1)).Item.Ref
	refs := make([]Ref, 4)
	for i := range refs {
		refs[i] = f.block("b", byte(i+1), nil, Paragraph)
	}
	err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		for i, pos := range []string{strings.Repeat("0", 31) + "1", "V", "y", "z"} {
			r, err := loadRecord(t.Context(), tx, refs[i].ID)
			if err != nil {
				return err
			}
			b := r.Item.Data.(Block)
			b.Position = pos
			r.Item.Data = b
			if err := putRecord(t.Context(), tx, r); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before := f.record(refs[1].ID)
	result := f.execute("a", &MoveBlocksOp{Blocks: []Target{editTarget(refs[2])}, Place: Placement{Page: editTarget(page), Before: &Target{Ref: &refs[0]}}})
	requireOutcomes(t, result, "applied")
	if len(result.Records) != 5 {
		t.Fatalf("canonical effects omit rekeys/aggregate: %d", len(result.Records))
	}
	if got := f.record(refs[1].ID); got.Item.Ver != before.Item.Ver {
		t.Fatal("physical rekey advanced item ver")
	}
	if got := f.undo("a", false); got.Applied != 1 {
		t.Fatalf("undo=%+v", got)
	}
	want := []ID{refs[0].ID, refs[1].ID, refs[2].ID, refs[3].ID}
	if got := editOrder(t, f, page.ID); !slices.Equal(got, want) {
		t.Fatalf("undo order=%v want=%v", got, want)
	}
}

func TestEditClosedIdentityLedgerDoesNotEvictAllUndo(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	page := f.record(vocabID(PageKind, 1)).Item.Ref
	for i := 0; i < 120; i++ {
		result, err := f.tryExecute(editOptions{Browser: true, Session: "a", ActionID: fmt.Sprintf("%0128d", i), UndoBudget: 4096}, &SetPageTitleOp{Page: editTarget(page), Title: fmt.Sprint(i)})
		if err != nil {
			t.Fatal(err)
		}
		requireOutcomes(t, result, "applied")
	}
	if f.count("a", "undo") == 0 || f.count("a", "undo") >= MaxUndoEntries {
		t.Fatalf("bounded undo count=%d", f.count("a", "undo"))
	}
	if got := f.undo("a", false); got.Applied != 1 {
		t.Fatalf("closed ids disabled undo=%+v", got)
	}
	if title := f.record(page.ID).Item.Data.(Page).Title; title != "118" {
		t.Fatal(title)
	}
}

func TestEditPositiveStateAndItemGuards(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	page := f.record(vocabID(PageKind, 1)).Item
	state, err := f.s.state(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.tryExecute(editOptions{Guards: &Guards{State: &State{Store: state.Store, Seq: state.Seq}, Items: []ItemGuard{{Ref: page.Ref, Ver: page.Ver}}}}, &SetPageTitleOp{Page: editTarget(page.Ref), Title: "guard passed"})
	if err != nil {
		t.Fatal(err)
	}
	requireOutcomes(t, result, "applied")
	if f.record(page.ID()).Item.Data.(Page).Title != "guard passed" {
		t.Fatal("positive guards did not apply")
	}
	if _, err := f.tryExecute(editOptions{Guards: &Guards{State: &State{Store: state.Store, Seq: state.Seq}}}, &SetPageTitleOp{Page: editTarget(page.Ref), Title: "must not apply"}); err == nil {
		t.Fatal("stale state guard applied")
	}
}

func TestEditOwnedLanguageAndURLAdmission(t *testing.T) {
	for _, field := range []string{"language", "text-url", "caption-url"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			f := newUndoTestStore(t)
			page := f.record(vocabID(PageKind, 1)).Item.Ref
			id := vocabID(BlockKind, 1)
			content := editText(Paragraph, "linked")
			switch field {
			case "language":
				content = Content{Kind: Code, Language: strings.Repeat("한", MaxNameCodePoints+1)}
			case "text-url":
				content.Text[0].Link = &Link{URL: "https://example.com/" + strings.Repeat("a", MaxURLBytes)}
			case "caption-url":
				content = Content{Kind: Image, Asset: strings.Repeat("a", 64), Caption: RichText{{Text: "caption", Marks: []Mark{}, Link: &Link{URL: "https://example.com/" + strings.Repeat("a", MaxURLBytes)}}}}
			}
			requireOutcomes(t, f.execute("a", &CreateBlockOp{CreateFields: CreateFields{Ref: "large", ProposedID: &id}, Place: Placement{Page: editTarget(page)}, Content: content}), "refused:item_limit")
		})
	}
}

func TestEditSecondPlacementRebalanceUndo(t *testing.T) {
	for _, compound := range []bool{false, true} {
		t.Run(fmt.Sprintf("compound=%t", compound), func(t *testing.T) {
			t.Parallel()
			f := newUndoTestStore(t)
			page := f.record(vocabID(PageKind, 1)).Item.Ref
			refs := make([]Ref, 4)
			for i := range refs {
				refs[i] = f.block("b", byte(i+1), nil, Paragraph)
			}
			positions := []string{strings.Repeat("0", 31) + "y", strings.Repeat("0", 30) + "1", "a", "b"}
			if compound {
				positions[2], positions[3] = "y", "z"
			}
			err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
				for i, pos := range positions {
					r, err := loadRecord(t.Context(), tx, refs[i].ID)
					if err != nil {
						return err
					}
					b := r.Item.Data.(Block)
					b.Position = pos
					r.Item.Data = b
					if err := putRecord(t.Context(), tx, r); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			place := Placement{Page: editTarget(page), Before: &Target{Ref: &refs[1]}}
			if compound {
				unit := "move-create-move"
				id := vocabID(BlockKind, 5)
				meta := OperationMeta{UnitID: &unit}
				requireOutcomes(t, f.execute("a", &MoveBlocksOp{OperationMeta: meta, Blocks: []Target{editTarget(refs[2])}, Place: place}, &CreateBlockOp{OperationMeta: meta, CreateFields: CreateFields{Ref: "extra", ProposedID: &id}, Place: Placement{Page: editTarget(page), After: &Target{Ref: &refs[2]}}, Content: editText(Paragraph, "extra")}, &MoveBlocksOp{OperationMeta: meta, Blocks: []Target{editTarget(refs[3])}, Place: Placement{Page: editTarget(page)}}), "applied", "applied", "applied")
				if f.record(refs[1].ID).Item.Data.(Block).Position == positions[1] {
					t.Fatal("compound did not trigger rekey")
				}
			} else {
				requireOutcomes(t, f.execute("a", &MoveBlocksOp{Blocks: []Target{editTarget(refs[2]), editTarget(refs[3])}, Place: place}), "applied")
				if f.record(refs[1].ID).Item.Data.(Block).Position == positions[1] {
					t.Fatal("second placement did not trigger rekey")
				}
			}
			if got := f.undo("a", false); got.Applied != 1 {
				t.Fatalf("undo=%+v", got)
			}
			want := []ID{refs[0].ID, refs[1].ID, refs[2].ID, refs[3].ID}
			if got := editOrder(t, f, page.ID); !slices.Equal(got, want) {
				t.Fatalf("undo=%v want=%v", got, want)
			}
		})
	}
}

func TestEditRetainedURLExcessCannotMultiply(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	ref := f.block("b", 1, nil, Paragraph)
	first := "https://example.com/" + strings.Repeat("a", MaxURLBytes+100)
	f.change("b", editOptions{}, func(e *edit) error {
		r, err := loadRecord(e.ctx, e.tx, ref.ID)
		if err != nil {
			return err
		}
		b := r.Item.Data.(Block)
		b.Content.Text = RichText{{Text: "one", Marks: []Mark{}, Link: &Link{URL: first}}}
		r.Item.Data = b
		return e.write(r, ContentGroup)
	})
	content := f.record(ref.ID).Item.Data.(Block).Content
	content.Text = append(content.Text, Run{Text: "two", Marks: []Mark{}, Link: &Link{URL: "https://example.com/" + strings.Repeat("b", MaxURLBytes+1)}})
	base := Version(1)
	requireOutcomes(t, f.execute("a", &SetBlockOp{Block: editTarget(ref), Content: content, BaseV: &base}), "refused:item_limit")
}

func TestEditIndependentConversionUndoComponents(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	a := f.block("b", 1, nil, Paragraph)
	b := f.block("b", 2, nil, Paragraph)
	requireOutcomes(t, f.execute("a", &TurnBlocksOp{Blocks: []Target{editTarget(a), editTarget(b)}, Kind: Heading1}), "applied")
	base := Version(1)
	f.execute("b", &SetBlockOp{Block: editTarget(b), Content: editText(Heading1, "later"), BaseV: &base})
	got := f.undo("a", false)
	if got.Applied != 1 || got.LaterEdits != 1 || f.record(a.ID).Item.Data.(Block).Content.Kind != Paragraph || f.record(b.ID).Item.Data.(Block).Content.Kind != Heading1 {
		t.Fatalf("independent conversions=%+v", got)
	}
	if redo := f.undo("a", true); redo.Applied != 1 || f.record(a.ID).Item.Data.(Block).Content.Kind != Heading1 || f.record(b.ID).Item.Data.(Block).Content.Text[0].Text != "later" {
		t.Fatalf("redo replayed skipped change=%+v", redo)
	}
}
