package sheet

import (
	"slices"
	"strconv"
	"sync/atomic"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// A tab snapshot is never held as a whole set of encoded frames outside a
// budget. Its memory has three owners, and each byte is charged to its
// owner before it is allocated:
//
//   - The capture: the copied records, the row ids of the head and the
//     working buffers of the encoder. They are charged to the capture
//     budget (capture.reserve) and stay charged until the last frame was
//     handed on.
//   - The transport: one encoded frame at a time, built after the send
//     queue of its connection has room for it, or written straight to an
//     HTTP response.
//   - The snapshot cache: frames kept for other connections (snapEntry).
//
// The encoder therefore produces one frame, hands it to emit and reuses
// its buffers for the next.

const (
	snapshotCacheMax = 64 << 20

	// frameSlack is the room a snapshot frame keeps for its type, its
	// generation and its closing brackets, which the encoder does not
	// measure.
	frameSlack = 256

	// What one encoded cell takes at most beside its text: brackets, the
	// column id, the kind, the style index, and a number with its shown and
	// its edit text, which no format makes longer than shownMax bytes.
	shownMax  = 512
	cellFixed = 96 + 2*(shownMax+2)
	// cellMax is the most one encoded cell takes under the text limit: its
	// value and its edit text, each a JSON string of MaxTextLen code points
	// of at most 6 bytes.
	cellMax = cellFixed + 2*(6*formula.MaxTextLen+2)
)

// snapshotCached is the host-wide count of bytes of snapshot frames that
// the caches of all sheets hold or are collecting. It never passes
// snapshotCacheMax: reserveSnapshotCache is the only place that adds to
// it, and it adds only what still fits.
var snapshotCached atomic.Int64

func reserveSnapshotCache(n int64) bool {
	for {
		held := snapshotCached.Load()
		if held+n > snapshotCacheMax {
			return false
		}
		if snapshotCached.CompareAndSwap(held, held+n) {
			return true
		}
	}
}

// encoderGrowths counts encoder buffers that outgrew their reservation.
// Under the sheet's limits none does; a test checks it.
var encoderGrowths atomic.Int64

type snapKey struct {
	tab    TabID
	public bool
	epoch  string // the access epoch of the route
	shut   uint64 // the hub's count of Public withdrawals
}

// snapEntry is the encoded frames of one tab snapshot, kept so that other
// connections need no capture of their own.
//
// Every byte of an entry is reserved before the frame that holds it is
// kept, in two places: in the host-wide cache limit, until the entry
// leaves the cache or is given up unfinished, and in the capture budget,
// until the cache and every connection that sends the entry have let go of
// it. keep, giveUp, hub.cache, hub.dropSnaps and hub.unref are the only
// places that move those counts.
type snapEntry struct {
	seq       uint64
	wireEpoch uint64
	frames    []string // each without its leading {"type":...,"generation":N,
	kinds     []string
	bytes     int64
	refs      int // holders: the cache and sending connections; under hub.mu
}

// entryFrameCost is what an entry counts for a frame beside its text.
const entryFrameCost = 48

// keep adds a frame to an entry that is being collected and returns the
// kept text. It reports false, and keeps nothing, when a budget has no
// room.
func (e *snapEntry) keep(kind string, rest []byte) (string, bool) {
	n := int64(len(rest)) + entryFrameCost
	if !reserveSnapshotCache(n) {
		return "", false
	}
	if captureBytes.Add(n) > captureBudget {
		captureBytes.Add(-n)
		snapshotCached.Add(-n)
		return "", false
	}
	text := string(rest)
	e.frames, e.kinds = append(e.frames, text), append(e.kinds, kind)
	e.bytes += n
	return text, true
}

// giveUp returns what an entry that never reached the cache reserved.
func (e *snapEntry) giveUp() {
	snapshotCached.Add(-e.bytes)
	captureBytes.Add(-e.bytes)
	e.frames, e.kinds, e.bytes = nil, nil, 0
	signalCaptureRoom()
}

// dropSnaps empties the snapshot cache. The caller holds h.mu.
func (h *hub) dropSnaps() {
	for key, e := range h.snaps {
		snapshotCached.Add(-e.bytes)
		delete(h.snaps, key)
		h.unref(e)
	}
}

// unref lets go of a cached entry. The last holder gives its bytes back to
// the capture budget. The caller holds h.mu.
func (h *hub) unref(e *snapEntry) {
	if e.refs--; e.refs == 0 {
		captureBytes.Add(-e.bytes)
		signalCaptureRoom()
	}
}

// cache puts a collected entry into the cache if it is still what a new
// connection would be sent: the sheet, the access epoch and the format
// list are the ones it was built under and no other connection was first.
func (h *hub) cache(key snapKey, e *snapEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.last != e.seq || h.wireEpoch != e.wireEpoch || h.epoch != key.shut || h.snaps[key] != nil {
		e.giveUp()
		return
	}
	e.refs = 1
	h.snaps[key] = e
}

// snapSource is what a connection sends one snapshot from: frames the
// cache holds, or a capture of its own with the encoder that turns it into
// frames. The connection closes the source when it is done with it.
type snapSource struct {
	tab       TabID
	seq       uint64
	wireEpoch uint64
	key       snapKey
	entry     *snapEntry
	enc       *snapEncoder
}

func (s *snapSource) close(h *hub) {
	if s.entry != nil {
		h.mu.Lock()
		h.unref(s.entry)
		h.mu.Unlock()
		return
	}
	s.enc.c.release()
}

// openSnapshot returns the source of a snapshot of a tab at the current
// sequence: the cached frames while the sequence and the access epoch are
// still the ones they were built under (section 12.2), or a new capture.
// A tab that is gone is replaced by the first tab, so a client that
// watched it, or asked for it, gets a tab it can show. The head carries
// the tab list of its sequence, which names that tab.
// Before the first Public snapshot of an access epoch the format list is
// rebuilt first, and also when it is past its bound, which the writer
// otherwise sees to after every commit (tightenWire).
func (in *Instance) openSnapshot(tab TabID, public bool, epoch string) (*snapSource, error) {
	h := in.hub
	for {
		in.mu.RLock()
		wb := in.wb
		t := wb.byID[tab]
		if t == nil && len(wb.tabs) > 0 {
			t = wb.tabs[0] // a sheet keeps at least one tab
		}
		if t == nil {
			in.mu.RUnlock()
			return nil, errGone
		}
		h.mu.Lock()
		key := snapKey{t.id, public, epoch, h.epoch}
		rebuild := (public && h.publicWire != publicWireKey(epoch, h.epoch)) || wb.wireLoose()
		src := &snapSource{tab: t.id, seq: wb.seq, wireEpoch: h.wireEpoch, key: key}
		if e := h.snaps[key]; e != nil && e.seq == wb.seq && !rebuild {
			e.refs++
			src.entry = e
		}
		h.mu.Unlock()
		if rebuild {
			in.mu.RUnlock()
			in.rebuildWire(public, epoch)
			continue
		}
		if src.entry != nil {
			in.mu.RUnlock()
			return src, nil
		}
		c, head, err := wb.captureTab(t)
		in.mu.RUnlock()
		if err != nil {
			return nil, err
		}
		if src.enc, err = newSnapEncoder(c, head); err != nil {
			c.release()
			return nil, err
		}
		return src, nil
	}
}

func publicWireKey(epoch string, shut uint64) string {
	return epoch + "/" + strconv.FormatUint(shut, 10)
}

// wireSlack is how many formats the wire list may hold beyond twice the
// states in use that carry a format before it is rebuilt.
const wireSlack = 1024

// wireLoose reports whether the format list holds far more formats than
// the sheet uses. It counts the states that carry a format, which only
// cells hold: blank records keep states of the default format, and any
// number of them between two ticks would let the list grow with them.
// The caller holds the read lock.
func (wb *Workbook) wireLoose() bool {
	return len(wb.wire.list) > 2*wb.styles.formatted+wireSlack
}

// rebuildWire replaces the format list by the formats in use, and for a
// Public snapshot notes the access epoch that has now seen a rebuilt list.
// A list that changed starts a new wire epoch (hub.restyle).
func (in *Instance) rebuildWire(public bool, epoch string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	changed := in.wb.compactWire()
	h := in.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if public {
		h.publicWire = publicWireKey(epoch, h.epoch)
	}
	if changed {
		h.restyle()
	}
}

// restyle starts a new wire epoch after the format list was rebuilt:
// cached frames are dropped, no client resumes from the sequences of the
// list before, and every connection that watches a tab is resynced, with a
// snapshot that carries the new list, before it gets a delta encoded
// against it (conn.deliver). The caller holds h.mu.
func (h *hub) restyle() {
	h.wireEpoch++
	h.wireAt = h.last
	h.dropSnaps()
}

// tightenWire keeps the format list within its bound while the stream
// runs: when the list has grown past twice the states in use that carry
// a format plus wireSlack, it is rebuilt and a new wire epoch starts. The writer calls it
// after every commit, under the write lock, once the deltas of the commit
// are in the log and the unused styles are swept, and wakes the
// connections when it has released the lock. A connection then moves to
// the new list at once (conn.deliver), so neither the host nor a browser
// holds a list past the bound for longer than one commit, whether or not a
// new connection arrives.
//
// A rebuild leaves at most one format more than those states, the
// default format. The next one needs the list to pass twice those states
// plus wireSlack again, so at least wireSlack formats are added to the list, or go out of
// use, between two rebuilds.
func (in *Instance) tightenWire() {
	if !in.wb.wireLoose() || !in.wb.compactWire() {
		return
	}
	h := in.hub
	h.mu.Lock()
	h.restyle()
	h.mu.Unlock()
}

// tabHead is what a snapshot's head carries beside the capture: the axes
// and the settings of the tab, and the tab list and the date of the
// sheet, copied under the read lock.
type tabHead struct {
	rows  []uint64 // row ids in order
	fixed []byte   // seq, date, tabs, cols, widths and frozen counts, encoded
}

// headTabsSize bounds the date and the tab list in a head: every tab with
// its id and counts, and its name, each byte of which takes at most 6
// bytes as JSON.
func headTabsSize(wb *Workbook) int {
	n := 64
	for _, t := range wb.tabs {
		n += 192 + 6*len(t.name)
	}
	return n
}

// captureTab copies a whole tab and what its tab_head needs of the axes.
// The caller holds the read lock.
func (wb *Workbook) captureTab(t *Tab) (*capture, *tabHead, error) {
	full := formula.Range{Last: formula.Pos{Row: int32(t.rows.len()) - 1, Col: int32(t.cols.len()) - 1}}
	c, err := wb.captureWire(t, full)
	if err != nil {
		return nil, nil, err
	}
	fixedSize := 64*t.cols.len() + 256 + headTabsSize(wb)
	if !c.reserve(formula.HeapBytes(uintptr(8*t.rows.len()), false) + formula.HeapBytes(uintptr(fixedSize), false) + tabHeadBytes) {
		c.release()
		return nil, nil, unavailable("capture_budget")
	}
	head := &tabHead{rows: slices.Clone(t.rows.ids)}
	fixed := make([]byte, 0, fixedSize)
	fixed = append(fixed, `,"seq":"`...)
	fixed = strconv.AppendUint(fixed, wb.seq, 10)
	// A client drops the deltas it held for its editor when a snapshot
	// replaces its grid, and with them what they changed of the tab list
	// and the date. So every head carries both as of its sequence.
	fixed = append(fixed, `","date":"`...)
	fixed = append(fixed, wb.date.String()...)
	fixed = append(fixed, `","tabs":`...)
	fixed = appendTabInfo(fixed, wb)
	fixed = append(fixed, `,"cols":`...)
	fixed = appendRuns(fixed, t.cols.ids)
	fixed = append(fixed, `,"widths":{`...)
	first := true
	for _, id := range t.cols.ids {
		if w, ok := t.widths[ColID(id)]; ok && w.px != defaultWidth {
			if !first {
				fixed = append(fixed, ',')
			}
			first = false
			fixed = append(fixed, '"')
			fixed = strconv.AppendUint(fixed, id, 10)
			fixed = append(fixed, '"', ':')
			fixed = strconv.AppendInt(fixed, int64(w.px), 10)
		}
	}
	fixed = append(fixed, `},"frozen_rows":`...)
	fixed = strconv.AppendInt(fixed, int64(t.frozenRows), 10)
	fixed = append(fixed, `,"frozen_cols":`...)
	head.fixed = strconv.AppendInt(fixed, int64(t.frozenCols), 10)
	return c, head, nil
}

// snapEncoder turns a tab capture into snapshot frames, one at a time. Its
// working buffers are sized from the capture and charged to it before they
// are allocated, so encoding allocates nothing that is not counted,
// whatever the text holds.
type snapEncoder struct {
	c    *capture
	head *tabHead
	tab  string
	// buf holds the head frame being built, body the rows frame, item one
	// run, format or cell, and scratch the texts of one cell.
	buf, body, item, scratch []byte
}

func digits(n uint64) int {
	d := 1
	for n >= 10 {
		n /= 10
		d++
	}
	return d
}

// newSnapEncoder reserves the encoder's buffers on the capture, which
// stays the caller's to release.
//
// No frame passes the server frame limit, whatever the tab holds. A head
// whose row runs and styles do not fit ends with more, and tab_more frames
// carry the rest in order. A row whose cells do not fit a frame goes on in
// the next rows frame under the same row id.
func newSnapEncoder(c *capture, head *tabHead) (*snapEncoder, error) {
	enc := &snapEncoder{c: c, head: head, tab: `"tab":` + strconv.FormatUint(uint64(c.tab), 10)}
	// The head: its fixed fields, every run and every format, exactly.
	headSize := len(enc.tab) + len(head.fixed) + 64
	forRuns(head.rows, func(first, count uint64) { headSize += digits(first) + digits(count) + 4 })
	itemSize := 64
	var one [512]byte
	for _, f := range c.formats {
		n := 2
		if out, err := appendFormat(one[:0], f); err == nil {
			n = len(out)
		}
		headSize += n + 1
		itemSize = max(itemSize, n)
	}
	// The rows: an upper bound for each cell from the bytes of its texts,
	// of which a control character takes six as JSON. The scratch buffer
	// holds a cell's value as text and what it shows.
	scratchSize, bodySize := shownMax+64, len(enc.tab)+96
	for i := range c.rows {
		bodySize += 32
		for k := range c.rows[i].cells {
			cell := &c.rows[i].cells[k]
			value := len(cell.val.Text())
			size := cellFixed + 6*value
			if cell.f == nil {
				size += 6 * (len(cell.content.Text) + 1)
			}
			itemSize = max(itemSize, min(size, cellMax))
			scratchSize = max(scratchSize, 2*min(value, 4*formula.MaxTextLen)+shownMax+64)
			if bodySize < serverFrameMax {
				bodySize += size + 1
			}
		}
	}
	headSize = min(headSize, serverFrameMax)
	bodySize = min(bodySize, serverFrameMax)
	var buffers int64
	for _, n := range [...]int{headSize, bodySize, itemSize, scratchSize} {
		buffers += formula.HeapBytes(uintptr(n), false)
	}
	if !c.reserve(buffers) {
		return nil, unavailable("capture_budget")
	}
	enc.buf, enc.body = make([]byte, 0, headSize), make([]byte, 0, bodySize)
	enc.item, enc.scratch = make([]byte, 0, itemSize), make([]byte, 0, scratchSize)
	return enc, nil
}

// run encodes the snapshot: the head, rows frames of at most 2,000 rows,
// and tab_done. It hands each frame to emit without its type and
// generation, which the caller puts in front, and stops when emit returns
// false. The bytes of a frame are the encoder's and are valid only until
// emit returns.
func (enc *snapEncoder) run(emit func(kind string, rest []byte) bool) bool {
	c, tab := enc.c, enc.tab
	limit := serverFrameMax - frameSlack
	sizes := [4]int{cap(enc.buf), cap(enc.body), cap(enc.item), cap(enc.scratch)}
	defer func() {
		if sizes != [4]int{cap(enc.buf), cap(enc.body), cap(enc.item), cap(enc.scratch)} {
			encoderGrowths.Add(1)
		}
	}()
	ok := true

	// The head: the fixed fields, then the row runs and the styles as far
	// as they fit, the rest in tab_more frames.
	kind := "tab_head"
	buf := append(append(enc.buf[:0], tab...), enc.head.fixed...)
	open, items := "", 0
	closeFrame := func(more bool) {
		if !ok {
			return
		}
		if open != "" {
			buf = append(buf, ']')
		}
		if more {
			buf = append(buf, `,"more":true`...)
		}
		buf = append(buf, '}')
		ok = emit(kind, buf)
		kind, open, items = "tab_more", "", 0
		buf = append(buf[:0], tab...)
	}
	item := enc.item[:0]
	put := func(list string) {
		if items > 0 && len(buf)+len(item)+len(list)+8 > limit {
			closeFrame(true)
		}
		if open != list {
			if open != "" {
				buf = append(buf, ']')
			}
			buf = append(append(append(buf, ',', '"'), list...), '"', ':', '[')
			open = list
		} else {
			buf = append(buf, ',')
		}
		buf = append(buf, item...)
		items++
	}
	forRuns(enc.head.rows, func(first, count uint64) {
		if !ok {
			return
		}
		item = append(item[:0], '[')
		item = strconv.AppendUint(item, first, 10)
		item = append(item, ',')
		item = strconv.AppendUint(item, count, 10)
		item = append(item, ']')
		put("rows")
	})
	for _, f := range c.formats {
		if !ok {
			break
		}
		var err error
		if item, err = appendFormat(item[:0], f); err != nil {
			item = append(item[:0], '{', '}')
		}
		put("styles")
	}
	closeFrame(false)
	enc.buf = buf

	// The rows. A frame starts with its part number and is closed when it
	// holds 2,000 rows or the next cell does not fit.
	body := enc.body[:0]
	parts, rows, start := 0, 0, 0
	openPart := func() {
		body = append(body[:0], tab...)
		body = append(body, `,"part":`...)
		body = strconv.AppendInt(body, int64(parts), 10)
		body = append(body, `,"rows":[`...)
		rows, start = 0, len(body)
	}
	closePart := func() {
		if rows == 0 || !ok {
			return
		}
		body = append(body, ']', '}')
		ok = emit("rows", body)
		parts++
		openPart()
	}
	openPart()
	for i := range c.rows {
		if !ok {
			break
		}
		row := &c.rows[i]
		mark, before := len(body), rows // the frame without this row
		openRow := func() {
			if rows > 0 {
				body = append(body, ',')
			}
			body = append(body, '[')
			body = strconv.AppendUint(body, uint64(row.id), 10)
			body = append(body, ',', '[')
			rows++
		}
		openRow()
		inRow := 0 // cells of this row in the open frame
		for k := range row.cells {
			cell := &row.cells[k]
			item = appendCellOut(item[:0], cell.id, cell.content, cell.f, cell.val, c.formats[cell.style], int(cell.style), &enc.scratch)
			if len(body)+len(item)+4 > limit && (inRow > 0 || before > 0) {
				if inRow > 0 {
					body = append(body, ']', ']')
				} else {
					body, rows = body[:mark], before
				}
				closePart()
				if !ok {
					break
				}
				mark, before, inRow = start, 0, 0
				openRow()
			}
			if inRow > 0 {
				body = append(body, ',')
			}
			body = append(body, item...)
			inRow++
		}
		if !ok {
			break
		}
		body = append(body, ']', ']')
		if rows >= snapshotRows {
			closePart()
		}
	}
	closePart()
	enc.body, enc.item = body, item
	if !ok {
		return false
	}
	done := append(body[:0], tab...)
	done = append(done, `,"parts":`...)
	done = strconv.AppendInt(done, int64(parts), 10)
	return emit("tab_done", append(done, '}'))
}
