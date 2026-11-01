package provider

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
)

// blockedFunnel accepts the Funnel listener but reports it as not open, as
// tsnet does when Funnel setup fails (error) or the node has no Funnel
// route (unavailable).
type blockedFunnel struct {
	*fakeTail
	state string
}

func (b blockedFunnel) FunnelState(string) ExposureEndpoint {
	return ExposureEndpoint{Provider: Funnel, URL: "https://notes.example.ts.net", State: b.state, Detail: "funnel is not allowed by the tailnet policy"}
}

// statusPortal registers every slug and reports the state it was given for
// it, as the Portal backend does with its relays' status. Like Portal, Serve
// returns no URL, and a URL exists only while a relay is ready.
type statusPortal struct {
	state string
}

func (statusPortal) Serve(context.Context, string, http.Handler, bool) (string, error) {
	return "", nil
}
func (statusPortal) SetHidden(string, bool) error { return nil }
func (statusPortal) Stop(string) error            { return nil }
func (p statusPortal) URL(slug string) string {
	if p.state != stateReady {
		return ""
	}
	return "https://" + slug + ".relay.example"
}
func (p statusPortal) Status() core.NetStatus {
	return core.NetStatus{Kind: "portal", Enabled: true, Hosts: []core.HostInfo{{
		Host: "notes", URL: p.URL("notes"), State: p.state, Detail: "relay " + p.state,
	}}}
}
func (statusPortal) Close() error { return nil }

func TestServePublicReportsPublicRoutesThatNeverOpen(t *testing.T) {
	for _, tc := range []struct {
		name      string
		permitted []ID
		funnel    string
		portal    string
		wantErr   bool
	}{
		{name: "funnel setup failed", permitted: []ID{Funnel}, funnel: stateError, wantErr: true},
		{name: "funnel unavailable", permitted: []ID{Funnel}, funnel: stateUnavailable, wantErr: true},
		{name: "portal relay error", permitted: []ID{Portal}, portal: stateError, wantErr: true},
		{name: "both not open", permitted: []ID{Funnel, Portal}, funnel: stateError, portal: stateError, wantErr: true},
		{name: "portal open beside blocked funnel", permitted: []ID{Funnel, Portal}, funnel: stateError, portal: stateReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := Save(dir, File{Version: 1, Permitted: tc.permitted, Migration: Migration{GrantsFromState: []ID{}}}); err != nil {
				t.Fatal(err)
			}
			ln, err := local.Listen("127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			m, err := New(dir, Options{Local: ln, Tailscale: blockedFunnel{&fakeTail{}, tc.funnel}, Portal: statusPortal{state: tc.portal}})
			if err != nil {
				t.Fatal(err)
			}
			res, err := m.ServeExposure(context.Background(), ExposureRequest{
				Slug: "notes", Host: "notes", Visibility: "public", Audience: AudienceCurrent,
				Handler: text("public"), PrivateHandler: text("private"), Permitted: tc.permitted,
			})
			if tc.wantErr && !errors.Is(err, ErrProviderNotPermitted) {
				t.Fatalf("no public route opened, ServeExposure error = %v, want %v", err, ErrProviderNotPermitted)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("a public route opened, ServeExposure error = %v, want nil", err)
			}
			// Every attempted route still reports its endpoint, with the state
			// its backend gave. The retained Local route opens in every case.
			want := map[ID]string{Local: stateReady}
			for _, id := range tc.permitted {
				switch id {
				case Funnel:
					want[id] = tc.funnel
				case Portal:
					want[id] = tc.portal
				}
			}
			if len(res.Endpoints) != len(want) {
				t.Fatalf("endpoints = %+v, want one each for %v", res.Endpoints, want)
			}
			for id, state := range want {
				ep, ok := endpointOf(res, id)
				if !ok || ep.State != state {
					t.Fatalf("%s endpoint = %+v (found %v), want state %q", id, ep, ok, state)
				}
			}
		})
	}
}
