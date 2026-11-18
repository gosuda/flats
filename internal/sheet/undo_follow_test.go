package sheet

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// A formula an undo entry holds follows an insert into a tab that is
// deleted afterwards, whether the entry is in memory or in a scratch file.
func TestUndoFollowsEditsOfDeletedTab(t *testing.T) {
	bothWays(t, func(t *testing.T, e *env) {
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"write","tab":"Data","at":"A1","rows":[[7],[8]]},
			{"op":"write","tab":"Sheet1","at":"A1","rows":[["=Data!A1","=Data!A2"]]}]}`)
		alice, bob := e.person("Alice"), e.person("Bob")
		alice.do(e.setOp("Sheet1!A1", "text"))
		alice.do(e.deleteColsOp("Sheet1", 2, 1))
		bob.do(e.insertRowsOp("Data", 1, 1))
		bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Data")))
		wantUndo(t, "undo of the column", undo(t, alice), undoAck{Applied: 1})
		wantUndo(t, "undo of the text", undo(t, alice), undoAck{Applied: 1})
		wantUndo(t, "undo of the tab", undo(t, bob), undoAck{Applied: 1})
		wantTexts(t, e, map[string]string{"Sheet1!A1": "=Data!A2", "Sheet1!B1": "=Data!A3"})
		e.want("Sheet1!A1", "7")
		e.want("Sheet1!B1", "8")
	})
}

// A formula an entry holds moves with a sort as the formulas of the sheet
// do: a reference that leaves the grid at the new place is #REF! from then
// on (spec rule 15), also when a later sort takes the row back.
func TestUndoKeptFormulaMovesWithSort(t *testing.T) {
	bothWays(t, func(t *testing.T, e *env) {
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[5],[3],[2],[1,"=A1","=A1"]]}]}`)
		alice, bob := e.person("Alice"), e.person("Bob")
		alice.do(e.setOp("Sheet1!B4", "text"))
		// Row 4 goes to row 2, where a reference three rows up has no row.
		bob.do(e.sortOp("Sheet1", 2, 3, 1, false))
		wantUndo(t, "undo of the text", undo(t, alice), undoAck{Applied: 1})
		wantTexts(t, e, map[string]string{"Sheet1!B2": "=#REF!", "Sheet1!C2": "=#REF!"})
		bob.do(e.sortOp("Sheet1", 2, 3, 1, true))
		wantTexts(t, e, map[string]string{"Sheet1!B4": "=#REF!", "Sheet1!C4": "=#REF!"})
	})
	// The same for a formula in a deleted column.
	bothWays(t, func(t *testing.T, e *env) {
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[5],[3],[2],[1,"=A1","=A1"]]}]}`)
		alice, bob := e.person("Alice"), e.person("Bob")
		alice.do(e.deleteColsOp("Sheet1", 2, 1))
		bob.do(e.sortOp("Sheet1", 2, 3, 1, false))
		wantUndo(t, "undo of the column", undo(t, alice), undoAck{Applied: 1})
		wantTexts(t, e, map[string]string{"Sheet1!B2": "=#REF!", "Sheet1!C2": "=#REF!"})
		bob.do(e.sortOp("Sheet1", 2, 3, 1, true))
		wantTexts(t, e, map[string]string{"Sheet1!B4": "=#REF!", "Sheet1!C4": "=#REF!"})
	})
}

// followCase is one random history: Alice makes entries that hold formulas,
// rows, columns and a tab, other writers change the structure of three
// tabs, and Alice undoes everything. Some formulas name a tab that does not
// exist, and tabs take and give up names, that one included, or are renamed
// to the name they have, as it is or in other capitals. It returns
// every formula text of the sheet at the end. spill keeps every payload in
// a scratch file, and plain follows every edit one by one, whatever an
// entry depends on.
func followCase(t *testing.T, seed uint64, spill, plain bool) (texts []string, trace []string) {
	e := start(t)
	e.in.mu.Lock()
	p := e.in.undoPool()
	p.plain = plain
	if spill {
		p.entryMax, p.edits.segment = -1, 1
	}
	e.in.mu.Unlock()
	rng := rand.New(rand.NewPCG(seed, 99))
	const rows, cols = 8, 6
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"add_tab","name":"Extra"}]}`)
	start := []string{"Sheet1", "Data", "Extra"}
	// The names a formula can hold: the three tabs, and two that no tab has
	// at the start.
	pool := []string{"Sheet1", "Data", "Extra", "Ghost", "Spare"}
	for _, tab := range start {
		var sb strings.Builder
		for r := 1; r <= rows; r++ {
			if r > 1 {
				sb.WriteByte(',')
			}
			sb.WriteByte('[')
			for c := 0; c < cols; c++ {
				if c > 0 {
					sb.WriteByte(',')
				}
				fmt.Fprintf(&sb, `"=%c%d+%s!%c%d+$%c$%d"`, 'A'+rng.IntN(cols), 1+rng.IntN(rows), pool[rng.IntN(len(pool))], 'A'+rng.IntN(cols), 1+rng.IntN(rows),
					'A'+rng.IntN(cols), 1+rng.IntN(rows))
			}
			sb.WriteByte(']')
		}
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":%q,"at":"A1","rows":[%s]}]}`, tab, sb.String()))
	}
	alice, bob := e.person("Alice"), e.person("Bob")
	// live returns the names of the tabs, and size the rows and columns of
	// one.
	live := func() []string {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		names := make([]string, len(e.in.wb.tabs))
		for i, tb := range e.in.wb.tabs {
			names[i] = tb.name
		}
		return names
	}
	size := func(tab string) (int, int) {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		tb := e.in.wb.tab(tab)
		return tb.rows.len(), tb.cols.len()
	}
	pick := func() string {
		names := live()
		return names[rng.IntN(len(names))]
	}
	// free returns a name of the pool, or a new one, that no tab has.
	free := func(i int) string {
		names := live()
		taken := func(name string) bool {
			return slices.ContainsFunc(names, func(have string) bool { return strings.EqualFold(have, name) })
		}
		if name := pool[rng.IntN(len(pool))]; !taken(name) {
			return name
		}
		return fmt.Sprintf("Tab_%d", i)
	}
	note := func(format string, args ...any) { trace = append(trace, fmt.Sprintf(format, args...)) }
	// Alice's entries: values before an edit, deleted rows and columns, and
	// a deleted tab.
	entries := 0
	for i := 0; i < 4; i++ {
		tab := pick()
		nr, nc := size(tab)
		var ack ackBody
		switch k := rng.IntN(7); {
		case k < 2:
			r, c := 1+rng.IntN(nr), 1+rng.IntN(nc)
			note("alice clears %s!R%dC%d", tab, r, c)
			ack = alice.do(e.clearOp(tab, r, 1, c, 1))
		case k < 4:
			at := 1 + rng.IntN(nr-1)
			note("alice deletes row %d of %s", at, tab)
			ack = alice.do(e.deleteRowsOp(tab, at, 1))
		case k < 6:
			at := 1 + rng.IntN(nc-1)
			note("alice deletes column %d of %s", at, tab)
			ack = alice.do(e.deleteColsOp(tab, at, 1))
		default:
			note("alice deletes the tab %s", tab)
			ack = alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID(tab)))
		}
		if status(ack) == "applied" {
			entries++
		}
	}
	// Bob and an agent change the structure. Bob's undo brings back what he
	// deleted, tabs included; an agent call makes several edits at once.
	rename := func(i int, tab string) {
		name := free(i)
		// One rename in five is to the name the tab has, as it is or in
		// other capitals.
		switch rng.IntN(10) {
		case 0:
			name = tab
		case 1:
			if name = strings.ToUpper(tab); name == tab {
				name = strings.ToLower(tab)
			}
		}
		note("bob renames %s to %s", tab, name)
		bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":%q}`, e.tabID(tab), name))
	}
	for i := 0; i < 20; i++ {
		tab := pick()
		nr, nc := size(tab)
		switch k := rng.IntN(14); {
		case k < 2:
			at := 1 + rng.IntN(nr)
			note("bob inserts a row at %d of %s", at, tab)
			bob.do(e.insertRowsOp(tab, at, 1+rng.IntN(2)))
		case k < 4:
			at := 1 + rng.IntN(nc)
			note("bob inserts a column at %d of %s", at, tab)
			bob.do(e.insertColsOp(tab, at, 1))
		case k < 5 && nr > 2:
			at := 1 + rng.IntN(nr-1)
			note("bob deletes row %d of %s", at, tab)
			bob.do(e.deleteRowsOp(tab, at, 1))
		case k < 6 && nc > 2:
			at := 1 + rng.IntN(nc-1)
			note("bob deletes column %d of %s", at, tab)
			bob.do(e.deleteColsOp(tab, at, 1))
		case k < 7:
			note("bob undoes")
			bob.do(`{"op":"undo"}`)
		case k < 8 && len(live()) > 1:
			note("bob deletes the tab %s", tab)
			bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID(tab)))
		case k < 9:
			rename(i, tab)
		case k < 11:
			// Renames in a row, of this tab and of others.
			for n := 3 + rng.IntN(4); n > 0; n-- {
				rename(i*10+n, pick())
			}
		case k < 12 && len(live()) > 1:
			// Two tabs trade names, in one call.
			names := live()
			a := names[rng.IntN(len(names))]
			b := names[rng.IntN(len(names))]
			if a == b {
				continue
			}
			note("an agent trades the names of %s and %s", a, b)
			e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"rename_tab","tab":%q,"name":"Trade_%d"},{"op":"rename_tab","tab":%q,"name":%q},{"op":"rename_tab","tab":"Trade_%d","name":%q}]}`,
				a, i, b, a, i, b))
		case k < 13 && nr > 3:
			note("bob sorts rows 2 and 3 of %s", tab)
			bob.do(e.sortOp(tab, 2, 2, 1+rng.IntN(nc), rng.IntN(2) == 0))
		default:
			if nr < 3 {
				continue
			}
			note("an agent inserts two rows at 1 and deletes row 3 of %s", tab)
			e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_rows","tab":%q,"at":1,"count":2},{"op":"delete_rows","tab":%q,"at":3,"count":1}]}`, tab, tab))
		}
	}
	for ; entries > 0; entries-- {
		ack := alice.do(`{"op":"undo"}`)
		if ack.Undo == nil {
			t.Fatalf("seed %d: undo: %s", seed, status(ack))
		}
		note("alice undoes: %+v", *ack.Undo)
	}
	// Bob takes back what he can, so that the tabs and rows that Alice's
	// formulas name return and show what they kept.
	for i := 0; i < 40; i++ {
		ack := bob.do(`{"op":"undo"}`)
		if ack.Undo == nil || (ack.Undo.Applied == 0 && ack.Undo.Skipped == 0) {
			break
		}
	}
	e.in.mu.RLock()
	for _, tb := range e.in.wb.tabs {
		for r, row := range tb.rows.ids {
			rd := tb.data[RowID(row)]
			if rd == nil {
				continue
			}
			for _, slot := range rd.cells {
				if slot.c.f != nil {
					pos := tb.cols.pos[uint64(slot.col)]
					texts = append(texts, fmt.Sprintf("%s!R%dC%d %s", tb.name, r+1, pos+1, slot.c.f.Text(formula.Pos{Row: int32(r), Col: pos})))
				}
			}
		}
	}
	e.in.mu.RUnlock()
	slices.Sort(texts)
	// A run plays hundreds of these; each gives its sheet up here.
	e.in.Stop()
	return texts, trace
}

