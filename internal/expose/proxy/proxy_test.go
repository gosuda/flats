package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gosuda/flats/internal/config"
)

func mustTemplate(t *testing.T, s string) config.Template {
	t.Helper()
	tmpl, err := config.ParseTemplate(s)
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

// seen records what a route handler received.
type seen struct {
	host, proto, path string
	header            http.Header
}

func listen(t *testing.T, tmpl *atomic.Pointer[config.Template]) *Net {
	t.Helper()
	n, err := Listen("127.0.0.1:0", func() config.Template { return *tmpl.Load() })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

func route(n *Net, host string, got *seen) {
	n.Serve(context.Background(), host, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = seen{host: r.Host, proto: r.Header.Get("X-Forwarded-Proto"), path: r.URL.Path, header: r.Header.Clone()}
		io.WriteString(w, "flat "+host)
	}))
}

// get sends a request to the listener with the Host a proxy forwarded.
func get(t *testing.T, n *Net, host, path string, header http.Header) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", "http://"+n.Addr()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestRouting(t *testing.T) {
	var tmpl atomic.Pointer[config.Template]
	https := mustTemplate(t, "https://{host}.site.example.org")
	tmpl.Store(&https)
	n := listen(t, &tmpl)
	var blog, preview seen
	route(n, "blog", &blog)
	route(n, "blog-ab12cd34", &preview)

	for _, host := range []string{"blog.site.example.org", "BLOG.site.example.org:8080", "blog.site.example.org."} {
		blog = seen{}
		code, body := get(t, n, host, "/page", nil)
		if code != http.StatusOK || body != "flat blog" {
			t.Fatalf("%s: %d %q", host, code, body)
		}
		if blog.host != "blog.site.example.org" || blog.proto != "https" || blog.path != "/page" {
			t.Errorf("%s: handler saw %+v", host, blog)
		}
	}
	if code, body := get(t, n, "blog-ab12cd34.site.example.org", "/", nil); code != http.StatusOK || body != "flat blog-ab12cd34" {
		t.Errorf("preview: %d %q", code, body)
	}
	// A served label reaches its flat for every path, /metrics included:
	// Flats' own /metrics is never on this listener.
	blog = seen{}
	if code, body := get(t, n, "blog.site.example.org", "/metrics", nil); code != http.StatusOK || body != "flat blog" || blog.path != "/metrics" {
		t.Errorf("/metrics of a flat: %d %q %+v", code, body, blog)
	}

	for _, tc := range []struct{ host, path string }{
		{"shop.site.example.org", "/"},         // no route
		{"a.blog.site.example.org", "/"},       // nested label
		{"flats.site.example.org", "/"},        // a console name
		{"flats.site.example.org", "/mcp"},     // MCP of a console name
		{"blog.example.org", "/"},              // wrong domain
		{"site.example.org", "/"},              // no label
		{"blog.site.example.org.evil", "/"},    // suffix not at the end
		{"evil<script>.site.example.org", "/"}, // not a label
		{n.Addr(), "/metrics"},                 // the listener itself
		{"localhost", "/metrics"},
		{"blog.localhost", "/"},
	} {
		code, body := get(t, n, tc.host, tc.path, nil)
		if code != http.StatusNotFound || body != notFound+"\n" {
			t.Errorf("%s%s: %d %q", tc.host, tc.path, code, body)
		}
		if strings.Contains(body, tc.host) {
			t.Errorf("%s: 404 body repeats the Host", tc.host)
		}
	}

	// Stop removes the route.
	n.Stop("blog-ab12cd34")
	if code, _ := get(t, n, "blog-ab12cd34.site.example.org", "/", nil); code != http.StatusNotFound {
		t.Errorf("stopped preview: %d", code)
	}
}

