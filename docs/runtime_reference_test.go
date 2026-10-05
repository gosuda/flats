package runtimeref_test

import (
	"fmt"
	"strings"
	"testing"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/egress"
	"github.com/gosuda/flats/internal/runtime"
)

// Match a whole claim in its own section: equal numeric limits in unrelated
// APIs must not mask drift. Normalize wrapping, but preserve the statement.
func TestDocumentedRuntimeLimits(t *testing.T) {
	checks := []struct{ section, claim string }{
		{"JavaScript env.FILES", fmt.Sprintf("Each value is capped at **%d MiB (%s bytes)** of stored UTF-8 text;", runtime.MaxFileValue>>20, "10,485,760")},
		{"JavaScript env.FILES", fmt.Sprintf("Per-flat FILES total is **%d GiB (%s bytes)**;", runtime.MaxFilesTotal>>30, "1,073,741,824")},
		{"JavaScript env.DB", fmt.Sprintf("Query result cap is **%d MiB** while accumulating serialized rows", runtime.MaxQueryResult>>20)},
		{"Handler, request and response", fmt.Sprintf("Incoming body max **%d MiB** (413 if exceeded).", runtime.MaxRequestBody>>20)},
		{"Handler, request and response", fmt.Sprintf("Decoded response max **%d MiB**.", runtime.MaxResponseBody>>20)},
		{"Capabilities, limits and secrets", fmt.Sprintf("default **%d-second wall-clock deadline**, **%d MiB wasm memory per VM**", int(runtime.DefaultTimeout.Seconds()), runtime.DefaultMemoryPages*65536>>20)},
		{"Capabilities, limits and secrets", fmt.Sprintf("max **%s upload files**,", "20,000")},
		{"JavaScript outbound HTTP", fmt.Sprintf("**%d exact origins**", egress.MaxOrigins)},
		{"JavaScript outbound HTTP", fmt.Sprintf("**%d KiB URL**", egress.MaxURL>>10)},
		{"JavaScript outbound HTTP", fmt.Sprintf("**%d KiB supplied request headers**", egress.MaxHeaders>>10)},
		{"JavaScript outbound HTTP", fmt.Sprintf("**%d header names**", egress.MaxHeaderFields)},
		{"JavaScript outbound HTTP", fmt.Sprintf("**%d values per name**", egress.MaxHeaderFields)},
		{"JavaScript outbound HTTP", fmt.Sprintf("**%d MiB request body**", egress.MaxRequestBody>>20)},
		{"JavaScript outbound HTTP", fmt.Sprintf("**%d KiB response headers**", egress.MaxHeaders>>10)},
		{"JavaScript outbound HTTP", fmt.Sprintf("**%d MiB response body**", egress.MaxResponseBody>>20)},
		{"JavaScript outbound HTTP", fmt.Sprintf("**%d-second deadline**", int(egress.Timeout.Seconds()))},
	}
	for _, c := range checks {
		t.Run(c.section+"/"+c.claim, func(t *testing.T) {
			_, body, ok := strings.Cut(runtimeref.Markdown, "## "+c.section+"\n")
			if !ok {
				t.Fatal("missing section")
			}
			body, _, _ = strings.Cut(body, "\n## ")
			body = strings.Join(strings.Fields(body), " ")
			if strings.Count(body, c.claim) != 1 {
				t.Fatalf("section must contain exactly one complete limit claim: %s", c.claim)
			}
		})
	}
	// These human-readable byte counts must track their actual constants too.
	if runtime.MaxFileValue != 10485760 || runtime.MaxFilesTotal != 1073741824 || bundle.MaxFiles != 20000 {
		t.Fatal("byte/upload counts changed: update the published exact counts")
	}
}

// The embedded reference is also the agent-facing contract: distinguish a host
// restart from automatic worker recovery, and explain approval-time settings.
func TestDocumentedEnvironmentActivation(t *testing.T) {
	_, body, ok := strings.Cut(runtimeref.Markdown, "Ordinary app environment variables and operator-managed secrets")
	if !ok {
		t.Fatal("missing environment contract")
	}
	body, _, _ = strings.Cut(body, "`list_secrets {slug}`")
	body = strings.Join(strings.Fields(body), " ")
	for _, claim := range []string{
		"standalone data snapshot restoration captures current variables and secrets",
		"when activation begins; the health check and live worker share that snapshot.",
		"Settings are not pinned to the approval request or code version.",
		"Writes after capture apply at the next activation.",
		"a Flats host restart load current settings; automatic worker restarts reuse the captured snapshot.",
		"JavaScript `DB`/`FILES` host bindings take precedence over historical secrets",
		"WASI receives their stored strings.",
		"Replacing a historical secret must pass the current write validation.",
	} {
		if !strings.Contains(body, claim) {
			t.Errorf("missing environment behavior disclosure: %s", claim)
		}
	}
	if strings.Contains(body, "runtime restart") {
		t.Error("ambiguous runtime restart activation guidance")
	}
}
