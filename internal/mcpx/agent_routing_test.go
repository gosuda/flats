package mcpx

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/agentsetup"
)

// The routing text that `flats agent-setup docs` puts into agent files names
// tools by their backticked identifiers. Each one must be a real tool or a
// guide topic, so the instruction cannot drift from the server.
func TestAgentRoutingTextNamesRealTools(t *testing.T) {
	e := newEnv(t)
	tools := toolNames(t, connect(t, e.localURL))
	var named []string
	for _, m := range regexp.MustCompile("`([a-z_.]+)`").FindAllStringSubmatch(agentsetup.RoutingText, -1) {
		named = append(named, m[1])
	}
	for _, want := range []string{"check_client", "guide", "create_flat", "save_document"} {
		if !slices.Contains(tools, want) {
			t.Errorf("the server has no tool %q", want)
		}
		if !strings.Contains(agentsetup.RoutingText, want) {
			t.Errorf("the routing text does not name %q", want)
		}
	}
	for _, n := range named {
		switch {
		case strings.HasPrefix(n, "topic."):
			var out GuideOut
			if _, failed := call(t, e.local, "guide", map[string]any{"items": []string{n}}, &out); failed || len(out.Items) != 1 || out.Items[0].ID != n {
				t.Errorf("guide has no topic %q", n)
			}
		case n == "docs":
			// the create_flat type
		case !slices.Contains(tools, n):
			t.Errorf("the routing text names `%s`, which is not an MCP tool", n)
		}
	}
}
