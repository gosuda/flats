package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/mcpx"
	"github.com/gosuda/flats/internal/store"
)

// startBridge runs `flats mcp` against base and returns an SDK client
// session speaking to it over stdio, plus a channel with the exit code.
func startBridge(t *testing.T, ctx context.Context, base string) (*mcp.ClientSession, <-chan int) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan int, 1)
	go func() {
		done <- Run(ctx, []string{"mcp"}, Env{Stdin: inR, Stdout: outW, ConfigDir: t.TempDir(),
			Getenv: func(k string) string {
				if k == "FLATS_URL" {
					return base
				}
				return ""
			}})
		outW.Close()
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "bridge-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return session, done
}

func waitExit(t *testing.T, done <-chan int) {
	t.Helper()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("flats mcp exit %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("flats mcp did not exit after stdin closed")
	}
}

// TestMCPBridgeRealHost relays the real Flats MCP endpoint. The bridge runs
// on the host, so the loopback-only directory upload keeps working.
func TestMCPBridgeRealHost(t *testing.T) {
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
	svc, err := core.New(context.Background(), core.Config{DataDir: filepath.Join(dir, "data"), Store: st, Private: priv, Public: local.NewPublic(pubNet), ConsoleURL: func() string { return "http://console.test" }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpx.Handler(svc, mcpx.Options{Version: "test"}))
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close(); svc.Close(); priv.Close(); pubNet.Close(); st.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, done := startBridge(t, ctx, srv.URL)
	if init := session.InitializeResult(); init == nil || init.ServerInfo.Name != "flats" || !strings.Contains(init.Instructions, "operator") {
		t.Fatalf("initialize: %+v", init)
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"save_draft", "publish", "get_runtime_reference"} {
		if !names[want] {
			t.Errorf("tool %s missing from %v", want, names)
		}
	}
	if _, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "flats://docs/runtime-api/v1"}); err != nil {
		t.Fatalf("resource: %v", err)
	}

	site := t.TempDir()
	if err := os.WriteFile(filepath.Join(site, "index.html"), []byte("<h1>bridged</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "save_version_from_dir", Arguments: map[string]any{"slug": "bridged", "dir": site}})
	if err != nil || res.IsError {
		t.Fatalf("from_dir through the bridge: %+v %v", res, err)
	}
	draft, err := svc.GetDraft(ctx, "bridged")
	if err != nil || draft.Revision != 1 {
		t.Fatalf("draft %+v %v", draft, err)
	}
	// A failing tool comes back as a tool error, not a transport failure.
	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "save_version_from_dir", Arguments: map[string]any{"slug": "bridged", "dir": "relative"}})
	if err != nil || !res.IsError {
		t.Fatalf("tool error: %+v %v", res, err)
	}

	session.Close()
	waitExit(t, done)
}

// TestMCPBridgeStatefulSSE covers hosts that answer with event streams and
// a session ID, including concurrent calls and ending the session.
func TestMCPBridgeStatefulSSE(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "sse-host", Version: "1"}, nil)
	type in struct {
		Text  string `json:"text"`
		Delay int    `json:"delay_ms"`
	}
	type out struct {
		Echo string `json:"echo"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "echo"}, func(ctx context.Context, _ *mcp.CallToolRequest, a in) (*mcp.CallToolResult, out, error) {
		time.Sleep(time.Duration(a.Delay) * time.Millisecond)
		return nil, out{Echo: a.Text}, nil
	})
	var mu sync.Mutex
	var sessions, deletes []string
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.Method == http.MethodDelete {
			deletes = append(deletes, r.Header.Get("Mcp-Session-Id"))
		} else if id := r.Header.Get("Mcp-Session-Id"); id != "" {
			sessions = append(sessions, id)
		}
		mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, done := startBridge(t, ctx, srv.URL)
	var wg sync.WaitGroup
	results := make([]string, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The first call is the slowest, so replies arrive out of order.
			res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": strings.Repeat("x", i+1), "delay_ms": (len(results) - i) * 30}})
			if err != nil || res.IsError {
				t.Errorf("call %d: %+v %v", i, res, err)
				return
			}
			b, _ := json.Marshal(res.StructuredContent)
			results[i] = string(b)
		}()
	}
	wg.Wait()
	for i, got := range results {
		if want := `{"echo":"` + strings.Repeat("x", i+1) + `"}`; got != want {
			t.Errorf("call %d = %s, want %s", i, got, want)
		}
	}
	session.Close()
	waitExit(t, done)

	mu.Lock()
	defer mu.Unlock()
	if len(sessions) == 0 || len(deletes) != 1 || deletes[0] != sessions[0] {
		t.Fatalf("session %v, deletes %v", sessions, deletes)
	}
}

// TestMCPBridgeUnreachableHost answers requests with an error that says how
// to point the bridge at the right host, and ignores notifications.
func TestMCPBridgeUnreachableHost(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var errb strings.Builder
	done := make(chan int, 1)
	go func() {
		done <- Run(context.Background(), []string{"mcp"}, Env{Stdin: inR, Stdout: outW, Stderr: &errb, ConfigDir: t.TempDir(),
			Getenv: func(k string) string {
				if k == "FLATS_URL" {
					return "http://127.0.0.1:1"
				}
				return ""
			}})
		outW.Close()
	}()
	go func() {
		io.WriteString(inW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
		io.WriteString(inW, "not json\n")
		io.WriteString(inW, `{"jsonrpc":"2.0","id":7,"method":"initialize","params":{}}`+"\n")
	}()
	// Closing stdin ends the bridge, so close it only after both replies.
	var lines []map[string]any
	sc := bufio.NewScanner(outR)
	for len(lines) < 2 && sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("stdout line is not JSON: %q", sc.Text())
		}
		lines = append(lines, m)
	}
	inW.Close()
	for sc.Scan() {
		t.Errorf("unexpected output: %s", sc.Text())
	}
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(lines) != 2 {
		t.Fatalf("want a parse error and one reply, got %v", lines)
	}
	parse, reply := lines[0], lines[1]
	if parse["id"] != nil || parse["error"].(map[string]any)["code"].(float64) != -32700 {
		t.Errorf("parse error: %v", parse)
	}
	msg, _ := reply["error"].(map[string]any)["message"].(string)
	if reply["id"].(float64) != 7 || !strings.Contains(msg, "cannot reach Flats at http://127.0.0.1:1/mcp") || !strings.Contains(msg, "flats connect") {
		t.Errorf("reply: %v", reply)
	}
	if !strings.Contains(errb.String(), "relaying to http://127.0.0.1:1/mcp (env)") {
		t.Errorf("stderr: %s", errb.String())
	}
}

// TestMCPBridgeExitsWhileHostStalls ends the bridge when the client closes
// stdin even if the host never answers initialize.
func TestMCPBridgeExitsWhileHostStalls(t *testing.T) {
	received := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	inR, inW := io.Pipe()
	done := make(chan int, 1)
	go func() {
		done <- Run(context.Background(), []string{"mcp", "--url", srv.URL}, Env{Stdin: inR, Stdout: io.Discard, ConfigDir: t.TempDir()})
	}()
	io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n")
	io.WriteString(inW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	<-received
	inW.Close()
	waitExit(t, done)
	select {
	case <-received:
		t.Fatal("a message was sent before initialize answered")
	default:
	}
}
