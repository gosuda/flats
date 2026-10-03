package mcpx

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/oesni/flats/internal/core"
	"github.com/oesni/flats/internal/expose/local"
	"github.com/oesni/flats/internal/store"
)

type env struct {
	localURL, remoteURL string
	svc                 *core.Service
	priv                *local.Net
	pub                 *local.Public
	local               *mcp.ClientSession // connects from loopback
	remote              *mcp.ClientSession // looks like a tailnet peer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	priv, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pubNet, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pub := local.NewPublic(pubNet)
	svc, err := core.New(context.Background(), core.Config{DataDir: dir, Store: st, Private: priv, Public: pub,
		ConsoleURL: func() string { return "http://console.test" }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	h := Handler(svc, Options{Version: "test"})
	mux := http.NewServeMux()
	mux.Handle("/mcp", h)
	localSrv := httptest.NewServer(mux)
	remoteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "100.64.0.7:41641" // a tailnet peer
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		localSrv.Close()
		remoteSrv.Close()
		svc.Close()
		priv.Close()
		pubNet.Close()
		st.Close()
	})
	return &env{localURL: localSrv.URL + "/mcp", remoteURL: remoteSrv.URL, svc: svc, priv: priv, pub: pub,
		local: connect(t, localSrv.URL+"/mcp"), remote: connect(t, remoteSrv.URL)}
}

func connect(t *testing.T, endpoint string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "v0"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: endpoint}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// call runs a tool and returns its result text; out (if non-nil) receives the
// structured output.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	var texts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			texts = append(texts, tc.Text)
		}
	}
	if out != nil && !res.IsError {
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s: decode structured output %s: %v", name, raw, err)
		}
	}
	return strings.Join(texts, "\n"), res.IsError
}

func get(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func file(path, content, enc string) map[string]any {
	return map[string]any{"path": path, "content": content, "encoding": enc}
}

func TestListTools(t *testing.T) {
	e := newEnv(t)
	res, err := e.local.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
		if tool.OutputSchema == nil {
			t.Errorf("%s has no output schema", tool.Name)
		}
	}
	want := []string{"create_flat", "delete_flat", "deploy", "get_approval", "get_flat", "get_logs", "list_flats",
		"list_secrets", "list_versions", "open_preview", "rollback", "save_version", "save_version_from_dir", "set_visibility"}
	slices.Sort(names)
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	ins := e.local.InitializeResult().Instructions
	for _, s := range []string{"approval_url", "NOT access control", "flats.json", "save_version_from_dir"} {
		if !strings.Contains(ins, s) {
			t.Errorf("instructions do not mention %q", s)
		}
	}
}

