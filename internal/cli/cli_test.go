package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/launchd"
)

// fakeAPI serves canned replies keyed by "METHOD /path" and records requests.
type fakeAPI struct {
	t      *testing.T
	mu     sync.Mutex
	routes map[string]func(w http.ResponseWriter, r *http.Request, body []byte)
	reqs   []recorded
}

type recorded struct {
	method, path, query, client string
	body                        []byte
}

func newFakeAPI(t *testing.T) (*fakeAPI, *httptest.Server) {
	f := &fakeAPI{t: t, routes: map[string]func(http.ResponseWriter, *http.Request, []byte){}}
	f.handle("GET /api/status", 200, `{"ok": true, "system": {"tailnet": "example.ts.net"}, "upload_max_bytes": 20000000}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("X-Flats-Client"), body})
		h := f.routes[r.Method+" "+r.URL.Path]
		f.mu.Unlock()
		if h == nil {
			w.WriteHeader(404)
			io.WriteString(w, `{"error": "not found"}`)
			return
		}
		h(w, r, body)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeAPI) handle(route string, code int, body string) {
	f.routes[route] = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		io.WriteString(w, body)
	}
}

func (f *fakeAPI) last(route string) recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.reqs) - 1; i >= 0; i-- {
		if f.reqs[i].method+" "+f.reqs[i].path == route {
			return f.reqs[i]
		}
	}
	f.t.Fatalf("no request for %s", route)
	return recorded{}
}

type result struct {
	code           int
	stdout, stderr string
}

func run(t *testing.T, srvURL string, stdin string, args ...string) result {
	t.Helper()
	return runEnv(t, Env{Stdin: strings.NewReader(stdin)}, srvURL, args...)
}

func runEnv(t *testing.T, env Env, srvURL string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	env.Stdout, env.Stderr = &out, &errb
	// Never reach the real launchd or the real ~/Library/LaunchAgents
	// (`status` queries launchd on macOS).
	if env.Launchd == nil {
		env.Launchd = (&fakeLaunchctl{}).run
	}
	if env.Home == "" {
		env.Home = t.TempDir()
	}
	env.Getenv = func(k string) string {
		if k == "FLATS_URL" {
			return srvURL
		}
		return ""
	}
	code := Run(context.Background(), args, env)
	return result{code, out.String(), errb.String()}
}

const deployOK = `{
  "version": {"flat": "blog", "number": 4, "hash": "abc", "size": 2048, "files": 3, "kind": "static", "git_sha": "0123456789abcdef", "git_dirty": true, "created_at": "2026-10-03T10:00:00Z", "pruned": false},
  "deploy": {
    "flat": {"slug": "blog", "name": "Blog", "visibility": "public-unlisted", "live_version": 4,
             "private_url": "https://blog.tail1234.ts.net", "public_url": "https://blog.portal.example",
             "public_notice": "Unlisted only hides this flat from Portal relay listings. It is NOT access control: anyone with the URL can open it.",
             "versions": 4, "disk_bytes": 9000},
    "version": 4, "previous": 3,
    "health": {"path": "/", "status": 200, "ok": true, "millis": 12},
    "millis": 340
  }
}`

func TestDeploySuccess(t *testing.T) {
	api, srv := newFakeAPI(t)
	api.handle("POST /api/flats/blog/versions", 201, deployOK)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"index.html": "<h1>x</h1>", "css/site.css": "a{}", ".DS_Store": "x", "node_modules/m.js": "x"})

	// Flags after the positional directory must still parse.
	r := run(t, srv.URL, "", "deploy", dir, "--flat", "blog", "--message", "first try")
	if r.code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	req := api.last("POST /api/flats/blog/versions")
	if req.client != "cli" {
		t.Errorf("X-Flats-Client = %q", req.client)
	}
	if !strings.Contains(req.query, "deploy=1") || !strings.Contains(req.query, "message=first+try") {
		t.Errorf("query = %q", req.query)
	}
	if names := keys(untar(t, req.body)); strings.Join(names, ",") != "css/site.css,index.html" {
		t.Errorf("uploaded %v", names)
	}
	for _, want := range []string{
		"Saved version 4 of blog: 3 files, 2.0 kB, git 0123456 (dirty)",
		"Version 4 is live (was 3).",
		"health: GET / -> 200 in 12ms (ok)",
		"private: https://blog.tail1234.ts.net",
		"public:  https://blog.portal.example",
		"NOT access control",
		"Took ",
		"deploy 340ms",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, r.stdout)
		}
	}
}

func TestDeploySaveOnlyAndJSON(t *testing.T) {
	api, srv := newFakeAPI(t)
	api.handle("POST /api/flats/blog/versions", 201, `{"version": {"flat": "blog", "number": 5, "files": 1, "size": 3}}`)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"index.html": "hey"})

	r := run(t, srv.URL, "", "deploy", "--flat", "blog", "--save-only", dir)
	if r.code != 0 || !strings.Contains(r.stdout, "flats deploy --flat blog --version 5") {
		t.Fatalf("exit %d: %s %s", r.code, r.stdout, r.stderr)
	}
	if q := api.last("POST /api/flats/blog/versions").query; strings.Contains(q, "deploy") {
		t.Errorf("save-only query = %q", q)
	}

	r = run(t, srv.URL, "", "--json", "deploy", "--flat", "blog", dir)
	var v map[string]any
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &v) != nil || v["version"] == nil {
		t.Fatalf("json mode: exit %d stdout %q", r.code, r.stdout)
	}
}

func TestDeployValidationFailure(t *testing.T) {
	api, srv := newFakeAPI(t)
	api.handle("POST /api/flats/blog/versions", 422, `{
  "error": "upload rejected:\n- flats.json: unknown field \"knd\" (fix: use kind)\n- index.html is missing (fix: add index.html)",
  "problems": [
    {"path": "flats.json", "message": "unknown field \"knd\"", "fix": "use kind"},
    {"message": "index.html is missing", "fix": "add index.html at the bundle root"}
  ]
}`)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"flats.json": `{"knd":"static"}`})
	r := run(t, srv.URL, "", "deploy", dir, "--flat", "blog")
	if r.code != ExitError {
		t.Fatalf("exit %d", r.code)
	}
	for _, want := range []string{
		"error: upload rejected:\n",
		"  - flats.json: unknown field \"knd\"\n    fix: use kind\n",
		"  - index.html is missing\n    fix: add index.html at the bundle root\n",
	} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, r.stderr)
		}
	}
	// The problems are printed once, structured, not again from the message.
	if strings.Count(r.stderr, "use kind") != 1 {
		t.Errorf("problems repeated:\n%s", r.stderr)
	}

	r = run(t, srv.URL, "", "deploy", dir, "--flat", "blog", "--json")
	if r.code != ExitError || !strings.Contains(r.stdout, `"problems"`) {
		t.Fatalf("json: exit %d stdout %s", r.code, r.stdout)
	}
}

func TestDeployHealthFailure(t *testing.T) {
	api, srv := newFakeAPI(t)
	api.handle("POST /api/flats/blog/versions", 422, `{
  "version": {"flat": "blog", "number": 7, "files": 1, "size": 10},
  "deploy_error": {"error": "deploy of version 7 failed its health check: GET /healthz returned 500 (the previous live version keeps serving)",
                   "health": {"path": "/healthz", "status": 500, "ok": false, "millis": 4, "body_head": "boom\n trace"}}
}`)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"index.html": "x"})
	r := run(t, srv.URL, "", "deploy", dir, "--flat", "blog")
	if r.code != ExitError {
		t.Fatalf("exit %d", r.code)
	}
	for _, want := range []string{"previous live version keeps serving", "GET /healthz -> 500 in 4ms (failed)", "response body: boom trace", "--version 7"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, r.stderr)
		}
	}
}

func TestDeployExistingVersion(t *testing.T) {
	api, srv := newFakeAPI(t)
	api.handle("POST /api/flats/blog/deploy", 200, `{"flat": {"slug": "blog", "private_url": "https://blog.ts"}, "version": 2, "previous": 1, "health": {"path": "/", "status": 200, "ok": true}}`)
	r := run(t, srv.URL, "", "deploy", "--flat", "blog", "--version", "2")
	if r.code != 0 || !strings.Contains(r.stdout, "Version 2 is live (was 1).") {
		t.Fatalf("exit %d: %s %s", r.code, r.stdout, r.stderr)
	}
	if b := string(api.last("POST /api/flats/blog/deploy").body); b != `{"version":2}` {
		t.Errorf("body = %s", b)
	}
}

func TestUsageErrors(t *testing.T) {
	_, srv := newFakeAPI(t)
	for _, args := range [][]string{
		{},
		{"bogus"},
		{"deploy", "./dist"},
		{"deploy", "--flat", "x"},
		{"deploy", "--flat", "x", "--version", "2", "--message", "ignored"},
		{"visibility", "blog", "everyone"},
		{"info"},
		{"info", "a", "b"},
		{"list", "--nope"},
		{"secret"},
		{"secret", "show", "blog"},
	} {
		if r := run(t, srv.URL, "", args...); r.code != ExitUsage {
			t.Errorf("%v: exit %d, stderr %s", args, r.code, r.stderr)
		}
	}
	if r := run(t, srv.URL, "", "help", "deploy"); r.code != 0 || !strings.Contains(r.stdout, "usage: flats deploy") {
		t.Errorf("help: %d %s", r.code, r.stdout)
	}
	if r := run(t, srv.URL, "", "rollback", "-h"); r.code != 0 || !strings.Contains(r.stdout, "-to") {
		t.Errorf("-h: %d %s", r.code, r.stdout)
	}
}

func TestVisibilityPendingApproval(t *testing.T) {
	api, srv := newFakeAPI(t)
	api.handle("POST /api/flats/blog/visibility", 202, `{
  "status": "pending_approval",
  "approval": {"id": "apr-1", "flat": "blog", "action": "set_visibility", "status": "pending"},
  "approval_url": "https://flats.tail.ts.net/approvals/apr-1",
  "message": "This action needs the operator's approval."
}`)
	r := run(t, srv.URL, "", "visibility", "blog", "public-unlisted", "--reason", "share with a friend")
	if r.code != ExitPending {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "Approval needed: https://flats.tail.ts.net/approvals/apr-1") {
		t.Errorf("stdout: %s", r.stdout)
	}
	if !strings.Contains(r.stdout, core.UnlistedNotice+"\n") {
		t.Errorf("unlisted notice not printed verbatim:\n%s", r.stdout)
	}
	var body map[string]string
	_ = json.Unmarshal(api.last("POST /api/flats/blog/visibility").body, &body)
	if body["visibility"] != "public-unlisted" || body["reason"] != "share with a friend" {
		t.Errorf("body = %v", body)
	}

	r = run(t, srv.URL, "", "--json", "visibility", "blog", "public-unlisted")
	if r.code != ExitPending || !strings.Contains(r.stdout, `"approval_url"`) {
		t.Errorf("json: %d %s", r.code, r.stdout)
	}
}

func TestVisibilityApplied(t *testing.T) {
	api, srv := newFakeAPI(t)
	api.handle("POST /api/flats/blog/visibility", 200, `{"status": "done", "flat": {"slug": "blog", "visibility": "private"}, "message": "visibility changed to private"}`)
	r := run(t, srv.URL, "", "visibility", "blog", "private")
	if r.code != 0 || !strings.Contains(r.stdout, "blog is private") || strings.Contains(r.stdout, "NOT access control") {
		t.Fatalf("exit %d: %s", r.code, r.stdout)
	}
}

func TestDeletePending(t *testing.T) {
	api, srv := newFakeAPI(t)
	api.handle("DELETE /api/flats/blog", 202, `{"status": "pending_approval", "approval_url": "http://c/approvals/apr-2", "message": "needs approval"}`)
	r := run(t, srv.URL, "", "delete", "blog", "--reason", "old")
	if r.code != ExitPending || !strings.Contains(r.stdout, "http://c/approvals/apr-2") {
		t.Fatalf("exit %d: %s", r.code, r.stdout)
	}
	if q := api.last("DELETE /api/flats/blog").query; q != "reason=old" {
		t.Errorf("query = %q", q)
	}
}

func TestSecretSetNeverPrintsValue(t *testing.T) {
	api, srv := newFakeAPI(t)
	api.handle("PUT /api/flats/blog/secrets/API_TOKEN", 200, `{"name": "API_TOKEN", "status": "stored; redeploy to apply"}`)
	const value = "s3cr3t-value"
	r := run(t, srv.URL, value+"\n", "secret", "set", "blog", "API_TOKEN")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if strings.Contains(r.stdout+r.stderr, value) {
		t.Fatal("secret value was printed")
	}
	var body map[string]string
	_ = json.Unmarshal(api.last("PUT /api/flats/blog/secrets/API_TOKEN").body, &body)
	if body["value"] != value {
		t.Fatalf("sent %q (trailing newline must be trimmed)", body["value"])
	}
	if r := run(t, srv.URL, "", "secret", "set", "blog", "API_TOKEN"); r.code != ExitError {
		t.Errorf("empty stdin: exit %d", r.code)
	}

	api.handle("GET /api/flats/blog/secrets", 200, `{"secrets": [{"name": "API_TOKEN", "updated_at": "2026-10-01T00:00:00Z"}]}`)
	if r := run(t, srv.URL, "", "secret", "ls", "blog"); r.code != 0 || !strings.Contains(r.stdout, "API_TOKEN") {
		t.Errorf("ls: %d %s", r.code, r.stdout)
	}
	api.handle("DELETE /api/flats/blog/secrets/API_TOKEN", 403, `{"error": "secret values are set by the operator"}`)
	if r := run(t, srv.URL, "", "secret", "rm", "blog", "API_TOKEN"); r.code != ExitError || !strings.Contains(r.stderr, "set by the operator") {
		t.Errorf("rm: %d %s", r.code, r.stderr)
	}
}

func TestRollbackAndPreview(t *testing.T) {
	api, srv := newFakeAPI(t)
	api.handle("POST /api/flats/blog/rollback", 200, `{"flat": {"slug": "blog"}, "version": 2, "previous": 4, "health": {"path": "/", "status": 200, "ok": true}}`)
	if r := run(t, srv.URL, "", "rollback", "blog", "--to", "2"); r.code != 0 || !strings.Contains(r.stdout, "Version 2 is live (was 4).") {
		t.Fatalf("rollback: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if b := string(api.last("POST /api/flats/blog/rollback").body); b != `{"restore_data":false,"version":2}` {
		t.Errorf("rollback body = %s", b)
	}
	if run(t, srv.URL, "", "rollback", "blog"); string(api.last("POST /api/flats/blog/rollback").body) != `{"restore_data":false,"version":0}` {
		t.Errorf("default rollback body = %s", api.last("POST /api/flats/blog/rollback").body)
	}

	if run(t, srv.URL, "", "rollback", "blog", "--restore-data"); string(api.last("POST /api/flats/blog/rollback").body) != `{"restore_data":true,"version":0}` {
		t.Errorf("restore-data rollback body = %s", api.last("POST /api/flats/blog/rollback").body)
	}

	api.handle("GET /api/flats/blog/versions", 200, `{"versions": [{"number": 3}, {"number": 9}, {"number": 5}]}`)
	api.handle("POST /api/flats/blog/previews", 201, `{"host": "blog-ab12cd34", "version": 0, "target":"draft", "revision":3, "url": "https://blog-ab12cd34.tail.ts.net", "expires_at": "2026-10-04T10:00:00Z"}`)
	r := run(t, srv.URL, "", "preview", "blog")
	if r.code != 0 || !strings.Contains(r.stdout, "https://blog-ab12cd34.tail.ts.net") {
		t.Fatalf("preview: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if b := string(api.last("POST /api/flats/blog/previews").body); b != `{"version":0}` {
		t.Errorf("preview body = %s", b)
	}
}

func TestListInfoVersionsApprovals(t *testing.T) {
	api, srv := newFakeAPI(t)
	flat := `{"slug": "blog", "name": "Blog", "visibility": "private", "live_version": 2, "private_url": "https://blog.ts", "versions": 2, "disk_bytes": 1500,
	          "live": {"number": 2, "kind": "static", "files": 3, "size": 1200, "git_sha": "deadbeefcafe"}}`
	api.handle("GET /api/flats", 200, `{"flats": [`+flat+`]}`)
	api.handle("GET /api/flats/blog", 200, flat)
	api.handle("GET /api/flats/blog/versions", 200, `{"versions": [
	  {"number": 2, "kind": "static", "files": 3, "size": 1200, "git_sha": "deadbeefcafe", "message": "fix\nnav", "created_at": "2026-10-02T00:00:00Z"},
	  {"number": 1, "kind": "static", "files": 2, "size": 900, "pruned": true, "created_at": "2026-10-01T00:00:00Z"}]}`)
	api.handle("GET /api/approvals", 200, `{"approvals": [{"id": "apr-9", "flat": "blog", "action": "set_visibility", "params": {"visibility": "public-listed", "from": "private"}, "status": "pending", "via": "cli"}]}`)

	r := run(t, srv.URL, "", "list")
	if r.code != 0 || !strings.Contains(r.stdout, "blog") || !strings.Contains(r.stdout, "v2") {
		t.Errorf("list: %s", r.stdout)
	}
	r = run(t, srv.URL, "", "info", "blog")
	if r.code != 0 || !strings.Contains(r.stdout, "version 2, static, 3 files, 1.2 kB, git deadbee") {
		t.Errorf("info: %s", r.stdout)
	}
	r = run(t, srv.URL, "", "versions", "blog")
	if r.code != 0 || !strings.Contains(r.stdout, "live") || !strings.Contains(r.stdout, "pruned") || !strings.Contains(r.stdout, "fix nav") {
		t.Errorf("versions: %s", r.stdout)
	}
	r = run(t, srv.URL, "", "approvals", "--status", "pending")
	if r.code != 0 || !strings.Contains(r.stdout, "set_visibility public-listed") {
		t.Errorf("approvals: %s", r.stdout)
	}
	if q := api.last("GET /api/approvals").query; q != "status=pending" {
		t.Errorf("approvals query = %q", q)
	}
	r = run(t, srv.URL, "", "info", "nope")
	if r.code != ExitError || !strings.Contains(r.stderr, "not found") {
		t.Errorf("missing flat: %d %s", r.code, r.stderr)
	}
}

func TestLogsFollow(t *testing.T) {
	api, srv := newFakeAPI(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var queries []string
	api.handle("GET /api/flats/blog", 200, `{"slug": "blog"}`)
	api.routes["GET /api/flats/blog/logs"] = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		n := len(queries)
		mu.Unlock()
		switch n {
		case 1:
			io.WriteString(w, `{"events": [{"id": 1, "time": "2026-10-03T00:00:00Z", "level": "info", "kind": "deploy", "message": "v1 live"}]}`)
		case 2:
			io.WriteString(w, `{"events": [{"id": 2, "time": "2026-10-03T00:00:01Z", "level": "warn", "kind": "deploy", "message": "v2 live"}]}`)
		default:
			cancel()
			io.WriteString(w, `{"events": []}`)
		}
	}
	var out, errb bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errb, PollInterval: 5 * time.Millisecond,
		Getenv: func(k string) string {
			if k == "FLATS_URL" {
				return srv.URL
			}
			return ""
		}}
	code := Run(ctx, []string{"logs", "blog", "--follow", "--kind", "deploy"}, env)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "v1 live") || !strings.Contains(out.String(), "v2 live") {
		t.Errorf("out: %s", out.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(queries[0], "kind=deploy") || strings.Contains(queries[0], "after=") {
		t.Errorf("first query = %q", queries[0])
	}
	if !strings.Contains(queries[1], "after=1") || !strings.Contains(queries[2], "after=2") {
		t.Errorf("follow queries = %q", queries)
	}
}

func TestServeAndWorkerHooks(t *testing.T) {
	r := run(t, "", "", "serve")
	if r.code != ExitError || !strings.Contains(r.stderr, "serve is not wired yet") {
		t.Fatalf("unwired serve: %d %s", r.code, r.stderr)
	}
	var got []string
	Serve = func(args []string) error { got = args; return nil }
	Worker = func([]string) error { return errors.New("bad socket") }
	t.Cleanup(func() { Serve, Worker = nil, nil })
	if r := run(t, "", "", "serve", "--data", "/x"); r.code != 0 || strings.Join(got, " ") != "--data /x" {
		t.Fatalf("serve: %d %v", r.code, got)
	}
	if r := run(t, "", "", "worker"); r.code != ExitError || !strings.Contains(r.stderr, "bad socket") {
		t.Fatalf("worker: %d %s", r.code, r.stderr)
	}
}

func TestUnreachableServer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	r := run(t, u, "", "list")
	if r.code != ExitError || !strings.Contains(r.stderr, "cannot reach Flats at "+u) {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	// --url overrides FLATS_URL.
	_, live := newFakeAPI(t)
	if r := run(t, u, "", "status", "--url", live.URL); r.code != 0 || !strings.Contains(r.stdout, "running at "+live.URL) {
		t.Fatalf("--url: %d %s %s", r.code, r.stdout, r.stderr)
	}
}

func TestMCPConfig(t *testing.T) {
	r := run(t, "", "", "mcp-config", "--url", "http://127.0.0.1:7878/")
	if r.code != 0 {
		t.Fatal(r.stderr)
	}
	for _, want := range []string{
		"claude mcp add --transport http flats http://127.0.0.1:7878/mcp",
		"[mcp_servers.flats]",
		"every publish, activation, rollback",
		"visibility change in both directions",
		"does not make a version live",
		`url = "http://127.0.0.1:7878/mcp"`,
		`"mcpServers"`,
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("missing %q:\n%s", want, r.stdout)
		}
	}
	r = run(t, "", "", "mcp-config", "--json")
	var v struct {
		URL    string `json:"url"`
		Cursor struct {
			JSON struct {
				MCPServers map[string]struct{ URL string } `json:"mcpServers"`
			} `json:"json"`
		} `json:"cursor"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil || v.URL != DefaultURL+"/mcp" || v.Cursor.JSON.MCPServers["flats"].URL != v.URL {
		t.Fatalf("json: %v %+v\n%s", err, v, r.stdout)
	}
}

// fakeLaunchctl stands in for launchctl: bootstrap loads the job, bootout
// unloads it, and print fails while it is not loaded.
type fakeLaunchctl struct {
	mu     sync.Mutex
	calls  [][]string
	loaded bool
}

func (f *fakeLaunchctl) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	switch args[0] {
	case "print":
		if !f.loaded {
			return []byte("Could not find service"), errors.New("exit status 113")
		}
		return []byte("gui/501/dev.flats.serve = {\n\tstate = running\n\tpid = 77\n}\n"), nil
	case "bootstrap", "load":
		f.loaded = true
	case "bootout":
		if !f.loaded {
			return nil, errors.New("exit status 3")
		}
		f.loaded = false
	}
	return nil, nil
}

