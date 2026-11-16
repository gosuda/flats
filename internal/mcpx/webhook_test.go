package mcpx

import (
	"strings"
	"testing"
)

// docs/spec/event-webhook.md, rule 9: an agent cannot read or change the webhook. No tool has webhook in its name.
func TestNoAgentToolIsAboutTheWebhook(t *testing.T) {
	e := newEnv(t)
	names := toolNames(t, e.local)
	if len(names) == 0 {
		t.Fatal("the tool list is empty")
	}
	for _, name := range names {
		if strings.Contains(strings.ToLower(name), "webhook") {
			t.Errorf("the tool %q is about the webhook", name)
		}
	}
}
