package board

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/board/model"
	"github.com/gosuda/flats/internal/wsconn"
)

// Protocol limits (6, 12).
const (
	maxClientFrame  = 256 << 10
	maxServerFrame  = 512 << 10
	partDataMax     = 192 << 10
	partsMax        = 32
	assemblyMax     = 8 << 20
	hostAssemblyMax = 64 << 20
	privateQueueMax = 4 << 20
	publicQueueMax  = 1 << 20
	hostPublicMax   = 256 << 20
	feedBelow       = 1 << 20 // queue the next frame only below this many bytes in flight
	wsQueuedBytes   = 2 << 20
	publicWait      = 10 * time.Second
	guestShareMax   = 64 << 20  // guests' share of hostPublicMax
	guestFeedBelow  = 256 << 10 // feedBelow for a guest's socket
)

var (
	hostAssembly atomic.Int64
	publicQueued atomic.Int64 // bytes waiting in Public connections' queues and snapshot parts, host-wide
	publicSnaps  atomic.Int64 // retained snapshot states Public connections send from, host-wide
	// publicMu makes admission to the host's combined Public budget atomic
	// (reservePublic).
	publicMu sync.Mutex
	// Limits tests lower.
	assemblyTimeout = 30 * time.Second
	hostPublicCap   = int64(hostPublicMax)
	publicBudget    = func() *wsconn.Budget {
		b := &wsconn.Budget{}
		b.Lower(hostPublicMax)
		return b
	}()
	// guestShare counts the part of publicQueued and publicSnaps that
	// guests' sockets use, at most guestShareCap (TECH public edit 4.2).
	guestShare    atomic.Int64
	guestShareCap = int64(guestShareMax)
)

// Fatal codes the instance sends besides the engine's (4.8).
const (
	FatalDeleted     = "deleted"
	FatalRenamed     = "renamed"
	FatalTypeChanged = "type_changed"
)

var peerColors = []string{"#b4441c", "#0b7a55", "#1f5fbf", "#7a3bb5", "#b3264f", "#0d7480", "#8a6d0b", "#4a47b8"}

func colorOf(key string) string {
	h := fnv.New32a()
	h.Write([]byte(key))
	return peerColors[h.Sum32()%uint32(len(peerColors))]
}

// hub holds the board's open sockets and their pools (6), the connection
// labels of the editors on the board and the viewers' numbers (TECH public
// edit 3.3, 3.4).
type hub struct {
	mu sync.Mutex
	// all holds every accepted socket, welcomed or not, so closeAll reaches
	// it; conns holds the welcomed ones (register).
	all    map[*conn]struct{}
	conns  map[*conn]struct{}
	keys   map[string]int // joined sockets per editor key, members and guests
	labels map[string]int // editor key -> N of its label p:N, while it has a socket
	priv   int            // admitted Private sockets
	pub    int            // admitted Public sockets, guests and viewers
	guests int            // admitted guest sockets
	// viewers maps each registered viewer socket to its number; tokens
	// keeps each viewer token's number while a socket uses it and for a
	// minute after the last one closed.
	viewers map[*conn]int
	tokens  map[string]*viewerToken
	// departing holds the labels whose key left until its gone frame is
	// queued, so no other key takes one before (relayGone).
	departing map[int]bool
	// counts for PublicPresence, which must not wait for mu
	guestsNow, viewersNow atomic.Int64
	// stopped is set by closeAll with its fatal code.
	stopped  bool
	stopCode string
}

type viewerToken struct {
	n     int
	refs  int
	until time.Time // held until then once refs is 0
}

// viewerHold is how long a viewer's number stays reserved for its token
// after its socket closed (spec rule 9). Tests shorten it.
var viewerHold = time.Minute

// registerHook, when a test sets it, runs at the start of each register.
var registerHook atomic.Pointer[func()]

func newHub() *hub {
	return &hub{all: map[*conn]struct{}{}, conns: map[*conn]struct{}{}, keys: map[string]int{}, labels: map[string]int{},
		viewers: map[*conn]int{}, tokens: map[string]*viewerToken{}, departing: map[int]bool{}}
}

func (h *hub) admit(public bool, privMax, pubMax int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if public {
		if h.pub >= pubMax {
			return false
		}
		h.pub++
		hostStats.conns.Add(1)
		return true
	}
	if h.priv >= privMax {
		return false
	}
	h.priv++
	hostStats.conns.Add(1)
	return true
}

// admitGuest counts an admitted Public socket as a guest unless max guest
// sockets are on the board.
func (h *hub) admitGuest(c *conn, max int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.guests >= max {
		return false
	}
	h.guests++
	c.guestSlot = true
	return true
}

// join counts a member's or guest's socket for its editor key, giving the
// key a connection label when it is the key's first socket: the lowest N
// no other key holds and no departing key still reserves.
func (h *hub) join(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.keys[c.key] == 0 {
		used := maps.Clone(h.departing)
		for _, n := range h.labels {
			used[n] = true
		}
		n := 1
		for used[n] {
			n++
		}
		h.labels[c.key] = n
	}
	h.keys[c.key]++
	c.joined = true
	c.plabel = "p:" + strconv.Itoa(h.labels[c.key])
	c.plabelN = h.labels[c.key]
}

// leave ends a socket's admission. lastOfKey reports that it was the last
// socket of its editor key; its label stays reserved until relayGone has
// queued the gone frame.
func (h *hub) leave(c *conn) (lastOfKey bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	hostStats.conns.Add(-1)
	if c.public {
		h.pub--
	} else {
		h.priv--
	}
	if c.guestSlot {
		h.guests--
	}
	delete(h.all, c)
	if _, ok := h.conns[c]; ok {
		delete(h.conns, c)
		if c.guest {
			h.guestsNow.Add(-1)
		}
	}
	if n, ok := h.viewers[c]; ok {
		delete(h.viewers, c)
		h.viewersNow.Add(-1)
		if t := h.tokens[c.viewerToken]; t != nil && t.n == n {
			if t.refs--; t.refs <= 0 {
				t.until = time.Now().Add(viewerHold)
			}
		}
	}
	if !c.joined {
		return false
	}
	h.keys[c.key]--
	if h.keys[c.key] <= 0 {
		delete(h.keys, c.key)
		delete(h.labels, c.key)
		h.departing[c.plabelN] = true
		return true
	}
	return false
}

// gone frees a departed key's label once its gone frame is queued.
func (h *hub) gone(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.departing, c.plabelN)
}

// goneToMembers queues frame, c's departure, to the member sockets unless
// c's key has a socket again. It holds mu while it checks and queues, so
// join cannot add a socket of the key in between. Lock order: mu, then a
// socket's conn.mu; member sockets are Private and take no publicMu.
func (h *hub) goneToMembers(c *conn, frame []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.keys[c.key] > 0 {
		return
	}
	to := make([]*conn, 0, len(h.conns))
	for o := range h.conns {
		if !o.public {
			to = append(to, o)
		}
	}
	relayTo(to, c, frame, nil)
}

// viewerNumber gives a viewer socket its number: the one its token holds,
// or for an unknown token a new token and the lowest number no connected
// viewer or held token has (spec rule 9).
func (h *hub) viewerNumber(c *conn, token string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for tok, t := range h.tokens {
		if t.refs <= 0 && now.After(t.until) {
			delete(h.tokens, tok)
		}
	}
	t := h.tokens[token]
	if t == nil || !validToken(token) {
		used := map[int]bool{}
		for _, t := range h.tokens {
			used[t.n] = true
		}
		n := 1
		for used[n] {
			n++
		}
		token = randomToken()
		t = &viewerToken{n: n}
		h.tokens[token] = t
	}
	t.refs++
	c.viewerToken, c.viewerN = token, t.n
}

