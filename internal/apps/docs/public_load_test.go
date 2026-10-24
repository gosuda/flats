package docs

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestGuestLoad is the opt-in socket-level load test of anonymous public
// editing (docs/spec/docs-public-edit-TECH.md 8, row "Limits"; spec
// docs/spec/docs-public-edit.md, success criterion "Limits"). It measures
// socket delivery, a lower bound of the display time the spec targets.
//
//	DOCS_LOAD=1 (60 s) or DOCS_LOAD=<duration>, for example DOCS_LOAD=60s
//	DOCS_LOAD_FLOOD=0 leaves out the flood clients, for diagnosis
//
// One document flat at level edit. Every socket acknowledges frames as the
// page does (one cumulative receipt per busy moment). 20 guests edit
// index.md at the client's pace (4 updates a second) and resend after busy
// as the page does, first for 10 s without flood. Then flood clients fill
// the guest pool (30) and the viewer pool (40), send frames and bytes above
// the Public ingress buckets and the flat's guest byte budget, churn Public
// connections, and 2 unpaced guests send above the per-guest limit (they
// are answered busy; refused, they reconnect). Meanwhile member A edits
// index.md every 250 ms and member B, on the same document, times each
// edit's update frame. A third member connects and edits, and a member
// rollback runs, during the flood. The test asserts only robust facts:
// members are admitted, edit and roll back, no member socket closes during
// or after the flood, paced guests are never refused or closed (busy is
// allowed), acknowledged text after the rollback is kept in order, and
// member socket delivery p95 is under 200 ms. Guest throughput is only
// logged ("LOAD"), by phase: a determined flood may use up guest capacity
// (a documented limitation; Stop guest editing is the remedy).
func TestGuestLoad(t *testing.T) {
	setting := os.Getenv("DOCS_LOAD")
	if setting == "" {
		t.Skip("set DOCS_LOAD=1 (60 s) or DOCS_LOAD=<duration> to run the guest load test")
	}
	duration := 60 * time.Second
	if setting != "1" {
		d, err := time.ParseDuration(setting)
		if err != nil || d < 10*time.Second {
			t.Fatalf("DOCS_LOAD=%q: want 1 or a duration of at least 10s", setting)
		}
		duration = d
	}
	t.Logf("LOAD uptime at start: %s", uptime())

	texts := loadTexts()
	a := startAppModule(t, t.TempDir(), docsModule("v1", texts), "v1", map[string]string{})
	a.setAccess(t, "edit", 1)
	l := newLoadRun(a.srv.URL)
	t0 := l.t0
	// A failed step still stops the flood clients.
	t.Cleanup(func() {
		l.halt()
		l.floods.Wait()
	})
	since := func() time.Duration { return time.Since(t0).Round(time.Millisecond) }
	// DOCS_LOAD_FLOOD=0 leaves out the flood clients, for diagnosis: only
	// the paced guests and the members.
	flood := os.Getenv("DOCS_LOAD_FLOOD") != "0"

	// Members first, so their sockets exist before the flood.
	b := l.member(t, "index.md", "Member B", 0)
	b.setHandler(l.observe)
	memberA := l.member(t, "index.md", "Member A", 0)

	// The paced guests join before the flood clients use up the minute's
	// guest starts.
	guests := make([]*pacedGuest, 20)
	for i := range guests {
		guests[i] = l.pacedGuest(t, i)
	}
	// A phase without flood first: the paced guests and the members alone.
	quietStart := time.Now()
	if flood {
		t.Logf("LOAD at %v phase without flood, 10 s", since())
		time.Sleep(10 * time.Second)
	}
	floodStart := time.Now()
	if flood {
		l.startFlood(t)
	} else {
		t.Logf("LOAD DOCS_LOAD_FLOOD=0: no flood clients")
	}

	// Member A edits every 250 ms for the duration; B times delivery.
	editsDone := make(chan struct{})
	go func() {
		defer close(editsDone)
		l.memberEdits(memberA, duration)
	}()
	start := time.Now()
	t.Logf("LOAD at %v member edits start", since())
	type admission struct{ welcome, ack time.Duration }
	var admissions []admission
	var rollback, restoreCall time.Duration
	at := func(f float64) {
		if d := time.Duration(float64(duration)*f) - time.Since(start); d > 0 {
			time.Sleep(d)
		}
	}
	admit := func(n int) {
		w, ack := l.thirdMember(t, n)
		admissions = append(admissions, admission{w, ack})
	}
	at(1.0 / 3)
	t.Logf("LOAD at %v third member 1", since())
	admit(1)
	at(0.5)
	t.Logf("LOAD at %v rollback", since())
	rollback, restoreCall = l.rollback(t, a, texts)
	at(2.0 / 3)
	t.Logf("LOAD at %v third member 2", since())
	admit(2)
	<-editsDone
	t.Logf("LOAD at %v member edits end; flood stops", since())

	// Stop the flood, then let paced guests finish what they have.
	floodEnd := time.Now()
	l.halt()
	l.floods.Wait()
	for _, g := range guests {
		g.stopTyping.Store(true)
	}
	drainStart := time.Now()
	for open, _ := l.unacked(guests); open > 0 && time.Since(drainStart) < 20*time.Second; open, _ = l.unacked(guests) {
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("LOAD at %v drain ends", since())
	undrainedOpen, undrained := l.unacked(guests)
	// B sees the last edits.
	time.Sleep(2 * time.Second)
	// Only updates acknowledged before this read are certainly in it.
	finalAt := time.Now()
	final := a.markdown(t, "index.md")
	loadEnd := uptime()

	// --- report ---
	l.mu.Lock()
	lat := slices.Clone(l.latencies)
	missing := len(l.pending)
	l.mu.Unlock()
	t.Logf("LOAD duration %v, member A edits sent %d, acked %d, errors %v, ack p50 %v p95 %v",
		duration, l.aSent.Load(), l.aAcked.Load(), l.aErrors(), pct(l.ackTimes(), 0.5), pct(l.ackTimes(), 0.95))
	t.Logf("LOAD member->member socket delivery: count %d, p50 %v, p95 %v, p99 %v, max %v, not delivered %d",
		len(lat), pct(lat, 0.5), pct(lat, 0.95), pct(lat, 0.99), pct(lat, 1), missing)
	for i, ad := range admissions {
		t.Logf("LOAD third member admission %d: welcome after %v, edit acknowledged after %v", i+1, ad.welcome, ad.ack)
	}
	t.Logf("LOAD rollback: %v in all (stage + restore), restore call %v", rollback, restoreCall)
	var gs guestStats
	var guestAcks []time.Duration
	for _, g := range guests {
		g.mu.Lock()
		for _, x := range g.written {
			if !x.acked.IsZero() {
				guestAcks = append(guestAcks, x.acked.Sub(x.created))
			}
		}
		gs.created += g.created
		gs.sent += g.sent
		gs.acked += g.acked
		gs.busyFrames += g.busyFrames
		gs.busyEpisodes += g.busyEpisodes
		if g.fatal != "" {
			gs.fatal = append(gs.fatal, fmt.Sprintf("guest %d: %s", g.index, g.fatal))
		}
		g.mu.Unlock()
		if e := g.conn.endReason(); e != "" {
			gs.closed = append(gs.closed, fmt.Sprintf("guest %d: %s", g.index, e))
		}
	}
	t.Logf("LOAD paced guests (20): updates created %d, frames sent %d (with resends), acked %d, busy frames %d, busy episodes %d, unacked after drain %d (%d on open sockets), fatal %v, closed %v",
		gs.created, gs.sent, gs.acked, gs.busyFrames, gs.busyEpisodes, undrained, undrainedOpen, gs.fatal, gs.closed)
	t.Logf("LOAD paced guest updates, created to acknowledged: count %d, p50 %v, p95 %v, max %v",
		len(guestAcks), pct(guestAcks, 0.5), pct(guestAcks, 0.95), pct(guestAcks, 1))
	// The same by the phase each update was created in. Under the flood
	// guests may wait long (a documented limitation); it is not asserted.
	phase := func(name string, from, to time.Time) {
		var d []time.Duration
		created, acked := 0, 0
		for _, g := range guests {
			g.mu.Lock()
			for _, x := range g.written {
				if x.created.Before(from) || !x.created.Before(to) {
					continue
				}
				created++
				if !x.acked.IsZero() {
					acked++
					d = append(d, x.acked.Sub(x.created))
				}
			}
			g.mu.Unlock()
		}
		t.Logf("LOAD paced guest updates created %s: created %d, acked %d, created to acknowledged p50 %v, p95 %v, max %v",
			name, created, acked, pct(d, 0.5), pct(d, 0.95), pct(d, 1))
	}
	if flood {
		phase("before the flood", quietStart, floodStart)
		phase("during the flood", floodStart, floodEnd)
		phase("after the flood", floodEnd, finalAt)
	} else {
		phase("without flood", quietStart, finalAt)
	}
	l.mu.Lock()
	t.Logf("LOAD unpaced guests: connections %d, updates sent %d, acked %d, busy %d, refusals %v, hello errors %v, http %v",
		l.unpacedJoined, l.unpacedSent, l.unpacedAcked, l.unpacedBusy, l.unpacedRefusals, l.unpacedHello, l.unpacedHTTP)
	t.Logf("LOAD holding guests: joins %d, padded pings %d, ends %v, hello errors %v, http %v",
		l.holdingJoins, l.holdingPings, l.holdingEnds, l.holdingHello, l.holdingHTTP)
	t.Logf("LOAD flood viewers: joins %d, pings %d (padded %d), ends %v, http %v",
		l.viewerJoins, l.viewerPings, l.viewerPadded, l.viewerEnds, l.viewerHTTP)
	t.Logf("LOAD churn: attempts %d, outcomes %v", l.churnAttempts, l.churnOutcomes)
	l.mu.Unlock()
	t.Logf("LOAD member B socket: %s", b.stats(t0))
	t.Logf("LOAD member A socket: %s", memberA.stats(t0))
	t.Logf("LOAD member B frames received per second since the app started: %v", b.perSecond(t0))
	for _, g := range guests {
		t.Logf("LOAD paced guest %d socket: %s", g.index, g.conn.stats(t0))
	}
	t.Logf("LOAD uptime at end: %s", loadEnd)

	// --- robust facts ---
	if e := b.endReason(); e != "" {
		t.Errorf("member B's socket closed: %s", e)
	}
	if e := memberA.endReason(); e != "" {
		t.Errorf("member A's socket closed: %s", e)
	}
	if errs := l.aErrors(); len(errs) > 0 || l.aAcked.Load() != l.aSent.Load() {
		t.Errorf("member A: %d of %d edits acknowledged, errors %v", l.aAcked.Load(), l.aSent.Load(), errs)
	}
	if missing > 0 {
		t.Errorf("%d of member A's edits never reached member B", missing)
	}
	if len(gs.fatal) > 0 || len(gs.closed) > 0 {
		t.Errorf("paced guests were refused or closed: fatal %v, closed %v", gs.fatal, gs.closed)
	}
	// Nothing acknowledged after the rollback is lost, and each writer's
	// text keeps its order.
	l.checkAfterRollback(t, a, final, finalAt, guests)
	if p := pct(lat, 0.95); p >= 200*time.Millisecond {
		t.Errorf("member->member socket delivery p95 %v, want under 200ms (uptime at start and end logged above)", p)
	}
}

// loadTexts are the flat's documents: index.md for members and paced
// guests, flood.md for flood guests, v1.md to v4.md for flood viewers.
func loadTexts() map[string]string {
	texts := map[string]string{
		"index.md": initialText,
		"flood.md": "# Flood\n",
	}
	for i := 1; i <= 4; i++ {
		texts[fmt.Sprintf("v%d.md", i)] = fmt.Sprintf("# Viewers %d\n", i)
	}
	return texts
}

func newLoadRun(url string) *loadRun {
	return &loadRun{url: url, t0: time.Now(), stop: make(chan struct{}), pending: map[string]time.Time{}, aSentAt: map[string]time.Time{}}
}

// startFlood starts the flood clients and waits up to 20 s for them to fill
// the pools: 30 guests with the paced ones, and 40 viewers.
func (l *loadRun) startFlood(t *testing.T) {
	t.Helper()
	start := time.Now()
	for i := 0; i < 8; i++ {
		l.spawn(func() { l.holdingGuest(i) })
	}
	for i := 0; i < 2; i++ {
		l.spawn(func() { l.unpacedGuest(i) })
	}
	for i := 0; i < 40; i++ {
		l.spawn(func() { l.floodViewer(i) })
	}
	l.spawn(l.churn)
	for time.Since(start) < 20*time.Second && (l.guestsJoined.Load() < 10 || l.viewersJoined.Load() < 40) {
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("LOAD at %v pools after %v: flood guests %d/10, viewers %d/40", time.Since(l.t0).Round(time.Millisecond),
		time.Since(start).Round(time.Millisecond), l.guestsJoined.Load(), l.viewersJoined.Load())
}

// --- a minimal concurrent WebSocket client ---

// loadConn is a client socket like a browser's: sends are queued and never
// block the reader, numbered frames are confirmed with cumulative received
// receipts as the page sends them, and
// server pings are answered.
type loadConn struct {
	conn   net.Conn
	reader *bufio.Reader
	mu     sync.Mutex
	cond   *sync.Cond
	queue  []loadOut
	shut   bool
	done   chan struct{}
	end    string
	endAt  time.Time
	// Recorded for diagnosis: when frames arrived (when track is set) and
	// the most sends ever waiting in the client.
	track    bool
	arrivals []time.Time
	maxQueue int
	// Unacknowledged frames: the highest number and their bytes.
	unackedN     float64
	unackedBytes int
	receiptTimer *time.Timer
	receipts     int
	handler      atomic.Pointer[func(m map[string]any, at time.Time)]
}

// setHandler sets the function the reader calls for every JSON frame.
func (s *loadConn) setHandler(f func(m map[string]any, at time.Time)) { s.handler.Store(&f) }

type loadOut struct {
	op   byte
	data []byte
}

// errStatus is a refused upgrade.
type errStatus int

func (e errStatus) Error() string { return fmt.Sprintf("HTTP %d", int(e)) }

func dialLoad(url, doc string, headers map[string]string) (*loadConn, error) {
	host := strings.TrimPrefix(url, "http://")
	c, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 16)
	rand.Read(key)
	request := "GET /_docs/ws?doc=" + doc + " HTTP/1.1\r\nHost: " + host + "\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(key) + "\r\n"
	for k, v := range headers {
		request += k + ": " + v + "\r\n"
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err = c.Write([]byte(request + "\r\n")); err != nil {
		c.Close()
		return nil, err
	}
	reader := bufio.NewReader(c)
	r, err := http.ReadResponse(reader, nil)
	if err != nil {
		c.Close()
		return nil, err
	}
	if r.StatusCode != 101 {
		c.Close()
		return nil, errStatus(r.StatusCode)
	}
	c.SetDeadline(time.Time{})
	s := &loadConn{conn: c, reader: reader, done: make(chan struct{})}
	s.cond = sync.NewCond(&s.mu)
	return s, nil
}

// start runs the writer and the reader; set onFrame first.
func (s *loadConn) start() {
	go s.writeLoop()
	go s.readLoop()
}

func (s *loadConn) enqueue(op byte, data []byte) {
	s.mu.Lock()
	if !s.shut {
		s.queue = append(s.queue, loadOut{op, data})
		s.maxQueue = max(s.maxQueue, len(s.queue))
		s.cond.Signal()
	}
	s.mu.Unlock()
}

func (s *loadConn) send(m any) {
	b, _ := json.Marshal(m)
	s.enqueue(1, b)
}

// buffered is how many sends wait in the client.
func (s *loadConn) buffered() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

func (s *loadConn) close() {
	s.mu.Lock()
	s.shut = true
	s.cond.Broadcast()
	s.mu.Unlock()
	s.conn.Close()
}

func (s *loadConn) finish(reason string) {
	s.mu.Lock()
	if s.end == "" && !s.shut {
		s.end = reason
		s.endAt = time.Now()
	}
	s.shut = true
	s.cond.Broadcast()
	s.mu.Unlock()
	s.conn.Close()
}

// stats describes the socket for the report: frames received, the most in
// any second, the most sends waiting in the client, and how it ended.
func (s *loadConn) stats(start time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	peak := 0
	for i, j := 0, 0; j < len(s.arrivals); j++ {
		for s.arrivals[j].Sub(s.arrivals[i]) >= time.Second {
			i++
		}
		peak = max(peak, j-i+1)
	}
	end := "open"
	if s.end != "" {
		end = fmt.Sprintf("%s at %v", s.end, s.endAt.Sub(start).Round(time.Millisecond))
	}
	return fmt.Sprintf("frames received %d, most in 1 s %d, receipts sent %d, most queued sends %d, %s", len(s.arrivals), peak, s.receipts, s.maxQueue, end)
}

// perSecond lists the frames received in each second since start.
func (s *loadConn) perSecond(start time.Time) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int
	for _, at := range s.arrivals {
		i := int(at.Sub(start) / time.Second)
		for len(out) <= i {
			out = append(out, 0)
		}
		out[i]++
	}
	return out
}

// endReason is why the socket ended before the test closed it, or "".
func (s *loadConn) endReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.end
}

