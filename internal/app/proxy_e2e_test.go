package app

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/proxy"
	"github.com/gosuda/flats/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// echoFlat is a server flat that reports what it received.
const echoFlat = `export default { async fetch(request) {
 return Response.json({url: request.url, proto: request.headers.get("x-forwarded-proto"), identity: request.headers.get("tailscale-user-login")});
}};`

// front is the operator's reverse proxy in front of the proxy listener. It
// keeps the visitor's Host and, by mode, sets X-Forwarded-Proto ("set"),
// passes the client's value through ("client") or sends none ("none").
type front struct {
	*httptest.Server
	client *http.Client
}

func newFront(t *testing.T, target, mode string, tlsFront, dropPort bool) *front {
	t.Helper()
	u, _ := url.Parse("http://" + target)
	rp := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(u)
		r.Out.Host = r.In.Host
		if h, _, err := net.SplitHostPort(r.In.Host); err == nil && dropPort {
			r.Out.Host = h
		}
		switch mode {
		case "set":
			r.SetXForwarded()
		case "client":
			r.Out.Header.Set("X-Forwarded-Proto", r.In.Header.Get("X-Forwarded-Proto"))
		}
	}}
	f := &front{}
	if tlsFront {
		f.Server = httptest.NewTLSServer(rp)
	} else {
		f.Server = httptest.NewServer(rp)
	}
	t.Cleanup(f.Close)
	addr := f.Listener.Addr().String()
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	f.client = &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return f
}

// do sends method to u through the front with headers as key/value pairs.
func (f *front) do(t *testing.T, method, u, body string, hdr ...string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, u, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			req.Host = hdr[i+1]
			continue
		}
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), res.Header
}

// yjsInsert is a Yjs (v1) update from a new client that inserts text at the
// start of the editor's "markdown" text, with no origins, so it applies to
// any document state. TestYjsInsertMatchesFixture checks it against an update
// the yjs library produced.
func yjsInsert(client uint64, text string) string {
	varUint := func(b []byte, n uint64) []byte {
		for n >= 0x80 {
			b = append(b, byte(n)|0x80)
			n >>= 7
		}
		return append(b, byte(n))
	}
	varString := func(b []byte, s string) []byte { return append(varUint(b, uint64(len(s))), s...) }
	// One client with one struct at clock 0: a ContentString item (info 4)
	// whose parent is the root type "markdown"; then an empty delete set.
	u := varUint([]byte{1, 1}, client)
	u = append(u, 0, 4, 1)
	u = varString(u, "markdown")
	u = varString(u, text)
	return base64.StdEncoding.EncodeToString(append(u, 0))
}

func TestYjsInsertMatchesFixture(t *testing.T) {
	b, err := os.ReadFile("../apps/docs/testdata/updates.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct{ Independent string }
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	// internal/apps/docs/_web/fixtures.mjs: client 104 inserts this text.
	if got := yjsInsert(104, "Person 한국어 🙂\n"); got != f.Independent {
		t.Fatalf("yjsInsert = %s, want %s", got, f.Independent)
	}
}

// docsSession opens the document editor's WebSocket on label through the
// front with origin, sends hello and returns the first message or the close
// the server answered with. With update set it then sends that Yjs update
// after the welcome and returns the server's ack or error instead.
func (f *front) docsSession(t *testing.T, label, origin string, tlsFront bool, update string) (map[string]any, string) {
	t.Helper()
	var c net.Conn
	var err error
	if tlsFront {
		c, err = tls.Dial("tcp", f.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true, ServerName: label})
	} else {
		c, err = net.Dial("tcp", f.Listener.Addr().String())
	}
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))
	key := make([]byte, 16)
	rand.Read(key)
	fmt.Fprintf(c, "GET /_docs/ws?doc=index.md HTTP/1.1\r\nHost: %s\r\nOrigin: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n",
		label, origin, base64.StdEncoding.EncodeToString(key))
	r := bufio.NewReader(c)
	res, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Sprintf("upgrade %d", res.StatusCode)
	}
	send := func(m map[string]any) {
		msg, _ := json.Marshal(m)
		// A masked text frame with a zero mask key.
		frame := []byte{0x81, 0x80 | 126, byte(len(msg) >> 8), byte(len(msg)), 0, 0, 0, 0}
		if len(msg) < 126 {
			frame = []byte{0x81, 0x80 | byte(len(msg)), 0, 0, 0, 0}
		}
		if _, err := c.Write(append(frame, msg...)); err != nil {
			t.Fatal(err)
		}
	}
	send(map[string]any{"t": "hello", "v": 1, "doc": "index.md", "sv": "AA==", "name": "Proxy test", "known": nil})
	for {
		var h [2]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return nil, "read: " + err.Error()
		}
		n := uint64(h[1] & 127)
		switch n {
		case 126:
			var b [2]byte
			io.ReadFull(r, b[:])
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			io.ReadFull(r, b[:])
			n = binary.BigEndian.Uint64(b[:])
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, "read: " + err.Error()
		}
		switch h[0] & 15 {
		case 8:
			if len(payload) >= 2 {
				return nil, fmt.Sprintf("close %d %s", binary.BigEndian.Uint16(payload), payload[2:])
			}
			return nil, "close"
		case 1:
			var m map[string]any
			if err := json.Unmarshal(payload, &m); err != nil {
				t.Fatal(err)
			}
			switch {
			case m["t"] == "welcome" && update != "":
				// The docs app refuses an update id reused with other bytes.
				id := sha256.Sum256([]byte(update))
				send(map[string]any{"t": "update", "id": fmt.Sprintf("proxy-edit-%x", id[:8]), "u": update})
			case m["t"] == "welcome" && update == "", m["t"] == "error", m["t"] == "ack":
				return m, ""
			}
		}
	}
}

