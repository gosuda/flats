package portal_test

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/portal"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/store"
)

// docs/spec/portal-require-auth.md through the host: core, the provider manager and a Portal Net whose exposures are
// loopback listeners (the harness of the sign-in tests).

const gatedHost = "gated-0xfed.relay.test"

func setRequireAuth(t *testing.T, h *host, on bool) {
	t.Helper()
	v := "false"
	if on {
		v = "true"
	}
	if _, err := h.svc.UpdateSettings(context.Background(), map[string]string{core.SetRequireAuth: v}); err != nil {
		t.Fatal(err)
	}
}

func portalEndpointOf(t *testing.T, h *host, slug string) core.ExposureEndpoint {
	t.Helper()
	fv, err := h.svc.GetFlat(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range fv.Endpoints {
		if ep.Provider == core.ProviderPortal && ep.Host == slug {
			return ep
		}
	}
	t.Fatalf("%s has no Portal endpoint: %+v", slug, fv.Endpoints)
	return core.ExposureEndpoint{}
}

func TestRequireAuthOnThePortalRoute(t *testing.T) {
	ctx := context.Background()
	h := startHost(t, t.TempDir())
	deployServer(t, h, "app")
	publicOnPortal(t, h, "app")
	exp := h.px.Of(t, "app")
	exp.SetRelays(portal.ReadyRelay("relay.test", appHost))
	anon := portal.NewTestVisitor(exp.Addr(), appHost)
	wantPage(t, anon, "/", "v1 /")
	open := openSocketOK(t, anon)

	deployServer(t, h, "gated")
	publicOnPortal(t, h, "gated")
	approveSignIn(t, h, "gated", credential("guest-7Qx"))
	gexp := h.px.Of(t, "gated")
	gexp.SetRelays(portal.ReadyRelay("relay.test", gatedHost))
	gated := portal.NewTestVisitor(gexp.Addr(), gatedHost)
	wantSignInPage(t, gated, "/")

	// A flat served on Portal without sign-in stops being served when the setting turns on, and the connection that
	// was open on it ends (rule 16).
	setRequireAuth(t, h, true)
	waitClosed(t, open)
	if exp.Closes() != 1 {
		t.Fatalf("the Portal route of app closed %d times, want once", exp.Closes())
	}
	want := "portal.require_auth is on and app has no sign-in. Run flats signin set app to turn sign-in on, or turn the setting off in the console."
	if ep := portalEndpointOf(t, h, "app"); ep.State != "unavailable" || ep.Ready || ep.Detail != want {
		t.Fatalf("the Portal endpoint of app = %+v", ep)
	}
	events, err := h.st.ListEvents(ctx, "app", "exposure", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[len(events)-1].Level != "warn" || events[len(events)-1].Message != want {
		t.Fatalf("events = %+v", events)
	}
	if !strings.Contains(h.logs.all(), "portal.require_auth: app: "+want) {
		t.Fatalf("the host log has no line for app: %s", h.logs.all())
	}
	// The private route stays, and a flat with sign-in keeps its Portal route and its gate (rules 6 and 13).
	wantPage(t, privateVisitor(t, h, "app"), "/", "v1 /")
	wantSignInPage(t, gated, "/")
	if refused, err := h.svc.PortalRefused(ctx, true); err != nil || len(refused) != 1 || refused[0] != "app" {
		t.Fatalf("the refused flats = %q, %v", refused, err)
	}

	// A new flat is not served on Portal either: the approval fails with the text, and no exposure opens (rule 19).
	deployServer(t, h, "fresh")
	if err := h.svc.SetProviderPermission(ctx, "fresh", store.ProviderPortal, true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	count := h.px.Count()
	r, err := h.svc.SetVisibility(ctx, "fresh", store.Public, core.ViaConsole, "")
	if err != nil || r.Approval == nil {
		t.Fatalf("request = %+v, %v", r, err)
	}
	if _, err := h.svc.Decide(ctx, r.Approval.ID, true); err == nil || !strings.Contains(err.Error(), "portal.require_auth is on and fresh has no sign-in") {
		t.Fatalf("the approval of fresh = %v, want the refusal", err)
	}
	if h.px.Count() != count {
		t.Fatal("a refused open made an exposure")
	}
	if f, _ := h.svc.GetFlat(ctx, "fresh"); f.Visibility != store.Private {
		t.Fatalf("fresh is %s, want Private", f.Visibility)
	}

	// Turning the setting off serves app on Portal again (rule 17).
	setRequireAuth(t, h, false)
	if h.px.Count() != count+1 {
		t.Fatalf("exposures = %d, want one more for app", h.px.Count())
	}
	exp2 := h.px.Of(t, "app")
	exp2.SetRelays(portal.ReadyRelay("relay.test", appHost))
	wantPage(t, portal.NewTestVisitor(exp2.Addr(), appHost), "/", "v1 /")
	if ep := portalEndpointOf(t, h, "app"); ep.State == "unavailable" {
		t.Fatalf("the Portal endpoint after the setting is off = %+v", ep)
	}
	if refused, err := h.svc.PortalRefused(ctx, true); err != nil || len(refused) != 0 {
		t.Fatalf("the refused flats = %q, %v", refused, err)
	}

}

// A change of the host setting writes no audit row and sends no webhook event, in either direction (rule 5). Only the
// flat event and the host log record its effect on a flat.
func TestRequireAuthWritesNoAuditRowAndSendsNoEvent(t *testing.T) {
	h := startHost(t, t.TempDir())
	deployServer(t, h, "app")
	publicOnPortal(t, h, "app")
	h.px.Of(t, "app").SetRelays(portal.ReadyRelay("relay.test", appHost))
	counts := func() (audit, notes int) {
		h.notes.mu.Lock()
		defer h.notes.mu.Unlock()
		return len(auditRows(t, h.dir, "")), len(h.notes.bodies)
	}
	for _, on := range []bool{true, false} {
		audit, notes := counts()
		setRequireAuth(t, h, on)
		if a, n := counts(); a != audit || n != notes {
			t.Fatalf("setting %t: audit rows %d to %d, webhook events %d to %d", on, audit, a, notes, n)
		}
	}
	if events, err := h.st.ListEvents(context.Background(), "app", "exposure", 0, 100); err != nil || len(events) < 2 {
		t.Fatalf("the flat events of the two changes = %+v, %v", events, err)
	}
}

// A request that is not a WebSocket and that the flat holds open ends when the setting turns on, because the visit ends
// the public connections before it stops the route, so the stop is confirmed (rules 16 and 18).
func TestRequireAuthEndsARunningRequest(t *testing.T) {
	h := startHost(t, t.TempDir())
	deployServer(t, h, "app")
	publicOnPortal(t, h, "app")
	exp := h.px.Of(t, "app")
	exp.SetRelays(portal.ReadyRelay("relay.test", appHost))
	anon := portal.NewTestVisitor(exp.Addr(), appHost)
	wantPage(t, anon, "/", "v1 /")

	c, err := net.Dial("tcp", exp.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := io.WriteString(c, "GET /hold HTTP/1.1\r\nHost: "+appHost+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("the request that the flat holds = %v, %v", resp, err)
	}
	c.SetReadDeadline(time.Time{})

	setRequireAuth(t, h, true)
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.Copy(io.Discard, resp.Body); errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("the request that the flat holds did not end")
	}
	if exp.Closes() != 1 {
		t.Fatalf("the Portal route closed %d times, want one confirmed stop", exp.Closes())
	}
	if ep := portalEndpointOf(t, h, "app"); ep.State != "unavailable" {
		t.Fatalf("the Portal endpoint = %+v: a stop that was not confirmed shows the state error", ep)
	}
	if has, _ := h.mgr.HasProviderRoute(context.Background(), "app", provider.Portal); has {
		t.Fatal("the route is still registered after a confirmed stop")
	}
}

// Before the lease of the relay has expired, the off-visit cannot confirm the stop: the route stays registered, in the state
// error, and the host says that the Portal route did not open. The lease is the default one of 2 minutes, which no run of
// this test reaches.
func TestRequireAuthOffVisitBeforeTheLeaseHasExpired(t *testing.T) {
	ctx := context.Background()
	h := startHost(t, t.TempDir())
	deployServer(t, h, "app")
	publicOnPortal(t, h, "app")
	exp := h.px.Of(t, "app")
	exp.SetRelays(portal.ReadyRelay("relay.test", appHost))
	exp.FailClose(errors.New("relay refused unregister"))
	setRequireAuth(t, h, true)
	count := h.px.Count()
	setRequireAuth(t, h, false)
	if has, _ := h.mgr.HasProviderRoute(ctx, "app", provider.Portal); !has || h.px.Count() != count {
		t.Fatalf("the off-visit forgot the route or made an exposure before the lease expired: registered %t, exposures %d to %d", has, count, h.px.Count())
	}
	events, err := h.st.ListEvents(ctx, "app", "exposure", 0, 100)
	if err != nil || len(events) == 0 || !strings.Contains(events[len(events)-1].Message, "turned off, but the Portal route of app did not open") {
		t.Fatalf("events = %+v, %v", events, err)
	}
}

// Rule 17 with the real manager and the real Portal backend: a route whose stop was not confirmed stays registered, in the
// state error, and the detail that the manager reports for it is the one of the Portal backend (core.TeardownPending).
// The visit that turns the setting off opens it again, whichever path left it stuck.
func TestRequireAuthOpensARouteWhoseStopWasNotConfirmed(t *testing.T) {
	failedList := func(t *testing.T, h *host) {
		t.Helper()
		db, err := sql.Open("sqlite", filepath.Join(h.dir, "flats.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`ALTER TABLE flats RENAME TO flats_gone`); err != nil {
			t.Fatal(err)
		}
		setRequireAuth(t, h, true)
		if _, err := db.Exec(`ALTER TABLE flats_gone RENAME TO flats`); err != nil {
			t.Fatal(err)
		}
	}
	for name, turnOn := range map[string]func(*testing.T, *host){
		"the visit":     func(t *testing.T, h *host) { setRequireAuth(t, h, true) },
		"a failed list": failedList,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			h := startHost(t, t.TempDir())
			// A failed unregistration counts as confirmed once the relay has dropped the lease (the SDK closes only once).
			h.pn.SetLeaseTTL(200 * time.Millisecond)
			deployServer(t, h, "app")
			publicOnPortal(t, h, "app")
			exp := h.px.Of(t, "app")
			exp.SetRelays(portal.ReadyRelay("relay.test", appHost))
			exp.FailClose(errors.New("relay refused unregister"))

			turnOn(t, h)

			if has, _ := h.mgr.HasProviderRoute(ctx, "app", provider.Portal); !has {
				t.Fatal("a stop that was not confirmed forgot the route")
			}
			if ep := portalEndpointOf(t, h, "app"); ep.State != "error" || !strings.Contains(ep.Detail, core.TeardownPending) {
				t.Fatalf("the endpoint of the stuck route = %+v, want the state error and the detail of the backend", ep)
			}
			// Nothing serves the slug until the off-visit, so the route is still registered in the state error when the
			// lease has expired, however slow the run is (a visit that serves it again would confirm the stop).
			time.Sleep(300 * time.Millisecond)
			if has, _ := h.mgr.HasProviderRoute(ctx, "app", provider.Portal); !has {
				t.Fatal("the route was forgotten before the off-visit")
			}
			if ep := portalEndpointOf(t, h, "app"); ep.State != "error" || !strings.Contains(ep.Detail, core.TeardownPending) {
				t.Fatalf("the endpoint of the stuck route after the lease = %+v", ep)
			}
			count := h.px.Count()
			setRequireAuth(t, h, false)
			if h.px.Count() != count+1 {
				t.Fatalf("exposures = %d, want one more: the visit did not open the route whose stop was not confirmed", h.px.Count())
			}
			exp2 := h.px.Of(t, "app")
			exp2.SetRelays(portal.ReadyRelay("relay.test", appHost))
			wantPage(t, portal.NewTestVisitor(exp2.Addr(), appHost), "/", "v1 /")
			events, err := h.st.ListEvents(ctx, "app", "exposure", 0, 100)
			if err != nil || len(events) == 0 || !strings.Contains(events[len(events)-1].Message, "the Portal route of app is open again") {
				t.Fatalf("events = %+v, %v", events, err)
			}
		})
	}
}
