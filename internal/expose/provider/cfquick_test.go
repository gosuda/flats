package provider

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
)

// fakeQuick serves quick tunnel routes on a loopback Net under quick-<slug>
// and records Stop and Retire calls.
type fakeQuick struct {
	net              *local.Net
	serve            int
	stopped, retired []string
	stopErr          error
	retireErr        error
}

func (f *fakeQuick) Serve(ctx context.Context, slug string, h http.Handler) (string, error) {
	f.serve++
	return f.net.Serve(ctx, "quick-"+slug, h, false)
}

func (f *fakeQuick) Stop(slug string) error {
	if f.stopErr != nil {
		return f.stopErr
	}
	f.stopped = append(f.stopped, slug)
	return f.net.Stop("quick-" + slug)
}

func (f *fakeQuick) Retire(slug string) error {
	if f.retireErr != nil {
		return f.retireErr
	}
	f.retired = append(f.retired, slug)
	return f.net.Stop("quick-" + slug)
}

func (f *fakeQuick) URL(slug string) string { return f.net.URL("quick-" + slug) }

func (f *fakeQuick) Status() core.NetStatus {
	st := f.net.Status()
	st.Kind = "cloudflare-quick"
	for i := range st.Hosts {
		if h := st.Hosts[i].Host; len(h) > 6 && h[:6] == "quick-" {
			st.Hosts[i].Host = h[6:]
		}
	}
	return st
}

func (f *fakeQuick) Close() error { return nil }

func newFakeQuick(t *testing.T) *fakeQuick {
	t.Helper()
	ln, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return &fakeQuick{net: ln}
}

