package sheet

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// A format property is one value group of spec rule 28.
const (
	propBold = iota
	propItalic
	propUnderline
	propStrike
	propTextColor
	propFill
	propAlign
	propWrap
	propNumber
	propCount
)

var propNames = [propCount]string{"bold", "italic", "underline", "strikethrough", "text_color", "fill", "align", "wrap", "number"}

var alignNames = [...]string{"general", "left", "center", "right"}

// styleState is one interned pair of a format and the stamps of its
// properties (technical design, section 4.4).
type styleState struct {
	f      formula.Format
	stamps [propCount]uint64
}

type styleEntry struct {
	styleState
	refs int
}

// Style ids run from 1 to maxStyleID; 0 is the default format.
const maxStyleID = math.MaxUint32

// styleRefillLeft is how few ids may be left to give out before the tick
// looks for the ids that states gave back (styleTable.refill). It is far
// more than one tick's actions can take, and far fewer than the ids no
// state holds: a sheet holds at most maxCells states.
const styleRefillLeft = 1 << 30

// styleTable interns style states and counts the cells that use each. Id 0
// is the default format with no stamps and is never stored.
//
// An id names a state only while the table holds it. Ids that states gave
// back are given out again: a range format at a new sequence makes a new
// state for each distinct old one, so a table that gave every id out once
// would run out after some tens of thousands of them on a large range. The table gives out
// the ids above next first. Once few are left, the tick (and a load) makes
// free the runs of ids below next that no state holds. A state that left
// the table names no cell and no row of sheet.db that a later commit does
// not change: the commit that stages a touched id writes the state that
// holds it then, or deletes its row.
type styleTable struct {
	byID    map[uint32]*styleEntry
	ids     map[styleState]uint32
	touched map[uint32]struct{} // ids whose row in sheet.db is out of date
	// next is the lowest id above every id given out from it, up to
	// maxStyleID+1. free holds runs of ids below next that no state
	// holds, given out from the last run, and freeIDs counts them.
	next    uint64
	free    []styleRun
	freeIDs uint64
	// log holds the ids the table gave out since the last commit, in
	// order, for the revert of an action (forgetFrom).
	log []styleGiven
	// full is set when a state found no id. The action is refused, and the
	// revert of the operation that found no id clears it (forgetFrom).
	// fullAt is the place in log of the entry, without an id, that intern
	// adds then: the marks of later operations lie behind it, so their
	// revert keeps the failure of the earlier one.
	full   bool
	fullAt int
	// most is how many states the table held before it last lost one,
	// whose room its maps keep until the tick makes them again, and
	// touchedMost the same for touched.
	most, touchedMost int
	// used is the number of states that cells use. The others are left by
	// a change that took cells away from a state outside an action, such
	// as the tick's collection of blank records, and the tick forgets them
	// (dropUnused).
	used int
	// formatted is the number of states in use whose format is not the
	// default. Only a cell with a format holds one, so the cell limit
	// bounds it, whatever number of blank records keep states of the
	// default format for their stamps.
	formatted int
}

// styleRun is the free ids first to last.
type styleRun struct{ first, last uint32 }

// styleGiven is an id the table gave out, and whether it was touched
// before: an id the tick took a state off stays touched until a commit
// deletes its row.
type styleGiven struct {
	id      uint32
	touched bool
}

func newStyleTable() styleTable {
	return styleTable{byID: map[uint32]*styleEntry{}, ids: map[styleState]uint32{}, next: 1, touched: map[uint32]struct{}{}}
}

func (t *styleTable) intern(s styleState) uint32 {
	if s == (styleState{}) {
		return 0
	}
	if id, ok := t.ids[s]; ok {
		return id
	}
	id, ok := t.take()
	if !ok {
		// The cell gets the default format until the refused action is
		// reverted.
		if !t.full {
			t.full, t.fullAt = true, len(t.log)
			t.log = append(t.log, styleGiven{})
		}
		return 0
	}
	_, was := t.touched[id]
	t.log = append(t.log, styleGiven{id, was})
	t.byID[id] = &styleEntry{styleState: s}
	t.ids[s] = id
	t.touched[id] = struct{}{}
	return id
}