// accept adds a new socket before its hello. On a stopped board it is
// stopped at once instead, as closeAll would have.
func (h *hub) accept(c *conn) {
	h.mu.Lock()
	h.all[c] = struct{}{}
	stopped, code := h.stopped, h.stopCode
	h.mu.Unlock()
	if stopped {
		stopConn(c, code)
	}
}

// register adds a welcomed socket.
func (h *hub) register(c *conn) {
	if f := registerHook.Load(); f != nil {
		(*f)()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[c] = struct{}{}
	switch {
	case c.guest:
		h.guestsNow.Add(1)
	case c.public:
		h.viewers[c] = c.viewerN
		h.viewersNow.Add(1)
	}
}

// viewerList is the numbers of the registered viewers, sorted.
func (h *hub) viewerList() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]int, 0, len(h.viewers))
	for _, n := range h.viewers {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

func (h *hub) list() []*conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*conn, 0, len(h.conns))
	for c := range h.conns {
		out = append(out, c)
	}
	return out
}

func (h *hub) privateCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.priv
}

// closeAll stops every accepted socket, welcomed or still in its welcome,
// with the fatal code. It runs before Shutdown cancels the requests, whose
// sockets then close without one.
func (h *hub) closeAll(code string) {
	h.mu.Lock()
	h.stopped, h.stopCode = true, code
	all := make([]*conn, 0, len(h.all))
	for c := range h.all {
		all = append(all, c)
	}
	h.mu.Unlock()
	for _, c := range all {
		stopConn(c, code)
	}
}

// stopConn sends a stopping board's fatal code, if any, and closes the
// socket.
func stopConn(c *conn, code string) {
	if code != "" {
		b, _ := json.Marshal(map[string]string{"t": "fatal", "code": code})
		_ = c.ws.Send(string(b))
	}
	c.ws.Close(1000, "board stopped")
}

// conn is one WebSocket of the board. A Public socket (public) is a
// viewer, or a guest once its welcome made it one (TECH public edit 3.1).
type conn struct {
	in       *Instance
	ws       *wsconn.Conn
	public   bool
	guest    bool
	key      string
	name     string
	label    string
	color    string
	verified bool
	session  string // set by the apply loop at welcome
	// nick is a member's page nickname (hello.nick), the name guests see.
	nick string
	// epoch is the access epoch at a guest's welcome; a guest batch from
	// another epoch is refused (TECH public edit 6.1).
	epoch int64
	// joined: hub.join counted the socket for its key, whose connection
	// label is plabel (p:N); guestSlot: it holds one of the guest sockets.
	joined    bool
	plabel    string
	plabelN   int
	guestSlot bool
	// viewerToken and viewerN: a viewer's reconnect token and number.
	viewerToken string
	viewerN     int

	mu       sync.Mutex
	q        []outMsg
	qBytes   int64
	snaps    int // snapshot jobs queued
	gen      int64
	closing  bool
	wake     chan struct{}
	lastPres []byte
	// lastPresGuest is lastPres as guests see it (TECH public edit 3.3).
	lastPresGuest []byte
	// chargeGuest: frames queued from now on count toward the guests'
	// share of the host's Public budget. flight is the part of that share
	// for bytes the socket holds but has not written (pump-owned).
	chargeGuest bool
	flight      int64
	// overflowed: the queue went over its cap and the socket is closing,
	// or the pump stopped; no frame is queued any more.
	overflowed bool
	// handed is the seq the client's board reaches with the deltas and
	// snapshots the pump has handed to the socket (resync).
	handed int64

	asm       *assembly
	discarded int64 // the batch whose assembly was dropped as transient
	batches   rate
	pres      rate
}

// outMsg is a queued frame or snapshot. guest marks a frame charged to the
// guests' share of the host's Public budget as well.
type outMsg struct {
	frame []byte
	snap  *snapJob
	guest bool
	kind  frameKind
	seq   int64 // on the last frame of a delta: its seq
}

// frameKind tells resync what to do with a queued frame.
type frameKind uint8

const (
	frameOther frameKind = iota // kept in place
	frameDelta                  // a delta or delta part: the snapshot replaces it
	frameReply                  // an ack or reject: sent after the snapshot
)

// Snapshot audiences: what form a snapshot's elements take.
const (
	audPrivate = iota
	audPublic
	audGuest
)

type snapJob struct {
	gen, seq int64
	reset    bool
	aud      int
	data     *snapData
	done     sync.Once
}

// public reports whether the job sends to a Public socket, charged to the
// host's Public budget.
func (j *snapJob) public() bool { return j.aud != audPrivate }

// release gives up the job's hold on its snapshot state.
func (j *snapJob) release() {
	j.done.Do(func() {
		if j.public() {
			j.data.releasePublic(j.aud == audGuest)
		}
	})
}

// snapData is the board at one seq for snapshot transfers: the alive
// elements and the metadata, taken once in the apply loop and shared by
// every job of that seq (6 "Snapshot transfer"). Its elements share their
// immutable text and points with the state; parts are encoded one at a
// time while sending, so a job holds one part, never the whole board.
type snapData struct {
	seq  int64
	meta model.PrivateElement
	els  []model.PrivateElement
	cost int64 // estimated bytes this copy retains

	mu        sync.Mutex
	pubRefs   int // Public jobs holding it; they charge cost once host-wide
	guestRefs int // guest jobs holding it; they charge cost once to the guests' share
	ends      [3]struct {
		once sync.Once
		v    []int // end index of each part, per audience
	}
}

// snapElementCost estimates the bytes one copied element retains (the
// struct, its style and version maps) beyond the text and points it shares.
const snapElementCost = 1 << 10

// currentSnap returns the snapshot state of the current seq, taking it when
// the state changed since. Only the apply loop calls it.
func (in *Instance) currentSnap() *snapData {
	if d := in.snapCur; d != nil && d.seq == in.st.Seq() {
		return d
	}
	d := &snapData{seq: in.st.Seq(), meta: in.st.Board(), els: in.st.Alive()}
	d.cost = int64(len(d.els)+1) * snapElementCost
	in.snapCur = d
	return d
}

// dropStaleSnap forgets the snapshot state once the seq moved (a commit or
// a rolled-back transaction). A commit that leaves the seq unchanged (most
// ticks) keeps it, so viewers admitted across such commits share one copy
// of the board instead of each holding their own. Every change to the
// elements or the metadata takes a new seq. Only the apply loop calls it.
func (in *Instance) dropStaleSnap() {
	if d := in.snapCur; d != nil && d.seq != in.st.Seq() {
		in.snapCur = nil
	}
}

// reservePublic charges n bytes to counter (publicQueued or publicSnaps)
// when the host's combined Public total (6) still fits: queued frames and
// waiting snapshot parts, shared snapshot states, and the bytes in Public
// sockets (publicBudget). Every increase of the first two goes through
// here, under publicMu, so admissions across boards cannot both take the
// last room. publicBudget grows outside the lock only by frames that move
// out of publicQueued, which keep their charge there until the socket has
// taken them: a move counts twice for a moment, never zero times. Releases
// only lower the total and need no lock.
func reservePublic(counter *atomic.Int64, n int64) bool { return reserveShare(counter, n, 0) }

// reserveShare is reservePublic that also charges guestN bytes to the
// guests' share of the host's Public budget (guestShare), when that share
// stays within guestShareCap (TECH public edit 4.2).
func reserveShare(counter *atomic.Int64, n, guestN int64) bool {
	publicMu.Lock()
	defer publicMu.Unlock()
	if publicQueued.Load()+publicSnaps.Load()+publicBudget.Used()+n > hostPublicCap {
		return false
	}
	if guestN > 0 && guestShare.Load()+guestN > guestShareCap {
		return false
	}
	counter.Add(n)
	guestShare.Add(guestN)
	return true
}

