package notebook

import (
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
)

var notebookModelSeeds = flag.Int("notebook-model-seeds", 16, "number of deterministic notebook reference-model seeds (use 200 for the full stress run)")

// The oracle uses adjacency lists and writer provenance, not fractional keys,
// SQL helpers, engine conversion functions, or serialized undo entries.
type modelPlace struct {
	Kind   Kind
	Page   ID
	Parent ID
}

type modelItem struct {
	Ref       Ref
	Alive     bool
	Deleted   Sequence
	Parent    ID
	Page      ID
	Title     string
	Content   Content
	Versions  map[GroupKey]Version
	Aggregate Version
}

type notebookModel struct {
	Items map[ID]*modelItem
	Order map[modelPlace][]ID
	// Equal assignments in separate units can share a mutation sequence.
	Assigned map[ID]map[GroupKey]bool
}

func newNotebookModel() *notebookModel {
	m := &notebookModel{Items: map[ID]*modelItem{}, Order: map[modelPlace][]ID{}, Assigned: map[ID]map[GroupKey]bool{}}
	for i := byte(1); i <= 2; i++ {
		id := vocabID(PageKind, i)
		m.Items[id] = &modelItem{Ref: Ref{ID: id, Inc: 1}, Alive: true, Title: fmt.Sprintf("page%d", i), Versions: map[GroupKey]Version{}, Aggregate: 1}
		m.Order[modelPlace{Kind: PageKind}] = append(m.Order[modelPlace{Kind: PageKind}], id)
	}
	return m
}

func (m *notebookModel) clone() *notebookModel {
	c := &notebookModel{Items: map[ID]*modelItem{}, Order: map[modelPlace][]ID{}, Assigned: map[ID]map[GroupKey]bool{}}
	for id, n := range m.Items {
		v := *n
		v.Versions = maps.Clone(n.Versions)
		c.Items[id] = &v
	}
	for p, ids := range m.Order {
		c.Order[p] = slices.Clone(ids)
	}
	return c
}

func (n *modelItem) location() modelPlace {
	return modelPlace{Kind: n.Ref.ID.Kind(), Page: n.Page, Parent: n.Parent}
}
func (n *modelItem) version(g GroupKey) Version {
	if v, ok := n.Versions[g]; ok {
		return v
	}
	return Version(n.Ref.Inc)
}
func (n *modelItem) ver() Version {
	v := max(Version(n.Ref.Inc), n.Aggregate)
	for _, gv := range n.Versions {
		v = max(v, gv)
	}
	return v
}
func (m *notebookModel) assign(n *modelItem, g GroupKey, seq Sequence) {
	n.Versions[g] = Version(seq)
	if m.Assigned[n.Ref.ID] == nil {
		m.Assigned[n.Ref.ID] = map[GroupKey]bool{}
	}
	m.Assigned[n.Ref.ID][g] = true
	if n.Page != "" {
		m.Items[n.Page].Aggregate = Version(seq)
	}
}
func modelHolds(k ContentKind) bool { return k == Bullet || k == Numbered || k == Todo || k == Toggle }
func modelText(k ContentKind) bool {
	return slices.Contains([]ContentKind{Paragraph, Heading1, Heading2, Heading3, Bullet, Numbered, Todo, Toggle, Quote, Callout}, k)
}
func modelConversion(a, b ContentKind) bool {
	return a == b || (modelText(a) || a == Code) && (modelText(b) || b == Code)
}
func modelTurn(c Content, k ContentKind) Content {
	if c.Kind == k {
		return c
	}
	text := c.Text
	if c.Kind == Code {
		text = RichText{{Text: c.Code, Marks: []Mark{}}}
	}
	out := Content{Kind: k, Text: text}
	if k == Callout {
		out.Tone = NeutralTone
	}
	if k == Code {
		out.Text = nil
		for _, run := range text {
			out.Code += run.Text
		}
	}
	return out
}
func modelID(t *Target) ID {
	if t == nil {
		return ""
	}
	return t.Ref.ID
}

func modelTargets(op Operation) []Target {
	var targets []Target
	add := func(t *Target) {
		if t != nil {
			targets = append(targets, *t)
		}
	}
	place := func(p Placement) { targets = append(targets, p.Page); add(p.Parent); add(p.Before); add(p.After) }
	for _, s := range op.operationMeta().Structure {
		targets = append(targets, s.Block, s.Page)
		add(s.Parent)
		add(s.After)
	}
	switch op := op.(type) {
	case *CreatePageOp:
		add(op.Parent)
		add(op.Before)
	case *SetPageTitleOp:
		targets = append(targets, op.Page)
	case *MovePageOp:
		targets = append(targets, op.Page)
		add(op.Parent)
		add(op.Before)
	case *DeletePageOp:
		targets = append(targets, op.Page)
	case *CreateBlockOp:
		place(op.Place)
	case *SetBlockOp:
		targets = append(targets, op.Block)
	case *TurnBlocksOp:
		targets = append(targets, op.Blocks...)
	case *MoveBlocksOp:
		targets = append(targets, op.Blocks...)
		place(op.Place)
	case *DeleteBlocksOp:
		targets = append(targets, op.Blocks...)
	default:
		panic(fmt.Sprintf("unmodeled operation %T", op))
	}
	return targets
}