// take returns an id no state holds.
func (t *styleTable) take() (uint32, bool) {
	if n := len(t.free); n > 0 {
		r := &t.free[n-1]
		id := r.first
		if r.first == r.last {
			t.free = t.free[:n-1]
		} else {
			r.first++
		}
		t.freeIDs--
		return id, true
	}
	if t.next > maxStyleID {
		return 0, false
	}
	t.next++
	return uint32(t.next - 1), true
}

// giveBack makes an id that take gave out free again. Ids given back in
// the reverse order of take leave the table as it was before them.
func (t *styleTable) giveBack(id uint32) {
	switch n := len(t.free); {
	case uint64(id)+1 == t.next:
		t.next--
		return
	case n > 0 && uint64(t.free[n-1].first) == uint64(id)+1:
		t.free[n-1].first = id
	default:
		t.free = append(t.free, styleRun{id, id})
	}
	t.freeIDs++
}

// left returns how many ids the table can still give out.
func (t *styleTable) left() uint64 { return t.freeIDs + maxStyleID + 1 - t.next }

// refillDue reports whether few ids are left to give out.
func (t *styleTable) refillDue() bool { return t.left() < styleRefillLeft }

// refill makes free every id below next that no state holds. ids are the
// ids of every state of the table, in order. next falls to the id after
// the highest. pause, when it is not nil, is called every loadCheckCells
// ids; when it reports false, refill stops and leaves the table as it
// was.
func (t *styleTable) refill(ids []uint32, pause func() bool) bool {
	pace := loadPace{pause: pause}
	var free []styleRun
	var freeIDs uint64
	prev := uint64(0)
	for _, id := range ids {
		if !pace.step(1) {
			return false
		}
		if uint64(id) > prev+1 {
			free = append(free, styleRun{uint32(prev + 1), id - 1})
			freeIDs += uint64(id) - prev - 1
		}
		prev = uint64(id)
	}
	t.free, t.freeIDs, t.next = free, freeIDs, prev+1
	return true
}

// refillRoom refills the free ids when few are left, within the work of
// the ticker: a look at each state and a sort of their ids, which it
// counts as one step for each state. A free list that keeps the room of
// far more runs than it holds is made again.
func (t *styleTable) refillRoom(tk *ticker) bool {
	if t.refillDue() {
		if !tk.add(len(t.byID)) {
			return false
		}
		t.refill(slices.Sorted(maps.Keys(t.byID)), nil)
	}
	if roomDue(cap(t.free), len(t.free)) {
		t.free = slices.Clip(slices.Clone(t.free))
	}
	t.clipLog()
	return true
}

// mark returns where an action's ids begin in the log, for forgetFrom.
func (t *styleTable) mark() int { return len(t.log) }

func (t *styleTable) state(id uint32) styleState {
	if e := t.byID[id]; e != nil {
		return e.styleState
	}
	return styleState{}
}

func (t *styleTable) format(id uint32) formula.Format { return t.state(id).f }

func (t *styleTable) ref(id uint32, delta int) {
	e := t.byID[id]
	if e == nil {
		return
	}
	was := e.refs
	e.refs += delta
	formatted := 0
	if e.f != (formula.Format{}) {
		formatted = 1
	}
	switch {
	case was <= 0 && e.refs > 0:
		t.used++
		t.formatted += formatted
	case was > 0 && e.refs <= 0:
		t.used--
		t.formatted -= formatted
	}
	t.touched[id] = struct{}{}
}

// sweep forgets the touched entries that no cell uses, once the writer
// has staged the touched ids. No action can be reverted past a commit, so
// the log starts again.
func (t *styleTable) sweep() {
	t.most = max(t.most, len(t.ids))
	for id := range t.touched {
		if e := t.byID[id]; e != nil && e.refs <= 0 {
			delete(t.ids, e.styleState)
			delete(t.byID, id)
		}
	}
	// touched is used again. One that held many ids is made again, also
	// when a revert took most of them out before.
	if max(t.touchedMost, len(t.touched)) > 1024 {
		t.touched = make(map[uint32]struct{})
	} else {
		clear(t.touched)
	}
	t.touchedMost = 0
	t.settleLog()
}

