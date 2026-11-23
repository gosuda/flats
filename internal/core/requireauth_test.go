package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/store"
)

// docs/spec/portal-require-auth.md: while portal.require_auth is on, Portal serves a flat only when the flat has
// sign-in. The Portal provider and the provider manager have their own tests of the refusal; these tests run the
// service against a fake net that refuses a Portal open that carries the setting, as the manager does.

// requireAuthNet is signInRouteNet with a hook at each question whether a Portal route is registered, which the visit
// asks first for each flat (rule 16). A test uses it to break the store between that question and the read of the flat.
type requireAuthNet struct {
	*signInRouteNet
	onHas func(host string)
}

func newRequireAuthNet() *requireAuthNet { return &requireAuthNet{signInRouteNet: newSignInRouteNet()} }

func (n *requireAuthNet) HasProviderRoute(ctx context.Context, host string, id ProviderID) (bool, error) {
	if n.onHas != nil && id == ProviderPortal {
		n.onHas(host)
	}
	return n.signInRouteNet.HasProviderRoute(ctx, host, id)
}

// newRequireAuthService is a service with a fake net and a log that a test can read.
func newRequireAuthService(t *testing.T) (*Service, *requireAuthNet, string, func() string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var log strings.Builder
	s, err := New(context.Background(), Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(&log, format+"\n", args...)
		t.Logf(format, args...)
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); st.Close() })
	network := newRequireAuthNet()
	s.cfg.Lifecycle = network
	return s, network, dir, func() string { mu.Lock(); defer mu.Unlock(); return log.String() }
}

func setRequireAuth(t *testing.T, s *Service, on bool) {
	t.Helper()
	if _, err := s.UpdateSettings(t.Context(), map[string]string{SetRequireAuth: strconv.FormatBool(on)}); err != nil {
		t.Fatal(err)
	}
}

// requireAuthDetailOf is the detail of rule 10 for a flat without a grant for another public provider that sign-in excludes.
func requireAuthDetailOf(slug string) string {
	return "portal.require_auth is on and " + slug + " has no sign-in. Run flats signin set " + slug + " to turn sign-in on, or turn the setting off in the console."
}

func portalEndpoint(t *testing.T, s *Service, slug string) ExposureEndpoint {
	t.Helper()
	fv, err := s.GetFlat(t.Context(), slug)
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range fv.Endpoints {
		if ep.Provider == ProviderPortal && ep.Host == slug {
			return ep
		}
	}
	t.Fatalf("%s has no Portal endpoint: %+v", slug, fv.Endpoints)
	return ExposureEndpoint{}
}

func providerRequest(h http.Handler, id ProviderID) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "https://app.test/", nil)
	return serve(h, r.WithContext(WithRouteProvider(r.Context(), id)))
}

// epochOf returns the route epoch of the public routes of slug that the service holds now, and nil when it holds none.
func epochOf(s *Service, slug string) *routeEpoch {
	if e, ok := s.routeEpochs.Load("public:" + slug); ok {
		return e.(*routeEpoch)
	}
	return nil
}

func eventTexts(t *testing.T, s *Service, slug string) []string {
	t.Helper()
	evs, err := s.st.ListEvents(t.Context(), slug, "exposure", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		out = append(out, e.Level+": "+e.Message)
	}
	return out
}

func hasEvent(texts []string, level, part string) bool {
	return slices.ContainsFunc(texts, func(s string) bool { return strings.HasPrefix(s, level+": ") && strings.Contains(s, part) })
}

// Rules 6 to 8, 10 and 19: with the setting on, the Portal route of a flat without sign-in does not open, its endpoint
// shows the refusal, and the way out that the detail names is a way that the host accepts.
func TestRequireAuthRefusesANewPortalRoute(t *testing.T) {
	ctx := t.Context()
	s, network, _, _ := newRequireAuthService(t)
	setRequireAuth(t, s, true)

	// Portal is the only public route: the visibility approval fails with the text, and the flat stays Private.
	lifecycleSave(t, s, "solo", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "solo"))
	if err := s.SetProviderPermission(ctx, "solo", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	req, err := s.SetVisibility(ctx, "solo", store.Public, ViaAPI, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, req.Approval.ID, true); err == nil || !strings.Contains(err.Error(), requireAuthDetailOf("solo")) {
		t.Fatalf("the approval of a Portal-only flat = %v, want the text of rule 10", err)
	}
	if f, _ := s.st.GetFlat(ctx, "solo"); f.Visibility != store.Private || network.serves("solo", ProviderPortal) {
		t.Fatalf("solo is %s, Portal serves %t", f.Visibility, network.serves("solo", ProviderPortal))
	}

	// A second public provider serves, and only Portal is refused (decision 5).
	publicLifecycleFlat(t, s, "pair", store.ProviderFunnel, store.ProviderPortal)
	if !network.serves("pair", ProviderFunnel) || network.serves("pair", ProviderPortal) {
		t.Fatalf("pair: Funnel %t, Portal %t", network.serves("pair", ProviderFunnel), network.serves("pair", ProviderPortal))
	}
	ep := portalEndpoint(t, s, "pair")
	want := "portal.require_auth is on and pair has no sign-in. Sign-in is refused while a grant for tailscale-funnel exists. " +
		"Make the flat Private, revoke tailscale-funnel under Networks, then run flats signin set pair. Or turn the setting off in the console."
	if ep.State != "unavailable" || ep.Ready || ep.Detail != want {
		t.Fatalf("the Portal endpoint of pair = %+v", ep)
	}

	// Every action of the detail is one that the host accepts.
	req, err = s.SetVisibility(ctx, "pair", store.Private, ViaAPI, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, req.Approval.ID, true); err != nil {
		t.Fatalf("make the flat Private: %v", err)
	}
	if err := s.SetProviderPermission(ctx, "pair", store.ProviderFunnel, false, ViaConsole); err != nil {
		t.Fatalf("revoke Funnel: %v", err)
	}
	approveSignIn(t, s, "pair", oidcRequest)
	req, err = s.SetVisibility(ctx, "pair", store.Public, ViaAPI, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, req.Approval.ID, true); err != nil {
		t.Fatalf("make the flat Public with sign-in on: %v", err)
	}
	if !network.serves("pair", ProviderPortal) {
		t.Fatal("the Portal route of pair does not open once it has sign-in")
	}
}

