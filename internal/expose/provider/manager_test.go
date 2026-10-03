package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
)

func TestHistoricalStateDoesNotGrant(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tsnet", "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "portal"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "portal", "notes.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Permitted) != 0 || len(f.Migration.GrantsFromState) != 0 {
		t.Fatalf("historical directories granted %+v", f)
	}
	if !f.Migration.HistoricalTSNet || !f.Migration.HistoricalPortal {
		t.Fatalf("migration = %+v", f.Migration)
	}
	if _, err := os.Stat(filepath.Join(dir, "tsnet", "notes")); err != nil {
		t.Fatal(err)
	}
	again, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.Migration.AppliedAt != f.Migration.AppliedAt || len(again.Permitted) != 0 {
		t.Fatalf("reload changed the migration: %+v", again)
	}
}

func TestExposurePolicyTracksPermissionAndRuntimeDisable(t *testing.T) {
	m, ln := managerWith(t, File{Version: 1, Permitted: []ID{Portal, Funnel}})
	defer ln.Close()
	first, err := m.ExposurePolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f := m.File()
	f.Permitted[0], f.Permitted[1] = f.Permitted[1], f.Permitted[0]
	if err := Save(m.dir, f); err != nil {
		t.Fatal(err)
	}
	second, err := m.ExposurePolicy(context.Background())
	if err != nil || first != second {
		t.Fatalf("order changed policy: %v", err)
	}
	disabled, err := New(m.dir, Options{Local: ln, Tailscale: m.ts})
	if err != nil {
		t.Fatal(err)
	}
	token, err := disabled.ExposurePolicy(context.Background())
	if err != nil || token == first {
		t.Fatal("runtime-disabled Portal did not change policy")
	}
	f.Permitted = []ID{Portal}
	if err := Save(m.dir, f); err != nil {
		t.Fatal(err)
	}
	third, err := m.ExposurePolicy(context.Background())
	if err != nil || third == first {
		t.Fatal("persisted host permission change did not change policy")
	}
	copy := m.File()
	copy.Permitted[0] = Funnel
	if !m.File().Allows(Portal) {
		t.Fatal("File exposes mutable manager permission storage")
	}
}

func TestExposureObserverAndFreshPublicPrivateRoute(t *testing.T) {
	m, ln := managerWith(t, File{Version: 1, Permitted: []ID{Portal}})
	m.portal.(*fakePortal).Public = local.NewPublic(ln)
	defer ln.Close()
	defer m.Close()
	res, err := m.ServeExposure(context.Background(), ExposureRequest{Slug: "fresh", Visibility: "public",
		Audience: AudienceCurrent, Handler: text("published"), Permitted: []ID{Portal}})
	if err != nil {
		t.Fatal(err)
	}
	local := endpointURL(res, Local)
	if get(t, local) != "published" {
		t.Fatal("fresh Public manager has no independent Private route")
	}
	status, err := m.ExposureStatus(context.Background(), "fresh")
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range status.Endpoints {
		if !ep.Configured || !ep.Permitted || ep.Audience != AudienceCurrent || ep.Host != "fresh" {
			t.Fatalf("incomplete endpoint DTO: %+v", ep)
		}
	}
	before, _ := m.ExposurePolicy(context.Background())
	if _, err := m.StopPublicRoutes(context.Background(), "fresh"); err != nil {
		t.Fatal(err)
	}
	after, _ := m.ExposurePolicy(context.Background())
	if before != after {
		t.Fatal("readiness changed desired permission policy")
	}
	if get(t, local) != "published" {
		t.Fatal("stop removed fresh Private route")
	}
}

func TestFunnelNameIsNotAnAlias(t *testing.T) {
	if _, err := ParseID("funnel"); err == nil {
		t.Fatal("funnel was accepted as a provider id")
	}
	f, err := File{Version: 1, Permitted: []ID{}, Migration: Migration{GrantsFromState: []ID{}}}.Grant(ID("funnel"))
	if err == nil {
		t.Fatalf("grant stored %+v", f)
	}
}