// settleLog empties the log once no revert can reach what it holds.
func (t *styleTable) settleLog() {
	if cap(t.log) > 1024 {
		t.log = nil
	} else {
		t.log = t.log[:0]
	}
}

// clipLog makes the log again for the entries it holds once it keeps the
// room of far more (roomDue): a revert keeps the entries before its mark,
// which the reverts of earlier marks still need, and leaves the room of
// those it took out. The copy costs no more than the entries that made
// that room.
func (t *styleTable) clipLog() {
	if !roomDue(cap(t.log), len(t.log)) {
		return
	}
	if len(t.log) == 0 {
		t.log = nil
		return
	}
	t.log = slices.Clip(slices.Clone(t.log))
}

// forgetFrom forgets the states that the table gave ids to from the log's
// mark on and that no cell uses, and gives their ids back. An action notes
// the mark when it starts, and an operation when it begins. Once its
// revert has put every cell back, no cell uses those states, and no row of
// sheet.db holds them (technical design, section 4.4), so refused calls
// do not use the ids up. The work is one step for each state the action
// interned.
func (t *styleTable) forgetFrom(mark int) {
	if t.full && mark <= t.fullAt {
		t.full = false
	}
	if mark >= len(t.log) {
		return
	}
	t.most = max(t.most, len(t.ids))
	t.touchedMost = max(t.touchedMost, len(t.touched))
	for i := len(t.log) - 1; i >= mark; i-- {
		g := t.log[i]
		// The entry of a failure has id 0, which no state holds.
		e := t.byID[g.id]
		if e == nil || e.refs > 0 {
			continue
		}
		delete(t.ids, e.styleState)
		delete(t.byID, g.id)
		if !g.touched {
			delete(t.touched, g.id)
		}
		t.giveBack(g.id)
	}
	t.log = t.log[:mark]
	t.clipLog()
}

// dropUnused forgets the states no cell uses, within the work of the
// ticker, and reports whether it got through them. Their ids stay touched,
// so that the next commit deletes a row sheet.db may still hold of them.
func (t *styleTable) dropUnused(tk *ticker) bool {
	if len(t.byID) == t.used {
		return true
	}
	t.most = max(t.most, len(t.ids))
	for id, e := range t.byID {
		if len(t.byID) == t.used {
			break
		}
		if !tk.tick() {
			return false
		}
		if e.refs <= 0 {
			delete(t.ids, e.styleState)
			delete(t.byID, id)
			t.touched[id] = struct{}{}
		}
	}
	return true
}

// formatPatch names the properties a format operation writes and their new
// values.
type formatPatch struct {
	mask uint16
	f    formula.Format
}

func (p formatPatch) has(prop int) bool { return p.mask&(1<<prop) != 0 }

func copyProp(dst *formula.Format, src formula.Format, prop int) {
	switch prop {
	case propBold:
		dst.Bold = src.Bold
	case propItalic:
		dst.Italic = src.Italic
	case propUnderline:
		dst.Underline = src.Underline
	case propStrike:
		dst.Strikethrough = src.Strikethrough
	case propTextColor:
		dst.TextColor = src.TextColor
	case propFill:
		dst.Fill = src.Fill
	case propAlign:
		dst.Align = src.Align
	case propWrap:
		dst.Wrap = src.Wrap
	case propNumber:
		dst.Number = src.Number
	}
}

// apply returns the state after the patch wrote its properties at seq.
func (p formatPatch) apply(s styleState, seq uint64) styleState {
	for prop := 0; prop < propCount; prop++ {
		if p.has(prop) {
			copyProp(&s.f, p.f, prop)
			s.stamps[prop] = seq
		}
	}
	return s
}

