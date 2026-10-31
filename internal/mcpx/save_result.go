package mcpx

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A request can select the off server before a save reads the mode under the
// flat lock. Keep the off tools/list unchanged, but validate published receipts
// against their full schema rather than the earlier request's Draft-only schema.
func addSaveTool[In any](s *mcp.Server, t *tools, tool *mcp.Tool, handler mcp.ToolHandlerFor[In, SaveOut]) {
	full, err := jsonschema.For[SaveOut](nil)
	if err != nil {
		panic(err)
	}
	resolved, err := full.Resolve(nil)
	if err != nil {
		panic(err)
	}
	advertised, err := jsonschema.For[SaveOut](nil)
	if err != nil {
		panic(err)
	}
	if !t.publicInstance {
		delete(advertised.Properties, "version")
	}
	tool.OutputSchema = advertised
	mcp.AddTool(s, tool, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		res, out, err := handler(ctx, req, in)
		if err != nil || out.Version == 0 {
			return res, out, err
		}
		raw, err := json.Marshal(out)
		if err != nil {
			return nil, nil, err
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, nil, err
		}
		if err := resolved.Validate(value); err != nil {
			return nil, nil, fmt.Errorf("validating published save output: %w", err)
		}
		res.StructuredContent = json.RawMessage(raw)
		return res, nil, nil
	})
}
