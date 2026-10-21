package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
)

// recordingTail keeps the handlers fakeTail discards, so a test can send a
// request through the tailnet and Funnel routes.
type recordingTail struct {
	fakeTail
	private, funnel map[string]http.Handler
}

func (f *recordingTail) Serve(ctx context.Context, host string, h http.Handler, ephemeral bool) (string, error) {
	f.private[host] = h
	return f.fakeTail.Serve(ctx, host, h, ephemeral)
}

func (f *recordingTail) ServeFunnel(ctx context.Context, host string, h http.Handler) (string, error) {
	f.funnel[host] = h
	return f.fakeTail.ServeFunnel(ctx, host, h)
}

// arrival answers with the provider the request is marked with.
var arrival = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	id, ok := core.RouteProvider(r.Context())
	if !ok {
		id = "untagged"
	}
	w.Write([]byte(id))
})

func serveRecorded(h http.Handler) string {
	if h == nil {
		return "not served"
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	return rec.Body.String()
}

// Every route the manager opens marks its requests with the provider they
// arrived on, for current routes, previews and the Private routes a Public
// flat keeps.
func TestRoutesCarryArrivalProvider(t *testing.T) {
	ln, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	portalLn, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { portalLn.Close() })
	tail := &recordingTail{private: map[string]http.Handler{}, funnel: map[string]http.Handler{}}
	nb := newFakeNetbird(t)
	all := []ID{Tailscale, Funnel, Portal, Zrok, CFQuick, Netbird, NetbirdExpose}
	m, err := New(t.TempDir(), Options{Local: ln, Tailscale: tail, Portal: &fakePortal{Public: local.NewPublic(portalLn)},
		Zrok: newFakeZrok(t), CFQuick: newFakeQuick(t), Netbird: nb, Grants: &File{Permitted: all}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	priv, err := m.ServeExposure(ctx, ExposureRequest{Slug: "notes", Host: "notes", Visibility: "private",
		Audience: AudienceCurrent, Handler: arrival, Permitted: []ID{Tailscale, Netbird}})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := m.ServeExposure(ctx, ExposureRequest{Slug: "notes", Host: "notes-abcdefgh", Visibility: "private",
		Audience: AudienceDraft, Handler: arrival, Ephemeral: true, Permitted: []ID{Tailscale}})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := m.ServeExposure(ctx, ExposureRequest{Slug: "site", Host: "site", Visibility: "public",
		Audience: AudienceCurrent, Handler: arrival, PrivateHandler: arrival, Permitted: all})
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name, got string
		want      ID
	}{
		{"local", get(t, endpointURL(priv, Local)), Local},
		{"tailscale", serveRecorded(tail.private["notes"]), Tailscale},
		{"netbird", get(t, endpointURL(priv, Netbird)), Netbird},
		{"preview local", get(t, endpointURL(preview, Local)), Local},
		{"preview tailscale", serveRecorded(tail.private["notes-abcdefgh"]), Tailscale},
		{"public flat local", get(t, endpointURL(pub, Local)), Local},
		{"public flat tailscale", serveRecorded(tail.private["site"]), Tailscale},
		{"public flat netbird", get(t, endpointURL(pub, Netbird)), Netbird},
		{"funnel", serveRecorded(tail.funnel["site"]), Funnel},
		{"portal", get(t, endpointURL(pub, Portal)), Portal},
		{"zrok", get(t, endpointURL(pub, Zrok)), Zrok},
		{"cloudflare-quick", get(t, endpointURL(pub, CFQuick)), CFQuick},
		{"netbird-expose", get(t, endpointURL(pub, NetbirdExpose)), NetbirdExpose},
	} {
		if c.got != string(c.want) {
			t.Errorf("%s: request marked %q, want %q", c.name, c.got, c.want)
		}
	}
}
