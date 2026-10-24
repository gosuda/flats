package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

// Console domains record their network and name, and the NetBird console
// records the calling peer, in the request context that agent decisions
// read their arrival from.
func TestConsoleDomainContext(t *testing.T) {
	type seen struct {
		prov, name string
		ok         bool
		peer       string
	}
	var got seen
	record := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got.prov, got.name, got.ok = core.ConsoleDomainOf(r.Context())
		got.peer = core.NetbirdPeerOf(r.Context())
	})
	nb := nbConsole{&fakeNetbirdNet{}}
	h := withConsoleDomain(cdKey{providerNetbird, "ops"}, nb.handler("ops", record))
	for _, tc := range []struct {
		remote string
		want   seen
	}{
		{"127.0.0.1:4000", seen{providerNetbird, "ops", true, "laptop.netbird.cloud"}},
		{"100.90.1.2:4000", seen{providerNetbird, "ops", true, ""}},
	} {
		got = seen{}
		r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		r.RemoteAddr = tc.remote
		h.ServeHTTP(httptest.NewRecorder(), r)
		if got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.remote, got, tc.want)
		}
	}
	got = seen{}
	withConsoleDomain(cdKey{providerTailscale, "admin"}, record).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if got != (seen{providerTailscale, "admin", true, ""}) {
		t.Errorf("tailscale domain: %+v", got)
	}
}