func TestSaveVersionAndDeploy(t *testing.T) {
	e := newEnv(t)
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0xff, 0xfe}
	var out SaveOut
	text, isErr := call(t, e.local, "save_version", map[string]any{
		"slug": "blog",
		"files": []any{
			file("index.html", "<h1>héllo</h1>", "UTF-8"),
			file("img/logo.png", base64.StdEncoding.EncodeToString(png), "Base64"),
			file("flats.json", `{"name":"My Blog"}`, ""),
		},
		"git_sha": "abc123", "git_dirty": true, "message": "first", "deploy": true,
	}, &out)
	if isErr {
		t.Fatalf("save_version failed: %s", text)
	}
	if out.Version.Number != 1 || !out.Version.Live || out.Version.GitSHA != "abc123" || !out.Version.GitDirty || out.Version.Files != 3 {
		t.Fatalf("version = %+v", out.Version)
	}
	if out.Deploy == nil || !out.Deploy.Health.OK || out.Deploy.Version != 1 {
		t.Fatalf("deploy = %+v", out.Deploy)
	}
	if !strings.Contains(text, out.Deploy.PrivateURL) || !strings.Contains(text, "is live") {
		t.Fatalf("summary should name the live URL: %s", text)
	}
	code, body := get(t, out.Deploy.PrivateURL)
	if code != 200 || string(body) != "<h1>héllo</h1>" {
		t.Fatalf("GET private URL: %d %q", code, body)
	}
	if _, body := get(t, out.Deploy.PrivateURL+"/img/logo.png"); !bytes.Equal(body, png) {
		t.Fatalf("base64 file round trip: %v", body)
	}

	var flat FlatOut
	if text, isErr := call(t, e.local, "get_flat", map[string]any{"slug": "blog"}, &flat); isErr {
		t.Fatal(text)
	}
	if flat.Flat.Name != "My Blog" || flat.Flat.LiveVersion != 1 || flat.Flat.Visibility != "private" {
		t.Fatalf("flat = %+v", flat.Flat)
	}

	// A second save leaves live alone; list_versions marks the live one.
	if text, isErr := call(t, e.local, "save_version", map[string]any{"slug": "blog", "files": []any{file("index.html", "two", "")}}, nil); isErr {
		t.Fatal(text)
	}
	if _, body := get(t, out.Deploy.PrivateURL); string(body) != "<h1>héllo</h1>" {
		t.Fatalf("save without deploy changed live: %q", body)
	}
	var vs VersionsOut
	call(t, e.local, "list_versions", map[string]any{"slug": "blog"}, &vs)
	if len(vs.Versions) != 2 || vs.Versions[0].Number != 2 || vs.Versions[0].Live || !vs.Versions[1].Live {
		t.Fatalf("versions = %+v", vs)
	}
	var prev PreviewInfo
	if text, isErr := call(t, e.local, "open_preview", map[string]any{"slug": "blog", "version": 2}, &prev); isErr {
		t.Fatal(text)
	}
	if _, body := get(t, prev.URL); string(body) != "two" {
		t.Fatalf("preview serves %q", body)
	}
	var d DeployInfo
	if text, isErr := call(t, e.local, "deploy", map[string]any{"slug": "blog", "version": 2}, &d); isErr || d.Previous != 1 {
		t.Fatalf("deploy: %s %+v", text, d)
	}
	if text, isErr := call(t, e.local, "rollback", map[string]any{"slug": "blog"}, &d); isErr || d.Version != 1 {
		t.Fatalf("rollback: %s %+v", text, d)
	}
	var logs LogsOut
	call(t, e.local, "get_logs", map[string]any{"slug": "blog", "kind": "deploy"}, &logs)
	if len(logs.Events) != 3 || logs.NextAfter != logs.Events[2].ID {
		t.Fatalf("deploy events = %+v", logs)
	}
	var secs SecretsOut
	if text, isErr := call(t, e.local, "list_secrets", map[string]any{"slug": "blog"}, &secs); isErr || len(secs.Secrets) != 0 || secs.Note == "" {
		t.Fatalf("list_secrets: %s %+v", text, secs)
	}
}

func TestValidationErrorsHaveFixes(t *testing.T) {
	e := newEnv(t)
	text, isErr := call(t, e.local, "save_version", map[string]any{
		"slug": "site", "files": []any{file("readme.md", "# hi", "utf8")},
	}, nil)
	if !isErr {
		t.Fatalf("missing index.html accepted: %s", text)
	}
	for _, s := range []string{"index.html", "fix:", `"problems"`, `"fix"`} {
		if !strings.Contains(text, s) {
			t.Errorf("error text lacks %q:\n%s", s, text)
		}
	}
	text, isErr = call(t, e.local, "save_version", map[string]any{
		"slug": "site", "files": []any{file("index.html", "x", "utf8"), file("a.bin", "%%%", "base64"), file("b", "x", "hex")},
	}, nil)
	if !isErr || !strings.Contains(text, "a.bin: invalid base64") || !strings.Contains(text, `unknown encoding "hex"`) {
		t.Fatalf("encoding problems not reported:\n%s", text)
	}
	text, isErr = call(t, e.local, "save_version", map[string]any{
		"slug": "site", "files": []any{file("../etc/passwd", "x", "utf8"), file("index.html", "x", "")},
	}, nil)
	if !isErr || !strings.Contains(text, "escapes the bundle root") {
		t.Fatalf("path escape not reported:\n%s", text)
	}
	if _, err := e.svc.GetFlat(context.Background(), "site"); err == nil {
		t.Fatal("rejected uploads must not create flats")
	}
	call(t, e.local, "create_flat", map[string]any{"slug": "empty"}, nil)
	text, isErr = call(t, e.local, "rollback", map[string]any{"slug": "empty"}, nil)
	if !isErr || !strings.Contains(text, "deploy a saved version first") {
		t.Fatalf("rollback of an undeployed flat: %s", text)
	}
	text, isErr = call(t, e.local, "get_flat", map[string]any{"slug": "nope"}, nil)
	if !isErr || !strings.Contains(text, "list_flats") {
		t.Fatalf("unknown flat: %s", text)
	}
}