func (m *notebookModel) descendants(id ID) []ID {
	out := []ID{id}
	for at := 0; at < len(out); at++ {
		n := m.Items[out[at]]
		out = append(out, m.Order[modelPlace{Kind: n.Ref.ID.Kind(), Page: n.Page, Parent: n.Ref.ID}]...)
	}
	return out
}
func (m *notebookModel) detach(id ID) {
	n := m.Items[id]
	p := n.location()
	m.Order[p] = slices.DeleteFunc(m.Order[p], func(other ID) bool { return other == id })
}
func (m *notebookModel) insert(ids []ID, p modelPlace, before, after *Target) string {
	list := m.Order[p]
	at := len(list)
	anchor := before
	if after != nil {
		anchor = after
	}
	if anchor != nil {
		at = slices.Index(list, anchor.Ref.ID)
		if at < 0 {
			return "refused:structure"
		}
		if after != nil {
			at++
		}
	}
	m.Order[p] = slices.Insert(list, at, ids...)
	for _, id := range ids {
		m.Items[id].Parent, m.Items[id].Page = p.Parent, p.Page
	}
	return ""
}
func (m *notebookModel) destination(p Placement) (modelPlace, string) {
	loc := modelPlace{Kind: BlockKind, Page: p.Page.Ref.ID, Parent: modelID(p.Parent)}
	if loc.Parent != "" {
		n := m.Items[loc.Parent]
		if n.Page != loc.Page || !modelHolds(n.Content.Kind) {
			return loc, "refused:structure"
		}
	}
	return loc, ""
}
func (m *notebookModel) roots(targets []Target) []ID {
	selected := map[ID]bool{}
	for _, t := range targets {
		selected[t.Ref.ID] = true
	}
	var roots []ID
	var walk func(modelPlace, bool)
	walk = func(p modelPlace, covered bool) {
		for _, id := range m.Order[p] {
			chosen := selected[id]
			if chosen && !covered {
				roots = append(roots, id)
			}
			n := m.Items[id]
			walk(modelPlace{Kind: n.Ref.ID.Kind(), Page: n.Page, Parent: id}, covered || chosen)
		}
	}
	var walkPages func(ID)
	walkPages = func(parent ID) {
		for _, page := range m.Order[modelPlace{Kind: PageKind, Parent: parent}] {
			walk(modelPlace{Kind: BlockKind, Page: page}, false)
			walkPages(page)
		}
	}
	walkPages("")
	return roots
}

func (m *notebookModel) valid() string {
	pages := 0
	seen := map[ID]int{}
	for p, ids := range m.Order {
		for _, id := range ids {
			n := m.Items[id]
			if n == nil || !n.Alive || n.location() != p {
				return "refused:structure"
			}
			seen[id]++
		}
	}
	for id, n := range m.Items {
		if !n.Alive {
			continue
		}
		if seen[id] != 1 {
			return "refused:structure"
		}
		limit := 8
		if id.Kind() == PageKind {
			pages++
		} else {
			limit = 6
			p := m.Items[n.Page]
			if p == nil || !p.Alive || n.Page.Kind() != PageKind {
				return "refused:structure"
			}
		}
		path := map[ID]bool{id: true}
		depth := 1
		for parent := n.Parent; parent != ""; {
			p := m.Items[parent]
			if p == nil || !p.Alive || parent.Kind() != id.Kind() || path[parent] {
				return "refused:structure"
			}
			if id.Kind() == BlockKind && (p.Page != n.Page || !modelHolds(p.Content.Kind)) {
				return "refused:structure"
			}
			path[parent] = true
			depth++
			if depth > limit {
				return "refused:structure"
			}
			parent = p.Parent
		}
	}
	if pages == 0 {
		return "refused:last_page"
	}
	return ""
}

func (m *notebookModel) content(n *modelItem, c Content, base *Version, seq Sequence, r *editResult) string {
	if !modelConversion(n.Content.Kind, c.Kind) {
		return "ignored:kind"
	}
	if base != nil && *base != n.version(ContentGroup) {
		r.Overwrote = append(r.Overwrote, overwrite{Block: n.Ref, Version: n.version(ContentGroup), Base: *base})
	}
	n.Content = c
	m.assign(n, ContentGroup, seq)
	if !modelHolds(c.Kind) {
		children := slices.Clone(m.Order[modelPlace{Kind: BlockKind, Page: n.Page, Parent: n.Ref.ID}])
		for _, id := range children {
			m.detach(id)
		}
		if len(children) > 0 {
			anchor := Target{Ref: &n.Ref}
			if failure := m.insert(children, n.location(), nil, &anchor); failure != "" {
				return failure
			}
			for _, id := range children {
				m.assign(m.Items[id], PlaceGroup, seq)
			}
		}
	}
	return ""
}

func (m *notebookModel) checkStructure(checks []StructureCheck) string {
	for _, s := range checks {
		for _, target := range []*Target{&s.Block, &s.Page, s.Parent} {
			if target != nil && !m.live(*target) {
				return "ignored:gone"
			}
		}
		n := m.Items[s.Block.Ref.ID]
		if n.Page != s.Page.Ref.ID || n.Parent != modelID(s.Parent) {
			return "refused:structure"
		}
		if s.After != nil {
			if !m.live(*s.After) {
				return "ignored:gone"
			}
			list := m.Order[n.location()]
			i := slices.Index(list, n.Ref.ID)
			if i < 1 || list[i-1] != s.After.Ref.ID {
				return "refused:structure"
			}
		}
	}
	return ""
}