// Rule 7: an open of the Portal route carries the setting, and only for a flat without sign-in. The rename redirect is
// never refused, and an open that carries nothing is not refused either (the backend cannot know the setting).
func TestRequireAuthWhatAnOpenCarries(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "app", store.ProviderPortal, store.ProviderFunnel)
	publicLifecycleFlat(t, s, "gated", store.ProviderPortal)
	approveSignIn(t, s, "gated", oidcRequest)
	setRequireAuth(t, s, true)

	carried := func(slug string) *PortalSignIn {
		t.Helper()
		f, err := s.st.GetFlat(ctx, slug)
		if err != nil {
			t.Fatal(err)
		}
		fresh := newRequireAuthNet()
		s.cfg.Lifecycle = fresh
		if err := s.ensureLifecycle(ctx, f, fresh); err != nil && !strings.Contains(err.Error(), "portal.require_auth") {
			t.Fatal(err)
		}
		for _, req := range fresh.reqs {
			if req.Visibility == "public" && req.Host == slug {
				return req.SignIn
			}
		}
		t.Fatalf("no public open of %s", slug)
		return nil
	}
	if si := carried("app"); !si.Refused() || si.Refuse == "" || si.On() {
		t.Fatalf("the open of a flat without sign-in carries %v", si)
	}
	if si := carried("gated"); si.Refused() || !si.On() {
		t.Fatalf("the open of a flat with sign-in carries %v", si)
	}
	if (&PortalSignIn{Redirect: true, Refuse: "x"}).Refused() || (*PortalSignIn)(nil).Refused() {
		t.Fatal("the redirect and an open that carries nothing are refused")
	}
	// With the setting off the open carries nothing to refuse.
	setRequireAuth(t, s, false)
	if si := carried("app"); si.Refused() {
		t.Fatalf("the setting is off and the open carries %v", si)
	}
}

// Rules 16 and 17 and decision 6: turning the setting on stops the Portal route of a served flat without sign-in, ends
// its open connections, serves its other public routes again, and records the refusal. Turning it off opens the route.
func TestRequireAuthStopsAServedFlatAndOpensItAgain(t *testing.T) {
	s, network, _, log := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "app", store.ProviderFunnel, store.ProviderPortal)
	publicLifecycleFlat(t, s, "gated", store.ProviderPortal)
	approveSignIn(t, s, "gated", oidcRequest)
	before := network.handler("app", ProviderFunnel)
	if !network.serves("app", ProviderPortal) || providerRequest(before, ProviderFunnel).Code != http.StatusOK {
		t.Fatal("the test did not serve app on Funnel and Portal")
	}

	setRequireAuth(t, s, true)

	if network.serves("app", ProviderPortal) {
		t.Fatal("the Portal route of app stayed")
	}
	if !network.serves("gated", ProviderPortal) {
		t.Fatal("a flat with sign-in lost its Portal route")
	}
	// The connections that were open end (the old epoch answers 503), and the Funnel route serves again.
	if code := providerRequest(before, ProviderFunnel).Code; code != http.StatusServiceUnavailable {
		t.Fatalf("a request on the old handler = %d, want 503 (the epoch is cancelled)", code)
	}
	if code := providerRequest(network.handler("app", ProviderFunnel), ProviderFunnel).Code; code != http.StatusOK {
		t.Fatalf("the Funnel route after the change = %d", code)
	}
	if ep := portalEndpoint(t, s, "app"); ep.State != "unavailable" || ep.Detail == "" || !strings.HasPrefix(ep.Detail, PortalAuthDetailPrefix) {
		t.Fatalf("the Portal endpoint = %+v", ep)
	}
	if events := eventTexts(t, s, "app"); !hasEvent(events, "warn", "portal.require_auth is on and app has no sign-in") {
		t.Fatalf("events = %q", events)
	}
	if !strings.Contains(log(), "portal.require_auth: app: portal.require_auth is on and app has no sign-in") {
		t.Fatalf("the host log has no line for app: %s", log())
	}
	if events := eventTexts(t, s, "gated"); hasEvent(events, "warn", "portal.require_auth") {
		t.Fatalf("a flat with sign-in has an event of the setting: %q", events)
	}

	setRequireAuth(t, s, false)
	if !network.serves("app", ProviderPortal) {
		t.Fatal("the Portal route of app did not open again")
	}
	if ep := portalEndpoint(t, s, "app"); ep.State != "ready" {
		t.Fatalf("the Portal endpoint after the setting is off = %+v", ep)
	}
	if events := eventTexts(t, s, "app"); !hasEvent(events, "info", "the Portal route of app is open again") {
		t.Fatalf("events = %q", events)
	}
}

