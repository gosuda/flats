package notebook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	spillBudget   = int64(1 << 30)
	spillFloor    = int64(1 << 20)
	spillInterval = 10 * time.Second
	spillIdle     = 60 * time.Second
)

var (
	errSpillExpired     = errors.New("conflict:read_expired: cursor expired; read again")
	errSpillUnavailable = errors.New("unavailable: snapshot capture interrupted; retry")
)

// Scope selects the content and projection captured in one read transaction.
type Scope struct {
	Tree     bool
	Page     ID
	Database ID
	View     *ViewDef
	Public   bool
}

type spillRequest struct {
	scope      Scope
	cancel     context.CancelCauseFunc
	estimating bool
	started    time.Time
}

type spillWaiter struct {
	bytes             int64
	scope             Scope
	ready             chan struct{}
	admitted          bool
	fitting, reserved bool
	err               error
	manager           *SpillManager
	agent, ticket     string
	expires           time.Time
}

// SpillManager accounts for scratch files across captures and readers.
// Its directory is a stable host-owned namespace; live process owners are isolated.
type SpillManager struct {
	*spillState
	requests   map[*spillRequest]bool
	wrapWriter func(io.Writer) io.Writer
	parent     *SpillManager
}

type spillState struct {
	mu              sync.Mutex
	dir             string
	budget, used    int64
	now             func() time.Time
	spills          map[string]*Spill
	over            *Spill
	queue           []*spillWaiter
	timer           *time.Timer
	timerGeneration uint64
	initErr         error
	owners          map[*SpillManager]bool
	ownerLock       *os.File
	ownerDir        string
	closed          bool
}

// Spill is an immutable disk-backed scope. Delivery state is manager-owned.
type Spill struct {
	manager                             *SpillManager
	scope                               Scope
	token, dir                          string
	reserved, stored                    int64
	over, capturing, removed, traversed bool
	high                                int
	last, ended, contended, progressAt  time.Time
	written, checkpoint                 int64
	cancel                              context.CancelCauseFunc
	data, records, index                *os.File
	handles                             map[*os.File]bool
	parts                               int
	root, schemaNodes, windows          int64
	// Sequence is the read transaction's capture barrier, not a later live state.
	Sequence   Sequence
	cleanupErr error
}

// NewSpillManager uses a 1 GiB fitting-spill budget when budget is nonpositive.
func NewSpillManager(dir string, budget int64, now func() time.Time) *SpillManager {
	if budget <= 0 {
		budget = spillBudget
	}
	if now == nil {
		now = time.Now
	}
	m := &SpillManager{spillState: &spillState{budget: budget, now: now, spills: map[string]*Spill{}, owners: map[*SpillManager]bool{}}, requests: map[*spillRequest]bool{}}
	m.owners[m] = true
	m.initErr = m.openOwner(dir)
	return m
}

func spillName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) || len(name) != len(prefix)+32 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(name, prefix))
	return err == nil
}

