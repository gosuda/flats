package notebook

import (
	"cmp"
	"slices"
)

// History stores logical forest snapshots and assignment footprints. Components
// are connected dependencies in that forest, independent of executor journals.
type modelWrite struct {
	ID     ID
	Groups []GroupKey
}

type modelTransition struct {
	Before, After *notebookModel
	Components    [][]modelWrite
	Anchors       map[ID]Ref
}

type modelHistory struct {
	Undo, Redo map[string][]modelTransition
}

func newModelHistory() *modelHistory {
	return &modelHistory{Undo: map[string][]modelTransition{}, Redo: map[string][]modelTransition{}}
}

func modelOwnGroups(id ID) []GroupKey {
	if id.Kind() == PageKind {
		return []GroupKey{TitleGroup, PlaceGroup}
	}
	return []GroupKey{ContentGroup, PlaceGroup}
}

func modelFootprint(before, after *notebookModel) []modelWrite {
	var writes []modelWrite
	for _, id := range append(after.ids(PageKind, false), after.ids(BlockKind, false)...) {
		a, b := before.Items[id], after.Items[id]
		w := modelWrite{ID: id}
		if a == nil || a.Alive != b.Alive {
			writes = append(writes, w)
			continue
		}
		for _, g := range modelOwnGroups(id) {
			if after.Assigned[id][g] || a.version(g) != b.version(g) {
				w.Groups = append(w.Groups, g)
			}
		}
		if len(w.Groups) > 0 {
			writes = append(writes, w)
		}
	}
	return writes
}

func modelTransitionFor(before, after *notebookModel, ops []Operation) modelTransition {
	tr := modelTransition{Before: before.clone(), After: after.clone()}
	writes := modelFootprint(before, after)
	if len(writes) == 0 {
		return tr
	}
	if len(ops) != 1 || ops[0].operationMeta().UnitID != nil {
		tr.Components = [][]modelWrite{writes}
		return tr
	}
	if _, turn := ops[0].(*TurnBlocksOp); !turn {
		tr.Components = [][]modelWrite{writes}
		return tr
	}
	// A conversion owns the immediate children it hoists. Sharing one of those
	// nodes connects conversions transitively, but ancestry alone does not.
	remaining := map[ID]modelWrite{}
	for _, w := range writes {
		remaining[w.ID] = w
	}
	linked := func(a, b ID) bool {
		child, parent := before.Items[a], after.Items[b]
		return child != nil && parent != nil && child.Parent == b &&
			child.location() != after.Items[a].location() && !modelHolds(parent.Content.Kind)
	}
	for _, root := range writes {
		if _, ok := remaining[root.ID]; !ok {
			continue
		}
		component := []modelWrite{root}
		delete(remaining, root.ID)
		for at := 0; at < len(component); at++ {
			for _, w := range writes {
				if _, ok := remaining[w.ID]; ok && (linked(w.ID, component[at].ID) || linked(component[at].ID, w.ID)) {
					component = append(component, w)
					delete(remaining, w.ID)
				}
			}
		}
		slices.SortFunc(component, func(a, b modelWrite) int { return cmp.Compare(a.ID, b.ID) })
		tr.Components = append(tr.Components, component)
	}
	return tr
}

func (h *modelHistory) record(session string, tr modelTransition) {
	if session != "" && len(tr.Components) > 0 {
		h.Undo[session] = append(h.Undo[session], tr)
		delete(h.Redo, session)
	}
}

type modelUndoCounts struct {
	Applied, Skipped, Later, Missing, Structure int
	Empty                                       bool
}

func modelCounts(s undoSummary) modelUndoCounts {
	return modelUndoCounts{Applied: s.Applied, Skipped: s.Skipped, Later: s.LaterEdits, Missing: s.Missing, Structure: s.Structure, Empty: s.Empty}
}

func (tr modelTransition) blocked(m *notebookModel, component []modelWrite) string {
	deleting := map[ID]bool{}
	for _, w := range component {
		current, written, desired := m.Items[w.ID], tr.After.Items[w.ID], tr.Before.Items[w.ID]
		if current == nil || current.Ref != written.Ref || current.Alive != written.Alive {
			return "missing"
		}
		if !current.Alive {
			if current.Deleted != written.Deleted {
				return "later"
			}
			continue
		}
		groups := w.Groups
		if desired == nil || !desired.Alive {
			deleting[w.ID] = true
			groups = modelOwnGroups(w.ID)
		}
		for _, g := range groups {
			if current.version(g) != written.version(g) {
				return "later"
			}
		}
	}
	for id := range deleting {
		for child, n := range m.Items {
			if n.Alive && !deleting[child] && (n.Parent == id || n.Page == id) {
				return "later"
			}
		}
	}
	return ""
}