func quickManager(t *testing.T, dir string, permitted []ID, q QuickNet) *Manager {
	t.Helper()
	ln, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	opts := Options{Local: ln, Grants: &File{Permitted: permitted}}
	if q != nil {
		opts.CFQuick = q
	}
	m, err := New(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func publicReq(slug string) ExposureRequest {
	return ExposureRequest{Slug: slug, Host: slug, Visibility: "public", Audience: AudienceCurrent,
		Handler: text("public"), PrivateHandler: text("private"), Permitted: []ID{CFQuick}}
}

func TestParseIDAcceptsCloudflareQuick(t *testing.T) {
	if id, err := ParseID("cloudflare-quick"); err != nil || id != CFQuick {
		t.Fatalf("ParseID(cloudflare-quick) = %q, %v", id, err)
	}
	if !core.PublicProvider(CFQuick) {
		t.Fatal("cloudflare-quick is not classified as public")
	}
}

func TestCloudflareQuickServesPublicCurrentOnly(t *testing.T) {
	q := newFakeQuick(t)
	m := quickManager(t, t.TempDir(), []ID{CFQuick}, q)
	ctx := context.Background()

	_, err := m.ServeExposure(ctx, ExposureRequest{Slug: "notes", Host: "notes", Visibility: "private",
		Audience: AudienceCurrent, Handler: text("private"), Permitted: []ID{CFQuick}})
	if !errors.Is(err, ErrProviderNotPermitted) || q.serve != 0 {
		t.Fatalf("private request: err=%v serve=%d", err, q.serve)
	}
	_, err = m.ServeExposure(ctx, ExposureRequest{Slug: "notes", Host: "notes-draft", Visibility: "public",
		Audience: AudienceDraft, Handler: text("draft"), Permitted: []ID{CFQuick}, Ephemeral: true})
	if !errors.Is(err, ErrProviderNotPermitted) || q.serve != 0 {
		t.Fatalf("draft request: err=%v serve=%d", err, q.serve)
	}

	res, err := m.ServeExposure(ctx, publicReq("notes"))
	if err != nil || q.serve != 1 {
		t.Fatalf("public request: err=%v serve=%d endpoints=%+v", err, q.serve, res.Endpoints)
	}
	if got := get(t, endpointURL(res, CFQuick)); got != "public" {
		t.Fatalf("quick tunnel body = %q", got)
	}
	if got := get(t, endpointURL(res, Local)); got != "private" {
		t.Fatalf("local body = %q", got)
	}
	status, err := m.ExposureStatus(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(status.Endpoints, func(ep ExposureEndpoint) bool {
		return ep.Provider == CFQuick && ep.Audience == AudienceCurrent && ep.Ready && ep.Configured && ep.Permitted
	}) {
		t.Fatalf("status lacks a ready quick tunnel: %+v", status.Endpoints)
	}
	if ok, err := m.HasProviderRoute(ctx, "notes", CFQuick); err != nil || !ok {
		t.Fatalf("HasProviderRoute = %v, %v", ok, err)
	}
}

func TestCloudflareQuickRequiresGrantAndBackend(t *testing.T) {
	ctx := context.Background()
	q := newFakeQuick(t)
	ungranted := quickManager(t, t.TempDir(), nil, q)
	if _, err := ungranted.ServeExposure(ctx, publicReq("notes")); !errors.Is(err, ErrProviderNotPermitted) || q.serve != 0 {
		t.Fatalf("ungranted: err=%v serve=%d", err, q.serve)
	}
	unconfigured := quickManager(t, t.TempDir(), []ID{CFQuick}, nil)
	if _, err := unconfigured.ServeExposure(ctx, publicReq("notes")); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("unconfigured: err=%v", err)
	}
	if !slices.ContainsFunc(unconfigured.HostStatus(), func(ep ExposureEndpoint) bool {
		return ep.Provider == CFQuick && ep.Permitted && !ep.Configured
	}) {
		t.Fatalf("HostStatus = %+v", unconfigured.HostStatus())
	}
}

func TestCloudflareQuickPublicStopKeepsPrivateAndRetriesFailures(t *testing.T) {
	q := newFakeQuick(t)
	m := quickManager(t, t.TempDir(), []ID{CFQuick}, q)
	ctx := context.Background()
	res, err := m.ServeExposure(ctx, publicReq("notes"))
	if err != nil {
		t.Fatal(err)
	}
	q.stopErr = errors.New("edge unreachable")
	stop, err := m.StopPublicRoutes(ctx, "notes")
	if err != nil || !slices.Equal(stop.Unconfirmed, []ID{CFQuick}) || len(stop.Stopped) != 0 {
		t.Fatalf("failed stop = %+v, %v", stop, err)
	}
	if ok, _ := m.HasProviderRoute(ctx, "notes", CFQuick); !ok {
		t.Fatal("an unconfirmed stop forgot the route")
	}
	q.stopErr = nil
	stop, err = m.StopPublicRoutes(ctx, "notes")
	if err != nil || !slices.Equal(stop.Stopped, []ID{CFQuick}) || len(stop.Unconfirmed) != 0 {
		t.Fatalf("stop = %+v, %v", stop, err)
	}
	if len(q.retired) != 0 {
		t.Fatal("a visibility change deleted the tunnel record")
	}
	if got := get(t, endpointURL(res, Local)); got != "private" {
		t.Fatalf("local body after public stop = %q", got)
	}
}

func TestStopSlugRetiresQuickTunnel(t *testing.T) {
	q := newFakeQuick(t)
	m := quickManager(t, t.TempDir(), []ID{CFQuick}, q)
	ctx := context.Background()
	if _, err := m.ServeExposure(ctx, publicReq("notes")); err != nil {
		t.Fatal(err)
	}
	q.retireErr = errors.New("edge unreachable")
	if err := m.StopSlug(ctx, "notes"); !errors.Is(err, core.ErrPublicStopUnconfirmed) {
		t.Fatalf("failed retire: %v", err)
	}
	if ok, _ := m.HasProviderRoute(ctx, "notes", CFQuick); !ok {
		t.Fatal("a failed retire forgot the route")
	}
	q.retireErr = nil
	if err := m.StopSlug(ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(q.retired, "notes") {
		t.Fatalf("retired = %v", q.retired)
	}
	if ok, _ := m.HasProviderRoute(ctx, "notes", CFQuick); ok {
		t.Fatal("a retired quick tunnel route is still registered")
	}
}

func TestStopSlugDeletesQuickRecordWithProviderOff(t *testing.T) {
	dir := t.TempDir()
	qdir := filepath.Join(dir, "cloudflare-quick")
	if err := os.MkdirAll(qdir, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := filepath.Join(qdir, "was-public.json")
	if err := os.WriteFile(rec, []byte(`{"secret":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m := quickManager(t, dir, nil, nil)
	if err := m.StopSlug(context.Background(), "was-public"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rec); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a deleted flat kept its tunnel credentials: %v", err)
	}
}

func TestExposurePolicyCountsQuickOnlyWhileGranted(t *testing.T) {
	ctx := context.Background()
	m, ln := managerWith(t, File{Version: 1, Permitted: []ID{Portal}})
	defer ln.Close()
	before, err := m.ExposurePolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m.AttachCFQuick(newFakeQuick(t))
	if after, err := m.ExposurePolicy(ctx); err != nil || after != before {
		t.Fatal("an attached but ungranted quick tunnel backend changed the policy token")
	}
	granted, ln2 := managerWith(t, File{Version: 1, Permitted: []ID{Portal, CFQuick}})
	defer ln2.Close()
	without, err := granted.ExposurePolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	granted.AttachCFQuick(newFakeQuick(t))
	if with, err := granted.ExposurePolicy(ctx); err != nil || with == without {
		t.Fatal("attaching a granted quick tunnel backend did not change the policy token")
	}
}