// An entry that follows each structural edit as it commits and one that
// reads the edits from the scratch log at its undo restore the same
// formulas, through inserts, deletions, sorts, renames, deleted and
// restored tabs and calls with several edits. Both restore what an entry
// restores that follows every edit one by one: passing over the edits an
// entry does not depend on, and following renames in a row as one edit,
// change nothing.
func TestUndoFollowsAlikeInMemoryAndFromScratch(t *testing.T) {
	seeds := sized(150, 30)
	if testing.Short() || raceEnabled {
		seeds = 12
	}
	for seed := uint64(1); seed <= uint64(seeds); seed++ {
		want, trace := followCase(t, seed, false, true)
		for _, c := range []struct {
			name         string
			spill, plain bool
		}{{"in memory", false, false}, {"from scratch", true, false}, {"from scratch, every edit one by one", true, true}} {
			got, _ := followCase(t, seed, c.spill, c.plain)
			if slices.Equal(want, got) {
				continue
			}
			for i := 0; i < len(want) && i < len(got); i++ {
				if want[i] != got[i] {
					t.Errorf("seed %d: in memory, every edit one by one %q; %s %q", seed, want[i], c.name, got[i])
				}
			}
			t.Fatalf("seed %d: %d formulas and %d %s differ after\n%s", seed, len(want), len(got), c.name, strings.Join(trace, "\n"))
		}
	}
}