func (m *notebookModel) live(target Target) bool {
	n := m.Items[target.Ref.ID]
	return n != nil && n.Alive && n.Ref == *target.Ref
}

func (m *notebookModel) apply(op Operation, seq Sequence, result *editResult) string {
	for _, target := range modelTargets(op) {
		if !m.live(target) {
			return "ignored:gone"
		}
	}
	var created *modelItem
	var fields CreateFields
	switch op := op.(type) {
	case *CreatePageOp:
		fields = op.CreateFields
		created = &modelItem{Ref: Ref{ID: *fields.ProposedID, Inc: Incarnation(seq)}, Alive: true, Title: op.Title, Parent: modelID(op.Parent)}
	case *CreateBlockOp:
		loc, failure := m.destination(op.Place)
		if failure != "" {
			return failure
		}
		fields = op.CreateFields
		created = &modelItem{Ref: Ref{ID: *fields.ProposedID, Inc: Incarnation(seq)}, Alive: true, Content: op.Content, Page: loc.Page, Parent: loc.Parent}
	case *SetPageTitleOp:
		n := m.Items[op.Page.Ref.ID]
		n.Title = op.Title
		m.assign(n, TitleGroup, seq)
	case *MovePageOp:
		n := m.Items[op.Page.Ref.ID]
		m.detach(n.Ref.ID)
		if f := m.insert([]ID{n.Ref.ID}, modelPlace{Kind: PageKind, Parent: modelID(op.Parent)}, op.Before, nil); f != "" {
			return f
		}
		m.assign(n, PlaceGroup, seq)
	case *SetBlockOp:
		if f := m.content(m.Items[op.Block.Ref.ID], op.Content, op.BaseV, seq, result); f != "" {
			return f
		}
	case *TurnBlocksOp:
		for _, target := range op.Blocks {
			if !modelConversion(m.Items[target.Ref.ID].Content.Kind, op.Kind) {
				return "refused:kind"
			}
		}
		for _, target := range op.Blocks {
			n := m.Items[target.Ref.ID]
			if f := m.content(n, modelTurn(n.Content, op.Kind), nil, seq, result); f != "" {
				return f
			}
		}
	case *MoveBlocksOp:
		loc, f := m.destination(op.Place)
		if f != "" {
			return f
		}
		roots := m.roots(op.Blocks)
		var moving []ID
		for _, id := range roots {
			moving = append(moving, m.descendants(id)...)
		}
		if slices.Contains(moving, loc.Parent) {
			return "refused:structure"
		}
		oldPages := map[ID]ID{}
		for _, id := range moving {
			oldPages[id] = m.Items[id].Page
		}
		for _, id := range roots {
			m.detach(id)
		}
		if f := m.insert(roots, loc, op.Place.Before, op.Place.After); f != "" {
			return f
		}
		// Descendant adjacency lists move with their owner, including empty lists.
		for _, id := range moving {
			n := m.Items[id]
			old := oldPages[id]
			if old != loc.Page {
				key := modelPlace{Kind: BlockKind, Page: old, Parent: id}
				m.Order[modelPlace{Kind: BlockKind, Page: loc.Page, Parent: id}] = m.Order[key]
				delete(m.Order, key)
				n.Page = loc.Page
				m.Items[old].Aggregate = Version(seq)
			}
			if old != loc.Page || slices.Contains(roots, id) {
				m.assign(n, PlaceGroup, seq)
			}
		}
	case *DeletePageOp, *DeleteBlocksOp:
		var deleting []ID
		if p, ok := op.(*DeletePageOp); ok {
			deleting = m.descendants(p.Page.Ref.ID)
			for _, page := range slices.Clone(deleting) {
				for _, root := range m.Order[modelPlace{Kind: BlockKind, Page: page}] {
					deleting = append(deleting, m.descendants(root)...)
				}
			}
		} else {
			for _, id := range m.roots(op.(*DeleteBlocksOp).Blocks) {
				deleting = append(deleting, m.descendants(id)...)
			}
		}
		for _, id := range deleting {
			n := m.Items[id]
			m.detach(id)
			n.Alive, n.Deleted = false, seq
			if n.Page != "" {
				m.Items[n.Page].Aggregate = Version(seq)
			}
		}
	}
	if created != nil {
		id := created.Ref.ID
		if _, exists := m.Items[id]; exists {
			return "refused:allocated_id"
		}
		created.Versions, created.Aggregate = map[GroupKey]Version{}, Version(seq)
		m.Items[id] = created
		var before, after *Target
		switch op := op.(type) {
		case *CreatePageOp:
			before = op.Before
		case *CreateBlockOp:
			before, after = op.Place.Before, op.Place.After
		}
		if f := m.insert([]ID{id}, created.location(), before, after); f != "" {
			return f
		}
		if created.Page != "" {
			m.Items[created.Page].Aggregate = Version(seq)
		}
		result.Refs[fields.Ref] = created.Ref
	}
	return m.valid()
}

