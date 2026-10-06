package mcpx

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/core"
)

func TestRuntimeReferenceDiscovery(t *testing.T) {
	e := newEnv(t)
	for name, cs := range map[string]*mcp.ClientSession{"loopback": e.local, "remote": e.remote} {
		t.Run(name, func(t *testing.T) {
			ins := cs.InitializeResult().Instructions
			if !strings.Contains(ins, runtimeref.URI) || !strings.Contains(ins, "get_runtime_reference") {
				t.Fatal("initialization must point to both documentation discovery paths")
			}
			resources, err := cs.ListResources(context.Background(), nil)
			if err != nil || len(resources.Resources) != 2 {
				t.Fatalf("resource discovery: %+v, %v", resources, err)
			}
			var r *mcp.Resource
			for _, resource := range resources.Resources {
				if resource.URI == runtimeref.URI {
					r = resource
				}
			}
			if r == nil {
				t.Fatal("runtime resource missing")
			}
			if r.URI != runtimeref.URI || r.MIMEType != "text/markdown" {
				t.Fatalf("wrong resource metadata: %+v", r)
			}
			read, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: r.URI})
			if err != nil || len(read.Contents) != 1 {
				t.Fatalf("resource read: %+v, %v", read, err)
			}
			if read.Contents[0].Text != runtimeref.Markdown || read.Contents[0].URI != r.URI {
				t.Fatal("MCP resource differs from published contract")
			}
			var out referenceOut
			text, failed := call(t, cs, "get_runtime_reference", map[string]any{}, &out)
			if failed || text != runtimeref.Markdown || out.Markdown != text {
				t.Fatal("fallback tool must return the same complete contract")
			}
			if out.DocumentationVersion != runtimeref.Version || out.HostVersion != "test" || out.URI != r.URI {
				t.Fatalf("wrong reference identity: %+v", out)
			}
			if out.UploadLimitBytes != e.svc.UploadLimit() {
				t.Fatal("host upload limit missing or stale")
			}
			tools, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, tool := range tools.Tools {
				if tool.Name == "get_runtime_reference" {
					if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
						t.Fatal("documentation tool must be read-only and idempotent")
					}
				}
			}
			if _, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "flats://docs/runtime-api/v999"}); err == nil {
				t.Fatal("unknown reference version silently accepted")
			}
		})
	}
	flats, err := e.svc.ListFlats(context.Background())
	if err != nil || len(flats) != 0 {
		t.Fatalf("reading documentation changed flat state: %v, %v", flats, err)
	}
}

func TestRuntimeReferenceFollowsUploadLimit(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.UpdateSettings(context.Background(), map[string]string{core.SetUploadMaxBytes: "8192"}); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, e.localURL)
	var out referenceOut
	_, failed := call(t, cs, "get_runtime_reference", map[string]any{}, &out)
	if failed || out.UploadLimitBytes != 8192 || !strings.Contains(cs.InitializeResult().Instructions, "8192 bytes") {
		t.Fatal("reference/instructions did not refresh with host settings")
	}
}

func TestRuntimeDocsDiscoveryConsistency(t *testing.T) {
	checks := []struct{ path, section, paragraph string }{
		{"../../README.md", "## Agent integration", "Restart/reconnect your client, list tools, then read resource\n`flats://docs/runtime-api/v1` or call **`get_runtime_reference` with `{}`**.\nThe [runtime API v1 reference](docs/runtime-api-v1.md) is embedded in the host\nand available through MCP without an installed skill or source checkout."},
		{"../../plugins/flats/skills/flats-deploy/SKILL.md", "## Runtime API discovery", "Before authoring a server app, read MCP resource `flats://docs/runtime-api/v1`\nor call the read-only `get_runtime_reference` tool with `{}`. The complete\n[runtime API v1 reference](../../../../docs/runtime-api-v1.md) ships in the host;\nMCP clients do not need this skill installed."},
		{"../../docs/design.md", "## Server flats (handler ABI)", "The authoritative [runtime API v1 reference](runtime-api-v1.md) is embedded\nin the binary and discoverable as MCP resource `flats://docs/runtime-api/v1`\nor read-only tool `get_runtime_reference`. It requires no skill/source access."},
	}
	for _, c := range checks {
		b, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatal(err)
		}
		_, section, ok := strings.Cut(string(b), c.section+"\n")
		if !ok {
			t.Fatalf("%s missing discovery section %s", c.path, c.section)
		}
		section, _, _ = strings.Cut(section, "\n## ")
		normalized := strings.Join(strings.Fields(section), " ")
		want := strings.Join(strings.Fields(c.paragraph), " ")
		if strings.Count(normalized, want) != 1 {
			t.Errorf("%s must contain the complete discovery instruction in its section", c.path)
		}
	}
}

func TestRuntimeDocsWASIEnvironmentConsistency(t *testing.T) {
	checks := []struct{ path, section, claim string }{
		{"../../README.md", "## Static and server flats", "only the flat's configured environment variables and secrets, with no inherited host environment, plus clocks and randomness."},
		{"../../docs/design.md", "## Server flats (handler ABI)", "Environment includes only the flat's configured environment variables and secrets, with no inherited host process environment."},
		{"../../plugins/flats/skills/flats-deploy/SKILL.md", "## Server flats", "Only the flat's configured environment variables and secrets are injected as environment variables; there is no inherited host environment."},
	}
	for _, c := range checks {
		b, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatal(err)
		}
		_, section, ok := strings.Cut(string(b), c.section+"\n")
		if !ok {
			t.Fatalf("%s missing %s", c.path, c.section)
		}
		section, _, _ = strings.Cut(section, "\n## ")
		if strings.Count(strings.Join(strings.Fields(section), " "), c.claim) != 1 {
			t.Errorf("%s must describe the actual app-scoped WASI environment", c.path)
		}
	}
}
