package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The Manager keeps one pre-started worker process ("spare") while it has an
// instance that is not stopped: a server flat or a logical document, asleep
// or not. The spare has finished exec, Go init and the flat-independent
// engine preparation, holds no flat's code or data, and waits for its spec.
// A spawn adopts it instead of starting a process; anything that goes wrong
// with it makes that spawn start a new process the ordinary way.

// spareDelay is how long after an instance became ready the spare starts
// (a server flat that started, or a registered document), so that
// short-lived instances (scratch trials, whose own start and health checks
// take longer than the delay) do not churn processes. Stopping the last
// instance before then cancels it. A variable for tests.
var spareDelay = 250 * time.Millisecond

// spareRefillDelay is how long after an adoption finished (the adopted
// worker reported ready, or the adoption failed) the replacement spare
// starts, so that its exec, Go init and preparation do not compete for CPU
// with the worker that was just woken and the requests that prepare it.
// A variable for tests.
var spareRefillDelay = time.Second

// A spare that exits before adoption is replaced after spareRetryMin,
// doubling up to spareRetryMax; it is never disabled for good.
const (
	spareRetryMin = time.Second
	spareRetryMax = 10 * time.Minute
)

// spare is a pre-started worker listening on its own socket path.
type spare struct {
	p    *proc
	sock string
}

type spareState struct {
	mu       sync.Mutex
	live     int    // instances that are not stopped
	closed   bool   // Close was called
	cur      *spare // waiting for adoption
	timer    *time.Timer
	due      time.Time      // when timer fires (tests)
	seq      uint64         // identifies timer; a stopped timer's callback does nothing
	starting bool           // a spare is being started outside mu
	wg       sync.WaitGroup // spare starts and stops in progress
	failures int            // spares that exited before adoption, in a row
}

// track counts an instance until the returned function (idempotent) is
// called when it stops. The instance requests the spare with instanceReady.
func (m *Manager) track() func() {
	s := &m.spare
	s.mu.Lock()
	s.live++
	s.mu.Unlock()
	var once sync.Once
	return func() { once.Do(m.untrack) }
}

// instanceReady requests a spare spareDelay after a tracked instance became
// ready, unless one exists or is already requested.
func (m *Manager) instanceReady() {
	s := &m.spare
	s.mu.Lock()
	m.scheduleSpareLocked(spareDelay)
	s.mu.Unlock()
}

// untrack stops the spare when the last instance stopped. It does not wait
// for the spare to exit, which may take until it finished preparing; Close
// does.
func (m *Manager) untrack() {
	s := &m.spare
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live--
	if s.live > 0 {
		return
	}
	m.stopTimerLocked()
	if sp := s.cur; sp != nil {
		s.cur = nil
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			sp.stop()
		}()
	}
}

// Close stops the spare, waits until every spare it or untrack stopped has
// exited, and prevents new ones. It does not stop instances.
func (m *Manager) Close() {
	s := &m.spare
	s.mu.Lock()
	s.closed = true
	m.stopTimerLocked()
	sp := s.cur
	s.cur = nil
	s.mu.Unlock()
	if sp != nil {
		sp.stop()
	}
	s.wg.Wait() // a spare started meanwhile stops itself; stops by untrack finish
}

// SparePID is the process id of the spare waiting for adoption, or 0.
func (m *Manager) SparePID() int {
	s := &m.spare
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return 0
	}
	return s.cur.p.cmd.Process.Pid
}

// SpareReady reports whether a spare is waiting for adoption, alive and done
// preparing, so that the next spawn takes the fast path.
func (m *Manager) SpareReady() bool {
	s := &m.spare
	s.mu.Lock()
	sp := s.cur
	s.mu.Unlock()
	if sp == nil || sp.p.warmAt.Load() == 0 {
		return false
	}
	select {
	case <-sp.p.done:
		return false
	default:
		return true
	}
}

// takeSpare detaches the spare for adoption; nil if there is none. The
// caller checks that it is still alive.
func (m *Manager) takeSpare() *spare {
	if m == nil {
		return nil
	}
	s := &m.spare
	s.mu.Lock()
	defer s.mu.Unlock()
	sp := s.cur
	s.cur = nil
	if sp != nil {
		s.failures = 0 // it lasted until it was needed
	}
	return sp
}

// refillSpare requests a new spare spareRefillDelay after an adoption
// finished, unless one exists or is already requested.
func (m *Manager) refillSpare() {
	s := &m.spare
	s.mu.Lock()
	m.scheduleSpareLocked(spareRefillDelay)
	s.mu.Unlock()
}

