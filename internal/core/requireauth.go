package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/gosuda/flats/internal/store"
)

// The host setting portal.require_auth (docs/spec/portal-require-auth.md): while it is on, Portal serves a flat
// only when the flat has sign-in. The refusal itself is made where a Portal route opens (rules 7 and 8): an open
// carries the setting in PortalSignIn.Refuse. This file holds the detail of a refusal, the check of each request
// (rule 15), the visit that applies a change (rules 16 to 18) and the list that the status shows (rule 10).

// requireAuthRequestText is the answer of rule 15.
const requireAuthRequestText = "This flat has no sign-in, and this host serves a flat on Portal only with sign-in."

// requireAuthOn reports portal.require_auth. It does not wait for a settings save.
func (s *Service) requireAuthOn() bool { return s.settings.RequireAuth() }

// PortalAuthRefusal reports whether ep is a Portal endpoint that portal.require_auth refuses (rule 10).
func PortalAuthRefusal(ep ExposureEndpoint) bool {
	return ep.Provider == ProviderPortal && ep.State == "unavailable" && strings.HasPrefix(ep.Detail, PortalAuthDetailPrefix)
}

// onlyPortalAuth reports whether err is nil or made of refusals by portal.require_auth only, so that a failure of
// another provider is never taken for the refusal.
func onlyPortalAuth(err error) bool {
	if err == nil {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			if !onlyPortalAuth(e) {
				return false
			}
		}
		return true
	}
	ae, ok := err.(*PortalAuthError)
	return ok && ae.StopErr == nil
}

// requireAuthDetail is the detail of the Portal endpoint of a flat that the setting refuses (rule 10). Every action
// that it names is one that the host accepts for a Public flat: a grant for a public provider that sign-in excludes
// makes the host refuse sign-in on (sign-in rule 8), so the text then names the way out.
func (s *Service) requireAuthDetail(ctx context.Context, slugName string) string {
	var excluded []string
	if ids, err := s.permittedIDs(ctx, slugName); err == nil {
		for _, id := range ids {
			if store.SignInExcludes(string(id)) {
				excluded = append(excluded, string(id))
			}
		}
	}
	if len(excluded) == 0 {
		return fmt.Sprintf("%s and %s has no sign-in. Run flats signin set %s to turn sign-in on, or turn the setting off in the console.",
			PortalAuthDetailPrefix, slugName, slugName)
	}
	slices.Sort(excluded)
	list := strings.Join(excluded, ", ")
	return fmt.Sprintf("%s and %s has no sign-in. Sign-in is refused while a grant for %s exists. Make the flat Private, revoke %s under Networks, then run flats signin set %s. Or turn the setting off in the console.",
		PortalAuthDetailPrefix, slugName, list, list, slugName)
}

// portalRefusalNote is the sentence that the result of a visibility apply adds when portal.require_auth refuses the
// Portal route of the flat (rule 19): the setting is on, and the flat has a Portal grant and no sign-in. Another public
// provider opened, or the apply failed before this point. It is empty otherwise.
func (s *Service) portalRefusalNote(ctx context.Context, slugName string) string {
	if !s.requireAuthOn() {
		return ""
	}
	obs, ok := s.cfg.Lifecycle.(LifecycleObserver)
	if !ok {
		return ""
	}
	st, err := obs.ExposureStatus(ctx, slugName)
	if err != nil {
		return ""
	}
	// The note repeats the refusal that the Portal endpoint shows, so it says nothing when Portal is refused for another
	// cause, such as a host that does not permit Portal.
	for _, ep := range st.Endpoints {
		if ep.Host == slugName && ep.Audience == AudienceCurrent && PortalAuthRefusal(ep) {
			return "Portal is refused: " + ep.Detail
		}
	}
	return ""
}

