package mcpx

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	runtimeref "github.com/gosuda/flats/docs"
)

type referenceOut struct {
	DocumentationVersion string `json:"documentation_version"`
	URI                  string `json:"uri"`
	Markdown             string `json:"markdown"`
	HostVersion          string `json:"host_version"`
	UploadLimitBytes     int64  `json:"upload_limit_bytes"`
}

func registerReference(s *mcp.Server, hostVersion string, uploadLimit int64) {
	s.AddResource(&mcp.Resource{
		URI: runtimeref.URI, Name: "runtime-api-v1", MIMEType: "text/markdown",
		Description: "Complete Flats runtime API v1: FILES, SQLite, handler, limits, ordinary environment variables, secrets and approvals.",
	}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI: runtimeref.URI, MIMEType: "text/markdown", Text: runtimeref.Markdown,
		}}}, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "get_runtime_reference",
		Description: "Read the complete versioned runtime API before authoring a server app; " +
			"includes FILES/DB signatures, encoding, persistence, limits and deployment examples. No arguments.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, referenceOut, error) {
		out := referenceOut{
			DocumentationVersion: runtimeref.Version, URI: runtimeref.URI, Markdown: runtimeref.Markdown,
			HostVersion: hostVersion, UploadLimitBytes: uploadLimit,
		}
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: runtimeref.Markdown},
		}}, out, nil
	})
}

func registerContentTypes(s *mcp.Server, hostVersion string, uploadLimit int64) {
	s.AddResource(&mcp.Resource{URI: runtimeref.ContentTypesURI, Name: "content-types-v1", MIMEType: "text/markdown",
		Description: "Flats content types: websites and collaborative Markdown documents; manifests, access and agent workflow."},
		func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: runtimeref.ContentTypesURI, MIMEType: "text/markdown", Text: runtimeref.ContentTypesMarkdown}}}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "get_content_types", Description: "Read the versioned content-type contract before creating docs: save_document, get_document, bundle limits and approval-gated publication. Same as flats://docs/content-types/v1. No arguments.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, referenceOut, error) {
			out := referenceOut{DocumentationVersion: runtimeref.ContentTypesVersion, URI: runtimeref.ContentTypesURI, Markdown: runtimeref.ContentTypesMarkdown, HostVersion: hostVersion, UploadLimitBytes: uploadLimit}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out.Markdown}}}, out, nil
		})
}
