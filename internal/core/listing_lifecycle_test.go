package core_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/store"
)

// The production host routes public flats through the provider manager.
func TestPortalListingThroughLifecycleManager(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	loop, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pubLn, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pub := local.NewPublic(pubLn)
	provDir := filepath.Join(dir, "providers")
	if err := provider.Save(provDir, provider.File{Version: 1, Permitted: []provider.ID{provider.Portal}, Migration: provider.Migration{GrantsFromState: []provider.ID{}}}); err != nil {
		t.Fatal(err)
	}
	mgr, err := provider.New(provDir, provider.Options{Local: loop, Portal: pub})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := core.New(ctx, core.Config{DataDir: dir, Store: st, Private: loop, Lifecycle: mgr,
		ConsoleURL: func() string { return "http://console.test" }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close(); mgr.Close(); loop.Close(); pubLn.Close(); st.Close() })
	e := &env{dataDir: dir, svc: svc, priv: loop, pub: pub, st: st}

	if _, err := svc.SaveVersion(ctx, "blog", files("index.html", "hi"), core.SaveMeta{}, core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	publish(t, svc, "blog")
	permitPortal(t, svc, "blog")
	if _, err := svc.UpdateSettings(ctx, map[string]string{core.SetPortalHide: "true"}); err != nil {
		t.Fatal(err)
	}
	makePublic(t, e, "blog")
	if !listing(t, e, "blog") {
		t.Fatal("hide=true: public route opened listed")
	}
	if _, err := svc.SetPortalListing(ctx, "blog", store.ListingListed, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if listing(t, e, "blog") {
		t.Fatal("flat override listed not applied")
	}
	if _, err := svc.SetPortalListing(ctx, "blog", store.ListingDefault, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if !listing(t, e, "blog") {
		t.Fatal("default did not follow hide=true")
	}
	if _, err := svc.UpdateSettings(ctx, map[string]string{core.SetPortalHide: "false"}); err != nil {
		t.Fatal(err)
	}
	if listing(t, e, "blog") {
		t.Fatal("host default change not applied to the served route")
	}
}