func (m *notebookModel) action(opts editOptions, ops []Operation, seq Sequence, history *modelHistory) (editResult, string) {
	result := editResult{Results: []operationResult{}, Refs: map[string]Ref{}}
	if opts.Guards != nil {
		for _, g := range opts.Guards.Items {
			n := m.Items[g.Ref.ID]
			if n == nil || !n.Alive || n.Ref != g.Ref || n.ver() != g.Ver {
				return result, "refused:guard"
			}
		}
	}
	whole := m.clone()
	var transitions []modelTransition
	for start := 0; start < len(ops); {
		end := start + 1
		unit := ops[start].operationMeta().UnitID
		for unit != nil && end < len(ops) && ops[end].operationMeta().UnitID != nil && *ops[end].operationMeta().UnitID == *unit {
			end++
		}
		candidate := m.clone()
		staged := editResult{Refs: maps.Clone(result.Refs), Overwrote: slices.Clone(result.Overwrote)}
		failure, failed := m.checkStructure(ops[start].operationMeta().Structure), start
		for i := start; i < end && failure == ""; i++ {
			if failure = candidate.apply(ops[i], seq, &staged); failure != "" {
				failed = i
				break
			}
		}
		if failure != "" && !opts.Browser {
			*m = *whole
			return editResult{}, failure
		}
		if failure == "" {
			transitions = append(transitions, modelTransitionFor(m, candidate, ops[start:end]))
			*m = *candidate
			result.Refs, result.Overwrote = staged.Refs, staged.Overwrote
		}
		for i := start; i < end; i++ {
			r := operationResult{Index: i, UnitID: unit, Status: "applied"}
			if failure != "" {
				parts := strings.SplitN(failure, ":", 2)
				r.Status, r.Reason = parts[0], parts[1]
				if unit != nil {
					f := failed
					r.FailedIndex = &f
				}
			}
			result.Results = append(result.Results, r)
		}
		start = end
	}
	for _, transition := range transitions {
		history.record(opts.Session, transition)
	}
	return result, ""
}