func (s *loadConn) writeLoop() {
	for {
		s.mu.Lock()
		for len(s.queue) == 0 && !s.shut {
			s.cond.Wait()
		}
		if s.shut {
			s.mu.Unlock()
			return
		}
		f := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()
		header := []byte{0x80 | f.op}
		n := len(f.data)
		switch {
		case n < 126:
			header = append(header, 0x80|byte(n))
		case n <= 65535:
			header = append(header, 0xfe, byte(n>>8), byte(n))
		default:
			header = append(header, 0xff)
			var size [8]byte
			binary.BigEndian.PutUint64(size[:], uint64(n))
			header = append(header, size[:]...)
		}
		var mask [4]byte
		rand.Read(mask[:])
		header = append(header, mask[:]...)
		out := make([]byte, len(header)+n)
		copy(out, header)
		for i, b := range f.data {
			out[len(header)+i] = b ^ mask[i%4]
		}
		s.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := s.conn.Write(out); err != nil {
			s.finish("write: " + err.Error())
			return
		}
	}
}

func (s *loadConn) readLoop() {
	defer close(s.done)
	for {
		var h [2]byte
		if _, err := io.ReadFull(s.reader, h[:]); err != nil {
			s.finish("read: " + err.Error())
			return
		}
		n := uint64(h[1] & 127)
		if n == 126 {
			var size [2]byte
			if _, err := io.ReadFull(s.reader, size[:]); err != nil {
				s.finish("read: " + err.Error())
				return
			}
			n = uint64(binary.BigEndian.Uint16(size[:]))
		} else if n == 127 {
			var size [8]byte
			if _, err := io.ReadFull(s.reader, size[:]); err != nil {
				s.finish("read: " + err.Error())
				return
			}
			n = binary.BigEndian.Uint64(size[:])
		}
		if n > 8<<20 {
			s.finish(fmt.Sprint("large frame ", n))
			return
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(s.reader, b); err != nil {
			s.finish("read: " + err.Error())
			return
		}
		at := time.Now()
		if s.track {
			s.mu.Lock()
			s.arrivals = append(s.arrivals, at)
			s.mu.Unlock()
		}
		switch h[0] & 15 {
		case 9:
			s.enqueue(10, b)
			continue
		case 10:
			continue
		case 8:
			reason := "close"
			if len(b) >= 2 {
				reason = fmt.Sprintf("close %d %s", binary.BigEndian.Uint16(b[:2]), b[2:])
			}
			s.finish(reason)
			return
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			s.finish("invalid frame: " + err.Error())
			return
		}
		if f := s.handler.Load(); f != nil {
			(*f)(m, at)
		}
		if n, ok := m["n"].(float64); ok {
			s.receipt(n, len(b))
		}
	}
}

// Receipts as the page sends them (provider.js receipt): one cumulative
// received per busy moment, 200 ms after the first unacknowledged frame or
// at once after 256 KiB of unacknowledged frames.
const (
	loadReceiptBytes = 256 * 1024
	loadReceiptDelay = 200 * time.Millisecond
)

func (s *loadConn) receipt(n float64, size int) {
	s.mu.Lock()
	s.unackedN = max(s.unackedN, n)
	s.unackedBytes += size
	if s.unackedBytes >= loadReceiptBytes {
		s.mu.Unlock()
		s.flushReceipt()
		return
	}
	if s.receiptTimer == nil {
		s.receiptTimer = time.AfterFunc(loadReceiptDelay, s.flushReceipt)
	}
	s.mu.Unlock()
}

func (s *loadConn) flushReceipt() {
	s.mu.Lock()
	if s.receiptTimer != nil {
		s.receiptTimer.Stop()
		s.receiptTimer = nil
	}
	n := s.unackedN
	s.unackedN, s.unackedBytes = 0, 0
	if n > 0 {
		s.receipts++
	}
	s.mu.Unlock()
	if n > 0 {
		s.send(map[string]any{"t": "received", "n": n})
	}
}

// --- the run ---

type loadRun struct {
	url string
	t0  time.Time
	// guestText is the paced guests' text: "" for "[g00-00001]" on one line
	// per guest; "words" for words with a newline every 10 updates and a
	// blank line every 50, like typed paragraphs; "brackets" for the
	// bracketed form with a newline every 10 updates and no blank lines,
	// one long paragraph; "destinations" for "[g00-00001](", an unfinished
	// link destination, with no spaces or line breaks, so each guest's
	// text is one run that a link destination scan crosses.
	guestText string
	stop      chan struct{}
	halted    sync.Once
	floods    sync.WaitGroup

	guestsJoined, viewersJoined atomic.Int64
	aSent, aAcked               atomic.Int64

	mu          sync.Mutex
	pending     map[string]time.Time // A's markers not yet seen by B
	latencies   []time.Duration
	aAcks       []time.Duration
	aErrs       []string
	aSentAt     map[string]time.Time
	aTexts      []loadText
	rollbackEnd time.Time

	unpacedJoined, unpacedSent, unpacedAcked, unpacedBusy int
	unpacedRefusals, unpacedHello, unpacedHTTP            map[string]int
	holdingJoins, holdingPings                            int
	holdingEnds, holdingHello, holdingHTTP                map[string]int
	viewerJoins, viewerPings, viewerPadded                int
	viewerEnds, viewerHTTP                                map[string]int
	churnAttempts                                         int
	churnOutcomes                                         map[string]int
}

// loadText is one writer's inserted text and when it was created.
type loadText struct {
	id, text       string
	created, acked time.Time
}

type guestStats struct {
	created, sent, acked, busyFrames, busyEpisodes int
	fatal, closed                                  []string
}

func (l *loadRun) spawn(f func()) {
	l.floods.Add(1)
	go func() {
		defer l.floods.Done()
		f()
	}()
}

// halt stops the flood clients.
func (l *loadRun) halt() { l.halted.Do(func() { close(l.stop) }) }

func (l *loadRun) stopped() bool {
	select {
	case <-l.stop:
		return true
	default:
		return false
	}
}

// sleep waits d or until the flood stops; it reports whether to go on.
func (l *loadRun) sleep(d time.Duration) bool {
	select {
	case <-l.stop:
		return false
	case <-time.After(d):
		return true
	}
}

func count(m *map[string]int, key string) {
	if *m == nil {
		*m = map[string]int{}
	}
	(*m)[key]++
}

func endKind(reason string) string {
	if i := strings.Index(reason, "read: "); i == 0 {
		return "read error"
	}
	return reason
}

// publicDial connects a Public socket, retrying refusals of the runtime's
// Public slots and start rate (HTTP 503) as the page does.
func (l *loadRun) publicDial(doc string, http *map[string]int) *loadConn {
	for !l.stopped() {
		c, err := dialLoad(l.url, doc, publicHeaders("edit", 1))
		if err == nil {
			return c
		}
		l.mu.Lock()
		count(http, err.Error())
		l.mu.Unlock()
		if !l.sleep(200 * time.Millisecond) {
			return nil
		}
	}
	return nil
}

// member connects a Private socket and waits for its welcome.
func (l *loadRun) member(t *testing.T, doc, nick string, timeout time.Duration) *loadConn {
	t.Helper()
	c, err := dialLoad(l.url, doc, memberHeaders)
	if err != nil {
		t.Fatal("member", nick, err)
	}
	welcome := make(chan struct{}, 1)
	c.setHandler(func(m map[string]any, _ time.Time) {
		if m["t"] == "welcome" {
			select {
			case welcome <- struct{}{}:
			default:
			}
		}
	})
	c.track = true
	c.start()
	c.send(map[string]any{"t": "hello", "v": 1, "doc": doc, "sv": "AA==", "name": nick, "nick": nick})
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	select {
	case <-welcome:
	case <-time.After(timeout):
		t.Fatalf("member %s: no welcome within %v (%s)", nick, timeout, c.endReason())
	}
	t.Cleanup(c.close)
	// A page's heartbeat, about every 15 seconds.
	go func() {
		for {
			select {
			case <-c.done:
				return
			case <-time.After(15 * time.Second):
				c.send(map[string]any{"t": "ping"})
			}
		}
	}()
	return c
}

// observe is member B's frame handler: it times A's markers.
func (l *loadRun) observe(m map[string]any, at time.Time) {
	if m["t"] != "update" {
		return
	}
	u, _ := m["u"].(string)
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) == 0 {
		return
	}
	b, err := base64.StdEncoding.DecodeString(u)
	if err != nil {
		return
	}
	for marker, sent := range l.pending {
		if bytes.Contains(b, []byte(marker)) {
			l.latencies = append(l.latencies, at.Sub(sent))
			delete(l.pending, marker)
		}
	}
}

