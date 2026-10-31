package notebook

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

func editTarget(ref Ref) Target { return Target{Ref: &ref} }
func editText(kind ContentKind, text string) Content {
	c := Content{Kind: kind, Text: RichText{{Text: text, Marks: []Mark{}}}}
	if kind == Callout {
		c.Tone = NeutralTone
	}
	return c
}

func (f *undoTestStore) execute(session string, ops ...Operation) editResult {
	f.t.Helper()
	result, err := f.tryExecute(editOptions{Browser: true, Session: session}, ops...)
	if err != nil {
		f.t.Fatal(err)
	}
	return result
}

func (f *undoTestStore) tryExecute(opts editOptions, ops ...Operation) (editResult, error) {
	f.t.Helper()
	f.seq++
	f.now = f.now.Add(100 * time.Millisecond)
	opts.Seq, opts.Now = f.seq, f.now
	var result editResult
	err := f.s.transaction(f.t.Context(), func(tx *sql.Tx) error {
		var err error
		result, err = applyEdit(f.t.Context(), tx, opts, ops)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(f.t.Context(), `UPDATE meta SET seq=? WHERE singleton=1`, sequenceString(f.seq))
		return err
	})
	return result, err
}

func requireOutcomes(t *testing.T, result editResult, expected ...string) {
	t.Helper()
	if len(result.Results) != len(expected) {
		t.Fatalf("results=%+v want=%v", result.Results, expected)
	}
	for i, want := range expected {
		got := result.Results[i].Status
		if result.Results[i].Reason != "" {
			got += ":" + result.Results[i].Reason
		}
		if got != want {
			t.Fatalf("result[%d]=%s want=%s", i, got, want)
		}
	}
}

func TestEditPartialUnitsAndStrictCalls(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	page := f.record(vocabID(PageKind, 1)).Item.Ref
	other := f.record(vocabID(PageKind, 2)).Item.Ref
	block := f.block("b", 1, nil, Paragraph)
	f.text("b", block, "abcdef", editOptions{})
	unit := "split"
	newID := vocabID(BlockKind, 2)
	base := Version(1)
	split := []Operation{
		&SetBlockOp{OperationMeta: OperationMeta{UnitID: &unit, Structure: []StructureCheck{{Block: editTarget(block), Page: editTarget(page)}}}, Block: editTarget(block), Content: editText(Paragraph, "abc"), BaseV: &base},
		&CreateBlockOp{OperationMeta: OperationMeta{UnitID: &unit}, CreateFields: CreateFields{Ref: "tail", ProposedID: &newID}, Place: Placement{Page: editTarget(page), After: &Target{Ref: &block}}, Content: editText(Paragraph, "def")},
	}
	f.execute("b", &MoveBlocksOp{Blocks: []Target{editTarget(block)}, Place: Placement{Page: editTarget(other)}})
	results := f.execute("a", append(split, &SetPageTitleOp{Page: editTarget(page), Title: "independent"})...)
	requireOutcomes(t, results, "refused:structure", "refused:structure", "applied")
	if results.Results[0].FailedIndex == nil || *results.Results[0].FailedIndex != 0 || len(results.Refs) != 0 {
		t.Fatalf("failed unit leaked: %+v", results)
	}
	if f.record(block.ID).Item.Data.(Block).Content.Text[0].Text != "abcdef" {
		t.Fatal("failed split changed content")
	}
	if f.count("a", "undo") != 1 {
		t.Fatal("failed unit recorded undo")
	}
	agent := []Operation{&SetPageTitleOp{Page: editTarget(page), Title: "must roll back"}, &SetBlockOp{Block: editTarget(Ref{ID: block.ID, Inc: block.Inc + 100}), Content: editText(Paragraph, "stale")}}
	if _, err := f.tryExecute(editOptions{}, agent...); err == nil {
		t.Fatal("agent partially succeeded")
	}
	if f.record(page.ID).Item.Data.(Page).Title != "independent" {
		t.Fatal("strict refusal leaked title")
	}
}