// Rule 15: a route that opened without the check of the open still gives no access to a flat without sign-in on
// Portal, and no other provider is touched.
func TestRequireAuthChecksEachRequest(t *testing.T) {
	s, dir := newTestService(t)
	network := newSignInRouteNet()
	s.cfg.Lifecycle = network
	makeFunnelPublic(t, s, "app", "page")
	handler := network.handler("app", ProviderFunnel)
	if handler == nil {
		t.Fatal("no route")
	}
	setRequireAuth(t, s, true)
	// The test serves the flat on a route that the visit does not know: the same site handler, marked Portal.
	site := s.siteHandler("app", true)
	if rec := providerRequest(site, ProviderPortal); rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != requireAuthRequestText {
		t.Fatalf("a Portal request = %d %q", rec.Code, rec.Body)
	}
	if rec := providerRequest(site, ProviderFunnel); rec.Code != http.StatusOK {
		t.Fatalf("a Funnel request = %d", rec.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "https://app.test/", nil)
	if rec := serve(site, r); rec.Code != http.StatusOK {
		t.Fatalf("a request without a provider mark = %d", rec.Code)
	}
	setRequireAuth(t, s, false)
	if rec := providerRequest(site, ProviderPortal); rec.Code != http.StatusOK {
		t.Fatalf("a Portal request with the setting off = %d", rec.Code)
	}
	// A flat with sign-in on gets the answer of sign-in rule 44, not this one.
	setRequireAuth(t, s, true)
	writeSignInRow(t, dir, "app", store.SignInCredential, 1, "guest-7Qx")
	if rec := providerRequest(site, ProviderPortal); rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != signInNeededText {
		t.Fatalf("a Portal request for a flat with sign-in = %d %q", rec.Code, rec.Body)
	}
}

// Rule 18, first path: the host cannot read one flat. It ends that flat's public connections and stops its Portal route,
// and the visit goes on with the other flats.
func TestRequireAuthFlatThatCannotBeRead(t *testing.T) {
	s, network, dir, log := newRequireAuthService(t)
	for _, slug := range []string{"one", "two"} {
		publicLifecycleFlat(t, s, slug, store.ProviderFunnel, store.ProviderPortal)
	}
	var visited []string
	var original int64
	db := rawDB(t, dir)
	network.onHas = func(host string) {
		if slices.Contains(visited, host) {
			return
		}
		visited = append(visited, host)
		switch len(visited) {
		case 1: // the read of the first flat fails
			if err := db.QueryRow(`SELECT created_at FROM flats WHERE slug=?`, host).Scan(&original); err != nil {
				t.Error(err)
			}
			if _, err := db.Exec(`UPDATE flats SET created_at='unreadable' WHERE slug=?`, host); err != nil {
				t.Error(err)
			}
		case 2: // the second flat reads
			if _, err := db.Exec(`UPDATE flats SET created_at=? WHERE slug=?`, original, visited[0]); err != nil {
				t.Error(err)
			}
		}
	}
	old := map[string]http.Handler{"one": network.handler("one", ProviderFunnel), "two": network.handler("two", ProviderFunnel)}
	epochs := map[string]*routeEpoch{"one": epochOf(s, "one"), "two": epochOf(s, "two")}

	setRequireAuth(t, s, true)
	network.onHas = nil

	if len(visited) != 2 {
		t.Fatalf("the visit asked about %v", visited)
	}
	failed, read := visited[0], visited[1]
	for _, slug := range visited {
		if network.serves(slug, ProviderPortal) {
			t.Fatalf("the Portal route of %s stayed", slug)
		}
	}
	// The flat that cannot be read: its public connections end, and its other public routes answer 503 until a deploy,
	// a grant or a restart serves them again (decision 7).
	if epochs[failed].ctx.Err() == nil {
		t.Fatalf("the route epoch of %s did not end", failed)
	}
	if code := providerRequest(old[failed], ProviderFunnel).Code; code != http.StatusServiceUnavailable {
		t.Fatalf("a connection of %s that was open = %d, want it ended", failed, code)
	}
	if code := providerRequest(network.handler(failed, ProviderFunnel), ProviderFunnel).Code; code != http.StatusServiceUnavailable {
		t.Fatalf("the Funnel route of %s = %d, want 503 on the cancelled epoch", failed, code)
	}
	if len(network.failed) != 1 || !strings.Contains(network.failed[0], "portal.require_auth turned on, but the host cannot read "+failed+" to check its sign-in") ||
		!strings.Contains(network.failed[0], "A deploy, a grant, a restart or a sign-in change tries again.") {
		t.Fatalf("FailPortalGate = %q", network.failed)
	}
	if events := eventTexts(t, s, failed); !hasEvent(events, "error", "Its Portal route is stopped and its public connections are ended. Its other public routes answer 503 until a deploy, a grant or a restart.") {
		t.Fatalf("events of %s = %q", failed, events)
	}
	// The visit went on: the other flat is refused as usual, and its Funnel route serves again with a new epoch.
	if code := providerRequest(network.handler(read, ProviderFunnel), ProviderFunnel).Code; code != http.StatusOK {
		t.Fatalf("the Funnel route of %s = %d", read, code)
	}
	if events := eventTexts(t, s, read); !hasEvent(events, "warn", "portal.require_auth is on and "+read+" has no sign-in") {
		t.Fatalf("events of %s = %q", read, events)
	}
	if !strings.Contains(log(), "portal.require_auth: "+failed+": portal.require_auth turned on, but the host cannot read") {
		t.Fatalf("the host log = %s", log())
	}
}

// Rule 18, second path: the host cannot list the flats. It stops the Portal route of every live flat that has one,
// ends no connection, leaves the route of a rename redirect, and the host log names the flats also when the events
// cannot be written. Turning the setting off opens the stopped routes again, with sign-in on or off (rule 17).
func TestRequireAuthFlatsThatCannotBeListed(t *testing.T) {
	ctx := t.Context()
	s, network, dir, log := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "open", store.ProviderFunnel, store.ProviderPortal)
	publicLifecycleFlat(t, s, "gated", store.ProviderPortal)
	approveSignIn(t, s, "gated", oidcRequest)
	publicLifecycleFlat(t, s, "funnel", store.ProviderFunnel)
	// A rename leaves the redirect of the old slug on Portal, and the stray state of that slug in Service.live.
	if _, err := s.RenameSlug(ctx, "gated", "moved", ViaCLI); err != nil {
		t.Fatal(err)
	}
	s.state("gated")
	if !network.serves("gated", ProviderPortal) {
		t.Fatal("the test has no redirect route on Portal")
	}
	epochs := map[string]*routeEpoch{"open": epochOf(s, "open"), "funnel": epochOf(s, "funnel")}
	if epochs["open"] == nil || epochs["funnel"] == nil {
		t.Fatal("the test flats hold no route epoch")
	}

	db := rawDB(t, dir)
	if _, err := db.Exec(`ALTER TABLE flats RENAME TO flats_gone`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE events RENAME TO events_gone`); err != nil {
		t.Fatal(err)
	}
	setRequireAuth(t, s, true)

	for _, slug := range []string{"open", "moved"} {
		if network.serves(slug, ProviderPortal) {
			t.Fatalf("the Portal route of %s stayed", slug)
		}
	}
	if !network.serves("gated", ProviderPortal) {
		t.Fatal("the redirect route of the old slug stopped")
	}
	// No connection ends and no other route changes (decision 7).
	for slug, e := range epochs {
		if e.ctx.Err() != nil || epochOf(s, slug) != e {
			t.Fatalf("the route epoch of %s ended on the path of a failed list", slug)
		}
	}
	lines := log()
	if !strings.Contains(lines, "stopped the Portal route of 2 flats: moved, open") {
		t.Fatalf("the host log names no stopped flats: %s", lines)
	}
	if !strings.Contains(lines, "portal.require_auth: open: portal.require_auth turned on, but the host cannot list the flats to check open") ||
		!strings.Contains(lines, "A Portal connection that is open stays until it closes or the host restarts.") {
		t.Fatalf("the host log has no text of the event of open: %s", lines)
	}

	// Rule 17: the setting off opens them again.
	if _, err := db.Exec(`ALTER TABLE flats_gone RENAME TO flats`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE events_gone RENAME TO events`); err != nil {
		t.Fatal(err)
	}
	setRequireAuth(t, s, false)
	for _, slug := range []string{"open", "moved"} {
		if !network.serves(slug, ProviderPortal) {
			t.Fatalf("the Portal route of %s did not open again", slug)
		}
	}
}

