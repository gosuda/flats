// Package engine is the board's in-memory editing engine: it applies
// people's batches, agent calls, rollbacks and replacements to the current
// state and reports the canonical delta and every changed row, so a store
// can persist them in board.db. It reads no clock and no randomness; times
// and new ids come from the caller.
package engine

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/gosuda/flats/internal/board/model"
)

// Row is an element row: content fields plus runtime fields (3.1).
type Row struct {
	El      model.Element
	Inc     int64
	V       map[string]int64
	Deleted bool
	DelSeq  int64
	// DelKey is the editor key that deleted the row (TECH public edit 3.2).
	DelKey  string
	PreRel  *PreRel
	EndInc  [2]int64      // connectors: the target incarnation of each attached end
	EndLast [2][2]float64 // deleted connectors: each end's world coordinate at deletion
}

func (r *Row) alive() bool { return r != nil && !r.Deleted }

func (r *Row) clone() *Row {
	c := *r
	c.El = r.El.Clone()
	c.V = maps.Clone(r.V)
	return &c
}

// ver is max(inc, v[*]), the version agent guards compare (8.1).
func (r *Row) ver() int64 {
	v := r.Inc
	for _, x := range r.V {
		v = max(v, x)
	}
	return v
}

// PreRel holds the relations of an element from before the batch that
// deleted it started (3.1).
type PreRel struct {
	Frame    string      `json:"frame,omitempty"`
	FrameInc int64       `json:"frame_inc,omitempty"`
	Group    string      `json:"group,omitempty"`
	Ends     []EndRel    `json:"ends,omitempty"`
	Attached []AttachRel `json:"attached,omitempty"`
	Members  []MemberRel `json:"members,omitempty"`
}

// EndRel is a connector end at batch start and its target's incarnation.
type EndRel struct {
	End model.End `json:"end"`
	Inc int64     `json:"inc,omitempty"`
}

// AttachRel is a connector end that was attached to the deleted element.
type AttachRel struct {
	Conn    string     `json:"conn"`
	ConnInc int64      `json:"conn_inc"`
	End     int        `json:"end"`
	At      [2]float64 `json:"at"`
}

// MemberRel is a member of a deleted frame.
type MemberRel struct {
	ID  string `json:"id"`
	Inc int64  `json:"inc"`
}

// Session is a board.db sessions row.
type Session struct {
	ID       string `json:"id"`
	Key      string `json:"key"`
	Label    string `json:"label"`
	Verified bool   `json:"verified"`
	MaxN     int64  `json:"max_n"`
	LastAt   int64  `json:"last_at"`
	// StartSeq is the board's seq when the session started; a guest may
	// revive only rows it deleted after it.
	StartSeq int64 `json:"start_seq"`
}

// Receipt is the recorded outcome of one session batch (4.8).
type Receipt struct {
	Session string          `json:"session"`
	N       int64           `json:"n"`
	Seq     int64           `json:"seq"`
	Results json.RawMessage `json:"results,omitempty"`
	Reject  *Reject         `json:"reject,omitempty"`
}

// Reject is a refusal of a whole batch.
type Reject struct {
	Code   string `json:"code"`
	Retry  bool   `json:"retry"`
	Detail string `json:"detail,omitempty"`
}

// Asset is a board.db assets row. Refs counts alive image elements that
// reference the asset and DeletedRefs deleted ones; both are computed by the
// engine from content.
type Asset struct {
	Hash        string `json:"hash"`
	Mime        string `json:"mime"`
	W           int    `json:"w"`
	H           int    `json:"h"`
	Bytes       int64  `json:"bytes"`
	Refs        int    `json:"refs"`
	DeletedRefs int    `json:"deleted_refs"`
	HeldUntil   int64  `json:"held_until"`
}

type createdKey struct{ session, id string }

type connEnd struct {
	conn string
	end  int
}

type stacks struct {
	undo, redo []*UndoEntry
	next       int64
}

func (st *stacks) clone() *stacks {
	return &stacks{undo: slices.Clone(st.undo), redo: slices.Clone(st.redo), next: st.next}
}

