package provider

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

// grantQuick keeps all traffic on loopback, with explicit readiness/error
// transitions instead of a real quick tunnel or wall-clock waits.
type grantQuick struct {
	*fakeQuick
	state   string
	failure error
	opens   int
}

func (q *grantQuick) Serve(ctx context.Context, slug string, h http.Handler) (string, error) {
	if q.failure != nil {
		return "", q.failure
	}
	if !q.net.Serving("quick-" + slug) {
		q.opens++
	}
	return q.fakeQuick.Serve(ctx, slug, h)
}

func (q *grantQuick) Status() core.NetStatus {
	st := q.fakeQuick.Status()
	for i := range st.Hosts {
		st.Hosts[i].State = q.state
		if q.state == "error" {
			st.Hosts[i].Detail = "edge connection lost"
		}
	}
	return st
}

func grantEndpoint(t *testing.T, svc *core.Service, id ID) ExposureEndpoint {
	t.Helper()
	fv, err := svc.GetFlat(t.Context(), "site")
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range fv.Endpoints {
		if ep.Provider == id && ep.Host == "site" && ep.Audience == AudienceCurrent {
			return ep
		}
	}
	t.Fatalf("no current %s endpoint: %+v", id, fv.Endpoints)
	return ExposureEndpoint{}
}

func TestPublicProviderGrantQuickLifecycle(t *testing.T) {
	f := reviewerService(t, Portal)
	f.public.advance()
	publicReview(t, f)
	before, err := f.svc.GetFlat(t.Context(), "site")
	if err != nil {
		t.Fatal(err)
	}
	q := &grantQuick{fakeQuick: newFakeQuick(t), state: "starting"}
	f.manager.AttachCFQuick(q)
	if _, err := f.manager.SetGrant(CFQuick, true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := f.svc.SetProviderPermission(t.Context(), "site", string(CFQuick), true, core.ViaConsole); err != nil {
			t.Fatal(err)
		}
		ep := grantEndpoint(t, f.svc, CFQuick)
		if ep.State != "starting" || ep.Ready || !ep.Configured || !ep.Permitted || ep.URL != q.URL("site") {
			t.Fatalf("starting endpoint = %+v", ep)
		}
	}
	q.state = "ready"
	if ep := grantEndpoint(t, f.svc, CFQuick); ep.State != "ready" || !ep.Ready {
		t.Fatalf("readiness did not converge: %+v", ep)
	}
	if get(t, q.URL("site")) != "v1" {
		t.Fatal("new route does not serve the deployed content")
	}
	q.state = "error"
	if ep := grantEndpoint(t, f.svc, CFQuick); ep.State != "error" || ep.Ready || !strings.Contains(ep.Detail, "edge connection lost") {
		t.Fatalf("async error not visible: %+v", ep)
	}
	after, err := f.svc.GetFlat(t.Context(), "site")
	if err != nil {
		t.Fatal(err)
	}
	if after.Visibility != store.Public || after.PublicURL != before.PublicURL || after.PrivateURL != before.PrivateURL || after.LiveVersion != before.LiveVersion {
		t.Fatalf("existing exposure changed: before=%+v after=%+v", before, after)
	}
	if f.public.opens != 1 || f.public.stops != 0 || q.opens != 1 || len(q.stopped) != 0 {
		t.Fatalf("grant restarted a route: portal opens/stops=%d/%d quick opens/stops=%d/%d", f.public.opens, f.public.stops, q.opens, len(q.stopped))
	}
}