// pin pins or unpins revision n of the document at base through the front,
// as its History panel does with origin.
func (f *front) pin(t *testing.T, base, origin string, n int, pinned bool) int {
	t.Helper()
	code, _, _ := f.do(t, "POST", fmt.Sprintf("%s/_docs/api/revisions/%d/pin", base, n), fmt.Sprintf(`{"pinned":%t}`, pinned),
		"X-Flats-Docs", "1", "Content-Type", "application/json", "Origin", origin)
	return code
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || res.IsError {
		raw, _ := json.Marshal(res)
		t.Fatalf("%s: %v %s", name, err, raw)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s: %v %s", name, err, raw)
	}
}

// The completion criteria's routing and address checks, through a real
// reverse proxy that forwards the visitor's Host to the proxy listener.
func TestProxyEndToEnd(t *testing.T) {
	ctx := context.Background()
	path := proxyConfig(t, "proxy.addr", "127.0.0.1:0", "proxy.url_template", testProxyTemplate, "network.permitted", "proxy")
	o := configModeOptions(path)
	o.Overrides["host.server_runtime"] = "true"
	h, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	// The test binary runs the workers; nothing has started one yet.
	h.workers.Env = append([]string{"FLATS_CONTENT_TEST_WORKER=1"}, testImageEnv()...)
	h.workers.NoSpare = true
	listener := h.proxyBackend().(*proxy.Net).Addr()
	allow := func(slug string, id core.ProviderID) {
		t.Helper()
		if err := h.Svc.SetProviderPermission(ctx, slug, string(id), true, core.ViaConsole); err != nil {
			t.Fatalf("allow %s on %s: %v", id, slug, err)
		}
	}

	deploySite(t, h.Svc, "site", "<p>site content</p>")
	allow("site", core.ProviderProxy)
	deploySite(t, h.Svc, "hidden", "<p>hidden</p>")
	deploySite(t, h.Svc, "old-name", "<p>renamed content</p>")
	allow("old-name", core.ProviderProxy)
	if _, err := h.Svc.RenameSlug(ctx, "old-name", "new-name", core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Svc.SaveVersion(ctx, "notes", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte("# Notes\n")}}, core.SaveMeta{}, core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	allow("notes", core.ProviderProxy)
	deployBundle(t, h.Svc, "app", []bundle.File{{Path: "flats.json", Data: []byte(`{"kind":"server","entry":"server.js"}`)}, {Path: "server.js", Data: []byte(echoFlat)}})
	allow("app", core.ProviderProxy)
	preview, err := h.Svc.OpenPreview(ctx, "site", 0)
	if err != nil {
		t.Fatal(err)
	}

	tlsFront := newFront(t, listener, "set", true, false)
	t.Run("routes", func(t *testing.T) {
		for _, c := range []struct {
			name, url string
			code      int
			body      string
		}{
			{"current site", "https://site.site.example.org/", 200, "site content"},
			{"preview", "https://" + preview.Host + ".site.example.org/", 200, "site content"},
			{"ungranted flat", "https://hidden.site.example.org/", 404, ""},
			{"unknown label", "https://nothing.site.example.org/", 404, ""},
			{"nested label", "https://a.site.site.example.org/", 404, ""},
			{"console name", "https://flats.site.example.org/api/status", 404, ""},
			{"metrics", "https://nothing.site.example.org/metrics", 404, ""},
			{"other domain", "https://site.example.org/", 404, ""},
		} {
			code, body, _ := tlsFront.do(t, "GET", c.url, "")
			host, _ := url.Parse(c.url)
			if code != c.code || !strings.Contains(body, c.body) || (code == 404 && strings.Contains(body, host.Hostname())) {
				t.Errorf("%s: %d %.200q", c.name, code, body)
			}
		}
		// The console and metrics are not served there under the console's own Host either.
		for _, p := range []string{"/api/status", "/metrics", "/"} {
			if code, body, _ := tlsFront.do(t, "GET", "https://site.site.example.org"+p, "", "Host", h.Addr()); code != 404 || strings.Contains(body, h.Addr()) {
				t.Errorf("console Host %s: %d %.200q", p, code, body)
			}
		}
		code, _, hdr := tlsFront.do(t, "GET", "https://old-name.site.example.org/a?b=1", "")
		if code != http.StatusTemporaryRedirect || hdr.Get("Location") != "https://new-name.site.example.org/a?b=1" {
			t.Errorf("rename redirect: %d %q", code, hdr.Get("Location"))
		}
		if code, body, _ := tlsFront.do(t, "GET", "https://new-name.site.example.org/", ""); code != 200 || !strings.Contains(body, "renamed content") {
			t.Errorf("renamed flat: %d %.200q", code, body)
		}
	})

	t.Run("addresses", func(t *testing.T) {
		session, err := mcp.NewClient(&mcp.Implementation{Name: "proxy-e2e", Version: "1"}, nil).
			Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + h.Addr() + "/mcp"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		var got struct {
			Flat struct {
				PrivateURL    string                  `json:"private_url"`
				PrivateDetail string                  `json:"private_detail"`
				Endpoints     []core.ExposureEndpoint `json:"endpoints"`
			} `json:"flat"`
		}
		callTool(t, session, "get_flat", map[string]any{"slug": "site"}, &got)
		if got.Flat.PrivateURL != "https://site.site.example.org" {
			t.Errorf("get_flat private_url %q", got.Flat.PrivateURL)
		}
		urls := map[core.ProviderID]string{}
		for _, ep := range got.Flat.Endpoints {
			if ep.Host == "site" && ep.Audience == core.AudienceCurrent {
				urls[ep.Provider] = ep.URL
				if ep.Provider == core.ProviderProxy && (ep.State != "ready" || ep.Detail != proxy.Detail) {
					t.Errorf("proxy endpoint %+v", ep)
				}
			}
		}
		if urls[core.ProviderProxy] != "https://site.site.example.org" || !strings.Contains(urls[core.ProviderLocal], "site.localhost") {
			t.Errorf("get_flat endpoints %+v", got.Flat.Endpoints)
		}
		var list struct {
			Flats []struct {
				Slug       string `json:"slug"`
				PrivateURL string `json:"private_url"`
			} `json:"flats"`
		}
		callTool(t, session, "list_flats", map[string]any{}, &list)
		want := map[string]string{"site": "https://site.site.example.org", "new-name": "https://new-name.site.example.org",
			"notes": "https://notes.site.example.org", "app": "https://app.site.example.org"}
		for _, f := range list.Flats {
			if w, ok := want[f.Slug]; ok && f.PrivateURL != w {
				t.Errorf("list_flats %s: %q", f.Slug, f.PrivateURL)
			}
			if f.Slug == "hidden" && !strings.Contains(f.PrivateURL, ".localhost") {
				t.Errorf("list_flats hidden: %q", f.PrivateURL)
			}
		}
		var p struct {
			Host string `json:"host"`
			URL  string `json:"url"`
		}
		callTool(t, session, "open_preview", map[string]any{"slug": "site", "target": "draft", "version": 0}, &p)
		if p.URL != "https://"+p.Host+".site.example.org" {
			t.Errorf("open_preview url %q", p.URL)
		}
		if code, _, _ := tlsFront.do(t, "GET", p.URL+"/", ""); code != 200 {
			t.Errorf("open_preview address: %d", code)
		}
	})

	revisions, err := h.Svc.ListRevisions(ctx, "notes", 0, 10)
	if err != nil || len(revisions.Revisions) == 0 {
		t.Fatalf("revisions: %+v %v", revisions, err)
	}
	rev := revisions.Revisions[0].Number
	// Each call inserts its own text from a new Yjs client.
	yjsClient := uint64(300)
	markdown := func(t *testing.T) string {
		t.Helper()
		d, err := h.Svc.GetDocument(ctx, "notes", "index.md")
		if err != nil {
			t.Fatal(err)
		}
		return d.Markdown
	}
	edits := func(t *testing.T, f *front, tlsFront bool, origin, text string) {
		t.Helper()
		m, closed := f.docsSession(t, "notes.site.example.org", origin, tlsFront, "")
		if m["t"] != "welcome" || m["readonly"] != false {
			t.Errorf("editor session with Origin %s: %v %s", origin, m, closed)
		}
		// A text edit through the proxy is acknowledged and saved.
		yjsClient++
		if m, closed := f.docsSession(t, "notes.site.example.org", origin, tlsFront, yjsInsert(yjsClient, text+"\n")); m["t"] != "ack" {
			t.Errorf("edit with Origin %s: %v %s", origin, m, closed)
		}
		if got := markdown(t); !strings.Contains(got, text) {
			t.Errorf("edit with Origin %s not saved: %q", origin, got)
		}
		base := "https://notes.site.example.org"
		if !tlsFront {
			base = "http://notes.site.example.org"
		}
		for _, pinned := range []bool{true, false} {
			if code := f.pin(t, base, origin, rev, pinned); code != 200 {
				t.Errorf("pin %t with Origin %s: %d", pinned, origin, code)
			}
		}
		// The check stays: a different origin is refused.
		wrong := strings.Replace(origin, "https://", "http://", 1)
		yjsClient++
		if _, closed := f.docsSession(t, "notes.site.example.org", wrong, tlsFront, yjsInsert(yjsClient, "refused "+text+"\n")); !strings.Contains(closed, "cross-origin") {
			t.Errorf("editor session with Origin %s: %s", wrong, closed)
		}
		if got := markdown(t); strings.Contains(got, "refused "+text) {
			t.Errorf("edit with Origin %s saved: %q", wrong, got)
		}
		if code := f.pin(t, base, wrong, rev, true); code != 403 {
			t.Errorf("pin with Origin %s: %d", wrong, code)
		}
	}
	echo := func(t *testing.T, f *front, base string) map[string]any {
		t.Helper()
		code, body, _ := f.do(t, "GET", base+"/hello", "", "X-Forwarded-Proto", "http", "Tailscale-User-Login", "someone@example.org")
		var out map[string]any
		if code != 200 || json.Unmarshal([]byte(body), &out) != nil {
			t.Fatalf("server flat: %d %.300s", code, body)
		}
		return out
	}

	t.Run("TLS proxy with X-Forwarded-Proto", func(t *testing.T) {
		edits(t, tlsFront, true, "https://notes.site.example.org", "Edited through a TLS proxy that sets X-Forwarded-Proto")
	})
	t.Run("TLS proxy without X-Forwarded-Proto", func(t *testing.T) {
		edits(t, newFront(t, listener, "none", true, false), true, "https://notes.site.example.org", "Edited through a TLS proxy without X-Forwarded-Proto")
	})
	t.Run("server flat", func(t *testing.T) {
		out := echo(t, newFront(t, listener, "client", true, false), "https://app.site.example.org")
		if out["proto"] != "https" || out["url"] != "https://app.site.example.org/hello" || out["identity"] != nil {
			t.Errorf("server flat saw %v", out)
		}
	})
	t.Run("template port dropped by the proxy", func(t *testing.T) {
		if _, err := h.Svc.UpdateSettings(ctx, map[string]string{core.SetProxyTemplate: "https://{host}.site.example.org:8443"}); err != nil {
			t.Fatal(err)
		}
		defer h.Svc.UpdateSettings(ctx, map[string]string{core.SetProxyTemplate: testProxyTemplate})
		portFront := newFront(t, listener, "none", true, true)
		edits(t, portFront, true, "https://notes.site.example.org:8443", "Edited through a proxy that drops port 8443")
		if out := echo(t, portFront, "https://app.site.example.org"); out["url"] != "https://app.site.example.org:8443/hello" || out["proto"] != "https" {
			t.Errorf("server flat saw %v", out)
		}
		if f, _ := h.Svc.GetFlat(ctx, "site"); f.PrivateURL != "https://site.site.example.org:8443" {
			t.Errorf("private_url %q", f.PrivateURL)
		}
	})
	t.Run("template change", func(t *testing.T) {
		if _, err := h.Svc.UpdateSettings(ctx, map[string]string{core.SetProxyTemplate: "https://{host}.other.example.org"}); err != nil {
			t.Fatal(err)
		}
		defer h.Svc.UpdateSettings(ctx, map[string]string{core.SetProxyTemplate: testProxyTemplate})
		if code, _, _ := tlsFront.do(t, "GET", "https://site.site.example.org/", ""); code != 404 {
			t.Errorf("old name: %d", code)
		}
		if code, body, _ := tlsFront.do(t, "GET", "https://site.other.example.org/", ""); code != 200 || !strings.Contains(body, "site content") {
			t.Errorf("new name: %d %.200q", code, body)
		}
	})

	// Switch to proxy-public: no flat may allow proxy while it is turned off.
	client := &http.Client{}
	for _, slug := range []string{"site", "new-name", "notes", "app"} {
		if err := h.Svc.SetProviderPermission(ctx, slug, string(core.ProviderProxy), false, core.ViaConsole); err != nil {
			t.Fatalf("revoke %s: %v", slug, err)
		}
	}
	if code, out := setHost(t, h, client, "proxy", false); code != 200 {
		t.Fatalf("turn off proxy: %d %v", code, out)
	}
	if code, out := setHost(t, h, client, "proxy-public", true); code != 200 {
		t.Fatalf("turn on proxy-public: %d %v", code, out)
	}
	listener = h.proxyBackend().(*proxy.Net).Addr()
	public := newFront(t, listener, "set", true, false)
	allow("site", core.ProviderProxyPublic)
	allow("hidden", core.ProviderProxyPublic)
	if code, _, _ := public.do(t, "GET", "https://site.site.example.org/", ""); code != 404 {
		t.Fatalf("Private flat served on proxy-public: %d", code)
	}
	r, err := h.Svc.SetVisibility(ctx, "site", store.Public, core.ViaConsole, "")
	if err != nil || r.Approval == nil {
		t.Fatalf("visibility: %v %+v", err, r)
	}
	if _, err := h.Svc.Decide(ctx, r.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := public.do(t, "GET", "https://site.site.example.org/", ""); code != 200 || !strings.Contains(body, "site content") {
		t.Fatalf("Public flat on proxy-public: %d %.200q", code, body)
	}
	f, err := h.Svc.GetFlat(ctx, "site")
	if err != nil || f.PublicURL != "https://site.site.example.org" || f.ConnectionState != "ready" {
		t.Fatalf("public_url %q (%s) %v", f.PublicURL, f.ConnectionState, err)
	}
	if i := slices.IndexFunc(f.Endpoints, func(ep core.ExposureEndpoint) bool { return ep.Provider == core.ProviderProxyPublic }); i < 0 || f.Endpoints[i].Detail != proxy.Detail {
		t.Fatalf("endpoints %+v", f.Endpoints)
	}
	pv, err := h.Svc.OpenPreview(ctx, "site", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{pv.Host, "hidden", "notes"} {
		if code, _, _ := public.do(t, "GET", "https://"+label+".site.example.org/", ""); code != 404 {
			t.Errorf("%s served on proxy-public: %d", label, code)
		}
	}
}
