package notebook

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

func TestUndoPruneBudgetIncludesStateChanges(t *testing.T) {
	for _, initial := range []string{"absent", "false", "true"} {
		t.Run(initial, func(t *testing.T) {
			t.Parallel()
			f := newUndoTestStore(t)
			err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
				if initial != "absent" {
					if err := saveUndoState(t.Context(), tx, "a", undoState{Evicted: initial == "true"}); err != nil {
						return err
					}
				}
				for order := int64(1); order <= 3; order++ {
					if err := saveUndoEntry(t.Context(), tx, "a", "undo", order, undoEntry{ActionID: fmt.Sprintf("action-%d", order)}); err != nil {
						return err
					}
				}
				var entryBytes int64
				if err := tx.QueryRowContext(t.Context(), `SELECT bytes FROM undo WHERE session='a' AND stack='undo' AND entry_order=1`).Scan(&entryBytes); err != nil {
					return err
				}
				stateRaw, err := json.Marshal(undoState{Evicted: true})
				if err != nil {
					return err
				}
				budget := 2 * entryBytes
				wantOrders := []int64{3}
				if initial != "absent" {
					budget += int64(len(stateRaw))
					wantOrders = []int64{2, 3}
				}
				for pass := 0; pass < 2; pass++ {
					if err := pruneUndo(t.Context(), tx, budget); err != nil {
						return err
					}
					rows, err := readUndoRows(t.Context(), tx, "a")
					if err != nil {
						return err
					}
					var orders []int64
					for _, row := range rows {
						orders = append(orders, row.Order)
					}
					if !slices.Equal(orders, wantOrders) {
						t.Fatalf("pass %d: retained orders=%v, want %v", pass, orders, wantOrders)
					}
					var total int64
					if err := tx.QueryRowContext(t.Context(), `SELECT sum(bytes) FROM undo`).Scan(&total); err != nil {
						return err
					}
					if total > budget {
						t.Fatalf("pass %d: bytes=%d exceed budget=%d", pass, total, budget)
					}
				}
				state, err := loadUndoState(t.Context(), tx, "a")
				if err != nil {
					return err
				}
				if !state.Evicted {
					t.Fatal("eviction state not recorded")
				}
				for order := int64(1); order <= 3; order++ {
					closed, err := undoActionClosed(t.Context(), tx, "a", fmt.Sprintf("action-%d", order), false)
					if err != nil {
						return err
					}
					if closed == slices.Contains(wantOrders, order) {
						t.Fatalf("action-%d closed=%t, retained=%v", order, closed, wantOrders)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUndoPruneGlobalOrderAndSessionLimit(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		entries := []undoRow{
			{Session: "a", Stack: "undo", Order: 90},
			{Session: "b", Stack: "redo", Order: 1},
			{Session: "a", Stack: "undo", Order: 1},
		}
		for _, row := range entries {
			if err := saveUndoEntry(t.Context(), tx, row.Session, row.Stack, row.Order, undoEntry{}); err != nil {
				return err
			}
		}
		var entryBytes int64
		if err := tx.QueryRowContext(t.Context(), `SELECT bytes FROM undo WHERE session='a' AND stack='undo' AND entry_order=90`).Scan(&entryBytes); err != nil {
			return err
		}
		raw, err := json.Marshal(undoState{Evicted: true})
		if err != nil {
			return err
		}
		if err := pruneUndo(t.Context(), tx, 2*entryBytes+int64(len(raw))); err != nil {
			return err
		}
		rows, err := readUndoRows(t.Context(), tx, "")
		if err != nil {
			return err
		}
		if len(rows) != 2 || rows[0].Session != "b" || rows[0].Stack != "redo" || rows[1].Session != "a" || rows[1].Order != 1 {
			t.Fatalf("global eviction order: %+v", rows)
		}
		for order := int64(2); order <= MaxUndoEntries+1; order++ {
			stack := "undo"
			if order%2 == 0 {
				stack = "redo"
			}
			if err := saveUndoEntry(t.Context(), tx, "a", stack, order, undoEntry{}); err != nil {
				return err
			}
		}
		if err := pruneUndo(t.Context(), tx, MaxUndoBytes); err != nil {
			return err
		}
		rows, err = readUndoRows(t.Context(), tx, "")
		if err != nil {
			return err
		}
		if len(rows) != MaxUndoEntries+1 || rows[0].Session != "b" || rows[1].Order != 2 {
			t.Fatalf("per-session undo+redo limit: %+v", rows)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUndoCloseFiltersSessionAndAction(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		entries := []undoRow{
			{Session: "a", Stack: "undo", Order: 1, Entry: undoEntry{ActionID: "입력", Open: true}},
			{Session: "a", Stack: "redo", Order: 2, Entry: undoEntry{ActionID: "입력", Open: true}},
			{Session: "a", Stack: "undo", Order: 3, Entry: undoEntry{ActionID: "other", Open: true}},
			{Session: "b", Stack: "undo", Order: 1, Entry: undoEntry{ActionID: "입력", Open: true}},
			{Session: "a", Stack: "undo", Order: 4, Entry: undoEntry{Open: true}},
		}
		for _, row := range entries {
			if err := saveUndoEntry(t.Context(), tx, row.Session, row.Stack, row.Order, row.Entry); err != nil {
				return err
			}
		}
		for _, actionID := range []string{"입력", ""} {
			if err := closeUndoAction(t.Context(), tx, "a", actionID); err != nil {
				return err
			}
			rows, err := readUndoRows(t.Context(), tx, "")
			if err != nil {
				return err
			}
			for _, row := range rows {
				wantOpen := row.Session == "b" || (actionID != "" && row.Entry.ActionID != actionID)
				if row.Entry.Open != wantOpen {
					t.Fatalf("close %q: %+v", actionID, row)
				}
			}
		}
		if err := closeUndoAction(t.Context(), tx, "a", "never-open"); err != nil {
			return err
		}
		for _, actionID := range []string{"입력", "other", "never-open"} {
			closed, err := undoActionClosed(t.Context(), tx, "a", actionID, false)
			if err != nil {
				return err
			}
			if !closed {
				t.Fatalf("action %q was not durably closed", actionID)
			}
		}
		var invalidBytes int
		if err := tx.QueryRowContext(t.Context(), `SELECT count(*) FROM undo WHERE stack IN ('undo','redo','state') AND bytes<>length(CAST(components AS BLOB))`).Scan(&invalidBytes); err != nil {
			return err
		}
		if invalidBytes != 0 {
			t.Fatalf("incorrect byte charges in %d rows", invalidBytes)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