func TestEditSplitAnchorsAndMergeRaces(t *testing.T) {
	for _, race := range []string{"delete-next", "delete-source", "move-source", "split-first"} {
		t.Run(race, func(t *testing.T) {
			t.Parallel()
			f := newUndoTestStore(t)
			page := f.record(vocabID(PageKind, 1)).Item.Ref
			other := f.record(vocabID(PageKind, 2)).Item.Ref
			source := f.block("b", 1, nil, Paragraph)
			next := f.block("b", 2, nil, Paragraph)
			f.text("b", source, "abcdef", editOptions{})
			unit := "split"
			id := vocabID(BlockKind, 3)
			base := Version(1)
			split := []Operation{&SetBlockOp{OperationMeta: OperationMeta{UnitID: &unit, Structure: []StructureCheck{{Block: editTarget(source), Page: editTarget(page)}}}, Block: editTarget(source), Content: editText(Paragraph, "abc"), BaseV: &base}, &CreateBlockOp{OperationMeta: OperationMeta{UnitID: &unit}, CreateFields: CreateFields{Ref: "tail", ProposedID: &id}, Place: Placement{Page: editTarget(page), After: &Target{Ref: &source}}, Content: editText(Paragraph, "def")}}
			switch race {
			case "delete-next":
				f.execute("b", &DeleteBlocksOp{Blocks: []Target{editTarget(next)}})
			case "delete-source":
				f.execute("b", &DeleteBlocksOp{Blocks: []Target{editTarget(source)}})
			case "move-source":
				f.execute("b", &MoveBlocksOp{Blocks: []Target{editTarget(source)}, Place: Placement{Page: editTarget(other)}})
			}
			result := f.execute("a", split...)
			if race == "delete-source" {
				requireOutcomes(t, result, "ignored:gone", "ignored:gone")
				return
			}
			if race == "move-source" {
				requireOutcomes(t, result, "refused:structure", "refused:structure")
				return
			}
			requireOutcomes(t, result, "applied", "applied")
			if got := f.record(id).Item.Data.(Block).Content.Text[0].Text; got != "def" {
				t.Fatal(got)
			}
			if got := f.undo("a", false); got.Applied != 1 {
				t.Fatalf("split undo=%+v", got)
			}
			if f.record(id).Alive || f.record(source.ID).Item.Data.(Block).Content.Text[0].Text != "abcdef" {
				t.Fatal("split undo incomplete")
			}
			f.undo("a", true)
			tail := f.record(id).Item.Ref
			merge := "merge"
			base2 := Version(1)
			compound := []Operation{&SetBlockOp{OperationMeta: OperationMeta{UnitID: &merge, Structure: []StructureCheck{{Block: editTarget(tail), Page: editTarget(page), After: &Target{Ref: &source}}}}, Block: editTarget(source), Content: editText(Paragraph, "abcdef"), BaseV: &base2}, &DeleteBlocksOp{OperationMeta: OperationMeta{UnitID: &merge}, Blocks: []Target{editTarget(tail)}}}
			f.execute("b", &MoveBlocksOp{Blocks: []Target{editTarget(source)}, Place: Placement{Page: editTarget(other)}})
			requireOutcomes(t, f.execute("a", compound...), "refused:structure", "refused:structure")
			if !f.record(tail.ID).Alive || f.record(source.ID).Item.Data.(Block).Content.Text[0].Text != "abc" {
				t.Fatal("failed merge changed half")
			}
		})
	}
}

