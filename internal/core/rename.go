package core

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/oesni/flats/internal/slug"
	"github.com/oesni/flats/internal/store"
)

// RenameSlug changes a flat's slug (and therefore its addresses). The old
// addresses redirect to the new ones, keeping path and query, for the
// configured redirect window (7 days by default).
func (s *Service) RenameSlug(ctx context.Context, from, to string, via Via) (FlatView, error) {
	if err := slug.Validate(to); err != nil {
		return FlatView{}, err
	}
	if from == to {
		return s.GetFlat(ctx, from)
	}
	if _, err := s.st.GetFlat(ctx, to); err == nil {
		return FlatView{}, fmt.Errorf("%w: flat %q already exists", ErrConflict, to)
	}
	unlockFrom := s.lock(from)
	defer unlockFrom()
	unlockTo := s.lock(to)
	defer unlockTo()
	f, err := s.st.GetFlat(ctx, from)
	if err != nil {
		return FlatView{}, err
	}
	s.dropPreviews(ctx, from)
	s.mu.Lock()
	lf := s.live[from]
	delete(s.live, from)
	s.mu.Unlock()
	wasPublic, wasPrivate := false, false
	if lf != nil {
		if lf.publicServed && s.cfg.Public != nil {
			_ = s.cfg.Public.Stop(from)
			wasPublic = true
		}
		if lf.privateServed {
			_ = s.cfg.Private.Stop(from)
			wasPrivate = true
		}
	}
	till := s.now().Add(s.redirectWindow())
	if err := os.Rename(s.flatDir(from), s.flatDir(to)); err != nil && !os.IsNotExist(err) {
		return FlatView{}, err
	}
	if err := s.st.RenameFlat(ctx, from, to, till, s.now()); err != nil {
		_ = os.Rename(s.flatDir(to), s.flatDir(from))
		return FlatView{}, err
	}
	// Move the in-memory state under the new name; the live handler closes
	// over the state object, so rebuild handlers bound to the new slug.
	if lf != nil {
		s.mu.Lock()
		nl := &liveFlat{limiter: lf.limiter}
		if d := lf.cur.Load(); d != nil {
			nd := *d
			// Static handlers reference the version directory, which moved.
			if d.inst == nil {
				if rebuilt, err := s.build(ctx, to, d.version, s.dataDirOf(to)); err == nil {
					nd = *rebuilt
				}
			}
			nl.cur.Store(&nd)
		}
		s.live[to] = nl
		s.mu.Unlock()
	}
	f.Slug = to
	if err := s.ensureExposure(ctx, f); err != nil {
		s.Event(ctx, to, "error", "exposure", err.Error(), nil)
	}
	if wasPrivate || wasPublic {
		s.serveRedirect(ctx, from, to)
	}
	s.Event(ctx, to, "info", "rename", fmt.Sprintf("renamed %s -> %s via %s; old addresses redirect until %s", from, to, via, till.Format("2006-01-02 15:04 MST")), nil)
	return s.GetFlat(ctx, to)
}

func (s *Service) redirectHandler(newBase func() string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := strings.TrimSuffix(newBase(), "/") + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	})
}

// serveRedirect serves old -> new redirects on the networks the flat uses.
func (s *Service) serveRedirect(ctx context.Context, old, cur string) {
	f, err := s.st.GetFlat(ctx, cur)
	if err != nil {
		return
	}
	if _, err := s.cfg.Private.Serve(ctx, old, s.redirectHandler(func() string { return s.cfg.Private.URL(cur) }), false); err != nil {
		s.Event(ctx, cur, "error", "rename", "could not serve private redirect from "+old+": "+err.Error(), nil)
	}
	if f.Visibility.Public() && s.cfg.Public != nil {
		if _, err := s.cfg.Public.Serve(ctx, old, s.redirectHandler(func() string { return s.cfg.Public.URL(cur) }), true); err != nil {
			s.Event(ctx, cur, "error", "rename", "could not serve public redirect from "+old+": "+err.Error(), nil)
		}
	}
	s.mu.Lock()
	s.redir[old] = cur
	s.mu.Unlock()
}

func (s *Service) stopRedirect(old string) {
	s.mu.Lock()
	cur, ok := s.redir[old]
	delete(s.redir, old)
	s.mu.Unlock()
	if !ok {
		return
	}
	_ = s.cfg.Private.Stop(old)
	if s.cfg.Public != nil {
		_ = s.cfg.Public.Stop(old)
	}
	s.Event(context.Background(), cur, "info", "rename", "redirect from "+old+" expired", nil)
}

// Redirects lists active old->new redirects.
func (s *Service) Redirects() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.redir))
	for k, v := range s.redir {
		out[k] = v
	}
	return out
}

var _ = store.Private
