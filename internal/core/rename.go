package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/slug"
	"github.com/gosuda/flats/internal/store"
)

// RenameSlug changes a flat's slug (and therefore its addresses). The old
// addresses redirect to the new ones, keeping path and query, for the
// configured redirect window (7 days by default).
func (s *Service) RenameSlug(ctx context.Context, from, to string, via Via) (FlatView, error) {
	if err := slug.Validate(to); err != nil {
		return FlatView{}, invalid(err)
	}
	if from == to {
		return s.GetFlat(ctx, from)
	}
	// Lock both slugs in one order, so two renames cannot deadlock, and
	// check the target only while holding both.
	first, second := min(from, to), max(from, to)
	unlockFirst := s.lock(first)
	defer unlockFirst()
	unlockSecond := s.lock(second)
	defer unlockSecond()
	if err := s.checkReserved(ctx, to, from); err != nil {
		return FlatView{}, err
	}
	f, err := s.st.GetFlat(ctx, from)
	if err != nil {
		return FlatView{}, err
	}
	if _, err := s.st.GetFlat(ctx, to); err == nil {
		return FlatView{}, fmt.Errorf("%w: flat %q already exists", ErrConflict, to)
	} else if !errors.Is(err, store.ErrNotFound) {
		return FlatView{}, err
	}
	s.dropPreviews(ctx, from) // their data lives under the flat directory
	s.mu.Lock()
	lf := s.live[from]
	s.mu.Unlock()
	// Keep the old nodes/exposures: serveRedirect swaps their handlers to
	// redirects, so the old addresses keep resolving without a new login.
	wasPublic, wasPrivate := false, false
	if lf != nil {
		wasPublic = lf.publicServed && s.cfg.Public != nil
		wasPrivate = lf.privateServed
		// Page views are stored per slug and follow the rename in the DB.
		s.flushViews(ctx, from, lf)
	}
	// Only a flat that was served gets a redirect (and reserves its old slug).
	var till time.Time
	if wasPrivate || wasPublic {
		till = s.now().Add(s.redirectWindow())
	}
	moved := false
	if err := os.Rename(s.flatDir(from), s.flatDir(to)); err == nil {
		moved = true
	} else if !os.IsNotExist(err) {
		return FlatView{}, err
	}
	if err := s.st.RenameFlat(ctx, from, to, till, s.now()); err != nil {
		if moved {
			_ = os.Rename(s.flatDir(to), s.flatDir(from))
		}
		if errors.Is(err, store.ErrExists) {
			return FlatView{}, withKind(ErrConflict, err)
		}
		return FlatView{}, err
	}

	// The rename is committed: move the in-memory state to the new slug.
	s.mu.Lock()
	delete(s.live, from)
	own := s.redir[to] // to was this flat's own earlier slug
	delete(s.redir, to)
	var chained []string
	for old, r := range s.redir {
		if r.cur == from {
			chained = append(chained, old)
		}
	}
	s.mu.Unlock()
	if lf != nil {
		// The same state object keeps its counters and in-flight requests;
		// the served flags start over for the new host names.
		lf.privateServed, lf.publicServed = false, false
		if d := lf.cur.Load(); d != nil {
			// Handlers reference the flat directory, which moved: rebuild.
			if rebuilt, err := s.build(ctx, to, d.version, s.dataDirOf(to)); err == nil {
				lf.cur.Store(rebuilt)
				if d.inst != nil {
					d.inst.Stop()
				}
			} else {
				s.Event(ctx, to, "error", "rename", "could not restart the live version after rename: "+err.Error(), nil)
			}
		}
		s.mu.Lock()
		s.live[to] = lf
		s.mu.Unlock()
	}
	f.Slug = to
	if err := s.ensureExposure(ctx, f); err != nil {
		s.Event(ctx, to, "error", "exposure", err.Error(), nil)
	}
	if own != nil {
		// to was a redirect to this flat; ensureExposure took its private
		// host over. A public redirect stays only if the flat is public now.
		if own.public && !f.Visibility.Public() && s.cfg.Public != nil {
			_ = s.cfg.Public.Stop(to)
		}
		if lf == nil || lf.cur.Load() == nil {
			_ = s.cfg.Private.Stop(to)
		}
	}
	// Redirects to the old slug now lead to the new one directly.
	for _, old := range chained {
		s.serveRedirect(ctx, old, to)
	}
	if !till.IsZero() {
		s.serveRedirect(ctx, from, to)
		s.Event(ctx, to, "info", "rename", fmt.Sprintf("renamed %s -> %s via %s; old addresses redirect until %s", from, to, via, till.Format("2006-01-02 15:04 MST")), nil)
	} else {
		s.Event(ctx, to, "info", "rename", fmt.Sprintf("renamed %s -> %s via %s", from, to, via), nil)
	}
	return s.GetFlat(ctx, to)
}