// requireAuthEvent writes a flat event of kind exposure and one host log line with the slug and the text, so that
// a store that fails the event write still leaves the text in the log (rule 18).
func (s *Service) requireAuthEvent(ctx context.Context, slugName, level, msg string) {
	s.logf("portal.require_auth: %s: %s", slugName, msg)
	s.Event(ctx, slugName, level, "exposure", msg, nil)
}

// portalRouteRegistered reports whether the provider manager holds a Portal route for slug. It reads memory only. A
// manager that cannot tell, or an error, counts as registered, so that the visit fails closed.
func (s *Service) portalRouteRegistered(ctx context.Context, slugName string) bool {
	insp, ok := s.cfg.Lifecycle.(LifecycleRouteInspector)
	if !ok {
		return true
	}
	has, err := insp.HasProviderRoute(ctx, slugName, ProviderPortal)
	return err != nil || has
}

// applyRequireAuth applies a changed portal.require_auth to every flat (rules 16 to 18). It visits each flat under
// its lock, as applyHostListing does, and each step reads the current value and does nothing when it no longer
// matches the direction of the visit. A host without a provider manager has no visit (rule 9). The context is not
// cancelled by the request: the setting is saved, so the visit finishes.
func (s *Service) applyRequireAuth(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	if _, ok := s.lifecycleNet(); !ok {
		return
	}
	want := s.requireAuthOn()
	flats, err := s.st.ListFlats(ctx)
	if err != nil {
		if want {
			s.requireAuthListFailed(ctx, err)
		} else {
			s.logf("portal.require_auth turned off, but the host cannot list the flats to open their Portal routes again: %v; a deploy, a grant, a restart or a sign-in change opens a stopped route", err)
		}
		return
	}
	for _, f := range flats {
		func() {
			unlock := s.lock(f.Slug)
			defer unlock()
			if s.requireAuthOn() != want {
				return
			}
			if want {
				s.requireAuthRefuse(ctx, f.Slug)
			} else {
				s.requireAuthReopen(ctx, f.Slug)
			}
		}()
	}
}

// requireAuthRefuse is the visit of rule 16 for one flat. The caller holds its lock.
func (s *Service) requireAuthRefuse(ctx context.Context, slugName string) {
	if !s.portalRouteRegistered(ctx, slugName) {
		return // nothing serves the flat on Portal, and its other public routes keep their epoch
	}
	f, err := s.st.GetFlat(ctx, slugName)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return
	case err != nil:
		s.requireAuthReadFailed(ctx, slugName, err)
		return
	}
	if !f.Visibility.Public() || f.SignInProvider != "" {
		return
	}
	detail := s.requireAuthDetail(ctx, slugName)
	if s.state(slugName).cur.Load() == nil {
		// A flat without a runtime, such as one that a failed rename left with a Portal route: stop the route
		// directly (rule 16, step 3).
		_ = s.requireAuthStopPortal(ctx, slugName, detail)
		s.requireAuthEvent(ctx, slugName, "warn", detail)
		return
	}
	// End the open connections of the public routes, then serve them again: the Portal open is refused and every
	// other public route gets a new epoch (decision 6, rule 16, step 2).
	s.revokeRoute("public:" + slugName)
	err = s.reapplyExposure(ctx, f, ProviderPortal, false)
	if ae, refused := err.(*PortalAuthError); refused && onlyPortalAuth(err) && ae.StopErr == nil {
		// Fail closed: a Portal route that is still registered after the refusal is stopped here.
		if s.portalRouteRegistered(ctx, slugName) {
			_ = s.requireAuthStopPortal(ctx, slugName, detail)
		}
		s.requireAuthEvent(ctx, slugName, "warn", detail)
		return
	}
	if err == nil && !s.requireAuthOn() {
		return // the setting went off while the visit ran: the off-visit opens the route again
	}
	// The re-serve failed after the connections ended, or it did not refuse the Portal route: stop the Portal route
	// and name the failure. A public route that did not open again answers 503 on the cancelled epoch (rule 18).
	if err == nil {
		err = errors.New("the Portal route was not refused")
	}
	reason := oneSentence(fmt.Sprintf("portal.require_auth turned on, but the public routes of %s did not open again: %v", slugName, err))
	_ = s.requireAuthStopPortal(ctx, slugName, reason+". A deploy, a grant, a restart or a sign-in change tries again.")
	s.requireAuthEvent(ctx, slugName, "error", reason+". Its Portal route is stopped. A public route that did not open again answers 503 until a deploy, a grant or a restart.")
}

