package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

func makePublic(t *testing.T, e *env, slug string) {
	t.Helper()
	ctx := context.Background()
	r, err := e.svc.SetVisibility(ctx, slug, store.Public, core.ViaConsole, "")
	if err != nil || r.Status != "pending_approval" {
		t.Fatalf("visibility: %v %+v", err, r)
	}
	if _, err := e.svc.Decide(ctx, r.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
}

func listing(t *testing.T, e *env, slug string) bool {
	t.Helper()
	hidden, served := e.pub.Hidden(slug)
	if !served {
		t.Fatalf("%s is not served publicly", slug)
	}
	return hidden
}

func TestPortalListingFollowsHostDefaultAndFlatOverride(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	for _, slug := range []string{"follow", "pinned"} {
		if _, err := e.svc.SaveVersion(ctx, slug, files("index.html", "hi"), core.SaveMeta{}, core.ViaMCP); err != nil {
			t.Fatal(err)
		}
		publish(t, e.svc, slug)
		permitPortal(t, e.svc, slug)
	}
	// Overriding before the flat is public is stored and used when it opens.
	if _, err := e.svc.SetPortalListing(ctx, "pinned", store.ListingListed, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdateSettings(ctx, map[string]string{core.SetPortalHide: "true"}); err != nil {
		t.Fatal(err)
	}
	makePublic(t, e, "follow")
	makePublic(t, e, "pinned")
	if !listing(t, e, "follow") || listing(t, e, "pinned") {
		t.Fatal("host default hide=true: follow should be hidden, pinned listed")
	}
	fv, _ := e.svc.GetFlat(ctx, "follow")
	if fv.PortalListing != store.ListingDefault || !fv.PortalHidden || fv.PublicNotice != core.PublicAccessNotice {
		t.Fatalf("view %+v", fv)
	}

	// Changing the host default updates served routes that follow it at once.
	if _, err := e.svc.UpdateSettings(ctx, map[string]string{core.SetPortalHide: "false"}); err != nil {
		t.Fatal(err)
	}
	if listing(t, e, "follow") || listing(t, e, "pinned") {
		t.Fatal("host default hide=false: both should be listed")
	}

	// A per-flat override applies to the served route at once.
	fv, err := e.svc.SetPortalListing(ctx, "follow", store.ListingHidden, core.ViaConsole)
	if err != nil || fv.PortalListing != store.ListingHidden || !fv.PortalHidden {
		t.Fatalf("override: %v %+v", err, fv)
	}
	if !listing(t, e, "follow") {
		t.Fatal("override hidden not applied")
	}
	if _, err := e.svc.UpdateSettings(ctx, map[string]string{core.SetPortalHide: "true"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdateSettings(ctx, map[string]string{core.SetPortalHide: "false"}); err != nil {
		t.Fatal(err)
	}
	if !listing(t, e, "follow") {
		t.Fatal("host default overrode a flat's own choice")
	}
	if _, err := e.svc.SetPortalListing(ctx, "follow", store.ListingDefault, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if listing(t, e, "follow") {
		t.Fatal("back to default should follow hide=false")
	}

	// Going private and public again keeps the flat's choice.
	if _, err := e.svc.SetPortalListing(ctx, "pinned", store.ListingHidden, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	r, err := e.svc.SetVisibility(ctx, "pinned", store.Private, core.ViaConsole, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Decide(ctx, r.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	makePublic(t, e, "pinned")
	if !listing(t, e, "pinned") {
		t.Fatal("republished flat lost its hidden listing")
	}
}

func TestPortalListingIsOperatorOnly(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, err := e.svc.SaveVersion(ctx, "blog", files("index.html", "hi"), core.SaveMeta{}, core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	for _, via := range []core.Via{core.ViaMCP, core.ViaAPI, core.ViaCLI} {
		if _, err := e.svc.SetPortalListing(ctx, "blog", store.ListingListed, via); !errors.Is(err, core.ErrForbidden) {
			t.Fatalf("%s: %v", via, err)
		}
	}
	if _, err := e.svc.SetPortalListing(ctx, "blog", "unlisted", core.ViaConsole); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("invalid mode: %v", err)
	}
	if _, err := e.svc.SetPortalListing(ctx, "nope", store.ListingHidden, core.ViaConsole); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing flat: %v", err)
	}
	if _, err := e.svc.UpdateSettings(ctx, map[string]string{core.SetPortalHide: "maybe"}); err == nil {
		t.Fatal("invalid portal_hide accepted")
	}
}
