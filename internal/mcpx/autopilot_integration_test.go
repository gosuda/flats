package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

// These tests run the integrated feature over real HTTP: Autopilot is
// changed through the console routes with the headers the console page
// sends, and agents decide through MCP in later requests.

// consolePut changes an Experiments setting as the console page does.
func consolePut(t *testing.T, e *env, path string, body any) map[string]any {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	code, out := e.operatorCall(t, "PUT", "/experiments/autopilot"+path, string(b))
	if code != 200 {
		t.Fatalf("console PUT %s %s: %d %v", path, b, code, out)
	}
	return out
}

// consoleAutopilotOn allows exactly ops, stores guide and turns Autopilot
// on, confirming the operations the console reports, all through console
// requests.
func consoleAutopilotOn(t *testing.T, e *env, guide string, ops ...core.AutopilotOp) {
	t.Helper()
	for _, op := range core.AutopilotOps {
		consolePut(t, e, "/operations/"+string(op), map[string]bool{"allowed": slices.Contains(ops, op)})
	}
	out := consolePut(t, e, "/guide", map[string]string{"guide": guide})
	view, _ := out["autopilot"].(map[string]any)
	out = consolePut(t, e, "", map[string]any{"enabled": true, "confirmed_allowed": view["allowed"]})
	if view, _ := out["autopilot"].(map[string]any); view["enabled"] != true {
		t.Fatalf("turn on: %v", out)
	}
}

func consoleAutopilotOff(t *testing.T, e *env) {
	t.Helper()
	if out := consolePut(t, e, "", map[string]bool{"enabled": false}); out["autopilot"].(map[string]any)["enabled"] != false {
		t.Fatalf("turn off: %v", out)
	}
}

// connectWith connects an MCP client whose requests pass through edit.
func connectWith(t *testing.T, endpoint string, edit func(*http.Request)) *mcp.ClientSession {
	t.Helper()
	hc := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		edit(r)
		return http.DefaultTransport.RoundTrip(r)
	})}
	c := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "v0"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: hc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// pausedClient returns a client whose next tools/call, once armed, stops
// after Handler took its Autopilot copy. It waits on paused until the
// request is held and finishes when resume is closed.
type pausedClient struct {
	cs     *mcp.ClientSession
	armed  atomic.Bool
	paused chan struct{}
	resume chan struct{}
}

func newPausedClient(t *testing.T, e *env) *pausedClient {
	t.Helper()
	p := &pausedClient{paused: make(chan struct{}), resume: make(chan struct{})}
	p.cs = connectWith(t, e.localURL, func(r *http.Request) {
		if r.Method == http.MethodPost && p.armed.Load() {
			r.Header.Set("X-Test-Pause", "1")
		}
	})
	var once sync.Once
	hook := func(r *http.Request) {
		if r.Header.Get("X-Test-Pause") == "" {
			return
		}
		once.Do(func() {
			close(p.paused)
			<-p.resume
		})
	}
	stateTaken.Store(&hook)
	t.Cleanup(func() { stateTaken.Store(nil) })
	return p
}

// call starts a tool call that will be held; read its result from the
// returned channel after closing resume.
func (p *pausedClient) call(name string, args map[string]any) <-chan *mcp.CallToolResult {
	done := make(chan *mcp.CallToolResult, 1)
	p.armed.Store(true)
	go func() {
		res, err := p.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			res = &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "protocol error: " + err.Error()}}}
		}
		done <- res
	}()
	<-p.paused
	return done
}