// Rule 16: a flat with no registered Portal route is left as it is, also when the host cannot read it.
func TestRequireAuthLeavesAFlatWithoutAPortalRoute(t *testing.T) {
	s, network, dir, _ := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "other", store.ProviderFunnel)
	publicLifecycleFlat(t, s, "both", store.ProviderFunnel, store.ProviderPortal)
	epoch := epochOf(s, "other")
	eventsBefore := eventTexts(t, s, "other")
	db := rawDB(t, dir)
	if _, err := db.Exec(`ALTER TABLE flats RENAME TO flats_gone`); err != nil {
		t.Fatal(err)
	}
	setRequireAuth(t, s, true)
	if network.serves("both", ProviderPortal) {
		t.Fatal("the Portal route of both stayed")
	}
	if epoch == nil || epoch.ctx.Err() != nil || epochOf(s, "other") != epoch || !network.serves("other", ProviderFunnel) {
		t.Fatal("the epoch or the route of a flat without a Portal route changed")
	}
	if events := eventTexts(t, s, "other"); !slices.Equal(events, eventsBefore) {
		t.Fatalf("events of other = %q, before %q", events, eventsBefore)
	}
}

// Rule 17: the visit that turns the setting off serves again a route that is not registered, or registered with a state
// that a stop left (a stop that is not confirmed keeps the route registered and shows the state error), and leaves a
// route that serves.
func TestRequireAuthReopenConditions(t *testing.T) {
	ctx := t.Context()
	s, network, _, _ := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "app", store.ProviderPortal, store.ProviderFunnel)
	needs := func() bool {
		t.Helper()
		v, err := s.portalNeedsReopen(ctx, "app")
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if needs() {
		t.Fatal("a Portal route that serves needs to open again")
	}
	// What the provider manager reports for a registered route whose stop was not confirmed: the detail of the Portal
	// backend (the test of package portal_test makes it with the real manager and backend).
	stuck := "sign-in gate: x; " + TeardownPending
	for name, tc := range map[string]struct {
		state, detail string
		want          bool
	}{
		"a relay fault":        {"error", "relay unreachable", false},
		"an unconfirmed stop":  {"error", stuck, true},
		"unavailable":          {"unavailable", "", false},
		"starting":             {"starting", "", false},
		"key-expiring":         {"key-expiring", "", false},
		"error without detail": {"error", "", false},
	} {
		network.degraded = map[ProviderID]string{ProviderPortal: tc.state}
		network.degradedDetail = map[ProviderID]string{ProviderPortal: tc.detail}
		if got := needs(); got != tc.want {
			t.Errorf("a registered Portal route, %s: needs to open again = %t, want %t", name, got, tc.want)
		}
	}
	network.degraded, network.degradedDetail = nil, nil
	// A failed read of the routes is an error, not an answer.
	network.statusErr = errors.New("status unreadable")
	if v, err := s.portalNeedsReopen(ctx, "app"); err == nil || v {
		t.Fatalf("a failed read of the routes = %t, %v, want an error", v, err)
	}
	network.statusErr = nil
	if err := network.StopProviderRoutes(ctx, "app", ProviderPortal); err != nil {
		t.Fatal(err)
	}
	if !needs() {
		t.Fatal("a Portal route that is not registered does not need to open again")
	}
}