// appendFormat appends a format as the JSON object of section 18.2, with
// only the properties that differ from the default.
func appendFormat(dst []byte, f formula.Format) ([]byte, error) {
	dst = append(dst, '{')
	n := len(dst)
	flag := func(name string, on bool) {
		if on {
			dst = appendKey(dst, name, len(dst) > n)
			dst = append(dst, "true"...)
		}
	}
	if f.Align != formula.AlignGeneral {
		dst = appendKey(dst, "align", false)
		dst = strconv.AppendQuote(dst, alignNames[f.Align])
	}
	flag("bold", f.Bold)
	if f.Fill != 0 {
		dst = appendKey(dst, "fill", len(dst) > n)
		dst = strconv.AppendQuote(dst, f.Fill.String())
	}
	flag("italic", f.Italic)
	if f.Number != (formula.NumberFormat{}) {
		data, err := f.Number.MarshalJSON()
		if err != nil {
			return nil, err
		}
		dst = appendKey(dst, "number", len(dst) > n)
		dst = append(dst, data...)
	}
	flag("strikethrough", f.Strikethrough)
	if f.TextColor != 0 {
		dst = appendKey(dst, "text_color", len(dst) > n)
		dst = strconv.AppendQuote(dst, f.TextColor.String())
	}
	flag("underline", f.Underline)
	flag("wrap", f.Wrap)
	return append(dst, '}'), nil
}

func appendKey(dst []byte, name string, comma bool) []byte {
	if comma {
		dst = append(dst, ',')
	}
	dst = append(dst, '"')
	dst = append(dst, name...)
	return append(dst, '"', ':')
}

// decodeFormatPatch reads the named properties of a format object. A null
// resets its property. full is for a stored format, where null is refused.
// Each property comes at most once, and each key of the number format
// once, compared as decoded, so that an escaped spelling of a key is that
// key.
func decodeFormatPatch(data []byte, full bool) (formatPatch, *WriteError) {
	var p formatPatch
	var werr *WriteError
	dec := json.NewDecoder(bytes.NewReader(data))
	err := jsonObject(dec, func(name string) error {
		prop := slices.Index(propNames[:], name)
		switch {
		case prop < 0:
			werr = invalidf("format", "unknown format property %q", name)
		case p.mask&(1<<prop) != 0:
			werr = invalidf("format", "format property %q twice", name)
		}
		if werr != nil {
			return werr
		}
		p.mask |= 1 << prop
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return err
		}
		werr = p.read(prop, value, full)
		if werr != nil {
			return werr
		}
		return nil
	})
	if err == nil {
		err = jsonDone(dec)
	}
	switch {
	case werr != nil:
		return p, werr
	case err != nil:
		return formatPatch{}, invalidf("format", "a format is an object of named properties")
	}
	return p, nil
}

// numberKeys are the keys of a number format object.
var numberKeys = []string{"date", "decimals", "kind", "pattern", "symbol", "thousands", "time"}

// read reads the value of one property into the patch.
func (p *formatPatch) read(prop int, value json.RawMessage, full bool) *WriteError {
	name := propNames[prop]
	if bytes.Equal(value, []byte("null")) {
		if full {
			return invalidf("format", "format property %q is null", name)
		}
		return nil
	}
	var err error
	switch prop {
	case propBold:
		err = json.Unmarshal(value, &p.f.Bold)
	case propItalic:
		err = json.Unmarshal(value, &p.f.Italic)
	case propUnderline:
		err = json.Unmarshal(value, &p.f.Underline)
	case propStrike:
		err = json.Unmarshal(value, &p.f.Strikethrough)
	case propWrap:
		err = json.Unmarshal(value, &p.f.Wrap)
	case propTextColor, propFill:
		var name string
		if err = json.Unmarshal(value, &name); err == nil {
			c, ok := formula.ParseColor(name)
			if !ok {
				return invalidf("format", "%q is not a palette color", name)
			}
			if prop == propFill {
				p.f.Fill = c
			} else {
				p.f.TextColor = c
			}
		}
	case propAlign:
		var name string
		if err = json.Unmarshal(value, &name); err == nil {
			found := false
			for i, known := range alignNames {
				if known == name {
					p.f.Align, found = formula.Align(i), true
				}
			}
			if !found {
				return invalidf("format", "%q is not an alignment", name)
			}
		}
	case propNumber:
		// The number format's own decoder takes a key in any case and the
		// last of a key that comes twice, so its keys are read here first.
		if err = keysOnce(value, numberKeys); err != nil {
			break
		}
		if err = p.f.Number.UnmarshalJSON(value); err == nil && !p.f.Number.Valid() {
			err = fmt.Errorf("the number format is outside rule 6")
		}
		if p.f.Number.Kind == formula.NumberGeneral {
			p.f.Number = formula.NumberFormat{}
		}
	}
	if err != nil {
		return invalidf("format", "format property %q: %v", name, err)
	}
	return nil
}

