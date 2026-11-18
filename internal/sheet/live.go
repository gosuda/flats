package sheet

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
	"github.com/gosuda/flats/internal/wsconn"
)

const (
	privateQueue     = 4 << 20
	publicQueue      = 1 << 20
	maxPrivateConns  = 64
	maxPublicConns   = 256
	deltaLogEntries  = 20000
	deltaLogBytes    = 16 << 20
	deltaLogAge      = time.Hour
	assemblyBudget   = 64 << 20
	publicStartsPerS = 8
	sendBurst        = 128
	drainWait        = 30 * time.Second
)

var (
	publicSendBudget = func() *wsconn.Budget { b := new(wsconn.Budget); b.Lower(256 << 20); return b }()
	assemblyBytes    atomic.Int64 // host-wide bytes of actions being assembled
	deltaHeld        atomic.Int64 // host-wide bytes of entries that left their log and are still sent
	// deltaHeldBudget bounds deltaHeld. Tests lower it.
	deltaHeldBudget = func() *atomic.Int64 { n := new(atomic.Int64); n.Store(256 << 20); return n }()
	connections     atomic.Int64
	// livePing is how often a live connection is pinged, zero for the
	// interval of wsconn. Tests shorten it.
	livePing time.Duration
)

// HostConnections returns the number of open sheet connections of the host.
func HostConnections() int64 { return connections.Load() }

// hub holds the connections of one sheet, the delta log for reconnects and
// the snapshot cache. All of it lives in memory only.
//
// The delta log is bounded by deltaLogEntries, deltaLogAge and
// deltaLogBytes. bytes counts everything the log retains: the encoded
// source of every entry and every projected frame an entry keeps for
// reuse. Three rules hold it under deltaLogBytes, and they are enforced
// only in append and frame:
//
//   - a kept frame never pushes the log over the bound: frame keeps one
//     only when there is room after dropping the kept frames of older
//     entries, and otherwise hands it to its one sender unkept;
//   - when a new entry needs room, kept frames go first, oldest first,
//     since they can be built again, and whole entries after them;
//   - the newest entry always stays, whatever its size.
//
// A delta larger than a frame is never built whole: every connection cuts
// its parts from the entry's own fields as it sends them (sendParts), so
// the connections that send one entry share its one copy. A connection
// sends one entry at a time and holds it while it does (next, release).
// An entry that leaves the log meanwhile stays charged to deltaHeld,
// host-wide, until its last sender lets go; one whose charge does not fit
// deltaHeldBudget is revoked, and its senders stop and resync.
type hub struct {
	mu              sync.Mutex
	conns           map[*conn]struct{}
	log             []*deltaRec
	bytes           int    // source and kept frames of every entry
	kept            int    // the kept frames among bytes
	last            uint64 // newest sequence the connections may be at
	floor           uint64 // the log reaches back to a watermark at or above it
	private, public int
	epoch           uint64 // counts Public withdrawals
	snaps           map[snapKey]*snapEntry
	starts          []time.Time

	// wireEpoch counts the format lists the sheet has had: it moves when
	// the list is rebuilt and when a rollback installs another workbook.
	// Every log entry and every snapshot carries the epoch it was encoded
	// under, and a connection gets a delta only while it holds the list of
	// that epoch (conn.deliver). wireAt is the newest sequence of the
	// epoch before: no client resumes from it or from below. publicWire is
	// the Public access epoch that has seen a rebuilt list.
	wireEpoch  uint64
	wireAt     uint64
	publicWire string

	// resetAt is the sequence of the newest rollback in the stream.
	resetAt uint64

	// calc is set while the browsers are told that the sheet calculates
	// (calculating). early holds the connections that wait for their
	// session. They are not in conns yet and have no pump.
	calc  atomic.Bool
	early map[*conn]struct{}
}

func newHub() *hub {
	return &hub{conns: map[*conn]struct{}{}, early: map[*conn]struct{}{}, snaps: map[snapKey]*snapEntry{}}
}

func (h *hub) count() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return int64(h.private + h.public)
}

// start sets the sequence the stream begins at, after a load.
func (h *hub) start(seq uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, rec := range h.log {
		h.leave(rec)
	}
	h.log, h.bytes, h.kept, h.last, h.floor = nil, 0, 0, seq, seq
	h.dropSnaps()
}

func (h *hub) append(rec *deltaRec, now time.Time) {
	rec.at = now
	h.mu.Lock()
	defer h.mu.Unlock()
	if rec.reset {
		// The rollback installed a workbook with a format list of its own.
		h.wireEpoch++
		h.wireAt, h.resetAt = rec.seq, rec.seq
	}
	rec.wireEpoch = h.wireEpoch
	h.log = append(h.log, rec)
	h.bytes += rec.size
	h.last = rec.seq
	h.unkeep(0, rec.seq)
	drop := 0
	for drop < len(h.log)-1 && (len(h.log)-drop > deltaLogEntries || h.bytes > deltaLogBytes || now.Sub(h.log[drop].at) > deltaLogAge) {
		old := h.log[drop]
		h.bytes -= old.size + old.cached
		h.kept -= old.cached
		old.frames, old.cached = nil, 0
		h.leave(old)
		h.floor = old.seq
		drop++
	}
	// An entry that left the log is not reachable through the log's memory
	// either: cut clears the places it frees.
	h.log = cut(h.log, 0, drop)
	h.dropSnaps()
}

// leave marks an entry that left the log. When connections still send it,
// what it holds is charged to deltaHeld until the last of them lets go,
// and the entry is revoked when that does not fit. The caller holds h.mu.
func (h *hub) leave(rec *deltaRec) {
	rec.left = true
	if rec.holders == 0 || rec.charged != 0 {
		return
	}
	rec.charged = int64(rec.size)
	if deltaHeld.Add(rec.charged) > deltaHeldBudget.Load() {
		rec.revoked.Store(true)
	}
}

