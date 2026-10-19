package board

import (
	"bytes"
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

// hub holds the board's open sockets and their pools (6).
type hub struct {
	mu    sync.Mutex
	conns map[*conn]struct{}
	keys  map[string]int // registered Private sockets per editor key
	priv  int            // admitted Private sockets
	pub   int            // admitted Public sockets
}

func newHub() *hub { return &hub{conns: map[*conn]struct{}{}, keys: map[string]int{}} }

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

func (h *hub) leave(c *conn) (lastOfKey bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	hostStats.conns.Add(-1)
	if c.public {
		h.pub--
	} else {
		h.priv--
	}
	if _, ok := h.conns[c]; !ok {
		return false
	}
	delete(h.conns, c)
	if c.public {
		return false
	}
	h.keys[c.key]--
	if h.keys[c.key] <= 0 {
		delete(h.keys, c.key)
		return true
	}
	return false
}

func (h *hub) register(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[c] = struct{}{}
	if !c.public {
		h.keys[c.key]++
	}
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

func (h *hub) closeAll(code string) {
	h.mu.Lock()
	all := make([]*conn, 0, len(h.conns))
	for c := range h.conns {
		all = append(all, c)
	}
	h.mu.Unlock()
	for _, c := range all {
		if code != "" {
			b, _ := json.Marshal(map[string]string{"t": "fatal", "code": code})
			_ = c.ws.Send(string(b))
		}
		c.ws.Close(1000, "board stopped")
	}
}

// conn is one WebSocket of the board.
type conn struct {
	in       *Instance
	ws       *wsconn.Conn
	public   bool
	key      string
	name     string
	label    string
	color    string
	verified bool
	session  string // set by the apply loop at welcome

	mu       sync.Mutex
	q        []outMsg
	qBytes   int64
	snaps    int // snapshot jobs queued
	gen      int64
	closing  bool
	wake     chan struct{}
	lastPres []byte
	// overflowed: the queue went over its cap and the socket is closing.
	overflowed bool

	asm       *assembly
	discarded int64 // the batch whose assembly was dropped as transient
	batches   rate
	pres      rate
}

type outMsg struct {
	frame []byte
	snap  *snapJob
}

type snapJob struct {
	gen, seq int64
	reset    bool
	public   bool
	data     *snapData
	done     sync.Once
}

// release gives up the job's hold on its snapshot state.
func (j *snapJob) release() {
	j.done.Do(func() {
		if j.public {
			j.data.releasePublic()
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

	mu      sync.Mutex
	pubRefs int // Public jobs holding it; they charge cost once host-wide
	ends    [2]struct {
		once sync.Once
		v    []int // end index of each part; 0 private, 1 public
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
func reservePublic(counter *atomic.Int64, n int64) bool {
	publicMu.Lock()
	defer publicMu.Unlock()
	if publicQueued.Load()+publicSnaps.Load()+publicBudget.Used()+n > hostPublicCap {
		return false
	}
	counter.Add(n)
	return true
}

// acquirePublic charges the state to the host's Public budget when no
// Public job held it yet; false means it does not fit.
func (d *snapData) acquirePublic() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pubRefs == 0 && !reservePublic(&publicSnaps, d.cost) {
		return false
	}
	d.pubRefs++
	return true
}

func (d *snapData) releasePublic() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pubRefs--
	if d.pubRefs == 0 {
		publicSnaps.Add(-d.cost)
	}
}

func (d *snapData) element(i int, public bool) []byte {
	var b []byte
	if public {
		b, _ = json.Marshal(model.PublicElement(d.els[i].Element))
	} else {
		b, _ = json.Marshal(d.els[i])
	}
	return b
}

// partEnds cuts the elements into parts of at most 512 KiB, once per
// audience; each element is encoded only to measure it.
func (d *snapData) partEnds(public bool) []int {
	k := 0
	if public {
		k = 1
	}
	e := &d.ends[k]
	e.once.Do(func() {
		const budget = maxServerFrame - 4096
		size := 0
		for i := range d.els {
			n := len(d.element(i, public)) + 1
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
func (c *conn) enqueue(frames ...[]byte) bool {
	size := int64(0)
	for _, f := range frames {
		size += int64(len(f))
	}
	c.mu.Lock()
	if c.overflowed {
		c.mu.Unlock()
		return false
	}
	if c.overCap(size) || c.public && !reservePublic(&publicQueued, size) {
		c.overflowed = true
		c.mu.Unlock()
		c.close(1008, "too far behind; reconnect")
		return false
	}
	for _, f := range frames {
		c.q = append(c.q, outMsg{frame: f})
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

func (c *conn) currentGen() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// queueSnapshot queues a snapshot of the state the apply loop holds now.
// A Public socket whose snapshot state does not fit the host's Public
// budget is closed, and its client retries. Only the apply loop calls it.
func (c *conn) queueSnapshot(reset bool) {
	d := c.in.currentSnap()
	job := &snapJob{seq: d.seq, reset: reset, public: c.public, data: d}
	if c.public && !d.acquirePublic() {
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

// fatal tells the client to stop and closes the socket after the queue.
func (c *conn) fatal(code string) {
	c.send(map[string]string{"t": "fatal", "code": code})
	c.closeAfterQueue()
}

func (c *conn) closeAfterQueue() {
	c.mu.Lock()
	c.closing = true
	c.mu.Unlock()
	c.poke()
}

func (c *conn) close(code int, reason string) { c.ws.Close(code, reason) }

// deliver queues the frames of a committed delta, applying the queue caps
// (4.6, 6): a Private socket over 4 MiB switches to a snapshot with resync,
// or closes when a snapshot is already pending; a Public socket over 1 MiB,
// or Public sockets over 256 MiB host-wide, close. Only the apply loop
// calls it.
func (c *conn) deliver(rec deltaRec, publicBody []byte) {
	c.mu.Lock()
	gen := c.gen
	pendingSnap := c.snaps > 0
	c.mu.Unlock()
	var frames [][]byte
	if c.public {
		frames = deltaFrames(gen, rec.seq, publicBody, "")
	} else {
		from := ""
		if rec.session != "" && rec.session == c.session {
			from = rec.session + ":" + strconv.FormatInt(rec.n, 10)
		}
		frames = deltaFrames(gen, rec.seq, rec.body, from)
	}
	size := int64(0)
	for _, f := range frames {
		size += int64(len(f))
	}
	c.mu.Lock()
	over := c.overCap(size)
	c.mu.Unlock()
	if !over || c.public {
		c.enqueue(frames...) // closes a Public socket over a cap
		return
	}
	if pendingSnap {
		c.close(1008, "too far behind; reconnect")
		return
	}
	c.resync()
}

// resync drops the queue and switches the client to a snapshot of the
// current seq with a new generation (4.6). Only the apply loop calls it.
func (c *conn) resync() {
	c.mu.Lock()
	c.gen++
	dropped := c.q
	c.q = nil
	c.qBytes = 0
	c.snaps = 0
	gen := c.gen
	c.mu.Unlock()
	releaseJobs(dropped)
	b, _ := json.Marshal(map[string]any{"t": "resync", "gen": gen, "seq": c.in.st.Seq()})
	c.enqueue(b)
	c.queueSnapshot(false)
}

// pump feeds queued frames to the socket while fewer than 1 MiB are in
// flight, building snapshot parts as it goes.
func (c *conn) pump() {
	for {
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
			select {
			case <-c.wake:
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
		publicQueued.Add(-held)
		if !ok {
			c.dropQueue()
			return
		}
	}
}

func (c *conn) dropQueue() {
	c.mu.Lock()
	if c.public {
		publicQueued.Add(-c.qBytes)
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

// feed sends one frame once less than feedBelow bytes are in flight.
func (c *conn) feed(f []byte) bool {
	for c.ws.Budget().Used() >= feedBelow {
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
	ends := d.partEnds(j.public)
	var meta any = d.meta
	if j.public {
		meta = model.PublicElement(d.meta.Element)
	}
	start := 0
	for i, end := range ends {
		if c.currentGen() != j.gen {
			return true
		}
		// Reserve a whole frame before encoding, so the part's bytes never
		// exist uncharged, then give back what it did not use.
		if c.public && !reservePublic(&publicQueued, maxServerFrame) {
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
			b.Write(d.element(k, j.public))
		}
		b.WriteString("]}")
		start = end
		n := int64(b.Len())
		if c.public {
			if n <= maxServerFrame {
				publicQueued.Add(n - maxServerFrame)
			} else if !reservePublic(&publicQueued, n-maxServerFrame) {
				publicQueued.Add(-maxServerFrame)
				c.close(1008, "too far behind; reconnect")
				return false
			}
		}
		ok := c.feed(b.Bytes())
		if c.public {
			publicQueued.Add(-n)
		}
		if !ok {
			return false
		}
	}
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
func (in *Instance) broadcast(rec deltaRec, d *model.Delta) {
	var publicBody []byte
	for _, c := range in.hub.list() {
		if c.public && publicBody == nil {
			publicBody, _ = json.Marshal(model.PublicDelta(*d))
		}
		c.deliver(rec, publicBody)
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
	stop := context.AfterFunc(r.Context(), func() { ws.Close(1001, "going away") })
	defer stop()
	pumped := make(chan struct{})
	go func() { c.pump(); close(pumped) }()
	defer func() {
		last := in.hub.leave(c)
		// Let the pump send what is queued, such as a fatal message.
		c.closeAfterQueue()
		select {
		case <-pumped:
		case <-time.After(2 * time.Second):
		}
		ws.Close(1000, "")
		c.dropAssembly()
		if last {
			in.relayGone(c)
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
		var head struct {
			T string `json:"t"`
		}
		if json.Unmarshal([]byte(raw), &head) != nil {
			c.fatal(engine.FatalProtocol)
			return nil
		}
		switch head.T {
		case "ops", "ops_part", "pres", "share":
			if c.public || c.session == "" {
				c.fatal(engine.FatalProtocol)
				return nil
			}
		}
		var ok bool
		switch head.T {
		case "ops":
			ok = in.onOps(r.Context(), c, raw)
		case "ops_part":
			ok = in.onPart(r.Context(), c, raw)
		case "pres":
			ok = in.onPres(c, raw)
		case "share":
			ok = in.onShare(r.Context(), c, raw)
		case "hello":
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
		if !in.publicStarts.wait(r.Context(), publicWait) {
			c.close(1013, "busy; try again")
			return nil
		}
		return in.do(r.Context(), func() error {
			c.send(map[string]any{"t": "welcome", "session": "", "expires": 0, "access": "public", "me": nil})
			c.queueSnapshot(false)
			in.hub.register(c)
			return nil
		})
	}
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
		c.send(map[string]any{"t": "welcome", "session": res.Session, "expires": now + engine.SessionTTL, "access": "private",
			"me": map[string]any{"key": c.key, "name": c.name, "verified": c.verified, "color": c.color}})
		seq := st.Seq()
		// Catching up through the delta log is limited to half the queue
		// cap; a longer gap gets a snapshot instead of a closed socket.
		var catchUp [][]byte
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
						catchUp = append(catchUp, f)
						size += int64(len(f))
					}
				}
			}
			if size > in.privateQueue/2 {
				catchUp = nil
			}
		}
		switch {
		case res.New || h.Watermark == nil || *h.Watermark > seq:
			c.queueSnapshot(false)
		case *h.Watermark == seq:
		case catchUp != nil:
			c.enqueue(catchUp...)
		default:
			// A dropped reset after the watermark is reported too.
			reset := *h.Watermark < st.ResetFloor()
			for _, s := range st.Resets() {
				if s > *h.Watermark && s <= seq {
					reset = true
				}
			}
			c.queueSnapshot(reset)
		}
		for _, o := range in.hub.list() {
			if o != c && !o.public {
				o.mu.Lock()
				p := o.lastPres
				o.mu.Unlock()
				if p != nil {
					c.enqueue(p)
				}
			}
		}
		in.hub.register(c)
		return nil
	})
}

// logCovers reports whether the delta log holds every entry from seq on.
func (in *Instance) logCovers(from int64) bool {
	d := in.sd.deltas
	return len(d) > 0 && d[0].seq <= from && d[len(d)-1].seq == in.st.Seq()
}

// batchReq is a person batch on its way to the apply loop.
type batchReq struct {
	c   *conn
	n   int64
	raw []byte // the JSON ops array
}

func (b *batchReq) transient(code string) {
	hostStats.retried.Add(1)
	b.c.send(map[string]any{"t": "reject", "n": b.n, "code": code, "retry": true})
}

func (in *Instance) submit(ctx context.Context, b *batchReq) bool {
	if !b.c.batches.allow() {
		b.transient("rate_limited")
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
	ops, derr := engine.DecodeOps(b.raw)
	se, live := st.SessionRow(c.session)
	fresh := live && b.n == se.MaxN+1
	quota := in.b.Limits().DiskQuota
	if fresh && quota > 0 {
		if _, ok := in.meas.get(); !ok {
			in.meas.start()
			b.transient("no_budget")
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
	case derr != nil && fresh:
		out = st.RecordReject(c.session, b.n, now, engine.Reject{Code: engine.CodeInvalid, Detail: derr.Error()})
	case derr != nil:
		out = st.ApplyBatch(engine.Batch{Session: c.session, N: b.n, Now: now})
	default:
		out = st.ApplyBatch(engine.Batch{Session: c.session, N: b.n, Ops: ops, Now: now})
	}
	if !out.Replayed && out.Fatal == "" {
		err := in.writeBatch(t, b, &out)
		if err == nil && out.Reject == nil {
			var over, ok bool
			over, ok, err = t.overQuota(quota)
			switch {
			case err != nil:
			case !ok:
				undo()
				b.transient("no_budget")
				return
			case over:
				undo()
				saved = t.saveSide()
				_, _ = t.tx.ExecContext(t.ctx, `SAVEPOINT batch`)
				mark = st.Savepoint()
				out = st.RecordReject(c.session, b.n, now, engine.Reject{Code: "disk_quota", Detail: "the board is over its disk quota"})
				err = in.writeBatch(t, b, &out)
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
		b.c.send(map[string]any{"t": "reject", "n": b.n, "code": out.Reject.Code, "retry": false, "detail": out.Reject.Detail})
	default:
		results := out.Results
		if results == nil {
			results = json.RawMessage("[]")
		}
		hostStats.applied.Add(1)
		b.c.send(map[string]any{"t": "ack", "n": b.n, "seq": out.AckSeq, "results": results})
	}
}

type sideCopy struct {
	groups      map[string]editGroup
	dirty       map[int]int64
	bucketBytes map[int]int64
	newDeltas   int
	trimDeltas  int
	assets      map[string]*engine.Asset
	after       int
}

func (t *txn) saveSide() sideCopy {
	return sideCopy{maps.Clone(t.groups), maps.Clone(t.dirty), maps.Clone(t.bucketBytes), len(t.newDeltas), t.trimDeltas, maps.Clone(t.assets), len(t.after)}
}

func (t *txn) restoreSide(s sideCopy) {
	t.groups, t.dirty, t.bucketBytes, t.assets = s.groups, s.dirty, s.bucketBytes, s.assets
	t.newDeltas, t.trimDeltas, t.after = t.newDeltas[:s.newDeltas], s.trimDeltas, t.after[:s.after]
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
	out := map[string]any{"t": "pres", "key": c.key, "name": c.name, "color": c.color, "verified": c.verified}
	for k, valid := range presFields {
		v, ok := m[k]
		if !ok {
			continue
		}
		if string(v) != "null" && !valid(v) {
			return true
		}
		out[k] = v
	}
	frame, _ := json.Marshal(out)
	c.mu.Lock()
	c.lastPres = frame
	c.mu.Unlock()
	in.relayPres(c, frame)
	return true
}

// relayPres sends a presence frame to the other Private sockets, dropping
// it for sockets that are behind.
func (in *Instance) relayPres(from *conn, frame []byte) {
	for _, o := range in.hub.list() {
		if o == from || o.public {
			continue
		}
		o.mu.Lock()
		behind := o.qBytes > 256<<10 || o.snaps > 0
		o.mu.Unlock()
		if !behind {
			o.enqueue(frame)
		}
	}
}

// relayGone tells the others that the editor's last socket closed.
func (in *Instance) relayGone(c *conn) {
	if c.public || c.session == "" {
		return
	}
	frame, _ := json.Marshal(map[string]any{"t": "pres", "key": c.key, "gone": true})
	in.relayPres(c, frame)
}

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