// acquirePublic charges the state to the host's Public budget when no
// Public job held it yet, and for a guest job to the guests' share when no
// guest job held it yet; false means it does not fit.
func (d *snapData) acquirePublic(guest bool) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	var n, guestN int64
	if d.pubRefs == 0 {
		n = d.cost
	}
	if guest && d.guestRefs == 0 {
		guestN = d.cost
	}
	if (n > 0 || guestN > 0) && !reserveShare(&publicSnaps, n, guestN) {
		return false
	}
	d.pubRefs++
	if guest {
		d.guestRefs++
	}
	return true
}

func (d *snapData) releasePublic(guest bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pubRefs--
	if d.pubRefs == 0 {
		publicSnaps.Add(-d.cost)
	}
	if guest {
		if d.guestRefs--; d.guestRefs == 0 {
			guestShare.Add(-d.cost)
		}
	}
}

func (d *snapData) element(i int, aud int) []byte {
	var b []byte
	switch aud {
	case audPublic:
		b, _ = json.Marshal(model.PublicElement(d.els[i].Element))
	case audGuest:
		b, _ = json.Marshal(model.GuestView(d.els[i]))
	default:
		b, _ = json.Marshal(d.els[i])
	}
	return b
}

// partEnds cuts the elements into parts of at most 512 KiB, once per
// audience; each element is encoded only to measure it.
func (d *snapData) partEnds(aud int) []int {
	e := &d.ends[aud]
	e.once.Do(func() {
		const budget = maxServerFrame - 4096
		size := 0
		for i := range d.els {
			n := len(d.element(i, aud)) + 1
			if size+n > budget && size > 0 {
				e.v = append(e.v, i)
				size = 0
			}
			size += n
		}
		e.v = append(e.v, len(d.els))
	})
	return e.v
}

// rate is a token bucket owned by one reader goroutine.
type rate struct {
	perSec, burst, tokens float64
	last                  time.Time
}

func (r *rate) allow() bool {
	now := time.Now()
	if r.last.IsZero() {
		r.tokens = r.burst
	} else {
		r.tokens = min(r.burst, r.tokens+now.Sub(r.last).Seconds()*r.perSec)
	}
	r.last = now
	if r.tokens < 1 {
		return false
	}
	r.tokens--
	return true
}

// enqueue queues frames within the send queue caps (6): a socket whose
// queue would exceed its cap (Private 4 MiB with what is in flight, Public
// 1 MiB), or Public queues beyond the host cap, is closed instead and the
// frames are dropped. Every outgoing frame goes through it, so replies to a
// client that sends faster than it reads cannot grow the queue. deliver
// switches a Private socket to a snapshot before reaching the cap.
func (c *conn) enqueue(frames ...[]byte) bool { return c.enqueueAs(frameOther, frames...) }

// enqueueAs is enqueue for frames of one kind.
func (c *conn) enqueueAs(kind frameKind, frames ...[]byte) bool {
	msgs := make([]outMsg, len(frames))
	for i, f := range frames {
		msgs[i] = outMsg{frame: f, kind: kind}
	}
	return c.enqueueMsgs(msgs, false)
}

// enqueueDelta queues the frames of the delta at seq.
func (c *conn) enqueueDelta(seq int64, frames [][]byte) bool {
	msgs := make([]outMsg, len(frames))
	for i, f := range frames {
		msgs[i] = outMsg{frame: f, kind: frameDelta}
	}
	msgs[len(msgs)-1].seq = seq
	return c.enqueueMsgs(msgs, false)
}

// enqueueMsgs is enqueue for frames with their kind and seq. final also
// closes the socket after the queue, in the same step, so resync never
// finds a final frame queued on a socket that is not closing.
func (c *conn) enqueueMsgs(msgs []outMsg, final bool) bool {
	size := int64(0)
	for _, m := range msgs {
		size += int64(len(m.frame))
	}
	c.mu.Lock()
	if final {
		c.closing = true
		defer c.poke()
	}
	if c.overflowed {
		c.mu.Unlock()
		return false
	}
	if c.overCap(size) || c.public && !c.reserveQueued(size) {
		c.overflowed = true
		c.mu.Unlock()
		c.close(1008, "too far behind; reconnect")
		return false
	}
	for _, m := range msgs {
		m.guest = c.chargeGuest
		c.q = append(c.q, m)
	}
	c.qBytes += size
	c.mu.Unlock()
	c.poke()
	return true
}

// overCap reports whether size more bytes exceed the connection's queue
// cap; enqueue reserves the host's Public budget separately. c.mu is held.
func (c *conn) overCap(size int64) bool {
	if c.public {
		return c.qBytes+size > publicQueueMax
	}
	return c.qBytes+size+c.ws.Budget().Used() > c.in.privateQueue
}

// reserveQueued charges n bytes of a Public socket's frames to the host's
// Public budget, and a guest's also to the guests' share. c.mu is held, or
// the caller is the pump.
func (c *conn) reserveQueued(n int64) bool {
	var guestN int64
	if c.chargeGuest {
		guestN = n
	}
	return reserveShare(&publicQueued, n, guestN)
}

// unqueue returns the charge of n bytes of a Public socket's frames.
func unqueue(n int64, guest bool) {
	publicQueued.Add(-n)
	if guest {
		guestShare.Add(-n)
	}
}

func (c *conn) poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *conn) send(v any) {
	b, err := json.Marshal(v)
	if err == nil {
		c.enqueue(b)
	}
}

// reply sends a batch's ack or reject.
func (c *conn) reply(v any) {
	b, err := json.Marshal(v)
	if err == nil {
		c.enqueueAs(frameReply, b)
	}
}

func (c *conn) currentGen() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// queueSnapshot queues a snapshot of the state the apply loop holds now.
// A Public socket whose snapshot state does not fit the host's Public
// budget is closed, and its client retries. Only the apply loop calls it.
func (c *conn) queueSnapshot(reset bool) {
	job := c.snapJob(reset)
	if job == nil {
		c.close(1013, "busy; try again")
		return
	}
	c.mu.Lock()
	if c.overflowed {
		c.mu.Unlock()
		job.release()
		return
	}
	job.gen = c.gen
	c.q = append(c.q, outMsg{snap: job})
	c.snaps++
	c.mu.Unlock()
	c.poke()
}

// snapJob takes a snapshot job of the current seq in the socket's
// audience, or nil for a Public socket whose snapshot state does not fit
// the host's Public budget. Only the apply loop calls it.
func (c *conn) snapJob(reset bool) *snapJob {
	d := c.in.currentSnap()
	job := &snapJob{seq: d.seq, reset: reset, data: d}
	switch {
	case c.guest:
		job.aud = audGuest
	case c.public:
		job.aud = audPublic
	}
	if job.public() && !d.acquirePublic(c.guest) {
		return nil
	}
	return job
}

// fatal tells the client to stop and closes the socket after the queue.
func (c *conn) fatal(code string) { c.final(map[string]string{"t": "fatal", "code": code}) }

// final sends the socket's last message and closes it after the queue.
func (c *conn) final(v any) {
	b, _ := json.Marshal(v)
	c.enqueueMsgs([]outMsg{{frame: b}}, true)
}

func (c *conn) closeAfterQueue() {
	c.mu.Lock()
	c.closing = true
	c.mu.Unlock()
	c.poke()
}

func (c *conn) close(code int, reason string) { c.ws.Close(code, reason) }

