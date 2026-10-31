package notebook

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

func TestEditReferenceModelStructurePrecedence(t *testing.T) {
	t.Parallel()
	for _, dependencyGone := range []bool{false, true} {
		t.Run(fmt.Sprintf("dependency_gone=%t", dependencyGone), func(t *testing.T) {
			t.Parallel()
			r := newModelRun(t)
			id := vocabID(BlockKind, 1)
			page, other := vocabID(PageKind, 1), vocabID(PageKind, 2)
			r.browser("setup", &CreateBlockOp{CreateFields: CreateFields{Ref: "block", ProposedID: &id}, Place: Placement{Page: r.target(page)}, Content: modelContent(Paragraph, "before", 0)})
			stale := r.target(id)
			stale.Ref.Inc++
			check := StructureCheck{Block: r.target(id), Page: r.target(other)}
			wantStatus, wantReason := "refused", "structure"
			if dependencyGone {
				check.Block = stale
				wantStatus, wantReason = "ignored", "gone"
			}
			unit := "precedence"
			base := Version(1)
			ops := []Operation{
				&SetBlockOp{OperationMeta: OperationMeta{UnitID: &unit, Structure: []StructureCheck{check}}, Block: stale, BaseV: &base, Content: modelContent(Paragraph, "must not apply", 0)},
				&SetPageTitleOp{OperationMeta: OperationMeta{UnitID: &unit}, Page: r.target(other), Title: "must roll back"},
			}
			want, failure := r.m.clone().action(editOptions{Browser: true, Session: "a"}, ops, r.f.seq+1, newModelHistory())
			if failure != "" || len(want.Results) != 2 {
				t.Fatalf("oracle result=%+v failure=%s", want, failure)
			}
			for _, outcome := range want.Results {
				if outcome.Status != wantStatus || outcome.Reason != wantReason || outcome.FailedIndex == nil || *outcome.FailedIndex != 0 {
					t.Fatalf("oracle precedence=%+v want=%s:%s", outcome, wantStatus, wantReason)
				}
			}
			r.browser("a", ops...)
		})
	}
}

func TestEditReferenceModelUnitStartStructure(t *testing.T) {
	t.Parallel()
	r := newModelRun(t)
	page, other := vocabID(PageKind, 1), vocabID(PageKind, 2)
	id := vocabID(BlockKind, 1)
	r.browser("setup", &CreateBlockOp{CreateFields: CreateFields{Ref: "block", ProposedID: &id}, Place: Placement{Page: r.target(page)}, Content: modelContent(Paragraph, "before", 0)})
	unit := "move-then-write"
	base := Version(1)
	r.browser("a",
		&MoveBlocksOp{OperationMeta: OperationMeta{UnitID: &unit, Structure: []StructureCheck{{Block: r.target(id), Page: r.target(page)}}}, Blocks: []Target{r.target(id)}, Place: Placement{Page: r.target(other)}},
		&SetBlockOp{OperationMeta: OperationMeta{UnitID: &unit}, Block: r.target(id), BaseV: &base, Content: modelContent(Paragraph, "after move", 0)})
	r.control("a", "undo")
	r.control("a", "redo")
}

func modelChain(r *modelRun, depth int) []ID {
	r.t.Helper()
	var ids []ID
	for i := 0; i <= depth; i++ {
		id := vocabID(BlockKind, byte(i+1))
		place := Placement{Page: r.target(vocabID(PageKind, 1))}
		if i > 0 {
			place.Parent = modelPointer(r.target(ids[i-1]))
		}
		kind := Toggle
		if i == depth {
			kind = Paragraph
		}
		r.browser("setup", &CreateBlockOp{CreateFields: CreateFields{Ref: fmt.Sprint(id), ProposedID: &id}, Place: place, Content: modelContent(kind, "initial", 0)})
		ids = append(ids, id)
	}
	return ids
}

func TestEditReferenceModelRepeatedUnitWrites(t *testing.T) {
	t.Parallel()
	r := newModelRun(t)
	page := vocabID(PageKind, 1)
	r.browser("a", &SetPageTitleOp{Page: r.target(page), Title: "first"}, &SetPageTitleOp{Page: r.target(page), Title: "second"})
	r.control("a", "undo")
	r.control("a", "undo")
	r.control("a", "redo")
	r.control("a", "redo")
}

