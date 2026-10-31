package mcpx

import (
	"context"
	"encoding/json"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/buildinfo"
	"github.com/gosuda/flats/internal/clientstate"
	"github.com/gosuda/flats/internal/core"
)

type clientOriginKey struct{}

// clientManifest reports the guide revision of the guide a client reads in
// this request: the Autopilot revision while the request's copy is on.
func clientManifest(ctx context.Context, svc *core.Service) clientstate.Manifest {
	limit := svc.UploadLimit()
	m := clientstate.NewManifest(buildinfo.Get(), limit)
	m.GuideRevision = runtimeref.EffectiveGuideRevision(limit, stateOf(ctx).Guide, stateOf(ctx).Public)
	m.ServerURL, _ = ctx.Value(clientOriginKey{}).(string)
	return m
}

type checkResult struct {
	clientstate.CheckResult
	Autopilot      autopilotInfo      `json:"autopilot,omitzero"`
	PublicInstance publicInstanceInfo `json:"public_instance,omitzero"`
}

func registerClient(s *mcp.Server, svc *core.Service, autopilot, public bool) {
	check := &mcp.Tool{
		Name:        "check_client",
		Description: "Check CLI, loaded guide and bootstrap compatibility with this host. Read-only; omitted local facts remain unknown. Read guide topic.client-update for refresh steps. server_url is a routing hint, not authenticated identity.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}
	schema, err := jsonschema.For[checkResult](nil)
	if err != nil {
		panic(err)
	}
	// Each mode keeps its original schema: enabled members are required,
	// and disabled members are absent rather than optional.
	for _, mode := range []struct {
		name    string
		enabled bool
	}{{name: "autopilot", enabled: autopilot}, {name: "public_instance", enabled: public}} {
		if mode.enabled {
			schema.Required = append(schema.Required, mode.name)
		} else {
			delete(schema.Properties, mode.name)
		}
	}
	check.OutputSchema = schema
	mcp.AddTool(s, check, func(ctx context.Context, _ *mcp.CallToolRequest, in clientstate.CheckInput) (*mcp.CallToolResult, checkResult, error) {
		res, err := clientstate.Check(clientManifest(ctx, svc), in)
		out := checkResult{CheckResult: res}
		if err != nil {
			return nil, out, err
		}
		st := stateOf(ctx)
		if autopilot {
			out.Autopilot = autopilotInfoOf(st.Autopilot)
		}
		if public {
			out.PublicInstance = publicInstanceInfoOf(st.Public)
		}
		content, err := json.Marshal(out)
		if err != nil {
			return nil, out, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(content)}}}, out, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_client_skill",
		Description: "Read the host's thin Flats bootstrap SKILL.md, visible revision and SHA-256. Reading grants no permission to install; managed plugin caches must not be overwritten.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, clientstate.Manifest, error) {
		out := clientManifest(ctx, svc)
		content, err := json.Marshal(out)
		if err != nil {
			return nil, out, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(content)}}}, out, nil
	})
}