func TestUploadLimit(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.UpdateSettings(context.Background(), map[string]string{core.SetUploadMaxBytes: "1024"}); err != nil {
		t.Fatal(err)
	}
	text, isErr := call(t, e.local, "save_version", map[string]any{
		"slug": "big", "files": []any{file("index.html", strings.Repeat("a", 2000), "")},
	}, nil)
	if !isErr || !strings.Contains(text, "1024-byte limit") {
		t.Fatalf("over-limit upload: %s", text)
	}
	// A body far beyond the base64-adjusted limit is refused by the transport.
	_, err := e.local.CallTool(context.Background(), &mcp.CallToolParams{Name: "save_version", Arguments: map[string]any{
		"slug": "big", "files": []any{file("index.html", strings.Repeat("a", 2<<20), "")},
	}})
	if err == nil || !strings.Contains(err.Error(), "Request Entity Too Large") {
		t.Fatalf("oversized request body: %v", err)
	}
}

func TestFailedDeployKeepsLive(t *testing.T) {
	e := newEnv(t)
	var out SaveOut
	if text, isErr := call(t, e.local, "save_version", map[string]any{"slug": "site", "files": []any{file("index.html", "ok", "")}, "deploy": true}, &out); isErr {
		t.Fatal(text)
	}
	text, isErr := call(t, e.local, "save_version", map[string]any{"slug": "site", "deploy": true, "files": []any{
		file("index.html", "broken", ""), file("flats.json", `{"health":"/missing"}`, ""),
	}}, nil)
	if !isErr {
		t.Fatalf("failed health check reported as success: %s", text)
	}
	for _, s := range []string{"saved version 2", "previous live version 1 keeps serving", `"health"`, `"status":404`} {
		if !strings.Contains(text, s) {
			t.Errorf("deploy failure text lacks %q:\n%s", s, text)
		}
	}
	if _, body := get(t, out.Deploy.PrivateURL); string(body) != "ok" {
		t.Fatalf("live changed after failed deploy: %q", body)
	}
}

func TestSetVisibilityNeedsApproval(t *testing.T) {
	e := newEnv(t)
	call(t, e.local, "save_version", map[string]any{"slug": "demo", "files": []any{file("index.html", "hi", "")}, "deploy": true}, nil)
	var out ActionOut
	text, isErr := call(t, e.local, "set_visibility", map[string]any{"slug": "demo", "visibility": "public-unlisted", "reason": "show a friend"}, &out)
	if isErr {
		t.Fatal(text)
	}
	if out.Status != "pending_approval" || !strings.HasPrefix(out.ApprovalURL, "http://console.test/approvals/apr-") || out.ApprovalID == "" {
		t.Fatalf("result = %+v", out)
	}
	if out.Notice != core.UnlistedNotice || !strings.Contains(text, core.UnlistedNotice) || !strings.Contains(text, out.ApprovalURL) {
		t.Fatalf("pending result must carry the approval URL and the unlisted notice:\n%s", text)
	}
	if _, served := e.pub.Hidden("demo"); served {
		t.Fatal("flat went public without approval")
	}
	var fi FlatOut
	call(t, e.local, "get_flat", map[string]any{"slug": "demo"}, &fi)
	if fi.Flat.Visibility != "private" || fi.Flat.PublicURL != "" {
		t.Fatalf("flat changed before approval: %+v", fi.Flat)
	}
	var a ApprovalOut
	call(t, e.local, "get_approval", map[string]any{"id": out.ApprovalID}, &a)
	if a.Status != "pending" || a.Action != "set_visibility" || a.Params["visibility"] != "public-unlisted" || a.Reason != "show a friend" {
		t.Fatalf("approval = %+v", a)
	}

	// After the operator approves, the flat reports its public URL and notice.
	if _, err := e.svc.Decide(context.Background(), out.ApprovalID, true); err != nil {
		t.Fatal(err)
	}
	text, _ = call(t, e.local, "get_flat", map[string]any{"slug": "demo"}, &fi)
	if fi.Flat.PublicURL == "" || fi.Flat.PublicNotice != core.UnlistedNotice || !strings.Contains(text, core.UnlistedNotice) {
		t.Fatalf("public flat without notice: %s", text)
	}
	text, _ = call(t, e.local, "list_flats", nil, nil)
	if !strings.Contains(text, core.UnlistedNotice) {
		t.Fatalf("list_flats omits the notice next to a public URL: %s", text)
	}
	var d DeployInfo
	text, isErr = call(t, e.local, "deploy", map[string]any{"slug": "demo", "version": 1}, &d)
	if isErr || d.PublicURL == "" || d.PublicNotice != core.UnlistedNotice || !strings.Contains(text, d.PublicURL) || !strings.Contains(text, core.UnlistedNotice) {
		t.Fatalf("deploy of a public flat must show the public URL with its notice: %s %+v", text, d)
	}
	// Narrowing applies immediately.
	call(t, e.local, "set_visibility", map[string]any{"slug": "demo", "visibility": "private"}, &out)
	if out.Status != "done" || out.Visibility != "private" {
		t.Fatalf("narrowing: %+v", out)
	}
	if _, served := e.pub.Hidden("demo"); served {
		t.Fatal("still public after going private")
	}

	call(t, e.local, "delete_flat", map[string]any{"slug": "demo", "reason": "done"}, &out)
	if out.Status != "pending_approval" || out.ApprovalURL == "" {
		t.Fatalf("delete must wait for approval: %+v", out)
	}
	if _, err := e.svc.GetFlat(context.Background(), "demo"); err != nil {
		t.Fatal("flat deleted without approval")
	}
}