// deliver queues the frames of a committed delta, applying the queue caps
// (4.6, 6): a socket over its cap (Private 4 MiB, Public 1 MiB) switches to
// a snapshot with resync, or closes when a snapshot is already pending;
// Public sockets over 256 MiB host-wide, or guests over their share, close.
// Only the apply loop calls it.
func (c *conn) deliver(rec deltaRec, publicBody, guestBody []byte) {
	c.mu.Lock()
	gen := c.gen
	pendingSnap := c.snaps > 0
	c.mu.Unlock()
	from := ""
	if rec.session != "" && rec.session == c.session {
		from = rec.session + ":" + strconv.FormatInt(rec.n, 10)
	}
	var frames [][]byte
	switch {
	case c.guest:
		frames = deltaFrames(gen, rec.seq, guestBody, from)
	case c.public:
		frames = deltaFrames(gen, rec.seq, publicBody, "")
	default:
		frames = deltaFrames(gen, rec.seq, rec.body, from)
	}
	size := int64(0)
	for _, f := range frames {
		size += int64(len(f))
	}
	c.mu.Lock()
	over := c.overCap(size)
	c.mu.Unlock()
	if !over {
		c.enqueueDelta(rec.seq, frames) // closes a Public socket over the host's budget
		return
	}
	if pendingSnap {
		c.close(1008, "too far behind; reconnect")
		return
	}
	c.resync()
}

// resync switches the client to a snapshot of the current seq with a new
// generation (4.6): it drops the queued deltas and snapshots, which the
// snapshot replaces, and returns their charges. The snapshot is marked
// reset when a reset happened after what the socket was handed, as the
// dropped deltas or snapshots would have said. Acks and rejects move
// behind the snapshot, which holds their batches' changes, so a pending
// batch still settles; other frames stay where they are. A closing socket
// is left as it is. Only the apply loop calls it.
func (c *conn) resync() {
	c.mu.Lock()
	// A closing socket ends with its queue, which may hold a fatal frame
	// that its replies must not pass; its client gets the board when it
	// connects again. It takes no snapshot state either, whose refusal
	// would close it before its queue drained. c.mu stays held until the
	// queue is rebuilt, so the socket cannot start closing meanwhile.
	if c.overflowed || c.closing {
		c.mu.Unlock()
		return
	}
	job := c.snapJob(false)
	if job == nil {
		c.mu.Unlock()
		c.close(1013, "busy; try again")
		return
	}
	c.gen++
	job.gen = c.gen
	job.reset = c.in.resetAfter(c.handed)
	var keep, replies, dropped []outMsg
	for _, m := range c.q {
		switch {
		case m.snap != nil:
			job.reset = job.reset || m.snap.reset
			dropped = append(dropped, m)
		case m.kind == frameDelta:
			c.qBytes -= int64(len(m.frame))
			if c.public {
				unqueue(int64(len(m.frame)), m.guest)
			}
		case m.kind == frameReply:
			replies = append(replies, m)
		default:
			keep = append(keep, m)
		}
	}
	b, _ := json.Marshal(map[string]any{"t": "resync", "gen": c.gen, "seq": job.seq})
	size := int64(len(b))
	if c.overCap(size) || c.public && !c.reserveQueued(size) {
		c.overflowed = true
		c.q = append(keep, replies...)
		c.snaps = 0
		c.mu.Unlock()
		releaseJobs(append(dropped, outMsg{snap: job}))
		c.close(1008, "too far behind; reconnect")
		return
	}
	c.q = slices.Concat(keep, []outMsg{{frame: b, guest: c.chargeGuest}, {snap: job}}, replies)
	c.qBytes += size
	c.snaps = 1
	c.mu.Unlock()
	releaseJobs(dropped)
	c.poke()
}

// pump feeds queued frames to the socket while fewer than 1 MiB are in
// flight, building snapshot parts as it goes.
func (c *conn) pump() {
	defer func() {
		// Nothing is sent from here on: refuse later frames, so none keeps
		// a charge, and return what is queued or unwritten.
		c.mu.Lock()
		c.overflowed = true
		c.mu.Unlock()
		c.dropQueue()
		c.landAfterClose()
	}()
	for {
		c.landFlight()
		c.mu.Lock()
		var m outMsg
		var held int64 // host Public bytes this frame keeps until the socket takes it
		have := len(c.q) > 0
		if have {
			m = c.q[0]
			// Clear the consumed slot: once a snapshot job is released its
			// state is no longer charged, so the backing array must not keep
			// it (or a sent frame) reachable.
			c.q[0] = outMsg{}
			c.q = c.q[1:]
			if m.snap != nil {
				c.snaps--
			} else {
				c.qBytes -= int64(len(m.frame))
				if c.public {
					held = int64(len(m.frame))
				}
			}
		}
		closing := c.closing
		c.mu.Unlock()
		if !have {
			if closing {
				c.ws.Close(1000, "")
				return
			}
			var land <-chan time.Time
			if c.flight > 0 {
				land = time.After(flightPoll)
			}
			select {
			case <-c.wake:
				continue
			case <-land:
				continue
			case <-c.ws.Done():
				c.dropQueue()
				return
			}
		}
		if m.snap != nil {
			ok := c.sendSnapshot(m.snap)
			m.snap.release()
			if !ok {
				c.dropQueue()
				return
			}
			continue
		}
		ok := c.feed(m.frame)
		if held > 0 {
			c.handOver(held, m.guest)
		}
		if !ok {
			c.dropQueue()
			return
		}
		if m.seq > 0 {
			c.setHanded(m.seq)
		}
	}
}

// setHanded records that the client's board reaches seq with what the
// socket was handed.
func (c *conn) setHanded(seq int64) {
	c.mu.Lock()
	c.handed = max(c.handed, seq)
	c.mu.Unlock()
}

// resetAfter reports whether a reset may have happened after seq: a
// recorded reset in (seq, now], or seq below the recorded resets. Only the
// apply loop calls it.
func (in *Instance) resetAfter(seq int64) bool {
	st := in.st
	if seq < st.ResetFloor() {
		return true
	}
	for _, s := range st.Resets() {
		if s > seq && s <= st.Seq() {
			return true
		}
	}
	return false
}

// flightPoll is how often an idle pump returns the guests' share of bytes
// its socket has written meanwhile.
const flightPoll = 20 * time.Millisecond

// handOver moves the charge of n bytes of a Public socket's frame, which the
// socket now holds, from its queue: the host's Public budget counts them
// in publicBudget from here, but a guest's share keeps them, as flight,
// until the socket has written them (landFlight), so the guests' share
// bounds their sockets' unwritten bytes too (TECH public edit 4.2). Only
// the pump calls it.
func (c *conn) handOver(n int64, guest bool) {
	publicQueued.Add(-n)
	if guest {
		c.flight += n
	}
}

// landFlight returns the guests' share of the bytes the socket has written
// since. Only the pump calls it.
func (c *conn) landFlight() {
	if c.flight == 0 {
		return
	}
	if held := c.ws.Budget().Used(); c.flight > held {
		guestShare.Add(held - c.flight)
		c.flight = held
	}
}

// landAfterClose returns the rest of the guests' share once the socket is
// closed: its writer keeps queued and unwritten bytes until it has written
// or discarded them, for up to the close grace (wsconn). The pump calls it
// as it stops.
func (c *conn) landAfterClose() {
	c.landFlight()
	if c.flight == 0 {
		return
	}
	n := c.flight
	c.flight = 0
	go func() {
		<-c.ws.Done()
		guestShare.Add(-n)
	}()
}

func (c *conn) dropQueue() {
	c.mu.Lock()
	if c.public {
		for _, m := range c.q {
			if m.snap == nil {
				unqueue(int64(len(m.frame)), m.guest)
			}
		}
	}
	dropped := c.q
	c.q, c.qBytes, c.snaps = nil, 0, 0
	c.mu.Unlock()
	releaseJobs(dropped)
}

func releaseJobs(q []outMsg) {
	for _, m := range q {
		if m.snap != nil {
			m.snap.release()
		}
	}
}

// feed sends one frame once less than feedBelow bytes are in flight; a
// guest's socket keeps less in flight, guestFeedBelow, so one stalled guest
// holds little of the guests' share.
func (c *conn) feed(f []byte) bool {
	below := int64(feedBelow)
	if c.guest {
		below = guestFeedBelow
	}
	for c.ws.Budget().Used() >= below {
		c.landFlight()
		select {
		case <-c.ws.Done():
			return false
		case <-time.After(5 * time.Millisecond):
		}
	}
	return c.ws.Send(string(f)) == nil
}

