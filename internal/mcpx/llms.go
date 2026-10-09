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

// Paths served by LLMsHandler.
const (
	LLMsPath             = "/llms.txt"
	LLMsFullPath         = "/llms-full.txt"
	AgentGuidePath       = "/docs/agent-guide.md"
	RuntimeReferencePath = "/docs/runtime-api-v1.md"
	ContentTypesPath     = "/docs/content-types.md"
)

// LLMsPaths lists every path LLMsHandler answers.
var LLMsPaths = []string{LLMsPath, LLMsFullPath, AgentGuidePath, RuntimeReferencePath, ContentTypesPath}

// LLMsHandler serves agent-oriented documentation in the llms.txt format
// (https://llmstxt.org): the LLMsPath index, which links to the agent guide
// and the runtime/content-type references; LLMsFullPath concatenates the index
// and the agent guide, which already holds every guide topic.
// Mount it on the management server next to /mcp. The agent guide is built by
// the same code as the /mcp handler (on its own server instance), so its
// instructions, upload limit and tool list match what an MCP client sees.
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
	mux.HandleFunc("GET "+LLMsPath, func(w http.ResponseWriter, r *http.Request) {
		writeText(w, "text/plain; charset=utf-8", llmsIndex(origin(r), version))
	})
	mux.HandleFunc("GET "+AgentGuidePath, func(w http.ResponseWriter, r *http.Request) {
		guide, err := agentGuide(r.Context(), origin(r), servers)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeText(w, "text/markdown; charset=utf-8", guide)
	})
	mux.HandleFunc("GET "+ContentTypesPath, func(w http.ResponseWriter, r *http.Request) {
		writeText(w, "text/markdown; charset=utf-8", runtimeref.ContentTypesMarkdown)
	})
	mux.HandleFunc("GET "+RuntimeReferencePath, func(w http.ResponseWriter, r *http.Request) {
		writeText(w, "text/markdown; charset=utf-8", runtimeref.Markdown)
	})
	mux.HandleFunc("GET "+LLMsFullPath, func(w http.ResponseWriter, r *http.Request) {
		o := origin(r)
		guide, err := agentGuide(r.Context(), o, servers)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeText(w, "text/plain; charset=utf-8", llmsIndex(o, version)+"\n---\n\n"+guide)
	})
	return mux
}

func origin(r *http.Request) string {
	if r.TLS != nil {
		return "https://" + r.Host
	}
	return "http://" + r.Host
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

// llmsIndex is the llms.txt file: a title, a summary, free-form text without
// headings, and H2 sections that contain only link lists.
func llmsIndex(origin, version string) string {
	endpoint := origin + "/mcp"
	return fmt.Sprintf(`# Flats

> Flats hosts collaborative Markdown documents, websites and small server apps ("flats") on the operator's own machine. Agents save Private Draft content and request publication through MCP or the flats CLI; every publish, activation, rollback, deletion and visibility change waits for explicit operator approval.

This file is served by the Flats host at %s (version %s). Give the operator each approval_url exactly as returned, poll the approval, and report a version as live only after get_flat shows it current with a ready endpoint and you have fetched the page. Approval and provider decisions use the CSRF-protected console; it has no separate authentication, so only trusted local agents should run on the host.

Connect over MCP (Streamable HTTP) at %s:

- Claude Code: `+"`claude mcp add --transport http flats %s`"+`
- Codex: `+"`codex mcp add flats --url %s`"+`
- Cursor (.cursor/mcp.json): `+"`"+`{"mcpServers":{"flats":{"url":"%s"}}}`+"`"+`
- On the Flats host, `+"`flats mcp-config`"+` prints the same setup for the local management port.

Over MCP, call the read-only guide tool with topic.index first; it routes each task to short topics. The agent guide below holds the same topics in one document.

## Docs

- [Agent guide](%s%s): the MCP instructions and tools as this host reports them, the core CLI commands, and every guide topic and refusal page.
- [Runtime API v1](%s%s): complete server-flat contract (env.DB SQLite, env.FILES, handler and Response helpers, encoding, limits, ordinary environment variables, secrets, approvals). Also MCP resource %s and tool get_runtime_reference.

- [Content types](%s%s): docs Markdown manifests, save_document, get_document and live activation; also flats://docs/content-types/v1 and get_content_types.

## Optional

- [Full context](%s%s): this index and the agent guide in one file.
- [Source and README](https://github.com/gosuda/flats): install, operator setup, network providers and the trust model.
- [Deployment skill](https://github.com/gosuda/flats/blob/main/plugins/flats/skills/flats-deploy/SKILL.md): plugin skill that connects coding agents to a Flats host and hands off to the MCP guide.
`, origin, version, endpoint, endpoint, endpoint, endpoint,
		origin, AgentGuidePath, origin, RuntimeReferencePath, runtimeref.URI, origin, ContentTypesPath, origin, LLMsFullPath)
}

// agentGuide renders the MCP server's instructions and tools, the core CLI
// commands, and every guide topic and refusal page as one Markdown document.
func agentGuide(ctx context.Context, origin string, servers *serverCache) (string, error) {
	srv, limit := servers.current()
	tools, err := listTools(ctx, srv)
	if err != nil {
		return "", fmt.Errorf("list MCP tools: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `# Flats agent guide

Generated by the Flats host at %s (version %s). MCP endpoint (Streamable HTTP): %s/mcp

## Instructions

These are the instructions the MCP server sends at initialization.

%s

## MCP tools

`, origin, servers.version, origin, instructions(limit))
	for _, t := range tools {
		label := ""
		if t.Annotations != nil {
			switch {
			case t.Annotations.ReadOnlyHint:
				label = " (read-only)"
			case t.Annotations.DestructiveHint != nil && *t.Annotations.DestructiveHint:
				label = " (destructive)"
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

## Guide topics

The MCP tool ` + "`guide`" + ` serves each section below on its own (` + "`topic.<name>`" + `).

`)
	for i, name := range runtimeref.TopicOrder {
		md, _ := runtimeref.Topic(name)
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(md)
	}
	b.WriteString("\n## Refusals\n\n" + runtimeref.RefusalIntro)
	for _, c := range runtimeref.RefusalCategories() {
		md, _ := runtimeref.Refusal(c)
		b.WriteString("\n#" + strings.Replace(md, "## ", "## refusal.", 1))
	}
	return b.String(), nil
}