// State is the current state of one board.
type State struct {
	seq    int64
	resets []int64
	// resetAt holds when each reset happened (unix seconds), and
	// resetFloor the newest reset TrimResets dropped.
	resetAt    []int64
	resetFloor int64
	rows       map[string]*Row
	sessions   map[string]*Session
	created    map[createdKey]int64
	createdN   map[string]int // created rows per session
	receipts   map[ReceiptKey]*Receipt
	undo       map[string]*stacks
	assets     map[string]*Asset
	limits     model.Limits

	// Indexes over alive rows.
	members  map[string]map[string]struct{}
	groups   map[string]map[string]struct{}
	attached map[string]map[connEnd]struct{}
	buckets  map[int]map[string]struct{}
	alive    int
	live     int // assets referenced by alive elements

	sps   []*savepoint
	quiet bool // set while a rollback restores rows, so nothing is journaled
}

// New returns the state of a new, empty board.
func New(title string) *State {
	s := newState()
	s.seq = 1
	s.rows[model.BoardID] = &Row{
		El:  model.Element{ID: model.BoardID, Type: model.TypeBoard, Text: title, Grid: true},
		Inc: 1,
		V:   map[string]int64{model.BundleTitle: 1, model.BundleGrid: 1},
	}
	return s
}

func newState() *State {
	return &State{
		rows:     map[string]*Row{},
		sessions: map[string]*Session{},
		created:  map[createdKey]int64{},
		createdN: map[string]int{},
		receipts: map[ReceiptKey]*Receipt{},
		undo:     map[string]*stacks{},
		assets:   map[string]*Asset{},
		limits:   model.DefaultLimits,
		members:  map[string]map[string]struct{}{},
		groups:   map[string]map[string]struct{}{},
		attached: map[string]map[connEnd]struct{}{},
		buckets:  map[int]map[string]struct{}{},
	}
}

// SetLimits replaces the per-item limits.
func (s *State) SetLimits(l model.Limits) { s.limits = l }

// Limits returns the per-item limits.
func (s *State) Limits() model.Limits { return s.limits }

// Counts returns the number of alive elements and of the assets they
// reference.
func (s *State) Counts() (alive, assets int) { return s.alive, s.live }

// Seq is the seq of the last change.
func (s *State) Seq() int64 { return s.seq }

// Resets are the seqs of recent rollbacks and replacements (meta.resets).
func (s *State) Resets() []int64 { return slices.Clone(s.resets) }

// ResetTimes are when each of Resets happened, in unix seconds.
func (s *State) ResetTimes() []int64 { return slices.Clone(s.resetAt) }

// ResetFloor is the newest reset TrimResets dropped, 0 when none was: a
// client whose watermark is below it missed a rollback or replacement.
func (s *State) ResetFloor() int64 { return s.resetFloor }

// TrimResets drops the resets that happened before the unix time before,
// and then the oldest beyond keep, raising ResetFloor to the newest dropped.
func (s *State) TrimResets(before int64, keep int) Changed {
	m := s.Savepoint()
	sp := s.sps[m]
	drop := 0
	for drop < len(s.resets) && (s.resetAt[drop] < before || len(s.resets)-drop > keep) {
		drop++
	}
	if drop > 0 {
		s.resetFloor = max(s.resetFloor, s.resets[drop-1])
		s.resets, s.resetAt = slices.Clone(s.resets[drop:]), slices.Clone(s.resetAt[drop:])
	}
	c := s.changedSince(sp)
	s.Release(m)
	return c
}

// Alive returns the alive elements, without the metadata, sorted by id.
func (s *State) Alive() []model.PrivateElement {
	var out []model.PrivateElement
	for _, id := range slices.Sorted(maps.Keys(s.rows)) {
		if r := s.rows[id]; r.alive() && id != model.BoardID {
			out = append(out, private(r))
		}
	}
	return out
}

// Board returns the metadata pseudo-element.
func (s *State) Board() model.PrivateElement { return private(s.rows[model.BoardID]) }

// Get returns the alive element id.
func (s *State) Get(id string) (model.PrivateElement, bool) {
	r := s.rows[id]
	if !r.alive() {
		return model.PrivateElement{}, false
	}
	return private(r), true
}

// Ver is the agent guard version of the alive element id.
func (s *State) Ver(id string) (int64, bool) {
	r := s.rows[id]
	if !r.alive() {
		return 0, false
	}
	return r.ver(), true
}

func private(r *Row) model.PrivateElement {
	return model.PrivateElement{Element: r.El.Clone(), Inc: r.Inc, V: maps.Clone(r.V), EndInc: r.EndInc}
}

// Undo and redo stack names.
const (
	StackUndo = "undo"
	StackRedo = "redo"
)

