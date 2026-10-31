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
	"sync/atomic"

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
		return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
			st := stateOf(r.Context())
			return servers.get(st.Autopilot.Enabled, st.Public != nil)
		},
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
		ctx := context.WithValue(withState(r, svc), loopbackKey{}, fromLoopback(r))
		ctx = context.WithValue(ctx, clientOriginKey{}, origin(r))
		if hook := stateTaken.Load(); hook != nil {
			(*hook)(r)
		}
		h := local
		if api.ExternalOrigin(ctx) != "" {
			h = external
		}
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// stateTaken, when a test sets it, runs after a request took its Autopilot
// copy and before it is served.
var stateTaken atomic.Pointer[func(*http.Request)]

// serverCache holds the four mode combinations. Networks, allowed operations
// and guides are request state, not server state.
type serverModes struct{ autopilot, public bool }

type serverCache struct {
	svc     *core.Service
	version string
	mu      sync.Mutex
	limit   int64
	servers map[serverModes]*mcp.Server
}

func (c *serverCache) get(autopilot, public bool) *mcp.Server {
	srv, _ := c.server(autopilot, public)
	return srv
}

// current returns the fixed off server for llms.txt.
func (c *serverCache) current() (*mcp.Server, int64) { return c.server(false, false) }

func (c *serverCache) server(autopilot, public bool) (*mcp.Server, int64) {
	limit := c.svc.UploadLimit()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limit != limit || c.servers == nil {
		c.servers, c.limit = make(map[serverModes]*mcp.Server), limit
	}
	mode := serverModes{autopilot: autopilot, public: public}
	if srv := c.servers[mode]; srv != nil {
		return srv, limit
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "flats", Title: "Flats", Version: c.version},
		&mcp.ServerOptions{Instructions: runtimeref.ServerInstructions(limit, autopilot, mode.public)})
	t := &tools{svc: c.svc, autopilot: autopilot, publicInstance: mode.public}
	register(srv, t)
	registerGuide(srv, c.svc, autopilot)
	registerClient(srv, c.svc, autopilot, mode.public)
	registerReference(srv, c.version, limit)
	registerContentTypes(srv, c.version, limit)
	if autopilot {
		registerAutopilot(srv, t)
	}
	c.servers[mode] = srv
	return srv, limit
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