// unkeep drops kept frames, oldest entry first and only of entries before
// seq, until the log has room for n more bytes or no such frame is left.
// The caller holds h.mu.
func (h *hub) unkeep(n int, before uint64) {
	for _, rec := range h.log {
		if h.kept == 0 || h.bytes+n <= deltaLogBytes || rec.seq >= before {
			return
		}
		h.bytes -= rec.cached
		h.kept -= rec.cached
		rec.frames, rec.cached = nil, 0
	}
}

// frame returns the delta frame of an entry for a connection that watches
// tab, a frame of at most serverFrameMax bytes, from its fields. A frame
// is built once for the connections that share it while the log has room
// to keep it.
func (h *hub) frame(rec *deltaRec, tab TabID, public bool, fields []deltaField) string {
	key := frameKey{tab, public}
	h.mu.Lock()
	f, ok := rec.frames[key]
	h.mu.Unlock()
	if ok {
		return f
	}
	f = rec.build(fields)
	h.mu.Lock()
	defer h.mu.Unlock()
	if kept, ok := rec.frames[key]; ok {
		return kept // another connection built it meanwhile
	}
	if rec.left {
		return f // the entry has left the log
	}
	h.unkeep(len(f), rec.seq)
	if h.bytes+len(f) <= deltaLogBytes {
		if rec.frames == nil {
			rec.frames = map[frameKey]string{}
		}
		rec.frames[key] = f
		rec.cached += len(f)
		h.bytes += len(f)
		h.kept += len(f)
	}
	return f
}

// resumes reports whether a client that holds the state of seq can go on
// from the log: the log reaches back to it and the client's format list is
// the current one, whose epoch it returns.
func (h *hub) resumes(seq uint64) (wireEpoch uint64, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ok = seq <= h.last && seq >= h.floor && (h.wireEpoch == 0 || seq > h.wireAt)
	return h.wireEpoch, ok
}

// next returns the entry after seq, held for the caller until it calls
// release, and the wire epoch at that moment. rec is nil when there is
// none yet. ok is false when the log does not reach back to seq: the
// connection needs a new snapshot.
func (h *hub) next(seq uint64) (rec *deltaRec, wireEpoch uint64, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if seq > h.last || seq < h.floor {
		return nil, 0, false
	}
	i, _ := slices.BinarySearchFunc(h.log, seq+1, func(r *deltaRec, seq uint64) int {
		return cmp.Compare(r.seq, seq)
	})
	if i == len(h.log) {
		return nil, h.wireEpoch, true
	}
	rec = h.log[i]
	rec.holders++
	return rec, h.wireEpoch, true
}

// release lets go of an entry next returned. The last sender of an entry
// that left the log gives its charge back.
func (h *hub) release(rec *deltaRec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if rec.holders--; rec.holders == 0 && rec.charged != 0 {
		deltaHeld.Add(-rec.charged)
		rec.charged = 0
	}
}

// partCount returns the number of parts of the frame of fields, which it
// finds once for all the connections that send it.
func (h *hub) partCount(ctx context.Context, rec *deltaRec, key frameKey, fields []deltaField) (int, error) {
	h.mu.Lock()
	pc := rec.counts[key]
	if pc == nil {
		pc = &partCount{done: make(chan struct{})}
		if rec.counts == nil {
			rec.counts = map[frameKey]*partCount{}
		}
		rec.counts[key] = pc
		h.mu.Unlock()
		pc.n, pc.err = splitDelta(fields, nil)
		close(pc.done)
	} else {
		h.mu.Unlock()
	}
	select {
	case <-pc.done:
		return pc.n, pc.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (h *hub) wake() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.conns {
		c.poke()
	}
}

func (h *hub) closeAll(code string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.conns {
		if c.public {
			c.ws.Abort()
		} else {
			if code == "deleted" || code == "renamed" {
				c.ws.Send(envelope("fatal", "code", code))
			}
			c.ws.Close(1001, code)
		}
		c.cancel()
	}
	h.dropSnaps()
}

func (h *hub) withdrawPublic() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.epoch++
	for c := range h.conns {
		if c.public {
			c.ws.Abort()
			c.cancel()
		}
	}
	h.dropSnaps()
}

// calculating sets whether the sheet is in a recalculation its browsers
// are told of. The pump of every connection sends the status when it
// changed for the connection (conn.tell). A connection that waits for its
// session has no pump yet and is told here.
func (h *hub) calculating(on bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calc.Swap(on) == on {
		return
	}
	for c := range h.conns {
		c.poke()
	}
	for c := range h.early {
		c.ws.Send(envelope("status", "calculating", on))
	}
}

// awaitSession adds a connection that is about to wait for the writer to
// store or find its session, and tells it when the sheet calculates. The
// connection leaves the set when it is admitted or ends.
func (h *hub) awaitSession(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.early[c] = struct{}{}
	if h.calc.Load() {
		c.ws.Send(envelope("status", "calculating", true))
	}
}

func (h *hub) sessionSettled(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.early, c)
}

// overwritten tells the session whose content was replaced (section 7.2).
func (h *hub) overwritten(session uint32, tab TabID, row RowID, col ColID, replaced uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.conns {
		if c.s != nil && c.s.ord == session {
			c.note(envelope("overwritten", "tab", uint64(tab), "row", uint64(row), "col", uint64(col), "replaced", decimal(replaced)))
		}
	}
}