// ReceiptKey names a receipt.
type ReceiptKey struct {
	Session string
	N       int64
}

// CreatedKey names a created row.
type CreatedKey struct{ Session, ID string }

// UndoKey names an undo row.
type UndoKey struct {
	Session string
	Stack   string
	Idx     int64
}

// Changed lists the rows one apply changed. The store reads each named row
// back from the State (Element, SessionRow, ...); a row that no longer
// exists is deleted.
type Changed struct {
	Elements []string
	Buckets  []int // model.BoardBucket for board.json
	Created  []CreatedKey
	Undo     []UndoKey
	Sessions []string
	Receipts []ReceiptKey
	Assets   []string
	Meta     bool // seq or resets
}

// Empty reports whether nothing changed.
func (c Changed) Empty() bool {
	return len(c.Elements) == 0 && len(c.Created) == 0 && len(c.Undo) == 0 && len(c.Sessions) == 0 &&
		len(c.Receipts) == 0 && len(c.Assets) == 0 && !c.Meta
}

// prior is the value a key had before a savepoint first changed it.
type prior[V any] struct {
	v  V
	ok bool
}

type savepoint struct {
	rows       map[string]prior[*Row]
	created    map[createdKey]prior[int64]
	sessions   map[string]prior[*Session]
	receipts   map[ReceiptKey]prior[*Receipt]
	undo       map[string]prior[*stacks]
	assets     map[string]prior[*Asset]
	seq        int64
	resets     []int64
	resetAt    []int64
	resetFloor int64
	live       int
	// aliveCount is the number of alive elements at savepoint start.
	aliveCount int

	// Relations of rows at savepoint start that the savepoint changed, so
	// batch-start relations (4.1) can be found without a full scan.
	startAttach  map[string][]connEnd
	startMembers map[string][]string
}

func newSavepoint(s *State) *savepoint {
	return &savepoint{
		rows:         map[string]prior[*Row]{},
		created:      map[createdKey]prior[int64]{},
		sessions:     map[string]prior[*Session]{},
		receipts:     map[ReceiptKey]prior[*Receipt]{},
		undo:         map[string]prior[*stacks]{},
		assets:       map[string]prior[*Asset]{},
		seq:          s.seq,
		resets:       slices.Clone(s.resets),
		resetAt:      slices.Clone(s.resetAt),
		resetFloor:   s.resetFloor,
		live:         s.live,
		aliveCount:   s.alive,
		startAttach:  map[string][]connEnd{},
		startMembers: map[string][]string{},
	}
}

func remember[K comparable, V any](log map[K]prior[V], m map[K]V, k K) {
	if _, done := log[k]; done {
		return
	}
	v, ok := m[k]
	log[k] = prior[V]{v, ok}
}

func restore[K comparable, V any](log map[K]prior[V], m map[K]V) {
	for k, p := range log {
		if p.ok {
			m[k] = p.v
		} else {
			delete(m, k)
		}
	}
}

func mergeInto[K comparable, V any](dst, src map[K]prior[V]) {
	for k, p := range src {
		if _, ok := dst[k]; !ok {
			dst[k] = p
		}
	}
}

// Mark identifies a savepoint.
type Mark int

// Savepoint starts a savepoint. RollbackTo undoes every change made after
// it; Release keeps them. Savepoints nest.
func (s *State) Savepoint() Mark {
	s.sps = append(s.sps, newSavepoint(s))
	return Mark(len(s.sps) - 1)
}

// RollbackTo undoes every change since m and ends m and its inner savepoints.
func (s *State) RollbackTo(m Mark) {
	for len(s.sps) > int(m) {
		sp := s.sps[len(s.sps)-1]
		s.sps = s.sps[:len(s.sps)-1]
		s.quiet = true
		for id, p := range sp.rows {
			s.setRow(id, p.v)
		}
		s.quiet = false
		for k, p := range sp.created {
			if _, ok := s.created[k]; ok != p.ok {
				s.countCreated(k.session, ok)
			}
		}
		restore(sp.created, s.created)
		restore(sp.sessions, s.sessions)
		restore(sp.receipts, s.receipts)
		restore(sp.undo, s.undo)
		restore(sp.assets, s.assets)
		s.seq, s.resets, s.resetAt, s.resetFloor, s.live = sp.seq, sp.resets, sp.resetAt, sp.resetFloor, sp.live
	}
}

