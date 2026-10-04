package mcpx

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/core"
)

// RuntimeReferencePath serves the runtime API reference as Markdown over HTTP.
const RuntimeReferencePath = "/docs/runtime-api-v1.md"

// LLMsHandler serves agent-oriented documentation in the llms.txt format
// (https://llmstxt.org): GET /llms.txt, /llms-full.txt (the same index followed
// by the complete runtime reference) and RuntimeReferencePath. Mount it on
// the management server next to /mcp. The text is built from the live MCP
// server, so its instructions, upload limit and tool list match /mcp exactly.
//
// URLs in the text use the request's Host. The management listeners guard
// Host against DNS rebinding, so only this server's own names appear.
func LLMsHandler(svc *core.Service, opts Options) http.Handler {
	version := opts.Version
	if version == "" {
		version = "dev"
	}
	servers := &serverCache{svc: svc, version: version}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /llms.txt", func(w http.ResponseWriter, r *http.Request) {
		serveLLMs(w, r, servers, false)
	})
	mux.HandleFunc("GET /llms-full.txt", func(w http.ResponseWriter, r *http.Request) {
		serveLLMs(w, r, servers, true)
	})
	mux.HandleFunc("GET "+RuntimeReferencePath, func(w http.ResponseWriter, r *http.Request) {
		writeText(w, "text/markdown; charset=utf-8", runtimeref.Markdown)
	})
	return mux
}

func serveLLMs(w http.ResponseWriter, r *http.Request, servers *serverCache, full bool) {
	srv, limit := servers.current()
	tools, err := listTools(r.Context(), srv)
	if err != nil {
		http.Error(w, "list MCP tools: "+err.Error(), http.StatusInternalServerError)
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	text := llmsText(scheme+"://"+r.Host, servers.version, limit, tools)
	if full {
		text += "\n\n---\n\n" + runtimeref.Markdown
	}
	writeText(w, "text/plain; charset=utf-8", text)
}

func writeText(w http.ResponseWriter, ctype, text string) {
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("X-Content-Type-Options", "nosniff")
	// Revalidate every time: settings and the binary can change under the same URLs.
	h.Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(text))
}

// listTools reads the tool list through an in-memory MCP session, exactly as
// a connected agent would see it.
func listTools(ctx context.Context, srv *mcp.Server) ([]*mcp.Tool, error) {
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		return nil, err
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "flats-llms-txt", Version: "v1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		return nil, err
	}
	defer cs.Close()
	var tools []*mcp.Tool
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func llmsText(origin, version string, uploadLimit int64, tools []*mcp.Tool) string {
	endpoint := origin + "/mcp"
	var b strings.Builder
	fmt.Fprintf(&b, `# Flats

> Flats hosts websites and small server apps ("flats") on the operator's own machine. Agents save Private Draft content and request publication through MCP or the flats CLI; every publish, activation, rollback, deletion and visibility change waits for explicit operator approval.

This file is served by the Flats host at %s (version %s). Agents never hold operator authority: give the operator each approval_url exactly as returned, poll the approval, and report a version as live only after reading its ready endpoint.

## Connect

- MCP endpoint (Streamable HTTP): %s
- Claude Code: `+"`claude mcp add --transport http flats %s`"+`
- Codex: `+"`codex mcp add flats --url %s`"+`
- Cursor (.cursor/mcp.json): `+"`"+`{"mcpServers":{"flats":{"url":"%s"}}}`+"`"+`
- On the Flats host, `+"`flats mcp-config`"+` prints the same setup for the local management port.

## Docs

- [Runtime API v1](%s%s): complete server-flat contract (env.DB SQLite, env.FILES, handler and Response helpers, encoding, limits, secrets, approvals). Also MCP resource %s and tool get_runtime_reference.
- [Full context](%s/llms-full.txt): this file followed by the complete runtime API v1 reference.

## Agent guide

These are the instructions the MCP server sends at initialization.

%s

## MCP tools

`, origin, version, endpoint, endpoint, endpoint, endpoint, origin, RuntimeReferencePath, runtimeref.URI, origin, instructions(uploadLimit))
	for _, t := range tools {
		label := ""
		if t.Annotations != nil {
			switch {
			case t.Annotations.ReadOnlyHint:
				label = " (read-only)"
			case t.Annotations.DestructiveHint != nil && *t.Annotations.DestructiveHint:
				label = " (destructive; waits for operator approval)"
			}
		}
		fmt.Fprintf(&b, "- `%s`%s: %s\n", t.Name, label, t.Description)
	}
	b.WriteString(`
## CLI

On the Flats host, the flats CLI talks to the same server. Commands that request approval exit with code 3 while pending, also with --json.

- ` + "`flats draft <slug> <dir> --expected-revision N`" + `: save a Private Draft from a build directory.
- ` + "`flats deploy <dir> --flat <slug>`" + `: save Draft and request publish approval (` + "`--save-only`" + ` only saves).
- ` + "`flats publish <slug> --revision N --hash HASH`" + `: request approval to publish a frozen Draft.
- ` + "`flats approvals --json`" + `, ` + "`flats info <slug>`" + `, ` + "`flats logs <slug>`" + `: follow approvals, endpoints and events.
- ` + "`flats help`" + ` lists every command.

## Optional

- [Source and README](https://github.com/gosuda/flats): install, operator setup, network providers and the trust model.
- [Deployment skill](https://github.com/gosuda/flats/blob/main/plugins/flats/skills/flats-deploy/SKILL.md): step-by-step deploy, approval and verification workflow for coding agents.
`)
	return b.String()
}