func TestEditReferenceModelRevivalOwnership(t *testing.T) {
	t.Parallel()
	r := newModelRun(t)
	id := vocabID(BlockKind, 1)
	r.browser("a", &CreateBlockOp{CreateFields: CreateFields{Ref: "block", ProposedID: &id}, Place: Placement{Page: r.target(vocabID(PageKind, 1))}, Content: modelContent(Paragraph, "initial", 0)})
	r.writeGroup("a", id, "later")
	r.browser("a", &DeleteBlocksOp{Blocks: []Target{r.target(id)}})
	r.control("a", "revive")
	r.control("a", "undo")
	r.control("a", "undo")
	r.control("a", "redo")
	r.control("a", "redo")
}

func TestEditReferenceModelReviewOverlap(t *testing.T) {
	t.Parallel()
	r := newModelRun(t)
	ids := modelChain(r, 2)
	r.browser("a", &TurnBlocksOp{Blocks: []Target{r.target(ids[1]), r.target(ids[0])}, Kind: Paragraph})
	r.writeGroup("b", ids[1], "later text")
	r.control("a", "undo")
	if r.m.Items[ids[0]].Content.Kind != Paragraph || r.m.Items[ids[1]].Parent != "" || r.m.Items[ids[2]].Parent != "" {
		t.Fatal("overlapping conversion must remain applied as a whole")
	}
}

func TestEditReferenceModelOverlappingConversions(t *testing.T) {
	t.Parallel()
	for _, depth := range []int{2, 3} {
		for _, reversed := range []bool{false, true} {
			for edit := -1; edit < 2*depth; edit++ {
				t.Run(fmt.Sprintf("depth=%d/reverse=%t/later=%d", depth, reversed, edit), func(t *testing.T) {
					t.Parallel()
					r := newModelRun(t)
					ids := modelChain(r, depth)
					independent := vocabID(BlockKind, 10)
					r.browser("setup", &CreateBlockOp{CreateFields: CreateFields{Ref: "independent", ProposedID: &independent}, Place: Placement{Page: r.target(vocabID(PageKind, 1))}, Content: modelContent(Toggle, "unrelated", 0)})
					var selected []Target
					for _, id := range ids[:depth] {
						selected = append(selected, r.target(id))
					}
					if reversed {
						slices.Reverse(selected)
					}
					selected = append(selected, r.target(independent))
					r.browser("a", &TurnBlocksOp{Blocks: selected, Kind: Paragraph})
					if edit >= 0 && edit < depth {
						r.writeGroup("b", ids[edit], "later content")
					} else if edit >= depth {
						r.browser("b", &MoveBlocksOp{Blocks: []Target{r.target(ids[edit-depth+1])}, Place: Placement{Page: r.target(vocabID(PageKind, 2))}})
					}
					r.reopen()
					r.control("a", "undo")
					wantKind := Toggle
					if edit >= 0 {
						wantKind = Paragraph
					}
					if r.m.Items[ids[0]].Content.Kind != wantKind || r.m.Items[independent].Content.Kind != Toggle {
						t.Fatal("overlap was split or unrelated conversion was coupled")
					}
					r.control("a", "redo")
					r.control("a", "undo")
				})
			}
		}
	}
}