// requireAuthReadFailed is the first fail-closed path of rule 18: the host cannot read the flat. It ends the open
// public connections of the flat, which needs no read, and stops its Portal route (decision 7).
func (s *Service) requireAuthReadFailed(ctx context.Context, slugName string, cause error) {
	s.revokeRoute("public:" + slugName)
	reason := oneSentence(fmt.Sprintf("portal.require_auth turned on, but the host cannot read %s to check its sign-in: %v", slugName, cause))
	_ = s.requireAuthStopPortal(ctx, slugName, reason+". A deploy, a grant, a restart or a sign-in change tries again.")
	s.requireAuthEvent(ctx, slugName, "error", reason+". Its Portal route is stopped and its public connections are ended. Its other public routes answer 503 until a deploy, a grant or a restart.")
}

// requireAuthListFailed is the second fail-closed path of rule 18: the host cannot list the flats. It stops the Portal
// route of every live flat that has one, from the in-memory map of live flats, and ends no connection (decision 7). A
// slug that holds a redirect is skipped: its Portal route is a redirect that rule 7 never refuses.
func (s *Service) requireAuthListFailed(ctx context.Context, cause error) {
	s.mu.Lock()
	slugs := make([]string, 0, len(s.live))
	for slugName := range s.live {
		if _, redirect := s.redir[slugName]; !redirect {
			slugs = append(slugs, slugName)
		}
	}
	s.mu.Unlock()
	slices.Sort(slugs)
	var stopped []string
	for _, slugName := range slugs {
		func() {
			unlock := s.lock(slugName)
			defer unlock()
			if !s.requireAuthOn() || !s.portalRouteRegistered(ctx, slugName) {
				return
			}
			reason := oneSentence(fmt.Sprintf("portal.require_auth turned on, but the host cannot list the flats to check %s: %v", slugName, cause))
			_ = s.requireAuthStopPortal(ctx, slugName, reason+". A deploy, a grant, a restart or a sign-in change tries again.")
			s.requireAuthEvent(ctx, slugName, "error", reason+". Its Portal route is stopped. A Portal connection that is open stays until it closes or the host restarts.")
			stopped = append(stopped, slugName)
		}()
	}
	names := stopped
	if len(names) > 20 {
		names = append(slices.Clone(names[:20]), fmt.Sprintf("and %d more", len(stopped)-20))
	}
	s.logf("portal.require_auth turned on, but the host cannot list the flats: %v; stopped the Portal route of %d flats: %s", cause, len(stopped), strings.Join(names, ", "))
}

// oneSentence makes the text of an error one line without a final period, so that a sentence can follow it.
func oneSentence(text string) string {
	return strings.TrimRight(strings.ReplaceAll(text, "\n", "; "), ". ")
}

// requireAuthStopPortal stops the Portal route of a flat as a gate that cannot be built stops it (sign-in rule 35), with
// reason as the detail of its endpoint.
func (s *Service) requireAuthStopPortal(ctx context.Context, slugName, reason string) error {
	n, ok := s.signInNet()
	if !ok {
		return nil
	}
	return n.FailPortalGate(context.WithoutCancel(ctx), slugName, reason)
}