func resultText(res *mcp.CallToolResult) string {
	var texts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			texts = append(texts, tc.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// A console change reaches the very next MCP request: the decision tools
// appear and disappear with it, no tool reads or changes Experiments, and a
// call that took its copy while on is refused once the console turned
// Autopilot off (rules 3, 14, 15, 17).
func TestAutopilotConsoleToggleReachesMCP(t *testing.T) {
	e := newEnv(t)
	off := toolNames(t, e.local)
	for _, name := range off {
		if strings.Contains(name, "experiment") || strings.Contains(name, "autopilot") {
			t.Fatalf("tool %s while off", name)
		}
	}
	id := pendingDelete(t, e, "ap-toggle")

	consoleAutopilotOn(t, e, "", core.OpDeleteFlat)
	on := toolNames(t, e.local)
	want := append(slices.Clone(off), "approve_request", "reject_request")
	slices.Sort(on)
	slices.Sort(want)
	if !slices.Equal(on, want) {
		t.Fatalf("tools while on = %v, want %v", on, want)
	}

	p := newPausedClient(t, e)
	done := p.call("approve_request", map[string]any{"id": id, "agent": "late-bot"})
	consoleAutopilotOff(t, e)
	if names := toolNames(t, e.local); slices.Contains(names, "approve_request") || slices.Contains(names, "reject_request") {
		t.Fatalf("decision tools listed after the console turned Autopilot off: %v", names)
	}
	close(p.resume)
	res := <-done
	if text := resultText(res); !res.IsError || !strings.Contains(text, `"category":"autopilot_off"`) {
		t.Fatalf("held call after Turn off: %s", text)
	}
	if res, err := e.local.CallTool(context.Background(), &mcp.CallToolParams{Name: "approve_request", Arguments: map[string]any{"id": id, "agent": "late-bot"}}); err == nil && !res.IsError {
		t.Fatal("approve_request ran on the off server")
	}
	if a, _ := e.svc.GetApproval(context.Background(), id); a.Status != "pending" || a.DecidedBy != "" {
		t.Fatalf("refused calls changed the approval: %+v", a)
	}
}

// eventMessages returns every event message of a flat.
func eventMessages(t *testing.T, e *env, slug string) []string {
	t.Helper()
	evs, err := e.svc.Events(context.Background(), slug, "", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Message)
	}
	return out
}

func containsLine(lines []string, parts ...string) bool {
	return slices.ContainsFunc(lines, func(l string) bool {
		for _, p := range parts {
			if !strings.Contains(l, p) {
				return false
			}
		}
		return true
	})
}

// Requests made over MCP and approved over MCP by another session apply as
// the operator's approval does, with the agent's decision in the records
// (rules 18, 20, 26, 28).
func TestAutopilotAgentDecisionsEndToEnd(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	consoleAutopilotOn(t, e, "", core.OpCreateVersion, core.OpDeploy, core.OpMakePublic, core.OpDeleteFlat)
	const decider = `Autopilot (agent "pr-bot" via the console listener from 100.64.0.7)`

	// approve decides id from the tailnet-looking session and checks that
	// the result is what get_approval returns.
	approve := func(id string) ApprovalOut {
		t.Helper()
		var a, got ApprovalOut
		text, failed := call(t, e.remote, "approve_request", map[string]any{"id": id, "agent": "pr-bot"}, &a)
		if failed || a.Status != "approved" {
			t.Fatalf("approve %s: %s", id, text)
		}
		getText, _ := call(t, e.local, "get_approval", map[string]any{"id": id}, &got)
		ra, _ := json.Marshal(a)
		rg, _ := json.Marshal(got)
		if string(ra) != string(rg) || text != getText {
			t.Fatalf("approve_request result differs from get_approval:\n%s\n%s\n%s\n%s", ra, rg, text, getText)
		}
		d, ok := core.ParseAgentDecider(a.DecidedBy)
		if !ok || d.Agent != "pr-bot" || d.Arrival != (core.Arrival{Network: "listener", IP: "100.64.0.7"}) {
			t.Fatalf("decided_by = %q", a.DecidedBy)
		}
		return a
	}

	var saved saveFixtureOut
	if text, failed := call(t, e.local, "save_draft", map[string]any{"slug": "ap-e2e", "files": []any{file("index.html", "hello", "")}, "deploy": true}, &saved); failed {
		t.Fatal(text)
	}
	approve(saved.Deploy.ApprovalID)
	var dep DeployInfo
	if text, failed := call(t, e.local, "deploy", map[string]any{"slug": "ap-e2e", "version": 1}, &dep); failed {
		t.Fatal(text)
	}
	approve(dep.ApprovalID)
	f, err := e.svc.GetFlat(ctx, "ap-e2e")
	if err != nil || f.Versions != 1 || f.LiveVersion != 1 {
		t.Fatalf("after version and deploy: %+v %v", f, err)
	}
	if _, body := get(t, f.PrivateURL); string(body) != "hello" {
		t.Fatalf("deployed content: %q", body)
	}

	if code, _ := e.operatorCall(t, "POST", "/flats/ap-e2e/providers", `{"provider":"portal","permitted":true}`); code != 200 {
		t.Fatalf("provider grant: %d", code)
	}
	var vis ActionOut
	if text, failed := call(t, e.local, "set_visibility", map[string]any{"slug": "ap-e2e", "visibility": "public"}, &vis); failed || vis.Status != "pending_approval" {
		t.Fatalf("set_visibility: %s", text)
	}
	approve(vis.ApprovalID)
	if f, _ := e.svc.GetFlat(ctx, "ap-e2e"); f.Visibility != store.Public {
		t.Fatalf("visibility after agent approval: %s", f.Visibility)
	}

	events := eventMessages(t, e, "ap-e2e")
	for _, action := range []string{"create_version", "activate", "set_visibility"} {
		if !containsLine(events, action+" approved by "+decider) {
			t.Errorf("no %s decision event by Autopilot in %q", action, events)
		}
	}
	if !containsLine(events, "saved version 1", "via autopilot") || !containsLine(events, "version 1", "live", "via autopilot") {
		t.Errorf("applied operations do not say via autopilot: %q", events)
	}
	for _, ev := range events {
		// The provider grant is the operator's own console change.
		if strings.Contains(ev, "by the operator") || strings.Contains(ev, "via console") && !strings.HasPrefix(ev, "provider portal permitted") {
			t.Errorf("agent decision recorded as the operator's: %q", ev)
		}
	}

	var del ActionOut
	if text, failed := call(t, e.local, "delete_flat", map[string]any{"slug": "ap-e2e"}, &del); failed {
		t.Fatal(text)
	}
	a := approve(del.ApprovalID)
	if _, err := e.svc.GetFlat(ctx, "ap-e2e"); err == nil {
		t.Fatal("flat still exists after an approved deletion")
	}
	if kept, err := e.svc.GetApproval(ctx, del.ApprovalID); err != nil || kept.DecidedBy != a.DecidedBy || kept.Status != "approved" {
		t.Fatalf("approval record after deletion: %+v %v", kept, err)
	}
}

// consoleDecide decides an approval in the console without failing the
// test, so it can run on another goroutine.
func consoleDecide(e *env, id string, approve bool) (int, ApprovalOut, error) {
	action := "reject"
	if approve {
		action = "approve"
	}
	r, _ := http.NewRequest("POST", e.management.URL+"/console/api/approvals/"+id+"/"+action, nil)
	setOperatorHeaders(r, e.management.URL)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return 0, ApprovalOut{}, err
	}
	defer resp.Body.Close()
	var a ApprovalOut
	err = json.NewDecoder(resp.Body).Decode(&a)
	return resp.StatusCode, a, err
}