// Release ends m and its inner savepoints, keeping their changes.
func (s *State) Release(m Mark) {
	for len(s.sps) > int(m) {
		s.popKeep()
	}
}

func (s *State) popKeep() {
	sp := s.sps[len(s.sps)-1]
	s.sps = s.sps[:len(s.sps)-1]
	if len(s.sps) == 0 {
		return
	}
	parent := s.sps[len(s.sps)-1]
	for id, p := range sp.rows {
		if _, ok := parent.rows[id]; !ok {
			parent.rows[id] = p
			parent.noteStart(p.v)
		}
	}
	mergeInto(parent.created, sp.created)
	mergeInto(parent.sessions, sp.sessions)
	mergeInto(parent.receipts, sp.receipts)
	mergeInto(parent.undo, sp.undo)
	mergeInto(parent.assets, sp.assets)
}

func (sp *savepoint) noteStart(r *Row) {
	if !r.alive() {
		return
	}
	if r.El.Frame != "" {
		sp.startMembers[r.El.Frame] = append(sp.startMembers[r.El.Frame], r.El.ID)
	}
	if r.El.Type == model.TypeConnector {
		for k, e := range r.El.Ends {
			if e.El != "" {
				sp.startAttach[e.El] = append(sp.startAttach[e.El], connEnd{r.El.ID, k})
			}
		}
	}
}

func (s *State) top() *savepoint {
	if len(s.sps) == 0 || s.quiet {
		return nil
	}
	return s.sps[len(s.sps)-1]
}

// putRow replaces row id, journaling the previous row and keeping indexes
// and asset counts current. Rows are never changed in place.
func (s *State) putRow(id string, r *Row) {
	if sp := s.top(); sp != nil {
		if _, done := sp.rows[id]; !done {
			old := s.rows[id]
			sp.rows[id] = prior[*Row]{old, old != nil}
			sp.noteStart(old)
		}
	}
	s.setRow(id, r)
}

// setRow replaces row id (nil removes it) and updates the indexes.
func (s *State) setRow(id string, r *Row) {
	old := s.rows[id]
	if old != nil {
		s.index(old, -1)
	}
	if r == nil {
		delete(s.rows, id)
		return
	}
	s.rows[id] = r
	s.index(r, 1)
}

func (s *State) index(r *Row, d int) {
	if r.El.Type == model.TypeImage {
		s.countAsset(r.El.Asset, r.Deleted, d)
	}
	if r.Deleted || r.El.Type == model.TypeBoard {
		return
	}
	s.alive += d
	id := r.El.ID
	setIndex(s.buckets, model.BucketOf(id), id, d)
	if r.El.Frame != "" {
		setIndex(s.members, r.El.Frame, id, d)
	}
	if r.El.Group != "" {
		setIndex(s.groups, r.El.Group, id, d)
	}
	if r.El.Type == model.TypeConnector {
		for k, e := range r.El.Ends {
			if e.El != "" {
				setIndex(s.attached, e.El, connEnd{id, k}, d)
			}
		}
	}
}

func setIndex[I, K comparable](m map[I]map[K]struct{}, key I, v K, d int) {
	if d > 0 {
		if m[key] == nil {
			m[key] = map[K]struct{}{}
		}
		m[key][v] = struct{}{}
		return
	}
	delete(m[key], v)
	if len(m[key]) == 0 {
		delete(m, key)
	}
}

func (s *State) countAsset(hash string, deleted bool, d int) {
	a := s.editAsset(hash)
	if deleted {
		a.DeletedRefs += d
		return
	}
	before := a.Refs
	a.Refs += d
	if before == 0 && a.Refs > 0 {
		s.live++
	} else if before > 0 && a.Refs == 0 {
		s.live--
	}
}

func (s *State) editAsset(hash string) *Asset {
	if sp := s.top(); sp != nil {
		remember(sp.assets, s.assets, hash)
	}
	a := &Asset{Hash: hash}
	if old := s.assets[hash]; old != nil {
		*a = *old
	}
	s.assets[hash] = a
	return a
}

func (s *State) setCreated(k createdKey, inc int64) {
	if sp := s.top(); sp != nil {
		remember(sp.created, s.created, k)
	}
	_, had := s.created[k]
	if inc == 0 {
		if had {
			s.countCreated(k.session, true)
		}
		delete(s.created, k)
		return
	}
	if !had {
		s.countCreated(k.session, false)
	}
	s.created[k] = inc
}