func TestPublicProviderGrantFailureAndRetry(t *testing.T) {
	for _, hostEnabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "backend failure", false: "host disabled"}[hostEnabled], func(t *testing.T) {
			f := reviewerService(t, Portal)
			f.public.advance()
			publicReview(t, f)
			q := &grantQuick{fakeQuick: newFakeQuick(t), state: "ready"}
			f.manager.AttachCFQuick(q)
			if hostEnabled {
				if _, err := f.manager.SetGrant(CFQuick, true); err != nil {
					t.Fatal(err)
				}
				q.failure = errors.New("tunnel mint refused")
			}
			err := f.svc.SetProviderPermission(t.Context(), "site", string(CFQuick), true, core.ViaConsole)
			if !errors.Is(err, core.ErrProviderNotReady) || core.ErrorCategory(err) != "provider_not_ready" {
				t.Fatalf("grant failure not surfaced: %v", err)
			}
			ep := grantEndpoint(t, f.svc, CFQuick)
			wantState, wantDetail := "unavailable", "host permission absent"
			if hostEnabled {
				wantState, wantDetail = "error", "tunnel mint refused"
			}
			if ep.State != wantState || !strings.Contains(ep.Detail, wantDetail) || ep.Ready || q.opens != 0 {
				t.Fatalf("failed grant endpoint = %+v, opens=%d", ep, q.opens)
			}
			fv, err := f.svc.GetFlat(t.Context(), "site")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(fv.Providers, string(CFQuick)) || fv.Visibility != store.Public || fv.PublicURL == "" {
				t.Fatalf("failed grant lost permission or sibling: %+v", fv)
			}
			if get(t, fv.PublicURL) != "v1" || f.public.opens != 1 || f.public.stops != 0 {
				t.Fatal("failed grant interrupted the existing public route")
			}
			q.failure = nil
			if _, err := f.manager.SetGrant(CFQuick, true); err != nil {
				t.Fatal(err)
			}
			if err := f.svc.SetProviderPermission(t.Context(), "site", string(CFQuick), true, core.ViaConsole); err != nil {
				t.Fatal(err)
			}
			if ep := grantEndpoint(t, f.svc, CFQuick); !ep.Ready || q.opens != 1 {
				t.Fatalf("repeat grant did not recover: %+v opens=%d", ep, q.opens)
			}
		})
	}
}