// Renames in a row are followed as at most two renames for each name. The
// result is the formula that following every rename gives, for any order
// of names: names that trade places, names that pass through one another,
// names no tab has, and the names the two-step way borrows on its way.
func TestRenamesInARowComposeToTheSameFormulas(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 13))
	pool := []string{"A", "b", "C c", "D", "e", "F'f", "_1", "_2", "G", "Ünï"}
	quoted := func(name string) string {
		if !formula.TabNameNeedsQuotes(name) {
			return name
		}
		return "'" + strings.ReplaceAll(name, "'", "''") + "'"
	}
	spell := func(name string) string {
		if rng.IntN(2) == 0 {
			return strings.ToUpper(name)
		}
		return strings.ToLower(name)
	}
	composed := 0
	for round := 0; round < 5000; round++ {
		refs := make([]string, 1+rng.IntN(6))
		names := map[string]struct{}{}
		for i := range refs {
			name := pool[rng.IntN(len(pool))]
			refs[i] = quoted(spell(name)) + "!A1"
			names[formula.FoldName(name)] = struct{}{}
		}
		// The entry can know of more names than the formula holds.
		if rng.IntN(3) == 0 {
			names[formula.FoldName(pool[rng.IntN(len(pool))])] = struct{}{}
		}
		text := "=" + strings.Join(refs, "+")
		f, err := formula.Parse(text, formula.Pos{}, formula.Limits{})
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		run := make([]*logEntry, 3+rng.IntN(10))
		var trace []string
		for i := range run {
			tr := formula.Transform{Kind: formula.RenameTab, Tab: spell(pool[rng.IntN(len(pool))]), Name: spell(pool[rng.IntN(len(pool))])}
			run[i] = &logEntry{tr: tr}
			trace = append(trace, tr.Tab+" to "+tr.Name)
		}
		own := ""
		if rng.IntN(3) == 0 {
			own = formula.FoldName(pool[rng.IntN(len(pool))])
		}
		want := f
		for _, ed := range run {
			if own == "" || formula.FoldName(ed.tr.Tab) != own {
				want, _, _ = want.Apply("", formula.Pos{}, ed.tr)
			}
		}
		got := f
		list := composeRenames(run, own, names)
		for _, tr := range list {
			got, _, _ = got.Apply("", formula.Pos{}, tr)
		}
		if len(list) > 1 {
			composed++
		}
		if got.Stored() != want.Stored() {
			t.Fatalf("%s after %s, without the renames of %q:\none by one  %s\nas one edit %s\nthrough %+v", text, strings.Join(trace, ", "), own, want.Stored(), got.Stored(), list)
		}
	}
	if composed < 1000 {
		t.Fatalf("only %d of 5000 cases went through more than one rename", composed)
	}
}