// Rule 10: the list of the status is derived from the stored facts, so no record of a refusal outlives its cause.
func TestPortalRefusedList(t *testing.T) {
	ctx := t.Context()
	s, _, dir, _ := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "aaa", store.ProviderPortal) // the flat that gets sign-in has no grant that sign-in excludes
	for _, slug := range []string{"bbb", "ccc", "ddd"} {
		publicLifecycleFlat(t, s, slug, store.ProviderPortal, store.ProviderFunnel)
	}
	publicLifecycleFlat(t, s, "gated", store.ProviderPortal)
	approveSignIn(t, s, "gated", oidcRequest)
	publicLifecycleFlat(t, s, "nogrant", store.ProviderFunnel)
	lifecycleSave(t, s, "private", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "private"))
	if err := s.SetProviderPermission(ctx, "private", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	list := func(portal bool) []string {
		t.Helper()
		got, err := s.PortalRefused(ctx, portal)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := list(true); len(got) != 0 {
		t.Fatalf("the setting is off and the list is %q", got)
	}
	setRequireAuth(t, s, true)
	if got := list(true); !slices.Equal(got, []string{"aaa", "bbb", "ccc", "ddd"}) {
		t.Fatalf("the list = %q, want the Public flats with a Portal grant and no sign-in", got)
	}
	if got := list(false); len(got) != 0 {
		t.Fatalf("Portal is not permitted for the host and the list is %q", got)
	}
	// A flat leaves the list when it gets sign-in, loses its Portal grant, becomes Private or is deleted.
	approveSignIn(t, s, "aaa", oidcRequest)
	if got := list(true); !slices.Equal(got, []string{"bbb", "ccc", "ddd"}) {
		t.Fatalf("after sign-in on aaa: %q", got)
	}
	if err := s.SetProviderPermission(ctx, "bbb", store.ProviderPortal, false, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if got := list(true); !slices.Equal(got, []string{"ccc", "ddd"}) {
		t.Fatalf("after the revoke of bbb: %q", got)
	}
	req, err := s.SetVisibility(ctx, "ccc", store.Private, ViaAPI, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, req.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := list(true); !slices.Equal(got, []string{"ddd"}) {
		t.Fatalf("after ccc became Private: %q", got)
	}
	if _, err := rawDB(t, dir).Exec(`DELETE FROM flats WHERE slug='ddd'`); err != nil {
		t.Fatal(err)
	}
	if got := list(true); len(got) != 0 {
		t.Fatalf("after ddd was deleted: %q", got)
	}
	// With the store failing, the method returns the error, and the status shows null.
	if _, err := rawDB(t, dir).Exec(`ALTER TABLE flats RENAME TO flats_gone`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PortalRefused(ctx, true); err == nil || got != nil {
		t.Fatalf("a store that fails gave %q, %v", got, err)
	}
}

// Rule 20: a grant of Portal to a Public flat without sign-in is recorded, and its result says that the route is refused.
func TestRequireAuthGrantResult(t *testing.T) {
	ctx := t.Context()
	s, network, _, _ := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "app", store.ProviderFunnel)
	setRequireAuth(t, s, true)
	err := s.SetProviderPermission(ctx, "app", store.ProviderPortal, true, ViaConsole)
	var detail = "portal.require_auth is on and app has no sign-in. Sign-in is refused while a grant for tailscale-funnel exists."
	if err == nil || !errors.Is(err, ErrProviderNotReady) || !strings.Contains(err.Error(), "portal is permitted, but its route is refused: "+detail) ||
		strings.Contains(err.Error(), "allow this provider again") {
		t.Fatalf("the grant result = %v", err)
	}
	if ok, perr := s.st.ProviderPermitted(ctx, "app", store.ProviderPortal); perr != nil || !ok {
		t.Fatalf("the grant is not recorded: %t, %v", ok, perr)
	}
	if network.serves("app", ProviderPortal) {
		t.Fatal("the refused route opened")
	}
}

// Rule 21: turning sign-in off while the setting is on stops the Portal route, and the result says so. With the setting
// off the result is as before.
func TestRequireAuthSignInOffResult(t *testing.T) {
	ctx := t.Context()
	s, network, _, _ := newRequireAuthService(t)
	network.file = true
	for _, slug := range []string{"siton", "sitoff"} {
		publicLifecycleFlat(t, s, slug, store.ProviderPortal)
		approveSignIn(t, s, slug, guestRequest)
	}
	setRequireAuth(t, s, true)
	a := approveSignIn(t, s, "siton", SignInRequest{})
	if !strings.Contains(a.Result, "sign-in turned off; portal.require_auth is on, so the Portal route of siton is stopped") {
		t.Fatalf("the result = %q", a.Result)
	}
	if network.serves("siton", ProviderPortal) {
		t.Fatal("the Portal route of siton stayed after sign-in went off")
	}
	setRequireAuth(t, s, false)
	a = approveSignIn(t, s, "sitoff", SignInRequest{})
	if strings.Contains(a.Result, "portal.require_auth") || !network.serves("sitoff", ProviderPortal) {
		t.Fatalf("the result with the setting off = %q, Portal serves %t", a.Result, network.serves("sitoff", ProviderPortal))
	}
	_ = ctx
}

// Rule 19, the detail: without a grant that sign-in excludes the detail is the first form.
func TestRequireAuthDetailForms(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "plain", store.ProviderPortal)
	publicLifecycleFlat(t, s, "many", store.ProviderPortal, store.ProviderFunnel, store.ProviderZrok)
	if got := s.requireAuthDetail(ctx, "plain"); got != requireAuthDetailOf("plain") {
		t.Fatalf("the detail = %q", got)
	}
	got := s.requireAuthDetail(ctx, "many")
	if !strings.Contains(got, "while a grant for tailscale-funnel, zrok exists") || !strings.Contains(got, "revoke tailscale-funnel, zrok under Networks") ||
		!strings.HasPrefix(got, PortalAuthDetailPrefix) {
		t.Fatalf("the detail with two grants = %q", got)
	}
}

// Decision: the setting is a console and config setting. It is a boolean, it applies at once, and a change is saved
// before the visit runs.
func TestRequireAuthSettingValue(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := newRequireAuthService(t)
	if s.PortalRequireAuth() || Defaults[SetRequireAuth] != "false" {
		t.Fatal("the setting is on by default")
	}
	if _, err := s.UpdateSettings(ctx, map[string]string{SetRequireAuth: "maybe"}); err == nil || !strings.Contains(err.Error(), "must be true or false") {
		t.Fatalf("a value that is not a boolean = %v", err)
	}
	setRequireAuth(t, s, true)
	if key, ok := SettingConfigKey(SetRequireAuth); !s.PortalRequireAuth() || !ok || key != "portal.require_auth" {
		t.Fatalf("the setting is %t, key %q %t", s.PortalRequireAuth(), key, ok)
	}
	setRequireAuth(t, s, false)
	if s.PortalRequireAuth() {
		t.Fatal("the setting stayed on")
	}
}

// The issue: the setting survives a host restart. The console saves it to config.json, and a new service on the same
// files opens no Portal route for a flat without sign-in (rules 2 and 7).
func TestRequireAuthSurvivesARestart(t *testing.T) {
	ctx := t.Context()
	s, path := newReloadService(t)
	network := newRequireAuthNet()
	s.cfg.Lifecycle = network
	publicLifecycleFlat(t, s, "app", store.ProviderPortal, store.ProviderFunnel)
	if !network.serves("app", ProviderPortal) {
		t.Fatal("the test did not serve app on Portal")
	}
	setRequireAuth(t, s, true)
	if l, err := config.Load(path); err != nil {
		t.Fatal(err)
	} else if c, err := l.Doc.Effective(nil); err != nil {
		t.Fatal(err)
	} else if v, _, _ := c.Lookup("portal.require_auth"); v != true {
		t.Fatalf("config.json holds %v, want true", v)
	}

	// A restart: a new service on the same files serves the flat as the setting says.
	dir := filepath.Dir(path)
	l, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	src, err := NewSettingsSource(l.Doc, &SettingsFile{Mode: "config", Hash: l.Hash, Save: config.NewWriter(path).Save,
		DiskHash: func() (string, error) {
			b, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			return config.Hash(b), nil
		},
		Load: func() (*config.Loaded, error) { return config.Load(path) }})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	restarted := newRequireAuthNet()
	s2, err := New(ctx, Config{DataDir: dir, Store: st, Settings: src, Lifecycle: restarted,
		Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s2.Close(); st.Close() })
	if !s2.PortalRequireAuth() {
		t.Fatal("the setting did not survive the restart")
	}
	if restarted.serves("app", ProviderPortal) || !restarted.serves("app", ProviderFunnel) {
		t.Fatalf("after the restart Portal serves %t and Funnel serves %t", restarted.serves("app", ProviderPortal), restarted.serves("app", ProviderFunnel))
	}
	if ep := portalEndpoint(t, s2, "app"); ep.State != "unavailable" || !strings.HasPrefix(ep.Detail, PortalAuthDetailPrefix) {
		t.Fatalf("the Portal endpoint after the restart = %+v", ep)
	}
}

// A failure of another provider while the visit serves the public routes again is not taken for the refusal: the Portal
// route stops, and the flat event names the failure (rule 18).
func TestRequireAuthVisitReportsAProviderThatCannotOpen(t *testing.T) {
	s, network, _, _ := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "app", store.ProviderPortal, store.ProviderFunnel)
	network.failOpen = map[ProviderID]error{ProviderFunnel: errors.New("funnel is down")}
	setRequireAuth(t, s, true)
	if network.serves("app", ProviderPortal) {
		t.Fatal("the Portal route of app stayed")
	}
	events := eventTexts(t, s, "app")
	if !hasEvent(events, "error", "funnel is down") || !hasEvent(events, "error", "A public route that did not open again answers 503 until a deploy, a grant or a restart.") {
		t.Fatalf("events = %q", events)
	}
	if hasEvent(events, "warn", "portal.require_auth is on and app has no sign-in") {
		t.Fatalf("a failure of Funnel was taken for the refusal: %q", events)
	}
}

// Races: a change of the setting and an open of the Portal route of one flat that run at the same time give the result of
// one of the two orders. After both end, the flat is never served on Portal while the setting is on, and the Portal route
// is open again once the setting is off. Each open reads the setting under the lock of the flat (rule 7).
func TestRequireAuthChangeAndOpenAtTheSameTime(t *testing.T) {
	ctx := t.Context()
	s, network, _, _ := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "app", store.ProviderPortal, store.ProviderFunnel)
	for i := 0; i < 25; i++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			setRequireAuth(t, s, true)
		}()
		go func() {
			defer wg.Done()
			// An open that a grant or a key change makes: under the lock of the flat, as the callers do.
			unlock := s.lock("app")
			defer unlock()
			f, err := s.st.GetFlat(ctx, "app")
			if err != nil {
				t.Error(err)
				return
			}
			_ = s.reapplyExposure(ctx, f, ProviderPortal, true)
		}()
		wg.Wait()
		if network.serves("app", ProviderPortal) {
			t.Fatalf("round %d: the Portal route of a flat without sign-in is served while the setting is on", i)
		}
		setRequireAuth(t, s, false)
		if !network.serves("app", ProviderPortal) {
			t.Fatalf("round %d: the Portal route did not open when the setting went off", i)
		}
	}
}