// The operator and an agent deciding one approval at the same time: exactly
// one decision is recorded; the other gets the approval unchanged (same
// direction) or a conflict (rules 20, 21).
func TestAutopilotConsoleAndAgentRace(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	consoleAutopilotOn(t, e, "", core.OpDeleteFlat)
	for i := range 8 {
		id := pendingDelete(t, e, fmt.Sprintf("ap-race-%d", i))
		agentApproves, consoleApproves := i%2 == 0, i%4 < 2
		tool := "reject_request"
		if agentApproves {
			tool = "approve_request"
		}
		args := map[string]any{"id": id, "agent": "race-bot"}
		var wg sync.WaitGroup
		var code int
		var consoleOut ApprovalOut
		var consoleErr error
		var agentRes *mcp.CallToolResult
		var agentErr error
		startConsole := func() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				code, consoleOut, consoleErr = consoleDecide(e, id, consoleApproves)
			}()
		}
		if i < 4 {
			// Both requests start together.
			startConsole()
			agentRes, agentErr = e.local.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		} else {
			// The agent's request is already inside the handler when the
			// console's starts.
			p := newPausedClient(t, e)
			done := p.call(tool, args)
			close(p.resume)
			startConsole()
			agentRes = <-done
		}
		wg.Wait()
		if consoleErr != nil || agentErr != nil {
			t.Fatalf("round %d: %v %v", i, consoleErr, agentErr)
		}
		stored, err := e.svc.GetApproval(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		_, agentWon := core.ParseAgentDecider(stored.DecidedBy)
		if !agentWon && stored.DecidedBy != "console" {
			t.Fatalf("round %d: decided_by = %q", i, stored.DecidedBy)
		}
		var agentOut ApprovalOut
		if !agentRes.IsError {
			raw, _ := json.Marshal(agentRes.StructuredContent)
			if err := json.Unmarshal(raw, &agentOut); err != nil {
				t.Fatal(err)
			}
		}
		// Each side either reports the stored decision or a conflict.
		consoleConflict := code == http.StatusConflict
		agentConflict := agentRes.IsError && strings.Contains(resultText(agentRes), `"category":"conflict"`)
		if !consoleConflict && (code != 200 || consoleOut.DecidedBy != stored.DecidedBy || consoleOut.Status != stored.Status) {
			t.Errorf("round %d: console got %d %+v, stored %+v", i, code, consoleOut, stored)
		}
		if !agentConflict && (agentRes.IsError || agentOut.DecidedBy != stored.DecidedBy || agentOut.Status != stored.Status) {
			t.Errorf("round %d: agent got %s, stored %+v", i, resultText(agentRes), stored)
		}
		if consoleConflict && agentConflict {
			t.Errorf("round %d: both decisions refused", i)
		}
		if agentApproves != consoleApproves && !consoleConflict && !agentConflict {
			t.Errorf("round %d: opposite decisions both succeeded", i)
		}
	}
}

