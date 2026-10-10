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

// fakeZrok serves zrok routes on a loopback Net under zrok-<slug> and
// records Stop and Retire calls.
type fakeZrok struct {
	net                *local.Net
	serve              int
	stopped, retired   []string
	stopErr, retireErr error
	reserved           map[string]bool
}

func (f *fakeZrok) Reserved(slug string) bool { return f.reserved[slug] }

func (f *fakeZrok) Serve(ctx context.Context, slug string, h http.Handler) (string, error) {
	f.serve++
	return f.net.Serve(ctx, "zrok-"+slug, h, false)
}

func (f *fakeZrok) Stop(slug string) error {
	if f.stopErr != nil {
		return f.stopErr
	}
	f.stopped = append(f.stopped, slug)
	return f.net.Stop("zrok-" + slug)
}

func (f *fakeZrok) Retire(slug string) error {
	if f.retireErr != nil {
		return f.retireErr
	}
	f.retired = append(f.retired, slug)
	return f.net.Stop("zrok-" + slug)
}

func (f *fakeZrok) URL(slug string) string { return f.net.URL("zrok-" + slug) }

func (f *fakeZrok) Status() core.NetStatus {
	st := f.net.Status()
	st.Kind = "zrok"
	for i := range st.Hosts {
		if h := st.Hosts[i].Host; len(h) > 5 && h[:5] == "zrok-" {
			st.Hosts[i].Host = h[5:]
		}
	}
	return st
}

func (f *fakeZrok) Close() error { return nil }