// Renames in a row are followed for the names the formulas hold and for no
// other. A run in which thousands of other names pass to one name, which
// then goes back and forth, takes no work for those names.
func TestRenamesInARowFollowOnlyHeldNames(t *testing.T) {
	const tabs = 3000
	rename := func(from, to string) *logEntry {
		return &logEntry{tr: formula.Transform{Kind: formula.RenameTab, Tab: from, Name: to}}
	}
	run := []*logEntry{rename("Data", "Info")}
	// A new tab takes the name W each time, after the one that had it was
	// deleted.
	for i := 0; i < tabs; i++ {
		run = append(run, rename(fmt.Sprintf("Tab%d", i), "W"))
	}
	for i := 0; i < tabs; i++ {
		from, to := "W", "V"
		if i%2 == 1 {
			from, to = "V", "W"
		}
		run = append(run, rename(from, to))
	}
	run = append(run, rename("Info", "Facts"))
	before := allocated()
	list := composeRenames(run, "", map[string]struct{}{"data": {}, "ghost": {}})
	work := allocated() - before
	if want := []formula.Transform{{Kind: formula.RenameTab, Tab: "data", Name: "Facts"}}; !slices.Equal(list, want) {
		t.Fatalf("the renames compose to %+v, want %+v", list, want)
	}
	// Keeping every name that passed to W with each of its renames
	// allocates 48 KiB for each, over 100 MiB for the run.
	if limit := int64(1 << 20); measures && work > limit {
		t.Errorf("composing %d renames allocated %d KiB, over %d KiB", len(run), work>>10, limit>>10)
	}
}