// Rule 19: the result of a visibility apply says that Portal is refused, with the text of rule 10, when another public
// provider opened. A flat without a Portal grant, a flat with the setting off, and a flat with sign-in get no such sentence.
func TestRequireAuthVisibilityResult(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := newRequireAuthService(t)
	apply := func(slug string, providers ...string) store.Approval {
		t.Helper()
		lifecycleSave(t, s, slug, "one")
		lifecycleApprove(t, s, lifecycleRequest(t, s, slug))
		for _, p := range providers {
			if err := s.SetProviderPermission(ctx, slug, p, true, ViaConsole); err != nil {
				t.Fatal(err)
			}
		}
		req, err := s.SetVisibility(ctx, slug, store.Public, ViaAPI, "")
		if err != nil {
			t.Fatal(err)
		}
		a, err := s.Decide(ctx, req.Approval.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	if a := apply("before", store.ProviderPortal, store.ProviderFunnel); strings.Contains(a.Result, "Portal is refused") {
		t.Fatalf("the setting is off and the result says %q", a.Result)
	}
	setRequireAuth(t, s, true)
	a := apply("pair", store.ProviderPortal, store.ProviderFunnel)
	want := "; Portal is refused: portal.require_auth is on and pair has no sign-in. Sign-in is refused while a grant for tailscale-funnel exists."
	if !strings.HasPrefix(a.Result, "visibility changed to public") || !strings.Contains(a.Result, want) {
		t.Fatalf("the result = %q, want it to hold %q", a.Result, want)
	}
	if a := apply("nogrant", store.ProviderFunnel); strings.Contains(a.Result, "Portal is refused") {
		t.Fatalf("a flat without a Portal grant: the result says %q", a.Result)
	}
}

// Rule 17: a route that the setting did not stop is left alone. The off-visit writes nothing for a registered Portal
// route that serves, and nothing for a registered route that failed for another cause.
func TestRequireAuthOffVisitLeavesARouteThatServes(t *testing.T) {
	s, network, _, _ := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "app", store.ProviderPortal)
	approveSignIn(t, s, "app", oidcRequest)
	// The route is registered and failed for a relay fault, which the setting did not cause.
	network.degraded = map[ProviderID]string{ProviderPortal: "error"}
	network.degradedDetail = map[ProviderID]string{ProviderPortal: "relay unreachable"}
	setRequireAuth(t, s, true)
	setRequireAuth(t, s, false)
	for _, text := range eventTexts(t, s, "app") {
		if strings.Contains(text, "turned off") {
			t.Fatalf("the off-visit wrote an event for a route that the setting did not stop: %q", text)
		}
	}
	if !network.serves("app", ProviderPortal) {
		t.Fatal("the route of a flat with sign-in stopped")
	}
}

// Two changes at the same time end with the state of the last stored value, and a visit that runs late does nothing
// (rule 18, last bullet).
func TestRequireAuthTwoChangesAtTheSameTime(t *testing.T) {
	s, network, _, _ := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "app", store.ProviderPortal, store.ProviderFunnel)
	for round := 0; round < 15; round++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); setRequireAuth(t, s, true) }()
		go func() { defer wg.Done(); setRequireAuth(t, s, false) }()
		wg.Wait()
		on := s.PortalRequireAuth()
		if network.serves("app", ProviderPortal) == on {
			t.Fatalf("round %d: the setting is on %t and Portal serves %t", round, on, network.serves("app", ProviderPortal))
		}
		setRequireAuth(t, s, false)
		if !network.serves("app", ProviderPortal) {
			t.Fatalf("round %d: Portal does not serve with the setting off", round)
		}
	}

	// A late visit: the flat lock is held, so the visit of the first change waits after the second change is stored.
	wait := func(cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !cond(); {
			if time.Now().After(deadline) {
				t.Fatal("timeout")
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	unlock := s.lock("app")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); setRequireAuth(t, s, true) }()
	wait(s.PortalRequireAuth)
	wg.Add(1)
	go func() { defer wg.Done(); setRequireAuth(t, s, false) }()
	wait(func() bool { return !s.PortalRequireAuth() })
	unlock()
	wg.Wait()
	if !network.serves("app", ProviderPortal) {
		t.Fatal("a visit of an earlier change stopped the Portal route after the setting went off")
	}
}

