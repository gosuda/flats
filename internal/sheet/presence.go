package sheet

import (
	"strconv"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// presenceState is what one Private connection shows the others: the tab it
// watches, its active cell and selected range by ids, and whether the
// person is editing that cell. It lives in memory and never reaches Public
// connections or agents (spec rules 25 to 27).
type presenceState struct {
	set     bool
	tab     uint64
	active  *[2]uint64
	rng     []byte // the range as the list sends it, or nil
	editing bool
}

// presenceRuns is the number of id runs the rows and columns of a presence
// range hold together at most. With the bound on a label it keeps the list
// of every other person of a full sheet inside one frame.
const presenceRuns = 64

// A presence range is relayed to every other editor, whose client walks
// its ids. So the server relays only a range that costs a reader no more
// than a real selection can: the rows and the columns are each "all" or id
// runs that
//
//   - are at most presenceRuns runs together,
//   - name only ids the sheet has given out (below its counters),
//   - do not overlap each other, and
//   - hold together no more ids than a tab has rows or columns at most.
//
// The frame's shape gives the list its form, "all" or runs of numbers
// (protocol.go). appendPresenceIDs is the one place that checks what the
// runs name; anything else is a protocol error and reaches nobody.

// appendPresenceIDs appends the rows or the columns of a presence range in
// one form, whatever spacing the client sent. next is the sheet's id
// counter of the axis and limit the most rows or columns a tab can have.
// It reports the runs it read, and false for a list outside the rules.
func appendPresenceIDs(dst []byte, list *idList, next uint64, limit int) ([]byte, int, bool) {
	if list.all {
		return append(dst, `"all"`...), 0, true
	}
	if list.bad || len(list.items) > presenceRuns {
		return dst, 0, false
	}
	total := uint64(0)
	for i, run := range list.items {
		// first+count cannot wrap: both are checked against next first.
		if run.kind != idRun || run.a <= 0 || run.b <= 0 || uint64(run.a) >= next || uint64(run.b) > next-uint64(run.a) {
			return dst, 0, false
		}
		if total += uint64(run.b); total > uint64(limit) {
			return dst, 0, false
		}
		for _, other := range list.items[:i] {
			if run.a < other.a+other.b && other.a < run.a+run.b {
				return dst, 0, false
			}
		}
	}
	dst = append(dst, '[')
	for i, run := range list.items {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, '[')
		dst = strconv.AppendInt(dst, run.a, 10)
		dst = append(dst, ',')
		dst = strconv.AppendInt(dst, run.b, 10)
		dst = append(dst, ']')
	}
	return append(dst, ']'), len(list.items), true
}

func (c *conn) setPresence(f *presenceFrame) bool {
	// The counters only grow while the workbook lives, so an id that was
	// given out stays inside them.
	c.in.mu.RLock()
	wb := c.in.wb
	nextTab, nextRow, nextCol := wb.nextTab, wb.nextRow, wb.nextCol
	c.in.mu.RUnlock()
	if f.Tab == 0 || f.Tab >= nextTab {
		return false
	}
	if cell := f.Active; cell != nil && (cell[0] == 0 || cell[0] >= nextRow || cell[1] == 0 || cell[1] >= nextCol) {
		return false
	}
	var rng []byte
	if f.Range != nil {
		var rows, cols int
		var ok bool
		rng = append(rng, `{"rows":`...)
		if rng, rows, ok = appendPresenceIDs(rng, &f.Range.Rows, nextRow, formula.MaxRows); !ok {
			return false
		}
		rng = append(rng, `,"cols":`...)
		if rng, cols, ok = appendPresenceIDs(rng, &f.Range.Cols, nextCol, formula.MaxCols); !ok || rows+cols > presenceRuns {
			return false
		}
		rng = append(rng, '}')
	}
	// More than 20 updates per second are dropped, not an error.
	if !c.presences.allow(time.Now(), 20) {
		return true
	}
	c.mu.Lock()
	c.presence = presenceState{set: true, tab: f.Tab, active: f.Active, rng: rng, editing: f.Editing}
	c.mu.Unlock()
	c.in.presenceChanged(c)
	return true
}

// presenceChanged tells every Private connection but the one that changed
// that the people list is out of date.
func (in *Instance) presenceChanged(except *conn) {
	h := in.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.conns {
		if c == except || c.public {
			continue
		}
		c.mu.Lock()
		c.stale = true
		c.mu.Unlock()
		c.poke()
	}
}

// presenceFrame lists the other people for one connection.
func (in *Instance) presenceFrame(self *conn) string {
	h := in.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	dst := []byte(`{"type":"presence","people":[`)
	first := true
	for c := range h.conns {
		if c == self || c.public || c.s == nil {
			continue
		}
		c.mu.Lock()
		p := c.presence
		c.mu.Unlock()
		if !p.set {
			continue
		}
		if !first {
			dst = append(dst, ',')
		}
		first = false
		dst = append(dst, `{"session_id":`...)
		dst = appendString(dst, c.s.id)
		dst = append(dst, `,"label":`...)
		dst = appendString(dst, c.s.label)
		dst = append(dst, `,"verified":`...)
		dst = strconv.AppendBool(dst, c.s.verified)
		dst = append(dst, `,"color":`...)
		dst = strconv.AppendUint(dst, uint64(c.s.ord%12), 10)
		dst = append(dst, `,"tab":`...)
		dst = strconv.AppendUint(dst, p.tab, 10)
		if p.active != nil {
			dst = append(dst, `,"active":[`...)
			dst = strconv.AppendUint(dst, p.active[0], 10)
			dst = append(dst, ',')
			dst = strconv.AppendUint(dst, p.active[1], 10)
			dst = append(dst, ']')
		}
		if p.rng != nil {
			dst = append(dst, `,"range":`...)
			dst = append(dst, p.rng...)
		}
		if p.editing {
			dst = append(dst, `,"editing":true`...)
		}
		dst = append(dst, '}')
	}
	return string(append(dst, ']', '}'))
}