func (m *Manager) scheduleSpareLocked(delay time.Duration) {
	s := &m.spare
	if m.NoSpare || s.closed || s.live == 0 || s.cur != nil || s.starting || s.timer != nil {
		return
	}
	s.seq++
	seq := s.seq
	s.due = time.Now().Add(delay)
	s.timer = time.AfterFunc(delay, func() { m.startSpare(seq) })
}

func (m *Manager) stopTimerLocked() {
	s := &m.spare
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}

// startSpare runs from the timer with sequence seq.
func (m *Manager) startSpare(seq uint64) {
	s := &m.spare
	s.mu.Lock()
	if s.timer == nil || s.seq != seq { // stopped or replaced
		s.mu.Unlock()
		return
	}
	s.timer = nil
	if m.NoSpare || s.closed || s.live == 0 || s.cur != nil || s.starting {
		s.mu.Unlock()
		return
	}
	s.starting = true
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()

	sp, err := m.newSpare()
	s.mu.Lock()
	s.starting = false
	if err != nil {
		delay := m.spareFailedLocked(nil)
		s.mu.Unlock()
		m.logf("pre-started worker: %v; retrying in %s", err, delay)
		return
	}
	if s.closed || s.live == 0 {
		s.mu.Unlock()
		sp.stop()
		return
	}
	select {
	case <-sp.p.done: // exited before it was published; spareExited ignored it
		delay := m.spareFailedLocked(sp.p)
		s.mu.Unlock()
		os.Remove(sp.sock)
		m.logf("pre-started worker exited unexpectedly (%s); replacing it in %s", procFailure(sp.p), delay)
		return
	default:
	}
	s.cur = sp
	s.mu.Unlock()
}

// newSpare starts a pre-started worker process and sends it the prewarm line.
func (m *Manager) newSpare() (*spare, error) {
	if err := m.init(); err != nil {
		return nil, err
	}
	var rnd [8]byte
	rand.Read(rnd[:])
	sock := filepath.Join(m.sockDir, "s"+hex.EncodeToString(rnd[:])+".sock")
	lf, err := listenFile(sock)
	if err != nil {
		return nil, err
	}
	defer lf.Close()
	// Its own warm-up directory, which only it creates; startProc removes
	// it once it exited, also when it was killed or crashed in the warm-up.
	// Other entries of the scratch directory (other Managers' in a shared
	// temp directory) are never touched.
	warmDir := ""
	if m.scratchDir != "" {
		warmDir = filepath.Join(m.scratchDir, "s"+hex.EncodeToString(rnd[:]))
	}
	p, err := m.startProc(lf, nil, nil, warmDir)
	if err != nil {
		os.Remove(sock)
		return nil, err
	}
	sp := &spare{p: p, sock: sock}
	b, _ := json.Marshal(struct {
		Prewarm prewarmSpec `json:"prewarm"`
	}{prewarmSpec{CacheDir: m.cacheDir(), WarmDir: warmDir}})
	if _, err := p.stdin.Write(append(b, '\n')); err != nil {
		p.stopped.Store(true)
		p.cmd.Process.Kill()
		<-p.done
		os.Remove(sock)
		return nil, fmt.Errorf("start worker: %w", err)
	}
	return sp, nil
}

// spareExited is called when a process without an instance ends.
func (m *Manager) spareExited(p *proc) {
	s := &m.spare
	s.mu.Lock()
	sp := s.cur
	if sp == nil || sp.p != p { // stopped, being adopted, or not yet published
		s.mu.Unlock()
		return
	}
	s.cur = nil
	delay := m.spareFailedLocked(p)
	s.mu.Unlock()
	os.Remove(sp.sock)
	m.logf("pre-started worker exited unexpectedly (%s); replacing it in %s", procFailure(p), delay)
}

// spareFailedLocked schedules a replacement with backoff and returns its delay.
func (m *Manager) spareFailedLocked(p *proc) time.Duration {
	s := &m.spare
	if p != nil && time.Since(p.started) > time.Minute {
		s.failures = 0
	}
	s.failures++
	delay := spareRetryMin << min(s.failures-1, 10)
	delay = min(delay, spareRetryMax)
	m.scheduleSpareLocked(delay)
	return delay
}

func procFailure(p *proc) string {
	msg := fmt.Sprint(p.err)
	if t := strings.TrimSpace(p.tail.String()); t != "" {
		msg += ": " + lastLines(t, 5)
	}
	return msg
}

// stop ends a spare that was not adopted. It holds no flat, so it only
// needs a moment to exit on stdin EOF.
func (sp *spare) stop() {
	p := sp.p
	p.stopped.Store(true)
	p.stdin.Close()
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		p.cmd.Process.Kill()
		<-p.done
	}
	os.Remove(sp.sock)
}