// memberEdits sends one edit from member A every 250 ms.
func (l *loadRun) memberEdits(c *loadConn, duration time.Duration) {
	const client = 90001
	c.setHandler(func(m map[string]any, at time.Time) {
		switch m["t"] {
		case "ack":
			id, _ := m["id"].(string)
			l.mu.Lock()
			if sent, ok := l.aSentAt[id]; ok {
				l.aAcks = append(l.aAcks, at.Sub(sent))
				delete(l.aSentAt, id)
				l.aAcked.Add(1)
				if i := indexOf(id); i >= 0 && i < len(l.aTexts) {
					l.aTexts[i].acked = at
				}
			}
			l.mu.Unlock()
		case "error":
			l.mu.Lock()
			l.aErrs = append(l.aErrs, fmt.Sprint(m["code"], ": ", m["message"]))
			l.mu.Unlock()
		}
	})
	clock := uint64(0)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for i, start := 0, time.Now(); time.Since(start) < duration; i++ {
		marker := fmt.Sprintf("<A%05d>", i)
		u := yInsert(client, marker)
		if i > 0 {
			u = yAfter(client, clock, marker)
		}
		clock += uint64(len(marker))
		id := fmt.Sprintf("a-%05d", i)
		now := time.Now()
		l.mu.Lock()
		l.pending[marker] = now
		l.aSentAt[id] = now
		l.aTexts = append(l.aTexts, loadText{id: id, text: marker, created: now})
		l.mu.Unlock()
		l.aSent.Add(1)
		c.send(map[string]any{"t": "update", "id": id, "u": u})
		<-tick.C
	}
	// Wait for the last acknowledgements and deliveries.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		left := len(l.aSentAt) + len(l.pending)
		l.mu.Unlock()
		if left == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func indexOf(id string) int {
	var i int
	if _, err := fmt.Sscanf(id, "a-%d", &i); err != nil {
		return -1
	}
	return i
}

func (l *loadRun) aErrors() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.aErrs)
}

