package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/store"
)

const autopilotPath = "/console/api/experiments/autopilot"

// hostLog keeps the host log lines of a Service.
type hostLog struct {
	t     *testing.T
	mu    sync.Mutex
	lines []string
}

func (l *hostLog) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	l.t.Log(line)
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.mu.Unlock()
}

var changeTime = regexp.MustCompile(` \(at \d{4}-\d\d-\d\dT[^)]+\)$`)

// autopilot returns the Autopilot change lines, without their time suffix,
// which every one of them must have.
func (l *hostLog) autopilot() []string {
	l.t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		if !strings.HasPrefix(line, "autopilot: ") {
			continue
		}
		if !changeTime.MatchString(line) {
			l.t.Fatalf("change line without its time: %q", line)
		}
		out = append(out, changeTime.ReplaceAllString(line, ""))
	}
	return out
}

func setupAutopilot(t *testing.T) (*httptest.Server, *core.Service, *hostLog) {
	t.Helper()
	logs := &hostLog{t: t}
	srv, svc := setupWith(t, t.TempDir(), func(c *core.Config) { c.Logf = logs.logf })
	return srv, svc, logs
}

// send makes a request and returns the status and the raw body.
func send(t *testing.T, h http.Handler, method, url, body string, hdr map[string]string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, url, rd)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

type apView struct {
	Enabled   bool     `json:"enabled"`
	Allowed   []string `json:"allowed"`
	Guide     string   `json:"guide"`
	GuideMax  int      `json:"guide_max"`
	UpdatedAt string   `json:"updated_at"`
}

func putAutopilot(t *testing.T, srv *httptest.Server, path, body string) (int, map[string]any) {
	t.Helper()
	return req(t, "PUT", srv.URL+autopilotPath+path, strings.NewReader(body), consoleHdr(t, srv))
}

func getAutopilot(t *testing.T, srv *httptest.Server) apView {
	t.Helper()
	code, out := req(t, "GET", srv.URL+autopilotPath, nil, map[string]string{"X-Flats-Console": "1"})
	if code != 200 {
		t.Fatalf("get autopilot: %d %v", code, out)
	}
	return viewOf(t, out)
}

func viewOf(t *testing.T, out map[string]any) apView {
	t.Helper()
	b, _ := json.Marshal(out["autopilot"])
	var v apView
	if err := json.Unmarshal(b, &v); err != nil || out["autopilot"] == nil {
		t.Fatalf("no autopilot view in %v", out)
	}
	return v
}

func TestAutopilotConsoleRoutes(t *testing.T) {
	srv, svc, _ := setupAutopilot(t)

	code, out := req(t, "GET", srv.URL+autopilotPath, nil, map[string]string{"X-Flats-Console": "1"})
	b, _ := json.Marshal(out)
	if code != 200 || string(b) != `{"autopilot":{"allowed":[],"enabled":false,"guide":"","guide_max":4000}}` {
		t.Fatalf("default view: %d %s", code, b)
	}

	code, out = putAutopilot(t, srv, "/operations/delete_flat", `{"allowed":true}`)
	if v := viewOf(t, out); code != 200 || v.Enabled || !slices.Equal(v.Allowed, []string{"delete_flat"}) {
		t.Fatalf("allow delete_flat: %d %v", code, out)
	}
	code, out = putAutopilot(t, srv, "/operations/create_version", `{"allowed":true}`)
	v := viewOf(t, out)
	if code != 200 || !slices.Equal(v.Allowed, []string{"create_version", "delete_flat"}) {
		t.Fatalf("allowed is not in operation order: %d %v", code, out)
	}
	if at, err := time.Parse(time.RFC3339, v.UpdatedAt); err != nil || at.Location() != time.UTC {
		t.Fatalf("updated_at %q: %v", v.UpdatedAt, err)
	}

	code, out = putAutopilot(t, srv, "", `{"enabled":true,"confirmed_allowed":["delete_flat","create_version"]}`)
	if v := viewOf(t, out); code != 200 || !v.Enabled {
		t.Fatalf("turn on: %d %v", code, out)
	}
	code, out = putAutopilot(t, srv, "/guide", `{"guide":"Approve deploys of PR preview flats only."}`)
	if v := viewOf(t, out); code != 200 || v.Guide != "Approve deploys of PR preview flats only." || v.GuideMax != 4000 {
		t.Fatalf("guide: %d %v", code, out)
	}
	if got := svc.Autopilot(); !got.Enabled || got.Allowed.List()[1] != core.OpDeleteFlat || got.Guide == "" {
		t.Fatalf("service copy: %+v", got)
	}

	// Turning off keeps the operations and the guide (rule 8).
	code, out = putAutopilot(t, srv, "", `{"enabled":false}`)
	if v := viewOf(t, out); code != 200 || v.Enabled || len(v.Allowed) != 2 || v.Guide == "" {
		t.Fatalf("turn off: %d %v", code, out)
	}
	code, out = putAutopilot(t, srv, "/operations/create_version", `{"allowed":false}`)
	if v := viewOf(t, out); code != 200 || !slices.Equal(v.Allowed, []string{"delete_flat"}) {
		t.Fatalf("disallow: %d %v", code, out)
	}
	if v := getAutopilot(t, srv); v.Enabled || !slices.Equal(v.Allowed, []string{"delete_flat"}) || v.Guide == "" {
		t.Fatalf("stored view: %+v", v)
	}
}