func TestPublicProviderGrantPreservesPortalListing(t *testing.T) {
	f := reviewerService(t, Portal)
	// Start Public on an independent quick backend, then add Portal. Funnel
	// and Portal in reviewerService share a fake, so using Funnel here would
	// make the listing assertion depend on the public-provider iteration order.
	q := &grantQuick{fakeQuick: newFakeQuick(t), state: "ready"}
	f.manager.AttachCFQuick(q)
	if _, err := f.manager.SetGrant(CFQuick, true); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetProviderPermission(t.Context(), "site", string(CFQuick), true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetProviderPermission(t.Context(), "site", string(Portal), false, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	f.public.advance()
	publicReview(t, f)
	if _, err := f.svc.SetPortalListing(t.Context(), "site", store.ListingHidden, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetProviderPermission(t.Context(), "site", string(Portal), true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if hidden, served := f.public.Hidden("site"); !served || !hidden {
		t.Fatalf("new Portal route ignored hidden listing: served=%v hidden=%v", served, hidden)
	}
	if err := f.svc.SetProviderPermission(t.Context(), "site", string(Tailscale), true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if hidden, served := f.public.Hidden("site"); !served || !hidden {
		t.Fatal("later private grant reset the Portal listing")
	}
}

func TestPublicProviderGrantLeavesDraftPreviews(t *testing.T) {
	f := reviewerService(t, Portal)
	f.public.advance()
	publicReview(t, f)
	if _, err := f.svc.SaveVersion(t.Context(), "site", []bundle.File{{Path: "index.html", Data: []byte("draft")}}, core.SaveMeta{}, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	draft, err := f.svc.OpenPreview(t.Context(), "site", 0)
	if err != nil {
		t.Fatal(err)
	}
	version, err := f.svc.OpenPreview(t.Context(), "site", 1)
	if err != nil {
		t.Fatal(err)
	}
	previewEndpoints := func() []ExposureEndpoint {
		t.Helper()
		status, err := f.manager.ExposureStatus(t.Context(), "site")
		if err != nil {
			t.Fatal(err)
		}
		return slices.DeleteFunc(status.Endpoints, func(ep ExposureEndpoint) bool { return ep.Audience != AudienceDraft })
	}
	before := previewEndpoints()
	if len(before) != 2 || get(t, draft.URL) != "draft" || get(t, version.URL) != "v1" {
		t.Fatalf("draft and version previews not serving: %+v", before)
	}
	q := &grantQuick{fakeQuick: newFakeQuick(t), state: "ready"}
	f.manager.AttachCFQuick(q)
	if _, err := f.manager.SetGrant(CFQuick, true); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetProviderPermission(t.Context(), "site", string(CFQuick), true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if after := previewEndpoints(); !slices.Equal(before, after) {
		t.Fatalf("grant changed draft/preview routes: before=%+v after=%+v", before, after)
	}
	if get(t, draft.URL) != "draft" || get(t, version.URL) != "v1" || get(t, q.URL("site")) != "v1" {
		t.Fatal("grant changed pinned preview bytes or exposed the Draft")
	}
}

func TestProviderKeyRetryPreservesPublicExposure(t *testing.T) {
	f := reviewerService(t, Portal)
	f.public.advance()
	publicReview(t, f)
	n := newFakeNetbird(t)
	f.manager.AttachNetbird(n)
	for _, id := range []ID{Netbird, NetbirdExpose} {
		if _, err := f.manager.SetGrant(id, true); err != nil {
			t.Fatal(err)
		}
	}
	// An explicit public grant attempts NetBird Expose now, but a missing
	// key leaves its error observed while Portal keeps serving.
	n.serveErr = errors.New("NetBird setup key required")
	if err := f.svc.SetProviderPermission(t.Context(), "site", string(NetbirdExpose), true, core.ViaConsole); !errors.Is(err, core.ErrProviderNotReady) {
		t.Fatalf("public grant failure = %v", err)
	}
	if ep := grantEndpoint(t, f.svc, NetbirdExpose); ep.State != "error" {
		t.Fatalf("failed public grant was not observed: %+v", ep)
	}
	n.serveErr = nil
	f.svc.RetryProviderRoutes(t.Context(), Netbird, NetbirdExpose)
	if ep := grantEndpoint(t, f.svc, NetbirdExpose); !ep.Ready || n.public != 1 || n.private != 0 {
		t.Fatalf("key retry did not recover the observed public route: %+v public=%d private=%d", ep, n.public, n.private)
	}
	if get(t, n.PublicURL("site")) != "v1" {
		t.Fatal("retried public route did not serve current content")
	}
	if f.public.opens != 1 || f.public.stops != 0 || get(t, f.public.URL("site")) != "v1" {
		t.Fatal("key retry interrupted Portal")
	}
	// Confirmed public teardown leaves permissions allowed. A later key
	// registration must not reopen either stopped public route. Individual
	// public-provider stops are not supported by StopProviderRoutes.
	stopped, err := f.manager.StopPublicRoutes(t.Context(), "site")
	if err != nil || len(stopped.Unconfirmed) != 0 {
		t.Fatalf("public teardown failed: %+v %v", stopped, err)
	}
	f.svc.RetryProviderRoutes(t.Context(), Netbird, NetbirdExpose)
	if n.public != 1 || n.net.Serving("nbx-site") {
		t.Fatal("key retry reopened a stopped public route")
	}
	if f.public.opens != 1 || f.public.stops != 1 || f.public.URL("site") != "" {
		t.Fatal("key retry reopened stopped Portal")
	}
}

func TestProviderKeyRetryRecoversPrivateMeshOnly(t *testing.T) {
	f := reviewerService(t, Portal)
	n := newFakeNetbird(t)
	f.manager.AttachNetbird(n)
	for _, id := range []ID{Netbird, NetbirdExpose} {
		if _, err := f.manager.SetGrant(id, true); err != nil {
			t.Fatal(err)
		}
	}
	n.serveErr = errors.New("NetBird setup key required")
	if err := f.svc.SetProviderPermission(t.Context(), "site", string(Netbird), true, core.ViaConsole); !errors.Is(err, core.ErrProviderNotReady) {
		t.Fatalf("private grant failure = %v", err)
	}
	if err := f.svc.SetProviderPermission(t.Context(), "site", string(NetbirdExpose), true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	n.serveErr = nil
	f.svc.RetryProviderRoutes(t.Context(), Netbird, NetbirdExpose)
	fv, err := f.svc.GetFlat(t.Context(), "site")
	if err != nil {
		t.Fatal(err)
	}
	if fv.Visibility != store.Private || fv.PublicURL != "" || n.private != 1 || n.public != 0 || get(t, n.PrivateURL("site")) != "v1" {
		t.Fatalf("key retry changed Private exposure: flat=%+v private=%d public=%d", fv, n.private, n.public)
	}
}