func TestEditKindsInBothReceiveOrders(t *testing.T) {
	for _, kind := range []ContentKind{Paragraph, Toggle} {
		for _, remoteFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/remote=%t", kind, remoteFirst), func(t *testing.T) {
				t.Parallel()
				f := newUndoTestStore(t)
				block := f.block("b", 1, nil, kind)
				var child Ref
				if kind == Toggle {
					child = f.block("b", 2, &block.ID, Paragraph)
				}
				base := Version(1)
				set := &SetBlockOp{Block: editTarget(block), Content: editText(Paragraph, "Draft"), BaseV: &base}
				turn := &TurnBlocksOp{Blocks: []Target{editTarget(block)}, Kind: Heading1}
				if kind == Toggle {
					turn.Kind = Todo
				}
				if remoteFirst {
					f.execute("b", turn)
					requireOutcomes(t, f.execute("a", set), "applied")
				} else {
					f.execute("a", set)
					requireOutcomes(t, f.execute("b", turn), "applied")
				}
				got := f.record(block.ID).Item
				expected := turn.Kind
				if remoteFirst {
					expected = Paragraph
				}
				b := got.Data.(Block)
				if got.Ref != block || b.Content.Kind != expected || b.Content.Text[0].Text != "Draft" {
					t.Fatalf("content=%+v", got)
				}
				if kind == Toggle && f.record(child.ID).Item.Data.(Block).Parent != nil {
					t.Fatal("hoisted child was reattached")
				}
			})
		}
	}
	t.Run("forbidden-and-identity", func(t *testing.T) {
		t.Parallel()
		f := newUndoTestStore(t)
		page := f.record(vocabID(PageKind, 1)).Item.Ref
		id := vocabID(BlockKind, 1)
		f.execute("a", &CreateBlockOp{CreateFields: CreateFields{Ref: "link", ProposedID: &id}, Place: Placement{Page: editTarget(page)}, Content: Content{Kind: PageLink, Target: page.ID}})
		ref := f.record(id).Item.Ref
		base := Version(1)
		requireOutcomes(t, f.execute("b", &SetBlockOp{Block: editTarget(ref), Content: editText(Paragraph, "mine"), BaseV: &base}), "ignored:kind")
		if _, err := f.tryExecute(editOptions{}, &SetBlockOp{Block: editTarget(ref), Content: editText(Code, "bad")}); err == nil {
			t.Fatal("agent accepted forbidden conversion")
		}
		requireOutcomes(t, f.execute("b", &SetBlockOp{Block: editTarget(ref), Content: Content{Kind: PageLink, Target: vocabID(PageKind, 2)}, BaseV: &base}), "applied")
	})
}

func TestEditCurrentTreeLimitsCyclesAndCascade(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	first := f.record(vocabID(PageKind, 1)).Item.Ref
	second := f.record(vocabID(PageKind, 2)).Item.Ref
	parent := first
	for i := byte(3); i <= 9; i++ {
		id := vocabID(PageKind, i)
		result := f.execute("a", &CreatePageOp{CreateFields: CreateFields{Ref: fmt.Sprint(i), ProposedID: &id}, Parent: &Target{Ref: &parent}, Title: "nested"})
		requireOutcomes(t, result, "applied")
		parent = result.Refs[fmt.Sprint(i)]
	}
	requireOutcomes(t, f.execute("b", &MovePageOp{Page: editTarget(first), Parent: &Target{Ref: &parent}}), "refused:structure")
	requireOutcomes(t, f.execute("b", &MovePageOp{Page: editTarget(first), Parent: &Target{Ref: &second}}), "refused:structure")
	block := f.block("b", 1, nil, Toggle)
	f.block("b", 2, &block.ID, Paragraph)
	requireOutcomes(t, f.execute("a", &DeletePageOp{Page: editTarget(first)}), "applied")
	if f.record(parent.ID).Alive || f.record(block.ID).Alive {
		t.Fatal("page deletion did not cascade")
	}
	requireOutcomes(t, f.execute("b", &DeletePageOp{Page: editTarget(second)}), "refused:last_page")
	requireOutcomes(t, f.execute("b", &SetPageTitleOp{Page: editTarget(parent), Title: "stale"}), "ignored:gone")
	if got := f.undo("a", false); got.Applied != 1 {
		t.Fatalf("cascade restore=%+v", got)
	}
	requireOutcomes(t, f.execute("b", &SetPageTitleOp{Page: editTarget(first), Title: "stale after revive"}), "ignored:gone")
	if f.record(first.ID).Item.Ref.Inc == first.Inc {
		t.Fatal("revive retained incarnation")
	}
}

