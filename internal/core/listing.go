package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/gosuda/flats/internal/store"
)

// HiddenListingNotice explains what hiding a flat from relay listings does.
const HiddenListingNotice = "Hidden only keeps this flat out of Portal relay listings. It is NOT access control: anyone with the URL can open it."

// listingHidden resolves a flat's listing mode against the host default.
func (s *Service) listingHidden(mode string) bool {
	switch mode {
	case store.ListingHidden:
		return true
	case store.ListingListed:
		return false
	}
	return s.settings.get(SetPortalHide) == "true"
}

// portalHidden reports whether slug's public Portal route stays out of relay
// listings.
func (s *Service) portalHidden(ctx context.Context, slugName string) (bool, error) {
	mode, err := s.st.PortalListing(ctx, slugName)
	if err != nil {
		return false, err
	}
	return s.listingHidden(mode), nil
}

// SetPortalListing records whether a flat's Portal route appears in relay
// listings: default follows the host's portal_hide setting. Only the console
// may call it. A served public route changes at once; relays pick it up at
// their next lease renewal. It never changes visibility or who can open the
// flat.
func (s *Service) SetPortalListing(ctx context.Context, slugName, mode string, via Via) (FlatView, error) {
	if via != ViaConsole {
		return FlatView{}, forbiddenf("only the operator can change relay listing, in the web console")
	}
	if !store.ValidListing(mode) {
		return FlatView{}, invalidf("listing must be %q, %q or %q", store.ListingDefault, store.ListingHidden, store.ListingListed)
	}
	unlock := s.lock(slugName)
	defer unlock()
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return FlatView{}, err
	}
	if err := s.st.SetPortalListing(ctx, slugName, mode, s.now()); err != nil {
		return FlatView{}, err
	}
	hidden := s.listingHidden(mode)
	s.Event(ctx, slugName, "info", "listing", fmt.Sprintf("portal relay listing set to %s (hidden=%t) via %s", mode, hidden, via), nil)
	if err := s.applyListing(f, hidden); err != nil {
		s.Event(ctx, slugName, "error", "listing", err.Error(), nil)
		return FlatView{}, fmt.Errorf("%w: listing saved but not applied to the running Portal route: %w", ErrProviderNotReady, err)
	}
	return s.view(ctx, f), nil
}

// applyListing updates the served Portal route of a public flat. A flat
// without one picks the listing up when its route opens.
func (s *Service) applyListing(f store.Flat, hidden bool) error {
	if !f.Visibility.Public() {
		return nil
	}
	if ln, ok := s.lifecycleNet(); ok {
		if l, ok := ln.(LifecycleListingNet); ok {
			return l.SetPortalHidden(f.Slug, hidden)
		}
		return nil
	}
	if s.cfg.Public != nil && s.state(f.Slug).publicServed {
		return s.cfg.Public.SetHidden(f.Slug, hidden)
	}
	return nil
}

// applyHostListing applies a changed portal_hide default to every public
// flat that follows it. Each flat is checked under its lock, so a flat that
// becomes Public meanwhile is either seen here or reads the new default when
// it opens. Failures are logged on the flat; the setting stays saved.
func (s *Service) applyHostListing(ctx context.Context) {
	ctx = context.WithoutCancel(ctx) // the setting is saved; finish applying it
	flats, err := s.st.ListFlats(ctx)
	if err != nil {
		s.logf("apply portal listing: %v", err)
		return
	}
	for _, f := range flats {
		func() {
			unlock := s.lock(f.Slug)
			defer unlock()
			f, err := s.st.GetFlat(ctx, f.Slug) // it may have changed since the list
			if errors.Is(err, store.ErrNotFound) {
				return
			}
			if err == nil && !f.Visibility.Public() {
				return
			}
			var mode string
			if err == nil {
				mode, err = s.st.PortalListing(ctx, f.Slug)
			}
			if err == nil && mode != store.ListingDefault {
				return
			}
			if err == nil {
				err = s.applyListing(f, s.listingHidden(mode))
			}
			if err != nil {
				s.Event(ctx, f.Slug, "error", "listing", "apply host relay listing: "+err.Error(), nil)
			}
		}()
	}
}