func (l *loadRun) ackTimes() []time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.aAcks)
}

// thirdMember connects another member during the flood and edits; it
// returns the times to its welcome and to its edit's acknowledgement.
func (l *loadRun) thirdMember(t *testing.T, n int) (time.Duration, time.Duration) {
	t.Helper()
	start := time.Now()
	c, err := dialLoad(l.url, "index.md", memberHeaders)
	if err != nil {
		t.Fatalf("third member %d: %v", n, err)
	}
	defer c.close()
	frames := make(chan map[string]any, 256)
	c.setHandler(func(m map[string]any, _ time.Time) {
		switch m["t"] {
		case "welcome", "ack", "error":
			frames <- m
		}
	})
	c.start()
	c.send(map[string]any{"t": "hello", "v": 1, "doc": "index.md", "sv": "AA==", "name": "Third", "nick": "Third"})
	wait := func(kind string) map[string]any {
		select {
		case m := <-frames:
			if m["t"] != kind {
				t.Fatalf("third member %d: expected %s, got %v", n, kind, m)
			}
			return m
		case <-time.After(15 * time.Second):
			t.Fatalf("third member %d: no %s within 15s (%s)", n, kind, c.endReason())
		}
		return nil
	}
	wait("welcome")
	welcome := time.Since(start)
	sent := time.Now()
	c.send(map[string]any{"t": "update", "id": fmt.Sprint("third-", n), "u": yInsert(uint64(91000+n), fmt.Sprintf("third member %d\n", n))})
	wait("ack")
	return welcome, time.Since(sent)
}