// arrivalServer serves h with ctx added to every request.
func arrivalServer(t *testing.T, h http.Handler, wrap func(context.Context) context.Context) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(wrap(r.Context())))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/mcp"
}

// hostAddress returns a non-loopback, non-link-local IPv4 address of this
// machine, if it has one.
func hostAddress() (netip.Addr, bool) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return netip.Addr{}, false
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(n.IP)
		if ip = ip.Unmap(); ok && ip.Is4() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			return ip, true
		}
	}
	return netip.Addr{}, false
}

// Agent decisions record the network they arrived on, for every network
// the MCP endpoint is served on that a test can reach (rules 19, 26).
func TestAutopilotArrivalRecords(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	consoleAutopilotOn(t, e, "", core.OpDeleteFlat)
	h := Handler(e.svc, Options{Version: "test"})

	type arrivalCase struct {
		name string
		cs   func() *mcp.ClientSession
		want core.Arrival
	}
	cases := []arrivalCase{
		{"loopback", func() *mcp.ClientSession { return e.local }, core.Arrival{Network: "local"}},
		{"listener", func() *mcp.ClientSession { return e.remote }, core.Arrival{Network: "listener", IP: "100.64.0.7"}},
		{"loopback with forwarding headers", func() *mcp.ClientSession {
			return connectWith(t, e.localURL, func(r *http.Request) {
				r.Header.Set("X-Forwarded-For", "203.0.113.9")
				r.Header.Set("Forwarded", "for=203.0.113.9")
				r.Header.Set("X-Real-Ip", "203.0.113.9")
			})
		}, core.Arrival{Network: "local"}},
		{"tailscale login", func() *mcp.ClientSession {
			return connect(t, arrivalServer(t, h, func(c context.Context) context.Context {
				return core.WithTailnetPeer(core.WithConsoleDomain(c, "tailscale", "flats"), "bot@example.com", "ci-runner")
			}))
		}, core.Arrival{Network: "tailscale", Login: "bot@example.com"}},
		{"tailscale tagged node", func() *mcp.ClientSession {
			return connect(t, arrivalServer(t, h, func(c context.Context) context.Context {
				return core.WithTailnetPeer(core.WithConsoleDomain(c, "tailscale", "flats"), "", "ci-runner")
			}))
		}, core.Arrival{Network: "tailscale", Node: "ci-runner"}},
		{"netbird peer", func() *mcp.ClientSession {
			return connect(t, arrivalServer(t, h, func(c context.Context) context.Context {
				return core.WithNetbirdPeer(core.WithConsoleDomain(c, "netbird", "flats"), "ci.netbird.cloud")
			}))
		}, core.Arrival{Network: "netbird", Peer: "ci.netbird.cloud"}},
		{"netbird unknown peer", func() *mcp.ClientSession {
			return connect(t, arrivalServer(t, h, func(c context.Context) context.Context {
				return core.WithConsoleDomain(c, "netbird", "flats")
			}))
		}, core.Arrival{Network: "netbird"}},
		{"external origin", func() *mcp.ClientSession {
			srv := httptest.NewServer(&api.HostGuard{Hosts: func() []string { return nil },
				ExternalOrigins: func() []string { return []string{"http://flats.example.com"} }, Next: h})
			t.Cleanup(srv.Close)
			return connectWith(t, srv.URL+"/mcp", func(r *http.Request) { r.Host = "flats.example.com" })
		}, core.Arrival{Network: "external", Origin: "http://flats.example.com"}},
	}
	if ip, ok := hostAddress(); ok {
		cases = append(cases, arrivalCase{"wildcard listener reached through a host address", func() *mcp.ClientSession {
			ln, err := net.Listen("tcp", "0.0.0.0:0")
			if err != nil {
				t.Fatal(err)
			}
			srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: h}}
			srv.Start()
			t.Cleanup(srv.Close)
			port := ln.Addr().(*net.TCPAddr).Port
			return connect(t, fmt.Sprintf("http://%s/mcp", netip.AddrPortFrom(ip, uint16(port))))
		}, core.Arrival{Network: "local"}})
	} else {
		t.Log("no non-loopback host address: the wildcard listener case is skipped")
	}

	for i, c := range cases {
		cs := c.cs()
		for _, tool := range []string{"reject_request", "approve_request"} {
			slug := fmt.Sprintf("ap-arrival-%d-%s", i, strings.TrimSuffix(tool, "_request"))
			id := pendingDelete(t, e, slug)
			var a ApprovalOut
			if text, failed := call(t, cs, tool, map[string]any{"id": id, "agent": "net-bot"}, &a); failed {
				t.Fatalf("%s %s: %s", c.name, tool, text)
			}
			d, ok := core.ParseAgentDecider(a.DecidedBy)
			if !ok || d.Agent != "net-bot" || d.Arrival != c.want {
				t.Errorf("%s %s: decided_by = %q, want %+v", c.name, tool, a.DecidedBy, c.want)
			}
			if tool == "reject_request" {
				want := "delete rejected by " + core.DeciderText(a.DecidedBy)
				if events := eventMessages(t, e, slug); !containsLine(events, want) {
					t.Errorf("%s: no event %q in %q", c.name, want, events)
				}
			} else if _, err := e.svc.GetFlat(ctx, slug); err == nil {
				t.Errorf("%s: approved deletion did not apply", c.name)
			}
		}
	}
}

