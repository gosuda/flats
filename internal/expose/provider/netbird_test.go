package provider

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
)

// fakeNetbird serves private routes as nb-<host> and public routes as
// nbx-<slug> on a loopback Net and records the lifecycle calls.
type fakeNetbird struct {
	net                                      *local.Net
	private, public                          int
	stoppedPrivate, stoppedPublic            []string
	retiredPrivate, retiredPublic            []string
	stopPublicErr, retirePublicErr, serveErr error
}

func newFakeNetbird(t *testing.T) *fakeNetbird {
	t.Helper()
	ln, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return &fakeNetbird{net: ln}
}

func (f *fakeNetbird) ServePrivate(ctx context.Context, host string, h http.Handler) (string, error) {
	if f.serveErr != nil {
		return "", f.serveErr
	}
	f.private++
	return f.net.Serve(ctx, "nb-"+host, h, false)
}
func (f *fakeNetbird) StopPrivate(host string) error {
	f.stoppedPrivate = append(f.stoppedPrivate, host)
	return f.net.Stop("nb-" + host)
}
func (f *fakeNetbird) RetirePrivate(host string) error {
	f.retiredPrivate = append(f.retiredPrivate, host)
	return f.net.Stop("nb-" + host)
}
func (f *fakeNetbird) PrivateURL(host string) string { return f.net.URL("nb-" + host) }
func (f *fakeNetbird) PrivateStatus() core.NetStatus { return f.status("nb-", "netbird") }

func (f *fakeNetbird) ServePublic(ctx context.Context, slug string, h http.Handler) (string, error) {
	if f.serveErr != nil {
		return "", f.serveErr
	}
	f.public++
	return f.net.Serve(ctx, "nbx-"+slug, h, false)
}
func (f *fakeNetbird) StopPublic(slug string) error {
	if f.stopPublicErr != nil {
		return f.stopPublicErr
	}
	f.stoppedPublic = append(f.stoppedPublic, slug)
	return f.net.Stop("nbx-" + slug)
}
func (f *fakeNetbird) RetirePublic(slug string) error {
	if f.retirePublicErr != nil {
		return f.retirePublicErr
	}
	f.retiredPublic = append(f.retiredPublic, slug)
	return f.net.Stop("nbx-" + slug)
}
func (f *fakeNetbird) PublicURL(slug string) string { return f.net.URL("nbx-" + slug) }
func (f *fakeNetbird) PublicStatus() core.NetStatus { return f.status("nbx-", "netbird-expose") }
func (f *fakeNetbird) Account() string              { return "peer-fingerprint" }
func (f *fakeNetbird) Close() error                 { return nil }

func (f *fakeNetbird) status(prefix, kind string) core.NetStatus {
	st := f.net.Status()
	st.Kind = kind
	var hosts []core.HostInfo
	for _, h := range st.Hosts {
		if len(h.Host) > len(prefix) && h.Host[:len(prefix)] == prefix {
			h.Host = h.Host[len(prefix):]
			hosts = append(hosts, h)
		}
	}
	st.Hosts = hosts
	return st
}

