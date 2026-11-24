package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/store"
)

// The checks of the visit of rules 16 to 18 that the first tests of the setting leave open, found by changing each check
// of requireauth.go in turn: a change that these tests do not fail is a check that no test needs.

// Rule 10: PortalAuthRefusal tells a Portal endpoint that the setting refuses from one that failed for another cause. A
// route whose stop was not confirmed is an error, not a refusal, and so is a refusal of another provider.
func TestPortalAuthRefusal(t *testing.T) {
	detail := requireAuthDetailOf("app")
	for name, tc := range map[string]struct {
		ep   ExposureEndpoint
		want bool
	}{
		"a refused Portal route":       {ExposureEndpoint{Provider: ProviderPortal, State: "unavailable", Detail: detail}, true},
		"a stop that is not confirmed": {ExposureEndpoint{Provider: ProviderPortal, State: "error", Detail: detail + "; the stop of the route is not confirmed: relay refused"}, false},
		"another cause":                {ExposureEndpoint{Provider: ProviderPortal, State: "unavailable", Detail: "host permission absent"}, false},
		"another provider":             {ExposureEndpoint{Provider: ProviderFunnel, State: "unavailable", Detail: detail}, false},
		"a route that serves":          {ExposureEndpoint{Provider: ProviderPortal, State: "ready"}, false},
	} {
		if got := PortalAuthRefusal(tc.ep); got != tc.want {
			t.Errorf("%s: PortalAuthRefusal = %t, want %t", name, got, tc.want)
		}
	}
}

// Rule 10: the list of the status holds a flat only when Portal serves it now, so a Public flat without a live version is
// not in it.
func TestPortalRefusedListSkipsAFlatWithoutALiveVersion(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "live", store.ProviderPortal)
	if err := s.st.CreateFlat(ctx, store.Flat{Slug: "draft", Name: "draft", Visibility: store.Public, CreatedAt: apNow, UpdatedAt: apNow}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.st.SetProviderPermission(ctx, "draft", store.ProviderPortal, true, store.Audit{}); err != nil {
		t.Fatal(err)
	}
	setRequireAuth(t, s, true)
	got, err := s.PortalRefused(ctx, true)
	if err != nil || !slices.Equal(got, []string{"live"}) {
		t.Fatalf("the list = %q, %v, want only the flat with a live version", got, err)
	}
}

// Rules 16 and 18: each step of a visit reads the setting again under the lock of the flat and does nothing when it is off.
// The test turns the setting off when the visit asks the first flat for its Portal route, so the first flat is the last one
// that the visit may touch: the other flat keeps its epoch, its connections and its Portal route, and the first flat, whose
// re-serve then opens without a refusal, keeps its Portal route and gets no event of a failure.
func TestRequireAuthVisitDoesNothingWhenTheSettingIsOffAgain(t *testing.T) {
	s, network, _, _ := newRequireAuthService(t)
	for _, slug := range []string{"aaa", "bbb"} {
		publicLifecycleFlat(t, s, slug, store.ProviderPortal, store.ProviderFunnel)
	}
	epochs := map[string]*routeEpoch{"aaa": epochOf(s, "aaa"), "bbb": epochOf(s, "bbb")}
	first := ""
	network.onHas = func(host string) {
		if first == "" {
			first = host
			s.settings.requireAuth.Store(false)
		}
	}
	setRequireAuth(t, s, true)
	if first == "" {
		t.Fatal("the visit asked for no Portal route")
	}
	for _, slug := range []string{"aaa", "bbb"} {
		if !network.serves(slug, ProviderPortal) || !network.serves(slug, ProviderFunnel) {
			t.Fatalf("%s lost a route although the setting is off again", slug)
		}
		if events := eventTexts(t, s, slug); hasEvent(events, "error", "portal.require_auth") || hasEvent(events, "warn", "portal.require_auth") {
			t.Fatalf("%s has an event of a refusal that the setting no longer asks for: %q", slug, events)
		}
	}
	other := "aaa"
	if first == "aaa" {
		other = "bbb"
	}
	if epochOf(s, other) != epochs[other] || epochs[other].ctx.Err() != nil {
		t.Fatalf("the visit ended the connections of %s although the setting was off before it came to the flat", other)
	}
}

// Rule 18, the second path: a flat that the visit comes to after the setting turned off again keeps its Portal route.
func TestRequireAuthListFailureDoesNothingWhenTheSettingIsOffAgain(t *testing.T) {
	s, network, dir, _ := newRequireAuthService(t)
	for _, slug := range []string{"aaa", "bbb"} {
		publicLifecycleFlat(t, s, slug, store.ProviderPortal)
	}
	if _, err := rawDB(t, dir).Exec(`ALTER TABLE flats RENAME TO flats_gone`); err != nil {
		t.Fatal(err)
	}
	first := ""
	network.onHas = func(host string) {
		if first == "" {
			first = host
			s.settings.requireAuth.Store(false)
		}
	}
	setRequireAuth(t, s, true)
	if first == "" {
		t.Fatal("the visit asked for no Portal route")
	}
	other := "aaa"
	if first == "aaa" {
		other = "bbb"
	}
	if !network.serves(other, ProviderPortal) {
		t.Fatalf("the visit stopped the Portal route of %s after the setting was off again", other)
	}
}