func (m *notebookModel) ids(kind Kind, live bool) []ID {
	var ids []ID
	for id, n := range m.Items {
		if id.Kind() == kind && (!live || n.Alive) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

type modelLogical struct {
	Ref            Ref
	Alive          bool
	Deleted        Sequence
	Page, Parent   ID `json:",omitempty"`
	Rank           int
	Title          string
	Content        *Content
	Versions       map[GroupKey]Version
	Aggregate, Ver Version
}

func (m *notebookModel) logical() []modelLogical {
	ids := append(m.ids(PageKind, false), m.ids(BlockKind, false)...)
	out := make([]modelLogical, 0, len(ids))
	for _, id := range ids {
		n := m.Items[id]
		r := modelLogical{Ref: n.Ref, Alive: n.Alive, Deleted: n.Deleted, Page: n.Page, Parent: n.Parent, Rank: -1, Title: n.Title, Aggregate: n.Aggregate, Ver: n.ver(), Versions: map[GroupKey]Version{PlaceGroup: n.version(PlaceGroup)}}
		if id.Kind() == PageKind {
			r.Versions[TitleGroup] = n.version(TitleGroup)
		} else {
			c := n.Content
			r.Content = &c
			r.Versions[ContentGroup] = n.version(ContentGroup)
		}
		if n.Alive {
			r.Rank = slices.Index(m.Order[n.location()], id)
		}
		out = append(out, r)
	}
	return out
}

func readNotebookModel(t *testing.T, f *undoTestStore) *notebookModel {
	t.Helper()
	m := &notebookModel{Items: map[ID]*modelItem{}, Order: map[modelPlace][]ID{}}
	positions := map[ID]string{}
	err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), `SELECT id FROM items ORDER BY id`)
		if err != nil {
			return err
		}
		var ids []ID
		for rows.Next() {
			var id ID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, id := range ids {
			r, err := loadRecord(t.Context(), tx, id)
			if err != nil {
				return err
			}
			n := &modelItem{Ref: r.Item.Ref, Alive: r.Alive, Deleted: r.DeletedAt, Aggregate: r.Aggregate, Versions: r.Item.V}
			switch d := r.Item.Data.(type) {
			case Page:
				n.Title = d.Title
				if d.Parent != nil {
					n.Parent = *d.Parent
				}
				positions[id] = d.Position
			case Block:
				n.Page, n.Content = d.Page, d.Content
				if d.Parent != nil {
					n.Parent = *d.Parent
				}
				positions[id] = d.Position
			default:
				return fmt.Errorf("unexpected model item %s", id)
			}
			if n.ver() != r.Item.Ver {
				return fmt.Errorf("stored version for %s: %d != %d", id, r.Item.Ver, n.ver())
			}
			m.Items[id] = n
			if n.Alive {
				p := n.location()
				m.Order[p] = append(m.Order[p], id)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for p, ids := range m.Order {
		slices.SortFunc(ids, func(a, b ID) int {
			if c := strings.Compare(positions[a], positions[b]); c != 0 {
				return c
			}
			return strings.Compare(string(a), string(b))
		})
		m.Order[p] = ids
	}
	return m
}

type modelRun struct {
	t       *testing.T
	f       *undoTestStore
	m       *notebookModel
	history *modelHistory
	steps   []modelStep
	fault   func(modelStep, *modelRun)
}

type modelStep struct {
	Kind    string
	Options editOptions
	Ops     []Operation
	Seq     Sequence
}

func (s modelStep) String() string {
	if s.Kind != "edit" {
		return fmt.Sprintf("seq=%d %s session=%q", s.Seq, s.Kind, s.Options.Session)
	}
	raw, err := encodeOperationList(s.Ops, s.Options.Browser)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("seq=%d edit session=%q browser=%t guards=%+v ops=%s", s.Seq, s.Options.Session, s.Options.Browser, s.Options.Guards, raw)
}

func reduceModelSteps(input []modelStep, diverges func([]modelStep) bool) []modelStep {
	result := slices.Clone(input)
	for changed := true; changed; {
		changed = false
		for i := 0; i < len(result); {
			candidate := append(slices.Clone(result[:i]), result[i+1:]...)
			if diverges(candidate) {
				result = candidate
				changed = true
			} else {
				i++
			}
		}
	}
	return result
}

func replayModelSteps(t *testing.T, steps []modelStep, faults ...func(modelStep, *modelRun)) bool {
	t.Helper()
	r := newModelRun(t)
	defer func() {
		if err := r.f.s.close(); err != nil {
			t.Error(err)
		}
	}()
	for _, step := range steps {
		if mismatch := r.executeStep(step); mismatch != "" {
			return true
		}
		for _, fault := range faults {
			fault(step, r)
		}
		if r.mismatch() != "" {
			return true
		}
	}
	return false
}

func TestModelSequenceReducer(t *testing.T) {
	t.Parallel()
	reduced := reduceModelSteps([]modelStep{{Seq: 1}, {Seq: 2}, {Seq: 3}}, func(steps []modelStep) bool {
		return slices.ContainsFunc(steps, func(step modelStep) bool { return step.Seq == 2 })
	})
	if len(reduced) != 1 || reduced[0].Seq != 2 {
		t.Fatalf("not minimal: %+v", reduced)
	}
	if replayModelSteps(t, nil) {
		t.Fatal("empty replay diverges")
	}
}

func newModelRun(t *testing.T) *modelRun {
	t.Helper()
	f := newUndoTestStore(t)
	if _, err := f.s.db.ExecContext(t.Context(), `INSERT INTO sessions(token_hash,editor,label,verified,max_n,last_at) VALUES('d','d','d',0,'0',0),('setup','setup','setup',0,'0',0)`); err != nil {
		t.Fatal(err)
	}
	r := &modelRun{t: t, f: f, m: newNotebookModel(), history: newModelHistory()}
	r.check()
	return r
}
func formatModelSteps(steps []modelStep) string {
	var lines []string
	for _, step := range steps {
		lines = append(lines, step.String())
	}
	return strings.Join(lines, "\n")
}
func (r *modelRun) reducedFailure() ([]modelStep, string) {
	diverges := func(steps []modelStep) bool {
		if r.fault != nil {
			return replayModelSteps(r.t, steps, r.fault)
		}
		return replayModelSteps(r.t, steps)
	}
	if !diverges(r.steps) {
		return nil, ""
	}
	reduced := reduceModelSteps(r.steps, diverges)
	return reduced, fmt.Sprintf("\n1-minimal divergent sequence (%d events):\n%s", len(reduced), formatModelSteps(reduced))
}
func (r *modelRun) fail(format string, args ...any) {
	r.t.Helper()
	_, suffix := r.reducedFailure()
	r.t.Fatalf("%s\nfirst divergent prefix (%d events):\n%s%s", fmt.Sprintf(format, args...), len(r.steps), formatModelSteps(r.steps), suffix)
}
func (r *modelRun) mismatch() string {
	r.t.Helper()
	got := readNotebookModel(r.t, r.f)
	if failure := got.valid(); failure != "" {
		return "SQL tree invariant: " + failure
	}
	if failure := r.m.valid(); failure != "" {
		return "oracle invariant: " + failure
	}
	want, actual := r.m.logical(), got.logical()
	if !reflect.DeepEqual(want, actual) {
		w, err := json.MarshalIndent(want, "", "  ")
		if err != nil {
			r.t.Fatal(err)
		}
		a, err := json.MarshalIndent(actual, "", "  ")
		if err != nil {
			r.t.Fatal(err)
		}
		return fmt.Sprintf("logical state mismatch\nwant=%s\ngot=%s", w, a)
	}
	return ""
}
func (r *modelRun) check() {
	r.t.Helper()
	if mismatch := r.mismatch(); mismatch != "" {
		r.fail("%s", mismatch)
	}
}
func (r *modelRun) executeStep(step modelStep) string {
	r.t.Helper()
	if step.Kind == "reopen" {
		path := r.f.s.path
		if err := r.f.s.close(); err != nil {
			r.t.Fatal(err)
		}
		s, _, err := openStore(path)
		if err != nil {
			r.t.Fatal(err)
		}
		r.f.s = s
		return ""
	}
	r.f.seq = step.Seq - 1
	if step.Kind != "edit" {
		want := r.history.control(r.m, step.Options.Session, step.Kind == "redo", step.Seq)
		got := r.f.undo(step.Options.Session, step.Kind == "redo")
		if modelCounts(got) != want || got.Capacity != 0 || got.Evicted {
			return fmt.Sprintf("%s got=%+v want=%+v", step.Kind, got, want)
		}
		return ""
	}
	want, failure := r.m.action(step.Options, step.Ops, step.Seq, r.history)
	got, err := r.f.tryExecute(step.Options, step.Ops...)
	if failure != "" {
		var problem *editFailure
		if !errors.As(err, &problem) || problem.Status+":"+problem.Reason != failure {
			return fmt.Sprintf("call error=%v want=%s", err, failure)
		}
	} else if err != nil {
		return fmt.Sprintf("unexpected call error: %v", err)
	} else if !reflect.DeepEqual(got.Results, want.Results) || !maps.Equal(got.Refs, want.Refs) || !slices.Equal(got.Overwrote, want.Overwrote) || len(got.Copies) != 0 {
		return fmt.Sprintf("outcomes got=%+v want=%+v", got, want)
	}
	return ""
}
func (r *modelRun) step(step modelStep) {
	r.t.Helper()
	r.steps = append(r.steps, step)
	if mismatch := r.executeStep(step); mismatch != "" {
		r.fail("%s", mismatch)
	}
	if r.fault != nil {
		r.fault(step, r)
	}
	r.check()
}
func (r *modelRun) edit(opts editOptions, ops ...Operation) {
	r.t.Helper()
	if _, err := encodeOperationList(ops, opts.Browser); err != nil {
		r.t.Fatal(err)
	}
	r.step(modelStep{Kind: "edit", Options: opts, Ops: ops, Seq: r.f.seq + 1})
}
func (r *modelRun) reopen() {
	r.t.Helper()
	r.step(modelStep{Kind: "reopen", Seq: r.f.seq})
}
func (r *modelRun) control(session, kind string) {
	r.t.Helper()
	r.step(modelStep{Kind: kind, Options: editOptions{Session: session}, Seq: r.f.seq + 1})
}
func (r *modelRun) browser(session string, ops ...Operation) {
	r.t.Helper()
	r.edit(editOptions{Browser: true, Session: session}, ops...)
}
func (r *modelRun) target(id ID) Target { ref := r.m.Items[id].Ref; return Target{Ref: &ref} }
func modelPointer(t Target) *Target     { return &t }
func modelContent(k ContentKind, text string, variant int) Content {
	c := Content{Kind: k}
	switch {
	case modelText(k):
		marks := []Mark{}
		if variant%2 == 1 {
			marks = []Mark{Bold}
		}
		c.Text = RichText{{Text: text, Marks: marks}}
		if k == Todo {
			c.Checked = variant%2 == 1
		}
		if k == Toggle {
			c.OpenDefault = variant%2 == 1
		}
		if k == Callout {
			c.Tone = []Tone{NeutralTone, WarningTone}[variant%2]
		}
	case k == Code:
		c.Code, c.Language = text, []string{"", "go"}[variant%2]
	case k == PageLink:
		c.Target = vocabID(PageKind, byte(1+variant%2))
	}
	return c
}

func TestEditReferenceModel(t *testing.T) {
	t.Parallel()
	if *notebookModelSeeds < 1 {
		t.Fatal("-notebook-model-seeds must be positive")
	}
	for seed := 0; seed < *notebookModelSeeds; seed++ {
		t.Run(fmt.Sprintf("seed=%03d", seed), func(t *testing.T) {
			t.Parallel()
			r := newModelRun(t)
			rng := rand.New(rand.NewSource(int64(seed)))
			sessions := []string{"a", "b", "c", "d"}[:2+seed%3]
			nextPage, nextBlock := byte(3), byte(1)
			for i, k := range []ContentKind{Toggle, Todo, Paragraph, PageLink} {
				id := vocabID(BlockKind, nextBlock)
				nextBlock++
				place := Placement{Page: r.target(vocabID(PageKind, 1))}
				if i == 1 || i == 2 {
					place.Parent = modelPointer(r.target(vocabID(BlockKind, byte(i))))
				}
				r.browser("a", &CreateBlockOp{CreateFields: CreateFields{Ref: fmt.Sprint(id), ProposedID: &id}, Place: place, Content: modelContent(k, "initial", i)})
			}
			for step := 0; step < 32; step++ {
				session := sessions[rng.Intn(len(sessions))]
				strict := rng.Intn(5) == 0
				count := 1 + rng.Intn(3)
				var ops []Operation
				unit := fmt.Sprintf("unit-%d", step)
				compound := !strict && rng.Intn(3) == 0
				for j := 0; j < count; j++ {
					meta := OperationMeta{}
					if compound && j < 2 {
						meta.UnitID = &unit
					}
					pages := r.m.ids(PageKind, true)
					allPages := r.m.ids(PageKind, false)
					blocks := r.m.ids(BlockKind, false)
					liveBlocks := r.m.ids(BlockKind, true)
					if compound && j == 0 && len(liveBlocks) > 0 && rng.Intn(3) == 0 {
						n := r.m.Items[liveBlocks[rng.Intn(len(liveBlocks))]]
						check := StructureCheck{Block: r.target(n.Ref.ID), Page: r.target(n.Page)}
						if n.Parent != "" {
							check.Parent = modelPointer(r.target(n.Parent))
						}
						list := r.m.Order[n.location()]
						if at := slices.Index(list, n.Ref.ID); at > 0 && rng.Intn(2) == 0 {
							check.After = modelPointer(r.target(list[at-1]))
						}
						if rng.Intn(3) == 0 {
							check.Page = r.target(pages[rng.Intn(len(pages))])
						}
						meta.Structure = []StructureCheck{check}
					}
					pick := func(ids []ID) Target {
						target := r.target(ids[rng.Intn(len(ids))])
						if rng.Intn(8) == 0 {
							target.Ref.Inc += 999
						}
						return target
					}
					page := pick(pages)
					kind := rng.Intn(9)
					if len(blocks) == 0 {
						kind = 0
					}
					if strict && (kind == 0 || kind == 1) {
						kind = 2
					}
					var op Operation
					switch kind {
					case 0:
						id := vocabID(BlockKind, nextBlock)
						nextBlock++
						place := Placement{Page: page}
						if rng.Intn(3) == 0 && len(blocks) > 0 {
							place.Parent = modelPointer(pick(blocks))
						}
						if rng.Intn(4) == 0 && len(blocks) > 0 {
							place.After = modelPointer(pick(blocks))
						}
						ks := []ContentKind{Toggle, Bullet, Paragraph, Todo, Code, PageLink, Divider}
						op = &CreateBlockOp{OperationMeta: meta, CreateFields: CreateFields{Ref: fmt.Sprint(id), ProposedID: &id}, Place: place, Content: modelContent(ks[rng.Intn(len(ks))], fmt.Sprintf("s%d", rng.Intn(4)), rng.Intn(2))}
					case 1:
						id := vocabID(PageKind, nextPage)
						nextPage++
						var parent *Target
						if rng.Intn(2) == 0 {
							parent = modelPointer(page)
						}
						op = &CreatePageOp{OperationMeta: meta, CreateFields: CreateFields{Ref: fmt.Sprint(id), ProposedID: &id}, Parent: parent, Title: fmt.Sprintf("p%d", rng.Intn(4))}
					case 2:
						op = &SetPageTitleOp{OperationMeta: meta, Page: pick(allPages), Title: fmt.Sprintf("title%d", rng.Intn(3))}
					case 3:
						var parent, before *Target
						if rng.Intn(3) != 0 {
							parent = modelPointer(pick(allPages))
						}
						if rng.Intn(4) == 0 {
							before = modelPointer(pick(allPages))
						}
						op = &MovePageOp{OperationMeta: meta, Page: pick(allPages), Parent: parent, Before: before}
					case 4:
						ks := []ContentKind{Paragraph, Heading1, Toggle, Todo, Callout, Code, PageLink, Divider}
						base := Version(1)
						op = &SetBlockOp{OperationMeta: meta, Block: pick(blocks), BaseV: &base, Content: modelContent(ks[rng.Intn(len(ks))], fmt.Sprintf("text%d", rng.Intn(3)), rng.Intn(2))}
					case 5:
						place := Placement{Page: page}
						if rng.Intn(3) != 0 {
							place.Parent = modelPointer(pick(blocks))
						}
						if rng.Intn(3) == 0 {
							place.Before = modelPointer(pick(blocks))
						} else if rng.Intn(3) == 0 {
							place.After = modelPointer(pick(blocks))
						}
						selected := []Target{pick(blocks)}
						if rng.Intn(3) == 0 {
							other := pick(blocks)
							if other.Ref.ID != selected[0].Ref.ID {
								selected = append(selected, other)
							}
						}
						op = &MoveBlocksOp{OperationMeta: meta, Blocks: selected, Place: place}
					case 6:
						ks := []ContentKind{Paragraph, Toggle, Bullet, Code, Heading2}
						selected := []Target{pick(blocks)}
						if rng.Intn(2) == 0 {
							first := r.m.Items[selected[0].Ref.ID]
							if first.Alive && first.Parent != "" {
								selected = append(selected, r.target(first.Parent))
							} else if other := pick(blocks); other.Ref.ID != selected[0].Ref.ID {
								selected = append(selected, other)
							}
							if rng.Intn(2) == 0 {
								slices.Reverse(selected)
							}
						}
						op = &TurnBlocksOp{OperationMeta: meta, Blocks: selected, Kind: ks[rng.Intn(len(ks))]}
					case 7:
						op = &DeleteBlocksOp{OperationMeta: meta, Blocks: []Target{pick(blocks)}}
					case 8:
						op = &DeletePageOp{OperationMeta: meta, Page: pick(allPages)}
					}
					ops = append(ops, op)
				}
				opts := editOptions{Browser: !strict, Session: session}
				if strict && rng.Intn(3) == 0 {
					id := r.m.ids(PageKind, true)[0]
					n := r.m.Items[id]
					v := n.ver()
					if rng.Intn(2) == 0 {
						v++
					}
					opts.Guards = &Guards{Items: []ItemGuard{{Ref: n.Ref, Ver: v}}}
				}
				r.edit(opts, ops...)
				if step%8 == 7 {
					r.reopen()
				}
			}
			r.reopen()
		})
	}
}

func (r *modelRun) writeGroup(session string, id ID, value string) {
	r.t.Helper()
	n := r.m.Items[id]
	if id.Kind() == PageKind {
		r.browser(session, &SetPageTitleOp{Page: r.target(id), Title: value})
	} else {
		base := n.version(ContentGroup)
		r.browser(session, &SetBlockOp{Block: r.target(id), Content: modelContent(Paragraph, value, 0), BaseV: &base})
	}
}

func TestEditReferenceModelPersonalUndo(t *testing.T) {
	t.Parallel()
	for seed := 0; seed < 24; seed++ {
		t.Run(fmt.Sprintf("seed=%03d", seed), func(t *testing.T) {
			t.Parallel()
			r := newModelRun(t)
			page := vocabID(PageKind, 1)
			block := vocabID(BlockKind, 1)
			r.browser("setup", &CreateBlockOp{CreateFields: CreateFields{Ref: "block", ProposedID: &block}, Place: Placement{Page: r.target(page)}, Content: modelContent(Paragraph, "initial", 0)})
			r.writeGroup("a", page, "A")
			r.writeGroup("b", page, "B")
			r.writeGroup("a", page, "A")
			r.control("a", "undo")
			r.control("a", "undo")
			r.writeGroup("a", block, "same")
			r.writeGroup("b", block, "same")
			r.control("a", "undo")
			rng := rand.New(rand.NewSource(int64(seed)))
			sessions := []string{"a", "b", "c", "d"}[:2+seed%3]
			for step := 0; step < 32; step++ {
				session := sessions[rng.Intn(len(sessions))]
				if rng.Intn(3) == 0 {
					r.control(session, []string{"undo", "redo"}[rng.Intn(2)])
				} else {
					id := page
					if rng.Intn(2) == 0 {
						id = block
					}
					r.writeGroup(session, id, fmt.Sprintf("value%d", rng.Intn(3)))
				}
				if step%8 == 7 {
					r.reopen()
				}
			}
		})
	}
}

func TestEditReferenceModelDepthAndRevival(t *testing.T) {
	t.Parallel()
	r := newModelRun(t)
	first, second := vocabID(PageKind, 1), vocabID(PageKind, 2)
	parent := first
	for i := byte(3); i <= 9; i++ {
		id := vocabID(PageKind, i)
		r.browser("a", &CreatePageOp{CreateFields: CreateFields{Ref: fmt.Sprint(id), ProposedID: &id}, Parent: modelPointer(r.target(parent)), Title: "nested"})
		parent = id
	}
	r.browser("b", &MovePageOp{Page: r.target(first), Parent: modelPointer(r.target(second))})
	r.browser("b", &MovePageOp{Page: r.target(first), Parent: modelPointer(r.target(parent))})
	var blockParent *Target
	for i := byte(1); i <= 6; i++ {
		id := vocabID(BlockKind, i)
		r.browser("a", &CreateBlockOp{CreateFields: CreateFields{Ref: fmt.Sprint(id), ProposedID: &id}, Place: Placement{Page: r.target(first), Parent: blockParent}, Content: modelContent(Toggle, "nested", 0)})
		blockParent = modelPointer(r.target(id))
	}
	root := vocabID(BlockKind, 1)
	r.browser("b", &MoveBlocksOp{Blocks: []Target{r.target(root)}, Place: Placement{Page: r.target(first), Parent: blockParent}})
	id := vocabID(BlockKind, 7)
	r.browser("b", &CreateBlockOp{CreateFields: CreateFields{Ref: "deep", ProposedID: &id}, Place: Placement{Page: r.target(first), Parent: blockParent}, Content: modelContent(Paragraph, "too deep", 0)})
	r.browser("b", &CreateBlockOp{CreateFields: CreateFields{Ref: "root", ProposedID: &id}, Place: Placement{Page: r.target(first)}, Content: modelContent(Toggle, "root", 0)})
	r.browser("b", &MoveBlocksOp{Blocks: []Target{r.target(root)}, Place: Placement{Page: r.target(first), Parent: modelPointer(r.target(id))}})
	r.browser("b", &MoveBlocksOp{Blocks: []Target{r.target(root), r.target(vocabID(BlockKind, 2))}, Place: Placement{Page: r.target(second)}})
	old := r.target(root)
	r.browser("c", &DeleteBlocksOp{Blocks: []Target{old}})
	r.control("c", "revive")
	base := Version(1)
	r.browser("b", &SetBlockOp{Block: old, Content: modelContent(Paragraph, "stale", 0), BaseV: &base}, &SetPageTitleOp{Page: r.target(second), Title: "independent"})
	unit := "atomic"
	r.browser("b",
		&SetPageTitleOp{OperationMeta: OperationMeta{UnitID: &unit}, Page: r.target(second), Title: "must rollback"},
		&SetBlockOp{OperationMeta: OperationMeta{UnitID: &unit}, Block: old, Content: modelContent(Paragraph, "stale", 0), BaseV: &base},
		&SetPageTitleOp{Page: r.target(second), Title: "independent"})
	r.edit(editOptions{}, &SetPageTitleOp{Page: r.target(second), Title: "must rollback"}, &DeleteBlocksOp{Blocks: []Target{old}})
	r.browser("a", &SetBlockOp{Block: r.target(root), Content: modelContent(Paragraph, "hoist", 1), BaseV: &base})
	r.browser("b", &TurnBlocksOp{Blocks: []Target{r.target(root)}, Kind: Toggle})
	stalePage := r.target(parent)
	r.browser("b", &DeletePageOp{Page: r.target(first)})
	r.browser("a", &SetPageTitleOp{Page: stalePage, Title: "deleted"}, &SetPageTitleOp{Page: r.target(second), Title: "survived"})
	r.browser("b", &DeletePageOp{Page: r.target(second)})
	r.reopen()
}