// get_approval names the agent tools only for a pending approval whose
// operations are all allowed now (rule 23).
func TestAutopilotGetApprovalTextFollowsConsole(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	const today = "Waiting for the operator to decide in the Flats console."
	const agents = "or an agent with approve_request or reject_request, since Autopilot allows its operations."
	consoleAutopilotOn(t, e, "", core.OpDeleteFlat)

	allowed := pendingDelete(t, e, "ap-text-allowed")
	var saved saveFixtureOut
	if text, failed := call(t, e.local, "save_draft", map[string]any{"slug": "ap-text-version", "files": []any{file("index.html", "v", "")}, "deploy": true}, &saved); failed {
		t.Fatal(text)
	}
	notAllowed := saved.Deploy.ApprovalID
	unclassified := "apr-unclassified"
	if err := e.st.InsertApproval(ctx, store.Approval{ID: unclassified, Flat: "ap-text-version", Action: "set_visibility",
		Params: json.RawMessage(`{"visibility":"public"}`), Status: "pending", Via: "mcp"}); err != nil {
		t.Fatal(err)
	}

	text := func(id string) string {
		t.Helper()
		got, failed := call(t, e.local, "get_approval", map[string]any{"id": id}, nil)
		if failed {
			t.Fatal(got)
		}
		return got
	}
	if got := text(allowed); !strings.Contains(got, agents) || strings.Contains(got, today) {
		t.Errorf("allowed: %s", got)
	}
	for _, id := range []string{notAllowed, unclassified} {
		if got := text(id); !strings.Contains(got, today) || strings.Contains(got, agents) {
			t.Errorf("%s: %s", id, got)
		}
	}
	consolePut(t, e, "/operations/delete_flat", map[string]bool{"allowed": false})
	if got := text(allowed); !strings.Contains(got, today) || strings.Contains(got, agents) {
		t.Errorf("after the console disallowed delete_flat: %s", got)
	}
}