func (tr modelTransition) restore(m *notebookModel, component []modelWrite, seq Sequence) string {
	reviving := map[ID]bool{}
	var placing []ID
	for _, w := range component {
		n, desired := m.Items[w.ID], tr.Before.Items[w.ID]
		oldPage := n.Page
		switch {
		case desired == nil || !desired.Alive:
			m.detach(w.ID)
			n.Alive, n.Deleted = false, seq
		case !n.Alive:
			reviving[w.ID] = true
			copy := *desired
			copy.Ref.Inc, copy.Alive, copy.Deleted = Incarnation(seq), true, 0
			copy.Versions, copy.Aggregate = map[GroupKey]Version{}, Version(seq)
			m.Items[w.ID] = &copy
			placing = append(placing, w.ID)
		default:
			for _, g := range w.Groups {
				switch g {
				case TitleGroup:
					n.Title = desired.Title
				case ContentGroup:
					n.Content = desired.Content
				case PlaceGroup:
					placing = append(placing, w.ID)
					continue
				}
				m.assign(n, g, seq)
			}
		}
		if oldPage != "" {
			m.Items[oldPage].Aggregate = Version(seq)
		}
	}
	// Restore predecessor edges in their old list order, never key-space order.
	slices.SortFunc(placing, func(a, b ID) int {
		x, y := tr.Before.Items[a], tr.Before.Items[b]
		if x.location() == y.location() {
			list := tr.Before.Order[x.location()]
			return slices.Index(list, a) - slices.Index(list, b)
		}
		return cmp.Or(cmp.Compare(a.Kind(), b.Kind()), cmp.Compare(x.Page, y.Page), cmp.Compare(x.Parent, y.Parent))
	})
	for _, id := range placing {
		if !reviving[id] {
			m.detach(id)
		}
	}
	for _, id := range placing {
		n, desired := m.Items[id], tr.Before.Items[id]
		loc := desired.location()
		if loc.Parent != "" && (m.Items[loc.Parent] == nil || !m.Items[loc.Parent].Alive) {
			if !reviving[id] {
				return "structure"
			}
			loc.Parent = ""
		}
		if id.Kind() == BlockKind && (m.Items[loc.Page] == nil || !m.Items[loc.Page].Alive) {
			return "structure"
		}
		list := tr.Before.Order[desired.location()]
		at := slices.Index(list, id)
		anchor := func(index int) *Target {
			if index < 0 || index >= len(list) {
				return nil
			}
			other := m.Items[list[index]]
			if other == nil || !other.Alive || other.location() != loc ||
				(!reviving[list[index]] && other.Ref != tr.anchorRef(list[index])) ||
				!slices.Contains(m.Order[loc], list[index]) {
				return nil
			}
			return modelPointer(Target{Ref: &other.Ref})
		}
		after := anchor(at - 1)
		var before *Target
		if after == nil {
			before = anchor(at + 1)
		}
		if m.insert([]ID{id}, loc, before, after) != "" {
			return "structure"
		}
		m.assign(n, PlaceGroup, seq)
	}
	if m.valid() != "" {
		return "structure"
	}
	return ""
}

func (tr modelTransition) anchorRef(id ID) Ref {
	if ref, ok := tr.Anchors[id]; ok {
		return ref
	}
	return tr.Before.Items[id].Ref
}

func (h *modelHistory) relink(session string, tr modelTransition, component []modelWrite, before, after *notebookModel) {
	for _, stack := range []map[string][]modelTransition{h.Undo, h.Redo} {
		for i := range stack[session] {
			older := &stack[session][i]
			for _, w := range component {
				id := w.ID
				desired, previous, current := tr.Before.Items[id], before.Items[id], after.Items[id]
				if desired == nil || !desired.Alive {
					continue
				}
				revived := !previous.Alive && current.Alive
				if revived && older.Before.Items[id] != nil && older.anchorRef(id) == previous.Ref {
					if older.Anchors == nil {
						older.Anchors = map[ID]Ref{}
					}
					older.Anchors[id] = current.Ref
				}
				written := older.After.Items[id]
				if written == nil || written.Ref != previous.Ref {
					continue
				}
				groups := w.Groups
				if revived {
					groups = modelOwnGroups(id)
					versions := map[GroupKey]Version{}
					for _, g := range groups {
						versions[g] = written.version(g)
					}
					written.Versions = versions
					written.Ref = current.Ref
				}
				for _, g := range groups {
					if written.version(g) == desired.version(g) {
						written.Versions[g] = current.version(g)
					}
				}
			}
		}
	}
}

func (h *modelHistory) control(m *notebookModel, session string, redo bool, seq Sequence) modelUndoCounts {
	from, to := h.Undo, h.Redo
	if redo {
		from, to = to, from
	}
	stack := from[session]
	if len(stack) == 0 {
		return modelUndoCounts{Empty: true}
	}
	tr := stack[len(stack)-1]
	from[session] = stack[:len(stack)-1]
	start := m.clone()
	reasons := make([]string, len(tr.Components))
	for i, component := range tr.Components {
		reasons[i] = tr.blocked(start, component)
	}
	result := modelUndoCounts{}
	reverse := modelTransition{Before: start}
	for i, component := range tr.Components {
		reason := reasons[i]
		if reason == "" {
			candidate := m.clone()
			reason = tr.restore(candidate, component, seq)
			if reason == "" {
				reverse.Components = append(reverse.Components, modelFootprint(m, candidate))
				h.relink(session, tr, component, m, candidate)
				*m = *candidate
				result.Applied++
			}
		}
		if reason != "" {
			result.Skipped++
			switch reason {
			case "later":
				result.Later++
			case "missing":
				result.Missing++
			case "structure":
				result.Structure++
			}
		}
	}
	if len(reverse.Components) > 0 {
		reverse.After = m.clone()
		to[session] = append(to[session], reverse)
	}
	return result
}
