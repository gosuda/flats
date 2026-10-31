package core_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/apps/docs"
	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/core"
	flatsruntime "github.com/gosuda/flats/internal/runtime"
	"github.com/gosuda/flats/internal/store"
	"github.com/gosuda/flats/internal/wsconn"
)

// The runtime re-executes this binary, as in the built-in docs app tests.
func TestMain(m *testing.M) {
	if os.Getenv("FLATS_PRIVATE_LINK_TEST_WORKER") == "1" {
		if err := flatsruntime.WorkerMain(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type linkRegistration struct {
	name   string
	h      http.Handler
	public bool
}

type linkNetwork struct {
	*fixtureNodes
	recordMu sync.Mutex
	legacy   []linkRegistration
	requests []core.ExposureRequest
}

func newLinkNetwork() *linkNetwork {
	return &linkNetwork{fixtureNodes: newFixtureNodes("links.test")}
}

func (n *linkNetwork) Serve(ctx context.Context, host string, h http.Handler, ephemeral bool) (string, error) {
	n.recordMu.Lock()
	n.legacy = append(n.legacy, linkRegistration{name: host, h: h})
	n.recordMu.Unlock()
	return n.fixtureNodes.Serve(ctx, host, h, ephemeral)
}

func (n *linkNetwork) SetHidden(string, bool) error { return nil }

func (n *linkNetwork) ServeExposure(_ context.Context, r core.ExposureRequest) (core.ExposureResult, error) {
	n.recordMu.Lock()
	n.requests = append(n.requests, r)
	n.recordMu.Unlock()
	eps := []core.ExposureEndpoint{routeLinkEndpoint(core.ProviderLocal, r.Host, r.Audience)}
	if r.Visibility == "public" {
		eps = append(eps, routeLinkEndpoint(core.ProviderPortal, r.Host, r.Audience))
	}
	return core.ExposureResult{Endpoints: eps}, nil
}

func routeLinkEndpoint(p core.ProviderID, host string, audience core.ExposureAudience) core.ExposureEndpoint {
	return core.ExposureEndpoint{Provider: p, URL: "https://" + host + "." + string(p) + ".test",
		State: "ready", Configured: true, Permitted: true, Ready: true, Host: host, Audience: audience}
}

func (n *linkNetwork) ExposurePolicy(context.Context) (string, error) { return "configured", nil }
func (n *linkNetwork) ExposureStatus(_ context.Context, host string) (core.ExposureResult, error) {
	return core.ExposureResult{Endpoints: []core.ExposureEndpoint{
		routeLinkEndpoint(core.ProviderLocal, host, core.AudienceCurrent),
		routeLinkEndpoint(core.ProviderPortal, host, core.AudienceCurrent),
	}}, nil
}
func (n *linkNetwork) StopPublicRoutes(context.Context, string) (core.PublicStopResult, error) {
	return core.PublicStopResult{Stopped: []core.ProviderID{core.ProviderPortal}}, nil
}
func (n *linkNetwork) StopExposure(context.Context, string) error { return nil }

func linkMode(t *testing.T, s *core.Service, on bool) {
	t.Helper()
	status, err := s.SetPublicInstance(t.Context(), on, "portal", "tester")
	if err != nil || status.Enabled != on {
		t.Fatalf("SetPublicInstance(%t) = %+v, %v", on, status, err)
	}
}

func configureLinkMode(t *testing.T, s *core.Service) {
	t.Helper()
	if _, err := s.SetPermittedNetworks([]string{"portal"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSettings(t.Context(), map[string]string{core.SetDefaultPublic: "portal", core.SetRateLimit: "10000"}); err != nil {
		t.Fatal(err)
	}
}

func newLinkService(t *testing.T, lifecycle bool, now func() time.Time, runtime core.Runtime) (*core.Service, *linkNetwork, *linkNetwork) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	priv, pub := newLinkNetwork(), newLinkNetwork()
	cfg := core.Config{DataDir: dir, Store: st, Private: priv, Public: pub, Now: now, Logf: t.Logf,
		Runtime: runtime, DocsApp: core.DocsApp{FS: docs.FS(), Hash: docs.Hash(), Entry: docs.Entry, ContentModule: docs.ContentModule}}
	if lifecycle {
		cfg.Lifecycle = priv
	}
	s, err := core.New(t.Context(), cfg)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); st.Close() })
	configureLinkMode(t, s)
	return s, priv, pub
}

func publicLinkFlat(t *testing.T, s *core.Service, slug string, edit bool) {
	t.Helper()
	permitPortal(t, s, slug)
	access := store.AccessView
	if edit {
		access = store.AccessEdit
	}
	r, err := s.SetVisibilityAccess(t.Context(), slug, store.Public, access, core.ViaConsole, "")
	if err != nil || r.Approval == nil {
		t.Fatalf("make %s Public: %+v %v", slug, r, err)
	}
	if _, err := s.Decide(t.Context(), r.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
}

func TestPublicInstanceEveryRegisteredLink(t *testing.T) {
	for _, lifecycle := range []bool{false, true} {
		name := "legacy"
		if lifecycle {
			name = "lifecycle"
		}
		t.Run(name, func(t *testing.T) {
			s, priv, pub := newLinkService(t, lifecycle, nil, nil)
			for _, slug := range []string{"site", "renamed"} {
				if _, err := s.SaveVersion(t.Context(), slug, files("index.html", "private-link-content"), core.SaveMeta{}, core.ViaAPI); err != nil {
					t.Fatal(err)
				}
				publish(t, s, slug)
				publicLinkFlat(t, s, slug, false)
			}
			if _, err := s.RenameSlug(t.Context(), "renamed", "moved", core.ViaConsole); err != nil {
				t.Fatal(err)
			}
			for _, version := range []int{0, 1} {
				if _, err := s.OpenPreview(t.Context(), "site", version); err != nil {
					t.Fatal(err)
				}
			}
			var registrations []linkRegistration
			if lifecycle {
				priv.recordMu.Lock()
				for i, r := range priv.requests {
					registrations = append(registrations, linkRegistration{name: fmt.Sprintf("%d/%s/%s/handler", i, r.Host, r.Audience), h: r.Handler})
					if r.PrivateHandler != nil {
						registrations = append(registrations, linkRegistration{name: fmt.Sprintf("%d/%s/private-handler", i, r.Host), h: r.PrivateHandler})
					}
				}
				priv.recordMu.Unlock()
			} else {
				priv.recordMu.Lock()
				registrations = append(registrations, priv.legacy...)
				priv.recordMu.Unlock()
				pub.recordMu.Lock()
				for _, r := range pub.legacy {
					r.public = true
					registrations = append(registrations, r)
				}
				pub.recordMu.Unlock()
			}
			if len(registrations) == 0 {
				t.Fatal("no registered handlers")
			}
			providers := []core.ProviderID{""}
			if lifecycle {
				providers = []core.ProviderID{core.ProviderLocal, core.ProviderTailscale, core.ProviderNetbird, core.ProviderProxy,
					core.ProviderFunnel, core.ProviderPortal, core.ProviderZrok, core.ProviderCloudflareQuick, core.ProviderNetbirdExpose, core.ProviderProxyPublic}
			}
			type probe struct {
				r        linkRegistration
				provider core.ProviderID
				before   *httptest.ResponseRecorder
			}
			request := func(r linkRegistration, p core.ProviderID) *httptest.ResponseRecorder {
				req := httptest.NewRequest("GET", "/?keep=query", nil)
				if p != "" {
					req = req.WithContext(core.WithRouteProvider(req.Context(), p))
				}
				rec := httptest.NewRecorder()
				r.h.ServeHTTP(rec, req)
				return rec
			}
			var probes []probe
			for _, r := range registrations {
				for _, p := range providers {
					before := request(r, p)
					if before.Code == http.StatusForbidden {
						t.Fatalf("off: %s/%s unexpectedly forbidden: %s", r.name, p, before.Body.String())
					}
					probes = append(probes, probe{r, p, before})
				}
			}
			for _, on := range []bool{true, false} {
				linkMode(t, s, on)
				for _, p := range probes {
					got := request(p.r, p.provider)
					public := p.r.public
					if lifecycle {
						public = p.provider == core.ProviderFunnel || p.provider == core.ProviderPortal || p.provider == core.ProviderZrok ||
							p.provider == core.ProviderCloudflareQuick || p.provider == core.ProviderNetbirdExpose || p.provider == core.ProviderProxyPublic
					}
					if on && !public {
						if got.Code != http.StatusForbidden || got.Header().Get("Cache-Control") != "no-store" ||
							!strings.Contains(got.Body.String(), "Private links are turned off on this host.") || strings.Contains(got.Body.String(), "private-link-content") || got.Header().Get("Location") != "" {
							t.Errorf("on: %s/%s = %d %v %q", p.r.name, p.provider, got.Code, got.Header(), got.Body.String())
						}
					} else if got.Code != p.before.Code || got.Body.String() != p.before.Body.String() || got.Header().Get("Location") != p.before.Header().Get("Location") {
						t.Errorf("on=%t: %s/%s = %d %q %q, want unchanged %d %q %q", on, p.r.name, p.provider,
							got.Code, got.Body.String(), got.Header().Get("Location"), p.before.Code, p.before.Body.String(), p.before.Header().Get("Location"))
					}
				}
			}
			t.Logf("checked %d registered handlers and %d provider/handler pairs through off/on/off", len(registrations), len(probes))
		})
	}
}

func TestPublicInstanceForbiddenVisitsDoNotExtendPreview(t *testing.T) {
	var clock atomic.Int64
	start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	clock.Store(start.UnixMilli())
	s, priv, _ := newLinkService(t, false, func() time.Time { return time.UnixMilli(clock.Load()) }, nil)
	if _, err := s.UpdateSettings(t.Context(), map[string]string{core.SetPreviewTTL: "60"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveVersion(t.Context(), "preview", files("index.html", "preview-content"), core.SaveMeta{}, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	pv, err := s.OpenPreview(t.Context(), "preview", 0)
	if err != nil {
		t.Fatal(err)
	}
	priv.mu.Lock()
	h := priv.handlers[pv.Host]
	priv.mu.Unlock()
	linkMode(t, s, true)
	for _, elapsed := range []time.Duration{30 * time.Second, 59 * time.Second} {
		clock.Store(start.Add(elapsed).UnixMilli())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("visit at %s = %d", elapsed, rec.Code)
		}
	}
	clock.Store(start.Add(61 * time.Second).UnixMilli())
	s.Sweep(t.Context())
	previews, err := s.ListPreviews(t.Context(), "preview")
	if err != nil || len(previews) != 0 {
		t.Fatalf("preview survived 403-only visits: %+v %v", previews, err)
	}
	priv.mu.Lock()
	_, serving := priv.handlers[pv.Host]
	priv.mu.Unlock()
	if serving {
		t.Fatal("expired preview route is still registered")
	}
}

type linkOriginConn struct {
	net.Conn
	origin string
	sent   bool
}

func (c *linkOriginConn) Write(p []byte) (int, error) {
	if !c.sent && bytes.HasSuffix(p, []byte("\r\n\r\n")) {
		c.sent = true
		q := append(bytes.Clone(p[:len(p)-2]), []byte("Origin: "+c.origin+"\r\n\r\n")...)
		if _, err := c.Conn.Write(q); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	return c.Conn.Write(p)
}

func dialLinkSocket(t *testing.T, base, path string, hello any) *wsconn.Conn {
	t.Helper()
	host := strings.TrimPrefix(base, "http://")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ws, err := wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp", host)
		if err != nil {
			return nil, err
		}
		return &linkOriginConn{Conn: c, origin: base}, nil
	}, host, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close(1000, "") })
	b, err := json.Marshal(hello)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFrame(wsconn.OpText, b); err != nil {
		t.Fatal(err)
	}
	linkSocketMessage(t, ws, "welcome")
	return ws
}

func linkSocketMessage(t *testing.T, ws *wsconn.Conn, kind string) {
	t.Helper()
	ws.NetConn().SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		raw, err := ws.ReadMessage(64 << 20)
		if err != nil {
			t.Fatalf("waiting for %s: %v", kind, err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		if m["t"] == kind {
			return
		}
	}
}

// The proxy normally closes upgraded sockets on request cancellation. Detach
// only WebSockets so it cannot mask the gate's transport-close obligation.
type detachedLinkRuntime struct{ *flatsruntime.Manager }

func (rt detachedLinkRuntime) Start(ctx context.Context, spec core.RuntimeSpec) (core.Instance, error) {
	in, err := rt.Manager.Start(ctx, spec)
	if err != nil || !spec.ManagedDocs {
		return in, err
	}
	return detachedLinkDocs{in.(core.ManagedDocsInstance)}, nil
}

type detachedLinkDocs struct{ core.ManagedDocsInstance }

func (in detachedLinkDocs) TryAcquire(foreground bool) (core.RuntimeLease, bool) {
	lease, ok := in.ManagedDocsInstance.TryAcquire(foreground)
	if !ok {
		return nil, false
	}
	return detachedLinkLease{lease}, true
}

func (in detachedLinkDocs) Wake(ctx context.Context, prepare func(context.Context, http.Handler) error) (core.RuntimeLease, error) {
	lease, err := in.ManagedDocsInstance.Wake(ctx, prepare)
	if err != nil {
		return nil, err
	}
	return detachedLinkLease{lease}, nil
}

type detachedLinkLease struct{ core.RuntimeLease }

func (lease detachedLinkLease) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if wsconn.IsUpgrade(r) {
		r = r.WithContext(context.WithoutCancel(r.Context()))
	}
	lease.RuntimeLease.ServeHTTP(w, r)
}

func TestPublicInstanceClosesPrivateStreamsOnly(t *testing.T) {
	for _, kind := range []string{"board", "docs", "design"} {
		t.Run(kind, func(t *testing.T) {
			var runtime core.Runtime
			if kind == "docs" {
				mgr := &flatsruntime.Manager{DataDir: t.TempDir(), Env: []string{"FLATS_PRIVATE_LINK_TEST_WORKER=1"}, NoSpare: true, Logf: t.Logf}
				t.Cleanup(func() { mgr.Close() })
				runtime = detachedLinkRuntime{mgr}
			}
			s, priv, pub := newLinkService(t, false, nil, runtime)
			switch kind {
			case "board":
				if _, err := s.CreateBoard(t.Context(), "stream", "Stream", core.ViaAPI); err != nil {
					t.Fatal(err)
				}
			case "docs":
				if _, err := s.SaveDocument(t.Context(), "stream", "index.md", "# Stream\n", "Stream", core.SaveMeta{}, core.ViaAPI); err != nil {
					t.Fatal(err)
				}
			case "design":
				if _, _, err := s.CreateDesign(t.Context(), "stream", "Stream", []core.DesignFile{
					{Path: "canvas.json", Data: []byte(`{"title":"Stream","boards":{"a.html":{"x":0,"y":0,"w":1280,"h":800}},"order":["a.html"]}`)},
					{Path: "a.html", Data: []byte("<h1>Stream</h1>")},
				}, "", core.ViaAPI); err != nil {
					t.Fatal(err)
				}
			}
			publicLinkFlat(t, s, "stream", kind == "docs")
			priv.mu.Lock()
			privateHandler := priv.handlers["stream"]
			priv.mu.Unlock()
			pub.mu.Lock()
			publicHandler := pub.handlers["stream"]
			pub.mu.Unlock()
			privateServer := httptest.NewServer(privateHandler)
			t.Cleanup(privateServer.Close)
			publicServer := httptest.NewServer(publicHandler)
			t.Cleanup(publicServer.Close)
			if kind == "design" {
				open := func(base string) (*http.Response, *bufio.Reader) {
					t.Helper()
					client := &http.Client{Timeout: 10 * time.Second}
					res, err := client.Get(base + "/_design/api/events")
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { res.Body.Close() })
					reader := bufio.NewReader(res.Body)
					line, err := reader.ReadString('\n')
					if err != nil || res.StatusCode != 200 || line != "event: hello\n" {
						t.Fatalf("SSE handshake = %d %q %v", res.StatusCode, line, err)
					}
					return res, reader
				}
				privateRes, privateReader := open(privateServer.URL)
				_, publicReader := open(publicServer.URL)
				linkMode(t, s, true)
				if _, err := io.Copy(io.Discard, privateReader); err != nil {
					t.Fatalf("private SSE did not end cleanly after Turn on: %v", err)
				}
				privateRes.Body.Close()
				if _, err := s.WriteDesign(t.Context(), "stream", core.DesignWrite{Files: []core.DesignFile{{Path: "b.html", Data: []byte("<h1>After</h1>")}}}, core.ViaAPI); err != nil {
					t.Fatal(err)
				}
				for {
					line, err := publicReader.ReadString('\n')
					if err != nil {
						t.Fatalf("public SSE ended after Turn on: %v", err)
					}
					if line == "event: change\n" {
						break
					}
				}
			} else {
				path := "/_board/ws"
				privateHello := map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}, "editor": "e1", "name": "Pat"}
				publicHello := privateHello
				if kind == "docs" {
					path = "/_docs/ws?doc=index.md"
					privateHello = map[string]any{"t": "hello", "v": 1, "doc": "index.md", "sv": "AA==", "name": "Pat"}
					publicHello = map[string]any{"t": "hello", "v": 1, "doc": "index.md", "sv": "AA==", "guest": map[string]any{"name": "Guest"}}
				}
				privateWS := dialLinkSocket(t, privateServer.URL, path, privateHello)
				publicWS := dialLinkSocket(t, publicServer.URL, path, publicHello)
				linkMode(t, s, true)
				privateWS.NetConn().SetReadDeadline(time.Now().Add(2 * time.Second))
				for {
					_, err := privateWS.ReadMessage(64 << 20)
					if err != nil {
						var ne net.Error
						if errors.As(err, &ne) && ne.Timeout() {
							t.Fatalf("private %s WebSocket still open after Turn on: %v", kind, err)
						}
						break
					}
				}
				if kind == "board" {
					var ops []engine.AgentOp
					if err := json.Unmarshal([]byte(`[{"op":"add","el":{"type":"sticky","text":"After Turn on"}}]`), &ops); err != nil {
						t.Fatal(err)
					}
					if _, err := s.UpdateBoard(t.Context(), "stream", core.BoardUpdate{OpID: "after-turn-on", Ops: ops}, core.ViaAPI); err != nil {
						t.Fatal(err)
					}
					linkSocketMessage(t, publicWS, "delta")
				} else {
					data, err := os.ReadFile("../apps/docs/testdata/updates.json")
					if err != nil {
						t.Fatal(err)
					}
					var updates struct{ Independent string }
					if err := json.Unmarshal(data, &updates); err != nil {
						t.Fatal(err)
					}
					b, err := json.Marshal(map[string]any{"t": "update", "id": "after-turn-on", "u": updates.Independent})
					if err != nil {
						t.Fatal(err)
					}
					if err := publicWS.WriteFrame(wsconn.OpText, b); err != nil {
						t.Fatal(err)
					}
					linkSocketMessage(t, publicWS, "ack")
					dialLinkSocket(t, publicServer.URL, path, publicHello)
				}
			}
			linkMode(t, s, false)
			if code, _, _ := get(t, privateServer.URL); code != 200 {
				t.Fatalf("private %s route not restored after Turn off: %d", kind, code)
			}
		})
	}
}