func TestRequestRewrite(t *testing.T) {
	var tmpl atomic.Pointer[config.Template]
	withPort := mustTemplate(t, "https://{host}.site.example.org:8443")
	tmpl.Store(&withPort)
	n := listen(t, &tmpl)
	var blog seen
	route(n, "blog", &blog)

	header := http.Header{
		"X-Forwarded-Proto":          {"http"},
		"X-Forwarded-Host":           {"other.example.org"},
		"Tailscale-User-Login":       {"alice@example.org"},
		"Tailscale-User-Name":        {"Alice"},
		"tailscale_user_login":       {"bob@example.org"},
		"Tailscale_User-Name":        {"Bob"},
		"TAILSCALE-USER-PROFILE-PIC": {"x"},
		"Tailscale-Something":        {"kept"},
		"Cookie":                     {"a=b"},
	}
	// The proxy dropped the template's port from Host; the flat still sees it.
	if code, _ := get(t, n, "blog.site.example.org", "/", header); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if blog.host != "blog.site.example.org:8443" || blog.proto != "https" {
		t.Errorf("handler saw Host %q, X-Forwarded-Proto %q", blog.host, blog.proto)
	}
	if got := blog.header.Values("X-Forwarded-Proto"); len(got) != 1 {
		t.Errorf("X-Forwarded-Proto values = %q", got)
	}
	for k := range blog.header {
		if strings.HasPrefix(strings.ReplaceAll(strings.ToLower(k), "_", "-"), "tailscale-user-") {
			t.Errorf("identity header %s reached the flat", k)
		}
	}
	if blog.header.Get("Tailscale-Something") != "kept" || blog.header.Get("Cookie") != "a=b" ||
		blog.header.Get("X-Forwarded-Host") != "other.example.org" {
		t.Errorf("other headers changed: %v", blog.header)
	}

	// An http template sets http, whatever the client sent.
	plain := mustTemplate(t, "http://{host}.flats.internal")
	tmpl.Store(&plain)
	if code, _ := get(t, n, "blog.flats.internal:8080", "/", http.Header{"X-Forwarded-Proto": {"https"}}); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if blog.host != "blog.flats.internal" || blog.proto != "http" {
		t.Errorf("handler saw Host %q, X-Forwarded-Proto %q", blog.host, blog.proto)
	}
}

func TestTemplateChange(t *testing.T) {
	var tmpl atomic.Pointer[config.Template]
	first := mustTemplate(t, "https://{host}.site.example.org")
	tmpl.Store(&first)
	n := listen(t, &tmpl)
	var blog seen
	route(n, "blog", &blog)
	if got := n.URL("blog"); got != "https://blog.site.example.org" {
		t.Errorf("URL = %q", got)
	}
	if code, _ := get(t, n, "blog.site.example.org", "/", nil); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}

	second := mustTemplate(t, "http://{host}.flats.example.net:8080")
	tmpl.Store(&second)
	if code, _ := get(t, n, "blog.site.example.org", "/", nil); code != http.StatusNotFound {
		t.Errorf("old name: %d", code)
	}
	if code, _ := get(t, n, "blog.flats.example.net", "/", nil); code != http.StatusOK || blog.host != "blog.flats.example.net:8080" {
		t.Errorf("new name: %d, handler saw %q", code, blog.host)
	}
	if got := n.URL("blog"); got != "http://blog.flats.example.net:8080" {
		t.Errorf("URL = %q", got)
	}
	st := n.Status()
	if len(st.Hosts) != 1 || st.Hosts[0].URL != "http://blog.flats.example.net:8080" {
		t.Errorf("Status = %+v", st)
	}

	// Without a template nothing is served and no address is built.
	var none config.Template
	tmpl.Store(&none)
	if code, _ := get(t, n, "blog.flats.example.net", "/", nil); code != http.StatusNotFound {
		t.Errorf("no template: %d", code)
	}
	if got := n.URL("blog"); got != "" {
		t.Errorf("URL without a template = %q", got)
	}
}

func TestServeStatusClose(t *testing.T) {
	var tmpl atomic.Pointer[config.Template]
	https := mustTemplate(t, "https://{host}.site.example.org")
	tmpl.Store(&https)
	n, err := Listen("127.0.0.1:0", func() config.Template { return *tmpl.Load() })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.Serve(context.Background(), "", http.NotFoundHandler()); err == nil {
		t.Error("Serve accepted an empty host")
	}
	if st := n.Status(); st.Kind != Kind || !st.Enabled || st.Hosts == nil || len(st.Hosts) != 0 {
		t.Errorf("empty Status = %+v", st)
	}
	for _, h := range []string{"shop", "blog-ab12cd34", "blog"} {
		url, err := n.Serve(context.Background(), h, http.NotFoundHandler())
		if err != nil || url != "https://"+h+".site.example.org" {
			t.Fatalf("Serve(%s) = %q, %v", h, url, err)
		}
	}
	n.Stop("shop")
	n.Stop("unknown")
	st := n.Status()
	if len(st.Hosts) != 2 {
		t.Fatalf("Status = %+v", st)
	}
	for i, h := range []string{"blog", "blog-ab12cd34"} {
		hi := st.Hosts[i]
		if hi.Host != h || hi.URL != "https://"+h+".site.example.org" || hi.State != "ready" || hi.Detail != Detail {
			t.Errorf("Hosts[%d] = %+v", i, hi)
		}
	}

	addr := n.Addr()
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("address still taken after Close: %v", err)
	}
	ln.Close()
}

func TestListenTaken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if n, err := Listen(ln.Addr().String(), func() config.Template { return config.Template{} }); err == nil {
		n.Close()
		t.Fatal("Listen bound a taken address")
	}
}
