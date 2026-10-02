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

	"github.com/oesni/flats/internal/core"
	"github.com/oesni/flats/internal/slug"
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
	limit := c.svc.UploadLimit()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.srv == nil || c.limit != limit {
		srv := mcp.NewServer(&mcp.Implementation{Name: "flats", Title: "Flats", Version: c.version},
			&mcp.ServerOptions{Instructions: instructions(limit)})
		register(srv, &tools{svc: c.svc})
		c.srv, c.limit = srv, limit
	}
	return c.srv
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

func instructions(uploadLimit int64) string {
	return fmt.Sprintf(`Flats hosts websites ("flats") on the operator's own machine. Each flat has a slug (%d-%d characters: lowercase letters, digits and single hyphens, starting with a letter) and a private URL on the operator's tailnet.

Workflow
1. save_version uploads the build output (what a browser needs: index.html, JS, CSS, images). Saving never changes what is live. Use encoding "utf8" for text and "base64" for binary files (images, fonts, wasm). A missing flat is created on first save. Pass deploy=true to save and deploy in one call.
2. deploy makes a saved version live after a health check: GET the manifest "health" path (default "/") must answer 2xx/3xx within 15s. If it fails, the previous live version keeps serving; read the health result, fix the build and save again.
3. open_preview serves any saved version at a temporary private URL without touching live. Previews close on the next deploy of the flat or after 24h unused.
4. rollback redeploys an earlier version (default: the one live before the current one). get_logs shows deploys, health checks and runtime output.

Exposure
- Flats start private: only the operator's tailnet can open the private URL.
- Making a flat public (set_visibility public-unlisted or public-listed) and delete_flat need the operator's approval. The call returns status "pending_approval" and an approval_url: give that URL to the user exactly as returned. Nothing changes until they approve it in the Flats console; poll get_approval. Making a flat private again applies immediately.
- public-unlisted is NOT access control. It only hides the flat from Portal relay listings; anyone with the URL can open it. Never describe it as private or protected.

flats.json (optional, at the bundle root; unknown fields are rejected)
  name, kind ("static" default or "server"), entry (static default index.html; server default server.js, index.js, main.wasm or server.wasm), spa (serve the entry for unknown paths), not_found (e.g. "404.html", served with status 404), health (default "/"), screenshot (thumbnail path).
Server flats: export default { async fetch(request, env) { return new Response("hi") } }. env.DB is SQLite (query/exec), env.FILES is a key-value file store, secrets arrive as env values. Only the operator sets secret values; list_secrets shows names.

Limits: %d bytes total (uncompressed) per upload (operator-configurable), %d files, no symlinks or paths outside the root. A single wrapping directory such as dist/ is stripped; .git and .DS_Store are skipped (save_version_from_dir also skips node_modules). Do not upload sources or node_modules. Every validation problem comes with a fix hint.
For large builds on the Flats host itself, call save_version_from_dir with an absolute directory path, or run the CLI: flats deploy <dir>.`, slug.MinLen, slug.MaxLen, uploadLimit, maxFiles)
}