// sendSnapshot sends a snapshot in parts of at most 512 KiB, encoding each
// part only when it is its turn. A newer generation (a resync) abandons
// it. A Public part waiting for room counts toward the host's Public
// budget; a part that does not fit closes the connection (1008), as a
// queue over its cap does.
func (c *conn) sendSnapshot(j *snapJob) bool {
	d := j.data
	ends := d.partEnds(j.aud)
	var meta any = d.meta
	if j.aud == audPublic {
		meta = model.PublicElement(d.meta.Element)
	}
	guest := j.aud == audGuest
	var guestN int64
	if guest {
		guestN = maxServerFrame
	}
	start := 0
	for i, end := range ends {
		if c.currentGen() != j.gen {
			return true
		}
		// Reserve a whole frame before encoding, so the part's bytes never
		// exist uncharged, then give back what it did not use.
		if j.public() && !reserveShare(&publicQueued, maxServerFrame, guestN) {
			c.close(1008, "too far behind; reconnect")
			return false
		}
		var b bytes.Buffer
		fmt.Fprintf(&b, `{"t":"snapshot","gen":%d,"seq":%d,"part":%d,"parts":%d`, j.gen, j.seq, i+1, len(ends))
		if j.reset {
			b.WriteString(`,"reset":true`)
		}
		if i == 0 {
			metaJSON, _ := json.Marshal(meta)
			b.WriteString(`,"meta":`)
			b.Write(metaJSON)
		}
		b.WriteString(`,"elements":[`)
		for k := start; k < end; k++ {
			if k > start {
				b.WriteByte(',')
			}
			b.Write(d.element(k, j.aud))
		}
		b.WriteString("]}")
		start = end
		n := int64(b.Len())
		if j.public() {
			extra := n - maxServerFrame
			var guestExtra int64
			if guest {
				guestExtra = extra
			}
			if extra <= 0 {
				unqueue(-extra, guest)
			} else if !reserveShare(&publicQueued, extra, guestExtra) {
				unqueue(maxServerFrame, guest)
				c.close(1008, "too far behind; reconnect")
				return false
			}
		}
		ok := c.feed(b.Bytes())
		if j.public() {
			c.handOver(n, guest)
		}
		if !ok {
			return false
		}
	}
	c.setHanded(j.seq)
	return true
}

// deltaFrames encodes a delta (body: its JSON object without from) as one
// delta frame, or as delta_part frames when that would exceed 512 KiB.
func deltaFrames(gen, seq int64, body []byte, from string) [][]byte {
	inner := body
	if from != "" {
		fromJSON, _ := json.Marshal(from)
		inner = slices.Concat([]byte(`{"from":`), fromJSON, []byte(","), body[1:])
	}
	head := fmt.Sprintf(`{"t":"delta","gen":%d,`, gen)
	if len(head)+len(inner)-1 <= maxServerFrame {
		return [][]byte{slices.Concat([]byte(head), inner[1:])}
	}
	// JSON text needs at most 2 bytes per byte inside a string literal
	// (quotes and backslashes), so 240 KiB slices fit a 512 KiB frame.
	const chunk = 240 << 10
	var slicesOf [][]byte
	for len(inner) > 0 {
		n := min(chunk, len(inner))
		for n < len(inner) && !utf8.RuneStart(inner[n]) {
			n--
		}
		slicesOf = append(slicesOf, inner[:n])
		inner = inner[n:]
	}
	frames := make([][]byte, len(slicesOf))
	for i, s := range slicesOf {
		frames[i] = model.Canonical(map[string]any{"t": "delta_part", "gen": gen, "seq": seq, "part": i + 1, "parts": len(slicesOf), "data": string(s)})
	}
	return frames
}

// broadcast sends a committed delta to every registered socket. Only the
// sender's socket gets from (a session id is a resume credential).
// Viewers get the public projection and guests the guest projection, each
// encoded once per delta.
func (in *Instance) broadcast(rec deltaRec, d *model.Delta) {
	var publicBody, guestBody []byte
	for _, c := range in.hub.list() {
		switch {
		case c.guest && guestBody == nil:
			g := model.GuestDelta(*d)
			g.From = ""
			guestBody, _ = json.Marshal(g)
		case c.public && !c.guest && publicBody == nil:
			publicBody, _ = json.Marshal(model.PublicDelta(*d))
		}
		c.deliver(rec, publicBody, guestBody)
	}
}

// helloMsg is the first client message (Appendix A).
type helloMsg struct {
	T         string `json:"t"`
	V         int    `json:"v"`
	Session   string `json:"session"`
	Watermark *int64 `json:"watermark"`
	Pending   struct {
		Count  int   `json:"count"`
		FirstN int64 `json:"first_n"`
	} `json:"pending"`
	Editor string `json:"editor"`
	Name   string `json:"name"`
	// Guest asks to join a Public board at level edit as a guest; token is
	// the guest token the server issued earlier (TECH public edit 3.1).
	Guest *struct {
		Name  string `json:"name"`
		Token string `json:"token"`
	} `json:"guest"`
	// ViewerToken is a viewer's reconnect token (spec rule 9).
	ViewerToken string `json:"viewer_token"`
	// Nick is the page's collaborator name; guests see a member under it
	// (docs-public-edit-TECH 5.1).
	Nick string `json:"nick"`
}

func validEditor(s string) bool {
	if s == "" || len(s) > 80 {
		return false
	}
	for _, r := range s {
		if r > unicode.MaxASCII || !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("-_.:", r)) {
			return false
		}
	}
	return true
}

// cleanName collapses control characters and runs of spaces and keeps at
// most 80 characters.
func cleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if model.TextLen(s) > 80 {
		s = string([]rune(s)[:80])
	}
	return s
}

// tailscaleLogin is the trusted identity of a private request (6.1); the
// header counts only when core marked the request private.
func tailscaleLogin(r *http.Request) string {
	if !private(r) {
		return ""
	}
	l := strings.TrimSpace(r.Header.Get("Tailscale-User-Login"))
	if l == "" || len(l) > 200 || strings.ContainsFunc(l, unicode.IsControl) {
		return ""
	}
	return l
}

// personLabel names the person behind a private HTTP request: the verified
// login, else "Person". Browser HTTP requests carry no collaborator name.
func personLabel(r *http.Request) (key, label string) {
	if l := tailscaleLogin(r); l != "" {
		return "ts:" + l, l
	}
	return "person", "Person"
}

// sameOrigin implements the write protection's origin rule (7.1).
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin"
	}
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return false
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return origin == scheme+"://"+r.Host
}

func (in *Instance) serveWS(w http.ResponseWriter, r *http.Request) {
	if !wsconn.IsUpgrade(r) {
		http.Error(w, "WebSocket upgrade required", http.StatusBadRequest)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "request refused: not from this board", http.StatusForbidden)
		return
	}
	public := !private(r)
	if !in.hub.admit(public, in.privateMax, in.publicMax) {
		http.Error(w, "too many connections to this board", http.StatusServiceUnavailable)
		return
	}
	if !public {
		in.meas.warm()
	}
	opts := wsconn.Options{QueuedBytes: wsQueuedBytes}
	if public {
		opts.Shared = publicBudget
	}
	ws, err := wsconn.Upgrade(w, r, opts)
	if err != nil {
		in.hub.leave(&conn{public: public})
		return
	}
	c := &conn{
		in: in, ws: ws, public: public, wake: make(chan struct{}, 1),
		batches: rate{perSec: 30, burst: 60}, pres: rate{perSec: 20, burst: 20},
	}
	in.hub.accept(c)
	stop := context.AfterFunc(r.Context(), func() { ws.Close(1001, "going away") })
	defer stop()
	pumped := make(chan struct{})
	go func() { c.pump(); close(pumped) }()
	defer func() {
		// The others learn at once that the editor left, before this
		// socket's own queue drains.
		if in.hub.leave(c) {
			in.relayGone(c)
		}
		// Let the pump send what is queued, such as a fatal message.
		c.closeAfterQueue()
		select {
		case <-pumped:
		case <-time.After(2 * time.Second):
		}
		ws.Close(1000, "")
		c.dropAssembly()
		if c.viewerN > 0 {
			in.notifyViewers()
		}
	}()
	if err := in.readLoop(r, c); err != nil {
		if ce, ok := err.(*wsconn.CloseError); ok && ce.Code == 1009 {
			ws.Close(1009, "message too big")
		}
	}
}