// keysOnce checks that data is one object whose keys are among keys, each
// at most once and in the case given.
func keysOnce(data []byte, keys []string) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	err := jsonObject(dec, jsonOnce(keys, func(int) error {
		var skip json.RawMessage
		return dec.Decode(&skip)
	}))
	if err == nil {
		err = jsonDone(dec)
	}
	return err
}

func encodeStamps9(s [propCount]uint64) []byte {
	out := make([]byte, 0, propCount)
	for _, v := range s {
		out = binary.AppendUvarint(out, v)
	}
	return out
}

func decodeStamps9(data []byte) (s [propCount]uint64, err error) {
	for i := range s {
		v, n := binary.Uvarint(data)
		if n <= 0 {
			return s, fmt.Errorf("sheet.db style stamps are damaged")
		}
		s[i], data = v, data[n:]
	}
	// The writer stores the nine stamps and nothing after them.
	if len(data) > 0 {
		return s, fmt.Errorf("sheet.db style stamps are damaged")
	}
	return s, nil
}

// wireFormats is the format list every connection shares: a cell on the
// wire names its format by its index here, and index 0 is the default
// format. A snapshot sends the list as it is and a delta sends what it
// added, so the encoded frames of one tab serve every connection.
//
// Between two rebuilds the list only grows, so an index never changes its
// meaning for a client that holds the list. compactWire starts a new list;
// the hub then counts a new wire epoch and no connection gets a frame of
// one epoch on top of the list of another (hub.wireEpoch).
type wireFormats struct {
	list []formula.Format
	idx  map[formula.Format]int
}

func newWireFormats() wireFormats {
	return wireFormats{list: []formula.Format{{}}, idx: map[formula.Format]int{{}: 0}}
}

func (w *wireFormats) index(f formula.Format) {
	if _, ok := w.idx[f]; ok {
		return
	}
	w.list = append(w.list, f)
	w.idx[f] = len(w.list) - 1
}

// indexStyles registers the format of every style in the wire list, in
// style id order: ids holds the ids of every style, in order. A capture
// and a delta name a cell's format by its index there, so every workbook
// built from storage calls it before it is read. pause, when it is not
// nil, is called every loadCheckCells formats and stops the work when it
// reports false. The table is as storage holds it, so the log starts
// again.
func (wb *Workbook) indexStyles(ids []uint32, pause func() bool) bool {
	wb.styles.settleLog()
	for i, id := range ids {
		if pause != nil && i%loadCheckCells == loadCheckCells-1 && !pause() {
			return false
		}
		wb.wire.index(wb.styles.byID[id].f)
	}
	return true
}

// compactWire replaces the wire list by the default format and the formats
// of the styles cells use, in style id order, and reports whether that
// dropped a format. The caller holds the write lock outside any action.
func (wb *Workbook) compactWire() bool {
	ids := make([]uint32, 0, len(wb.styles.byID))
	for id, e := range wb.styles.byID {
		if e.refs > 0 {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	fresh := newWireFormats()
	for _, id := range ids {
		fresh.index(wb.styles.byID[id].f)
	}
	// Every format in use is in the old list, so equal lengths mean that
	// the old list holds nothing else.
	if len(fresh.list) == len(wb.wire.list) {
		return false
	}
	wb.wire = fresh
	return true
}