// A deletion that an entry keeps apart is a deletion of the tab as it was
// named then. When it is applied after all, it changes the references that
// named the tab when it happened and no reference that has the same name
// for another reason: a name the formula held before the tab took it, or
// the name of a tab that took it after the first one was deleted.
func TestUndoKeptDeletionMeetsOtherUsesOfTheName(t *testing.T) {
	setup := func(e *env) (alice, bob *person) {
		e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Sheet1","name":"Data"},{"op":"add_tab","name":"Other"},
			{"op":"write","tab":"Data","at":"A1","rows":[[1],[2],[3],[4],[5],[6]]}]}`)
		alice, bob = e.person("Alice"), e.person("Bob")
		return alice, bob
	}
	renameOp := func(e *env, from, to string) string {
		return jsonf(`{"op":"rename_tab","tab":%d,"name":%q}`, e.tabID(from), to)
	}
	t.Run("the tab takes a name the formula holds", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			alice, bob := setup(e)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Data","at":"B1","rows":[["=A1+Ghost!A5","=A5+Ghost!A5"]]}]}`)
			alice.do(e.setOp("Data!B1", "x"), e.setOp("Data!C1", "y"))
			bob.do(e.deleteRowsOp("Data", 3, 1))
			bob.do(renameOp(e, "Data", "Ghost"))
			wantUndo(t, "undo of the overwrite", undo(t, alice), undoAck{Applied: 2})
			wantTexts(t, e, map[string]string{"Ghost!B1": "=A1+Ghost!A5", "Ghost!C1": "=A4+Ghost!A5"})
		})
	})
	t.Run("the rows return after the tab took the name", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			alice, bob := setup(e)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Data","at":"B1","rows":[["=A5+Ghost!A5"]]}]}`)
			alice.do(e.setOp("Data!B1", "x"))
			bob.do(e.deleteRowsOp("Data", 3, 1))
			bob.do(renameOp(e, "Data", "Ghost"))
			wantUndo(t, "undo of the rename", undo(t, bob), undoAck{Applied: 1})
			wantUndo(t, "undo of the deletion", undo(t, bob), undoAck{Applied: 1})
			wantUndo(t, "undo of the overwrite", undo(t, alice), undoAck{Applied: 1})
			// The deletion was applied when the names met. From then on
			// the formula follows as one of the sheet does: the rows that
			// return are an insert, for every reference that names the
			// tab now.
			wantTexts(t, e, map[string]string{"Data!B1": "=A5+Data!A6"})
		})
	})
	t.Run("renamed to a name the formula holds and back", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			alice, bob := setup(e)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Other","at":"B1","rows":[["=Data!A5+Ghost!A5"]]}]}`)
			alice.do(e.setOp("Other!B1", "x"))
			bob.do(e.deleteRowsOp("Data", 3, 1))
			bob.do(renameOp(e, "Data", "Ghost"))
			bob.do(renameOp(e, "Ghost", "Data"))
			wantUndo(t, "undo of the overwrite", undo(t, alice), undoAck{Applied: 1})
			wantTexts(t, e, map[string]string{"Other!B1": "=Data!A4+Data!A5"})
		})
	})
	t.Run("a new tab takes the name of the deleted one", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			alice, bob := setup(e)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Other","at":"B1","rows":[["=Data!A5"]]}]}`)
			alice.do(e.setOp("Other!B1", "x"))
			bob.do(e.deleteRowsOp("Data", 3, 1))
			bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Data")))
			e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"insert_rows","tab":"Data","at":5,"count":1}]}`)
			wantUndo(t, "undo of the overwrite", undo(t, alice), undoAck{Applied: 1})
			wantTexts(t, e, map[string]string{"Other!B1": "=Data!A4"})
		})
	})
	t.Run("the deleted tab returns under another name", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			alice, bob := setup(e)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Other","at":"B1","rows":[["=Data!A5"]]}]}`)
			alice.do(e.setOp("Other!B1", "x"))
			bob.do(e.deleteRowsOp("Data", 3, 1))
			bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Data")))
			e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"}]}`)
			wantUndo(t, "undo of the tab deletion", undo(t, bob), undoAck{Applied: 1})
			e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Data","at":5,"count":1},{"op":"insert_rows","tab":"Data (restored)","at":1,"count":1}]}`)
			wantUndo(t, "undo of the overwrite", undo(t, alice), undoAck{Applied: 1})
			wantTexts(t, e, map[string]string{"Other!B1": "=Data!A4"})
		})
	})
	t.Run("the tab is recovered from a revision", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			alice, bob := setup(e)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Other","at":"B1","rows":[["=Data!A5"]]}]}`)
			rev := closeRevision(e)
			tab := e.tabID("Data")
			alice.do(e.setOp("Other!B1", "x"))
			bob.do(e.deleteRowsOp("Data", 3, 1))
			bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, tab))
			if _, err := bob.recover(rev, tab); err != nil {
				t.Fatal(err)
			}
			e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Data","at":5,"count":1}]}`)
			wantUndo(t, "undo of the overwrite", undo(t, alice), undoAck{Applied: 1})
			wantTexts(t, e, map[string]string{"Other!B1": "=Data!A4"})
		})
	})
}

