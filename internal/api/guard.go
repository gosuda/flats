package api

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// HostGuard wraps the whole management server (API, MCP and console) on one
// listener. It refuses requests whose Host is not a name of that listener,
// which defeats DNS rebinding: a page that points its own name at
// 127.0.0.1 or at the console node still sends its own name as Host. It
// also refuses requests whose Origin, when present, is not one of those
// names.
type HostGuard struct {
	// Hosts returns the host names this listener answers to (any case;
	// IPv6 literals without brackets). It is called per request, so names
	// learned later, such as the tailnet's MagicDNS name, take effect.
	Hosts func() []string
	// Ports lists the accepted ports; "" accepts a Host without a port.
	Ports []string
	Next  http.Handler
}

func (g *HostGuard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !g.allowed(r.Host) {
		writeErr(w, http.StatusForbidden, fmt.Errorf("Host %q is not an address of this Flats server (DNS rebinding protection); use the console URL or the loopback address", r.Host))
		return
	}
	if o := r.Header.Get("Origin"); o != "" && !g.allowedOrigin(o) {
		writeErr(w, http.StatusForbidden, fmt.Errorf("cross-origin request from %q refused: only the Flats console itself may call this server from a browser", o))
		return
	}
	g.Next.ServeHTTP(w, r)
}

// splitHost splits a Host header or URL host into a lowercase name and port.
func splitHost(hostport string) (name, port string) {
	name = hostport
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		name, port = h, p
	} else {
		name = strings.TrimSuffix(strings.TrimPrefix(name, "["), "]")
	}
	return strings.TrimSuffix(strings.ToLower(name), "."), port
}

func (g *HostGuard) allowed(hostport string) bool {
	if hostport == "" {
		return false
	}
	name, port := splitHost(hostport)
	if name == "" || !slices.Contains(g.Ports, port) {
		return false
	}
	for _, h := range g.Hosts() {
		if h != "" && strings.EqualFold(strings.TrimSuffix(h, "."), name) {
			return true
		}
	}
	return false
}

func (g *HostGuard) allowedOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" || u.User != nil {
		return false // includes the opaque origin "null"
	}
	return g.allowed(u.Host)
}

// sameOrigin reports whether a present Origin names the host the request was
// sent to. It is the API's own check, independent of HostGuard.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	on, op := splitHost(u.Host)
	rn, rp := splitHost(r.Host)
	if op == "" {
		op = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	if rp == "" {
		rp = op // a Host without a port uses the scheme's default
	}
	return on == rn && op == rp
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// bodyTypes are the request Content-Types that make a browser send a CORS
// preflight (none is CORS-safelisted), so a page on another origin cannot
// send them without the server's consent.
var bodyTypes = []string{
	"application/json",
	"application/gzip", "application/x-gzip", "application/x-tar",
	"application/zip", "application/x-zip-compressed", "application/octet-stream",
}

var (
	errCrossSite = errors.New("cross-site browser requests to the Flats API are refused")
	errNoClient  = errors.New("requests that change something must send Content-Type: application/json (or an archive type for uploads) or an X-Flats-Client header; this keeps web pages from calling the API")
)

// agentRequest guards /api against cross-site browser requests (CSRF). Real
// clients never send Sec-Fetch-Site; a browser sends it on every request.
// Mutations must carry a header no cross-origin page can send without a
// preflight, which this server never grants.
func agentRequest(r *http.Request) (int, error) {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site", "same-site":
		return http.StatusForbidden, errCrossSite
	}
	if !sameOrigin(r) {
		return http.StatusForbidden, errCrossSite
	}
	if safeMethod(r.Method) || r.Header.Get("X-Flats-Client") != "" {
		return 0, nil
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && slices.Contains(bodyTypes, mt) {
		return 0, nil
	}
	return http.StatusUnsupportedMediaType, errNoClient
}

// consoleRequest provides browser CSRF protection, not operator identity.
// Requests carry
// X-Flats-Console: 1 (a custom header, so cross-origin pages need a
// preflight), never X-Flats-Client, and mutations come same-origin with the
// browser's Origin header. OperatorAuthority separately validates decisions.
func consoleRequest(r *http.Request) error {
	if r.Header.Get("X-Flats-Console") != "1" {
		return errors.New("console endpoints only accept requests from the Flats web console")
	}
	if r.Header.Get("X-Flats-Client") != "" {
		return errors.New("console endpoints do not accept the CLI or other API clients; the operator decides in the web console")
	}
	if !sameOrigin(r) {
		return errCrossSite
	}
	if safeMethod(r.Method) {
		return nil
	}
	if r.Header.Get("Sec-Fetch-Site") != "same-origin" || r.Header.Get("Origin") == "" {
		return errors.New("console changes must come from the Flats console page in a browser (same-origin request with an Origin header)")
	}
	return nil
}

type approverKey struct{}

// TailnetIdentity records the Tailscale-User-Login header as the identity of
// the operator for console decisions. Mount it only behind the tsnet identity
// middleware, which drops client-sent Tailscale-User-* headers and sets them
// from WhoIs.
func TailnetIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if login := r.Header.Get("Tailscale-User-Login"); login != "" {
			if dec, err := new(mime.WordDecoder).DecodeHeader(login); err == nil {
				login = dec
			}
			r = r.WithContext(context.WithValue(r.Context(), approverKey{}, login))
		}
		next.ServeHTTP(w, r)
	})
}

// approverOf returns the tailnet login recorded by TailnetIdentity, or "".
func approverOf(ctx context.Context) string {
	s, _ := ctx.Value(approverKey{}).(string)
	return s
}
