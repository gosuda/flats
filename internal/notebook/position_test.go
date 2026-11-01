package notebook

import (
	"database/sql"
	"strings"
	"testing"
)

func TestPositionRebalancePreservesLogicalVersions(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	page := f.record(vocabID(PageKind, 1)).Item.Ref
	root := f.block("b", 1, nil, Paragraph)
	err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		r, err := loadRecord(t.Context(), tx, root.ID)
		if err != nil {
			return err
		}
		b := r.Item.Data.(Block)
		b.Position = strings.Repeat("0", 31) + "1"
		r.Item.Data = b
		return putRecord(t.Context(), tx, r)
	})
	if err != nil {
		t.Fatal(err)
	}
	before := f.record(root.ID)
	id := vocabID(BlockKind, 2)
	requireOutcomes(t, f.execute("a", &CreateBlockOp{CreateFields: CreateFields{Ref: "before", ProposedID: &id}, Place: Placement{Page: editTarget(page), Before: &Target{Ref: &root}}, Content: editText(Paragraph, "")}), "applied")
	after := f.record(root.ID)
	if before.Item.Ver != after.Item.Ver || before.Item.Ref != after.Item.Ref || before.Item.V[PlaceGroup] != after.Item.V[PlaceGroup] {
		t.Fatal("rebalancing bumped logical version")
	}
	if after.Item.Data.(Block).Position == before.Item.Data.(Block).Position {
		t.Fatal("fixture did not trigger rebalance")
	}
	if len(f.record(id).Item.Data.(Block).Position) > MaxPositionBytes {
		t.Fatal("key cap exceeded")
	}
	if got := f.undo("a", false); got.Applied != 1 || f.record(id).Alive {
		t.Fatalf("creation undo after rekey=%+v", got)
	}
}

func TestPositionsBetween(t *testing.T) {
	for _, test := range []struct {
		name, left, right string
		count             int
	}{
		{name: "empty", count: 0},
		{name: "start", right: "1", count: 501},
		{name: "middle", left: "A", right: "B", count: 501},
		{name: "end", left: "z", count: 501},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			keys, err := positionsBetween(test.left, test.right, test.count)
			if err != nil || len(keys) != test.count {
				t.Fatalf("positions=%d err=%v", len(keys), err)
			}
			left := test.left
			for _, key := range keys {
				if key <= left || test.right != "" && key >= test.right || len(key) > MaxPositionBytes || strings.HasSuffix(key, "0") {
					t.Fatalf("invalid key %q in (%q,%q)", key, left, test.right)
				}
				left = key
			}
		})
	}
	t.Run("rebalance needed", func(t *testing.T) {
		t.Parallel()
		if _, err := positionsBetween("", strings.Repeat("0", MaxPositionBytes-1)+"1", 501); err == nil {
			t.Fatal("overlong batch keys accepted")
		}
	})
}

func TestPositionIntervals(t *testing.T) {
	t.Parallel()
	keys := rebalancePositions(2000)
	for i, key := range keys {
		left := ""
		if i > 0 {
			left = keys[i-1]
		}
		middle, err := positionBetween(left, key)
		if err != nil || middle <= left || middle >= key || strings.HasSuffix(middle, "0") {
			t.Fatalf("between(%q,%q)=%q %v", left, key, middle, err)
		}
	}
}