// readLoop reads the hello and then the client's messages until the socket
// closes.
func (in *Instance) readLoop(r *http.Request, c *conn) error {
	raw, err := c.ws.ReadMessage(maxClientFrame)
	if err != nil {
		return err
	}
	var h helloMsg
	if json.Unmarshal([]byte(raw), &h) != nil || h.T != "hello" || h.V != 1 {
		c.fatal(engine.FatalProtocol)
		return nil
	}
	if err := in.welcome(r, c, h); err != nil {
		return err
	}
	for {
		raw, err := c.ws.ReadMessage(maxClientFrame)
		if err != nil {
			return err
		}
		// Every frame of a guest counts toward the board's guest byte
		// budget, refused and ignored ones included (TECH public edit 4.2).
		within := !c.guest || in.pub.guestBytes.count(float64(len(raw)))
		var head struct {
			T string `json:"t"`
		}
		if json.Unmarshal([]byte(raw), &head) != nil {
			c.fatal(engine.FatalProtocol)
			return nil
		}
		switch head.T {
		case "ops", "pres":
			if c.public && !c.guest || c.session == "" {
				c.fatal(engine.FatalProtocol)
				return nil
			}
		case "ops_part", "share":
			if c.public || c.session == "" {
				c.fatal(engine.FatalProtocol)
				return nil
			}
		}
		var ok bool
		switch {
		case c.guest && head.T == "ops":
			ok = in.onGuestOps(r.Context(), c, raw, within)
		case c.guest && head.T == "pres":
			ok = in.guestPres(c, raw, within)
		case head.T == "ops":
			ok = in.onOps(r.Context(), c, raw)
		case head.T == "ops_part":
			ok = in.onPart(r.Context(), c, raw)
		case head.T == "pres":
			ok = in.onPres(c, raw)
		case head.T == "share":
			ok = in.onShare(r.Context(), c, raw)
		case head.T == "hello":
			ok = false
		default:
			ok = true // unknown message kinds are ignored
		}
		if !ok {
			c.fatal(engine.FatalProtocol)
			return nil
		}
	}
}

// welcome starts or resumes the socket's session and sends the state.
func (in *Instance) welcome(r *http.Request, c *conn, h helloMsg) error {
	if c.public {
		if h.Guest != nil && guestCapable(r, in) {
			return in.guestWelcome(r, c, h)
		}
		return in.viewerWelcome(r, c, h)
	}
	// Both kinds of member get the nickname they sent, never a Tailscale
	// display name: that one must not reach guests.
	c.nick, _ = model.CleanGuestName(h.Nick)
	if login := tailscaleLogin(r); login != "" {
		c.key, c.verified, c.label = "ts:"+login, true, login
		c.name = cleanName(r.Header.Get("Tailscale-User-Name"))
		if c.name == "" {
			c.name = login
		}
	} else {
		if !validEditor(h.Editor) {
			c.fatal(engine.FatalProtocol)
			return nil
		}
		c.key, c.name = "browser:"+h.Editor, cleanName(h.Name)
		c.label = c.name
		if c.label == "" {
			c.label = "Person"
		}
	}
	c.color = colorOf(c.key)
	return in.do(r.Context(), func() error {
		st := in.st
		t, err := in.begin()
		if err != nil {
			c.close(1011, "unavailable")
			return nil
		}
		now := in.now().Unix()
		res := st.Hello(engine.Hello{Session: h.Session, Pending: h.Pending.Count, NewID: randomToken(),
			Key: c.key, Label: c.label, Verified: c.verified, Now: now})
		if err := t.persist(res.Changed, st.Seq()); err != nil {
			t.rollback()
			c.close(1011, "unavailable")
			return err
		}
		if err := t.commit(); err != nil {
			c.close(1011, "unavailable")
			return err
		}
		if res.Fatal != "" {
			c.fatal(res.Fatal)
			return nil
		}
		c.session = res.Session
		in.hub.join(c)
		c.send(map[string]any{"t": "welcome", "session": res.Session, "expires": now + engine.SessionTTL, "access": "private",
			"level": in.level, "access_epoch": in.accessSeq.Load(), "me": map[string]any{"key": c.key, "name": c.name, "verified": c.verified, "color": c.color}})
		seq := st.Seq()
		// Catching up through the delta log is limited to half the queue
		// cap; a longer gap gets a snapshot instead of a closed socket.
		var catchUp []outMsg
		if h.Watermark != nil && *h.Watermark < seq && !res.New && in.logCovers(*h.Watermark+1) {
			gen := c.currentGen()
			size := int64(0)
			for _, d := range in.sd.deltas {
				if d.seq > *h.Watermark && size <= in.privateQueue/2 {
					from := ""
					if d.session == c.session {
						from = d.session + ":" + strconv.FormatInt(d.n, 10)
					}
					for _, f := range deltaFrames(gen, d.seq, d.body, from) {
						catchUp = append(catchUp, outMsg{frame: f, kind: frameDelta})
						size += int64(len(f))
					}
					catchUp[len(catchUp)-1].seq = d.seq
				}
			}
			if size > in.privateQueue/2 {
				catchUp = nil
			}
		}
		if h.Watermark != nil && *h.Watermark <= seq && !res.New {
			c.setHanded(*h.Watermark)
		}
		switch {
		case res.New || h.Watermark == nil || *h.Watermark > seq:
			c.queueSnapshot(false)
		case *h.Watermark == seq:
		case catchUp != nil:
			c.enqueueMsgs(catchUp, false)
		default:
			// A dropped reset after the watermark is reported too.
			c.queueSnapshot(in.resetAfter(*h.Watermark))
		}
		in.sendPresence(c)
		in.hub.register(c)
		return nil
	})
}

// sendPresence sends a new member's or guest's socket the last presence of
// the other members and guests, in its audience's form, and the viewers.
// Only the apply loop calls it, before it registers c.
func (in *Instance) sendPresence(c *conn) {
	for _, o := range in.hub.list() {
		if o == c || o.public && !o.guest {
			continue
		}
		o.mu.Lock()
		p := o.lastPres
		if c.guest {
			p = o.lastPresGuest
		}
		o.mu.Unlock()
		if p != nil {
			c.enqueue(p)
		}
	}
	if f := in.viewers.frame(in.hub.viewerList()); f != nil {
		c.enqueue(f)
	}
}

// logCovers reports whether the delta log holds every entry from seq on.
func (in *Instance) logCovers(from int64) bool {
	d := in.sd.deltas
	return len(d) > 0 && d[0].seq <= from && d[len(d)-1].seq == in.st.Seq()
}

// batchReq is a person batch on its way to the apply loop. refuse is set
// when the reader already refused a guest's batch (TECH public edit 4.1):
// the apply loop records the refusal.
type batchReq struct {
	c      *conn
	n      int64
	raw    []byte // the JSON ops array
	refuse *engine.Reject
}

func (b *batchReq) transient(code string) {
	hostStats.retried.Add(1)
	b.c.reply(map[string]any{"t": "reject", "n": b.n, "code": code, "retry": true})
}