// countCreated adjusts the created rows of session for a row that existed
// (and is removed) or did not (and is added).
func (s *State) countCreated(session string, existed bool) {
	if existed {
		if s.createdN[session]--; s.createdN[session] <= 0 {
			delete(s.createdN, session)
		}
		return
	}
	s.createdN[session]++
}

// CreatedCount is the number of created rows of session.
func (s *State) CreatedCount(session string) int { return s.createdN[session] }

func (s *State) putSession(se *Session) {
	if sp := s.top(); sp != nil {
		remember(sp.sessions, s.sessions, se.ID)
	}
	s.sessions[se.ID] = se
}

func (s *State) deleteSession(id string) {
	if sp := s.top(); sp != nil {
		remember(sp.sessions, s.sessions, id)
	}
	delete(s.sessions, id)
}

func (s *State) putReceipt(r *Receipt) {
	k := ReceiptKey{r.Session, r.N}
	if sp := s.top(); sp != nil {
		remember(sp.receipts, s.receipts, k)
	}
	s.receipts[k] = r
}

func (s *State) deleteReceipt(k ReceiptKey) {
	if sp := s.top(); sp != nil {
		remember(sp.receipts, s.receipts, k)
	}
	delete(s.receipts, k)
}

// editStacks returns a private copy of a session's stacks to change.
func (s *State) editStacks(session string) *stacks {
	if sp := s.top(); sp != nil {
		remember(sp.undo, s.undo, session)
	}
	st := &stacks{next: 1}
	if old := s.undo[session]; old != nil {
		st = old.clone()
	}
	s.undo[session] = st
	return st
}

func (s *State) deleteStacks(session string) {
	if s.undo[session] == nil {
		return
	}
	if sp := s.top(); sp != nil {
		remember(sp.undo, s.undo, session)
	}
	delete(s.undo, session)
}

// changedSince lists the rows changed inside savepoint sp.
func (s *State) changedSince(sp *savepoint) Changed {
	var c Changed
	buckets := map[int]bool{}
	for _, id := range slices.Sorted(maps.Keys(sp.rows)) {
		old, cur := sp.rows[id].v, s.rows[id]
		if old == cur {
			continue
		}
		c.Elements = append(c.Elements, id)
		if contentMayDiffer(old, cur) {
			buckets[model.BucketOf(id)] = true
		}
	}
	c.Buckets = slices.Sorted(maps.Keys(buckets))
	for k, p := range sp.created {
		if v, ok := s.created[k]; ok != p.ok || v != p.v {
			c.Created = append(c.Created, CreatedKey{k.session, k.id})
		}
	}
	slices.SortFunc(c.Created, func(a, b CreatedKey) int {
		return cmp.Or(cmp.Compare(a.Session, b.Session), cmp.Compare(a.ID, b.ID))
	})
	for _, id := range slices.Sorted(maps.Keys(sp.sessions)) {
		if p := sp.sessions[id]; !p.ok || p.v != s.sessions[id] {
			c.Sessions = append(c.Sessions, id)
		}
	}
	for k, p := range sp.receipts {
		if !p.ok || p.v != s.receipts[k] {
			c.Receipts = append(c.Receipts, k)
		}
	}
	slices.SortFunc(c.Receipts, func(a, b ReceiptKey) int {
		return cmp.Or(cmp.Compare(a.Session, b.Session), cmp.Compare(a.N, b.N))
	})
	for _, session := range slices.Sorted(maps.Keys(sp.undo)) {
		c.Undo = append(c.Undo, diffStacks(session, sp.undo[session].v, s.undo[session])...)
	}
	for _, h := range slices.Sorted(maps.Keys(sp.assets)) {
		p := sp.assets[h]
		if cur := s.assets[h]; !p.ok || cur == nil || *p.v != *cur {
			c.Assets = append(c.Assets, h)
		}
	}
	c.Meta = sp.seq != s.seq || !slices.Equal(sp.resets, s.resets) || !slices.Equal(sp.resetAt, s.resetAt) || sp.resetFloor != s.resetFloor
	return c
}

func contentMayDiffer(old, cur *Row) bool {
	if old.alive() != cur.alive() {
		return true
	}
	if !cur.alive() {
		return false
	}
	return old.Inc != cur.Inc || old.El.Z != cur.El.Z || !maps.Equal(old.V, cur.V) || !sameUnversioned(old.El, cur.El) ||
		!model.SameAuthorship(old.El, cur.El)
}