// redirectHandler redirects to the same path under newBase(). The redirect
// is temporary (307): it ends with the redirect window, so browsers must not
// cache it. While the new address is not known yet (a Portal exposure that
// is still starting), it answers 503 instead of redirecting to itself.
func (s *Service) redirectHandler(newBase func() string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := strings.TrimSuffix(newBase(), "/")
		if base == "" {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "this flat moved; its new address is not ready yet, try again shortly", http.StatusServiceUnavailable)
			return
		}
		http.Redirect(w, r, base+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	})
}

// serveRedirect serves old -> cur redirects on the networks the flat uses.
func (s *Service) serveRedirect(ctx context.Context, old, cur string) {
	f, err := s.st.GetFlat(ctx, cur)
	if err != nil {
		return
	}
	if _, err := s.cfg.Private.Serve(ctx, old, s.redirectHandler(func() string { return s.cfg.Private.URL(cur) }), false); err != nil {
		s.Event(ctx, cur, "error", "rename", "could not serve private redirect from "+old+": "+err.Error(), nil)
	}
	s.mu.Lock()
	prev := s.redir[old]
	s.mu.Unlock()
	public := prev != nil && prev.public
	if f.Visibility.Public() && s.cfg.Public != nil {
		if _, err := s.cfg.Public.Serve(ctx, old, s.redirectHandler(func() string { return s.cfg.Public.URL(cur) }), true); err != nil {
			s.Event(ctx, cur, "error", "rename", "could not serve public redirect from "+old+": "+err.Error(), nil)
		} else {
			public = true
		}
	}
	s.mu.Lock()
	s.redir[old] = &redirect{cur: cur, public: public}
	s.mu.Unlock()
}

// stopPublicRedirects stops the public redirects to cur, which is no longer
// public: they would point at an address that does not answer.
func (s *Service) stopPublicRedirects(cur string) {
	if s.cfg.Public == nil {
		return
	}
	var olds []string
	s.mu.Lock()
	for old, r := range s.redir {
		if r.cur == cur && r.public {
			r.public = false
			olds = append(olds, old)
		}
	}
	s.mu.Unlock()
	for _, old := range olds {
		_ = s.cfg.Public.Stop(old)
	}
}

func (s *Service) stopRedirect(old string) {
	s.mu.Lock()
	r, ok := s.redir[old]
	delete(s.redir, old)
	s.mu.Unlock()
	if !ok {
		return
	}
	_ = s.cfg.Private.Stop(old)
	if r.public && s.cfg.Public != nil {
		_ = s.cfg.Public.Stop(old)
	}
	s.Event(context.Background(), r.cur, "info", "rename", "redirect from "+old+" expired", nil)
}

// syncRedirects re-points or stops served redirects to match the database:
// a redirect follows later renames of its flat and ends with its window.
func (s *Service) syncRedirects(ctx context.Context) {
	now := s.now()
	_ = s.st.DeleteExpiredRedirects(ctx, now)
	s.mu.Lock()
	served := make(map[string]string, len(s.redir))
	for old, r := range s.redir {
		served[old] = r.cur
	}
	s.mu.Unlock()
	for old, cur := range served {
		r, err := s.st.RedirectFor(ctx, old, now)
		switch {
		case errors.Is(err, store.ErrNotFound):
			s.stopRedirect(old)
		case err == nil && r.Flat != cur:
			s.serveRedirect(ctx, old, r.Flat)
		}
	}
}

// Redirects lists active old->new redirects.
func (s *Service) Redirects() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.redir))
	for k, r := range s.redir {
		out[k] = r.cur
	}
	return out
}
