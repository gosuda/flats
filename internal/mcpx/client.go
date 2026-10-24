package mcpx

import (
	"context"
	"encoding/json"

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
	m.GuideRevision = runtimeref.EffectiveGuideRevision(limit, stateOf(ctx).Guide)
	m.ServerURL, _ = ctx.Value(clientOriginKey{}).(string)
	return m
}

func registerClient(s *mcp.Server, svc *core.Service, autopilot bool) {
	check := &mcp.Tool{
		Name:        "check_client",
		Description: "Check CLI, loaded guide and bootstrap compatibility with this host. Read-only; omitted local facts remain unknown. Read guide topic.client-update for refresh steps. server_url is a routing hint, not authenticated identity.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}
	if autopilot {
		// The result gains an additive autopilot member; the off server keeps
		// today's result type and schema.
		mcp.AddTool(s, check, func(ctx context.Context, _ *mcp.CallToolRequest, in clientstate.CheckInput) (*mcp.CallToolResult, checkResultAutopilot, error) {
			res, err := clientstate.Check(clientManifest(ctx, svc), in)
			out := checkResultAutopilot{CheckResult: res, Autopilot: autopilotInfoOf(stateOf(ctx).Autopilot)}
			if err != nil {
				return nil, out, err
			}
			content, err := json.Marshal(out)
			if err != nil {
				return nil, out, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(content)}}}, out, nil
		})
	} else {
		mcp.AddTool(s, check, func(ctx context.Context, _ *mcp.CallToolRequest, in clientstate.CheckInput) (*mcp.CallToolResult, clientstate.CheckResult, error) {
			out, err := clientstate.Check(clientManifest(ctx, svc), in)
			if err != nil {
				return nil, out, err
			}
			content, err := json.Marshal(out)
			if err != nil {
				return nil, out, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(content)}}}, out, nil
		})
	}
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