// Turning on names the operations the confirmation dialog listed; when the
// stored ones differ, nothing changes (rule 10).
func TestAutopilotTurnOnNeedsTheStoredOperations(t *testing.T) {
	srv, _, logs := setupAutopilot(t)
	putAutopilot(t, srv, "/operations/deploy", `{"allowed":true}`)
	for _, confirmed := range []string{`[]`, `["make_public"]`, `["deploy","make_public"]`} {
		code, out := putAutopilot(t, srv, "", `{"enabled":true,"confirmed_allowed":`+confirmed+`}`)
		if code != 409 || out["category"] != "conflict" || !strings.Contains(fmt.Sprint(out["error"]), "turn Autopilot on again") {
			t.Fatalf("stale confirmation %s: %d %v", confirmed, code, out)
		}
		if v := getAutopilot(t, srv); v.Enabled || !slices.Equal(v.Allowed, []string{"deploy"}) {
			t.Fatalf("refused turn on changed the settings: %+v", v)
		}
	}
	if code, out := putAutopilot(t, srv, "", `{"enabled":true,"confirmed_allowed":["deploy","deploy"]}`); code != 200 || !viewOf(t, out).Enabled {
		t.Fatalf("turn on: %d %v", code, out)
	}
	if n := len(logs.autopilot()); n != 2 {
		t.Fatalf("want 2 change lines, got %v", logs.autopilot())
	}
}

func TestAutopilotRefusesInvalidRequests(t *testing.T) {
	srv, _, logs := setupAutopilot(t)
	putAutopilot(t, srv, "/operations/deploy", `{"allowed":true}`)
	before := getAutopilot(t, srv)
	lines := len(logs.autopilot())
	for _, tc := range []struct{ path, body string }{
		{"", ``},
		{"", `{}`},
		{"", `{"enabled":null}`},
		{"", `{"enabled":"yes"}`},
		{"", `{"enabled":1}`},
		{"", `{"enabled":true}`},
		{"", `{"enabled":true,"confirmed_allowed":null}`},
		{"", `{"enabled":true,"confirmed_allowed":"deploy"}`},
		{"", `{"enabled":true,"confirmed_allowed":[1]}`},
		{"", `{"enabled":true,"confirmed_allowed":["deploy","bogus"]}`},
		{"", `{"enabled":false,"confirmed_allowed":[]}`},
		{"", `{"enabled":false,"confirmed_allowed":null}`},
		{"", `{"enabled":false,"extra":1}`},
		{"", `[true]`},
		{"/operations/deploy", `{}`},
		{"/operations/deploy", `{"allowed":"false"}`},
		{"/operations/deploy", `{"allowed":0}`},
		{"/operations/deploy", `{"allowed":false,"op":"deploy"}`},
		{"/operations/bogus", `{"allowed":true}`},
		{"/operations/Deploy", `{"allowed":true}`},
		{"/guide", `{}`},
		{"/guide", `{"guide":null}`},
		{"/guide", `{"guide":5}`},
		{"/guide", `{"guide":["a"]}`},
		{"/guide", `{"guide":"` + strings.Repeat("😀", core.AutopilotGuideMax+1) + `"}`},
		{"/guide", `{"guide":"` + strings.Repeat("a", core.AutopilotGuideMax+1) + `"}`},
	} {
		code, out := putAutopilot(t, srv, tc.path, tc.body)
		if code != 400 || out["category"] != "invalid" {
			t.Errorf("PUT %q %.80s: %d %v", tc.path, tc.body, code, out)
		}
	}
	if after := getAutopilot(t, srv); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("refused requests changed the settings: %+v, was %+v", after, before)
	}
	if got := logs.autopilot(); len(got) != lines {
		t.Fatalf("refused requests were logged: %v", got)
	}

	// The limit counts code points, so 4,000 emoji fit.
	full := strings.Repeat("😀", core.AutopilotGuideMax)
	body, _ := json.Marshal(map[string]string{"guide": full})
	if code, out := putAutopilot(t, srv, "/guide", string(body)); code != 200 || viewOf(t, out).Guide != full {
		t.Fatalf("guide at the limit: %d", code)
	}
}