func TestInstallUninstallStatusWithFakeLaunchd(t *testing.T) {
	_, srv := newFakeAPI(t)
	home := t.TempDir()
	exe := filepath.Join(home, "flats-bin")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	fl := &fakeLaunchctl{}
	env := Env{Launchd: fl.run, Home: home, GOOS: "darwin", InstallWait: time.Second}
	data := filepath.Join(home, "data")
	r := runEnv(t, env, srv.URL, "install", "--executable", exe, "--data", data, "--", "--authkey-file", "/k")
	if r.code != 0 {
		t.Fatalf("install: %d %s %s", r.code, r.stdout, r.stderr)
	}
	plist, err := os.ReadFile(launchd.PlistPath(home))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<string>serve</string>", "<string>--data</string>", "<string>" + data + "</string>", "<string>--authkey-file</string>", "data/logs/serve.err.log"} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("plist missing %q", want)
		}
	}
	if !strings.Contains(r.stdout, "Flats is running at "+srv.URL) {
		t.Errorf("stdout: %s", r.stdout)
	}

	r = runEnv(t, env, srv.URL, "status")
	if r.code != 0 || !strings.Contains(r.stdout, "launchd: loaded, running, pid 77") || !strings.Contains(r.stdout, "example.ts.net") {
		t.Errorf("status: %d %s", r.code, r.stdout)
	}

	r = runEnv(t, env, srv.URL, "uninstall")
	if r.code != 0 || !strings.Contains(r.stdout, "Removed") {
		t.Fatalf("uninstall: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if _, err := os.Stat(launchd.PlistPath(home)); !os.IsNotExist(err) {
		t.Fatal("plist not removed")
	}
	for _, c := range fl.calls {
		if c[0] != "launchctl" {
			t.Fatalf("unexpected command %v", c)
		}
	}

	env.GOOS = "linux"
	t.Setenv("XDG_CONFIG_HOME", "")
	fl.calls = nil
	if r := runEnv(t, env, srv.URL, "install", "--data", filepath.Join(env.Home, "data")); r.code != 0 || !strings.Contains(r.stdout, "systemd user service") {
		t.Errorf("linux install: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if len(fl.calls) == 0 || fl.calls[0][0] != "systemctl" {
		t.Errorf("linux install should use systemctl, calls %v", fl.calls)
	}
	if _, err := os.Stat(filepath.Join(env.Home, ".config/systemd/user/flats.service")); err != nil {
		t.Errorf("unit not written: %v", err)
	}
	if r := runEnv(t, env, srv.URL, "uninstall"); r.code != 0 || !strings.Contains(r.stdout, "Removed systemd user service") {
		t.Errorf("linux uninstall: %d %s %s", r.code, r.stdout, r.stderr)
	}
	env.GOOS = "windows"
	if r := runEnv(t, env, srv.URL, "install"); r.code != ExitError || !strings.Contains(r.stderr, "systemd") {
		t.Errorf("windows install: %d %s", r.code, r.stderr)
	}
}

func TestUnlistedNoticeMatchesCore(t *testing.T) {
	if unlistedNotice != core.UnlistedNotice {
		t.Fatalf("cli notice drifted from core:\n%q\n%q", unlistedNotice, core.UnlistedNotice)
	}
}

func TestPublicURLsAlwaysCarryTheNotice(t *testing.T) {
	api, srv := newFakeAPI(t)
	unlisted := `{"slug": "blog", "visibility": "public-unlisted", "live_version": 1, "private_url": "https://blog.ts",
	              "public_url": "https://blog.portal.example", "public_notice": "` + core.UnlistedNotice + `", "versions": 1}`
	// An older server may send a public URL without a notice.
	bare := `{"slug": "shop", "visibility": "public-listed", "live_version": 1, "private_url": "https://shop.ts",
	          "public_url": "https://shop.portal.example", "versions": 1}`
	private := `{"slug": "notes", "visibility": "private", "private_url": "https://notes.ts", "versions": 0}`
	api.handle("GET /api/flats", 200, `{"flats": [`+unlisted+`,`+bare+`,`+private+`]}`)
	api.handle("GET /api/flats/shop", 200, bare)
	api.handle("POST /api/flats/shop/deploy", 200, `{"flat": `+bare+`, "version": 1, "health": {"path": "/", "status": 200, "ok": true}}`)
	api.handle("POST /api/flats/shop/visibility", 200, `{"status": "done", "flat": `+bare+`}`)
	api.handle("GET /api/status", 200, `{"ok": true, "system": {"public": {"kind": "portal", "enabled": true,
	   "hosts": [{"host": "blog", "url": "https://blog.portal.example", "state": "ready"}]}}}`)

	r := run(t, srv.URL, "", "list")
	if r.code != 0 || !strings.Contains(r.stdout, "https://blog.portal.example (*)") ||
		!strings.Contains(r.stdout, "blog: "+core.UnlistedNotice) || !strings.Contains(r.stdout, "shop: "+publicURLNotice) ||
		strings.Contains(r.stdout, "notes:") {
		t.Errorf("list must annotate every public URL:\n%s", r.stdout)
	}
	for _, args := range [][]string{{"info", "shop"}, {"deploy", "--flat", "shop", "--version", "1"}, {"visibility", "shop", "public-listed"}} {
		r := run(t, srv.URL, "", args...)
		if r.code != 0 || !strings.Contains(r.stdout, "https://shop.portal.example") || !strings.Contains(r.stdout, publicURLNotice) {
			t.Errorf("%v must print the notice with the public URL: %d\n%s%s", args, r.code, r.stdout, r.stderr)
		}
	}
	r = runEnv(t, Env{GOOS: "linux"}, srv.URL, "status")
	if r.code != 0 || !strings.Contains(r.stdout, "https://blog.portal.example") || !strings.Contains(r.stdout, publicURLNotice) {
		t.Errorf("status must print the notice with public hosts:\n%s", r.stdout)
	}
}

func TestUnknownFlatIsAnError(t *testing.T) {
	api, srv := newFakeAPI(t)
	// Older servers answer the events and versions lists of a missing flat
	// with empty 200s; the CLI must still fail.
	api.handle("GET /api/flats/nope/logs", 200, `{"events": null}`)
	api.handle("GET /api/flats/nope/versions", 200, `{"versions": null}`)
	for _, args := range [][]string{{"logs", "nope"}, {"logs", "nope", "--follow"}, {"logs", "nope", "--json"}, {"versions", "nope"}, {"versions", "nope", "--json"}, {"info", "nope"}} {
		r := run(t, srv.URL, "", args...)
		msg := r.stderr + r.stdout
		if r.code != ExitError || !strings.Contains(msg, `flat \"nope\" not found`) && !strings.Contains(msg, `flat "nope" not found`) {
			t.Errorf("%v: want exit 1 with a not-found message, got %d\n%s", args, r.code, msg)
		}
	}

	// A flat deleted while following its logs ends the follow with an error.
	api.handle("GET /api/flats/gone", 200, `{"slug": "gone"}`)
	var mu sync.Mutex
	calls := 0
	api.routes["GET /api/flats/gone/logs"] = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			io.WriteString(w, `{"events": [{"id": 1, "time": "2026-10-03T00:00:00Z", "level": "info", "kind": "deploy", "message": "v1 live"}]}`)
			return
		}
		w.WriteHeader(404)
		io.WriteString(w, `{"error": "flat \"gone\": not found"}`)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out, errb bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errb, PollInterval: 5 * time.Millisecond,
		Getenv: func(k string) string {
			if k == "FLATS_URL" {
				return srv.URL
			}
			return ""
		}}
	code := Run(ctx, []string{"logs", "gone", "-f"}, env)
	if ctx.Err() != nil || code != ExitError || !strings.Contains(errb.String(), "no longer exists") || !strings.Contains(out.String(), "v1 live") {
		t.Fatalf("follow must stop on 404: exit %d, ctx %v\n%s%s", code, ctx.Err(), out.String(), errb.String())
	}
}