// requireAuthReopen is the visit of rule 17 for one flat. The caller holds its lock. It serves the public routes again
// for a Public flat that has a runtime and a Portal grant, when its Portal route is not registered or is stopped, so
// the Portal route opens as a restart opens it. It publishes nothing new.
func (s *Service) requireAuthReopen(ctx context.Context, slugName string) {
	f, err := s.st.GetFlat(ctx, slugName)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		s.requireAuthEvent(ctx, slugName, "error", fmt.Sprintf("portal.require_auth turned off, but the host cannot read %s to open its Portal route again: %v. A deploy, a grant, a restart or a sign-in change tries again.", slugName, err))
		return
	}
	if !f.Visibility.Public() || s.state(slugName).cur.Load() == nil {
		return
	}
	granted, err := s.st.ProviderPermitted(ctx, slugName, store.ProviderPortal)
	if err != nil {
		s.requireAuthEvent(ctx, slugName, "error", fmt.Sprintf("portal.require_auth turned off, but the host cannot read the Portal grant of %s: %v. A deploy, a grant, a restart or a sign-in change tries again.", slugName, err))
		return
	}
	if !granted {
		return
	}
	needs, err := s.portalNeedsReopen(ctx, slugName)
	if err != nil {
		s.requireAuthEvent(ctx, slugName, "error", fmt.Sprintf("portal.require_auth turned off, but the host cannot read the routes of %s to open its Portal route again: %v. A deploy, a grant, a restart or a sign-in change tries again.", slugName, err))
		return
	}
	if !needs {
		return
	}
	if err := s.reapplyExposure(ctx, f, ProviderPortal, true); err != nil {
		s.requireAuthEvent(ctx, slugName, "warn", "portal.require_auth turned off, but the Portal route of "+slugName+" did not open: "+err.Error())
		return
	}
	s.requireAuthEvent(ctx, slugName, "info", "portal.require_auth turned off: the Portal route of "+slugName+" is open again")
}

// portalNeedsReopen reports whether the Portal route of a flat is not registered, or is registered with a stop that was
// not confirmed: the routes that rule 17 serves again. A registered route reports the detail of the Portal backend, which
// holds TeardownPending for such a stop. A route in the state error for another cause, such as a relay fault, was not
// stopped by the setting and is left alone. The error is a failed read of the routes.
func (s *Service) portalNeedsReopen(ctx context.Context, slugName string) (bool, error) {
	if !s.portalRouteRegistered(ctx, slugName) {
		return true, nil
	}
	obs, ok := s.cfg.Lifecycle.(LifecycleObserver)
	if !ok {
		return false, nil
	}
	st, err := obs.ExposureStatus(ctx, slugName)
	if err != nil {
		return false, err
	}
	for _, ep := range st.Endpoints {
		if ep.Provider == ProviderPortal && ep.Audience == AudienceCurrent && ep.Host == slugName {
			return ep.State == "error" && strings.Contains(ep.Detail, TeardownPending), nil
		}
	}
	return false, nil
}

// PortalRefused lists the flats that portal.require_auth refuses now, sorted (rule 10): the setting is on, Portal is
// permitted for the host, and the flat is Public with a live version, has a Portal grant and has no sign-in. The
// list is derived from the stored facts, so no record of a refusal outlives its cause.
func (s *Service) PortalRefused(ctx context.Context, portalPermitted bool) ([]string, error) {
	out := []string{}
	if !s.requireAuthOn() || !portalPermitted {
		return out, nil
	}
	flats, err := s.st.ListFlats(ctx)
	if err != nil {
		return nil, err
	}
	for _, f := range flats {
		if !f.Visibility.Public() || f.LiveVersion <= 0 || f.SignInProvider != "" {
			continue
		}
		granted, err := s.st.ProviderPermitted(ctx, f.Slug, store.ProviderPortal)
		if err != nil {
			return nil, err
		}
		if granted {
			out = append(out, f.Slug)
		}
	}
	slices.Sort(out)
	return out, nil
}

// PortalRequireAuth reports the value of portal.require_auth for the status of the host.
func (s *Service) PortalRequireAuth() bool { return s.requireAuthOn() }