// The routes exist only under /console/api and refuse what is not a console
// request, as the console's other settings do (rule 3).
func TestAutopilotConsoleOnly(t *testing.T) {
	srv, _, logs := setupAutopilot(t)
	puts := map[string]string{
		"":                        `{"enabled":true,"confirmed_allowed":[]}`,
		"/operations/delete_flat": `{"allowed":true}`,
		"/guide":                  `{"guide":"x"}`,
	}
	for path, body := range puts {
		for _, hdr := range []map[string]string{nil, consoleHdr(t, srv), {"X-Flats-Client": "cli", "Content-Type": "application/json"}} {
			if code, out := req(t, "PUT", srv.URL+"/api/experiments/autopilot"+path, strings.NewReader(body), hdr); code != 404 {
				t.Errorf("PUT /api/experiments/autopilot%s = %d %v", path, code, out)
			}
		}
	}
	if code, _ := req(t, "GET", srv.URL+"/api/experiments/autopilot", nil, nil); code != 404 {
		t.Errorf("GET /api/experiments/autopilot = %d", code)
	}

	with := func(k, v string) map[string]string {
		h := consoleHdr(t, srv)
		h[k] = v
		return h
	}
	refused := map[string]map[string]string{
		"no console header": with("X-Flats-Console", ""),
		"cli":               with("X-Flats-Client", "cli"),
		"other client":      with("X-Flats-Client", "agent"),
		"cross-site":        {"X-Flats-Console": "1", "Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example", "Content-Type": "application/json"},
		"foreign origin":    with("Origin", "http://attacker.example:7878"),
		"no origin":         with("Origin", ""),
		"no fetch site":     with("Sec-Fetch-Site", ""),
		"same-site":         with("Sec-Fetch-Site", "same-site"),
	}
	for name, hdr := range refused {
		for path, body := range puts {
			if code, out := req(t, "PUT", srv.URL+autopilotPath+path, strings.NewReader(body), hdr); code != 403 {
				t.Errorf("%s: PUT %s = %d %v", name, path, code, out)
			}
		}
	}
	for name, hdr := range map[string]map[string]string{
		"no console header": {"X-Flats-Client": ""},
		"cli":               {"X-Flats-Console": "1", "X-Flats-Client": "cli"},
		"foreign origin":    {"X-Flats-Console": "1", "Origin": "https://evil.example"},
	} {
		if code, out := req(t, "GET", srv.URL+autopilotPath, nil, hdr); code != 403 {
			t.Errorf("%s: GET = %d %v", name, code, out)
		}
	}
	if v := getAutopilot(t, srv); v.Enabled || len(v.Allowed) != 0 || v.Guide != "" {
		t.Fatalf("a refused request changed the settings: %+v", v)
	}
	if got := logs.autopilot(); len(got) != 0 {
		t.Fatalf("refused requests were logged: %v", got)
	}
}