// The stated limit of the spec: an open that is not the visit, such as a grant or a key change, can stop the Portal route of
// a flat between the change of the setting and the visit of that flat. It ends no connection that is open on Portal, because
// only the visit revokes the epoch. The test makes that order and pins it, so that a change of the limit is a decision.
func TestRequireAuthOpenBeforeTheVisitEndsNoConnection(t *testing.T) {
	ctx := t.Context()
	s, network, _, _ := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "app", store.ProviderPortal, store.ProviderFunnel)
	epoch := epochOf(s, "app")
	if epoch == nil {
		t.Fatal("the flat holds no route epoch")
	}
	unlock := s.lock("app") // the visit of the change waits for the flat
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); setRequireAuth(t, s, true) }()
	for deadline := time.Now().Add(5 * time.Second); !s.PortalRequireAuth(); {
		if time.Now().After(deadline) {
			t.Fatal("the setting was not stored")
		}
		time.Sleep(2 * time.Millisecond)
	}
	// The open that is not the visit runs first, under the lock of the flat.
	f, err := s.st.GetFlat(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reapplyExposure(ctx, f, ProviderPortal, true); err == nil {
		t.Fatal("the open of the Portal route was not refused")
	}
	unlock()
	wg.Wait()
	if network.serves("app", ProviderPortal) {
		t.Fatal("the Portal route stayed")
	}
	if epoch.ctx.Err() != nil || epochOf(s, "app") != epoch {
		t.Fatal("the epoch ended: the limit of the spec changed, and the spec and the docs must change with it")
	}
}

// A reload by SIGHUP does the same visit as a console save (rule 18).
func TestRequireAuthReloadDoesTheVisit(t *testing.T) {
	ctx := t.Context()
	s, path := newReloadService(t)
	network := newRequireAuthNet()
	s.cfg.Lifecycle = network
	publicLifecycleFlat(t, s, "app", store.ProviderPortal, store.ProviderFunnel)
	editConfig(t, path, func(d *config.Document) error { return d.Set("portal.require_auth", "true") })
	rep, err := s.ReloadConfig(ctx)
	if err != nil || !slices.Contains(rep.Applied, SetRequireAuth) || slices.Contains(rep.AwaitRestart, "portal.require_auth") {
		t.Fatalf("reload = %+v, %v: the key must be listed as applied", rep, err)
	}
	if network.serves("app", ProviderPortal) || !network.serves("app", ProviderFunnel) {
		t.Fatalf("after the reload Portal serves %t and Funnel serves %t", network.serves("app", ProviderPortal), network.serves("app", ProviderFunnel))
	}
	editConfig(t, path, func(d *config.Document) error { return d.Set("portal.require_auth", "false") })
	if _, err := s.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if !network.serves("app", ProviderPortal) {
		t.Fatal("the reload that turned the setting off did not open the Portal route")
	}
}

// A client that drops the request after the commit does not leave a flat served: the visit does not depend on it.
func TestRequireAuthSaveCancelledAfterTheCommit(t *testing.T) {
	s, network, _, _ := newRequireAuthService(t)
	for _, slug := range []string{"one", "two"} {
		publicLifecycleFlat(t, s, slug, store.ProviderPortal, store.ProviderFunnel)
	}
	ctx, cancel := context.WithCancel(t.Context())
	network.onHas = func(string) { cancel() }
	if _, err := s.UpdateSettings(ctx, map[string]string{SetRequireAuth: "true"}); err != nil {
		t.Fatal(err)
	}
	network.onHas = nil
	if ctx.Err() == nil {
		t.Fatal("the test did not cancel the request")
	}
	// An effect that only a completed visit has: the refusal event, no failed read, and the other public route that is
	// served again. A visit that reads with the cancelled context fails each read, stops each Portal route, and ends the
	// public connections without a re-serve.
	for _, slug := range []string{"one", "two"} {
		if network.serves(slug, ProviderPortal) {
			t.Fatalf("the Portal route of %s stayed after the request was cancelled", slug)
		}
		events := eventTexts(t, s, slug)
		if !hasEvent(events, "warn", "portal.require_auth is on and "+slug+" has no sign-in") || hasEvent(events, "error", "cannot read") {
			t.Fatalf("events of %s = %q", slug, events)
		}
		if code := providerRequest(network.handler(slug, ProviderFunnel), ProviderFunnel).Code; code != http.StatusOK {
			t.Fatalf("the Funnel route of %s = %d, want it served again", slug, code)
		}
	}
}