// rollback restores the flat's original texts through the host restore
// operation, as core's rollback_revision does: stage each text, then
// restore. It returns the whole duration and the restore call's.
func (l *loadRun) rollback(t *testing.T, a *app, texts map[string]string) (time.Duration, time.Duration) {
	t.Helper()
	start := time.Now()
	var documents []map[string]any
	for path, text := range texts {
		status, out := a.hostOp(t, "POST", "stage", map[string]any{"text": text})
		if status != 200 {
			t.Fatal("stage", path, status, out)
		}
		documents = append(documents, map[string]any{"path": path, "hash": out["hash"]})
	}
	restoreStart := time.Now()
	status, out := a.hostOp(t, "POST", "restore", map[string]any{"id": "load-rollback-1", "label": "Load test", "rollback_of": 1, "documents": documents})
	if status != 200 {
		t.Fatal("restore", status, out)
	}
	l.mu.Lock()
	l.rollbackEnd = time.Now()
	l.mu.Unlock()
	return time.Since(start), time.Since(restoreStart)
}

// --- guests ---

// loadGuestWindow is how many updates a guest page keeps sent and
// unacknowledged (provider.js GUEST_WINDOW).
const loadGuestWindow = 2

// pacedGuest is a guest page: 4 updates a second at most, at most
// loadGuestWindow unacknowledged, in order, and after busy it waits a second
// and resends everything unacknowledged, the first marked retry
// (provider.js). Typing while updates wait starts no new update: the page
// merges it into the waiting one.
type pacedGuest struct {
	index      int
	client     uint64
	conn       *loadConn
	stopTyping atomic.Bool

	mu           sync.Mutex
	items        []string // unacknowledged ids, in order
	data         map[string]string
	texts        map[string]int // id -> index in written
	written      []loadText
	unsent       []string
	inflight     map[string]bool
	retryNext    bool
	busyUntil    time.Time
	log          []time.Time
	clock        uint64
	created      int
	sent         int
	acked        int
	busyFrames   int
	busyEpisodes int
	fatal        string
}

