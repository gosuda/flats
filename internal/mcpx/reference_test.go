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
			if err != nil || len(resources.Resources) != 1 {
				t.Fatalf("resource discovery: %+v, %v", resources, err)
			}
			r := resources.Resources[0]
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
	for _, path := range []string{"../../README.md", "../../plugins/flats/skills/flats-deploy/SKILL.md", "../../docs/design.md"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, marker := range []string{runtimeref.URI, "get_runtime_reference", "runtime-api-v1.md"} {
			if !strings.Contains(string(b), marker) {
				t.Errorf("%s omits %s", path, marker)
			}
		}
	}
}