func TestEditReferenceModelStructuralUndo(t *testing.T) {
	t.Parallel()
	for seed := 0; seed < *notebookModelSeeds; seed++ {
		t.Run(fmt.Sprintf("seed=%03d", seed), func(t *testing.T) {
			t.Parallel()
			r := newModelRun(t)
			rng := rand.New(rand.NewSource(int64(seed)))
			modelChain(r, 2+rng.Intn(2))
			for i := 0; i < 24; i++ {
				session := []string{"a", "b", "c"}[rng.Intn(3)]
				live := r.m.ids(BlockKind, true)
				if len(live) == 0 {
					r.control(session, "undo")
					continue
				}
				id := live[rng.Intn(len(live))]
				n := r.m.Items[id]
				switch rng.Intn(7) {
				case 0, 1:
					selected := []Target{r.target(id)}
					if n.Parent != "" {
						selected = append(selected, r.target(n.Parent))
					} else if children := r.m.Order[modelPlace{Kind: BlockKind, Page: n.Page, Parent: id}]; len(children) > 0 {
						selected = append(selected, r.target(children[0]))
					}
					if rng.Intn(2) == 0 {
						slices.Reverse(selected)
					}
					check := StructureCheck{Block: r.target(id), Page: r.target(n.Page)}
					if n.Parent != "" {
						check.Parent = modelPointer(r.target(n.Parent))
					}
					meta := OperationMeta{}
					if rng.Intn(2) == 0 {
						unit := fmt.Sprintf("structure-%d", i)
						meta = OperationMeta{UnitID: &unit, Structure: []StructureCheck{check}}
					}
					r.browser(session, &TurnBlocksOp{OperationMeta: meta, Blocks: selected, Kind: []ContentKind{Paragraph, Toggle, Bullet}[rng.Intn(3)]})
				case 2:
					r.browser(session, &MoveBlocksOp{Blocks: []Target{r.target(id)}, Place: Placement{Page: r.target(vocabID(PageKind, byte(1+rng.Intn(2))))}})
				case 3:
					base := n.version(ContentGroup)
					r.browser(session, &SetBlockOp{Block: r.target(id), BaseV: &base, Content: modelContent(n.Content.Kind, fmt.Sprintf("later%d", rng.Intn(3)), 0)})
				case 4:
					r.browser(session, &DeleteBlocksOp{Blocks: []Target{r.target(id)}})
					r.control(session, "revive")
					r.control(session, "redo")
					r.control(session, "revive")
				case 5:
					r.control(session, []string{"undo", "redo"}[rng.Intn(2)])
				case 6:
					r.reopen()
				}
			}
			r.reopen()
		})
	}
}

func TestModelSequenceReducerControls(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"undo", "redo", "reopen", "revive"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			r := newModelRun(t)
			page, other := vocabID(PageKind, 1), vocabID(PageKind, 2)
			r.writeGroup("b", other, "noise")
			r.reopen()
			if kind == "revive" {
				modelChain(r, 1)
				r.browser("a", &DeleteBlocksOp{Blocks: []Target{r.target(vocabID(BlockKind, 1))}})
				r.control("a", "revive")
			} else {
				r.writeGroup("a", page, "sentinel")
				if kind != "reopen" {
					r.control("a", "undo")
					if kind == "redo" {
						r.control("a", "redo")
					}
				} else {
					r.reopen()
				}
			}
			r.writeGroup("b", other, "more noise")
			if replayModelSteps(t, r.steps) {
				t.Fatal("unmodified control sequence diverges")
			}
			fault := func(step modelStep, run *modelRun) {
				if step.Kind != kind {
					return
				}
				n := run.m.Items[page]
				trigger := false
				switch kind {
				case "undo":
					trigger = n.Title == "page1" && n.version(TitleGroup) == Version(step.Seq)
				case "redo":
					trigger = n.Title == "sentinel" && n.version(TitleGroup) == Version(step.Seq)
				case "reopen":
					trigger = n.Title == "sentinel"
				case "revive":
					block := run.m.Items[vocabID(BlockKind, 1)]
					trigger = block != nil && block.Alive && block.Ref.Inc == Incarnation(step.Seq)
				}
				if trigger {
					n.Title = "injected discrepancy"
				}
			}
			diverges := func(steps []modelStep) bool { return replayModelSteps(t, steps, fault) }
			if !diverges(r.steps) {
				t.Fatal("fault did not reproduce")
			}
			r.fault = fault
			reduced, report := r.reducedFailure()
			wantCount := 2
			if kind == "redo" || kind == "revive" {
				wantCount = 3
			}
			if len(reduced) != wantCount || !strings.Contains(report, "1-minimal divergent sequence") || !slices.ContainsFunc(reduced, func(step modelStep) bool { return step.Kind == kind }) || !diverges(reduced) {
				t.Fatalf("control failure did not reduce/replay: %+v", reduced)
			}
			for i := range reduced {
				if diverges(append(slices.Clone(reduced[:i]), reduced[i+1:]...)) {
					t.Fatalf("event %d is not necessary", i)
				}
			}
			if replayModelSteps(t, reduced) {
				t.Fatal("reduction retained an unrelated discrepancy")
			}
			t.Log(report)
		})
	}
}
