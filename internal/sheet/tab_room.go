package sheet

import "slices"

// A tab's maps and arrays keep the room of the most they held: a Go map
// keeps its table after its entries are deleted, and an array its length
// when a list shrinks inside it. A revert takes out what a refused or
// failed action put in, and a deletion takes rows out of a tab, so what
// they hold can fall far below that room. The tick makes such a map or
// array again for what it holds (compactTabs); a row's list of cells is
// made again as it shrinks (clipped).

// tabRoomWork is what one tick may spend making the maps and arrays of the
// tabs again, in the steps of a ticker: a million entries or ids. It is
// more than the largest of them holds, so each tick makes at least one
// again while one is due.
const tabRoomWork = 1 << 20 * paceSteps

// roomDue reports whether a structure that held most entries and holds n
// now keeps enough room for the others to be made again.
func roomDue(most, n int) bool { return most > 2*n+1024 }

// dropRow takes a row out of data, noting the size it had.
func (t *Tab) dropRow(row RowID) {
	t.dataMost = max(t.dataMost, len(t.data))
	delete(t.data, row)
}

// dropWidth takes a column's width out of widths, noting the size it had.
func (t *Tab) dropWidth(col ColID) {
	t.widthsMost = max(t.widthsMost, len(t.widths))
	delete(t.widths, col)
}

// dropSort takes a row's last sort out of lastSort, noting the size it had.
func (t *Tab) dropSort(row RowID) {
	t.sortsMost = max(t.sortsMost, len(t.lastSort))
	delete(t.lastSort, row)
}

// clipped returns the cells of a row in an array of their own size once
// they take a quarter of the one they lie in or less. Making the array
// again only then costs each removal a constant share of the copies.
func clipped(cells []cellSlot) []cellSlot {
	if c := cap(cells); c > 16 && len(cells) <= c/4 {
		return slices.Clip(slices.Clone(cells))
	}
	return cells
}

// compactTabs makes the maps and arrays of the tabs again where they keep
// the room of many more entries than they hold, within work steps. Each is
// made whole before it replaces the old one, so a stop leaves it as it
// was, and the next tick goes on with what is still due.
func (in *Instance) compactTabs(work int) {
	more := func(steps int) bool {
		work -= steps
		return work >= 0
	}
	if !in.wb.styles.compactRoom(more) || !in.wb.wire.compactRoom(more) {
		return
	}
	for _, t := range in.wb.tabs {
		if !t.compactRoom(more) {
			return
		}
	}
}

// compactRoom makes again what of the tab is due (roomDue). It returns
// false where more stopped it. What reads see stays as it is: the maps
// hold the same entries, the axes the same ids in the same order with the
// same positions and born stamps, and the row files are the same.
func (t *Tab) compactRoom(more pacer) bool {
	tk := ticker{more: more}
	if roomDue(t.dataMost, len(t.data)) {
		data, ok := compactMap(t.data, &tk)
		if !ok {
			return false
		}
		t.data, t.dataMost = data, len(data)
	}
	if roomDue(t.widthsMost, len(t.widths)) {
		widths, ok := compactMap(t.widths, &tk)
		if !ok {
			return false
		}
		t.widths, t.widthsMost = widths, len(widths)
	}
	if roomDue(t.sortsMost, len(t.lastSort)) {
		sorts, ok := compactMap(t.lastSort, &tk)
		if !ok {
			return false
		}
		t.lastSort, t.sortsMost = sorts, len(sorts)
	}
	for _, a := range []*axis{&t.rows, &t.cols} {
		// The map of positions has one entry for each id, so it held no
		// more than the array of ids has room for.
		if roomDue(cap(a.ids), len(a.ids)) || roomDue(cap(a.born), len(a.born)) {
			fresh, ok := a.compacted(&tk)
			if !ok {
				return false
			}
			*a = fresh
		}
	}
	if roomDue(cap(t.byPos), len(t.rows.ids)) {
		if !tk.add(len(t.rows.ids)) {
			return false
		}
		t.byPos, t.byPosOK = nil, false
		t.rowsByPos()
	}
	if roomDue(cap(t.chunks), len(t.chunks)) {
		if !tk.add(len(t.chunks)) {
			return false
		}
		t.chunks = slices.Clone(t.chunks)
	}
	return tk.done()
}

// compactRoom forgets the style states no cell uses, and makes the maps of
// the states again once they hold far fewer than before (roomDue). That
// is after the revert of a refused or failed action, which leaves the room
// of the states it interned, and after a cleared range. It returns false
// where more stopped it.
func (t *styleTable) compactRoom(more pacer) bool {
	tk := ticker{more: more}
	if !t.dropUnused(&tk) || !t.refillRoom(&tk) {
		return false
	}
	if roomDue(t.most, len(t.ids)) {
		byID, ok := compactMap(t.byID, &tk)
		if !ok {
			return false
		}
		ids, ok := compactMap(t.ids, &tk)
		if !ok {
			return false
		}
		t.byID, t.ids, t.most = byID, ids, len(ids)
	}
	if roomDue(t.touchedMost, len(t.touched)) {
		touched, ok := compactMap(t.touched, &tk)
		if !ok {
			return false
		}
		t.touched, t.touchedMost = touched, len(touched)
	}
	return tk.done()
}

// compactRoom makes the wire list and its index again once the list keeps
// room for far more formats than it holds. The revert of a refused action
// takes out the formats it added and leaves their room. The index has one
// entry for each format, so it held no more than the list has room for.
// Every format keeps its index, so the wire epoch stays. It returns false
// where more stopped it.
func (w *wireFormats) compactRoom(more pacer) bool {
	if !roomDue(cap(w.list), len(w.list)) {
		return true
	}
	tk := ticker{more: more}
	idx, ok := compactMap(w.idx, &tk)
	if !ok || !tk.add(len(w.list)) {
		return false
	}
	w.list, w.idx = slices.Clip(slices.Clone(w.list)), idx
	return true
}

// clearRoom empties a map that is used again, and makes it again where it
// held many entries, which it would keep the room of.
func clearRoom[K comparable, V any](m *map[K]V) {
	if len(*m) > 1024 {
		*m = make(map[K]V)
		return
	}
	clear(*m)
}