func TestGrantPersistsWithoutServing(t *testing.T) {
	dir := t.TempDir()
	f, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	f, err = f.Grant(Funnel)
	if err != nil {
		t.Fatal(err)
	}
	f.PrivateBackend = "local"
	if err := Save(dir, f); err != nil {
		t.Fatal(err)
	}
	ln, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	m, err := New(dir, Options{Local: ln})
	if err != nil {
		t.Fatal(err)
	}
	if !m.File().Allows(Funnel) || m.File().Allows(Portal) {
		t.Fatalf("file = %+v", m.File())
	}
	_, err = m.ServeExposure(context.Background(), ExposureRequest{
		Slug: "notes", Host: "notes", Visibility: "public", Audience: AudienceCurrent,
		Handler:   http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		Permitted: []ID{Funnel},
	})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("missing backend: %v", err)
	}
}

func TestDraftAndPrivateDoNotOpenPublicRoutes(t *testing.T) {
	m, ln := managerWith(t, File{Version: 1, Permitted: []ID{}})
	defer ln.Close()
	tail := m.ts.(*fakeTail)
	pub := m.portal.(*fakePortal)
	res, err := m.ServeExposure(context.Background(), ExposureRequest{
		Slug: "notes", Host: "notes", Visibility: "private", Audience: AudienceDraft,
		Handler: text("draft"), Ephemeral: true, Permitted: []ID{Tailscale, Funnel, Portal},
	})
	if !errors.Is(err, ErrProviderNotPermitted) {
		t.Fatalf("draft public permit: %v", err)
	}
	if tail.funnel != 0 || pub.serve != 0 || tail.serve != 0 {
		t.Fatalf("unauthorized backends used: tailscale %d funnel %d portal %d", tail.serve, tail.funnel, pub.serve)
	}
	body := get(t, endpointURL(res, Local))
	if body != "draft" {
		t.Fatalf("local body = %q", body)
	}
}

func TestNoFallbackWhenFunnelFails(t *testing.T) {
	dir := t.TempDir()
	f := File{Version: 1, Permitted: []ID{Funnel}, Migration: Migration{GrantsFromState: []ID{}}}
	if err := Save(dir, f); err != nil {
		t.Fatal(err)
	}
	ln, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	pubLn, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pubLn.Close() })
	tail := &fakeTail{funnelErr: errors.New("funnel refused")}
	pub := &fakePortal{Public: local.NewPublic(pubLn)}
	m, err := New(dir, Options{Local: ln, Tailscale: tail, Portal: pub})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.ServeExposure(context.Background(), ExposureRequest{
		Slug: "notes", Host: "notes", Visibility: "public", Audience: AudienceCurrent,
		Handler: text("public"), Permitted: []ID{Funnel},
	})
	if err == nil || tail.funnel != 1 || pub.serve != 0 {
		t.Fatalf("err=%v funnel=%d portal=%d", err, tail.funnel, pub.serve)
	}
}

func TestPublicStopLeavesPrivateAndReportsUnconfirmed(t *testing.T) {
	dir := t.TempDir()
	f := File{Version: 1, Permitted: []ID{Portal, Funnel}, Migration: Migration{GrantsFromState: []ID{}}}
	if err := Save(dir, f); err != nil {
		t.Fatal(err)
	}
	ln, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	pubLn, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pubLn.Close() })
	tail := &fakeTail{stopErr: errors.New("funnel listener still open"), funnelErr: nil}
	pub := &fakePortal{Public: local.NewPublic(pubLn)}
	m, err := New(dir, Options{Local: ln, Tailscale: tail, Portal: pub})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	priv, err := m.ServeExposure(ctx, ExposureRequest{
		Slug: "notes", Host: "notes", Visibility: "private", Audience: AudienceCurrent,
		Handler: text("private"), Permitted: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	pubRes, err := m.ServeExposure(ctx, ExposureRequest{
		Slug: "notes", Host: "notes", Visibility: "public", Audience: AudienceCurrent,
		Handler: text("portal"), Permitted: []ID{Portal, Funnel},
	})
	if err != nil || tail.funnel != 1 || pub.serve != 1 {
		t.Fatalf("public err=%v funnel=%d portal=%d endpoints=%+v", err, tail.funnel, pub.serve, pubRes.Endpoints)
	}
	if got := get(t, endpointURL(pubRes, Portal)); got != "portal" {
		t.Fatalf("portal body = %q", got)
	}
	stop, err := m.StopPublicRoutes(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(stop.Unconfirmed) != 1 || stop.Unconfirmed[0] != Funnel {
		t.Fatalf("stop = %+v", stop)
	}
	if len(stop.Stopped) != 1 || stop.Stopped[0] != Portal {
		t.Fatalf("portal was not confirmed down: %+v", stop)
	}
	gone, err := http.Get(endpointURL(pubRes, Portal))
	if err != nil {
		t.Fatal(err)
	}
	gone.Body.Close()
	if gone.StatusCode == http.StatusOK {
		t.Fatal("portal route still served")
	}
	if got := get(t, endpointURL(priv, Local)); got != "private" {
		t.Fatalf("private body after public teardown = %q", got)
	}
	m2, err := New(dir, Options{Local: ln, Tailscale: tail, Portal: pub})
	if err != nil {
		t.Fatal(err)
	}
	if !m2.File().Allows(Portal) {
		t.Fatal("grant did not survive a new manager")
	}
	if _, ok := m2.take("notes", Portal, false); ok {
		t.Fatal("a new manager restored a public route")
	}
}

func managerWith(t *testing.T, f File) (*Manager, *local.Net) {
	t.Helper()
	dir := t.TempDir()
	if f.Migration.GrantsFromState == nil {
		f.Migration.GrantsFromState = []ID{}
	}
	if err := Save(dir, f); err != nil {
		t.Fatal(err)
	}
	ln, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, Options{Local: ln, Tailscale: &fakeTail{}, Portal: &fakePortal{}})
	if err != nil {
		t.Fatal(err)
	}
	return m, ln
}

func text(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Tailscale-User-Login") != "" {
			http.Error(w, "identity leaked", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, body)
	})
}

