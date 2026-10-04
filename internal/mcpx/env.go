package mcpx

import (
	"context"
	"fmt"
	"strings"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const envNote = "Ordinary values are readable by management clients; use operator-managed secrets for credentials. Changes apply to live on the next deploy/redeploy (after any required approval) or Flats host restart. New previews capture current settings; automatic worker restarts reuse their captured settings. Values are server-only and never enter frontend bundles."

type EnvOut struct {
	Env  []store.EnvVar `json:"env" jsonschema:"ordinary environment variables including values; excludes secrets"`
	Note string         `json:"note"`
}
type EnvNameIn struct {
	Slug string `json:"slug" jsonschema:"flat slug"`
	Name string `json:"name" jsonschema:"environment variable name"`
}
type SetEnvIn struct {
	Slug  string `json:"slug" jsonschema:"flat slug"`
	Name  string `json:"name" jsonschema:"environment variable name"`
	Value string `json:"value" jsonschema:"ordinary value, including an empty string; do not store credentials here"`
}
type EnvChangeOut struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Note   string `json:"note"`
}

func (t *tools) listEnv(ctx context.Context, _ *mcp.CallToolRequest, in SlugIn) (*mcp.CallToolResult, EnvOut, error) {
	vars, err := t.svc.ListEnv(ctx, in.Slug)
	if err != nil {
		return nil, EnvOut{}, toolErr(err, notFoundHint(err, in.Slug))
	}
	if vars == nil {
		vars = []store.EnvVar{}
	}
	out := EnvOut{Env: vars, Note: envNote}
	var b strings.Builder
	fmt.Fprintf(&b, "%s has %d ordinary environment variable(s).\n%s", in.Slug, len(vars), envNote)
	for _, v := range vars {
		fmt.Fprintf(&b, "\n%s=%q", v.Name, v.Value)
	}
	return result(b.String(), out), out, nil
}
func (t *tools) setEnv(ctx context.Context, _ *mcp.CallToolRequest, in SetEnvIn) (*mcp.CallToolResult, EnvChangeOut, error) {
	if err := t.svc.SetEnv(ctx, in.Slug, in.Name, in.Value, core.ViaMCP); err != nil {
		return nil, EnvChangeOut{}, toolErr(err, notFoundHint(err, in.Slug))
	}
	out := EnvChangeOut{Name: in.Name, Status: "stored", Note: envNote}
	return result(fmt.Sprintf("Stored %s for %s. %s", in.Name, in.Slug, envNote), out), out, nil
}
func (t *tools) deleteEnv(ctx context.Context, _ *mcp.CallToolRequest, in EnvNameIn) (*mcp.CallToolResult, EnvChangeOut, error) {
	if err := t.svc.DeleteEnv(ctx, in.Slug, in.Name, core.ViaMCP); err != nil {
		return nil, EnvChangeOut{}, toolErr(err, notFoundHint(err, in.Slug))
	}
	out := EnvChangeOut{Name: in.Name, Status: "deleted", Note: envNote}
	return result(fmt.Sprintf("Deleted %s from %s. %s", in.Name, in.Slug, envNote), out), out, nil
}
