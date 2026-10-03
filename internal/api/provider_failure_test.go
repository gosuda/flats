package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

// A disposable network boundary reports exact typed failures without opening
// a non-local route. Core still performs the real frozen approval application.
type decisionNetwork struct{ failure error }

func (n *decisionNetwork) ServeExposure(_ context.Context, r core.ExposureRequest) (core.ExposureResult, error) {
	id := core.ProviderLocal
	if r.Visibility == "public" {
		id = core.ProviderFunnel
		if n.failure != nil {
			return core.ExposureResult{}, n.failure
		}
	}
	return core.ExposureResult{Endpoints: []core.ExposureEndpoint{{Provider: id, State: "ready", Ready: true, Configured: true, Permitted: true, Audience: r.Audience, Host: r.Host}}}, nil
}
func (n *decisionNetwork) StopPublicRoutes(context.Context, string) (core.PublicStopResult, error) {
	return core.PublicStopResult{Stopped: []core.ProviderID{core.ProviderFunnel}}, nil
}

func TestAuthorizedProviderApplyFailureAndPositiveControl(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
	}{
		{"provider_not_permitted", core.ErrProviderNotPermitted},
		{"provider_not_ready", core.ErrProviderNotReady},
		{"provider_unavailable", core.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			network := &decisionNetwork{}
			srv, _, _ := setupWithLifecycle(t, network)
			saveAndPublish(t, srv, "policy", "CURRENT")
			code, out := req(t, "POST", srv.URL+"/console/api/flats/policy/providers", strings.NewReader(`{"provider":"tailscale-funnel","permitted":true}`), consoleHdr(t, srv))
			if code != 200 {
				t.Fatalf("grant: %d %v", code, out)
			}
			code, pending := req(t, "POST", srv.URL+"/api/flats/policy/visibility", strings.NewReader(`{"visibility":"public"}`), nil)
			if code != 202 {
				t.Fatalf("request: %d %v", code, pending)
			}
			network.failure = fmt.Errorf("host provider: %w", tc.failure)
			id := pending["approval"].(map[string]any)["id"].(string)
			code, out = req(t, "POST", srv.URL+"/console/api/approvals/"+id+"/approve", nil, consoleHdr(t, srv))
			if code != 409 || out["category"] != tc.name {
				t.Fatalf("typed apply failure: %d %v", code, out)
			}
			a := out["approval"].(map[string]any)
			if a["status"] != "failed" || a["result_data"].(map[string]any)["failure_code"] != tc.name {
				t.Fatalf("persisted failure: %v", a)
			}
			_, persisted := req(t, "GET", srv.URL+"/api/approvals/"+id, nil, nil)
			if persisted["result_data"].(map[string]any)["failure_code"] != tc.name {
				t.Fatalf("read failure: %v", persisted)
			}
			flatState(t, srv, "policy", 1, "private", 1)
			network.failure = nil
			code, pending = req(t, "POST", srv.URL+"/api/flats/policy/visibility", strings.NewReader(`{"visibility":"public"}`), nil)
			if code != 202 {
				t.Fatalf("retry: %d %v", code, pending)
			}
			approveRequest(t, srv, pending)
			flatState(t, srv, "policy", 1, "public", 1)
		})
	}
}