// Rule 16: only a Public flat is refused. A Private flat that holds a Portal route, which a failed change of visibility
// can leave, is not the business of the setting.
func TestRequireAuthVisitLeavesAPrivateFlat(t *testing.T) {
	ctx := t.Context()
	s, network, _, _ := newRequireAuthService(t)
	lifecycleSave(t, s, "priv", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "priv"))
	if _, err := network.ServeExposure(ctx, ExposureRequest{Slug: "priv", Host: "priv", Visibility: "public", Audience: AudienceCurrent,
		Handler: http.NotFoundHandler(), Permitted: []ProviderID{ProviderPortal}}); err != nil {
		t.Fatal(err)
	}
	if !network.serves("priv", ProviderPortal) {
		t.Fatal("the test did not register a Portal route")
	}
	setRequireAuth(t, s, true)
	if !network.serves("priv", ProviderPortal) {
		t.Fatal("the visit stopped the Portal route of a Private flat")
	}
	if events := eventTexts(t, s, "priv"); hasEvent(events, "error", "portal.require_auth") || hasEvent(events, "warn", "portal.require_auth") {
		t.Fatalf("events of a Private flat = %q", events)
	}
}

// Rule 16, step 3: a flat without a runtime that holds a Portal route, such as one that a failed rename left, loses the
// route directly, and the flat event and the host log say so.
func TestRequireAuthStopsAFlatWithoutARuntime(t *testing.T) {
	s, network, _, log := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "app", store.ProviderPortal, store.ProviderFunnel)
	s.state("app").cur.Store(nil)
	epoch := epochOf(s, "app")
	setRequireAuth(t, s, true)
	if epoch == nil || epoch.ctx.Err() != nil || epochOf(s, "app") != epoch {
		t.Fatal("the visit ended the public connections of a flat without a runtime; rule 16 stops only its Portal route")
	}
	if network.serves("app", ProviderPortal) {
		t.Fatal("the Portal route of a flat without a runtime stayed")
	}
	if !network.serves("app", ProviderFunnel) {
		t.Fatal("the visit touched the other public route of a flat without a runtime")
	}
	if events := eventTexts(t, s, "app"); !hasEvent(events, "warn", "portal.require_auth is on and app has no sign-in") {
		t.Fatalf("events = %q", events)
	}
	if !strings.Contains(log(), "portal.require_auth: app: ") {
		t.Fatalf("the host log has no line for app: %s", log())
	}
}

// leavingNet registers the Portal route again after it refused the open, as a provider manager with a fault would.
type leavingNet struct{ *requireAuthNet }

func (n *leavingNet) ServeExposure(ctx context.Context, req ExposureRequest) (ExposureResult, error) {
	res, err := n.requireAuthNet.ServeExposure(ctx, req)
	if req.SignIn.Refused() {
		key := lifecycleRouteKey{host: req.Host, provider: ProviderPortal}
		n.lifecycleRouteNet.mu.Lock()
		n.routes[key] = req.Handler
		n.owners[key] = req.Slug
		n.lifecycleRouteNet.mu.Unlock()
	}
	return res, err
}

// Rule 16: the visit fails closed. A Portal route that is still registered after the open refused it is stopped.
func TestRequireAuthStopsARouteThatTheRefusalLeftRegistered(t *testing.T) {
	s, network, _, _ := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "app", store.ProviderPortal, store.ProviderFunnel)
	s.cfg.Lifecycle = &leavingNet{network}
	setRequireAuth(t, s, true)
	if network.serves("app", ProviderPortal) {
		t.Fatal("a Portal route that the refusal left registered still serves")
	}
	if len(network.failed) != 1 || !strings.HasPrefix(network.failed[0], PortalAuthDetailPrefix) {
		t.Fatalf("the reasons of the stops = %q", network.failed)
	}
}

// Rule 17: the off-visit opens only a route that the exposure of the flat already used: the flat is Public, has a runtime and
// has a Portal grant. For a flat that lacks one of the three it opens nothing and writes nothing.
func TestRequireAuthOffVisitOpensOnlyWhatTheExposureUsed(t *testing.T) {
	ctx := t.Context()
	s, network, _, _ := newRequireAuthService(t)
	lifecycleSave(t, s, "priv", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "priv"))
	if err := s.SetProviderPermission(ctx, "priv", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	publicLifecycleFlat(t, s, "norun", store.ProviderPortal)
	if err := network.StopProviderRoutes(ctx, "norun", ProviderPortal); err != nil {
		t.Fatal(err)
	}
	s.state("norun").cur.Store(nil)
	publicLifecycleFlat(t, s, "nogrant", store.ProviderFunnel)
	before := map[string][]string{}
	for _, slug := range []string{"priv", "norun", "nogrant"} {
		before[slug] = eventTexts(t, s, slug)
	}
	setRequireAuth(t, s, true)
	setRequireAuth(t, s, false)
	for _, slug := range []string{"priv", "norun", "nogrant"} {
		if network.serves(slug, ProviderPortal) {
			t.Errorf("the off-visit opened a Portal route for %s", slug)
		}
		if events := eventTexts(t, s, slug); !slices.Equal(events, before[slug]) {
			t.Errorf("%s: events %q, before %q", slug, events, before[slug])
		}
	}
}

