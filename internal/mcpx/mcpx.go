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
		registerReference(srv, c.version, limit)
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

func instructions(uploadLimit int64) string {
	return fmt.Sprintf(`Flats hosts websites ("flats") on the operator's own machine. Each flat has a slug (%d-%d characters: lowercase letters, digits and single hyphens, starting with a letter) and a Private URL through Local loopback or explicitly permitted Tailscale.

Runtime reference
Before authoring a server app, read resource flats://docs/runtime-api/v1 (resources/read), or call the read-only get_runtime_reference tool with {}. It contains the complete versioned FILES/DB, handler/response, encoding, persistence, ordinary environment variables, secrets and limits contract; no installed skill or source checkout is needed. FILES methods and DB methods are synchronous.

Workflow
1. save_draft or save_version uploads COMPLETE build content as a Private Draft revision, never a published version. Use encoding "utf8" for text and "base64" for binary files. A missing flat is created on first save. expected_revision detects conflicting edits; on conflict preserve your content, read get_draft and reconcile. deploy=true requests publish approval after saving.
2. publish freezes current Draft revision/hash and returns pending_approval with approval_url. deploy version 0 is compatible with publishing the current Draft; positive versions request activation of existing published versions. No health check, activation or new vN runs before a separately authorized operator decision. Successful publish assigns v1, v2, etc. list_versions contains only published versions.
3. open_preview version 0 (target draft) serves current Draft at a Private URL; positive versions preview published content. Current visitors keep seeing the published version while Draft changes. Previews follow loopback or existing tailnet ACL policy and never use Public providers.
4. rollback requests approval to activate an earlier published version. Code only by default; restore_data=true separately freezes explicit DB/FILES restoration. No data restoration occurs before operator approval. Read get_approval result_data for failure_code and actual data_impact/health_data/live_data.

Exposure
- New flats are Unpublished and Private. Private uses loopback or the existing tailnet ACL; it does not promise owner-only access. publication, live_version, visibility, provider permission/configuration and connection state are separate fields, never inferred from a URL.
- Visibility has only private/public; legacy public-listed/public-unlisted inputs normalize to public. BOTH transition directions require explicit operator approval. Same visibility is unchanged, and Unpublished cannot become Public. delete_flat also waits for approval.
- Give approval_url to the operator exactly as returned; poll get_approval. Agents have no operator session, decision tool, or provider grant tool. Headers cannot confer approval authority. Connecting/configuring a provider grants no publish or visibility consent.
- Local is always permitted; nonlocal tailscale, tailscale-funnel and portal require explicit operator permission/configuration. Public Tailscale means Funnel (internet), not Serve (tailnet). Public is NOT access control: anyone on the internet can open a ready Public route.

flats.json (optional, at the bundle root; unknown fields are rejected)
  name, kind ("static" default or "server"), entry (static default index.html; server default server.js, index.js, main.wasm or server.wasm), spa (serve the entry for unknown paths), not_found (e.g. "404.html", served with status 404), health (default "/"), screenshot (thumbnail path).
Server flats: export default { async fetch(request, env) { return new Response("hi") } }. env.DB is SQLite (query/exec), env.FILES is a per-flat local-disk string key-value store (not S3), ordinary environment variables and secrets arrive as env values (WASI receives only these as environment variables). Use list_env/set_env/delete_env for readable ordinary configuration. Values are server-only, never bundled into frontend assets, and live changes apply on the next deploy/redeploy, rollback or data restoration after any required approval, or Flats host restart. New previews capture current settings; running instances and automatic worker restarts keep their captured settings. Only the operator sets secret values; list_secrets shows names without values. Never put credentials in ordinary env variables. JavaScript server-side global fetch requires an operator-managed exact HTTP(S) origin allowlist; read get_network and ask the operator to grant origins in the console or with flats network set on the host. Agents cannot change it. Empty policy denies server fetch. Browser fetch uses the browser's real CORS/CSP protections and receives no injected secrets. Policy updates apply on the next approved activation, Flats host restart or new preview; running workers and automatic restarts retain captured grants. After clearing grants, redeploy to revoke live access.

Limits: %d bytes total (uncompressed) per upload (operator-configurable), %d files, no symlinks or paths outside the root. A single wrapping directory such as dist/ is stripped; .git and .DS_Store are skipped (save_version_from_dir also skips node_modules). Do not upload sources or node_modules. Every validation problem comes with a fix hint.
For large builds on the Flats host itself, call save_version_from_dir with an absolute directory path, or run the CLI: flats deploy <dir>.`, slug.MinLen, slug.MaxLen, uploadLimit, maxFiles)
}