// A failed read of the routes on the off-visit opens nothing for that flat, and the host says why (rule 17).
func TestRequireAuthOffVisitWithUnreadableRoutes(t *testing.T) {
	ctx := t.Context()
	s, network, _, _ := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "gated", store.ProviderPortal)
	approveSignIn(t, s, "gated", oidcRequest)
	network.statusErr = errors.New("status unreadable")
	unlock := s.lock("gated")
	s.requireAuthReopen(ctx, "gated")
	unlock()
	if events := eventTexts(t, s, "gated"); !hasEvent(events, "error", "cannot read the routes of gated to open its Portal route again") {
		t.Fatalf("events = %q", events)
	}
	if !network.serves("gated", ProviderPortal) {
		t.Fatal("the route of a flat with sign-in stopped")
	}
}

// The off-visit that cannot list the flats opens nothing and says so in the log. A flat that it cannot read stays
// stopped with an event, and the next visit opens it (rule 17).
func TestRequireAuthOffVisitThatCannotRead(t *testing.T) {
	ctx := t.Context()
	s, network, dir, log := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "app", store.ProviderPortal, store.ProviderFunnel)
	setRequireAuth(t, s, true)
	db := rawDB(t, dir)
	if _, err := db.Exec(`ALTER TABLE flats RENAME TO flats_gone`); err != nil {
		t.Fatal(err)
	}
	setRequireAuth(t, s, false)
	if network.serves("app", ProviderPortal) {
		t.Fatal("the off-visit opened a route without a store")
	}
	if !strings.Contains(log(), "turned off, but the host cannot list the flats to open their Portal routes again") {
		t.Fatalf("the host log = %s", log())
	}
	if _, err := db.Exec(`ALTER TABLE flats_gone RENAME TO flats`); err != nil {
		t.Fatal(err)
	}

	// One flat that cannot be read.
	var original int64
	if err := db.QueryRow(`SELECT created_at FROM flats WHERE slug='app'`).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE flats SET created_at='unreadable' WHERE slug='app'`); err != nil {
		t.Fatal(err)
	}
	unlock := s.lock("app")
	s.requireAuthReopen(ctx, "app")
	unlock()
	if network.serves("app", ProviderPortal) {
		t.Fatal("a flat that cannot be read was opened")
	}
	if events := eventTexts(t, s, "app"); !hasEvent(events, "error", "turned off, but the host cannot read app to open its Portal route again") {
		t.Fatalf("events = %q", events)
	}
	if _, err := db.Exec(`UPDATE flats SET created_at=? WHERE slug='app'`, original); err != nil {
		t.Fatal(err)
	}
	unlock = s.lock("app")
	s.requireAuthReopen(ctx, "app")
	unlock()
	if !network.serves("app", ProviderPortal) {
		t.Fatal("the next visit did not open the route")
	}
}

// Rule 16: a flat with no registered Portal route is not read. The route check comes before the read, so a flat that the
// host cannot read keeps its epoch and its routes when it has nothing on Portal.
func TestRequireAuthDoesNotReadAFlatWithoutAPortalRoute(t *testing.T) {
	s, network, dir, _ := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "other", store.ProviderFunnel)
	publicLifecycleFlat(t, s, "both", store.ProviderFunnel, store.ProviderPortal)
	epoch := epochOf(s, "other")
	eventsBefore := eventTexts(t, s, "other")
	db := rawDB(t, dir)
	// The list is newest first: "both" is visited before "other", and its hook breaks the row of "other" before the visit
	// reaches it. A visit that read the flat before it checked the route would fail that read.
	if _, err := db.Exec(`UPDATE flats SET updated_at=updated_at+100 WHERE slug='both'`); err != nil {
		t.Fatal(err)
	}
	network.onHas = func(host string) {
		if host == "both" {
			if _, err := db.Exec(`UPDATE flats SET created_at='unreadable' WHERE slug='other'`); err != nil {
				t.Error(err)
			}
		}
	}
	setRequireAuth(t, s, true)
	network.onHas = nil
	if epoch == nil || epoch.ctx.Err() != nil || epochOf(s, "other") != epoch || !network.serves("other", ProviderFunnel) {
		t.Fatal("the host read a flat without a Portal route, and its epoch or route changed")
	}
	if events := eventTexts(t, s, "other"); !slices.Equal(events, eventsBefore) {
		t.Fatalf("events of other = %q, before %q", events, eventsBefore)
	}
	if network.serves("both", ProviderPortal) {
		t.Fatal("the Portal route of both stayed")
	}
}

func TestOnlyPortalAuth(t *testing.T) {
	refusal := &PortalAuthError{Detail: "x"}
	stuck := &PortalAuthError{Detail: "x", StopErr: errors.New("requests still running")}
	other := errors.New("funnel is down")
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"nil":                   {nil, true},
		"a refusal":             {refusal, true},
		"two refusals":          {errors.Join(refusal, refusal), true},
		"a refusal and another": {errors.Join(refusal, other), false},
		"another":               {other, false},
		"an unconfirmed stop":   {stuck, false},
		"nested":                {errors.Join(errors.Join(refusal), refusal), true},
	} {
		if got := onlyPortalAuth(tc.err); got != tc.want {
			t.Errorf("%s: onlyPortalAuth = %t, want %t", name, got, tc.want)
		}
	}
}