func (l *loadRun) pacedGuest(t *testing.T, i int) *pacedGuest {
	t.Helper()
	g := &pacedGuest{index: i, client: uint64(20000 + i), data: map[string]string{}, texts: map[string]int{}, inflight: map[string]bool{}}
	var c *loadConn
	for tries := 0; ; tries++ {
		var err error
		c, err = dialLoad(l.url, "index.md", publicHeaders("edit", 1))
		if err == nil {
			break
		}
		if tries > 100 {
			t.Fatal("paced guest", i, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	welcome := make(chan map[string]any, 1)
	c.setHandler(func(m map[string]any, _ time.Time) {
		switch m["t"] {
		case "welcome":
			welcome <- m
		case "ack":
			id, _ := m["id"].(string)
			g.mu.Lock()
			if _, ok := g.data[id]; ok {
				delete(g.data, id)
				g.items = slices.DeleteFunc(g.items, func(s string) bool { return s == id })
				delete(g.inflight, id)
				g.acked++
				g.written[g.texts[id]].acked = time.Now()
			}
			g.mu.Unlock()
		case "busy":
			id, _ := m["id"].(string)
			g.mu.Lock()
			g.busyFrames++
			if g.inflight[id] {
				g.busyEpisodes++
				clear(g.inflight)
				g.unsent = slices.Clone(g.items)
				g.retryNext = true
				g.busyUntil = time.Now().Add(time.Second)
			}
			g.mu.Unlock()
		case "error":
			g.mu.Lock()
			if g.fatal == "" {
				g.fatal = fmt.Sprint(m["code"], ": ", m["message"])
			}
			g.mu.Unlock()
		}
	})
	c.track = true
	c.start()
	c.send(map[string]any{"t": "hello", "v": 1, "doc": "index.md", "sv": "AA==", "guest": map[string]any{"name": fmt.Sprint("Paced ", i)}})
	select {
	case w := <-welcome:
		if me, _ := w["me"].(map[string]any); me["kind"] != "guest" {
			t.Fatal("paced guest", i, "joined as", w["me"])
		}
	case <-time.After(15 * time.Second):
		t.Fatal("paced guest", i, "no welcome", c.endReason())
	}
	g.conn = c
	t.Cleanup(c.close)
	go g.run(l)
	return g
}

func (g *pacedGuest) run(l *loadRun) {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	lastTyped, heartbeat := time.Time{}, time.Now()
	for range tick.C {
		select {
		case <-g.conn.done:
			return
		default:
		}
		now := time.Now()
		if now.Sub(heartbeat) > 15*time.Second {
			heartbeat = now
			g.conn.send(map[string]any{"t": "ping"})
		}
		g.mu.Lock()
		if !g.busyUntil.IsZero() && now.Before(g.busyUntil) {
			g.mu.Unlock()
			continue
		}
		g.busyUntil = time.Time{}
		// Typing: a page merges what it cannot send yet, so a new update
		// starts only when nothing waits.
		if !g.stopTyping.Load() && len(g.unsent) == 0 && now.Sub(lastTyped) >= 250*time.Millisecond {
			lastTyped = now
			text := fmt.Sprintf("[g%02d-%05d]", g.index, g.created)
			switch l.guestText {
			case "words":
				text = fmt.Sprintf("g%02dw%05d ", g.index, g.created)
				if g.created%50 == 49 {
					text += "\n\n"
				} else if g.created%10 == 9 {
					text += "\n"
				}
			case "brackets":
				if g.created%10 == 9 {
					text += "\n"
				}
			case "destinations":
				text += "("
			}
			u := yInsert(g.client, text)
			if g.clock > 0 {
				u = yAfter(g.client, g.clock, text)
			}
			g.clock += uint64(len(text))
			id := fmt.Sprintf("g%02d-%05d", g.index, g.created)
			g.created++
			g.items = append(g.items, id)
			g.data[id] = u
			g.texts[id] = len(g.written)
			g.written = append(g.written, loadText{id: id, text: text, created: now})
			g.unsent = append(g.unsent, id)
		}
		// Pacing: at most 4 updates in any second, and at most
		// loadGuestWindow sent and unacknowledged; an ack lets the next go.
		for len(g.unsent) > 0 && len(g.inflight) < loadGuestWindow {
			g.log = slices.DeleteFunc(g.log, func(s time.Time) bool { return now.Sub(s) >= time.Second })
			if len(g.log) >= 4 {
				break
			}
			id := g.unsent[0]
			g.unsent = g.unsent[1:]
			u, ok := g.data[id]
			if !ok {
				continue
			}
			m := map[string]any{"t": "update", "id": id, "u": u}
			if g.retryNext {
				m["retry"] = true
				g.retryNext = false
			}
			g.inflight[id] = true
			g.log = append(g.log, now)
			g.sent++
			g.conn.send(m)
		}
		g.mu.Unlock()
	}
}

// unacked counts the paced guests' unacknowledged updates: on open sockets,
// and on all of them.
func (l *loadRun) unacked(guests []*pacedGuest) (open, all int) {
	for _, g := range guests {
		g.mu.Lock()
		if g.fatal == "" && g.conn.endReason() == "" {
			open += len(g.items)
		}
		all += len(g.items)
		g.mu.Unlock()
	}
	return open, all
}

// guestHello joins c as a guest and returns the first answer: welcome or
// error.
func guestHello(c *loadConn, doc, name, token string, client chan map[string]any) map[string]any {
	g := map[string]any{"name": name}
	if token != "" {
		g["token"] = token
	}
	c.send(map[string]any{"t": "hello", "v": 1, "doc": doc, "sv": "AA==", "guest": g})
	select {
	case m := <-client:
		return m
	case <-c.done:
		return map[string]any{"t": "closed", "code": c.endReason()}
	case <-time.After(15 * time.Second):
		return map[string]any{"t": "timeout"}
	}
}

// holdingGuest fills a guest slot in flood.md and sends padded pings, so the
// flat's guest byte budget and the Public byte buckets overflow.
func (l *loadRun) holdingGuest(i int) {
	padding := strings.Repeat("x", 350*1024)
	for !l.stopped() {
		c := l.publicDial("flood.md", &l.holdingHTTP)
		if c == nil {
			return
		}
		first := make(chan map[string]any, 1)
		c.setHandler(func(m map[string]any, _ time.Time) {
			if m["t"] == "welcome" || m["t"] == "error" {
				select {
				case first <- m:
				default:
				}
			}
		})
		c.start()
		m := guestHello(c, "flood.md", fmt.Sprint("Holder ", i), "", first)
		if m["t"] != "welcome" {
			c.close()
			l.mu.Lock()
			count(&l.holdingHello, fmt.Sprint(m["t"], " ", m["code"]))
			l.mu.Unlock()
			if !l.sleep(time.Second) {
				return
			}
			continue
		}
		l.guestsJoined.Add(1)
		l.mu.Lock()
		l.holdingJoins++
		l.mu.Unlock()
		for alive := true; alive && !l.stopped(); {
			if c.buffered() < 4 {
				c.send(map[string]any{"t": "ping", "padding": padding})
				l.mu.Lock()
				l.holdingPings++
				l.mu.Unlock()
			}
			select {
			case <-c.done:
				alive = false
			case <-l.stop:
			case <-time.After(1500 * time.Millisecond):
			}
		}
		l.guestsJoined.Add(-1)
		if e := c.endReason(); e != "" {
			l.mu.Lock()
			count(&l.holdingEnds, endKind(e))
			l.mu.Unlock()
		}
		c.close()
	}
}

// unpacedGuest sends updates far above the per-guest limit; when refused it
// rejoins with its token and a new client id.
func (l *loadRun) unpacedGuest(i int) {
	token := ""
	for n := 0; !l.stopped(); n++ {
		c := l.publicDial("flood.md", &l.unpacedHTTP)
		if c == nil {
			return
		}
		first := make(chan map[string]any, 1)
		var acked, busy, refusals atomic.Int64
		var refusal atomic.Value
		var joined atomic.Bool
		c.setHandler(func(m map[string]any, _ time.Time) {
			switch m["t"] {
			case "welcome":
				select {
				case first <- m:
				default:
				}
			case "ack":
				acked.Add(1)
			case "busy":
				busy.Add(1)
			case "error":
				if !joined.Load() {
					select {
					case first <- m:
					default:
					}
					return
				}
				refusals.Add(1)
				refusal.Store(fmt.Sprint(m["code"]))
			}
		})
		c.start()
		m := guestHello(c, "flood.md", fmt.Sprint("Unpaced ", i), token, first)
		if m["t"] != "welcome" {
			c.close()
			l.mu.Lock()
			count(&l.unpacedHello, fmt.Sprint(m["t"], " ", m["code"]))
			l.mu.Unlock()
			if !l.sleep(500 * time.Millisecond) {
				return
			}
			continue
		}
		joined.Store(true)
		l.guestsJoined.Add(1)
		if tk, ok := m["guest_token"].(string); ok {
			token = tk
		}
		client := uint64(30000 + 1000*i + n)
		clock := uint64(0)
		sent := 0
		for alive := true; alive && !l.stopped(); sent++ {
			text := fmt.Sprintf("[u%d-%d-%d]", i, n, sent)
			u := yInsert(client, text)
			if clock > 0 {
				u = yAfter(client, clock, text)
			}
			clock += uint64(len(text))
			c.send(map[string]any{"t": "update", "id": fmt.Sprintf("u%d-%d-%d", i, n, sent), "u": u})
			select {
			case <-c.done:
				alive = false
			case <-l.stop:
			case <-time.After(20 * time.Millisecond):
			}
		}
		l.guestsJoined.Add(-1)
		l.mu.Lock()
		l.unpacedJoined++
		l.unpacedSent += sent
		l.unpacedAcked += int(acked.Load())
		l.unpacedBusy += int(busy.Load())
		if r, ok := refusal.Load().(string); ok {
			count(&l.unpacedRefusals, r)
		} else if e := c.endReason(); e != "" {
			count(&l.unpacedRefusals, endKind(e))
		}
		l.mu.Unlock()
		c.close()
	}
}

// floodViewer fills a viewer slot in v1..v4 (10 each). Even-numbered
// viewers send padded pings above the Public byte buckets, odd ones small
// pings above the frame buckets.
func (l *loadRun) floodViewer(i int) {
	doc := fmt.Sprintf("v%d.md", i%4+1)
	padding := strings.Repeat("y", 350*1024)
	for !l.stopped() {
		c := l.publicDial(doc, &l.viewerHTTP)
		if c == nil {
			return
		}
		first := make(chan map[string]any, 1)
		c.setHandler(func(m map[string]any, _ time.Time) {
			if m["t"] == "welcome" {
				select {
				case first <- m:
				default:
				}
			}
		})
		c.start()
		c.send(map[string]any{"t": "hello", "v": 1, "doc": doc, "sv": "AA=="})
		joined := false
		select {
		case <-first:
			joined = true
		case <-c.done:
		case <-l.stop:
		case <-time.After(15 * time.Second):
		}
		if joined {
			l.viewersJoined.Add(1)
			l.mu.Lock()
			l.viewerJoins++
			l.mu.Unlock()
		}
		for alive := joined; alive && !l.stopped(); {
			wait := 30 * time.Millisecond
			if c.buffered() < 8 {
				if i%2 == 0 {
					c.send(map[string]any{"t": "ping", "padding": padding})
					wait = 1500 * time.Millisecond
				} else {
					c.send(map[string]any{"t": "ping"})
				}
				l.mu.Lock()
				l.viewerPings++
				if i%2 == 0 {
					l.viewerPadded++
				}
				l.mu.Unlock()
			}
			select {
			case <-c.done:
				alive = false
			case <-l.stop:
			case <-time.After(wait):
			}
		}
		if joined {
			l.viewersJoined.Add(-1)
		}
		if e := c.endReason(); e != "" {
			l.mu.Lock()
			count(&l.viewerEnds, endKind(e))
			l.mu.Unlock()
		}
		c.close()
		if !l.sleep(200 * time.Millisecond) {
			return
		}
	}
}

// churn opens extra Public connections above the pools, five a second.
func (l *loadRun) churn() {
	for l.sleep(200 * time.Millisecond) {
		docs := []string{"index.md", "v1.md", "flood.md"}
		l.mu.Lock()
		l.churnAttempts++
		attempt := l.churnAttempts
		doc := docs[attempt%len(docs)]
		l.mu.Unlock()
		c, err := dialLoad(l.url, doc, publicHeaders("edit", 1))
		if err != nil {
			l.mu.Lock()
			count(&l.churnOutcomes, err.Error())
			l.mu.Unlock()
			continue
		}
		first := make(chan map[string]any, 1)
		c.setHandler(func(m map[string]any, _ time.Time) {
			if m["t"] == "welcome" || m["t"] == "error" {
				select {
				case first <- m:
				default:
				}
			}
		})
		c.start()
		var outcome string
		// Alternate guest and viewer hellos.
		if attempt%2 == 0 {
			m := guestHello(c, doc, "Churn", "", first)
			outcome = fmt.Sprint("guest ", m["t"], " ", m["code"])
		} else {
			c.send(map[string]any{"t": "hello", "v": 1, "doc": doc, "sv": "AA=="})
			select {
			case m := <-first:
				outcome = fmt.Sprint("viewer ", m["t"], " ", m["code"])
			case <-c.done:
				outcome = "viewer " + c.endReason()
			case <-time.After(15 * time.Second):
				outcome = "viewer timeout"
			}
		}
		c.close()
		l.mu.Lock()
		count(&l.churnOutcomes, outcome)
		l.mu.Unlock()
	}
}

// checkAfterRollback: every text a writer created after the rollback
// returned and that was acknowledged before the final read is in it, in the writer's
// order. A missing text is looked up in the document's receipts.
func (l *loadRun) checkAfterRollback(t *testing.T, a *app, final string, finalAt time.Time, guests []*pacedGuest) {
	t.Helper()
	l.mu.Lock()
	after := l.rollbackEnd
	aTexts := slices.Clone(l.aTexts)
	l.mu.Unlock()
	rel := func(at time.Time) time.Duration { return at.Sub(l.t0).Round(time.Millisecond) }
	var db *sql.DB
	checked := 0
	check := func(who string, texts []loadText) {
		at := -1
		var missing, disordered []string
		for _, x := range texts {
			if x.acked.IsZero() || !x.acked.Before(finalAt) || !x.created.After(after) {
				continue
			}
			checked++
			i := strings.Index(final, x.text)
			if i < 0 {
				if db == nil {
					db = a.db(t)
				}
				receipt := "no receipt"
				var seq int64
				if err := db.QueryRow("SELECT seq FROM flats_docs_receipts WHERE doc='index.md' AND id=?", x.id).Scan(&seq); err == nil {
					receipt = fmt.Sprint("receipt seq ", seq)
				}
				missing = append(missing, fmt.Sprintf("%s (created %v, acked %v, %s)", x.text, rel(x.created), rel(x.acked), receipt))
				continue
			}
			if i <= at {
				disordered = append(disordered, x.text)
			}
			at = i
		}
		if len(missing) > 0 {
			t.Errorf("%s: %d acknowledged texts missing from the final text: %v", who, len(missing), missing[:min(len(missing), 5)])
		}
		if len(disordered) > 0 {
			t.Errorf("%s: %d texts out of order: %v", who, len(disordered), disordered[:min(len(disordered), 5)])
		}
	}
	check("member A", aTexts)
	for _, g := range guests {
		g.mu.Lock()
		texts := slices.Clone(g.written)
		g.mu.Unlock()
		check(fmt.Sprint("guest ", g.index), texts)
	}
	t.Logf("LOAD after the rollback (at %v): %d acknowledged texts checked", rel(after), checked)
}

// --- helpers ---

func pct(d []time.Duration, q float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := slices.Clone(d)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(math.Ceil(q*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	return s[i].Round(100 * time.Microsecond)
}

func uptime() string {
	out, err := exec.Command("uptime").Output()
	if err != nil {
		return errors.Join(errors.New("uptime"), err).Error()
	}
	return strings.TrimSpace(string(out))
}