func TestInstallCredentialPassthroughAtCLIBoundary(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", "")
			_, srv := newFakeAPI(t)
			home := t.TempDir()
			fl := &fakeLaunchctl{}
			env := Env{Launchd: fl.run, Home: home, GOOS: goos, InstallWait: time.Second}
			credential := filepath.Join(home, "operator credentials", "credential")
			exe := filepath.Join(home, "flats")
			if err := os.WriteFile(exe, []byte("x"), 0755); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"--operator-credential-file", "relative"}, {"--operator-credential-file=relative"}, {"-operator-credential-file", "relative"}, {"--operator-credential-file"}} {
				r := runEnv(t, env, srv.URL, append([]string{"install", "--executable", exe, "--"}, args...)...)
				if r.code == 0 || len(fl.calls) != 0 || !strings.Contains(r.stderr, "absolute path") {
					t.Fatalf("invalid path installed: %+v calls=%v", r, fl.calls)
				}
			}
			for _, args := range [][]string{{"--operator-credential-file", credential}, {"--operator-credential-file=" + credential}} {
				r := runEnv(t, env, srv.URL, append([]string{"install", "--executable", exe, "--"}, args...)...)
				if r.code != 0 {
					t.Fatalf("documented install failed: %+v", r)
				}
				path := launchd.PlistPath(home)
				if goos == "linux" {
					path = filepath.Join(home, ".config", "systemd", "user", "flats.service")
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(raw), "operator-credential-file") || !strings.Contains(string(raw), "operator credentials/credential") {
					t.Fatal("credential passthrough missing", string(raw))
				}
			}
		})
	}
}
