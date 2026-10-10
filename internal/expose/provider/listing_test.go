package provider

import (
	"context"
	"testing"

	"github.com/gosuda/flats/internal/expose/local"
)

func TestPortalListingFollowsRequestAndSetPortalHidden(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, File{Version: 1, Permitted: []ID{Portal}, Migration: Migration{GrantsFromState: []ID{}}}); err != nil {
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
	pub := &fakePortal{Public: local.NewPublic(pubLn)}
	m, err := New(dir, Options{Local: ln, Portal: pub})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Without a route nothing is touched.
	if err := m.SetPortalHidden("notes", true); err != nil {
		t.Fatal(err)
	}
	if _, served := pub.Hidden("notes"); served {
		t.Fatal("SetPortalHidden opened a route")
	}
	if _, err := m.ServeExposure(ctx, ExposureRequest{Slug: "notes", Host: "notes", Visibility: "public",
		Audience: AudienceCurrent, Handler: text("public"), Permitted: []ID{Portal}, Hidden: true}); err != nil {
		t.Fatal(err)
	}
	if hidden, served := pub.Hidden("notes"); !served || !hidden {
		t.Fatalf("served=%v hidden=%v, want hidden route", served, hidden)
	}
	if err := m.SetPortalHidden("notes", false); err != nil {
		t.Fatal(err)
	}
	if hidden, _ := pub.Hidden("notes"); hidden {
		t.Fatal("SetPortalHidden(false) not applied")
	}
	if pub.serve != 1 {
		t.Fatalf("listing change reopened the route: serve=%d", pub.serve)
	}
}