func endpointURL(res ExposureResult, id ID) string {
	for _, ep := range res.Endpoints {
		if ep.Provider == id {
			return ep.URL
		}
	}
	return ""
}

func get(t *testing.T, url string) string {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("%s: %d %s", url, res.StatusCode, b)
	}
	return string(b)
}

type fakeTail struct {
	serve, funnel             int
	stop, funnelStops, closes int
	serveErr                  error
	funnelErr                 error
	stopErr                   error
	tailStopErr               error
	tailStopErrByHost         map[string]error
	stopHosts                 []string
	stopped                   bool
}

func (f *fakeTail) Serve(context.Context, string, http.Handler, bool) (string, error) {
	f.serve++
	if f.serveErr != nil {
		return "", f.serveErr
	}
	return "https://notes.example.ts.net", nil
}
func (f *fakeTail) Stop(host string) error {
	f.stop++
	f.stopHosts = append(f.stopHosts, host)
	if err := f.tailStopErrByHost[host]; err != nil {
		return err
	}
	return f.tailStopErr
}
func (f *fakeTail) URL(string) string { return "https://notes.example.ts.net" }
func (f *fakeTail) Status() core.NetStatus {
	return core.NetStatus{Kind: "tailscale", Enabled: true, Hosts: []core.HostInfo{{
		Host: "notes", URL: "https://notes.example.ts.net", State: "ready",
	}}}
}
func (f *fakeTail) Close() error { f.closes++; return nil }
func (f *fakeTail) ServeFunnel(context.Context, string, http.Handler) (string, error) {
	f.funnel++
	if f.funnelErr != nil {
		return "", f.funnelErr
	}
	f.stopped = false
	return "https://notes.example.ts.net", nil
}
func (f *fakeTail) StopFunnel(string) error {
	f.funnelStops++
	if f.stopErr != nil {
		return f.stopErr
	}
	f.stopped = true
	return nil
}
func (f *fakeTail) FunnelState(string) ExposureEndpoint {
	if f.stopped {
		return ExposureEndpoint{Provider: Funnel, State: stateUnavailable, Detail: "closed"}
	}
	return ExposureEndpoint{Provider: Funnel, URL: "https://notes.example.ts.net", State: stateReady}
}

