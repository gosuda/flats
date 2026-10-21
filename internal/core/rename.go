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
	if err := slug.Validate(from); err != nil {
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
	if err := s.checkStorageJournal(from); err != nil {
		return FlatView{}, err
	}
	if err := s.requireUnusedStorage(to); err != nil {
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
	// A handler may open DB/FILES lazily using its original absolute path.
	// Disable new requests and wait for the worker to exit before moving data.
	var current *deployed
	if lf != nil {
		current = lf.cur.Swap(nil)
		if current != nil && current.inst != nil {
			stopInstance(current.inst, "renamed")
		}
	}
	restoreCurrent := func() {
		if current == nil {
			return
		}
		// Not an activation: keep the captured settings snapshot.
		rebuilt, err := s.buildWithEnvironment(context.WithValue(context.WithoutCancel(ctx), lazyDocsKey{}, true), from, current.version, s.dataDirOf(from), current.environment, lf.liveSink())
		if err != nil {
			s.Event(context.WithoutCancel(ctx), from, "error", "rename", "could not restart the live version after failed rename: "+err.Error(), nil)
			return
		}
		lf.cur.Store(rebuilt)
	}
	fromDir, err := s.storagePath(from)
	if err != nil {
		restoreCurrent()
		return FlatView{}, err
	}
	toDir, err := s.storagePath(to)
	if err != nil {
		restoreCurrent()
		return FlatView{}, err
	}
	op := storageOperation{Kind: "rename", From: from, To: to}
	if err := s.beginStorageOperation(op); err != nil {
		restoreCurrent()
		return FlatView{}, err
	}
	moved := false
	root := s.storageRoot()
	if err := os.Rename(fromDir, toDir); err == nil {
		moved = true
		if err := syncDirectory(root); err != nil {
			rollbackErr := os.Rename(toDir, fromDir)
			if rollbackErr != nil {
				// Keep the journal so startup can recover the remaining
				// files. The worker stays stopped, and every storage write
				// to either slug is refused until then (lockStorage).
				return FlatView{}, errors.Join(err, rollbackErr)
			}
			// The data is back at the old path: restart the old worker.
			restoreCurrent()
			rollbackErr = syncDirectory(root)
			if rollbackErr == nil {
				rollbackErr = s.finishStorageOperation(op)
			}
			return FlatView{}, errors.Join(err, rollbackErr)
		}
	} else if !os.IsNotExist(err) {
		restoreCurrent()
		return FlatView{}, errors.Join(err, s.finishStorageOperation(op))
	}
	if err := s.st.RenameFlat(ctx, from, to, till, s.now()); err != nil {
		if moved {
			if rollbackErr := os.Rename(toDir, fromDir); rollbackErr != nil {
				// Keep the journal so startup can recover the remaining
				// files. The worker stays stopped, and every storage write
				// to either slug is refused until then (lockStorage).
				return FlatView{}, errors.Join(err, fmt.Errorf("restore storage after failed rename: %w", rollbackErr))
			}
			if rollbackErr := syncDirectory(root); rollbackErr != nil {
				restoreCurrent()
				return FlatView{}, errors.Join(err, rollbackErr)
			}
		}
		restoreCurrent()
		err = errors.Join(err, s.finishStorageOperation(op))
		if errors.Is(err, store.ErrExists) {
			return FlatView{}, withKind(ErrConflict, err)
		}
		return FlatView{}, err
	}
	// Keep the journal until the metadata commit is durable: after a power
	// loss the database could otherwise return to the old slug with nothing
	// left that links it to the moved directory.
	if err := s.st.Sync(context.WithoutCancel(ctx)); err != nil {
		s.logf("rename %s -> %s committed; its journal stays until the next startup because the database could not be synced: %v", from, to, err)
	} else if err := s.finishStorageOperation(op); err != nil {
		// The database and storage are committed; startup can remove the
		// remaining journal. Do not undo or interrupt the in-memory rename.
		s.logf("rename %s -> %s committed but journal cleanup failed: %v", from, to, err)
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
		if current != nil {
			// Rebuild against the moved path with the captured settings
			// snapshot (a rename is not an activation). Failure must leave
			// the flat unavailable, never with a handler bound to the old slug.
			// The rename is committed, so a caller that went away must not
			// leave the flat without its worker.
			bg := context.WithoutCancel(ctx)
			// The held lf's sink: delete(s.live, from) above means a lookup by
			// slug would create a stray state.
			if rebuilt, err := s.buildWithEnvironment(context.WithValue(bg, lazyDocsKey{}, true), to, current.version, s.dataDirOf(to), current.environment, lf.liveSink()); err == nil {
				lf.cur.Store(rebuilt)
			} else {
				s.Event(bg, to, "error", "rename", "could not restart the live version after rename: "+err.Error(), nil)
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

// redirectHandler redirects to the same path under the base URL target
// returns for the provider the request arrived on ("" when it is not
// marked) and whether that provider is public. The redirect is temporary
// (307): it ends with the redirect window, so browsers must not cache it.
// While the new address is not known yet (a Portal exposure that is still
// starting), it answers 503 instead of redirecting to itself.
func (s *Service) redirectHandler(target func(p ProviderID, public bool) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := RouteProvider(r.Context())
		base := strings.TrimSuffix(target(p, PublicProvider(p)), "/")
		if base == "" {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "this flat moved; its new address is not ready yet, try again shortly", http.StatusServiceUnavailable)
			return
		}
		http.Redirect(w, r, base+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	})
}

// redirectTarget is the base URL a redirect to cur sends a visitor who
// arrived on provider p: cur's address on p when it has one there,
// otherwise its private_url or, for a public provider, its public_url. A
// public redirect has no target while cur is not Public.
func (s *Service) redirectTarget(cur string, p ProviderID, public bool) string {
	fv, _ := s.GetFlat(context.Background(), cur)
	if public && !fv.Visibility.Public() {
		return ""
	}
	for _, ep := range fv.Endpoints {
		if ep.Provider != p || ep.Audience != AudienceCurrent || ep.Host != fv.Slug {
			continue
		}
		if (public && publicCandidate(ep)) || (!public && privateCandidate(ep, privateOrder)) {
			return ep.URL
		}
	}
	if public {
		return fv.PublicURL
	}
	return fv.PrivateURL
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
		h := s.redirectHandler(func(p ProviderID, public bool) string { return s.redirectTarget(cur, p, public) })
		_, err = n.ServeExposure(ctx, ExposureRequest{Slug: old, Host: old, Visibility: "private", Audience: AudienceCurrent, Handler: h, Permitted: privateProviders(ids)})
		if err != nil {
			s.Event(ctx, cur, "error", "rename", err.Error(), nil)
		}
		public := false
		if f.Visibility.Public() {
			res, err := n.ServeExposure(ctx, ExposureRequest{Slug: old, Host: old, Visibility: "public", Audience: AudienceCurrent, Handler: h, PrivateHandler: h, Permitted: ids, Hidden: true})
			for _, ep := range res.Endpoints {
				if PublicProvider(ep.Provider) {
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
	if _, err := s.cfg.Private.Serve(ctx, old, s.redirectHandler(func(ProviderID, bool) string { return s.cfg.Private.URL(cur) }), false); err != nil {
		s.Event(ctx, cur, "error", "rename", "could not serve private redirect from "+old+": "+err.Error(), nil)
	}
	s.mu.Lock()
	prev := s.redir[old]
	s.mu.Unlock()
	public := prev != nil && prev.public
	if f.Visibility.Public() && s.cfg.Public != nil {
		if _, err := s.cfg.Public.Serve(ctx, old, s.redirectHandler(func(ProviderID, bool) string { return s.cfg.Public.URL(cur) }), true); err != nil {
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
