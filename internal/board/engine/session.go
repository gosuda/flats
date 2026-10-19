package engine

// SessionTTL is how long a session lives after its last activity, in
// seconds (4.8).
const SessionTTL = 24 * 60 * 60

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
	if se != nil && h.Now >= se.LastAt+SessionTTL {
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
	s.putSession(&Session{ID: h.NewID, Key: h.Key, Label: h.Label, Verified: h.Verified, LastAt: h.Now})
	return HelloResult{Session: h.NewID, New: true, Changed: s.changedSince(sp)}
}

// Sessions is the number of sessions, live or not yet expired by
// ExpireSessions.
func (s *State) Sessions() int { return len(s.sessions) }

// TouchSessions marks the sessions open sockets use as active at now, so a
// session does not expire while its socket stays connected. A session is
// written only when its last activity is at least an hour old.
func (s *State) TouchSessions(ids []string, now int64) Changed {
	m := s.Savepoint()
	sp := s.sps[m]
	for _, id := range ids {
		if se := s.sessions[id]; se != nil && now-se.LastAt >= 3600 {
			c := *se
			c.LastAt = now
			s.putSession(&c)
		}
	}
	c := s.changedSince(sp)
	s.Release(m)
	return c
}

// ExpireSessions deletes sessions idle for SessionTTL, with their created
// rows, receipts and undo stacks.
func (s *State) ExpireSessions(now int64) Changed {
	m := s.Savepoint()
	sp := s.sps[m]
	for id, se := range s.sessions {
		if now >= se.LastAt+SessionTTL {
			s.dropSession(id)
		}
	}
	c := s.changedSince(sp)
	s.Release(m)
	return c
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