func (in *Instance) submit(ctx context.Context, b *batchReq) bool {
	if !b.c.batches.allow() {
		code := "rate_limited"
		if b.c.guest {
			code = CodeGuestRate
		}
		b.transient(code)
		return true
	}
	select {
	case in.items <- &item{batch: b}:
		return true
	case <-ctx.Done():
		return false
	case <-in.ctx.Done():
		return false
	}
}

func (in *Instance) onOps(ctx context.Context, c *conn, raw string) bool {
	var m struct {
		N   int64           `json:"n"`
		Ops json.RawMessage `json:"ops"`
	}
	c.mu.Lock()
	assembling := c.asm != nil
	c.mu.Unlock()
	if json.Unmarshal([]byte(raw), &m) != nil || m.N <= 0 || assembling {
		return false
	}
	return in.submit(ctx, &batchReq{c: c, n: m.N, raw: m.Ops})
}

// assembly collects the ops_part frames of one batch (4.5). The reader
// and the timeout timer change it only under c.mu, and whichever detaches
// it from c.asm first decides its outcome and returns its bytes to the
// host budget.
type assembly struct {
	n       int64
	parts   int
	next    int
	buf     []byte
	started time.Time
	timer   *time.Timer
}

// reserveAssembly charges n bytes to the host's assembly memory unless
// that would exceed 64 MiB.
func reserveAssembly(n int64) bool {
	for {
		old := hostAssembly.Load()
		if old+n > hostAssemblyMax {
			return false
		}
		if hostAssembly.CompareAndSwap(old, old+n) {
			return true
		}
	}
}

// detachAssembly removes a, when it is still the socket's assembly, and
// releases its bytes; it reports whether it did. c.mu is held.
func (c *conn) detachAssembly(a *assembly) bool {
	if a == nil || c.asm != a {
		return false
	}
	c.asm = nil
	c.discarded = a.n
	a.timer.Stop()
	hostAssembly.Add(-int64(len(a.buf)))
	return true
}

func (c *conn) dropAssembly() {
	c.mu.Lock()
	c.detachAssembly(c.asm)
	c.mu.Unlock()
}

func (in *Instance) onPart(ctx context.Context, c *conn, raw string) bool {
	var m struct {
		N     int64  `json:"n"`
		Part  int    `json:"part"`
		Parts int    `json:"parts"`
		Data  string `json:"data"`
	}
	if json.Unmarshal([]byte(raw), &m) != nil || m.N <= 0 || m.Parts < 2 || m.Parts > partsMax || len(m.Data) > partDataMax {
		return false
	}
	n := int64(len(m.Data))
	c.mu.Lock()
	a := c.asm
	if a == nil {
		if m.Part != 1 {
			// The rest of a batch dropped as transient (too large, timed
			// out, busy) may still arrive; any other skipped part is a
			// protocol error.
			ok := m.N == c.discarded
			c.mu.Unlock()
			return ok
		}
		a = &assembly{n: m.N, parts: m.Parts, next: 1, started: time.Now()}
		c.asm = a
		a.timer = time.AfterFunc(assemblyTimeout, func() {
			c.mu.Lock()
			current := c.detachAssembly(a)
			c.mu.Unlock()
			if current {
				(&batchReq{c: c, n: a.n}).transient("timeout")
			}
		})
	}
	if m.N != a.n || m.Parts != a.parts || m.Part != a.next {
		c.mu.Unlock()
		return false
	}
	var refuse string
	switch {
	case time.Since(a.started) > assemblyTimeout:
		refuse = "timeout"
	case int64(len(a.buf))+n > assemblyMax:
		refuse = "too_large"
	case !reserveAssembly(n):
		refuse = "busy"
	}
	if refuse != "" {
		c.detachAssembly(a)
		c.mu.Unlock()
		(&batchReq{c: c, n: a.n}).transient(refuse)
		return true
	}
	a.buf = append(a.buf, m.Data...)
	a.next++
	if a.next <= a.parts {
		c.mu.Unlock()
		return true
	}
	data := a.buf
	c.detachAssembly(a)
	c.mu.Unlock()
	return in.submit(ctx, &batchReq{c: c, n: a.n, raw: data})
}

// stageBatch applies one person batch inside the group commit t (4.5, 4.6,
// 5.4): a quota refusal rolls back only this batch and records the refusal.
func (in *Instance) stageBatch(t *txn, b *batchReq) {
	st := in.st
	c := b.c
	now := in.now().Unix()
	if c.guest && (c.epoch != in.epoch || in.level != AccessEdit) {
		// The level changed since this guest's welcome: its socket is
		// closing with editing_ended (TECH public edit 6.1).
		c.reply(map[string]any{"t": "reject", "n": b.n, "code": CodePermission, "retry": false,
			"detail": "the owner turned off guest editing"})
		return
	}
	ops, derr := engine.DecodeOps(b.raw)
	se, live := st.SessionRow(c.session)
	fresh := live && b.n == se.MaxN+1
	if c.guest && fresh && b.refuse == nil && derr == nil && !in.pub.guestOps.take(float64(len(ops))) {
		b.transient(CodeGuestRate)
		return
	}
	quota := in.b.Limits().DiskQuota
	if fresh && quota > 0 {
		if _, ok := in.meas.get(); !ok {
			in.meas.start()
			b.transient("no_budget")
			return
		}
	}
	var room *guestRoom
	if c.guest && fresh && b.refuse == nil && derr == nil {
		var err error
		if room, err = in.measureRoom(t, c, quota); err != nil {
			b.transient("unavailable")
			return
		}
	}
	saved := t.saveSide()
	if _, err := t.tx.ExecContext(t.ctx, `SAVEPOINT batch`); err != nil {
		b.transient("unavailable")
		return
	}
	mark := st.Savepoint()
	undo := func() {
		_, _ = t.tx.ExecContext(t.ctx, `ROLLBACK TO batch`)
		_, _ = t.tx.ExecContext(t.ctx, `RELEASE batch`)
		st.RollbackTo(mark)
		t.restoreSide(saved)
	}
	var out engine.Outcome
	switch {
	case b.refuse != nil && fresh:
		out = st.RecordReject(c.session, b.n, now, *b.refuse)
	case derr != nil && fresh:
		out = st.RecordReject(c.session, b.n, now, engine.Reject{Code: engine.CodeInvalid, Detail: derr.Error()})
	case derr != nil || b.refuse != nil:
		out = st.ApplyBatch(engine.Batch{Session: c.session, N: b.n, Now: now})
	default:
		out = st.ApplyBatch(engine.Batch{Session: c.session, N: b.n, Ops: ops, Now: now})
	}
	if !out.Replayed && out.Fatal == "" {
		err := in.writeBatch(t, b, &out)
		if err == nil && out.Reject == nil {
			refuse := func(code, detail string) {
				undo()
				saved = t.saveSide()
				_, _ = t.tx.ExecContext(t.ctx, `SAVEPOINT batch`)
				mark = st.Savepoint()
				out = st.RecordReject(c.session, b.n, now, engine.Reject{Code: code, Detail: detail})
				err = in.writeBatch(t, b, &out)
			}
			var over, ok bool
			over, ok, err = t.overQuota(quota)
			switch {
			case err != nil:
			case !ok:
				undo()
				b.transient("no_budget")
				return
			case over:
				refuse("disk_quota", "the board is over its disk quota")
			case room != nil:
				var detail string
				if detail, err = in.checkRoom(t, room, out.Changed); err == nil && detail != "" {
					refuse(CodeGuestRoom, detail)
				}
			}
		}
		if err != nil {
			undo()
			b.transient("unavailable")
			return
		}
	}
	_, _ = t.tx.ExecContext(t.ctx, `RELEASE batch`)
	st.Release(mark)
	t.onFail = append(t.onFail, func() { b.transient("unavailable") })
	t.after = append(t.after, func() { in.answer(b, out) })
}