func TestSaveVersionFromDir(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(t.TempDir(), "dist")
	if err := os.MkdirAll(filepath.Join(dir, "css"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("from dir"), 0o644)
	os.WriteFile(filepath.Join(dir, "css", "a.css"), []byte("body{}"), 0o644)
	args := map[string]any{"slug": "local", "dir": dir, "deploy": true}

	text, isErr := call(t, e.remote, "save_version_from_dir", args, nil)
	if !isErr || !strings.Contains(text, "loopback") || !strings.Contains(text, "save_version") {
		t.Fatalf("remote caller must be refused with a hint: %s", text)
	}
	if fs, _ := e.svc.ListFlats(context.Background()); len(fs) != 0 {
		t.Fatal("refused call created a flat")
	}

	var out SaveOut
	text, isErr = call(t, e.local, "save_version_from_dir", args, &out)
	if isErr {
		t.Fatalf("loopback caller refused: %s", text)
	}
	if out.Version.Files != 2 || out.Deploy == nil {
		t.Fatalf("out = %+v", out)
	}
	if _, body := get(t, out.Deploy.PrivateURL); string(body) != "from dir" {
		t.Fatalf("served %q", body)
	}
	// A symlinked build directory is resolved; links inside it are still refused.
	link := filepath.Join(t.TempDir(), "out")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if text, isErr := call(t, e.local, "save_version_from_dir", map[string]any{"slug": "local", "dir": link}, &out); isErr || out.Version.Files != 2 {
		t.Fatalf("symlinked dir: %s %+v", text, out)
	}
	if err := os.Symlink(filepath.Join(dir, "index.html"), filepath.Join(dir, "alias.html")); err != nil {
		t.Fatal(err)
	}
	if text, isErr := call(t, e.local, "save_version_from_dir", map[string]any{"slug": "local", "dir": link}, nil); !isErr || !strings.Contains(text, "alias.html") {
		t.Fatalf("link inside the dir accepted: %s", text)
	}
	if text, isErr := call(t, e.local, "save_version_from_dir", map[string]any{"slug": "local", "dir": "dist"}, nil); !isErr || !strings.Contains(text, "absolute") {
		t.Fatalf("relative dir: %s", text)
	}
	// The remote session still works for inline saves.
	if text, isErr := call(t, e.remote, "save_version", map[string]any{"slug": "local", "files": []any{file("index.html", "inline", "")}}, nil); isErr {
		t.Fatal(text)
	}
}

func TestFromLoopback(t *testing.T) {
	for _, tc := range []struct {
		remote string
		header string
		want   bool
		local  net.Addr
	}{
		{"127.0.0.1:5000", "", true, nil},
		{"[::1]:5000", "", true, &net.TCPAddr{IP: net.IPv6loopback, Port: 7878}},
		{"[::ffff:127.0.0.1]:5000", "", true, nil},
		// Loopback peer but the connection arrived on a tailnet address.
		{"127.0.0.1:5000", "", false, &net.TCPAddr{IP: net.ParseIP("100.64.0.1"), Port: 443}},
		{"100.64.0.7:5000", "", false, nil},
		{"192.168.1.2:5000", "", false, nil},
		{"127.0.0.1:5000", "X-Forwarded-For", false, nil},
		{"127.0.0.1:5000", "Forwarded", false, nil},
		{"127.0.0.1:5000", "Tailscale-User-Login", false, nil},
	} {
		r := httptest.NewRequest("POST", "/mcp", nil)
		r.RemoteAddr = tc.remote
		if tc.header != "" {
			r.Header.Set(tc.header, "x")
		}
		if tc.local != nil {
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, tc.local))
		}
		if got := fromLoopback(r); got != tc.want {
			t.Errorf("fromLoopback(%s, %s) = %v, want %v", tc.remote, tc.header, got, tc.want)
		}
	}
}