// Rule 19: the sentence that a visibility result adds repeats the refusal of the setting and nothing else. It is empty when
// Portal is unavailable for another cause, and when the setting is off, also if the endpoint still holds an old refusal.
func TestRequireAuthRefusalNoteRepeatsOnlyTheRefusal(t *testing.T) {
	ctx := t.Context()
	s, network, _, _ := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "app", store.ProviderPortal)
	set := func(state, detail string) {
		network.degraded = map[ProviderID]string{ProviderPortal: state}
		network.degradedDetail = map[ProviderID]string{ProviderPortal: detail}
	}
	// The setting is on, and Portal is unavailable for another cause.
	s.settings.requireAuth.Store(true)
	set("unavailable", "host permission absent")
	if note := s.portalRefusalNote(ctx, "app"); note != "" {
		t.Fatalf("Portal is unavailable for another cause and the note says %q", note)
	}
	// The setting is on, and the endpoint holds the refusal.
	set("unavailable", requireAuthDetailOf("app"))
	if note := s.portalRefusalNote(ctx, "app"); note != "Portal is refused: "+requireAuthDetailOf("app") {
		t.Fatalf("the note of a refusal = %q", note)
	}
	// The setting is off, and the endpoint still holds the refusal of the time before.
	s.settings.requireAuth.Store(false)
	if note := s.portalRefusalNote(ctx, "app"); note != "" {
		t.Fatalf("the setting is off and the note says %q", note)
	}
}

// Rules 9 and 14: on the legacy path without a provider manager, the next open that the service makes refuses a Portal route
// that is served and stops it. A change of the setting alone opens nothing there: only a deploy or a restart does.
func TestRequireAuthLegacyOpenStopsAServedRoute(t *testing.T) {
	ctx := t.Context()
	s, _ := newTestService(t)
	public := &memPublic{memNet{hosts: map[string]http.Handler{}}}
	s.cfg.Public = public
	lifecycleSave(t, s, "app", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "app"))
	if err := s.SetProviderPermission(ctx, "app", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	req, err := s.SetVisibility(ctx, "app", store.Public, ViaConsole, "")
	if err != nil || req.Approval == nil {
		t.Fatalf("visibility request = %+v, %v", req, err)
	}
	if _, err := s.Decide(ctx, req.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	served := func() bool {
		public.mu.Lock()
		defer public.mu.Unlock()
		_, ok := public.hosts["app"]
		return ok
	}
	reopen := func() error {
		unlock := s.lock("app")
		defer unlock()
		return s.reopenPublic(ctx, "app", false)
	}
	if !served() {
		t.Fatal("the test did not serve the flat on the legacy backend")
	}
	setRequireAuth(t, s, true)
	if !served() {
		t.Fatal("a change of the setting stopped a legacy route; the spec says it keeps serving until the next open")
	}
	var refusal *PortalAuthError
	if err := reopen(); !errors.As(err, &refusal) || refusal.StopErr != nil {
		t.Fatalf("the open of a served route = %v, want the refusal with a confirmed stop", err)
	}
	if served() {
		t.Fatal("a refused open left the route serving")
	}
	setRequireAuth(t, s, false)
	if err := reopen(); err != nil || served() {
		t.Fatalf("with the setting off, an open of a route that is not served = %v, serving %t, want nothing", err, served())
	}
}

// Rule 18, the second path: the one host log line of a failed list names at most 20 slugs and then how many more.
func TestRequireAuthFailedListLogLineNamesTwentyFlats(t *testing.T) {
	s, network, dir, log := newRequireAuthService(t)
	var slugs []string
	for i := 0; i < 22; i++ {
		slug := fmt.Sprintf("f%02d", i)
		slugs = append(slugs, slug)
		publicLifecycleFlat(t, s, slug, store.ProviderPortal)
	}
	if _, err := rawDB(t, dir).Exec(`ALTER TABLE flats RENAME TO flats_gone`); err != nil {
		t.Fatal(err)
	}
	setRequireAuth(t, s, true)
	for _, slug := range slugs {
		if network.serves(slug, ProviderPortal) {
			t.Fatalf("the Portal route of %s stayed", slug)
		}
	}
	want := "stopped the Portal route of 22 flats: " + strings.Join(slugs[:20], ", ") + ", and 2 more"
	if !strings.Contains(log(), want) {
		t.Fatalf("the host log has no line with %q:\n%s", want, log())
	}
	if strings.Contains(log(), "f20, ") || strings.Contains(log(), ", f21") {
		t.Fatal("the line names more than 20 slugs")
	}
}