func TestStopSlugRetiresFunnelOnlyNodeAndRetainsFailures(t *testing.T) {
	m, ln := managerWith(t, File{Version: 1, Permitted: []ID{Funnel}})
	defer ln.Close()
	tail := m.ts.(*fakeTail)
	ctx := context.Background()
	for _, slug := range []string{"remove-me", "sibling"} {
		if _, err := m.ServeExposure(ctx, ExposureRequest{
			Slug: slug, Host: slug, Visibility: "public", Audience: AudienceCurrent,
			Handler: text(slug), Permitted: []ID{Funnel},
		}); err != nil {
			t.Fatal(err)
		}
	}

	tail.tailStopErr = errors.New("logout unavailable")
	if err := m.StopSlug(ctx, "remove-me"); err == nil {
		t.Fatal("StopSlug accepted an unconfirmed Funnel-only node retirement")
	}
	if tracked, err := m.HasProviderRoute(ctx, "remove-me", Funnel); err != nil || !tracked {
		t.Fatalf("failed Funnel-only retirement was forgotten: tracked=%v err=%v", tracked, err)
	}
	if tail.funnelStops != 1 || tail.stop != 1 {
		t.Fatalf("first teardown calls: StopFunnel=%d Stop=%d", tail.funnelStops, tail.stop)
	}

	tail.tailStopErr = nil
	if err := m.StopSlug(ctx, "remove-me"); err != nil {
		t.Fatal(err)
	}
	if tracked, err := m.HasProviderRoute(ctx, "remove-me", Funnel); err != nil || tracked {
		t.Fatalf("confirmed Funnel-only retirement stayed registered: tracked=%v err=%v", tracked, err)
	}
	if tracked, err := m.HasProviderRoute(ctx, "sibling", Funnel); err != nil || !tracked {
		t.Fatalf("sibling Funnel route was removed: tracked=%v err=%v", tracked, err)
	}
	if tail.funnelStops != 2 || tail.stop != 2 {
		t.Fatalf("retry calls: StopFunnel=%d Stop=%d", tail.funnelStops, tail.stop)
	}
}