// rawCall posts one tools/call the way clients on older protocol versions do
// (no initialize, stateless) and returns the decoded result.
func rawCall(t *testing.T, url, protocol, tool string, args map[string]any) (text string, isErr bool) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if protocol != "" {
		req.Header.Set("Mcp-Protocol-Version", protocol)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var msg struct {
		Result struct {
			Content []struct{ Text string }
			IsError bool
		}
		Error any
	}
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || json.Unmarshal(raw, &msg) != nil || msg.Error != nil {
		t.Fatalf("%s (protocol %q): %d %s", tool, protocol, resp.StatusCode, raw)
	}
	for _, c := range msg.Result.Content {
		text += c.Text + "\n"
	}
	return text, msg.Result.IsError
}

// The loopback flag must reach tool handlers on every protocol version
// agents use, not only on the version the Go client negotiates.
func TestLoopbackAcrossProtocols(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("hi"), 0o644)
	for i, proto := range []string{"", "2025-03-26", "2025-06-18", "2025-11-25"} {
		args := map[string]any{"slug": fmt.Sprintf("proto%d", i), "dir": dir}
		if text, isErr := rawCall(t, e.remoteURL, proto, "save_version_from_dir", args); !isErr || !strings.Contains(text, "loopback") {
			t.Errorf("protocol %q: remote caller not refused: %s", proto, text)
		}
		if text, isErr := rawCall(t, e.localURL, proto, "save_version_from_dir", args); isErr {
			t.Errorf("protocol %q: loopback caller refused: %s", proto, text)
		}
	}
}

func TestInstructionsFollowUploadLimit(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.UpdateSettings(context.Background(), map[string]string{core.SetUploadMaxBytes: "4096"}); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, e.localURL)
	if ins := cs.InitializeResult().Instructions; !strings.Contains(ins, "4096 bytes") {
		t.Fatalf("instructions quote a stale limit:\n%s", ins)
	}
}

func TestLogsLimit(t *testing.T) {
	e := newEnv(t)
	call(t, e.local, "create_flat", map[string]any{"slug": "noisy"}, nil)
	for i := range 300 {
		e.svc.Event(context.Background(), "noisy", "info", "runtime", fmt.Sprint("line ", i), nil)
	}
	var logs LogsOut
	call(t, e.local, "get_logs", map[string]any{"slug": "noisy", "kind": "runtime", "limit": 5000}, &logs)
	if len(logs.Events) != 300 {
		t.Fatalf("limit above the maximum returned %d of 300 events", len(logs.Events))
	}
	call(t, e.local, "get_logs", map[string]any{"slug": "noisy", "kind": "runtime", "limit": 10, "after": logs.Events[100].ID}, &logs)
	if len(logs.Events) != 10 || logs.Events[0].Message != "line 101" || logs.NextAfter != logs.Events[9].ID {
		t.Fatalf("paging: %+v", logs)
	}
}

func TestRollbackIsMarkedDestructive(t *testing.T) {
	e := newEnv(t)
	res, err := e.local.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "rollback" {
			continue
		}
		a := tool.Annotations
		if a == nil || a.DestructiveHint == nil || !*a.DestructiveHint || a.IdempotentHint {
			t.Fatalf("rollback must be destructive and not idempotent: %+v", a)
		}
		if !strings.Contains(tool.Description, "REPLACES the flat's current database") || !strings.Contains(tool.Description, "backed up") {
			t.Errorf("description must say what restore_data does: %s", tool.Description)
		}
		schema, _ := json.Marshal(tool.InputSchema)
		if !strings.Contains(string(schema), "replace the current database with the snapshot") || !strings.Contains(string(schema), "backing the current database up") {
			t.Errorf("restore_data schema must explain the replacement: %s", schema)
		}
		return
	}
	t.Fatal("no rollback tool")
}

// Regression (real-tailnet gate): tool text tells the agent when the private
// URL does not answer yet, and stays quiet when it does.
func TestDeployTextNotesPendingHost(t *testing.T) {
	d := DeployInfo{Version: 1, PrivateURL: "https://blog.tail1.ts.net", PrivateState: "starting", PrivateDetail: "waiting for its HTTPS certificate"}
	if got := deployText("blog", d, "Deployed"); !strings.Contains(got, "does not answer yet (starting: waiting for its HTTPS certificate)") {
		t.Errorf("pending deploy text %q", got)
	}
	d.PrivateState, d.PrivateDetail = "ready", ""
	if got := deployText("blog", d, "Deployed"); strings.Contains(got, "answer yet") {
		t.Errorf("ready deploy text %q", got)
	}
}
