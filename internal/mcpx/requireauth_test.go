package mcpx

import (
	"strings"
	"testing"
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