// admitPublicStart limits Public snapshot starts to eight per second.
func (h *hub) admitPublicStart(now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.starts = slices.DeleteFunc(h.starts, func(at time.Time) bool { return now.Sub(at) >= time.Second })
	if len(h.starts) >= publicStartsPerS {
		return false
	}
	h.starts = append(h.starts, now)
	return true
}

// publicStartNow is the clock of the Public start limit, and
// publicStartPause waits before a start that the limit held back is tried
// again. It returns false when the connection ends first. Tests replace
// both to step through the limit without waiting.
var (
	publicStartNow   = time.Now
	publicStartPause = func(ctx context.Context) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
			return true
		}
	}
)

type connCmd struct {
	frame      string // a frame to send in order
	ackSeq     uint64 // for an ack: deltas through this sequence go first
	watch      bool
	generation int
	tab        TabID
	fatal      string // the code of a fatal frame that ends the connection
}

// conn is one WebSocket connection. Its handler goroutine reads frames and
// its sender goroutine, the pump (run), writes them.
//
// Order on the wire. Only the pump decides what a client receives and in
// which order; the reader and every other goroutine queue a command. These
// rules hold, each enforced in the one place named:
//
//  1. Commands leave in the order they were queued, and a fatal frame is a
//     command (endWith). So every ack and answer that was decided before a
//     fatal is on the wire before it. Only a connection that is broken or
//     over its queue, and every connection of an instance that stops, is
//     closed at once.
//  2. A snapshot goes on the wire only while it is the newest intent of
//     the connection. intent counts the watches the pump has started to
//     serve and the resyncs it has begun; a snapshot remembers the count
//     it started under and sendSnapshot refuses any other.
//  3. A resync never continues an older capture. When the deltas that
//     precede a snapshot cannot be delivered, snapshot drops its capture,
//     cuts the client off (cutOff) and captures the tab again, under a
//     generation above every one in use. Recovery never runs nested in a
//     snapshot: deliver reports, and only flush and snapshot act on it.
//  4. A delta reaches a client only on top of the format list it was
//     encoded against (deliver compares wireEpoch).
type conn struct {
	in        *Instance
	ws        *wsconn.Conn
	ctx       context.Context
	cancel    context.CancelFunc
	public    bool
	epoch     string
	s         *session
	wakeCh    chan struct{}
	cmds      chan connCmd
	seq       uint64
	tab       TabID
	gen       int
	resynced  bool
	burst     int
	ending    atomic.Bool
	intent    uint64       // see rule 2
	asked     atomic.Int64 // watches the reader has queued
	served    int64        // watches the pump has taken
	wireEpoch uint64       // of the format list the client holds for tab
	holding   *deltaRec    // the entry the pump sends (deliverOne)

	mu       sync.Mutex
	notes    []string
	presence presenceState
	stale    bool // presence of others changed

	peeks, presences rate

	// A watch that found the capture budget full waits here. The sender
	// goroutine tries it again when a capture gives room back.
	want      *connCmd
	freed     <-chan struct{}
	freedDone func()
	retryAt   time.Time
	waiting   bool // the pending watch waits for the capture budget
	told      bool // the calculating status the client has (tell)

	amu      sync.Mutex
	assembly map[uint64]*assembly
}

type rate struct {
	window time.Time
	count  int
}

func (r *rate) allow(now time.Time, perSecond int) bool {
	if now.Sub(r.window) >= time.Second {
		r.window, r.count = now, 0
	}
	r.count++
	return r.count <= perSecond
}

type assembly struct {
	parts []string
	have  int
	bytes int64
	timer interface{ Stop() bool }
}

// maxAssemblies is how many actions one connection may have in assembly.
// The client sends the parts of one action before the next.
const maxAssemblies = 4

// assemblyTimer runs fn when an assembly's lifetime ends. Tests replace it
// to end a lifetime without waiting.
var assemblyTimer = func(d time.Duration, fn func()) interface{ Stop() bool } { return time.AfterFunc(d, fn) }

func (c *conn) poke() {
	select {
	case c.wakeCh <- struct{}{}:
	default:
	}
}

func (c *conn) note(frame string) {
	c.mu.Lock()
	if len(c.notes) < 256 {
		c.notes = append(c.notes, frame)
	}
	c.mu.Unlock()
	c.poke()
}

// probes lets a test stop a connection or a read at a named step, to
// force an interleaving. It is nil outside tests.
var probes atomic.Pointer[func(in *Instance, step string)]

func (in *Instance) probe(step string) {
	if fn := probes.Load(); fn != nil {
		(*fn)(in, step)
	}
}

// fatal ends the connection at once with a fatal frame. It is for the time
// before the pump runs and for the pump itself; everyone else uses endWith.
func (c *conn) fatal(code string) {
	c.ws.Send(envelope("fatal", "code", code))
	c.ws.Close(1008, code)
}

// endWith queues the end of the connection: the pump sends the fatal frame
// after everything that was queued before it and closes. From then on the
// reader takes no frame.
func (c *conn) endWith(code string) {
	c.ending.Store(true)
	c.enqueue(connCmd{fatal: code})
}