func TestStopSlugKeepsLocalRouteUntilNodeRetirementIsConfirmed(t *testing.T) {
	m, ln := managerWith(t, File{Version: 1, Permitted: []ID{Funnel}})
	defer ln.Close()
	tail := m.ts.(*fakeTail)
	ctx := context.Background()
	res, err := m.ServeExposure(ctx, ExposureRequest{
		Slug: "failure-atomic", Host: "failure-atomic", Visibility: "public", Audience: AudienceCurrent,
		Handler: text("still-private"), Permitted: []ID{Funnel},
	})
	if err != nil {
		t.Fatal(err)
	}
	localURL := endpointURL(res, Local)
	tail.tailStopErr = errors.New("logout unavailable")
	if err := m.StopSlug(ctx, "failure-atomic"); err == nil {
		t.Fatal("StopSlug accepted an unconfirmed node retirement")
	}
	if got := get(t, localURL); got != "still-private" {
		t.Fatalf("failed retirement removed Local route: %q", got)
	}
	for _, id := range []ID{Local, Funnel} {
		if tracked, err := m.HasProviderRoute(ctx, "failure-atomic", id); err != nil || !tracked {
			t.Fatalf("failed retirement forgot %s: tracked=%v err=%v", id, tracked, err)
		}
	}

	tail.tailStopErr = nil
	if err := m.StopSlug(ctx, "failure-atomic"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []ID{Local, Funnel} {
		if tracked, err := m.HasProviderRoute(ctx, "failure-atomic", id); err != nil || tracked {
			t.Fatalf("confirmed retirement retained %s: tracked=%v err=%v", id, tracked, err)
		}
	}
	resHTTP, err := http.Get(localURL)
	if err != nil {
		t.Fatal(err)
	}
	resHTTP.Body.Close()
	if resHTTP.StatusCode != http.StatusNotFound {
		t.Fatalf("confirmed retirement left Local route: %d", resHTTP.StatusCode)
	}
}

func TestStopSlugChecksAllNodesBeforeStoppingAnyLocalRoute(t *testing.T) {
	m, ln := managerWith(t, File{Version: 1})
	defer ln.Close()
	tail := m.ts.(*fakeTail)
	ctx := context.Background()
	urls := map[string]string{}
	for _, tc := range []struct {
		host     string
		audience Audience
	}{
		{host: "multi-a", audience: AudienceCurrent},
		{host: "multi-b", audience: AudienceDraft},
	} {
		res, err := m.ServeExposure(ctx, ExposureRequest{
			Slug: "multi", Host: tc.host, Visibility: "private", Audience: tc.audience,
			Handler: text(tc.host), Ephemeral: tc.audience == AudienceDraft,
		})
		if err != nil {
			t.Fatal(err)
		}
		urls[tc.host] = endpointURL(res, Local)
	}
	tail.tailStopErrByHost = map[string]error{"multi-b": errors.New("second node logout unavailable")}
	if err := m.StopSlug(ctx, "multi"); err == nil {
		t.Fatal("StopSlug accepted one failed node candidate")
	}
	for host, routeURL := range urls {
		if got := get(t, routeURL); got != host {
			t.Fatalf("node phase failure removed Local %s: %q", host, got)
		}
	}
	wantHosts := []string{"multi", "multi-a", "multi-b"}
	if !slices.Equal(tail.stopHosts, wantHosts) {
		t.Fatalf("node retirement order = %v, want %v", tail.stopHosts, wantHosts)
	}

	tail.tailStopErrByHost = nil
	tail.stopHosts = nil
	if err := m.StopSlug(ctx, "multi"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tail.stopHosts, wantHosts) {
		t.Fatalf("retry node retirement order = %v, want %v", tail.stopHosts, wantHosts)
	}
}

func TestStopSlugClassifiesUnconfirmedFunnelListener(t *testing.T) {
	m, ln := managerWith(t, File{Version: 1, Permitted: []ID{Funnel}})
	defer ln.Close()
	tail := m.ts.(*fakeTail)
	ctx := context.Background()
	if _, err := m.ServeExposure(ctx, ExposureRequest{
		Slug: "still-public", Host: "still-public", Visibility: "public", Audience: AudienceCurrent,
		Handler: text("still-public"), Permitted: []ID{Funnel},
	}); err != nil {
		t.Fatal(err)
	}
	tail.stopErr = errors.New("Funnel listener stop not confirmed")
	err := m.StopSlug(ctx, "still-public")
	if !errors.Is(err, core.ErrPublicStopUnconfirmed) {
		t.Fatalf("unconfirmed Funnel listener lost its safety category: %v", err)
	}
	if tracked, routeErr := m.HasProviderRoute(ctx, "still-public", Funnel); routeErr != nil || !tracked {
		t.Fatalf("unconfirmed Funnel listener was forgotten: tracked=%v err=%v", tracked, routeErr)
	}
}

type fakePortal struct {
	*local.Public
	serve int
}

func (f *fakePortal) Serve(ctx context.Context, slug string, h http.Handler, hidden bool) (string, error) {
	f.serve++
	if f.Public == nil {
		return "", errors.New("portal backend missing")
	}
	return f.Public.Serve(ctx, slug, h, hidden)
}

func TestPreviewCleanupKeepsOtherHostsAndCurrent(t *testing.T) {
	m, ln := managerWith(t, File{Version: 1})
	defer ln.Close()
	defer m.Close()
	ctx := context.Background()
	urls := map[string]string{}
	for _, host := range []string{"notes", "notes-draft-one", "notes-draft-two"} {
		audience := AudienceDraft
		if host == "notes" {
			audience = AudienceCurrent
		}
		res, err := m.ServeExposure(ctx, ExposureRequest{Slug: "notes", Host: host, Audience: audience,
			Visibility: "private", Handler: text(host), Ephemeral: audience == AudienceDraft})
		if err != nil {
			t.Fatal(err)
		}
		urls[host] = endpointURL(res, Local)
	}
	if err := m.StopExposure(ctx, "notes-draft-one"); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"notes", "notes-draft-two"} {
		if got := get(t, urls[host]); got != host {
			t.Fatalf("%s: %s", host, got)
		}
	}
	resp, err := http.Get(urls["notes-draft-one"])
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stopped preview: %d", resp.StatusCode)
	}
	status, err := m.ExposureStatus(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, ep := range status.Endpoints {
		if ep.Provider == Local {
			seen[ep.Host] = ep.Ready
		}
	}
	if seen["notes-draft-one"] || !seen["notes"] || !seen["notes-draft-two"] || len(seen) != 2 {
		t.Fatalf("host-specific observed routes: %+v", status)
	}
}