// Two overlapping MCP requests each render the guide from their own copy
// of the settings, even when the console changes them in between (5.1, 5.9).
func TestAutopilotOverlappingRequestsKeepTheirCopy(t *testing.T) {
	e := newEnv(t)
	limit := e.svc.UploadLimit()
	consoleAutopilotOn(t, e, "guide A", core.OpDeploy)
	deployOnly, _ := core.ParseOpSet([]string{"deploy"})
	first := core.Autopilot{Enabled: true, Allowed: deployOnly, Guide: "guide A"}

	p := newPausedClient(t, e)
	done := p.call("guide", map[string]any{"items": []string{"topic.autopilot"}})
	consolePut(t, e, "/operations/make_public", map[string]bool{"allowed": true})
	consolePut(t, e, "/guide", map[string]string{"guide": "guide B"})
	second := first
	second.Guide = "guide B"
	second.Allowed, _ = core.ParseOpSet([]string{"deploy", "make_public"})

	var out2 GuideOut
	if text, failed := call(t, e.local, "guide", map[string]any{"items": []string{"topic.autopilot"}}, &out2); failed {
		t.Fatal(text)
	}
	close(p.resume)
	res := <-done
	if res.IsError {
		t.Fatal(resultText(res))
	}
	var out1 GuideOut
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &out1); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name     string
		out      GuideOut
		state    core.Autopilot
		has, not []string
	}{
		{"held request", out1, first, []string{"guide A", "(`deploy`)"}, []string{"guide B", "(`make_public`)"}},
		{"later request", out2, second, []string{"guide B", "(`deploy`)", "(`make_public`)"}, []string{"guide A"}},
	} {
		page := c.out.Items[0].Markdown
		for _, s := range c.has {
			if !strings.Contains(page, s) {
				t.Errorf("%s: topic.autopilot lacks %q", c.name, s)
			}
		}
		for _, s := range c.not {
			if strings.Contains(page, s) {
				t.Errorf("%s: topic.autopilot has %q of the other copy", c.name, s)
			}
		}
		if want := runtimeref.AutopilotGuideRevision(limit, *c.state.GuideState()); c.out.GuideRevision != want {
			t.Errorf("%s: guide_revision %s, want %s", c.name, c.out.GuideRevision, want)
		}
	}
}
