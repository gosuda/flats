//go:build consoledev

// A manual dev server for working on the console UI with real core/api
// behind it and seeded data:
//
//	FLATS_CONSOLE_ADDR=127.0.0.1:7979 go test -tags consoledev -run TestDevServer -timeout 0 ./internal/console
package console

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/flats/internal/api"
	docsapp "github.com/gosuda/flats/internal/apps/docs"
	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/runtime"
	"github.com/gosuda/flats/internal/store"
)

type devSystem struct{ priv, pub core.NetStatus }

func (d devSystem) Status(context.Context) any {
	return map[string]any{"host": "dev", "private": d.priv, "public": d.pub}
}

type liveSystem struct {
	priv *local.Net
	pub  *local.Public
}

func (s liveSystem) Status(ctx context.Context) any {
	return devSystem{priv: s.priv.Status(), pub: s.pub.Status()}.Status(ctx)
}

// devWorkerEnv makes this test binary a runtime worker: the Manager
// re-executes it to run server flats and documents.
const devWorkerEnv = "FLATS_CONSOLEDEV_WORKER"

func TestMain(m *testing.M) {
	if os.Getenv(devWorkerEnv) == "1" {
		if err := runtime.WorkerMain(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "worker:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestDevServer(t *testing.T) {
	addr := os.Getenv("FLATS_CONSOLE_ADDR")
	if addr == "" {
		t.Skip("set FLATS_CONSOLE_ADDR")
	}
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	priv, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pubNet, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pub := local.NewPublic(pubNet)
	// The runtime and docs app serve server flats and documents, as in
	// flats serve.
	docs := core.DocsApp{FS: docsapp.FS(), Hash: docsapp.Hash(), Entry: docsapp.Entry, ContentModule: docsapp.ContentModule}
	svc, err := core.New(ctx, core.Config{DataDir: dir, Store: st, Private: priv, Public: pub,
		Runtime: &runtime.Manager{DataDir: dir, Env: []string{devWorkerEnv + "=1"}, Logf: t.Logf}, DocsApp: docs,
		ConsoleURL: func() string { return "http://" + addr }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}

	img := image.NewRGBA(image.Rect(0, 0, 160, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 160; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(80 + y), 200, 255})
		}
	}
	var shot bytes.Buffer
	_ = png.Encode(&shot, img)
	f := func(kv ...string) []bundle.File {
		var out []bundle.File
		for i := 0; i < len(kv); i += 2 {
			out = append(out, bundle.File{Path: kv[i], Data: []byte(kv[i+1])})
		}
		return out
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	// approve decides a request the way the operator does in the console.
	approve := func(a *store.Approval) {
		t.Helper()
		if a == nil {
			t.Fatal("expected a pending approval")
		}
		_, err := svc.Decide(ctx, a.ID, true)
		must(err)
	}
	// save requests saving the flat's draft as a new version and approves it
	// unless leavePending is set. Nothing goes live.
	save := func(slug string, leavePending bool) {
		t.Helper()
		res, err := svc.Publish(ctx, slug, 0, "", core.ViaMCP)
		if err != nil {
			t.Fatalf("save %s as a version: %v", slug, err)
		}
		if !leavePending {
			approve(res.Approval)
		}
	}
	// publishLatest requests publishing (deploying) the newest saved version
	// and approves it.
	publishLatest := func(slug string) {
		t.Helper()
		fv, err := svc.GetFlat(ctx, slug)
		must(err)
		_, err = svc.Deploy(ctx, slug, fv.LatestVersion, core.ViaMCP)
		var p *core.PendingApproval
		if !errors.As(err, &p) {
			t.Fatalf("publish %s v%d: %v", slug, fv.LatestVersion, err)
		}
		approve(p.Approval)
	}
	publish := func(slug string) {
		t.Helper()
		save(slug, false)
		publishLatest(slug)
	}
	// makePublic permits the local Portal stand-in and approves Public.
	makePublic := func(slug string) {
		t.Helper()
		must(svc.SetProviderPermission(ctx, slug, store.ProviderPortal, true, core.ViaConsole))
		res, err := svc.SetVisibility(ctx, slug, store.Public, core.ViaConsole, "")
		must(err)
		approve(res.Approval)
	}

	// blog: published twice (v2 live), Public, with a screenshot and a secret.
	_, err = svc.SaveVersion(ctx, "blog", f("index.html", "<h1>one</h1>"), core.SaveMeta{GitSHA: "abc1234def", Message: "first"}, core.ViaMCP)
	must(err)
	publish("blog")
	_, err = svc.SaveVersion(ctx, "blog", append(f("index.html", "<h1>two</h1>", "flats.json", `{"name":"My blog","screenshot":"shot.png"}`),
		bundle.File{Path: "shot.png", Data: shot.Bytes()}), core.SaveMeta{GitSHA: "beef5678", GitDirty: true, Message: "add screenshot and a much longer commit message to see wrapping"}, core.ViaCLI)
	must(err)
	publish("blog")
	makePublic("blog")
	must(svc.SetSecret(ctx, "blog", "API_KEY", "x", core.ViaConsole))

	// notes: never published, with a request to save a version waiting for
	// approval.
	_, err = svc.SaveVersion(ctx, "notes", f("index.html", "<h1>notes</h1>"), core.SaveMeta{}, core.ViaMCP)
	must(err)
	save("notes", true)

	// landing: v1 saved but never published (Publish v1).
	_, err = svc.SaveVersion(ctx, "landing", f("index.html", "<h1>landing v1</h1>"), core.SaveMeta{Message: "first draft"}, core.ViaMCP)
	must(err)
	save("landing", false)

	// shop: v1 live, v2 saved and not published yet (Publish v2).
	_, err = svc.SaveVersion(ctx, "shop", f("index.html", "<h1>shop v1</h1>"), core.SaveMeta{Message: "open the shop"}, core.ViaMCP)
	must(err)
	publish("shop")
	_, err = svc.SaveVersion(ctx, "shop", f("index.html", "<h1>shop v2</h1>"), core.SaveMeta{Message: "new prices"}, core.ViaMCP)
	must(err)
	save("shop", false)

	// docs-site: v2 live after a rollback from v3, plus unsaved draft changes
	// (Save as v4; v3 is Previously live).
	for i, msg := range []string{"one", "two", "three"} {
		_, err = svc.SaveVersion(ctx, "docs-site", f("index.html", fmt.Sprintf("<h1>docs %d</h1>", i+1)), core.SaveMeta{Message: msg}, core.ViaMCP)
		must(err)
		publish("docs-site")
	}
	_, err = svc.Rollback(ctx, "docs-site", 2, core.ViaConsole)
	var rb *core.PendingApproval
	if !errors.As(err, &rb) {
		t.Fatalf("roll back docs-site: %v", err)
	}
	approve(rb.Approval)
	_, err = svc.SaveVersion(ctx, "docs-site", f("index.html", "<h1>docs 4 draft</h1>"), core.SaveMeta{Message: "work in progress"}, core.ViaMCP)
	must(err)

	// handbook: a Private document; launch-notes: a Public document.
	_, err = svc.SaveVersion(ctx, "handbook", f("flats.json", `{"type":"docs","name":"Team handbook"}`,
		"index.md", "# Team handbook\n\nHow we work.\n", "onboarding.md", "# Onboarding\n"), core.SaveMeta{}, core.ViaMCP)
	must(err)
	_, err = svc.SaveVersion(ctx, "launch-notes", f("flats.json", `{"type":"docs","name":"Launch notes"}`,
		"index.md", "# Launch notes\n\nShipped today.\n"), core.SaveMeta{}, core.ViaMCP)
	must(err)
	makePublic("launch-notes")

	// studio: a design (canvas of artboards) linked as a prototype.
	var studio []core.DesignFile
	for _, name := range []string{"canvas.json", "styles.css", "home.html", "plan.html", "checkout.html", "landing.html"} {
		data, err := os.ReadFile(filepath.Join("testdata", "design-studio", name))
		must(err)
		studio = append(studio, core.DesignFile{Path: name, Data: data})
	}
	_, err = svc.WriteDesign(ctx, "studio", core.DesignWrite{Files: studio, Name: "Daily Grind", Message: "first draft"}, core.ViaMCP)
	must(err)

	// empty-one: created but empty, with a delete request waiting for approval.
	_, err = svc.CreateFlat(ctx, "empty-one", "Empty one", core.ViaMCP)
	must(err)
	_, err = svc.Delete(ctx, "empty-one", core.ViaCLI, "cleanup")
	must(err)

	srv := &api.Server{Svc: svc, System: liveSystem{priv: priv, pub: pub}}
	mux := http.NewServeMux()
	apiH := srv.Handler()
	mux.Handle("/api/", apiH)
	mux.Handle("/console/api/", apiH)
	mux.Handle("/", Handler())
	t.Logf("console at http://%s/", addr)
	t.Fatal(http.ListenAndServe(addr, mux))
}