// writeBatch persists an applied or refused batch: its rows, and for a
// change its delta and the editor's group.
func (in *Instance) writeBatch(t *txn, b *batchReq, out *engine.Outcome) error {
	if err := t.persist(out.Changed, max(out.Seq, in.st.Seq())); err != nil {
		return err
	}
	if out.Delta == nil {
		return nil
	}
	rec, err := t.addDelta(out.Delta, b.c.session, b.n)
	if err != nil {
		return err
	}
	d := out.Delta
	t.after = append(t.after, func() { in.broadcast(rec, d) })
	return t.touchGroup(b.c.key, b.c.label)
}

// answer sends the ack, reject or fatal of a committed batch.
func (in *Instance) answer(b *batchReq, out engine.Outcome) {
	switch {
	case out.Fatal != "":
		b.c.fatal(out.Fatal)
	case out.Reject != nil:
		hostStats.rejected.Add(1)
		b.c.reply(map[string]any{"t": "reject", "n": b.n, "code": out.Reject.Code, "retry": false, "detail": out.Reject.Detail})
	default:
		results := out.Results
		if results == nil {
			results = json.RawMessage("[]")
		}
		hostStats.applied.Add(1)
		b.c.reply(map[string]any{"t": "ack", "n": b.n, "seq": out.AckSeq, "results": results})
	}
}

type sideCopy struct {
	groups                   map[string]editGroup
	dirty                    map[int]int64
	bucketBytes              map[int]int64
	newDeltas                int
	trimDeltas               int
	assets                   map[string]*engine.Asset
	after                    int
	guestCharge, guestGroups int64
}

func (t *txn) saveSide() sideCopy {
	return sideCopy{maps.Clone(t.groups), maps.Clone(t.dirty), maps.Clone(t.bucketBytes), len(t.newDeltas), t.trimDeltas,
		maps.Clone(t.assets), len(t.after), t.guestCharge, t.guestGroups}
}

func (t *txn) restoreSide(s sideCopy) {
	t.groups, t.dirty, t.bucketBytes, t.assets = s.groups, s.dirty, s.bucketBytes, s.assets
	t.newDeltas, t.trimDeltas, t.after = t.newDeltas[:s.newDeltas], s.trimDeltas, t.after[:s.after]
	t.guestCharge, t.guestGroups = s.guestCharge, s.guestGroups
}

// presFields are the presence fields a client may send (6.1), with the
// form of a non-null value. The server adds key, name, color and verified.
var presFields = map[string]func(json.RawMessage) bool{
	"c":  func(r json.RawMessage) bool { var v [2]float64; return json.Unmarshal(r, &v) == nil },
	"vp": func(r json.RawMessage) bool { var v [5]float64; return json.Unmarshal(r, &v) == nil },
	"sel": func(r json.RawMessage) bool {
		var ids []string
		return json.Unmarshal(r, &ids) == nil && len(ids) <= model.MaxElements && !slices.ContainsFunc(ids, invalidID)
	},
	"drag": func(r json.RawMessage) bool {
		var m map[string][5]float64
		if json.Unmarshal(r, &m) != nil || len(m) > model.MaxElements {
			return false
		}
		for id := range m {
			if invalidID(id) {
				return false
			}
		}
		return true
	},
	"edit": func(r json.RawMessage) bool { var id string; return json.Unmarshal(r, &id) == nil && model.ValidID(id) },
}

func invalidID(id string) bool { return !model.ValidID(id) }

// onPres relays a valid presence update; a null field clears it on the
// other canvases. An invalid update is dropped.
func (in *Instance) onPres(c *conn, raw string) bool {
	if !c.pres.allow() {
		return true
	}
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &m) != nil {
		return false
	}
	fields := map[string]json.RawMessage{}
	for k, valid := range presFields {
		v, ok := m[k]
		if !ok {
			continue
		}
		if string(v) != "null" && !valid(v) {
			return true
		}
		fields[k] = v
	}
	forMembers, forGuests := c.presFrames(fields)
	c.mu.Lock()
	c.lastPres, c.lastPresGuest = forMembers, forGuests
	c.mu.Unlock()
	in.relayPres(c, forMembers, forGuests)
	return true
}

// presFrames builds a presence frame of c as members and as guests see it
// (TECH public edit 3.3): members see a guest's key and name with guest
// set; guests see connection labels, a member under its nickname or as
// "Member N" without one, unverified.
func (c *conn) presFrames(fields map[string]json.RawMessage) (forMembers, forGuests []byte) {
	m := map[string]any{"t": "pres", "key": c.key, "name": c.name, "color": c.color, "verified": c.verified}
	g := map[string]any{"t": "pres", "key": c.plabel, "name": c.name, "color": c.color, "verified": false}
	if c.guest {
		m["guest"], g["guest"] = true, true
	} else {
		g["name"] = cmp.Or(c.nick, "Member "+strconv.Itoa(c.plabelN))
	}
	for k, v := range fields {
		m[k], g[k] = v, v
	}
	forMembers, _ = json.Marshal(m)
	forGuests, _ = json.Marshal(g)
	return forMembers, forGuests
}

// relayPres sends a presence frame to the other members and guests, each
// in its audience's form (nil: none), dropping it for sockets that are
// behind. Viewers receive no presence.
func (in *Instance) relayPres(from *conn, forMembers, forGuests []byte) {
	relayTo(in.hub.list(), from, forMembers, forGuests)
}

// relayTo is relayPres to the sockets of to.
func relayTo(to []*conn, from *conn, forMembers, forGuests []byte) {
	for _, o := range to {
		if o == from || o.public && !o.guest {
			continue
		}
		o.mu.Lock()
		behind := o.qBytes > 256<<10 || o.snaps > 0
		o.mu.Unlock()
		if behind {
			continue
		}
		f := forMembers
		if o.guest {
			f = forGuests
		}
		if f != nil {
			o.enqueue(f)
		}
	}
}

// relayGone tells the others that the editor's last socket closed.
// Guests get the departed label, which no other key takes before this
// (hub.departing). Members get the key unless it came back meanwhile: the
// hub decides and queues that frame under its lock, which hub.join needs
// too, so a socket of the key that joins later has its presence queued
// after the gone frame in every member's queue.
func (in *Instance) relayGone(c *conn) {
	if !c.joined {
		return
	}
	forMembers, _ := json.Marshal(map[string]any{"t": "pres", "key": c.key, "gone": true})
	in.hub.goneToMembers(c, forMembers)
	if hook := goneHook.Load(); hook != nil {
		(*hook)(c.key)
	}
	forGuests, _ := json.Marshal(map[string]any{"t": "pres", "key": c.plabel, "gone": true})
	in.relayPres(c, nil, forGuests)
	in.hub.gone(c)
}

// goneHook runs after relayGone has dealt with the members. Tests set it.
var goneHook atomic.Pointer[func(key string)]

func (in *Instance) onShare(ctx context.Context, c *conn, raw string) bool {
	var m struct {
		IDs []string `json:"ids"`
	}
	if json.Unmarshal([]byte(raw), &m) != nil || len(m.IDs) == 0 || len(m.IDs) > model.MaxElements {
		return false
	}
	for _, id := range m.IDs {
		if !model.ValidID(id) {
			return false
		}
	}
	_ = in.do(ctx, func() error {
		t, err := in.begin()
		if err != nil {
			return err
		}
		t.shareN++
		n := t.shareN
		if err := setMeta(t.ctx, t.tx, "share_n", n); err != nil {
			t.rollback()
			return err
		}
		t.after = append(t.after, func() {
			in.shares.add(Share{N: n, IDs: slices.Clone(m.IDs), Key: c.key, Label: c.label, Verified: c.verified, At: in.now().Unix()})
			c.send(map[string]any{"t": "shared", "n": n})
		})
		return t.commit()
	})
	return true
}