func TestEditBlockDepthAndCrossPageVersions(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	page := f.record(vocabID(PageKind, 1)).Item.Ref
	other := f.record(vocabID(PageKind, 2)).Item.Ref
	root := f.block("b", 1, nil, Toggle)
	parent := root
	for i := byte(2); i <= 6; i++ {
		id := vocabID(BlockKind, i)
		r := f.execute("a", &CreateBlockOp{CreateFields: CreateFields{Ref: fmt.Sprint(i), ProposedID: &id}, Place: Placement{Page: editTarget(page), Parent: &Target{Ref: &parent}}, Content: editText(Toggle, "")})
		requireOutcomes(t, r, "applied")
		parent = r.Refs[fmt.Sprint(i)]
	}
	requireOutcomes(t, f.execute("a", &MoveBlocksOp{Blocks: []Target{editTarget(root)}, Place: Placement{Page: editTarget(page), Parent: &Target{Ref: &parent}}}), "refused:structure")
	id := vocabID(BlockKind, 7)
	requireOutcomes(t, f.execute("a", &CreateBlockOp{CreateFields: CreateFields{Ref: "too-deep", ProposedID: &id}, Place: Placement{Page: editTarget(page), Parent: &Target{Ref: &parent}}, Content: editText(Paragraph, "")}), "refused:structure")
	requireOutcomes(t, f.execute("a", &MoveBlocksOp{Blocks: []Target{editTarget(root), editTarget(parent)}, Place: Placement{Page: editTarget(other)}}), "applied")
	seq := Version(f.seq)
	for i := byte(1); i <= 6; i++ {
		r := f.record(vocabID(BlockKind, i))
		b := r.Item.Data.(Block)
		v, _ := r.Item.GroupVersion(PlaceGroup)
		if b.Page != other.ID || v != seq {
			t.Fatalf("move descendant=%+v", r)
		}
	}
	if f.record(page.ID).Aggregate != seq || f.record(other.ID).Aggregate != seq {
		t.Fatal("cross page aggregate not advanced")
	}
	if f.record(page.ID).Item.V[TitleGroup] != 0 {
		t.Fatal("aggregate touched title version")
	}
}

func TestEditAdmissionAndGuardRollback(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	page := f.record(vocabID(PageKind, 1)).Item.Ref
	block := f.block("b", 1, nil, Paragraph)
	f.text("b", block, "oversize", editOptions{})
	limits := defaultEditLimits()
	limits.Text = 3
	base := Version(1)
	result, err := f.tryExecute(editOptions{Browser: true, Session: "a", Limits: &limits}, &SetBlockOp{Block: editTarget(block), Content: editText(Paragraph, "smaller"), BaseV: &base})
	if err != nil {
		t.Fatal(err)
	}
	requireOutcomes(t, result, "applied")
	beforeUndo := f.count("a", "undo")
	result, err = f.tryExecute(editOptions{Browser: true, Session: "a", Limits: &limits}, &SetPageTitleOp{Page: editTarget(page), Title: "rollback"}, &SetBlockOp{Block: editTarget(block), Content: editText(Paragraph, "growing past excess"), BaseV: &base})
	if err != nil {
		t.Fatal(err)
	}
	requireOutcomes(t, result, "refused:item_limit", "refused:item_limit")
	if f.record(page.ID).Item.Data.(Page).Title != "page1" || f.count("a", "undo") != beforeUndo {
		t.Fatal("capacity refusal leaked title or undo")
	}
	guard := f.record(page.ID).Item
	f.execute("b", &SetPageTitleOp{Page: editTarget(page), Title: "new"})
	if _, err := f.tryExecute(editOptions{Guards: &Guards{Items: []ItemGuard{{Ref: guard.Ref, Ver: guard.Ver}}}}, &SetPageTitleOp{Page: editTarget(page), Title: "guarded"}); err == nil {
		t.Fatal("stale guard succeeded")
	}
	f.seq++
	var summary undoSummary
	err = f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		var err error
		summary, err = undoEdit(t.Context(), tx, editOptions{Session: "a", Seq: f.seq, Limits: &limits}, false)
		return err
	})
	if err != nil || summary.Capacity != 1 || summary.Applied != 0 {
		t.Fatalf("undo admission summary=%+v err=%v", summary, err)
	}
	if f.record(block.ID).Item.Data.(Block).Content.Text[0].Text != "smaller" || f.count("a", "undo") != beforeUndo-1 {
		t.Fatal("skipped undo changed content or retained consumed entry")
	}
}

