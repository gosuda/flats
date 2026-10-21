package engine

import (
	"cmp"
	"slices"

	"github.com/gosuda/flats/internal/board/model"
)

// SessionTTL is how long a session lives after its last activity, in
// seconds (4.8).
const SessionTTL = 24 * 60 * 60

// GuestSessionTTL is how long a guest's session lives after its last
// socket closed (TECH public edit 3.2).
const GuestSessionTTL = 60 * 60

// ttl is the lifetime of a session after its last activity.
func (se *Session) ttl() int64 {
	if model.IsGuestKey(se.Key) {
		return GuestSessionTTL
	}
	return SessionTTL
}

// expired reports whether the session is past its lifetime at now.
func (se *Session) expired(now int64) bool { return now >= se.LastAt+se.ttl() }

// Hello is a connection's request to start or resume a session (4.8).
type Hello struct {
	Session  string // the session the client holds, or ""
	Pending  int    // the client's unconfirmed batches
	NewID    string // a fresh random id for a new session
	Key      string // editor key the server decided
	Label    string
	Verified bool
	Now      int64
}

// HelloResult answers a Hello. Fatal is set when the client must stop.
type HelloResult struct {
	Session string
	New     bool
	Fatal   string
	Changed Changed
}

// Hello resumes a live session of the same editor, or starts a new one
// when the client has no unconfirmed batches. A client with pending batches
// whose session is gone gets FatalSessionExpired.
func (s *State) Hello(h Hello) HelloResult {
	m := s.Savepoint()
	sp := s.sps[m]
	defer s.Release(m)
	se := s.sessions[h.Session]
	if se != nil && se.expired(h.Now) {
		s.dropSession(se.ID)
		se = nil
	}
	if se != nil && se.Key == h.Key {
		c := *se
		c.Label, c.Verified, c.LastAt = h.Label, h.Verified, h.Now
		s.putSession(&c)
		return HelloResult{Session: c.ID, Changed: s.changedSince(sp)}
	}
	if h.Pending > 0 {
		return HelloResult{Fatal: FatalSessionExpired, Changed: s.changedSince(sp)}
	}
	s.putSession(&Session{ID: h.NewID, Key: h.Key, Label: h.Label, Verified: h.Verified, LastAt: h.Now, StartSeq: s.seq})
	return HelloResult{Session: h.NewID, New: true, Changed: s.changedSince(sp)}
}

// Sessions is the number of sessions, live or not yet expired by
// ExpireSessions.
func (s *State) Sessions() int { return len(s.sessions) }

// TouchSessions marks the sessions open sockets use as active at now, so a
// session does not expire while its socket stays connected. A session is
// written only when its last activity is at least an hour old, a guest's
// a minute, so that a guest session ends about an hour after its last
// socket closed.
func (s *State) TouchSessions(ids []string, now int64) Changed {
	m := s.Savepoint()
	sp := s.sps[m]
	for _, id := range ids {
		se := s.sessions[id]
		if se == nil {
			continue
		}
		every := int64(3600)
		if model.IsGuestKey(se.Key) {
			every = 60
		}
		if now-se.LastAt >= every {
			c := *se
			c.LastAt = now
			s.putSession(&c)
		}
	}
	c := s.changedSince(sp)
	s.Release(m)
	return c
}

// ExpireSessions deletes sessions idle for their lifetime (SessionTTL, a
// guest's GuestSessionTTL), with their created rows, receipts and undo
// stacks.
func (s *State) ExpireSessions(now int64) Changed {
	m := s.Savepoint()
	sp := s.sps[m]
	for id, se := range s.sessions {
		if se.expired(now) {
			s.dropSession(id)
		}
	}
	c := s.changedSince(sp)
	s.Release(m)
	return c
}

// CapGuestSessions drops the least recently active guest sessions without
// an open socket until at most limit guest sessions are left (TECH public
// edit 3.2).
func (s *State) CapGuestSessions(limit int, open map[string]bool) Changed {
	m := s.Savepoint()
	sp := s.sps[m]
	var guests []*Session
	for _, se := range s.sessions {
		if model.IsGuestKey(se.Key) {
			guests = append(guests, se)
		}
	}
	slices.SortFunc(guests, func(a, b *Session) int { return cmp.Or(cmp.Compare(a.LastAt, b.LastAt), cmp.Compare(a.ID, b.ID)) })
	over := len(guests) - limit
	for _, se := range guests {
		if over <= 0 {
			break
		}
		if !open[se.ID] {
			s.dropSession(se.ID)
			over--
		}
	}
	c := s.changedSince(sp)
	s.Release(m)
	return c
}

// LiveSession reports whether an unexpired session of editor key exists.
func (s *State) LiveSession(key string, now int64) bool {
	for _, se := range s.sessions {
		if se.Key == key && !se.expired(now) {
			return true
		}
	}
	return false
}

// GuestSession reports whether session id belongs to a guest.
func (s *State) GuestSession(id string) bool {
	se := s.sessions[id]
	return se != nil && model.IsGuestKey(se.Key)
}

// GuestCreated is the number of created rows of every guest session.
func (s *State) GuestCreated() int {
	n := 0
	for id, se := range s.sessions {
		if model.IsGuestKey(se.Key) {
			n += s.createdN[id]
		}
	}
	return n
}

func (s *State) dropSession(id string) {
	s.deleteSession(id)
	for k := range s.created {
		if k.session == id {
			s.setCreated(k, 0)
		}
	}
	for k := range s.receipts {
		if k.Session == id {
			s.deleteReceipt(k)
		}
	}
	s.deleteStacks(id)
}
