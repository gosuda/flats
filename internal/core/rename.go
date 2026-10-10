package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
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
	// Preview data lives below the flat directory. Do not rename while an
	// old-slug preview route remains reachable; leave all flat state retryable.
	if err := s.dropPreviews(ctx, from); err != nil {
		return FlatView{}, fmt.Errorf("close previews before rename: %w", err)
	}
	s.mu.Lock()
	lf := s.live[from]
	s.mu.Unlock()
	// Keep the old nodes/exposures: serveRedirect swaps their handlers to
	// redirects, so the old addresses keep resolving without a new login.
	wasPublic, wasPrivate := false, false
	if lf != nil {
		wasPublic = lf.publicServed
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

	s.revokeRoute("public:" + from)

	// The rename is committed: move the in-memory state to the new slug.
	s.mu.Lock()
	delete(s.live, from)
	own := s.redir[to] // to was this flat's own earlier slug
	delete(s.redir, to)
	var chained []string
	for old, r := range s.redir {
		if r.cur == from {
			// Durable expiry owners follow the renamed flat for later delete
			// cleanup, but an expired address must never be served again.
			r.cur = to
			if !r.teardownOnly && !r.retired {
				chained = append(chained, old)
			}
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
		if own.public && !f.Visibility.Public() {
			if err := s.stopPublicRoutes(ctx, to); err != nil {
				s.Event(ctx, to, "error", "rename", err.Error(), nil)
			}
		}
		if lf == nil || lf.cur.Load() == nil {
			_ = s.stopPreviewExposure(ctx, to)
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
	if n, ok := s.lifecycleNet(); ok {
		ids, err := s.permittedIDs(ctx, cur)
		if err != nil {
			return
		}
		_, err = n.ServeExposure(ctx, ExposureRequest{Slug: old, Host: old, Visibility: "private", Audience: AudienceCurrent, Handler: s.redirectHandler(func() string { fv, _ := s.GetFlat(context.Background(), cur); return fv.PrivateURL }), Permitted: privateProviders(ids)})
		if err != nil {
			s.Event(ctx, cur, "error", "rename", err.Error(), nil)
		}
		public := false
		if f.Visibility.Public() {
			target := s.redirectHandler(func() string {
				fv, _ := s.GetFlat(context.Background(), cur)
				if !fv.Visibility.Public() {
					return ""
				}
				return fv.PublicURL
			})
			res, err := n.ServeExposure(ctx, ExposureRequest{Slug: old, Host: old, Visibility: "public", Audience: AudienceCurrent, Handler: target, PrivateHandler: s.redirectHandler(func() string { fv, _ := s.GetFlat(context.Background(), cur); return fv.PrivateURL }), Permitted: ids, Hidden: true})
			for _, ep := range res.Endpoints {
				if ep.Provider == ProviderFunnel || ep.Provider == ProviderPortal {
					public = true
				}
			}
			if err != nil {
				s.Event(ctx, cur, "error", "rename", err.Error(), nil)
			}
		}
		s.mu.Lock()
		s.redir[old] = &redirect{cur: cur, public: public}
		s.mu.Unlock()
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
	var olds []string
	s.mu.Lock()
	for old, r := range s.redir {
		if r.cur == cur && r.public {
			olds = append(olds, old)
		}
	}
	s.mu.Unlock()
	for _, old := range olds {
		if err := s.stopPublicRoutes(context.Background(), old); err != nil {
			s.Event(context.Background(), cur, "error", "rename", err.Error(), nil)
			continue
		}
		s.mu.Lock()
		if r := s.redir[old]; r != nil {
			r.public = false
		}
		s.mu.Unlock()
	}
}

func (s *Service) stopRedirect(old string) error {
	s.mu.Lock()
	r := s.redir[old]
	if r != nil && r.retired {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	if r == nil {
		return nil
	}
	// Keep the registration in memory on an unconfirmed stop so later expiry
	// sweeps can retry and status continues to reflect the actual route.
	if err := s.stopSlugRoutes(context.Background(), old); err != nil {
		return err
	}
	s.mu.Lock()
	settled := s.redir[old] == r
	cur := ""
	if settled {
		r.teardownOnly = true
		r.retired = true
		r.public = false
		cur = r.cur
	}
	s.mu.Unlock()
	if settled {
		s.Event(context.Background(), cur, "info", "rename", "redirect from "+old+" expired", nil)
	}
	return nil
}

// syncRedirects re-points or stops served redirects to match the database:
// a redirect follows later renames of its flat and ends with its window.
func (s *Service) syncRedirects(ctx context.Context) {
	// A manual sweep may overlap the periodic one. Serialize their durable-row
	// snapshots so a late sweep cannot resurrect a row an earlier sweep removed.
	unlockSweep := s.lock("\x00redirect-sweep")
	defer unlockSweep()

	now := s.now()
	// Read every durable row, including expired ones. An expired redirect is
	// still the retry owner for its provider identity until teardown confirms;
	// deleting the row first would lose that owner across a restart.
	rows, err := s.st.ActiveRedirects(ctx, time.Time{})
	if err != nil {
		return
	}
	persisted := make(map[string]store.Redirect, len(rows))
	for _, row := range rows {
		persisted[row.Old] = row
	}
	s.mu.Lock()
	oldsSet := make(map[string]struct{}, len(s.redir)+len(persisted))
	for old := range s.redir {
		oldsSet[old] = struct{}{}
	}
	s.mu.Unlock()
	for old := range persisted {
		oldsSet[old] = struct{}{}
	}
	olds := make([]string, 0, len(oldsSet))
	for old := range oldsSet {
		olds = append(olds, old)
	}
	slices.Sort(olds)
	teardownFailed := false
	for _, old := range olds {
		unlock := s.lock(old)
		r, ok := persisted[old]
		expired := ok && !r.Until.After(now)
		s.mu.Lock()
		current := s.redir[old]
		if expired {
			if current == nil {
				current = &redirect{cur: r.Flat, teardownOnly: true}
				s.redir[old] = current
			} else {
				current.cur = r.Flat
				current.teardownOnly = true
			}
		}
		currentCur := ""
		currentTeardownOnly, currentRetired := false, false
		if current != nil {
			currentCur = current.cur
			currentTeardownOnly = current.teardownOnly
			currentRetired = current.retired
		}
		s.mu.Unlock()
		switch {
		case expired:
			if _, err := s.st.GetFlat(ctx, old); err == nil {
				// A real flat always owns its routes. This can only arise from an
				// externally repaired/legacy database; leave its route untouched.
				s.mu.Lock()
				if s.redir[old] == current && current.teardownOnly {
					delete(s.redir, old)
				}
				s.mu.Unlock()
				break
			} else if !errors.Is(err, store.ErrNotFound) {
				unlock()
				return
			}
			if err := s.stopRedirect(old); err != nil {
				s.Event(ctx, currentCur, "error", "rename", err.Error(), nil)
				teardownFailed = true
			}
		case !ok && current != nil:
			// A route whose durable row disappeared still needs one confirmed
			// stop, but no database cleanup must hold its reservation afterward.
			s.mu.Lock()
			current.teardownOnly = true
			currentCur = current.cur
			s.mu.Unlock()
			if err := s.stopRedirect(old); err != nil {
				s.Event(ctx, currentCur, "error", "rename", err.Error(), nil)
				teardownFailed = true
			} else {
				s.mu.Lock()
				if s.redir[old] == current && current.retired {
					delete(s.redir, old)
				}
				s.mu.Unlock()
			}
		case current != nil && r.Flat != currentCur && !currentTeardownOnly && !currentRetired:
			s.serveRedirect(ctx, old, r.Flat)
		}
		unlock()
	}
	if !teardownFailed {
		if err := s.st.DeleteExpiredRedirects(ctx, now); err != nil {
			return
		}
		s.mu.Lock()
		for old, row := range persisted {
			if row.Until.After(now) {
				continue
			}
			if r := s.redir[old]; r != nil && r.retired {
				delete(s.redir, old)
			}
		}
		s.mu.Unlock()
	}
}

// Redirects lists active old->new redirects.
func (s *Service) Redirects() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.redir))
	for k, r := range s.redir {
		if !r.retired {
			out[k] = r.cur
		}
	}
	return out
}
