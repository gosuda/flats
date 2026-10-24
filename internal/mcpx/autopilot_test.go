package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/clientstate"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

// setAutopilot allows exactly ops, sets the operation guide and turns
// Autopilot on.
func setAutopilot(t *testing.T, e *env, guide string, ops ...core.AutopilotOp) {
	t.Helper()
	ctx := context.Background()
	for _, op := range core.AutopilotOps {
		if _, err := e.svc.SetAutopilotOp(ctx, op, slices.Contains(ops, op), "test"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.svc.SetAutopilotGuide(ctx, guide, "test"); err != nil {
		t.Fatal(err)
	}
	confirmed := e.svc.Autopilot().Allowed
	if a, err := e.svc.SetAutopilotEnabled(ctx, true, &confirmed, "test"); err != nil || !a.Enabled {
		t.Fatalf("turn on: %+v %v", a, err)
	}
}

func autopilotOff(t *testing.T, e *env) {
	t.Helper()
	if a, err := e.svc.SetAutopilotEnabled(context.Background(), false, nil, "test"); err != nil || a.Enabled {
		t.Fatalf("turn off: %+v %v", a, err)
	}
}

// stubHostAddrs makes addrs this host's own addresses.
func stubHostAddrs(t *testing.T, addrs ...string) {
	t.Helper()
	var parsed []netip.Addr
	for _, a := range addrs {
		parsed = append(parsed, netip.MustParseAddr(a))
	}
	old := isHostAddr
	isHostAddr = func(src netip.Addr) bool { return slices.Contains(parsed, src) }
	t.Cleanup(func() { isHostAddr = old })
}

// pendingDelete creates a website flat and requests its deletion.
func pendingDelete(t *testing.T, e *env, slug string) string {
	t.Helper()
	if text, failed := call(t, e.local, "create_flat", map[string]any{"slug": slug}, nil); failed {
		t.Fatal(text)
	}
	var out ActionOut
	if text, failed := call(t, e.local, "delete_flat", map[string]any{"slug": slug}, &out); failed || out.Status != "pending_approval" {
		t.Fatalf("delete request: %s", text)
	}
	return out.ApprovalID
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// agentSurface captures everything an agent can read without changing
// state, from a client that connects anew: instructions, the tool list,
// every guide item, check_client, get_client_skill, both resources, the
// llms documents, GET /api/client and get_approval of approvalID.
func agentSurface(t *testing.T, e *env, approvalID string) map[string]string {
	t.Helper()
	ctx := context.Background()
	cs := connect(t, e.localURL)
	m := map[string]string{"instructions": cs.InitializeResult().Instructions}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(tools)
	m["tools/list"] = string(raw)
	read := func(name string, args map[string]any) {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(res)
		m[fmt.Sprintf("%s %v", name, args)] = string(raw)
	}
	for _, id := range append(guideItems(), "topic.autopilot", "refusal.autopilot_off", "refusal.autopilot_not_allowed") {
		read("guide", map[string]any{"items": []string{id}})
	}
	read("guide", map[string]any{})
	read("check_client", map[string]any{"cli_present": false, "guide_revision": strings.Repeat("0", 64)})
	read("check_client", map[string]any{"guide_revision": runtimeref.GuideRevision(e.svc.UploadLimit())})
	read("get_client_skill", map[string]any{})
	read("get_approval", map[string]any{"id": approvalID})
	for _, uri := range []string{runtimeref.URI, runtimeref.ContentTypesURI} {
		res, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(res)
		m[uri] = string(raw)
	}
	llms := httptest.NewServer(LLMsHandler(e.svc, Options{Version: "test"}))
	defer llms.Close()
	for _, path := range LLMsPaths {
		_, _, body := fetch(t, llms.Client(), "GET", llms.URL+path)
		m[path] = strings.ReplaceAll(body, llms.URL, "ORIGIN")
	}
	_, _, m["/api/client"] = fetch(t, http.DefaultClient, "GET", e.management.URL+"/api/client")
	return m
}

// Principle 2: with Autopilot off, every agent-visible output is today's,
// whether Autopilot was never on or was turned on, fully configured and
// turned off again in the console. The operator's decisions then keep
// today's records.
func TestAutopilotOffIdentity(t *testing.T) {
	stubHostAddrs(t)
	e := newEnv(t)
	id := pendingDelete(t, e, "ap-identity")
	never := agentSurface(t, e, id)
	if never["instructions"] != runtimeref.Instructions(e.svc.UploadLimit()) {
		t.Fatal("off instructions are not today's")
	}
	for _, key := range []string{"tools/list"} {
		if strings.Contains(never[key], "approve_request") || strings.Contains(never[key], "topic.autopilot") {
			t.Fatalf("%s mentions Autopilot while off", key)
		}
	}
	if !strings.Contains(never[`guide map[items:[topic.autopilot]]`], `"unknown":["topic.autopilot"]`) {
		t.Fatalf("topic.autopilot exists while off: %s", never[`guide map[items:[topic.autopilot]]`])
	}

	consoleAutopilotOn(t, e, "Approve PR previews.\n```\n## heading", core.AutopilotOps...)
	on := agentSurface(t, e, id)
	for _, key := range []string{"instructions", "tools/list", `guide map[items:[topic.approvals]]`, `get_approval map[id:` + id + `]`} {
		if on[key] == never[key] {
			t.Errorf("%s did not change while on", key)
		}
	}
	consoleAutopilotOff(t, e)

	again := agentSurface(t, e, id)
	if len(again) != len(never) {
		t.Fatalf("captured %d outputs, want %d", len(again), len(never))
	}
	for key, want := range never {
		if again[key] != want {
			t.Errorf("%s differs after on and off:\nnever on: %.400s\non-off:   %.400s", key, want, again[key])
		}
	}

	var saved saveFixtureOut
	if text, failed := call(t, e.local, "save_draft", map[string]any{"slug": "ap-identity-site", "files": []any{file("index.html", "hi", "")}, "deploy": true}, &saved); failed {
		t.Fatal(text)
	}
	e.approve(t, saved.Deploy.ApprovalID)
	if a, _ := e.svc.GetApproval(context.Background(), saved.Deploy.ApprovalID); a.DecidedBy != "console" {
		t.Errorf("operator decided_by = %q", a.DecidedBy)
	}
	events := eventMessages(t, e, "ap-identity-site")
	if !containsLine(events, "create_version approved by the operator: saved version 1") || !containsLine(events, "saved version 1", "via console") || containsLine(events, "autopilot") {
		t.Errorf("operator events after Autopilot was on and off: %q", events)
	}
}

// census lists phrases that claim agents cannot decide approvals; none of
// them may remain in a text agents read while Autopilot is on.
var census = []string{"no approval tool", "no approval or provider-grant tool", "approve your own", "ask the user before",
	"ask the user first", "agents never decide", "operator approval", "operator's approval", "until the operator approves",
	"wait for the operator", "waits for the operator"}

func TestAutopilotOnTextsCensus(t *testing.T) {
	e := newEnv(t)
	offTools, err := e.local.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	setAutopilot(t, e, "", core.OpDeploy)
	cs := connect(t, e.localURL)
	texts := map[string]string{"instructions": cs.InitializeResult().Instructions}
	onTools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range onTools.Tools {
		schema, _ := json.Marshal(tool.InputSchema)
		texts["tool "+tool.Name] = tool.Description + string(schema)
	}
	for _, id := range autopilotGuideItems() {
		var out GuideOut
		text, failed := call(t, cs, "guide", map[string]any{"items": []string{id}}, &out)
		if failed || len(out.Items) != 1 {
			t.Fatalf("%s: %s", id, text)
		}
		texts[id] = out.Items[0].Markdown
	}
	for name, text := range texts {
		lower := strings.ToLower(strings.Join(strings.Fields(text), " "))
		for _, phrase := range census {
			if strings.Contains(lower, phrase) {
				t.Errorf("%s still says %q", name, phrase)
			}
		}
	}

	// Each tool replacement matches its off description exactly once, and
	// the on description is the off one with the replacements.
	off := map[string]*mcp.Tool{}
	for _, tool := range offTools.Tools {
		off[tool.Name] = tool
	}
	for _, r := range autopilotToolReplacements {
		if n := strings.Count(off[r.tool].Description, r.old); n != 1 {
			t.Errorf("%s: %q occurs %d times", r.tool, r.old, n)
		}
	}
	for _, tool := range onTools.Tools {
		if o := off[tool.Name]; o != nil && tool.Description != autopilotDescription(tool.Name, o.Description) {
			t.Errorf("%s on description is not the off one with replacements", tool.Name)
		}
	}
	for _, name := range autopilotDecidable {
		if !strings.Contains(texts["tool "+name], autopilotToolNote) {
			t.Errorf("%s does not mention approve_request", name)
		}
	}

	// rollback's on input schema differs only in restore_data's description.
	var onRollback *mcp.Tool
	for _, tool := range onTools.Tools {
		if tool.Name == "rollback" {
			onRollback = tool
		}
	}
	offSchema, _ := json.Marshal(off["rollback"].InputSchema)
	onSchema, _ := json.Marshal(onRollback.InputSchema)
	if got := strings.Replace(string(offSchema), "Frozen for explicit operator approval.", "Frozen for approval.", 1); got != string(onSchema) {
		t.Errorf("rollback schemas differ beyond restore_data:\noff %s\non  %s", offSchema, onSchema)
	}
}

func TestAutopilotGuide(t *testing.T) {
	e := newEnv(t)
	setAutopilot(t, e, "Name flats pr-<number>.\n```\n# not a heading", core.OpCreateVersion, core.OpMakePublic)
	var out GuideOut
	text, failed := call(t, e.local, "guide", map[string]any{"items": []string{"topic.index", "topic.autopilot", "refusal.autopilot_off"}}, &out)
	if failed || len(out.Items) != 3 {
		t.Fatal(text)
	}
	if !slices.Equal(out.Available, autopilotGuideItems()) || !strings.Contains(out.Items[0].Markdown, "`topic.autopilot`") {
		t.Fatalf("index does not route to topic.autopilot: %v", out.Available)
	}
	page := out.Items[1].Markdown
	for _, want := range []string{"* Create versions (`create_version`)", "* Make Public (`make_public`)", "````\nName flats pr-<number>.\n```\n# not a heading\n````\n"} {
		if !strings.Contains(page, want) {
			t.Errorf("topic.autopilot lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "(`deploy`)") {
		t.Error("topic.autopilot lists an operation that is not allowed")
	}
	if !strings.HasPrefix(out.Items[2].Markdown, runtimeref.RefusalIntro) {
		t.Error("refusal page without its intro")
	}
	ins := e.local.InitializeResult().Instructions // connected while off
	call(t, e.local, "guide", map[string]any{"items": []string{"topic.instructions"}}, &out)
	if !strings.Contains(out.Items[0].Markdown, runtimeref.AutopilotInstructions(e.svc.UploadLimit())) || strings.Contains(out.Items[0].Markdown, ins) {
		t.Error("topic.instructions must be the on instructions")
	}
	want := runtimeref.AutopilotGuideRevision(e.svc.UploadLimit(), *e.svc.Autopilot().GuideState())
	if out.GuideRevision != want || !strings.Contains(text, "topic.autopilot") {
		t.Errorf("guide revision %s, want %s", out.GuideRevision, want)
	}
}

// Overlapping requests with different copies each render a consistent page
// and revision.
func TestAutopilotGuideStatePairing(t *testing.T) {
	a := &runtimeref.AutopilotState{Allowed: []runtimeref.AutopilotOperation{{ID: "deploy", Label: "Deploy"}}, Guide: "guide A"}
	b := &runtimeref.AutopilotState{Allowed: []runtimeref.AutopilotOperation{}, Guide: "guide B"}
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := a
			if i%2 == 1 {
				st = b
			}
			out := guide([]string{"topic.autopilot"}, 1000, st)
			if out.GuideRevision != runtimeref.AutopilotGuideRevision(1000, *st) || !strings.Contains(out.Items[0].Markdown, st.Guide) {
				t.Errorf("request %d mixed states", i)
			}
		}()
	}
	wg.Wait()
}

func TestAutopilotDecisionTools(t *testing.T) {
	stubHostAddrs(t)
	e := newEnv(t)
	ctx := context.Background()
	approveID := pendingDelete(t, e, "ap-approve")
	rejectID := pendingDelete(t, e, "ap-reject")

	if slices.Contains(toolNames(t, e.local), "approve_request") {
		t.Fatal("approve_request is listed while Autopilot is off")
	}
	setAutopilot(t, e, "")
	names := toolNames(t, e.local)
	if !slices.Contains(names, "approve_request") || !slices.Contains(names, "reject_request") {
		t.Fatalf("decision tools missing while on: %v", names)
	}

	// Not allowed yet: refused for both directions, nothing changes, and
	// get_approval keeps today's text.
	for _, tool := range []string{"approve_request", "reject_request"} {
		text, failed := call(t, e.local, tool, map[string]any{"id": approveID, "agent": "pr-bot"}, nil)
		if !failed || !strings.Contains(text, `"category":"autopilot_not_allowed"`) || !strings.Contains(text, "Delete flats (delete_flat)") ||
			!strings.Contains(text, "See guide refusal.autopilot_not_allowed.") {
			t.Fatalf("%s while not allowed: %s", tool, text)
		}
	}
	text, _ := call(t, e.local, "get_approval", map[string]any{"id": approveID}, nil)
	if !strings.Contains(text, "Waiting for the operator to decide in the Flats console.") {
		t.Fatalf("pending text while not allowed: %s", text)
	}

	setAutopilot(t, e, "", core.OpDeleteFlat)
	text, _ = call(t, e.local, "get_approval", map[string]any{"id": approveID}, nil)
	if !strings.Contains(text, "or an agent with approve_request or reject_request, since Autopilot allows its operations.") {
		t.Fatalf("pending text while allowed: %s", text)
	}

	// Invalid calls.
	for _, c := range []struct {
		args     map[string]any
		category string
	}{
		{map[string]any{"id": approveID, "agent": " "}, "invalid"},
		{map[string]any{"id": approveID, "agent": strings.Repeat("é", 101)}, "invalid"},
		{map[string]any{"id": "apr-missing", "agent": "pr-bot"}, "not_found"},
	} {
		if text, failed := call(t, e.local, "approve_request", c.args, nil); !failed || !strings.Contains(text, `"category":"`+c.category+`"`) {
			t.Errorf("%v: %s", c.args, text)
		}
	}

	// Approve from a tailnet-looking peer on the console listener.
	var a ApprovalOut
	text, failed := call(t, e.remote, "approve_request", map[string]any{"id": approveID, "agent": "pr-bot"}, &a)
	if failed || a.Status != "approved" {
		t.Fatalf("approve: %s", text)
	}
	d, ok := core.ParseAgentDecider(a.DecidedBy)
	if !ok || d.Agent != "pr-bot" || d.Arrival != (core.Arrival{Network: "listener", IP: "100.64.0.7"}) {
		t.Fatalf("decided_by = %q", a.DecidedBy)
	}
	// The same direction again answers unchanged; the other is a conflict.
	var again ApprovalOut
	if text, failed := call(t, e.local, "approve_request", map[string]any{"id": approveID, "agent": "other"}, &again); failed || again.DecidedBy != a.DecidedBy {
		t.Fatalf("repeat approve: %s", text)
	}
	if text, failed := call(t, e.local, "reject_request", map[string]any{"id": approveID, "agent": "pr-bot"}, nil); !failed || !strings.Contains(text, `"category":"conflict"`) {
		t.Fatalf("opposite decision: %s", text)
	}

	// Reject from this host: recorded as Local, with the event text.
	text, failed = call(t, e.local, "reject_request", map[string]any{"id": rejectID, "agent": "local-bot"}, &a)
	if failed || a.Status != "rejected" {
		t.Fatalf("reject: %s", text)
	}
	if d, ok := core.ParseAgentDecider(a.DecidedBy); !ok || d.Arrival.Network != "local" {
		t.Fatalf("decided_by = %q", a.DecidedBy)
	}
	events, err := e.svc.Events(ctx, "ap-reject", "approval", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		found = found || strings.Contains(ev.Message, `rejected by Autopilot (agent "local-bot" via Local)`)
	}
	if !found {
		t.Fatalf("no Autopilot rejection event: %+v", events)
	}

	// A call that reached the on server after Autopilot was turned off is
	// refused by core.
	third := pendingDelete(t, e, "ap-late")
	autopilotOff(t, e)
	onState := requestState{Autopilot: core.Autopilot{Enabled: true, Allowed: ^core.OpSet(0)}, Arrival: core.Arrival{Network: "local"}}
	_, _, err = (&tools{svc: e.svc, autopilot: true}).decide(context.WithValue(ctx, stateKey{}, onState), DecideIn{ID: third, Agent: "late"}, true)
	if err == nil || !strings.Contains(err.Error(), `"category":"autopilot_off"`) || !strings.Contains(err.Error(), "See guide refusal.autopilot_off.") {
		t.Fatalf("late call: %v", err)
	}
	if slices.Contains(toolNames(t, e.local), "approve_request") {
		t.Fatal("approve_request is still listed after Autopilot was turned off")
	}
	if res, err := e.local.CallTool(ctx, &mcp.CallToolParams{Name: "approve_request", Arguments: map[string]any{"id": third, "agent": "late"}}); err == nil && !res.IsError {
		t.Fatal("the off server ran approve_request")
	}
	if ap, _ := e.svc.GetApproval(ctx, third); ap.Status != "pending" {
		t.Fatalf("refused calls changed the approval: %+v", ap)
	}
}

// An approval that fails while applying after an agent's decision is a
// decision that took effect: the tool returns it with its failure code. A
// stale approval is a conflict and stays a tool error, as in the console.
func TestAutopilotFailedDecisionIsAResult(t *testing.T) {
	stubHostAddrs(t)
	e := newEnv(t)
	var out saveFixtureOut
	if text, failed := call(t, e.local, "save_draft", map[string]any{"slug": "ap-site", "files": []any{file("index.html", "ok", "")}, "deploy": true}, &out); failed {
		t.Fatal(text)
	}
	e.approve(t, out.Deploy.ApprovalID)
	if text, failed := call(t, e.local, "save_draft", map[string]any{"slug": "ap-site", "deploy": true, "files": []any{file("index.html", "broken", ""), file("flats.json", `{"health":"/missing"}`, "")}}, &out); failed {
		t.Fatal(text)
	}
	e.approve(t, out.Deploy.ApprovalID)
	var dep DeployInfo
	if text, failed := call(t, e.local, "deploy", map[string]any{"slug": "ap-site", "version": 2}, &dep); failed {
		t.Fatal(text)
	}
	setAutopilot(t, e, "", core.OpDeploy, core.OpCreateVersion)
	var a ApprovalOut
	text, failed := call(t, e.local, "approve_request", map[string]any{"id": dep.ApprovalID, "agent": "pr-bot"}, &a)
	if failed || a.Status != "failed" || a.ResultData == nil || a.ResultData.FailureCode != "health_check_failed" ||
		!strings.Contains(text, "Failure code: health_check_failed") {
		t.Fatalf("failed deploy: %v %s", failed, text)
	}

	if text, failed := call(t, e.local, "save_draft", map[string]any{"slug": "ap-site", "files": []any{file("index.html", "ONE", "")}, "deploy": true}, &out); failed {
		t.Fatal(text)
	}
	if text, failed := call(t, e.local, "save_draft", map[string]any{"slug": "ap-site", "files": []any{file("index.html", "TWO", "")}}, nil); failed {
		t.Fatal(text)
	}
	text, failed = call(t, e.local, "approve_request", map[string]any{"id": out.Deploy.ApprovalID, "agent": "pr-bot"}, nil)
	if !failed || !strings.Contains(text, `"category":"stale_approval"`) {
		t.Fatalf("stale approval: %s", text)
	}
	// The refused call still claimed the approval, as a console decision does.
	if text, failed := call(t, e.local, "get_approval", map[string]any{"id": out.Deploy.ApprovalID}, &a); failed ||
		a.Status != "failed" || a.ResultData == nil || a.ResultData.FailureCode != "stale_approval" {
		t.Fatalf("stale approval after decision: %v %s", failed, text)
	}
}

func TestAutopilotGetApprovalApplyingText(t *testing.T) {
	decided := core.EncodeAgentDecider(core.AgentDecision{Agent: "pr-bot", Arrival: core.Arrival{Network: "local"}})
	got := (&tools{}).approvalText(context.Background(), storeApproval("applying", decided), ApprovalOut{}, requestState{})
	if !strings.Contains(got, `Autopilot (agent "pr-bot" via Local) approved it; the operation is still applying.`) {
		t.Errorf("agent applying text: %s", got)
	}
	got = (&tools{}).approvalText(context.Background(), storeApproval("applying", "alice@example.com"), ApprovalOut{}, requestState{})
	if !strings.Contains(got, " The operator approved it; the operation is still applying.") {
		t.Errorf("operator applying text: %s", got)
	}
}

func storeApproval(status, decidedBy string) store.Approval {
	return store.Approval{ID: "apr-x", Flat: "blog", Action: "activate", Status: status, DecidedBy: decidedBy}
}

func TestAutopilotCheckClient(t *testing.T) {
	e := newEnv(t)
	limit := e.svc.UploadLimit()
	offRev := runtimeref.GuideRevision(limit)
	var off clientstate.CheckResult
	if text, failed := call(t, e.local, "check_client", map[string]any{"guide_revision": offRev, "cli_present": false}, &off); failed || off.GuideStatus != "current" {
		t.Fatalf("off: %s", text)
	}

	setAutopilot(t, e, "guide", core.OpDeploy)
	// An existing client that read the off guide decodes today's result,
	// unchanged except that its guide needs a refresh.
	var compat clientstate.CheckResult
	text, failed := call(t, e.local, "check_client", map[string]any{"guide_revision": offRev, "cli_present": false}, &compat)
	if failed || compat.GuideStatus != "refresh_required" {
		t.Fatalf("on: %s", text)
	}
	onRev := compat.GuideRevision
	if onRev == offRev || onRev != runtimeref.AutopilotGuideRevision(limit, *e.svc.Autopilot().GuideState()) {
		t.Fatal("check_client reports the wrong guide revision while on")
	}
	want := off
	want.GuideRevision, want.GuideStatus = onRev, "refresh_required"
	if compat != want {
		t.Fatalf("existing fields changed:\n%+v\n%+v", compat, want)
	}
	var on checkResultAutopilot
	call(t, e.local, "check_client", map[string]any{"guide_revision": onRev}, &on)
	if on.GuideStatus != "current" || !on.Autopilot.Enabled || !slices.Equal(on.Autopilot.Allowed, []string{"deploy"}) || on.Autopilot.Read != "guide topic.autopilot" {
		t.Fatalf("on result: %+v", on)
	}
	var skill clientstate.Manifest
	call(t, e.local, "get_client_skill", map[string]any{}, &skill)
	_, _, body := fetch(t, http.DefaultClient, "GET", e.management.URL+"/api/client")
	var apiOut clientstate.Manifest
	if err := json.Unmarshal([]byte(body), &apiOut); err != nil {
		t.Fatal(err)
	}
	if skill.GuideRevision != onRev || apiOut.GuideRevision != onRev {
		t.Fatalf("get_client_skill %s and /api/client %s disagree with %s", skill.GuideRevision, apiOut.GuideRevision, onRev)
	}

	setAutopilot(t, e, "guide", core.OpDeploy, core.OpRollback)
	call(t, e.local, "check_client", map[string]any{"guide_revision": onRev}, &on)
	if on.GuideStatus != "refresh_required" {
		t.Fatal("a switch change must refresh the guide")
	}
	autopilotOff(t, e)
	call(t, e.local, "check_client", map[string]any{"guide_revision": offRev}, &off)
	if off.GuideStatus != "current" || off.GuideRevision != offRev {
		t.Fatalf("off again: %+v", off)
	}
}

// The address matching itself (zones, IPv4 link-local, re-reading the
// interfaces) is tested with api.IsHostAddr.
func TestHostOrigin(t *testing.T) {
	stubHostAddrs(t, "192.168.1.10", "2001:db8::10", "fe80::10%en0")
	for _, c := range []struct {
		remote string
		want   core.Arrival
	}{
		{"127.0.0.1:4000", core.Arrival{Network: "local"}},
		{"[::1]:4000", core.Arrival{Network: "local"}},
		{"192.168.1.10:4000", core.Arrival{Network: "local"}},
		{"[2001:db8::10]:4000", core.Arrival{Network: "local"}},
		{"[::ffff:192.168.1.10]:4000", core.Arrival{Network: "local"}},
		{"[fe80::10%en0]:4000", core.Arrival{Network: "local"}},
		{"[fe80::10%en1]:4000", core.Arrival{Network: "listener", IP: "fe80::10%en1"}},
		{"[fe80::20%en0]:4000", core.Arrival{Network: "listener", IP: "fe80::20%en0"}},
		{"192.168.1.20:4000", core.Arrival{Network: "listener", IP: "192.168.1.20"}},
		{"[::ffff:192.168.1.20]:4000", core.Arrival{Network: "listener", IP: "192.168.1.20"}},
		{"[2001:db8::20]:4000", core.Arrival{Network: "listener", IP: "2001:db8::20"}},
	} {
		r := httptest.NewRequest("POST", "/mcp", nil)
		r.RemoteAddr = c.remote
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		r.Header.Set("Forwarded", "for=127.0.0.1")
		r.Header.Set("X-Real-Ip", "127.0.0.1")
		if got := hostOrigin(r); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.remote, got, c.want)
		}
	}
}

func TestArrivalOf(t *testing.T) {
	stubHostAddrs(t)
	base := func() *http.Request {
		r := httptest.NewRequest("POST", "/mcp", nil)
		r.RemoteAddr = "100.100.1.2:4000"
		return r
	}
	with := func(f func(context.Context) context.Context) core.Arrival {
		r := base()
		return arrivalOf(r.WithContext(f(r.Context())))
	}
	for name, c := range map[string]struct {
		got, want core.Arrival
	}{
		"tailscale login": {with(func(ctx context.Context) context.Context {
			return core.WithTailnetPeer(core.WithConsoleDomain(ctx, "tailscale", "flats"), "bot@example.com", "ci-runner")
		}), core.Arrival{Network: "tailscale", Login: "bot@example.com"}},
		"tailscale tagged": {with(func(ctx context.Context) context.Context {
			return core.WithTailnetPeer(core.WithConsoleDomain(ctx, "tailscale", "flats"), "", "ci-runner")
		}), core.Arrival{Network: "tailscale", Node: "ci-runner"}},
		"netbird peer": {with(func(ctx context.Context) context.Context {
			return core.WithNetbirdPeer(core.WithConsoleDomain(ctx, "netbird", "flats"), "ci.netbird.cloud")
		}), core.Arrival{Network: "netbird", Peer: "ci.netbird.cloud"}},
		"netbird unknown": {with(func(ctx context.Context) context.Context {
			return core.WithConsoleDomain(ctx, "netbird", "flats")
		}), core.Arrival{Network: "netbird"}},
		"listener": {arrivalOf(base()), core.Arrival{Network: "listener", IP: "100.100.1.2"}},
	} {
		if c.got != c.want {
			t.Errorf("%s: %+v, want %+v", name, c.got, c.want)
		}
	}

	var got core.Arrival
	guard := &api.HostGuard{Hosts: func() []string { return nil }, ExternalOrigins: func() []string { return []string{"https://flats.example.com"} },
		Next: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = arrivalOf(r) })}
	r := base()
	r.Host = "flats.example.com"
	guard.ServeHTTP(httptest.NewRecorder(), r)
	if got != (core.Arrival{Network: "external", Origin: "https://flats.example.com"}) {
		t.Errorf("external: %+v", got)
	}
}

// Autopilot refusal categories exist only while on, each with its page.
func TestAutopilotRefusalPagesCoverCategories(t *testing.T) {
	got := runtimeref.AutopilotRefusalCategories()
	want := slices.Clone(core.AutopilotErrorCategories)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("Autopilot refusal pages = %v, want %v", got, want)
	}
	for _, c := range want {
		if slices.Contains(core.ErrorCategories, c) {
			t.Errorf("%s must not be an always-on category", c)
		}
	}
}