// room waits until the send queue can take n more bytes. It returns false
// when the connection ends first.
func (c *conn) room(n int) bool {
	limit := c.ws.Budget().Limit()
	deadline := time.Now().Add(drainWait)
	for {
		if r := c.holding; r != nil && r.revoked.Load() {
			return false
		}
		used := c.ws.Budget().Used()
		if used == 0 {
			c.burst = 0
		}
		if c.burst < sendBurst && used+int64(n) <= limit-(64<<10) {
			return true
		}
		if used == 0 && c.burst < sendBurst {
			return true // a frame larger than the queue goes out alone
		}
		if time.Now().After(deadline) {
			c.ws.Close(1008, "lagging")
			return false
		}
		select {
		case <-c.ctx.Done():
			return false
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (c *conn) send(frame string) bool {
	c.burst++
	return c.ws.Send(frame) == nil
}

// tell sends the calculating status when it changed for the client. The
// status is set while the sheet calculates without an edit and while a
// watch of the connection waits for the capture budget. Only the pump
// calls it, so a status takes its place among the frames of the connection
// and none is lost to a full queue of notes.
func (c *conn) tell() bool {
	on := c.waiting || c.in.hub.calc.Load()
	if on == c.told {
		return true
	}
	if !c.room(64) || !c.send(envelope("status", "calculating", on)) {
		return false
	}
	c.told = on
	return true
}

// sendParts sends the frame of fields, which passes the frame limit, as
// delta_part frames. Each part is cut from the entry's fields when the
// queue has room for it, so the connection holds no more of the delta
// than the part it fills. Under the sheet's limits every delta can be
// cut; one that cannot ends the connection rather than reach a client
// whole.
func (c *conn) sendParts(rec *deltaRec, fields []deltaField) bool {
	count, err := c.in.hub.partCount(c.ctx, rec, frameKey{c.tab, c.public}, fields)
	if err != nil {
		if c.ctx.Err() == nil {
			c.ws.Close(1011, "delta")
		}
		return false
	}
	seq := decimal(rec.seq)
	stopped := false
	n, err := splitDelta(fields, func(i int, text []byte) bool {
		if i >= count {
			return false
		}
		head := partHead(seq, i, count)
		if !c.room(len(head) + len(text)) {
			stopped = true
			return false
		}
		var b strings.Builder
		b.Grow(len(head) + len(text))
		b.WriteString(head)
		b.Write(text)
		if !c.send(b.String()) {
			stopped = true
			return false
		}
		return true
	})
	if stopped {
		return false
	}
	if err != nil || n != count {
		c.ws.Close(1011, "delta")
		return false
	}
	return true
}

// flow tells how a delivery of deltas ended.
type flow int

const (
	flowing  flow = iota // everything due is queued
	behind               // the queue or the log cannot carry the next delta
	restyled             // the next delta needs a newer format list
	ended                // the connection ended
)

// deliver sends the deltas after the connection's sequence, through limit
// when it is not zero. It reports why it stopped and recovers nothing: its
// callers do (rule 3).
func (c *conn) deliver(limit uint64) flow {
	h := c.in.hub
	for {
		rec, epoch, ok := h.next(c.seq)
		if !ok {
			return behind
		}
		if rec == nil {
			// Every delta of the list the client holds is out. When the
			// list was rebuilt since, the client moves to the new one now
			// and not at its next delta, so it lets go of the old list
			// (section 12.2).
			if limit == 0 && c.tab != 0 && epoch != c.wireEpoch {
				return restyled
			}
			return flowing
		}
		how, more := c.deliverOne(rec, limit)
		h.release(rec)
		if !more {
			return how
		}
	}
}

// deliverOne sends one entry of the log. It reports whether delivery goes
// on after it, and else how it ended.
func (c *conn) deliverOne(rec *deltaRec, limit uint64) (flow, bool) {
	if limit != 0 && rec.seq > limit {
		return flowing, false
	}
	c.holding = rec
	defer func() { c.holding = nil }()
	// A revoked entry is not sent on: the client resyncs instead.
	lost := func() (flow, bool) {
		if rec.revoked.Load() {
			return behind, false
		}
		return ended, false
	}
	if rec.reset {
		if !c.room(64) || !c.send(envelope("reset", "seq", decimal(rec.seq))) {
			return lost()
		}
		c.seq, c.tab = rec.seq, 0
		return flowing, true
	}
	if c.tab != 0 && rec.wireEpoch != c.wireEpoch {
		return restyled, false
	}
	fields := rec.fields(c.tab, c.public)
	size := frameSize(rec.seq, fields)
	if used := c.ws.Budget().Used(); used+int64(size) > c.ws.Budget().Limit()-(64<<10) && used > 0 {
		return behind, false
	}
	if rec.revoked.Load() || c.burst >= sendBurst && !c.room(size) {
		return lost()
	}
	if size <= serverFrameMax {
		if !c.send(c.in.hub.frame(rec, c.tab, c.public, fields)) {
			return lost()
		}
	} else if !c.sendParts(rec, fields) {
		return lost()
	}
	c.seq = rec.seq
	return flowing, true
}

// flush delivers the deltas that are due. A connection that falls behind
// its queue, or behind the log, gets one resync with a new snapshot and is
// closed if it lags again (section 12.3). A connection whose format list
// was replaced gets a resync that does not count as lagging.
func (c *conn) flush(limit uint64) bool {
	switch c.deliver(limit) {
	case flowing:
		return true
	case behind:
		return c.resync(true)
	case restyled:
		return c.resync(false)
	}
	return false
}

// resync cuts the client off and sends it the tab it is to have: the tab a
// pending watch asked for, else the tab it watched. When the client has
// sent a watch that the pump has not reached yet, that watch brings the
// tab, and nothing is sent for the one the client is leaving.
func (c *conn) resync(lagged bool) bool {
	tab, above := c.tab, c.gen
	if c.want != nil {
		tab, above = c.want.tab, max(above, c.want.generation)
	}
	if !c.cutOff(lagged, above) {
		return false
	}
	if c.want != nil {
		c.want.generation = c.gen
	}
	if tab == 0 || c.asked.Load() > c.served {
		return true
	}
	return c.snapshot(c.gen, tab)
}

// cutOff tells the client that the state it holds is not continued: a
// resync frame with a generation above the connection's and above the one
// named, and the sequence the stream goes on from. That is the newest
// sequence, with the tab list and the date as of it. The client may have
// taken deltas without applying them, held for its editor, and it drops
// them at a resync, so whatever they changed of the tab list and the date
// reaches it here, also when the connection skipped nothing. Only a
// lagging connection counts the resync, and it gets one. When the deltas
// it skips hold a rollback the client gets the reset instead, as it would
// have at that delta, and starts over. Until the next snapshot is out the
// connection watches no tab, so it gets no cells of a tab it lost track
// of.
func (c *conn) cutOff(lagged bool, above int) bool {
	if lagged {
		if c.resynced {
			c.ws.Close(1008, "lagging")
			return false
		}
		c.resynced = true
	}
	if !c.room(64) {
		return false
	}
	// The log grows under the write lock, so the newest sequence and the
	// workbook read under the read lock belong together.
	c.in.mu.RLock()
	c.in.hub.mu.Lock()
	seq := c.in.hub.last
	rolledBack := c.in.hub.resetAt > c.seq
	c.in.hub.mu.Unlock()
	date, tabs := c.in.wb.date.String(), appendTabInfo(nil, c.in.wb)
	c.in.mu.RUnlock()
	c.intent++
	c.gen = max(c.gen, above) + 1
	frame := envelope("resync", "generation", c.gen, "seq", decimal(seq), "date", date, "tabs", json.RawMessage(tabs))
	if rolledBack {
		frame = envelope("reset", "seq", decimal(seq))
	}
	if !c.room(len(frame)) || !c.send(frame) {
		return false
	}
	c.seq, c.tab = seq, 0
	return true
}

// snapshot sends a tab as tab_head, rows frames and tab_done. Deltas up to
// the snapshot's sequence go first, so nothing older than the snapshot
// follows it, and the head carries the tab list and the date as of that
// sequence (captureTab). A tab that is gone gives way to the first tab
// (openSnapshot). When the capture budget is full the watch stays pending:
// the client is told to wait, and run tries again when a capture gives
// room back.
func (c *conn) snapshot(generation int, tab TabID) bool {
	for {
		c.intent++
		mine := c.intent
		if c.public {
			for !c.in.hub.admitPublicStart(publicStartNow()) {
				if !publicStartPause(c.ctx) {
					return false
				}
			}
		}
		c.stopWaiting()
		freed, done := awaitCaptureRoom()
		src, err := c.in.openSnapshot(tab, c.public, c.epoch)
		if err != nil {
			c.want, c.freed, c.freedDone = &connCmd{generation: generation, tab: tab}, freed, done
			// Two readers that refuse each other free no bytes, so the
			// watch is also tried again after a while, at a time of its own.
			c.retryAt = time.Now().Add(time.Second + time.Duration(rand.Int64N(int64(time.Second))))
			c.waiting = true
			return c.tell()
		}
		done()
		c.waiting = false
		if !c.tell() {
			src.close(c.in.hub)
			return false
		}
		c.in.probe("snapshot_captured")
		how := flowing
		if src.seq > c.seq {
			how = c.deliver(src.seq)
		}
		if how == flowing {
			c.gen, c.tab, c.wireEpoch = generation, src.tab, src.wireEpoch
			ok := c.sendSnapshot(src, generation, mine)
			src.close(c.in.hub)
			return ok
		}
		// The deltas before the snapshot did not all go out, so the client
		// is cut off at a state this capture does not continue. The
		// capture is dropped and the tab is captured again (rule 3).
		src.close(c.in.hub)
		if how == ended || !c.cutOff(how == behind, generation) {
			return false
		}
		generation = c.gen
	}
}

// sendSnapshot puts the frames of a snapshot on the wire: the frames of a
// cached entry, or the frames its own capture encodes, one at a time as
// the send queue takes them. The first connection that encodes a snapshot
// collects the frames for the cache while the budgets have room.
func (c *conn) sendSnapshot(src *snapSource, generation int, mine uint64) bool {
	if c.intent != mine {
		return true // superseded: rule 2
	}
	head := `,"generation":` + strconv.Itoa(generation) + ","
	if src.entry != nil {
		for i, rest := range src.entry.frames {
			if !c.room(len(rest)+64) || !c.send(`{"type":"`+src.entry.kinds[i]+`"`+head+rest) {
				return false
			}
		}
		return true
	}
	c.in.probe("snapshot_stream")
	entry := &snapEntry{seq: src.seq, wireEpoch: src.wireEpoch}
	ok := src.enc.run(func(kind string, rest []byte) bool {
		if !c.room(len(rest) + 64) {
			return false
		}
		if entry != nil {
			if kept, fits := entry.keep(kind, rest); fits {
				return c.send(`{"type":"` + kind + `"` + head + kept)
			}
			entry.giveUp()
			entry = nil
		}
		return c.send(`{"type":"` + kind + `"` + head + string(rest))
	})
	switch {
	case entry == nil:
	case ok:
		c.in.hub.cache(src.key, entry)
	default:
		entry.giveUp()
	}
	return ok
}

// stopWaiting forgets the pending watch. The client keeps its waiting
// state until the next snapshot attempt settles it.
func (c *conn) stopWaiting() {
	if c.want != nil {
		c.freedDone()
		c.want, c.freed, c.freedDone = nil, nil, nil
	}
}

func (c *conn) run() {
	defer c.stopWaiting()
	for {
		var again <-chan time.Time
		if c.want != nil {
			again = time.After(time.Until(c.retryAt))
		}
		retry := false
		select {
		case <-c.ctx.Done():
			return
		case cmd := <-c.cmds:
			c.in.probe("pump")
			switch {
			case cmd.fatal != "":
				c.fatal(cmd.fatal)
				return
			case cmd.watch:
				c.served++
				if !c.snapshot(cmd.generation, cmd.tab) {
					return
				}
			default:
				if cmd.ackSeq != 0 && !c.flush(cmd.ackSeq) {
					return
				}
				if !c.room(len(cmd.frame)) || !c.send(cmd.frame) {
					return
				}
			}
		case <-c.wakeCh:
		case <-c.freed:
			retry = true
		case <-again:
			retry = true
		}
		if want := c.want; retry && want != nil && !c.snapshot(want.generation, want.tab) {
			return
		}
		if !c.flush(0) || !c.tell() {
			return
		}
		c.mu.Lock()
		notes := c.notes
		c.notes = nil
		stale := c.stale
		c.stale = false
		c.mu.Unlock()
		for _, frame := range notes {
			if !c.room(len(frame)) || !c.send(frame) {
				return
			}
		}
		if stale && !c.public {
			frame := c.in.presenceFrame(c)
			if !c.room(len(frame)) || !c.send(frame) {
				return
			}
		}
	}
}

func (c *conn) enqueue(cmd connCmd) bool {
	select {
	case c.cmds <- cmd:
		return true
	case <-c.ctx.Done():
		return false
	}
}

func accessPrivate(r *http.Request) bool {
	values := r.Header.Values("X-Flats-Access")
	return len(values) == 1 && values[0] == "private"
}

// serveLive upgrades a connection and runs it until it ends.
func (in *Instance) serveLive(w http.ResponseWriter, r *http.Request, public bool) {
	if !wsconn.IsUpgrade(r) {
		http.Error(w, "WebSocket upgrade required", http.StatusBadRequest)
		return
	}
	in.admit.RLock()
	if in.ctx.Err() != nil {
		in.admit.RUnlock()
		writeError(w, unavailable("stopped"))
		return
	}
	in.conns.Add(1)
	in.admit.RUnlock()
	defer in.conns.Done()
	h := in.hub
	h.mu.Lock()
	exposure := h.epoch
	count, limit := h.private, maxPrivateConns
	if public {
		count, limit = h.public, maxPublicConns
	}
	if count >= limit {
		h.mu.Unlock()
		writeError(w, unavailable("connections"))
		return
	}
	if public {
		h.public++
	} else {
		h.private++
	}
	h.mu.Unlock()
	connections.Add(1)
	defer func() {
		connections.Add(-1)
		h.mu.Lock()
		if public {
			h.public--
		} else {
			h.private--
		}
		h.mu.Unlock()
	}()
	opts := wsconn.Options{QueuedBytes: privateQueue, PingInterval: livePing}
	if public {
		opts = wsconn.Options{QueuedBytes: publicQueue, Shared: publicSendBudget, PingInterval: livePing}
	}
	ws, err := wsconn.Upgrade(w, r, opts)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(in.ctx)
	c := &conn{in: in, ws: ws, ctx: ctx, cancel: cancel, public: public, epoch: r.Header.Get("X-Flats-Access-Epoch"),
		wakeCh: make(chan struct{}, 1), cmds: make(chan connCmd, 16), assembly: map[uint64]*assembly{}}
	defer func() {
		cancel()
		ws.Close(1000, "")
		c.amu.Lock()
		for _, a := range c.assembly {
			a.timer.Stop()
			assemblyBytes.Add(-a.bytes)
		}
		c.assembly = nil
		c.amu.Unlock()
	}()
	// Core cancels the request when the route's access changes.
	stopRoute := context.AfterFunc(r.Context(), func() {
		if public {
			ws.Abort()
		} else {
			ws.Close(1008, "access_changed")
		}
		cancel()
	})
	defer stopRoute()
	stopLife := context.AfterFunc(ctx, func() { ws.Close(1001, "unavailable") })
	defer stopLife()

	_ = ws.NetConn().SetReadDeadline(time.Now().Add(10 * time.Second))
	raw, err := ws.ReadMessage(clientFrameMax)
	if err != nil {
		return
	}
	_ = ws.NetConn().SetReadDeadline(time.Time{})
	// The hello is read once, by the walk that reads every frame.
	var hello helloFrame
	if err := hello.UnmarshalJSON([]byte(raw)); err != nil || hello.Type != "hello" || hello.Version != protocolVersion || hello.Pending < 0 {
		c.fatal("protocol")
		return
	}
	// A browser keeps the name a person gave on a Private route and sends
	// it whatever the route is by then. A Public connection opens no
	// session, so the name is not read. A session, a watermark and pending
	// actions come only from a page that was open on a Private route.
	if public && (hello.Session != "" || hello.Pending != 0 || hello.Watermark != "") {
		c.fatal("forbidden")
		return
	}
	// Nothing reads the connection while it waits for the load, for its
	// session or, once admitted, for the answer to an action. A page that
	// leaves meanwhile is found by the pings of the connection, and what
	// the connection waits for ends with it, except work the writer has
	// taken.
	go func() {
		select {
		case <-ws.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	select {
	case <-in.ready:
	default:
		ws.Send(envelope("status", "calculating", true))
		if in.await(ctx, 0) != nil {
			return
		}
		ws.Send(envelope("status", "calculating", false))
	}
	if in.loadErr != nil {
		c.fatal("unavailable")
		return
	}
	token := ""
	if !public {
		// The writer stores and finds sessions, and a date change or a
		// rollback keeps it for as long as it calculates. The page is told
		// meanwhile, as during a load.
		h.awaitSession(c)
		defer h.sessionSettled(c)
		switch {
		case hello.Session != "":
			c.s, err = in.resumeSession(ctx, hello.Session)
			token = hello.Session
			// A session without a row that left memory keeps no receipt. A
			// page with nothing pending opens a new session, as for one that
			// expired.
			if werr := asWriteError(err); err != nil && werr.Reason == "receipt_expired" && hello.Pending == 0 {
				err = &WriteError{Code: "conflict", Reason: "session_expired"}
			}
		case hello.Pending > 0:
			err = &WriteError{Code: "conflict", Reason: "session_expired"}
		default:
			c.s, token, err = in.openSession(ctx, r, hello.Name)
		}
		if err != nil {
			c.fatal(asWriteError(err).reason())
			return
		}
	}
	// Admit the connection under the epoch it was accepted in. A Public
	// connection that an access change overtook gets nothing.
	h.mu.Lock()
	if public && exposure != h.epoch {
		h.mu.Unlock()
		ws.Abort()
		return
	}
	delete(h.early, c)
	h.conns[c] = struct{}{}
	h.mu.Unlock()
	if c.s != nil {
		if old := in.attach(c.s, c); old != nil {
			old.ws.Close(1008, "session_moved")
			old.cancel()
		}
	}
	defer func() {
		h.mu.Lock()
		delete(h.conns, c)
		h.mu.Unlock()
		if c.s != nil {
			in.detach(c)
			in.presenceChanged(c)
		}
	}()

	in.mu.RLock()
	wb := in.wb
	seq := wb.seq
	date := wb.date.String()
	tabs := json.RawMessage(appendTabInfo(nil, wb))
	in.mu.RUnlock()
	c.seq = seq
	// A resumed connection whose watermark the log still reaches continues
	// from it: welcome names the watermark and the deltas after it follow.
	// The tab list and the date are known only as they are now, so the
	// welcome leaves them out. The client keeps those of its watermark and
	// the replayed deltas bring each change in its place.
	resumed, resnap := false, false
	if mark, ok := parseDecimal(hello.Watermark); ok && c.s != nil && hello.Tab != 0 {
		if epoch, ok := h.resumes(mark); ok && mark <= seq {
			c.seq, c.tab, c.wireEpoch = mark, TabID(hello.Tab), epoch
			resumed = true
		} else {
			// The log no longer reaches back: the tab comes as a new
			// snapshot, unasked.
			resnap = true
		}
	}
	fields := []any{"access", "private", "seq", decimal(c.seq)}
	if !resumed {
		fields = append(fields, "date", date, "tabs", tabs)
	}
	if public {
		fields[1] = "public"
	} else {
		me := map[string]any{"verified": c.s.verified}
		if c.s.label != "" {
			me["label"] = c.s.label
		}
		fields = append(fields, "session", token, "session_id", c.s.id, "me", me)
	}
	if ws.Send(envelope("welcome", fields...)) != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.run()
		cancel()
	}()
	if resnap {
		c.asked.Add(1)
		c.enqueue(connCmd{watch: true, tab: TabID(hello.Tab)})
	}
	c.poke()
	if !public {
		in.presenceChanged(nil)
	}
	if c.read() {
		// The pump ends the connection once it has sent what was queued.
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	cancel()
	<-done
}

// openSession creates the session of a new connection. Only the core
// route's decision supplies a verified identity (spec rule 26).
func (in *Instance) openSession(ctx context.Context, r *http.Request, name string) (*session, string, error) {
	editor, err := randomToken()
	if err != nil {
		return nil, "", err
	}
	// A session without a name has no label: its welcome carries none and
	// the client asks the person for a name.
	label := name
	login := strings.TrimSpace(r.Header.Get("Tailscale-User-Login"))
	verified := login != ""
	if verified {
		editor = "tailscale:" + login
		if label = r.Header.Get("Tailscale-User-Name"); label == "" {
			label = login
		}
	}
	return in.newSession(ctx, editor, label, verified)
}

// read handles the client's frames until the connection ends. It reports
// true when it queued the end of the connection itself: the pump then
// closes after the answers that were decided before (rule 1).
func (c *conn) read() bool {
	for {
		raw, err := c.ws.ReadMessage(clientFrameMax)
		if err != nil {
			return false
		}
		if c.ending.Load() {
			return true // nothing is taken after the end was decided
		}
		// The frame is read once. A frame that is none of Appendix A is a
		// protocol error on every connection, whatever it names as its
		// type; what a connection may send is decided from the frame
		// that was read.
		f, err := readConnFrame([]byte(raw))
		if err != nil {
			c.endWith("protocol")
			return true
		}
		if c.public && f.kind != "watch" && f.kind != "peek" {
			// The Public projection is an allow-list (section 17).
			c.endWith("forbidden")
			return true
		}
		var ok bool
		switch f.kind {
		case "watch":
			c.asked.Add(1)
			ok = c.enqueue(connCmd{watch: true, generation: f.watch.Generation, tab: TabID(f.watch.Tab)})
		case "peek":
			ok = c.peek(&f.peek)
		case "action":
			ok = c.action(&f.action)
		case "action_part":
			ok = c.actionPart(&f.part)
		case "rebase":
			ok = c.rebase(&f.rebase)
		case "presence":
			ok = c.setPresence(&f.presence)
		}
		if !ok {
			if c.ctx.Err() != nil {
				return false
			}
			c.endWith("protocol")
			return true
		}
		if c.ending.Load() {
			return true
		}
	}
}

// action submits an action and queues its ack behind the action's delta.
func (c *conn) action(f *actionFrame) bool {
	n, okN := parseDecimal(f.N)
	base, okBase := parseDecimal(f.Base)
	if !okN || !okBase || n == 0 {
		return false
	}
	ops, _ := json.Marshal(f.Ops)
	sum := sha256.Sum256(append([]byte(f.Base+"\n"), ops...))
	ack, err := c.in.submit(c.ctx, c.s, n, base, f.Ops, sum[:])
	if err != nil {
		if c.ctx.Err() != nil {
			return false // the connection ended while the action waited
		}
		werr := asWriteError(err)
		if werr.Code == "invalid" {
			return false
		}
		// The action got no answer on this connection. The end goes
		// through the pump, behind the acks of the actions before it.
		c.endWith(werr.reason())
		return true
	}
	var body struct {
		Seq string `json:"seq"`
	}
	_ = json.Unmarshal(ack, &body)
	seq, _ := parseDecimal(body.Seq)
	return c.enqueue(connCmd{frame: string(ack), ackSeq: seq})
}

// actionPart assembles an action that is larger than one frame: at most 8
// MiB in 32 parts within 30 seconds, under a host-wide budget.
func (c *conn) actionPart(f *actionPartFrame) bool {
	if f.Parts < 1 || f.Parts > actionMaxParts || f.Part < 0 || f.Part >= f.Parts {
		return false
	}
	n, ok := parseDecimal(f.N)
	if !ok {
		return false
	}
	whole, ok := c.addPart(n, f)
	if !ok || whole == "" {
		return ok
	}
	// The parts hold the text of one action, which is read like a frame.
	var action actionFrame
	if action.UnmarshalJSON([]byte(whole)) != nil || action.N != f.N {
		return false
	}
	return c.action(&action)
}

// addPart files one part and returns the action's text once it is whole.
// The first part of an action starts its lifetime.
func (c *conn) addPart(n uint64, f *actionPartFrame) (whole string, ok bool) {
	c.amu.Lock()
	defer c.amu.Unlock()
	if c.assembly == nil {
		return "", false // the connection has ended
	}
	a := c.assembly[n]
	if a == nil {
		if len(c.assembly) >= maxAssemblies {
			return "", false
		}
		a = &assembly{parts: make([]string, f.Parts)}
		a.timer = assemblyTimer(actionPartTTL, func() { c.expireAssembly(n, a) })
		c.assembly[n] = a
	}
	if len(a.parts) != f.Parts || a.parts[f.Part] != "" || f.Data == "" {
		return "", false
	}
	size := int64(len(f.Data))
	if a.bytes+size > actionMaxBytes || assemblyBytes.Add(size) > assemblyBudget {
		assemblyBytes.Add(-size)
		return "", false
	}
	a.parts[f.Part], a.bytes, a.have = f.Data, a.bytes+size, a.have+1
	if a.have < f.Parts {
		return "", true
	}
	a.timer.Stop()
	delete(c.assembly, n)
	assemblyBytes.Add(-a.bytes)
	return strings.Join(a.parts, ""), true
}

// expireAssembly ends an assembly whose lifetime passed without its last
// part. Its bytes go back to the host-wide budget at once, whether or not
// the connection sends anything more. The connection ends as unavailable:
// the client connects again and sends the whole action again.
func (c *conn) expireAssembly(n uint64, a *assembly) {
	c.amu.Lock()
	if c.assembly[n] != a {
		c.amu.Unlock()
		return
	}
	delete(c.assembly, n)
	assemblyBytes.Add(-a.bytes)
	c.amu.Unlock()
	c.endWith("unavailable")
}

// peek answers with the content of one cell as it would be typed: the A1
// text of a formula (section 12.4).
func (c *conn) peek(f *peekFrame) bool {
	if !c.peeks.allow(time.Now(), 60) {
		return false
	}
	in := c.in
	in.mu.RLock()
	wb := in.wb
	edit, isFormula := "", false
	if t := wb.byID[TabID(f.Tab)]; t != nil {
		if cell := t.cell(RowID(f.Row), ColID(f.Col)); cell != nil {
			pos, _ := t.posOf(RowID(f.Row), ColID(f.Col))
			if cell.f != nil {
				edit, isFormula = cell.f.Text(pos), true
			} else if !cell.blank() {
				edit = formula.EditText(cell.content, pos)
			}
		}
	}
	seq := wb.seq
	in.mu.RUnlock()
	return c.enqueue(connCmd{frame: envelope("peeked", "id", f.ID, "seq", decimal(seq), "edit", edit, "formula", isFormula)})
}

// rebase adjusts formula text an editor holds from its base through a held
// delta, for the exchange of section 8.4.
func (c *conn) rebase(f *rebaseFrame) bool {
	base, okBase := parseDecimal(f.Base)
	through, okThrough := parseDecimal(f.Through)
	if !okBase || !okThrough || through < base {
		return false
	}
	in := c.in
	in.mu.RLock()
	wb := in.wb
	refused := ""
	text, at := f.Text, f.At
	t := wb.byID[TabID(f.Tab)]
	switch {
	case through > wb.seq:
		refused = "unknown_sequence"
	case t == nil:
		refused = "gone"
	case len(text) > 1 && text[0] == '=':
		x := newTxn(wb, wb.seq, base, 0)
		x.log = &in.structs
		nf, pos, err := x.adjust(t, RowID(f.Row), text, formula.Pos{Row: at[0], Col: at[1]}, through)
		if err != nil {
			refused = err.reason()
		} else {
			text, at = nf.Text(pos), pair[int32]{pos.Row, pos.Col}
		}
	}
	in.mu.RUnlock()
	if refused != "" {
		return c.enqueue(connCmd{frame: envelope("rebased", "id", f.ID, "refused", refused)})
	}
	return c.enqueue(connCmd{frame: envelope("rebased", "id", f.ID, "text", text, "at", at, "base", f.Through)})
}