func TestExposureStatusRecomputesCurrentPermission(t *testing.T) {
	m, ln := managerWith(t, File{Version: 1, Permitted: []ID{Portal}})
	defer ln.Close()
	defer m.Close()
	m.portal.(*fakePortal).Public = local.NewPublic(ln)
	permitted := true
	m.permission = func(context.Context, string, ID) (bool, error) { return permitted, nil }
	_, err := m.ServeExposure(t.Context(), ExposureRequest{Slug: "status", Visibility: "public", Audience: AudienceCurrent, Handler: text("current"), Permitted: []ID{Portal}})
	if err != nil {
		t.Fatal(err)
	}
	permitted = false
	observed, err := m.ExposureStatus(t.Context(), "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range observed.Endpoints {
		if ep.Provider == Portal && (ep.Permitted || !ep.Ready) {
			t.Fatalf("policy/readiness conflated %+v", ep)
		}
	}
	permitted = true
	if _, err := m.StopPublicRoutes(t.Context(), "status"); err != nil {
		t.Fatal(err)
	}
	observed, err = m.ExposureStatus(t.Context(), "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range observed.Endpoints {
		if ep.Provider == Portal {
			t.Fatalf("stopped route was retained: %+v", ep)
		}
	}
}

func TestExposureStatusPreservesNeverRegisteredFailure(t *testing.T) {
	for _, tc := range []struct {
		name, state, detail string
		configure           func(*Manager)
		wantErr             error
	}{
		{"unconfigured", stateUnavailable, "tailscale is permitted but not configured", func(m *Manager) { m.ts = nil }, ErrNotConfigured},
		{"failed-open", stateError, "control refused", func(m *Manager) { m.ts.(*fakeTail).serveErr = errors.New("control refused") }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, ln := managerWith(t, File{Version: 1, Permitted: []ID{Tailscale}})
			defer ln.Close()
			defer m.Close()
			tc.configure(m)
			_, serveErr := m.ServeExposure(t.Context(), ExposureRequest{
				Slug: "refused", Visibility: "private", Audience: AudienceCurrent,
				Handler: text("private"), Permitted: []ID{Tailscale},
			})
			if serveErr == nil || (tc.wantErr != nil && !errors.Is(serveErr, tc.wantErr)) {
				t.Fatalf("ServeExposure error = %v, want %v", serveErr, tc.wantErr)
			}
			status, err := m.ExposureStatus(t.Context(), "refused")
			if err != nil {
				t.Fatal(err)
			}
			for _, ep := range status.Endpoints {
				if ep.Provider != Tailscale {
					continue
				}
				if ep.State != tc.state || ep.Detail != tc.detail {
					t.Fatalf("never-registered failure was rewritten as a stopped route: %+v", ep)
				}
				return
			}
			t.Fatal("missing refused tailscale endpoint")
		})
	}
}

func TestExposureStatusMarksFormerRegistrationStopped(t *testing.T) {
	m, ln := managerWith(t, File{Version: 1, Permitted: []ID{Tailscale}})
	defer ln.Close()
	defer m.Close()
	if _, err := m.ServeExposure(t.Context(), ExposureRequest{
		Slug: "former", Visibility: "private", Audience: AudienceCurrent,
		Handler: text("private"), Permitted: []ID{Tailscale},
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.take("former", Tailscale, true); !ok {
		t.Fatal("missing registered tailscale route")
	}
	status, err := m.ExposureStatus(t.Context(), "former")
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range status.Endpoints {
		if ep.Provider == Tailscale {
			if ep.State != stateStopped || ep.Detail != "route stopped" {
				t.Fatalf("former registration status = %+v", ep)
			}
			return
		}
	}
	t.Fatal("missing former tailscale endpoint")
}

func TestManagerCloseForgetsRoutesWithoutDestructiveBackendStops(t *testing.T) {
	m, ln := managerWith(t, File{Version: 1, Permitted: []ID{Tailscale, Funnel}})
	defer ln.Close()
	tail := m.ts.(*fakeTail)
	ctx := context.Background()
	if _, err := m.ServeExposure(ctx, ExposureRequest{Slug: "notes", Visibility: "private", Audience: AudienceCurrent,
		Handler: text("private"), Permitted: []ID{Tailscale}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ServeExposure(ctx, ExposureRequest{Slug: "notes", Visibility: "public", Audience: AudienceCurrent,
		Handler: text("public"), Permitted: []ID{Tailscale, Funnel}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if tail.stop != 0 || tail.funnelStops != 0 || tail.closes != 0 {
		t.Fatalf("manager performed destructive backend cleanup: stop=%d funnel=%d close=%d", tail.stop, tail.funnelStops, tail.closes)
	}
	status, err := m.ExposureStatus(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Endpoints) != 0 || len(m.routes) != 0 {
		t.Fatalf("manager retained closed registrations: endpoints=%+v routes=%+v", status.Endpoints, m.routes)
	}
}