func (m *SpillManager) openOwner(root string) error {
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	namespace, err := os.OpenFile(filepath.Join(root, ".owners"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer namespace.Close()
	if err := syscall.Flock(int(namespace.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(namespace.Fd()), syscall.LOCK_UN)
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if spillName(entry.Name(), "notebook-spill-") {
			if err := os.RemoveAll(path); err != nil {
				return err
			}
			continue
		}
		if !entry.IsDir() || !spillName(entry.Name(), "owner-") {
			continue
		}
		lock, err := os.OpenFile(filepath.Join(path, ".owner"), os.O_CREATE|os.O_RDWR, 0600)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			err = os.RemoveAll(path)
		}
		lock.Close()
		if err != nil && !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
	}
	token, err := randomNotebookToken()
	if err != nil {
		return err
	}
	// Use fixed-size names so discovery never treats unrelated directories as owners.
	sum := contentHash([]byte(token))
	m.ownerDir = filepath.Join(root, "owner-"+sum[:32])
	m.dir = filepath.Join(m.ownerDir, "spills")
	if err := os.MkdirAll(m.dir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(m.ownerDir, ".owner"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		lock.Close()
		return err
	}
	m.ownerLock = lock
	_, err = fmt.Fprintf(lock, "%d\n", os.Getpid())
	return err
}

func (m *SpillManager) forInstance() *SpillManager {
	m.mu.Lock()
	defer m.mu.Unlock()
	child := &SpillManager{spillState: m.spillState, requests: map[*spillRequest]bool{}, parent: m}
	m.owners[child] = true
	return child
}

// Close releases the host owner's captures and scratch directory.
func (m *SpillManager) Close() error {
	m.CancelScope(func(Scope) bool { return true })
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	m.timerGeneration++
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	err := os.RemoveAll(m.ownerDir)
	if m.ownerLock != nil {
		err = errors.Join(err, m.ownerLock.Close())
		m.ownerLock = nil
	}
	return err
}

type agentAdmissionKey struct{}
type agentAdmission struct{ query, ticket string }

func (m *SpillManager) retryLocked(w *spillWaiter) error {
	position := 0
	for _, queued := range m.queue {
		if queued.err == nil {
			position++
		}
		if queued == w {
			break
		}
	}
	hint, _ := json.Marshal(struct {
		Admission string `json:"admission"`
		Position  int    `json:"position"`
		Expires   int64  `json:"expires_in_ms"`
		Retry     string `json:"retry"`
	}{w.ticket, position, max(int64(1), w.expires.Sub(m.now()).Milliseconds()), "repeat the read with retry_hint set to admission before expiry"})
	return &WriteError{Code: "unavailable", Reason: "spill_admission", Retry: true, Detail: string(hint)}
}

func (m *SpillManager) acquire(ctx context.Context, bytes int64, scope Scope) (*Spill, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.initErr != nil || m.closed {
		return nil, errSpillUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.expireLocked()
	agent, isAgent := ctx.Value(agentAdmissionKey{}).(agentAdmission)
	var w *spillWaiter
	if isAgent && agent.ticket != "" {
		for _, queued := range m.queue {
			if queued.manager == m && queued.agent == agent.query && queued.ticket == agent.ticket && queued.err == nil {
				queued.expires = m.now().Add(spillInterval)
				w = queued
				break
			}
		}
	}
	if isAgent && agent.ticket != "" && w == nil {
		return nil, expiredRead()
	}
	keep, consumed := false, false
	defer func() {
		if !keep {
			for i, q := range m.queue {
				if q == w {
					m.queue = append(m.queue[:i], m.queue[i+1:]...)
					break
				}
			}
			if !consumed && w != nil && w.reserved && w.fitting {
				m.used -= w.bytes
			}
		}
		m.admitLocked()
		m.scheduleLocked()
	}()
	if w == nil {
		w = &spillWaiter{bytes: bytes, scope: scope, ready: make(chan struct{}), manager: m, agent: agent.query}
		if bytes <= m.budget-m.used {
			w.admitted, w.fitting = true, true
		} else if m.over == nil && !m.overGrantedLocked() && !m.waitingLocked() {
			w.admitted = true
		} else {
			if isAgent {
				var err error
				w.ticket, err = randomNotebookToken()
				if err != nil {
					return nil, err
				}
				w.expires = m.now().Add(spillInterval)
			}
			m.queue = append(m.queue, w)
			m.admitLocked()
		}
	}
	if isAgent && !w.admitted && w.err == nil {
		keep = true
		return nil, m.retryLocked(w)
	}
	for !w.admitted && w.err == nil {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-w.ready:
		}
		m.mu.Lock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if w.err != nil {
		return nil, w.err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	s := &Spill{manager: m, scope: scope, token: hex.EncodeToString(token[:]), reserved: w.bytes, capturing: true, high: -1, progressAt: m.now(), handles: map[*os.File]bool{}}
	if w.fitting {
		if !w.reserved {
			m.used += w.bytes
		}
	} else {
		s.over = true
		m.over = s
	}
	s.dir = filepath.Join(m.dir, "notebook-spill-"+s.token)
	m.spills[s.token] = s
	consumed = true
	return s, nil
}

func (m *SpillManager) scheduleLocked() {
	m.timerGeneration++
	generation := m.timerGeneration
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	now := m.now()
	delay := spillIdle
	active := len(m.spills) > 0
	for _, w := range m.queue {
		if w.agent != "" {
			active = true
			delay = min(delay, w.expires.Sub(now))
		}
	}
	for owner := range m.owners {
		for r := range owner.requests {
			if r.estimating {
				active = true
				delay = min(delay, r.started.Add(spillInterval).Sub(now))
			}
		}
	}
	if !active {
		return
	}
	for _, s := range m.spills {
		deadline := s.last.Add(spillIdle)
		if s.capturing {
			deadline = s.progressAt.Add(spillInterval)
		} else if s.over && m.waitingLocked() {
			deadline = minTime(deadline, maxTime(s.ended, s.contended, s.last).Add(spillInterval))
		}
		delay = min(delay, deadline.Sub(now))
	}
	m.timer = time.AfterFunc(max(delay, time.Nanosecond), func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if generation != m.timerGeneration {
			return
		}
		m.timer = nil
		m.expireLocked()
		m.scheduleLocked()
	})
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (m *SpillManager) contentionLocked() {
	if m.over == nil || !m.waitingLocked() {
		return
	}
	s := m.over
	if s.contended.IsZero() {
		s.contended = m.now()
	}
	if !s.capturing && s.traversed {
		m.removeLocked(s, errSpillExpired)
	}
}

func (m *SpillManager) waitingLocked() bool {
	for _, w := range m.queue {
		if !w.admitted && w.err == nil {
			return true
		}
	}
	return false
}
func (m *SpillManager) overGrantedLocked() bool {
	for _, w := range m.queue {
		if w.admitted && !w.fitting && w.err == nil {
			return true
		}
	}
	return false
}

func (m *SpillManager) admitLocked() {
	for _, w := range m.queue {
		if w.err != nil || w.fitting || w.bytes > m.budget-m.used {
			continue
		}
		m.used += w.bytes
		w.fitting = true
		w.reserved = true
		if !w.admitted {
			w.admitted = true
			close(w.ready)
		}
	}
	if m.over == nil && !m.overGrantedLocked() {
		for _, w := range m.queue {
			if !w.admitted && w.err == nil {
				w.admitted = true
				w.reserved = true
				close(w.ready)
				break
			}
		}
	}
	if !m.waitingLocked() {
		if m.over != nil {
			m.over.contended = time.Time{}
		}
		return
	}
	m.contentionLocked()
}

func (m *SpillManager) expireLocked() {
	now := m.now()
	for i := len(m.queue) - 1; i >= 0; i-- {
		w := m.queue[i]
		if w.agent == "" || now.Before(w.expires) {
			continue
		}
		if w.reserved && w.fitting {
			m.used -= w.bytes
			w.reserved = false
		}
		w.err = errSpillExpired
		if !w.admitted {
			close(w.ready)
		}
		m.queue = append(m.queue[:i], m.queue[i+1:]...)
	}
	for owner := range m.owners {
		for r := range owner.requests {
			if r.estimating && now.Sub(r.started) >= spillInterval {
				r.estimating = false
				r.cancel(errSpillUnavailable)
			}
		}
	}
	for _, s := range m.spills {
		if s.capturing {
			if now.Sub(s.progressAt) >= spillInterval {
				if s.written-s.checkpoint < spillFloor || now.Sub(s.progressAt) >= 2*spillInterval {
					m.removeLocked(s, errSpillUnavailable)
					continue
				}
				s.checkpoint = s.written
				s.progressAt = now
			}
			continue
		}
		expired := now.Sub(s.last) >= spillIdle
		if s.over && m.waitingLocked() {
			expired = expired || s.traversed || now.Sub(maxTime(s.ended, s.contended, s.last)) >= spillInterval
		}
		if expired {
			m.removeLocked(s, errSpillExpired)
		}
	}
	m.admitLocked()
}

func maxTime(times ...time.Time) time.Time {
	var out time.Time
	for _, t := range times {
		if t.After(out) {
			out = t
		}
	}
	return out
}

func (m *SpillManager) removeLocked(s *Spill, cause error) {
	if s.removed {
		return
	}
	s.removed = true
	if s.cancel != nil {
		s.cancel(cause)
	}
	for f := range s.handles {
		_ = f.Close()
		delete(s.handles, f)
	}
	delete(m.spills, s.token)
	if s.over {
		m.over = nil
	} else {
		m.used -= s.reserved
	}
	// Close before releasing admission: unlinked open files still occupy scratch.
	if err := os.RemoveAll(s.dir); err != nil {
		s.cleanupErr = err
		m.initErr = errors.Join(m.initErr, err)
		for _, w := range m.queue {
			w.err = errSpillUnavailable
			if !w.admitted {
				close(w.ready)
			}
		}
		m.queue = nil
	}
	m.admitLocked()
}

func (s *Spill) charge(n int64) error {
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	if s.removed {
		return errSpillUnavailable
	}
	next := s.stored + n
	if !s.over && next > s.reserved {
		extra := next - s.reserved
		if extra <= m.budget-m.used {
			m.used += extra
			s.reserved = next
		} else {
			if m.over != nil || m.overGrantedLocked() || m.waitingLocked() {
				return errSpillUnavailable
			}
			m.used -= s.reserved
			s.over = true
			m.over = s
			s.reserved = next
		}
	}
	s.stored = next
	return nil
}

func (s *Spill) progress(n int) error {
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	s.written += int64(n)
	m.expireLocked()
	if s.removed {
		return errSpillUnavailable
	}
	return nil
}

// Cursor returns the opaque manager-local capture identity. Access is checked by the caller.
func (s *Spill) Cursor() string { return s.token }

// Close releases this spill without cancelling readers of the same content.
func (s *Spill) Close() error {
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeLocked(s, errSpillExpired)
	return s.cleanupErr
}

// Lookup resolves a live capture without renewing its delivery clock.
func (m *SpillManager) Lookup(cursor string) (*Spill, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	s, ok := m.spills[cursor]
	return s, ok && !s.capturing && (m.parent == nil || s.manager == m)
}

// CancelScope removes captures, disconnected spills, and queued matching reads.
func (m *SpillManager) CancelScope(match func(Scope) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for owner := range m.owners {
		if m.parent != nil && owner != m {
			continue
		}
		for r := range owner.requests {
			if match(r.scope) {
				r.cancel(errSpillExpired)
			}
		}
	}
	for i := len(m.queue) - 1; i >= 0; i-- {
		w := m.queue[i]
		if (m.parent == nil || w.manager == m) && match(w.scope) {
			if w.reserved && w.fitting {
				m.used -= w.bytes
				w.reserved = false
			}
			w.err = errSpillExpired
			if !w.admitted {
				close(w.ready)
			}
			m.queue = append(m.queue[:i], m.queue[i+1:]...)
		}
	}
	for _, s := range m.spills {
		if (m.parent == nil || s.manager == m) && match(s.scope) {
			m.removeLocked(s, errSpillExpired)
		}
	}
	m.admitLocked()
}