func diffStacks(session string, old, cur *stacks) []UndoKey {
	var keys []UndoKey
	for _, name := range []string{StackUndo, StackRedo} {
		before := map[int64]*UndoEntry{}
		if old != nil {
			for _, e := range old.stack(name) {
				before[e.Idx] = e
			}
		}
		if cur != nil {
			for _, e := range cur.stack(name) {
				if before[e.Idx] != e {
					keys = append(keys, UndoKey{session, name, e.Idx})
				}
				delete(before, e.Idx)
			}
		}
		for _, idx := range slices.Sorted(maps.Keys(before)) {
			keys = append(keys, UndoKey{session, name, idx})
		}
	}
	slices.SortFunc(keys, func(a, b UndoKey) int {
		return cmp.Or(cmp.Compare(a.Stack, b.Stack), cmp.Compare(a.Idx, b.Idx))
	})
	return keys
}

func (st *stacks) stack(name string) []*UndoEntry {
	if name == StackRedo {
		return st.redo
	}
	return st.undo
}

// Rows is the persistent form of a State, one slice per board.db table.
type Rows struct {
	Seq        int64
	Resets     []int64
	ResetAt    []int64 // when each reset happened, unix seconds
	ResetFloor int64
	Elements   []ElementRow
	Sessions   []Session
	Created    []CreatedRow
	Receipts   []Receipt
	Undo       []UndoRow
	Assets     []Asset
}

// ElementRow is an elements row. Data is the canonical content JSON.
type ElementRow struct {
	ID      string
	Inc     int64
	Data    json.RawMessage
	V       map[string]int64
	Deleted bool
	DelSeq  int64
	DelKey  string
	PreRel  *PreRel
	EndInc  [2]int64
	EndLast [2][2]float64
	Bucket  int
}

// CreatedRow is a created row (4.1).
type CreatedRow struct {
	Session string
	ID      string
	Inc     int64
}

// UndoRow is an undo row (4.10).
type UndoRow struct {
	Session string
	Stack   string
	Idx     int64
	Entry   *UndoEntry
}

// Element returns element row id, including deleted rows.
func (s *State) Element(id string) (ElementRow, bool) {
	r := s.rows[id]
	if r == nil {
		return ElementRow{}, false
	}
	return ElementRow{
		ID: id, Inc: r.Inc, Data: r.El.CanonicalJSON(), V: maps.Clone(r.V),
		Deleted: r.Deleted, DelSeq: r.DelSeq, DelKey: r.DelKey, PreRel: r.PreRel,
		EndInc: r.EndInc, EndLast: r.EndLast, Bucket: model.BucketOf(id),
	}, true
}

// SessionRow returns session id.
func (s *State) SessionRow(id string) (Session, bool) {
	se := s.sessions[id]
	if se == nil {
		return Session{}, false
	}
	return *se, true
}

// CreatedInc returns the created row of k.
func (s *State) CreatedInc(k CreatedKey) (int64, bool) {
	inc, ok := s.created[createdKey{k.Session, k.ID}]
	return inc, ok
}

// ReceiptRow returns receipt k.
func (s *State) ReceiptRow(k ReceiptKey) (Receipt, bool) {
	r := s.receipts[k]
	if r == nil {
		return Receipt{}, false
	}
	return *r, true
}

// UndoRow returns undo row k.
func (s *State) UndoRow(k UndoKey) (UndoRow, bool) {
	if st := s.undo[k.Session]; st != nil {
		for _, e := range st.stack(k.Stack) {
			if e.Idx == k.Idx {
				return UndoRow{k.Session, k.Stack, k.Idx, e}, true
			}
		}
	}
	return UndoRow{}, false
}

// AssetRow returns asset row hash.
func (s *State) AssetRow(hash string) (Asset, bool) {
	a := s.assets[hash]
	if a == nil {
		return Asset{}, false
	}
	return *a, true
}

// BucketFile returns the canonical snapshot file of bucket b, or false when
// the bucket holds no alive element.
func (s *State) BucketFile(b int) ([]byte, bool) {
	if b == model.BoardBucket {
		return s.rows[model.BoardID].El.CanonicalJSON(), true
	}
	var els []model.Element
	for id := range s.buckets[b] {
		els = append(els, s.rows[id].El)
	}
	if len(els) == 0 {
		return nil, false
	}
	return model.BucketJSON(els), true
}