// A formula an entry holds as the value before an edit ends when its own
// row is deleted for good: it stays as it was then. When an undo brings
// the row back later, the formula is not followed from the place the
// cell had before, as if it had never left.
func TestUndoKeptFormulaEndsWithItsRow(t *testing.T) {
	bothWays(t, func(t *testing.T, e *env) {
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"x"],[2,"x"],[3,"=A7+A1"],[9],[8],[7],[6],[5]]}]}`)
		alice, bob := e.person("Alice"), e.person("Bob")
		alice.do(e.setOp("Sheet1!B3", "text"))
		bob.do(e.deleteRowsOp("Sheet1", 2, 2))
		// The sort leaves the deleted rows no place to return to, so the
		// deletion counts for the formula: its cell is gone.
		if ack := bob.do(e.sortOp("Sheet1", 2, 5, 1, false)); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		wantUndo(t, "undo of the sort", undo(t, bob), undoAck{Applied: 1})
		wantUndo(t, "undo of the deletion", undo(t, bob), undoAck{Applied: 1})
		wantUndo(t, "undo of the overwrite", undo(t, alice), undoAck{Applied: 1})
		wantTexts(t, e, map[string]string{"Sheet1!B3": "=A7+A1"})
	})
}

// A deletion an entry keeps apart is applied early only when a name comes
// to mean another tab for the entry's formulas. A rename that leaves every
// reference with the tab it named does not: one to the same name, one that
// only changes capitals, and one to a name no formula of the entry holds.
// The rows that return give the references back.
func TestUndoKeptDeletionOutlivesRenamesThatMixNothing(t *testing.T) {
	for _, name := range []string{"Data", "DATA", "data", "Fresh"} {
		t.Run("the tab of the deletion is renamed to "+name, func(t *testing.T) {
			bothWays(t, func(t *testing.T, e *env) {
				e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"write","tab":"Data","at":"A1","rows":[[1],[2],[3]]},
					{"op":"write","tab":"Sheet1","at":"A1","rows":[["=Data!A2"]]}]}`)
				alice, bob := e.person("Alice"), e.person("Bob")
				alice.set("Sheet1!A1", "x")
				bob.do(e.deleteRowsOp("Data", 2, 1))
				e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"rename_tab","tab":"Data","name":%q}]}`, name))
				wantUndo(t, "undo of the deletion", undo(t, bob), undoAck{Applied: 1})
				wantUndo(t, "undo of the overwrite", undo(t, alice), undoAck{Applied: 1})
				e.wantText("Sheet1!A1", "="+name+"!A2")
				e.want("Sheet1!A1", "2")
			})
		})
	}
	// An entry keeps the names of 32 tabs and depends on every name beyond
	// that. With 33 it follows every rename, and still applies a kept
	// deletion only before a name its formulas hold.
	for _, names := range []int{depsKept, depsKept + 1} {
		for _, c := range []struct{ to, data, none0 string }{
			{"Fresh", "Fresh!A2", "None0!A5"},
			{"DATA", "DATA!A2", "None0!A5"},
			// The formula holds None0 already. The deletion is applied
			// before the tab takes that name, and the rows that return are
			// an insert for every reference that has the name by then.
			{"None0", "#REF!", "None0!A6"},
		} {
			t.Run(fmt.Sprintf("a formula of %d names, the tab renamed to %s", names, c.to), func(t *testing.T) {
				bothWays(t, func(t *testing.T, e *env) {
					e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"write","tab":"Data","at":"A1","rows":[[1],[2],[3]]}]}`)
					text := func(data, none0 string) string {
						return "=" + data + "+" + none0 + "+" + longRefs(names-2, func(i int) string { return fmt.Sprintf("None%d!A5", i+1) })[1:]
					}
					e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[%q]]}]}`, text("Data!A2", "None0!A5")))
					alice, bob := e.person("Alice"), e.person("Bob")
					alice.set("Sheet1!A1", "x")
					e.in.mu.RLock()
					deps := alice.s.undo.undo[0].deps
					all := deps != nil && deps.allNames
					e.in.mu.RUnlock()
					if all != (names > depsKept) {
						t.Fatalf("an entry whose formula names %d tabs depends on every name: %v", names, all)
					}
					bob.do(e.deleteRowsOp("Data", 2, 1))
					e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"rename_tab","tab":"Data","name":%q}]}`, c.to))
					wantUndo(t, "undo of the deletion", undo(t, bob), undoAck{Applied: 1})
					wantUndo(t, "undo of the overwrite", undo(t, alice), undoAck{Applied: 1})
					e.wantText("Sheet1!A1", text(c.data, c.none0))
				})
			})
		}
	}
	// A tab takes the name the deletion's tab had, and the entry follows
	// that rename because it lies in the tab. Its formulas hold no name,
	// so the name means nothing to them.
	t.Run("another tab takes the name and no formula holds it", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"add_tab","name":"Other"},
				{"op":"write","tab":"Data","at":"A1","rows":[[1,"=A3"],[2],[3]]},{"op":"write","tab":"Other","at":"B1","rows":[["=A1"]]}]}`)
			alice, bob := e.person("Alice"), e.person("Bob")
			if ack := alice.do(e.setOp("Data!B1", "x"), e.setOp("Other!B1", "y")); status(ack) != "applied,applied" {
				t.Fatal(status(ack))
			}
			data := e.tabID("Data")
			bob.do(e.deleteRowsOp("Data", 3, 1))
			bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, data))
			e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Other","name":"Data"}]}`)
			e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Data","name":"Other"}]}`)
			wantUndo(t, "undo of the tab deletion", undo(t, bob), undoAck{Applied: 1})
			wantUndo(t, "undo of the row deletion", undo(t, bob), undoAck{Applied: 1})
			wantUndo(t, "undo of the overwrites", undo(t, alice), undoAck{Applied: 2})
			wantTexts(t, e, map[string]string{"Data!B1": "=A3", "Other!B1": "=A1"})
		})
	})
	// Another tab takes the name of the deletion's tab, which is deleted,
	// and is deleted itself before anything rewrites a reference for it.
	// The first tab returns under its name, and its rows to their place.
	t.Run("another tab has the name for a while and nothing is rewritten for it", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"add_tab","name":"Other"},
				{"op":"write","tab":"Data","at":"A1","rows":[[1],[2],[3]]},{"op":"write","tab":"Sheet1","at":"A1","rows":[["=Data!A3"]]},
				{"op":"write","tab":"Other","at":"B1","rows":[["=A1"]]}]}`)
			alice, bob := e.person("Alice"), e.person("Bob")
			if ack := alice.do(e.setOp("Sheet1!A1", "x"), e.setOp("Other!B1", "y")); status(ack) != "applied,applied" {
				t.Fatal(status(ack))
			}
			bob.do(e.deleteRowsOp("Data", 3, 1))
			bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Data")))
			e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Other","name":"Data"}]}`)
			e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"Data"}]}`)
			wantUndo(t, "undo of the tab deletion", undo(t, bob), undoAck{Applied: 1})
			wantUndo(t, "undo of the row deletion", undo(t, bob), undoAck{Applied: 1})
			if u := undo(t, alice); u.Applied != 1 || u.Skipped != 1 {
				t.Fatalf("undo of the overwrites = %+v, want one applied and the one in the deleted tab skipped", *u)
			}
			e.wantText("Sheet1!A1", "=Data!A3")
			e.want("Sheet1!A1", "3")
		})
	})
}

// A deletion an entry keeps apart is applied before an edit rewrites the
// references it concerns for another tab, and before a rename gives its
// name to references that named another tab. It stays a deletion under
// the name its tab had when the tab comes back under another one.
func TestUndoKeptDeletionKeepsTheNameItsTabHad(t *testing.T) {
	setup := func(e *env) (alice, bob *person) {
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"add_tab","name":"Other"},
			{"op":"write","tab":"Data","at":"A1","rows":[[1],[2],[3],[4],[5],[6]]},{"op":"write","tab":"Sheet1","at":"A1","rows":[["=Data!A5+Data!A3"]]}]}`)
		alice, bob = e.person("Alice"), e.person("Bob")
		alice.set("Sheet1!A1", "x")
		bob.do(e.deleteRowsOp("Data", 3, 1))
		bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Data")))
		return alice, bob
	}
	t.Run("another tab takes the name and gives it up", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			alice, bob := setup(e)
			e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Other","name":"Data"}]}`)
			// This rename rewrites the references to Data for the other
			// tab, so the deletion counts for them first.
			e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Data","name":"Other"}]}`)
			wantUndo(t, "undo of the tab deletion", undo(t, bob), undoAck{Applied: 1})
			wantUndo(t, "undo of the row deletion", undo(t, bob), undoAck{Applied: 1})
			wantUndo(t, "undo of the overwrite", undo(t, alice), undoAck{Applied: 1})
			e.wantText("Sheet1!A1", "=Other!A4+#REF!")
		})
	})
	// The formula also reads Other, which takes the name of the deleted
	// tab. The deletion counts first, for the references that named the
	// deleted tab, and the others read Other under its new name as before.
	t.Run("another tab takes the name for references that named it", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"add_tab","name":"Other"},
				{"op":"write","tab":"Data","at":"A1","rows":[[1],[2],[3],[4],[5],[6]]},{"op":"write","tab":"Other","at":"A1","rows":[[10],[20],[30],[40],[50],[60]]},
				{"op":"write","tab":"Sheet1","at":"A1","rows":[["=Data!A5+Other!A5"]]}]}`)
			alice, bob := e.person("Alice"), e.person("Bob")
			alice.set("Sheet1!A1", "x")
			bob.do(e.deleteRowsOp("Data", 3, 1))
			bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Data")))
			e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Other","name":"Data"}]}`)
			wantUndo(t, "undo of the overwrite", undo(t, alice), undoAck{Applied: 1})
			e.wantText("Sheet1!A1", "=Data!A4+Data!A5")
			e.want("Sheet1!A1", "90")
		})
	})
	t.Run("the tab returns under another name and is renamed", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			alice, bob := setup(e)
			e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"}]}`)
			wantUndo(t, "undo of the tab deletion", undo(t, bob), undoAck{Applied: 1})
			e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Data (restored)","name":"Info"}]}`)
			wantUndo(t, "undo of the overwrite", undo(t, alice), undoAck{Applied: 1})
			// The references name the new tab Data now. The deletion took
			// the rows they named in the tab that had the name.
			e.wantText("Sheet1!A1", "=Data!A4+#REF!")
		})
	})
	t.Run("the tab returns under another name and takes its own back", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			alice, bob := setup(e)
			e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"}]}`)
			wantUndo(t, "undo of the tab deletion", undo(t, bob), undoAck{Applied: 1})
			e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"Data"},{"op":"rename_tab","tab":"Data (restored)","name":"Data"}]}`)
			wantUndo(t, "undo of the row deletion", undo(t, bob), undoAck{Applied: 1})
			wantUndo(t, "undo of the overwrite", undo(t, alice), undoAck{Applied: 1})
			e.wantText("Sheet1!A1", "=Data!A5+Data!A3")
			e.want("Sheet1!A1", "8")
		})
	})
}
