// Package mcpx exposes Flats to coding agents as an MCP server over
// Streamable HTTP (stateless, JSON responses). Every tool calls core.Service
// directly with via = core.ViaMCP.
package mcpx

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/slug"
)

// Options configures the MCP handler.
type Options struct {
	Version string // reported as the server implementation version
}

// Handler returns the MCP endpoint (mount at /mcp).
func Handler(svc *core.Service, opts Options) http.Handler {
	version := opts.Version
	if version == "" {
		version = "dev"
	}
	servers := &serverCache{svc: svc, version: version}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return servers.get() },
		&mcp.StreamableHTTPOptions{
			Stateless:    true,
			JSONResponse: true,
			// The body limit is applied per request below, because the upload
			// limit is a runtime setting.
			MaxRequestBodyBytes: -1,
		})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody(svc.UploadLimit()))
		ctx := context.WithValue(r.Context(), loopbackKey{}, fromLoopback(r))
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// serverCache rebuilds the MCP server when the upload limit changes, so the
// limit quoted in the server instructions matches the one enforced.
type serverCache struct {
	svc     *core.Service
	version string

	mu    sync.Mutex
	limit int64
	srv   *mcp.Server
}

func (c *serverCache) get() *mcp.Server {
	srv, _ := c.current()
	return srv
}

// current returns the server together with the upload limit it was built for.
func (c *serverCache) current() (*mcp.Server, int64) {
	limit := c.svc.UploadLimit()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.srv == nil || c.limit != limit {
		srv := mcp.NewServer(&mcp.Implementation{Name: "flats", Title: "Flats", Version: c.version},
			&mcp.ServerOptions{Instructions: instructions(limit)})
		register(srv, &tools{svc: c.svc})
		registerGuide(srv, c.svc)
		registerReference(srv, c.version, limit)
		registerContentTypes(srv, c.version, limit)
		c.srv, c.limit = srv, limit
	}
	return c.srv, c.limit
}

// maxBody bounds a JSON-RPC request: inline files arrive base64 (4/3 of
// their size) or as escaped JSON strings, plus envelope overhead.
func maxBody(uploadLimit int64) int64 {
	return uploadLimit + uploadLimit/2 + 1<<20
}

type loopbackKey struct{}

// isLoopback reports whether the MCP request came from a process on this host.
func isLoopback(ctx context.Context) bool {
	v, _ := ctx.Value(loopbackKey{}).(bool)
	return v
}

// fromLoopback is true only for a direct connection from this machine: both
// ends are loopback and no proxy forwarded the request on someone's behalf.
func fromLoopback(r *http.Request) bool {
	if !loopbackAddr(r.RemoteAddr) {
		return false
	}
	if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && la != nil && !loopbackAddr(la.String()) {
		return false
	}
	for _, h := range []string{"Forwarded", "X-Forwarded-For", "X-Real-Ip", "Tailscale-User-Login"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	return true
}

func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// instructions are sent at initialization. They keep only the first move and
// the invariants that must always be in context; everything else is a guide
// topic (docs/agent), read on demand.
func instructions(uploadLimit int64) string {
	return fmt.Sprintf(`Flats hosts websites, small server apps and Markdown documents ("flats") on the operator's own machine. Each flat has a slug (%d-%d characters: lowercase letters, digits and single hyphens, starting with a letter) and a Private URL through Local loopback or explicitly permitted Tailscale.

First move: call guide {"items":["topic.index"]}. It routes your task to short topics (static sites, page design, server apps, documents, approvals, rollback and data, preview and verification); request several items per call. For an existing flat, call get_flat before changing it. Server code: read guide topic.server (the complete contract is also resource %s and get_runtime_reference). Documents: guide topic.docs (also %s and get_content_types). A call Flats refuses names its category; guide refusal.<category> explains the fix. Clients that drop these instructions can read guide topic.instructions.

Always:
- Saving (save_draft, save_version, save_document) only changes a Private Draft. Publish, activation, rollback, data restore, visibility changes in BOTH directions and delete_flat wait for explicit operator approval and change nothing before it. Exception: update_document edits a running docs flat's live text at once, without approval; on a Public flat everyone sees it immediately.
- Give the user approval_url exactly as returned and poll get_approval. Never approve your own request or imitate the console; MCP has no approval tool.
- Ask the user before set_visibility, delete_flat or rollback with restore_data.
- Public is NOT access control: anyone on the internet can open a ready Public route. Private follows loopback or the existing tailnet ACL; it is not owner-only.
- Never infer state from a URL. publication, live_version, visibility and connection state are separate fields. Report a version live only after get_flat shows it current with a ready endpoint and you fetched the page.
- Upload complete build output, not sources: at most %d bytes total per upload (operator-configurable) and %d files.`, slug.MinLen, slug.MaxLen, runtimeref.URI, runtimeref.ContentTypesURI, uploadLimit, maxFiles)
}
