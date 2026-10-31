package provider

import "context"

// slugGate serializes one slug's opens against its retirement. Opens share
// the gate with each other; StopSlug holds it alone. A waiting retirement
// admits no new open, so a stream of opens cannot starve it. An open files
// every route it registers under its request slug and StopSlug retires only
// its own slug's routes, so gates of different slugs never exclude each other.
type slugGate struct {
	opens    int           // ServeExposure calls holding the gate
	retiring bool          // a StopSlug call holds the gate
	waiting  int           // StopSlug calls waiting for the gate
	users    int           // holders and waiters; the entry is deleted at zero
	changed  chan struct{} // closed and replaced when a waiter may proceed
}

// wake lets every waiter re-check the gate. Called with Manager.gateMu held.
func (g *slugGate) wake() {
	close(g.changed)
	g.changed = make(chan struct{})
}

// enterSlug waits for slug's gate: shared for an open, exclusive for a
// retirement. Admission is checked before ctx, so an uncontended call
// behaves the same whether or not ctx is done; a cancelled wait returns
// ctx.Err() and leaves the gate as it found it. leave releases the gate.
func (m *Manager) enterSlug(ctx context.Context, slug string, retire bool) (leave func(), err error) {
	m.gateMu.Lock()
	if m.gates == nil {
		m.gates = map[string]*slugGate{}
	}
	g := m.gates[slug]
	if g == nil {
		g = &slugGate{changed: make(chan struct{})}
		m.gates[slug] = g
	}
	g.users++
	if retire {
		g.waiting++
	}
	for {
		if retire && !g.retiring && g.opens == 0 {
			g.waiting--
			g.retiring = true
			break
		}
		if !retire && !g.retiring && g.waiting == 0 {
			g.opens++
			break
		}
		changed := g.changed
		m.gateMu.Unlock()
		select {
		case <-changed:
			m.gateMu.Lock()
		case <-ctx.Done():
			m.gateMu.Lock()
			if retire {
				g.waiting--
				// Opens held back only by this waiting retirement may go.
				g.wake()
			}
			m.leaveGateLocked(slug, g)
			m.gateMu.Unlock()
			return nil, ctx.Err()
		}
	}
	m.gateMu.Unlock()
	return func() {
		m.gateMu.Lock()
		defer m.gateMu.Unlock()
		if retire {
			g.retiring = false
		} else {
			g.opens--
		}
		g.wake()
		m.leaveGateLocked(slug, g)
	}, nil
}

// leaveGateLocked drops one user of g and deletes slug's entry with the
// last one. Called with Manager.gateMu held.
func (m *Manager) leaveGateLocked(slug string, g *slugGate) {
	g.users--
	if g.users == 0 {
		delete(m.gates, slug)
	}
}
