package mcpx

import (
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

// docs/spec/portal-require-auth.md, rule 3: no MCP tool changes a host setting, so an agent cannot change
// portal.require_auth. The change paths of the setting are the console, config.json with SIGHUP, and the offline CLI.
func TestNoToolChangesAHostSetting(t *testing.T) {
	e := newEnv(t)
	for _, name := range toolNames(t, e.local) {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "setting") || strings.Contains(lower, "require_auth") || strings.Contains(lower, "config") {
			t.Errorf("the tool %q reaches the host settings", name)
		}
	}
}

// Rules 12 and 22: in public instance mode, the text of an MCP result names no address for a flat whose Portal route the
// setting refuses, and it gives the reason of the endpoint, so the agent does not tell the user that the flat is available.
func TestLinkTextOfARefusedPortalRoute(t *testing.T) {
	detail := "portal.require_auth is on and site has no sign-in. Run flats signin set site to turn sign-in on, or turn the setting off in the console."
	fi := FlatInfo{
		privateLinksOff: true, Slug: "site", Visibility: "public", ConnectionState: "unavailable",
		Endpoints: []core.ExposureEndpoint{{Provider: core.ProviderPortal, Host: "site", State: "unavailable", Detail: detail}},
	}
	got := linkText(fi)
	if strings.Contains(got, "://") || !strings.Contains(got, "the public link is not open yet (unavailable: "+detail+")") {
		t.Fatalf("the text for a refused Portal route = %q", got)
	}
}
