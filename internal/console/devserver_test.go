//go:build consoledev

// A manual dev server for working on the console UI with real core/api
// behind it and seeded data:
//
//	FLATS_CONSOLE_ADDR=127.0.0.1:7979 go test -tags consoledev -run TestDevServer -timeout 0 ./internal/console
package console

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
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
	svc, err := core.New(ctx, core.Config{DataDir: dir, Store: st, Private: priv, Public: pub,
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
	_, err = svc.SaveVersion(ctx, "blog", f("index.html", "<h1>one</h1>"), core.SaveMeta{GitSHA: "abc1234def", Message: "first"}, core.ViaMCP)
	must(err)
	_, err = svc.SaveVersion(ctx, "blog", append(f("index.html", "<h1>two</h1>", "flats.json", `{"name":"My blog","screenshot":"shot.png"}`),
		bundle.File{Path: "shot.png", Data: shot.Bytes()}), core.SaveMeta{GitSHA: "beef5678", GitDirty: true, Message: "add screenshot and a much longer commit message to see wrapping"}, core.ViaCLI)
	must(err)
	_, err = svc.Deploy(ctx, "blog", 1, core.ViaMCP)
	must(err)
	_, err = svc.Deploy(ctx, "blog", 2, core.ViaMCP)
	must(err)
	_, err = svc.SetVisibility(ctx, "blog", store.PublicUnlisted, core.ViaConsole, "")
	must(err)
	_, err = svc.SaveVersion(ctx, "notes", f("index.html", "<h1>notes</h1>"), core.SaveMeta{}, core.ViaMCP)
	must(err)
	_, err = svc.CreateFlat(ctx, "empty-one", "Empty one", core.ViaMCP)
	must(err)
	_, err = svc.SetVisibility(ctx, "notes", store.PublicListed, core.ViaMCP, "share with friends")
	must(err)
	_, err = svc.Delete(ctx, "empty-one", core.ViaCLI, "cleanup")
	must(err)
	must(svc.SetSecret(ctx, "blog", "API_KEY", "x", core.ViaConsole))

	srv := &api.Server{Svc: svc, System: liveSystem{priv: priv, pub: pub}}
	mux := http.NewServeMux()
	apiH := srv.Handler()
	mux.Handle("/api/", apiH)
	mux.Handle("/console/api/", apiH)
	mux.Handle("/", Handler())
	t.Logf("console at http://%s/", addr)
	t.Fatal(http.ListenAndServe(addr, mux))
}
