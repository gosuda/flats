package mcpx

import (
	"fmt"
	"github.com/gosuda/flats/internal/core"
	"strings"
	"testing"
)

func TestDocsIdleCommittedHintOverridesEveryRefusal(t *testing.T) {
	for _, name := range []string{"pick another slug", "the running document is unchanged", "fix the build and save a new version"} {
		t.Run(name, func(t *testing.T) {
			err := toolErr(&core.DeployError{Committed: true, Document: true, Cause: fmt.Errorf("%w: preparation failed", core.ErrUnavailable)}, name)
			text := err.Error()
			for _, want := range []string{"saved and committed", "Do not repeat", "get_logs", "get_flat"} {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %q: %s", want, text)
				}
			}
			if strings.Contains(text, "Hint: "+name) {
				t.Fatalf("stale refusal survived: %s", text)
			}
		})
	}
}