// Each change is logged with who made it; the guide's text never is
// (rule 4). Calls that change nothing log nothing.
func TestAutopilotHostLog(t *testing.T) {
	srv, _, logs := setupAutopilot(t)
	const secret = "approve only flats named pr-preview-1234"
	const loopback = "the console on the loopback listener (no tailnet identity)"
	steps := []struct{ path, body string }{
		{"", `{"enabled":false}`}, // already off
		{"", `{"enabled":true,"confirmed_allowed":[]}`},
		{"", `{"enabled":true,"confirmed_allowed":[]}`}, // already on
		{"/operations/make_public", `{"allowed":true}`},
		{"/operations/make_public", `{"allowed":true}`}, // already allowed
		{"/guide", `{"guide":"` + secret + `"}`},
		{"/guide", `{"guide":"` + secret + `"}`}, // same text
		{"/operations/make_public", `{"allowed":false}`},
		{"", `{"enabled":false}`},
		{"/guide", `{"guide":""}`},
	}
	for _, s := range steps {
		if code, out := putAutopilot(t, srv, s.path, s.body); code != 200 {
			t.Fatalf("PUT %q %s: %d %v", s.path, s.body, code, out)
		}
	}
	want := []string{
		"autopilot: turned on by " + loopback + "; agents may decide: nothing",
		"autopilot: agents may now decide make_public; changed by " + loopback,
		fmt.Sprintf("autopilot: operation guide changed by %s (%d of 4000 characters)", loopback, len(secret)),
		"autopilot: agents may no longer decide make_public; changed by " + loopback,
		"autopilot: turned off by " + loopback,
		"autopilot: operation guide changed by " + loopback + " (0 of 4000 characters)",
	}
	if got := logs.autopilot(); !slices.Equal(got, want) {
		t.Fatalf("host log:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	logs.mu.Lock()
	defer logs.mu.Unlock()
	for _, line := range logs.lines {
		if strings.Contains(line, "pr-preview-1234") {
			t.Fatalf("the guide text reached the host log: %q", line)
		}
	}
}

// The host log names the tailnet user, the NetBird peer, the external
// origin or the console listener's remote caller a change came through.
func TestAutopilotChangedBy(t *testing.T) {
	srv, svc, logs := setupAutopilot(t)
	handler := (&api.Server{Svc: svc}).Handler()
	consoleHeaders := func(origin string) map[string]string {
		return map[string]string{"X-Flats-Console": "1", "Sec-Fetch-Site": "same-origin", "Origin": origin, "Content-Type": "application/json"}
	}
	allow := func(h http.Handler, base string, hdr map[string]string, op string) {
		t.Helper()
		if code, body := send(t, h, "PUT", base+autopilotPath+"/operations/"+op, `{"allowed":true}`, hdr); code != 200 {
			t.Fatalf("allow %s through %s: %d %s", op, base, code, body)
		}
	}

	tail := api.TailnetIdentity(handler)
	hdr := consoleHeaders("http://console.tailnet.ts.net")
	hdr["Tailscale-User-Login"] = "alice@example.com"
	allow(tail, "http://console.tailnet.ts.net", hdr, "create_version")
	// A login cannot add a line of its own to the host log.
	hdr["Tailscale-User-Login"] = "=?utf-8?q?mallory=0Aautopilot:_turned_off?="
	allow(tail, "http://console.tailnet.ts.net", hdr, "delete_version")

	nb := api.WithApprover(func(*http.Request) string { return "netbird:laptop.netbird.cloud" }, handler)
	allow(api.PlainHTTPConsole(nb), "http://admin.netbird.cloud", map[string]string{"X-Flats-Console": "1", "Origin": "http://admin.netbird.cloud", "Content-Type": "application/json"}, "deploy")

	external := &api.HostGuard{
		Hosts:           func() []string { return []string{"localhost"} },
		Ports:           []string{"7878"},
		ExternalOrigins: func() []string { return []string{"https://flats.example.org"} },
		Next:            handler,
	}
	allow(external, "https://flats.example.org", consoleHeaders("https://flats.example.org"), "rollback")

	// A caller on another machine reaching a non-loopback console listener;
	// a forwarding header does not change where the connection came from.
	remote := consoleHeaders("http://console.example")
	remote["X-Forwarded-For"] = "127.0.0.1"
	allow(handler, "http://console.example", remote, "make_private")

	if code, out := putAutopilot(t, srv, "/operations/make_public", `{"allowed":true}`); code != 200 {
		t.Fatalf("loopback: %d %v", code, out)
	}

	want := []string{
		`autopilot: agents may now decide create_version; changed by tailnet user "alice@example.com"`,
		`autopilot: agents may now decide delete_version; changed by tailnet user "mallory\nautopilot: turned off"`,
		`autopilot: agents may now decide deploy; changed by NetBird peer "laptop.netbird.cloud"`,
		`autopilot: agents may now decide rollback; changed by the console via external origin https://flats.example.org (no authenticated identity)`,
		`autopilot: agents may now decide make_private; changed by the console listener from 192.0.2.1 (no authenticated identity)`,
		`autopilot: agents may now decide make_public; changed by the console on the loopback listener (no tailnet identity)`,
	}
	if got := logs.autopilot(); !slices.Equal(got, want) {
		t.Fatalf("host log:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The console shows an error, not Autopilot off, when the stored settings
// cannot be read.
func TestAutopilotUnreadableStore(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	priv, _ := local.Listen("127.0.0.1:0")
	t.Cleanup(func() { priv.Close() })
	svc, err := core.New(context.Background(), core.Config{DataDir: dir, Store: st, Private: priv, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&api.Server{Svc: svc}).Handler())
	t.Cleanup(func() { srv.Close(); svc.Close() })
	if code, out := putAutopilot(t, srv, "", `{"enabled":true,"confirmed_allowed":[]}`); code != 200 {
		t.Fatalf("turn on: %d %v", code, out)
	}
	st.Close()
	code, out := req(t, "GET", srv.URL+autopilotPath, nil, map[string]string{"X-Flats-Console": "1"})
	if code != 500 || out["autopilot"] != nil || !strings.Contains(fmt.Sprint(out["error"]), "read the Autopilot settings") {
		t.Fatalf("unreadable store: %d %v", code, out)
	}
	if code, out := putAutopilot(t, srv, "", `{"enabled":false}`); code != 500 || out["autopilot"] != nil {
		t.Fatalf("turn off on an unreadable store: %d %v", code, out)
	}
}