// Export returns every row of the state.
func (s *State) Export() Rows {
	out := Rows{Seq: s.seq, Resets: slices.Clone(s.resets), ResetAt: slices.Clone(s.resetAt), ResetFloor: s.resetFloor}
	for _, id := range slices.Sorted(maps.Keys(s.rows)) {
		r, _ := s.Element(id)
		out.Elements = append(out.Elements, r)
	}
	for _, id := range slices.Sorted(maps.Keys(s.sessions)) {
		out.Sessions = append(out.Sessions, *s.sessions[id])
	}
	for k, inc := range s.created {
		out.Created = append(out.Created, CreatedRow{k.session, k.id, inc})
	}
	slices.SortFunc(out.Created, func(a, b CreatedRow) int {
		return cmp.Or(cmp.Compare(a.Session, b.Session), cmp.Compare(a.ID, b.ID))
	})
	for _, r := range s.receipts {
		out.Receipts = append(out.Receipts, *r)
	}
	slices.SortFunc(out.Receipts, func(a, b Receipt) int {
		return cmp.Or(cmp.Compare(a.Session, b.Session), cmp.Compare(a.N, b.N))
	})
	for _, session := range slices.Sorted(maps.Keys(s.undo)) {
		st := s.undo[session]
		for _, name := range []string{StackUndo, StackRedo} {
			for _, e := range st.stack(name) {
				out.Undo = append(out.Undo, UndoRow{session, name, e.Idx, e})
			}
		}
	}
	for _, h := range slices.Sorted(maps.Keys(s.assets)) {
		out.Assets = append(out.Assets, *s.assets[h])
	}
	return out
}

// Load builds a State from stored rows. Undo rows are ordered by Idx within
// each stack.
func Load(rows Rows) (*State, error) {
	s := newState()
	s.seq, s.resets, s.resetFloor = rows.Seq, slices.Clone(rows.Resets), rows.ResetFloor
	if len(s.resets) > 0 {
		// A reset stored without its time counts as long ago.
		s.resetAt = make([]int64, len(s.resets))
		copy(s.resetAt, rows.ResetAt)
	}
	for _, a := range rows.Assets {
		a.Refs, a.DeletedRefs = 0, 0
		s.assets[a.Hash] = &a
	}
	for _, er := range rows.Elements {
		var el model.Element
		if er.ID == model.BoardID {
			var b struct {
				Title string `json:"title"`
				Grid  bool   `json:"grid"`
			}
			if err := json.Unmarshal(er.Data, &b); err != nil {
				return nil, fmt.Errorf("board metadata row: %w", err)
			}
			el = model.Element{ID: model.BoardID, Type: model.TypeBoard, Text: b.Title, Grid: b.Grid}
		} else if err := json.Unmarshal(er.Data, &el); err != nil {
			return nil, fmt.Errorf("element row %s: %w", er.ID, err)
		}
		if el.ID != er.ID {
			return nil, fmt.Errorf("element row %s holds element %s", er.ID, el.ID)
		}
		s.setRow(er.ID, &Row{
			El: el, Inc: er.Inc, V: maps.Clone(er.V), Deleted: er.Deleted, DelSeq: er.DelSeq, DelKey: er.DelKey,
			PreRel: er.PreRel, EndInc: er.EndInc, EndLast: er.EndLast,
		})
	}
	if s.rows[model.BoardID] == nil {
		return nil, fmt.Errorf("board metadata row is missing")
	}
	for _, se := range rows.Sessions {
		s.sessions[se.ID] = &se
	}
	for _, c := range rows.Created {
		k := createdKey{c.Session, c.ID}
		if _, dup := s.created[k]; !dup {
			s.createdN[c.Session]++
		}
		s.created[k] = c.Inc
	}
	for _, r := range rows.Receipts {
		s.receipts[ReceiptKey{r.Session, r.N}] = &r
	}
	sorted := slices.Clone(rows.Undo)
	slices.SortFunc(sorted, func(a, b UndoRow) int { return cmp.Compare(a.Idx, b.Idx) })
	for _, u := range sorted {
		st := s.undo[u.Session]
		if st == nil {
			st = &stacks{next: 1}
			s.undo[u.Session] = st
		}
		if u.Stack == StackRedo {
			st.redo = append(st.redo, u.Entry)
		} else {
			st.undo = append(st.undo, u.Entry)
		}
		st.next = max(st.next, u.Idx+1)
	}
	return s, nil
}