func netbirdManager(t *testing.T, permitted []ID, n NetbirdNet) *Manager {
	t.Helper()
	ln, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	m, err := New(t.TempDir(), Options{Local: ln, Netbird: n, Grants: &File{Permitted: permitted}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParseIDAcceptsNetbird(t *testing.T) {
	for _, id := range []ID{Netbird, NetbirdExpose} {
		if got, err := ParseID(string(id)); err != nil || got != id {
			t.Fatalf("ParseID(%s) = %q, %v", id, got, err)
		}
	}
	if core.PublicProvider(Netbird) || !core.PublicProvider(NetbirdExpose) {
		t.Fatal("PublicProvider classification is wrong")
	}
	if !core.PrivateMeshProvider(Netbird) || core.PrivateMeshProvider(NetbirdExpose) {
		t.Fatal("PrivateMeshProvider classification is wrong")
	}
}

func TestNetbirdPrivateRouteNeedsBothGates(t *testing.T) {
	n := newFakeNetbird(t)
	ctx := context.Background()
	req := ExposureRequest{Slug: "notes", Host: "notes", Visibility: "private", Audience: AudienceCurrent,
		Handler: text("private"), Permitted: []ID{Netbird}}

	// Per-flat permission without the host grant opens nothing.
	m := netbirdManager(t, nil, n)
	if _, err := m.ServeExposure(ctx, req); !errors.Is(err, ErrProviderNotPermitted) || n.private != 0 {
		t.Fatalf("no host grant: err=%v private=%d", err, n.private)
	}
	// Host grant without a backend is not configured, not substituted.
	m = netbirdManager(t, []ID{Netbird}, nil)
	if _, err := m.ServeExposure(ctx, req); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("no backend: err=%v", err)
	}
	m = netbirdManager(t, []ID{Netbird}, n)
	res, err := m.ServeExposure(ctx, req)
	if err != nil || n.private != 1 {
		t.Fatalf("serve: err=%v private=%d endpoints=%+v", err, n.private, res.Endpoints)
	}
	if got := get(t, endpointURL(res, Netbird)); got != "private" {
		t.Fatalf("netbird body = %q", got)
	}
	if has, _ := m.HasProviderRoute(ctx, "notes", Netbird); !has {
		t.Fatal("the netbird route is not registered")
	}
	// Revoking stops only the NetBird private route and keeps its port.
	if err := m.StopProviderRoutes(ctx, "notes", Netbird); err != nil || !slices.Equal(n.stoppedPrivate, []string{"notes"}) || len(n.retiredPrivate) != 0 {
		t.Fatalf("StopProviderRoutes: err=%v stopped=%v retired=%v", err, n.stoppedPrivate, n.retiredPrivate)
	}
	if has, _ := m.HasProviderRoute(ctx, "notes", Netbird); has {
		t.Fatal("the netbird route is still registered after StopProviderRoutes")
	}
}

func TestNetbirdSkipsPreviews(t *testing.T) {
	n := newFakeNetbird(t)
	m := netbirdManager(t, []ID{Netbird}, n)
	_, err := m.ServeExposure(context.Background(), ExposureRequest{Slug: "notes", Host: "p-1", Visibility: "private", Audience: AudienceDraft,
		Handler: text("draft"), Permitted: []ID{Netbird}, Ephemeral: true})
	if err != nil || n.private != 0 {
		t.Fatalf("preview: err=%v private=%d", err, n.private)
	}
}

func TestNetbirdExposeServesPublicCurrentOnly(t *testing.T) {
	n := newFakeNetbird(t)
	m := netbirdManager(t, []ID{NetbirdExpose, Netbird}, n)
	ctx := context.Background()

	// Drafts and private flats never reach the public provider.
	_, err := m.ServeExposure(ctx, ExposureRequest{Slug: "notes", Host: "notes", Visibility: "private",
		Audience: AudienceCurrent, Handler: text("private"), Permitted: []ID{NetbirdExpose}})
	if !errors.Is(err, ErrProviderNotPermitted) || n.public != 0 {
		t.Fatalf("private request: err=%v public=%d", err, n.public)
	}

	res, err := m.ServeExposure(ctx, ExposureRequest{Slug: "notes", Host: "notes", Visibility: "public",
		Audience: AudienceCurrent, Handler: text("public"), PrivateHandler: text("private"), Permitted: []ID{NetbirdExpose, Netbird}})
	if err != nil || n.public != 1 || n.private != 1 {
		t.Fatalf("public request: err=%v public=%d private=%d endpoints=%+v", err, n.public, n.private, res.Endpoints)
	}
	if got := get(t, endpointURL(res, NetbirdExpose)); got != "public" {
		t.Fatalf("expose body = %q", got)
	}
	// The flat keeps its independent private mesh route with the private handler.
	if got := get(t, endpointURL(res, Netbird)); got != "private" {
		t.Fatalf("netbird private body = %q", got)
	}

	// Going private stops the expose session and keeps the private route.
	stop, err := m.StopPublicRoutes(ctx, "notes")
	if err != nil || !slices.Equal(stop.Stopped, []ID{NetbirdExpose}) || len(stop.Unconfirmed) != 0 {
		t.Fatalf("StopPublicRoutes: %+v, %v", stop, err)
	}
	if !slices.Equal(n.stoppedPublic, []string{"notes"}) {
		t.Fatalf("stopped public = %v", n.stoppedPublic)
	}
	if has, _ := m.HasProviderRoute(ctx, "notes", Netbird); !has {
		t.Fatal("the private netbird route was stopped with the public one")
	}

	// A failed stop is reported unconfirmed and keeps the route registered.
	if _, err := m.ServeExposure(ctx, ExposureRequest{Slug: "notes", Host: "notes", Visibility: "public",
		Audience: AudienceCurrent, Handler: text("public"), PrivateHandler: text("private"), Permitted: []ID{NetbirdExpose}}); err != nil {
		t.Fatal(err)
	}
	n.stopPublicErr = errors.New("session will not end")
	stop, _ = m.StopPublicRoutes(ctx, "notes")
	if !slices.Equal(stop.Unconfirmed, []ID{NetbirdExpose}) || len(stop.Stopped) != 0 {
		t.Fatalf("failed stop: %+v", stop)
	}
}

func TestNetbirdExposeFailureIsNotReady(t *testing.T) {
	n := newFakeNetbird(t)
	n.serveErr = errors.New("management refused the expose request")
	m := netbirdManager(t, []ID{NetbirdExpose}, n)
	res, err := m.ServeExposure(context.Background(), ExposureRequest{Slug: "notes", Host: "notes", Visibility: "public",
		Audience: AudienceCurrent, Handler: text("public"), Permitted: []ID{NetbirdExpose}})
	if !errors.Is(err, core.ErrProviderNotReady) {
		t.Fatalf("err = %v", err)
	}
	for _, ep := range res.Endpoints {
		if ep.Provider == NetbirdExpose && ep.State != stateError {
			t.Fatalf("expose endpoint = %+v", ep)
		}
	}
}

func TestNetbirdStopSlugRetiresPortsAndRoutes(t *testing.T) {
	n := newFakeNetbird(t)
	m := netbirdManager(t, []ID{NetbirdExpose, Netbird}, n)
	ctx := context.Background()
	if _, err := m.ServeExposure(ctx, ExposureRequest{Slug: "notes", Host: "notes", Visibility: "public",
		Audience: AudienceCurrent, Handler: text("public"), PrivateHandler: text("private"), Permitted: []ID{NetbirdExpose, Netbird}}); err != nil {
		t.Fatal(err)
	}
	n.retirePublicErr = errors.New("expose session still alive")
	if err := m.StopSlug(ctx, "notes"); !errors.Is(err, core.ErrPublicStopUnconfirmed) {
		t.Fatalf("failed retire: %v", err)
	}
	if has, _ := m.HasProviderRoute(ctx, "notes", NetbirdExpose); !has {
		t.Fatal("a failed retirement forgot the route")
	}
	n.retirePublicErr = nil
	if err := m.StopSlug(ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(n.retiredPublic, "notes") || !slices.Contains(n.retiredPrivate, "notes") {
		t.Fatalf("retired public=%v private=%v", n.retiredPublic, n.retiredPrivate)
	}
	for _, id := range []ID{Netbird, NetbirdExpose} {
		if has, _ := m.HasProviderRoute(ctx, "notes", id); has {
			t.Fatalf("%s is still registered after StopSlug", id)
		}
	}
}

func TestNetbirdPolicyCountsGrantedBackendOnly(t *testing.T) {
	n := newFakeNetbird(t)
	ctx := context.Background()
	off, err := netbirdManager(t, nil, n).ExposurePolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bare, _ := netbirdManager(t, nil, nil).ExposurePolicy(ctx)
	if off != bare {
		t.Fatal("an attached but ungranted NetBird backend changed the policy token")
	}
	on, _ := netbirdManager(t, []ID{Netbird}, n).ExposurePolicy(ctx)
	onBare, _ := netbirdManager(t, []ID{Netbird}, nil).ExposurePolicy(ctx)
	if on == onBare {
		t.Fatal("a granted NetBird backend does not change the policy token")
	}
}

func TestDetachNetbirdRefusesWhileRoutesExist(t *testing.T) {
	n := newFakeNetbird(t)
	m := netbirdManager(t, []ID{Netbird}, n)
	if _, err := m.ServeExposure(context.Background(), ExposureRequest{Slug: "notes", Host: "notes", Visibility: "private",
		Audience: AudienceCurrent, Handler: text("private"), Permitted: []ID{Netbird}}); err != nil {
		t.Fatal(err)
	}
	if m.DetachNetbird() {
		t.Fatal("detached with a registered route")
	}
	if err := m.StopProviderRoutes(context.Background(), "notes", Netbird); err != nil {
		t.Fatal(err)
	}
	if !m.DetachNetbird() {
		t.Fatal("could not detach without routes")
	}
}

type retireDriver struct {
	NetbirdDriver
	dirs, hosts []string
	err         error
}

func (d *retireDriver) RetireStored(dir, host string) error {
	d.dirs, d.hosts = append(d.dirs, dir), append(d.hosts, host)
	return d.err
}

// Deleting a flat while no NetBird backend runs still releases the ports an
// earlier run recorded for it, through the linked driver.
func TestStopSlugRetiresStoredNetbirdPortsWithoutBackend(t *testing.T) {
	d := &retireDriver{}
	RegisterNetbird(d)
	t.Cleanup(func() { RegisterNetbird(nil) })
	m := netbirdManager(t, nil, nil)
	if err := m.StopSlug(context.Background(), "notes"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.hosts, []string{"notes"}) || len(d.dirs) != 1 || d.dirs[0] != m.netbirdDir {
		t.Fatalf("RetireStored calls: hosts=%v dirs=%v want dir %q", d.hosts, d.dirs, m.netbirdDir)
	}
	// A failed release fails the deletion so it can be retried.
	d.err = errors.New("disk full")
	if err := m.StopSlug(context.Background(), "notes"); err == nil {
		t.Fatal("a failed release did not fail StopSlug")
	}
	// A stock build has no driver and nothing to release.
	RegisterNetbird(nil)
	if err := m.StopSlug(context.Background(), "notes"); err != nil {
		t.Fatalf("stock build: %v", err)
	}
}