func TestEditDuplicate501HasOneUndo(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	page := f.record(vocabID(PageKind, 1)).Item.Ref
	refs := make([]Target, 501)
	err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		keys := rebalancePositions(501)
		for i := range refs {
			record := copyTestRecord(copyTestID(BlockKind, uint64(i+100)), Block{Page: page.ID, Position: keys[i], Content: editText(Paragraph, fmt.Sprint(i))})
			record.Item.Ref.Inc, record.Item.Ver, record.Aggregate = 1, 1, 1
			if err := putRecord(t.Context(), tx, record); err != nil {
				return err
			}
			refs[i] = editTarget(record.Item.Ref)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	result := f.execute("a", &DuplicateBlocksOp{Blocks: refs, After: refs[len(refs)-1]})
	requireOutcomes(t, result, "applied")
	if len(result.Copies) != 501 || f.count("a", "undo") != 1 {
		t.Fatalf("copy mapping=%d undo=%d", len(result.Copies), f.count("a", "undo"))
	}
	if got := f.undo("a", false); got.Applied != 1 {
		t.Fatalf("undo=%+v", got)
	}
	if err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		for _, copy := range result.Copies {
			copied, err := loadRecord(t.Context(), tx, copy.Ref.ID)
			if err != nil {
				return err
			}
			source, err := loadRecord(t.Context(), tx, copy.Source.ID)
			if err != nil {
				return err
			}
			if copied.Alive || !source.Alive {
				t.Fatal("copy undo removed wrong ids")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEditZeroIncarnationAndAllocatedIDs(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	page := f.record(vocabID(PageKind, 1)).Item.Ref
	id := vocabID(BlockKind, 1)
	base := Version(0)
	requireOutcomes(t, f.execute("a", &CreateBlockOp{CreateFields: CreateFields{Ref: "new", ProposedID: &id}, Place: Placement{Page: editTarget(page)}, Content: editText(Paragraph, "")}, &SetBlockOp{Block: editTarget(Ref{ID: id}), Content: editText(Paragraph, "pending"), BaseV: &base}), "applied", "applied")
	requireOutcomes(t, f.execute("b", &SetBlockOp{Block: editTarget(Ref{ID: id}), Content: editText(Paragraph, "not mine"), BaseV: &base}), "ignored:gone")
	ref := f.record(id).Item.Ref
	f.execute("a", &DeleteBlocksOp{Blocks: []Target{editTarget(ref)}})
	requireOutcomes(t, f.execute("b", &CreateBlockOp{CreateFields: CreateFields{Ref: "reuse", ProposedID: &id}, Place: Placement{Page: editTarget(page)}, Content: editText(Paragraph, "")}), "refused:allocated_id")
	if f.record(id).Alive {
		t.Fatal("ordinary create revived allocated id")
	}
}

func TestEditMalformedUnitsRefuseBeforeMutation(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	page := f.record(vocabID(PageKind, 1)).Item.Ref
	unit := "repeated"
	_, err := f.tryExecute(editOptions{Browser: true, Session: "a"}, &SetPageTitleOp{OperationMeta: OperationMeta{UnitID: &unit}, Page: editTarget(page), Title: "one"}, &SetPageTitleOp{Page: editTarget(page), Title: "two"}, &SetPageTitleOp{OperationMeta: OperationMeta{UnitID: &unit}, Page: editTarget(page), Title: "three"})
	if err == nil || !strings.Contains(err.Error(), "noncontiguous") || f.record(page.ID).Item.Data.(Page).Title != "page1" {
		t.Fatalf("malformed units: %v", err)
	}
}
