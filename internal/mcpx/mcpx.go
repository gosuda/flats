// Package mcpx exposes Flats to coding agents as an MCP server over
// Streamable HTTP (stateless, JSON responses). Every tool calls core.Service
// directly with via = core.ViaMCP.
package mcpx

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/core"
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
	newHandler := func(external bool) http.Handler {
		return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return servers.get() },
			&mcp.StreamableHTTPOptions{
				Stateless:    true,
				JSONResponse: true,
				// Only the external handler skips the SDK's localhost-name
				// check. HostGuard has already validated its declared origin.
				DisableLocalhostProtection: external,
				// The body limit below follows the runtime upload setting.
				MaxRequestBodyBytes: -1,
			})
	}
	local, external := newHandler(false), newHandler(true)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody(svc.UploadLimit()))
		ctx := context.WithValue(r.Context(), loopbackKey{}, fromLoopback(r))
		ctx = context.WithValue(ctx, clientOriginKey{}, origin(r))
		h := local
		if api.ExternalOrigin(ctx) != "" {
			h = external
		}
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
		registerClient(srv, c.svc)
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
	if api.ExternalOrigin(r.Context()) != "" {
		return false
	}
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
	return runtimeref.Instructions(uploadLimit)
}
