package sheet

import (
	"fmt"
	"maps"
	"slices"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// A formula that an undo entry keeps, as the value before an edit or in a
// deleted row, column or tab, follows the structure that changes while the
// entry waits, as if it had stayed in its cell (technical design, section
// 14). The model computes that by itself, in the plainest way it can: it
// gives the formula a world of its own and replays every later structural
// edit in it.
//
// A world is the axes of every tab, as lists of ids, from the moment the
// formula left the sheet: one world for the cells an action overwrites,
// and one for each stretch of rows or columns a deletion takes, made when
// that stretch leaves. What the world knows is ids, positions counted in
// its own lists, and formula.Apply and formula.Moved for the rewriting of
// one formula by one edit.
//
// A world follows the edits its entry depends on (depends): those of the
// tabs its formulas lie in or name, and of the tabs it keeps a deletion
// of. For any other edit the world only keeps its lists of ids in step.
//
// Rows and columns that are deleted afterwards, in a tab the world depends
// on, stay in the world's axes as a gap, so a kept formula keeps its
// references to them and has them again when an undo brings the rows back
// to the same place. A gap becomes a deletion after all, with #REF! and
// shortened ranges, when its rows come back elsewhere, when a sort orders
// the rows of its tab, and for every gap that is still open when the entry
// is undone, and the oldest one when the entry holds more than 32. A gap is
// a deletion of its tab as the tab was named, and it changes the references
// that named the tab when the rows went (Appendix B, engine item 23). The
// model applies it by the name it has, so those have to be the references
// written with that name. An edit that would leave it otherwise comes after
// the deletion. To find such an edit the model looks at the references
// themselves, before and after it (under, worldRenamed, parts).
//
// An insert lands in the world where it lands in the sheet: above the row
// that is below it. A gap at that place stays above the new rows while
// the row that was above the gap still stands, because that is where its
// rows would return to (Appendix B, engine item 17). The same rule says
// whether rows that come back have reached their gap; for that, the rows
// the entry itself deleted are not in the way. A sort moves the
// rows of the world as it moves those of the sheet, and a kept formula
// whose own row moves is rewritten as a formula of the sheet is. A rename
// renames.
//
// A tab recovery that brings a tab back under its id puts the tab of the
// revision in the world in place of the one the world knew, which it keeps
// aside with its gaps (shelf). Edits of the recovered tab move the axes of
// the tab of the revision. When the recovery is undone and an undo brings
// back the tab that was deleted before it, the world has the tab it knew
// again, gaps and all, so the entries that knew it apply to it as before
// (Appendix B, engine item 26). A gap that is set aside stays a gap: it
// becomes a deletion when its entry is undone, at a sort of the tab and as
// the oldest of more than 32, like any other.
//
// The stretch an undo brings back is the entry's own gap. Its formulas are
// settled, with every other open gap made a deletion, before any stretch
// returns, and they keep that form at the place the rows return to, also
// when that is another place (engine item 23).

// smKept is one formula a world follows, by the ids of its cell.
type smKept struct {
	tab, row, col uint64
	f             *formula.Formula
	orig          string // its stored form when it was kept
	// A formula of a deleted tab stays at its place in that tab, at, and
	// the tab had the name gone. Edits of a live tab of that name are
	// edits of another tab and of no name the formula could mean, so it
	// does not follow them; edits of other tabs it follows by their names.
	frozen bool
	at     formula.Pos
	gone   string
	// dead says that the cell's own row or column was deleted for good in
	// this world: the formula stays as it was then.
	dead bool
}

// smGap is a stretch of rows or columns that was deleted in the sheet and
// is still in the world's axis.
type smGap struct {
	rows  bool
	ids   []uint64
	above uint64 // the one that was above them, 0 for none
	below uint64 // and the one that was below them
	// name is the name of the tab, as it is now. The deletion changes the
	// references that named the tab when it happened, and it is applied
	// by this name (Appendix B, engine item 23).
	name string
	seq  int // the order in which the world took its gaps
	// own says that the entry's own action deleted them: they return when
	// the entry is undone and are never taken for good before.
	own bool
}

type smWorldTab struct {
	name       string
	rows, cols []uint64
	gaps       []*smGap
	deleted    bool // the tab is deleted in the sheet
	replaced   bool // a recovery put a tab of a revision under the id
	// shelf is the tab the world knew before the recovery that replaced
	// it, kept for when that tab comes back.
	shelf *smWorldTab
}

// knows reports whether the world's tab is the tab st: its rows and
// columns, apart from those its gaps hold, are the tab's.
func (wt *smWorldTab) knows(st *smTab) bool {
	live := func(axis []uint64, rows bool) []uint64 {
		return slices.DeleteFunc(slices.Clone(axis), func(id uint64) bool { return wt.gone(rows, id) != nil })
	}
	return slices.Equal(live(wt.rows, true), st.rows) && slices.Equal(live(wt.cols, false), st.cols)
}

func (wt *smWorldTab) axis(rows bool) *[]uint64 {
	if rows {
		return &wt.rows
	}
	return &wt.cols
}

type smWorld struct {
	tabs map[uint64]*smWorldTab
	kept []*smKept
	// edits counts the structural edits the world has replayed, and the
	// others what became of them, for the summary of a run.
	edits                                     int
	inserts, closed, resolved, sorts, renames int
	parted                                    int // deletions applied because of a name
	gapSeq                                    int
	// entry points at the worlds of the entry this one belongs to: one
	// for the cells an action overwrites, or one for each stretch of rows
	// or columns its deletion took. A kept deletion is applied early for
	// the entry's formulas, whichever of its worlds holds them.
	entry *[]*smWorld
}

// newWorld makes a world for the entry the writer records, from the sheet
// as it is now.
func (m *smModel) newWorld(w *smWriter) *smWorld {
	world := &smWorld{tabs: map[uint64]*smWorldTab{}}
	for i, st := range m.meta {
		world.tabs[st.id] = &smWorldTab{name: m.tabs[i].name, rows: slices.Clone(st.rows), cols: slices.Clone(st.cols)}
	}
	m.worlds = append(m.worlds, world)
	w.worlds = append(w.worlds, world)
	world.entry = &w.worlds
	return world
}

// world returns the world for the cells the writer's action overwrites,
// made before the action's first operation changes the sheet.
func (m *smModel) world(w *smWriter) *smWorld {
	if w.world == nil {
		w.world = m.newWorld(w)
	}
	return w.world
}

// keep puts a formula of the sheet into the writer's world.
func (m *smModel) keep(w *smWriter, tab, row, col uint64, f *formula.Formula) *smKept {
	if f == nil || w.rec == nil {
		return nil
	}
	return m.keepIn(m.world(w), tab, row, col, f)
}

func (m *smModel) keepIn(world *smWorld, tab, row, col uint64, f *formula.Formula) *smKept {
	if f == nil || world == nil {
		return nil
	}
	k := &smKept{tab: tab, row: row, col: col, f: f, orig: f.Stored()}
	world.kept = append(world.kept, k)
	return k
}

// smMostGaps names the summary line with the most gaps a world held.
const smMostGaps = "most gaps a world held at a time (the engine keeps 32)"

func (world *smWorld) open() (n int) {
	for _, wt := range world.tabs {
		n += len(wt.gaps)
	}
	return n
}

func (wt *smWorldTab) gone(rows bool, id uint64) *smGap {
	for _, g := range wt.gaps {
		if g.rows == rows && slices.Contains(g.ids, id) {
			return g
		}
	}
	return nil
}

func (world *smWorld) pos(k *smKept) (formula.Pos, bool) {
	if k.frozen {
		return k.at, true
	}
	wt := world.tabs[k.tab]
	if wt == nil {
		return formula.Pos{}, false
	}
	r, c := slices.Index(wt.rows, k.row), slices.Index(wt.cols, k.col)
	return formula.Pos{Row: int32(r), Col: int32(c)}, r >= 0 && c >= 0
}

// apply rewrites every kept formula of the world for one insert or delete
// of the tab, given in the world's own positions.
func (world *smWorld) apply(tab uint64, tr formula.Transform) {
	for _, k := range world.kept {
		if k.dead {
			continue
		}
		if k.frozen {
			if formula.FoldName(tr.Tab) != formula.FoldName(k.gone) {
				k.f, _, _ = k.f.Apply("", k.at, tr)
			}
			continue
		}
		pos, ok := world.pos(k)
		if !ok {
			k.dead = true
			continue
		}
		// The formula's own tab has the edit's name when it is the edit's
		// tab, and no name the edit could mean otherwise.
		name := ""
		if k.tab == tab {
			name = tr.Tab
			if _, alive := tr.Map(name, pos); !alive {
				k.dead = true
				continue
			}
		}
		k.f, _, _ = k.f.Apply(name, pos, tr)
	}
}

func transformKind(rows, insert bool) formula.TransformKind {
	switch {
	case rows && insert:
		return formula.InsertRows
	case rows:
		return formula.DeleteRows
	case insert:
		return formula.InsertCols
	}
	return formula.DeleteCols
}

// resolve makes a gap a deletion after all.
func (world *smWorld) resolve(tab uint64, g *smGap) {
	wt := world.tabs[tab]
	axis := wt.axis(g.rows)
	// The rows of a gap lay together in the sheet when they were deleted.
	// In the world, rows of older gaps can lie between them, so they go
	// stretch by stretch, the last first.
	for hi := len(*axis); hi > 0; hi-- {
		if !slices.Contains(g.ids, (*axis)[hi-1]) {
			continue
		}
		lo := hi - 1
		for lo > 0 && slices.Contains(g.ids, (*axis)[lo-1]) {
			lo--
		}
		world.apply(tab, formula.Transform{Kind: transformKind(g.rows, false), Tab: g.name, At: int32(lo), Count: int32(hi - lo)})
		*axis = slices.Delete(*axis, lo, hi)
		hi = lo + 1
	}
	wt.gaps = slices.DeleteFunc(wt.gaps, func(x *smGap) bool { return x == g })
}

// resolveShelved makes a gap of the tab the world keeps aside a deletion
// after all, with the places that tab had.
func (world *smWorld) resolveShelved(tab uint64, g *smGap) {
	wt := world.tabs[tab]
	world.tabs[tab] = wt.shelf
	world.resolve(tab, g)
	world.tabs[tab] = wt
}

// depends reports whether the world's entry depends on a tab that has the
// given name: a formula it keeps lies in the tab or names it, or the world
// keeps a deletion of the tab. An entry follows the edits of the tabs it
// depends on and no others (Appendix B, engine items 12 and 23). The
// formulas of a deleted tab lie nowhere, and their own tab's name is not a
// name they depend on.
func (world *smWorld) depends(tab uint64, name string) bool {
	if wt := world.tabs[tab]; wt != nil && len(wt.gaps) > 0 {
		return true
	}
	folded := formula.FoldName(name)
	for _, k := range world.kept {
		if !k.frozen && k.tab == tab {
			return true
		}
		if k.frozen && formula.FoldName(k.gone) == folded {
			continue
		}
		named := false
		k.f.Tabs(func(n string) { named = named || formula.FoldName(n) == folded })
		if named {
			return true
		}
	}
	return false
}

// names counts the tab names the world's formulas hold and the tabs it
// lies in. The engine stops telling tabs apart above depsKept of either,
// and a history of the test has to stay below that.
func (world *smWorld) names() (names, tabs int) {
	held, lies := map[string]bool{}, map[uint64]bool{}
	for _, k := range world.kept {
		if !k.frozen {
			lies[k.tab] = true
		}
		k.f.Tabs(func(n string) { held[formula.FoldName(n)] = true })
	}
	for id, wt := range world.tabs {
		if len(wt.gaps) > 0 {
			lies[id] = true
		}
	}
	return len(held), len(lies)
}

// smNamed is the i-th reference of a kept formula that is written with a
// tab name. A rename gives a reference another name and leaves its place
// among them.
type smNamed struct {
	k *smKept
	i int
}

// under returns the references that the formulas of the world's entry
// write with the given tab name. after, if not nil, gives the form of each
// formula to look at instead of the one it has.
func (world *smWorld) under(name string, after func(*smKept) *formula.Formula) map[smNamed]bool {
	out := map[smNamed]bool{}
	for _, mate := range *world.entry {
		for _, k := range mate.kept {
			f := k.f
			if after != nil {
				f = after(k)
			}
			i := 0
			f.Tabs(func(n string) {
				if formula.FoldName(n) == formula.FoldName(name) {
					out[smNamed{k, i}] = true
				}
				i++
			})
		}
	}
	return out
}

// smOpen is a deletion a world keeps, with its tab.
type smOpen struct {
	id uint64
	g  *smGap
}

// early applies kept deletions that an edit would mix up with other
// references, before the edit.
func (world *smWorld) early(list []smOpen) {
	for _, o := range list {
		world.resolve(o.id, o.g)
		world.resolved++
		world.parted++
	}
}

// parts returns, for each world for which follows holds, the kept
// deletions whose references an insert or a deletion in the tab with the
// given name would rewrite. Those are the deletions of other tabs that
// have that name for the model, where a formula of the entry is written
// with it. The other tab is deleted, or came back under another name, and
// the references read the tab of the edit now. A deletion that no
// reference of the entry is written under stays, because the edit
// rewrites nothing it concerns.
//
// Every world is looked at before the edit changes any. The formulas of
// one entry can lie in several worlds, and the answer is one for the
// entry as it is before the edit.
func (m *smModel) parts(tab uint64, name string, follows func(*smWorld) bool) map[*smWorld][]smOpen {
	out := map[*smWorld][]smOpen{}
	for _, world := range m.worlds {
		if !follows(world) {
			continue
		}
		for id, wt := range world.tabs {
			if id == tab {
				continue
			}
			for _, g := range wt.gaps {
				if !g.own && formula.FoldName(g.name) == formula.FoldName(name) && len(world.under(g.name, nil)) > 0 {
					out[world] = append(out[world], smOpen{id, g})
				}
			}
		}
	}
	return out
}

// worldDeleted notes a stretch of rows or columns that the tab with the
// given name lost. own are the worlds of the entry whose action deleted
// them. A world that depends on the tab keeps the deletion as a gap; for
// any other the rows are gone, and no formula of it could tell.
func (m *smModel) worldDeleted(own []*smWorld, tab uint64, name string, rows bool, ids []uint64, above, below uint64) {
	parts := m.parts(tab, name, func(world *smWorld) bool {
		return world.tabs[tab] != nil && !slices.Contains(own, world) && world.depends(tab, name)
	})
	for _, world := range m.worlds {
		wt := world.tabs[tab]
		if wt == nil {
			continue
		}
		mine := slices.Contains(own, world)
		if !mine && !world.depends(tab, name) {
			axis := wt.axis(rows)
			*axis = slices.DeleteFunc(*axis, func(id uint64) bool { return slices.Contains(ids, id) })
			continue
		}
		if !mine {
			world.edits++
			world.early(parts[world])
		}
		world.gapSeq++
		wt.gaps = append(wt.gaps, &smGap{rows: rows, ids: slices.Clone(ids), above: above, below: below, own: mine, name: name, seq: world.gapSeq})
		// The engine keeps 32 deletions apart for an entry, the rows its
		// own action deleted not counted, and applies the oldest when
		// one more comes.
		var oldest *smGap
		oldestTab, open, shelved := uint64(0), 0, false
		for id, t := range world.tabs {
			gaps := t.gaps
			if t.shelf != nil {
				gaps = append(slices.Clone(gaps), t.shelf.gaps...)
			}
			for k, g := range gaps {
				if g.own {
					continue
				}
				if open++; oldest == nil || g.seq < oldest.seq {
					oldest, oldestTab, shelved = g, id, k >= len(t.gaps)
				}
			}
		}
		if open > gapsKept {
			if shelved {
				world.resolveShelved(oldestTab, oldest)
			} else {
				world.resolve(oldestTab, oldest)
			}
			world.resolved++
			if m.paths != nil {
				m.paths["a kept deletion applied because its entry kept 32 newer ones"]++
			}
			open--
		}
		if m.paths != nil && open > m.paths[smMostGaps] {
			m.paths[smMostGaps] = open
		}
		if names, tabs := world.names(); (names > depsKept || tabs > depsKept) && m.fault == nil {
			m.fault = fmt.Errorf("an undo entry of the model depends on %d names and %d tabs: above %d the engine follows every edit, which the model does not", names, tabs, depsKept)
		}
	}
}

// worldInserted replays an insert: new rows or columns, or ones an undo
// brings back. before is the id below the place in the sheet, 0 for the
// end.
//
// The place in the world is above that row. Gaps that lie there stay above
// the new rows while the row that was above them still stands, and go
// below them when it does not (Appendix B, engine item 17): that is where
// the rows of each gap would return to. Rows that come back and reach
// their own gap this way are back in it, and the gap closes. Rows that
// come back elsewhere leave their gap a deletion.
func (m *smModel) worldInserted(tab uint64, name string, rows bool, ids []uint64, before uint64) {
	parts := m.parts(tab, name, func(world *smWorld) bool { return world.tabs[tab] != nil && world.depends(tab, name) })
	for _, world := range m.worlds {
		wt := world.tabs[tab]
		if wt == nil {
			continue
		}
		axis := wt.axis(rows)
		if !world.depends(tab, name) {
			// The entry does not follow the edit, and the world keeps no
			// deletion of the tab: the rows only take their place.
			at := len(*axis)
			if before != 0 {
				at = slices.Index(*axis, before)
			}
			*axis = slices.Insert(*axis, at, ids...)
			continue
		}
		world.edits++
		world.early(parts[world])
		var back *smGap // the gap of the rows, if they come back
		for _, g := range wt.gaps {
			if g.rows == rows && slices.Contains(ids, g.ids[0]) {
				back = g
			}
		}
		// through says that the entry's own rows are not in the way: rows
		// that come back reach their gap as if the own rows, which are
		// not in the sheet either, were not there.
		place := func(through bool) (at int, home bool) {
			at = len(*axis)
			if before != 0 {
				at = slices.Index(*axis, before)
			}
			for at > 0 {
				g := wt.gone(rows, (*axis)[at-1])
				if g == nil {
					break
				}
				if g == back {
					return at, slices.Equal(g.ids, ids)
				}
				// The rows of a gap return above the row that was below
				// them. So new rows lie below the gap when they bring
				// that row back, and while the gap's upper neighbour
				// stands.
				if !(through && g.own) {
					if g.below != 0 && slices.Contains(ids, g.below) {
						break
					}
					if g.above != 0 && slices.Contains(*axis, g.above) && wt.gone(rows, g.above) == nil {
						break
					}
				}
				for at > 0 && wt.gone(rows, (*axis)[at-1]) == g {
					at--
				}
			}
			return at, false
		}
		if _, home := place(true); home {
			wt.gaps = slices.DeleteFunc(wt.gaps, func(x *smGap) bool { return x == back })
			if !back.own {
				world.closed++
			}
			continue
		}
		own := false
		if back != nil {
			if own = back.own; own {
				// The entry's own rows return with their formulas as
				// they are stored (engine item 23): they move to the new
				// place in the world without a rewrite.
				*axis = slices.DeleteFunc(*axis, func(id uint64) bool { return slices.Contains(back.ids, id) })
				wt.gaps = slices.DeleteFunc(wt.gaps, func(x *smGap) bool { return x == back })
			} else {
				world.resolve(tab, back)
				world.resolved++
			}
			back = nil
		}
		at, _ := place(false)
		world.inserts++
		if !own {
			world.apply(tab, formula.Transform{Kind: transformKind(rows, true), Tab: name, At: int32(at), Count: int32(len(ids))})
		}
		*axis = slices.Insert(*axis, at, ids...)
	}
}

// worldAppended notes new rows or columns at the end of a tab. They lie
// after everything a world holds of the tab, its gaps included, and no
// kept formula changes.
func (m *smModel) worldAppended(tab uint64, rows bool, ids []uint64) {
	for _, world := range m.worlds {
		if wt := world.tabs[tab]; wt != nil {
			axis := wt.axis(rows)
			*axis = append(*axis, ids...)
		}
	}
}

// worldSorted replays a sort: the rows at the given positions of the sheet
// now hold the given ids. Every gap of rows in the tab becomes a deletion,
// and a kept formula whose own row moved shifts with it.
func (m *smModel) worldSorted(tab uint64, was, now []uint64) {
	for _, world := range m.worlds {
		wt := world.tabs[tab]
		if wt == nil {
			continue
		}
		if world.depends(tab, "") {
			world.edits++
			world.sorts++
		}
		for _, g := range slices.Clone(wt.gaps) {
			if g.rows && !g.own {
				world.resolve(tab, g)
				world.resolved++
			}
		}
		if wt.shelf != nil {
			for _, g := range slices.Clone(wt.shelf.gaps) {
				if g.rows && !g.own {
					world.resolveShelved(tab, g)
					world.resolved++
				}
			}
		}
		from := map[uint64]formula.Pos{}
		for _, k := range world.kept {
			if pos, ok := world.pos(k); ok && k.tab == tab && !k.dead {
				from[k.row] = pos
			}
		}
		places := make([]int, len(was))
		for i, id := range was {
			places[i] = slices.Index(wt.rows, id)
		}
		for i, id := range now {
			if places[i] >= 0 {
				wt.rows[places[i]] = id
			}
		}
		for _, k := range world.kept {
			if k.tab != tab || k.dead {
				continue
			}
			if to, ok := world.pos(k); ok && to.Row != from[k.row].Row {
				at := formula.Pos{Row: from[k.row].Row, Col: to.Col}
				k.f = k.f.Moved(at, to)
			}
		}
	}
}

// worldRenamed replays the rename of a tab. A world that depends on the
// tab renames the references that name it.
//
// A kept deletion changes the references that named its tab when the rows
// went, and the model finds them by the name the deletion has. A rename
// of the deletion's tab from that name takes the deletion along. So for
// each deletion the world keeps, the references written with its name
// before the rename must be the ones written with its name after it. If
// the rename would add a reference to them or take one away, the deletion
// is applied first. A rename to the same name, or to the same name in
// other capitals, changes the name of no reference, and one to a name no
// formula holds adds none.
func (m *smModel) worldRenamed(tab uint64, tr formula.Transform) {
	renamed := func(k *smKept) *formula.Formula {
		if k.frozen && formula.FoldName(k.gone) == formula.FoldName(tr.Tab) {
			return k.f // the name of its own tab, which another tab has now
		}
		f, _, _ := k.f.Apply("", formula.Pos{}, tr)
		return f
	}
	// follows says that the deletion is one of the renamed tab under the
	// name the tab gives up.
	follows := func(id uint64, g *smGap) bool {
		return id == tab && formula.FoldName(g.name) == formula.FoldName(tr.Tab)
	}
	// Every world is looked at before the rename changes any, because the
	// formulas of one entry can lie in several.
	mixed := map[*smWorld][]smOpen{}
	var depend []*smWorld
	for _, world := range m.worlds {
		if !world.depends(tab, tr.Tab) {
			continue
		}
		depend = append(depend, world)
		for id, t := range world.tabs {
			for _, g := range t.gaps {
				if g.own {
					continue
				}
				name := g.name
				if follows(id, g) {
					name = tr.Name
				}
				if !maps.Equal(world.under(g.name, nil), world.under(name, renamed)) {
					mixed[world] = append(mixed[world], smOpen{id, g})
				} else if follows(id, g) && formula.FoldName(tr.Tab) == formula.FoldName(tr.Name) && m.paths != nil {
					m.paths["a kept deletion that stayed through a rename to the same name, or in other capitals"]++
				}
			}
		}
	}
	for _, world := range m.worlds {
		wt := world.tabs[tab]
		if !slices.Contains(depend, world) {
			if wt != nil {
				wt.name = tr.Name
			}
			continue
		}
		world.edits++
		world.renames++
		world.early(mixed[world])
		for _, k := range world.kept {
			k.f = renamed(k)
		}
		if wt != nil {
			wt.name = tr.Name
			for _, g := range wt.gaps {
				if follows(tab, g) {
					g.name = tr.Name
				}
			}
		}
	}
}

// worldTab notes a tab that the sheet gained, with its axes, or that came
// back, or that it lost.
func (m *smModel) worldTab(st *smTab, name string, deleted bool) {
	for _, world := range m.worlds {
		wt := world.tabs[st.id]
		if wt == nil {
			wt = &smWorldTab{rows: slices.Clone(st.rows), cols: slices.Clone(st.cols)}
			world.tabs[st.id] = wt
		}
		wt.name, wt.deleted = name, deleted
	}
}

// copy returns the axes and the gaps of the world, without its formulas,
// for working out what an undo would find without changing the entry.
func (world *smWorld) copy() *smWorld {
	scratch := &smWorld{tabs: map[uint64]*smWorldTab{}}
	cp := func(wt *smWorldTab) *smWorldTab {
		c := *wt
		c.rows, c.cols, c.gaps = slices.Clone(wt.rows), slices.Clone(wt.cols), slices.Clone(wt.gaps)
		return &c
	}
	for id, wt := range world.tabs {
		c := cp(wt)
		if wt.shelf != nil {
			c.shelf = cp(wt.shelf)
		}
		scratch.tabs[id] = c
	}
	return scratch
}

// settled returns a kept formula as its entry's undo writes it: with every
// gap that is still open made a deletion. It works on a copy of the world,
// so that an undo that is refused or skipped leaves the entry as it was.
//
// at is where the cell lies in the sheet now, for a cell that never left
// it: the model must have followed the formula to that place. A formula in
// rows or columns that the undo is about to bring back has no place yet
// (at is nil): it is settled where its gap is, and keeps that form at the
// place the rows return to, also when that is another one (Appendix B,
// engine item 23).
func (m *smModel) settled(world *smWorld, k *smKept, at *formula.Pos) (*formula.Formula, error) {
	if k == nil {
		return nil, nil
	}
	scratch := world.copy()
	scratch.kept = []*smKept{{tab: k.tab, row: k.row, col: k.col, f: k.f, orig: k.orig, dead: k.dead, frozen: k.frozen, at: k.at, gone: k.gone}}
	open := 0
	for id, wt := range scratch.tabs {
		for _, g := range slices.Clone(wt.gaps) {
			if g.own {
				continue // the rows the undo brings back
			}
			scratch.resolve(id, g)
			open++
		}
		// The steps of the tab set aside are skipped, its own rows
		// included, so every gap it holds is a deletion.
		if wt.shelf == nil {
			continue
		}
		for _, g := range slices.Clone(wt.shelf.gaps) {
			scratch.resolveShelved(id, g)
			open++
		}
	}
	out := scratch.kept[0]
	if m.paths != nil {
		// What the formula went through before this undo wrote it.
		for name, n := range map[string]int{
			"an insert": world.inserts, "a deletion that was taken back": world.closed, "a deletion that stayed": world.resolved + open,
			"a sort": world.sorts, "a rename": world.renames, "a deletion applied because a name passed on": world.parted} {
			if n > 0 {
				m.paths["a kept formula written by an undo after "+name]++
			}
		}
		if world.edits > 0 && out.f.Stored() != k.orig {
			m.paths["a kept formula written by an undo with another text than it was kept with"]++
		}
		if out.dead {
			m.paths["a kept formula whose own row or column was deleted for good"]++
		}
	}
	if pos, ok := scratch.pos(out); at != nil && !out.dead && (!ok || pos != *at) {
		return nil, fmt.Errorf("the model followed a kept formula of cell %d/%d to %v, and the cell lies at %v", k.row, k.col, pos, *at)
	}
	return out.f, nil
}