func zrokManager(t *testing.T, permitted []ID, z ZrokNet) *Manager {
	t.Helper()
	ln, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	m, err := New(t.TempDir(), Options{Local: ln, Zrok: z, Grants: &File{Permitted: permitted}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func newFakeZrok(t *testing.T) *fakeZrok {
	t.Helper()
	ln, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return &fakeZrok{net: ln}
}

func TestParseIDAcceptsZrok(t *testing.T) {
	if id, err := ParseID("zrok"); err != nil || id != Zrok {
		t.Fatalf("ParseID(zrok) = %q, %v", id, err)
	}
	if !core.PublicProvider(Zrok) || core.PublicProvider(Tailscale) {
		t.Fatal("PublicProvider classification is wrong")
	}
}

func TestZrokServesPublicCurrentOnly(t *testing.T) {
	z := newFakeZrok(t)
	m := zrokManager(t, []ID{Zrok}, z)
	ctx := context.Background()

	// Drafts and private flats never reach zrok.
	_, err := m.ServeExposure(ctx, ExposureRequest{Slug: "notes", Host: "notes", Visibility: "private",
		Audience: AudienceCurrent, Handler: text("private"), Permitted: []ID{Zrok}})
	if !errors.Is(err, ErrProviderNotPermitted) || z.serve != 0 {
		t.Fatalf("private request: err=%v serve=%d", err, z.serve)
	}
	_, err = m.ServeExposure(ctx, ExposureRequest{Slug: "notes", Host: "notes-draft", Visibility: "public",
		Audience: AudienceDraft, Handler: text("draft"), Permitted: []ID{Zrok}, Ephemeral: true})
	if !errors.Is(err, ErrProviderNotPermitted) || z.serve != 0 {
		t.Fatalf("draft request: err=%v serve=%d", err, z.serve)
	}

	res, err := m.ServeExposure(ctx, ExposureRequest{Slug: "notes", Host: "notes", Visibility: "public",
		Audience: AudienceCurrent, Handler: text("public"), PrivateHandler: text("private"), Permitted: []ID{Zrok}})
	if err != nil || z.serve != 1 {
		t.Fatalf("public request: err=%v serve=%d endpoints=%+v", err, z.serve, res.Endpoints)
	}
	if got := get(t, endpointURL(res, Zrok)); got != "public" {
		t.Fatalf("zrok body = %q", got)
	}
	if got := get(t, endpointURL(res, Local)); got != "private" {
		t.Fatalf("local body = %q", got)
	}
	status, err := m.ExposureStatus(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ep := range status.Endpoints {
		if ep.Provider == Zrok && ep.Audience == AudienceCurrent {
			found = true
			if !ep.Ready || !ep.Configured || !ep.Permitted {
				t.Fatalf("zrok endpoint = %+v", ep)
			}
		}
	}
	if !found {
		t.Fatalf("status lacks zrok: %+v", status.Endpoints)
	}
	if ok, err := m.HasProviderRoute(ctx, "notes", Zrok); err != nil || !ok {
		t.Fatalf("HasProviderRoute = %v, %v", ok, err)
	}
}

func TestZrokRequiresGrantAndBackend(t *testing.T) {
	ctx := context.Background()
	req := ExposureRequest{Slug: "notes", Host: "notes", Visibility: "public", Audience: AudienceCurrent,
		Handler: text("public"), Permitted: []ID{Zrok}}

	z := newFakeZrok(t)
	ungranted := zrokManager(t, nil, z)
	if _, err := ungranted.ServeExposure(ctx, req); !errors.Is(err, ErrProviderNotPermitted) || z.serve != 0 {
		t.Fatalf("ungranted: err=%v serve=%d", err, z.serve)
	}

	unconfigured := zrokManager(t, []ID{Zrok}, nil)
	res, err := unconfigured.ServeExposure(ctx, req)
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("unconfigured: err=%v", err)
	}
	for _, ep := range res.Endpoints {
		if ep.Provider == Zrok && (ep.Configured || ep.State == stateReady) {
			t.Fatalf("unconfigured zrok endpoint = %+v", ep)
		}
	}
	host := unconfigured.HostStatus()
	if !slices.ContainsFunc(host, func(ep ExposureEndpoint) bool { return ep.Provider == Zrok && ep.Permitted && !ep.Configured }) {
		t.Fatalf("HostStatus = %+v", host)
	}
}

func TestZrokPublicStopKeepsPrivateAndRetriesFailures(t *testing.T) {
	z := newFakeZrok(t)
	m := zrokManager(t, []ID{Zrok}, z)
	ctx := context.Background()
	res, err := m.ServeExposure(ctx, ExposureRequest{Slug: "notes", Host: "notes", Visibility: "public",
		Audience: AudienceCurrent, Handler: text("public"), PrivateHandler: text("private"), Permitted: []ID{Zrok}})
	if err != nil {
		t.Fatal(err)
	}
	z.stopErr = errors.New("controller unavailable")
	stop, err := m.StopPublicRoutes(ctx, "notes")
	if err != nil || !slices.Equal(stop.Unconfirmed, []ID{Zrok}) || len(stop.Stopped) != 0 {
		t.Fatalf("failed stop = %+v, %v", stop, err)
	}
	if ok, _ := m.HasProviderRoute(ctx, "notes", Zrok); !ok {
		t.Fatal("unconfirmed zrok stop forgot the route")
	}
	z.stopErr = nil
	stop, err = m.StopPublicRoutes(ctx, "notes")
	if err != nil || !slices.Equal(stop.Stopped, []ID{Zrok}) || len(stop.Unconfirmed) != 0 {
		t.Fatalf("stop = %+v, %v", stop, err)
	}
	if len(z.retired) != 0 {
		t.Fatal("a visibility change released the zrok name")
	}
	if got := get(t, endpointURL(res, Local)); got != "private" {
		t.Fatalf("local body after public stop = %q", got)
	}
	if ok, _ := m.HasProviderRoute(ctx, "notes", Zrok); ok {
		t.Fatal("stopped zrok route still registered")
	}
}

func TestStopSlugRetiresZrokName(t *testing.T) {
	z := newFakeZrok(t)
	m := zrokManager(t, []ID{Zrok}, z)
	ctx := context.Background()
	if _, err := m.ServeExposure(ctx, ExposureRequest{Slug: "notes", Host: "notes", Visibility: "public",
		Audience: AudienceCurrent, Handler: text("public"), Permitted: []ID{Zrok}}); err != nil {
		t.Fatal(err)
	}
	z.retireErr = errors.New("controller unavailable")
	if err := m.StopSlug(ctx, "notes"); !errors.Is(err, core.ErrPublicStopUnconfirmed) {
		t.Fatalf("failed retire: %v", err)
	}
	if ok, _ := m.HasProviderRoute(ctx, "notes", Zrok); !ok {
		t.Fatal("failed retire forgot the zrok route")
	}
	z.retireErr = nil
	if err := m.StopSlug(ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(z.retired, []string{"notes"}) {
		t.Fatalf("retired = %v", z.retired)
	}
	if ok, _ := m.HasProviderRoute(ctx, "notes", Zrok); ok {
		t.Fatal("retired zrok route still registered")
	}
}

func TestStopSlugReleasesReservedNameWithoutRoute(t *testing.T) {
	z := newFakeZrok(t)
	z.reserved = map[string]bool{"was-public": true}
	// The flat no longer permits zrok and has no route: the backend's own
	// reservation record decides.
	m := zrokManager(t, []ID{Zrok}, z)
	ctx := context.Background()
	if err := m.StopSlug(ctx, "never-zrok"); err != nil || len(z.retired) != 0 {
		t.Fatalf("a flat without a zrok name touched zrok: %v %v", err, z.retired)
	}
	z.retireErr = errors.New("controller unavailable")
	if err := m.StopSlug(ctx, "was-public"); err == nil || errors.Is(err, core.ErrPublicStopUnconfirmed) {
		t.Fatalf("failed name release: %v", err)
	}
	z.retireErr = nil
	if err := m.StopSlug(ctx, "was-public"); err != nil || !slices.Equal(z.retired, []string{"was-public"}) {
		t.Fatalf("retired = %v, err = %v", z.retired, err)
	}
}

func TestExposurePolicyTokenUnchangedWithoutZrok(t *testing.T) {
	m, ln := managerWith(t, File{Version: 1, Permitted: []ID{Portal}})
	defer ln.Close()
	before, err := m.ExposurePolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m.AttachZrok(newFakeZrok(t))
	after, err := m.ExposurePolicy(context.Background())
	if err != nil || after == before {
		t.Fatal("attaching zrok did not change the policy token")
	}
}
