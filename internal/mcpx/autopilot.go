package mcpx

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/clientstate"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

// requestState is the Autopilot copy one HTTP request works with. Handler
// reads the settings once per request; the server choice, guide pages,
// guide revision and approval texts of that request all use this copy.
// Agent decisions check the stored settings again in core.
type requestState struct {
	Autopilot core.Autopilot
	Guide     *runtimeref.AutopilotState // Autopilot.GuideState(); nil while off
	Arrival   core.Arrival               // set only while Autopilot is on
}

type stateKey struct{}

// stateOf returns the request's copy; without one (an in-memory session)
// Autopilot is off.
func stateOf(ctx context.Context) requestState {
	st, _ := ctx.Value(stateKey{}).(requestState)
	return st
}

func withState(r *http.Request, svc *core.Service) context.Context {
	ap := svc.Autopilot()
	st := requestState{Autopilot: ap, Guide: ap.GuideState()}
	if ap.Enabled {
		st.Arrival = arrivalOf(r)
	}
	return context.WithValue(r.Context(), stateKey{}, st)
}

// arrivalOf names the network a request arrived on.
func arrivalOf(r *http.Request) core.Arrival {
	ctx := r.Context()
	if origin := api.ExternalOrigin(ctx); origin != "" {
		return core.Arrival{Network: "external", Origin: origin}
	}
	if prov, _, ok := core.ConsoleDomainOf(ctx); ok {
		switch prov {
		case "tailscale":
			login, node := core.TailnetPeerOf(ctx)
			if login != "" {
				node = ""
			}
			return core.Arrival{Network: "tailscale", Login: login, Node: node}
		case "netbird":
			return core.Arrival{Network: "netbird", Peer: core.NetbirdPeerOf(ctx)}
		}
	}
	return hostOrigin(r)
}

// hostOrigin classifies a request to the console listener by the socket's
// source address only: local for this host itself (loopback or one of its
// own interface addresses), else listener with the caller's address.
// Forwarding headers are client-sent and never read.
func hostOrigin(r *http.Request) core.Arrival {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		host, _, serr := net.SplitHostPort(r.RemoteAddr)
		if serr != nil {
			host = r.RemoteAddr
		}
		return core.Arrival{Network: "listener", IP: host}
	}
	src := ap.Addr().Unmap()
	if src.IsLoopback() || isHostAddr(src) {
		return core.Arrival{Network: "local"}
	}
	return core.Arrival{Network: "listener", IP: src.String()}
}

// isHostAddr reports whether an address is one of this host's own. Tests
// replace it.
var isHostAddr = api.IsHostAddr

// autopilotToolReplacements turn tool descriptions into their on text. Each
// old text occurs exactly once in the off description (a test checks it).
var autopilotToolReplacements = []struct{ tool, old, new string }{
	{"deploy", "Request operator approval to activate", "Request approval to activate"},
	{"create_version", "request explicit operator approval to save it", "request approval to save it"},
	{"delete_version", "Request explicit operator approval to delete", "Request approval to delete"},
	{"rollback", "Request operator approval to activate", "Request approval to activate"},
	{"rollback", "Ask the user before restore_data. Nothing changes before operator approval.", "Nothing changes before approval."},
	{"set_visibility", "BOTH directions require explicit operator approval; ask the user first.", "BOTH directions require approval."},
	{"delete_flat", "Always waits for the operator's approval (returns approval_url).", "Always waits for approval (returns approval_url)."},
	{"guide", "topic.approvals, ", "topic.approvals, topic.autopilot, "},
}

// autopilotDecidable lists the request tools whose approvals agents may
// decide under Autopilot; their on descriptions say so.
var autopilotDecidable = []string{"deploy", "create_version", "delete_version", "rollback", "set_visibility", "delete_flat"}

const autopilotToolNote = " While Autopilot is on, an agent may decide it with approve_request when the operator allows the operation (guide topic.autopilot)."

// autopilotDescription returns the on description of a tool.
func autopilotDescription(name, off string) string {
	d := off
	for _, r := range autopilotToolReplacements {
		if r.tool == name {
			d = strings.Replace(d, r.old, r.new, 1)
		}
	}
	for _, n := range autopilotDecidable {
		if n == name {
			d += autopilotToolNote
		}
	}
	return d
}

// describe gives a tool its on description on the Autopilot server and
// leaves it as it is on the off server.
func (t *tools) describe(tool *mcp.Tool) *mcp.Tool {
	if t.autopilot {
		tool.Description = autopilotDescription(tool.Name, tool.Description)
	}
	return tool
}

// rollbackInAutopilot is RollbackIn with the on description of restore_data;
// a test keeps the two schemas otherwise equal.
type rollbackInAutopilot struct {
	Slug        string `json:"slug" jsonschema:"flat slug"`
	Version     int    `json:"version,omitempty" jsonschema:"version to go back to (default: the one live before the current one)"`
	RestoreData bool   `json:"restore_data,omitempty" jsonschema:"server and docs flats: replace the current database and FILES with the snapshot taken before the current version was deployed (legacy DB-only snapshots keep FILES), after backing the current data up; writes since that deploy stop being live. Default false keeps data as it is. Frozen for approval."`
}

func (t *tools) rollbackAutopilot(ctx context.Context, req *mcp.CallToolRequest, in rollbackInAutopilot) (*mcp.CallToolResult, DeployInfo, error) {
	return t.rollback(ctx, req, RollbackIn(in))
}

// DecideIn names an approval and the deciding agent.
type DecideIn struct {
	ID    string `json:"id" jsonschema:"approval id (apr-...)"`
	Agent string `json:"agent" jsonschema:"your name for yourself, for example the bot's name; 1-100 characters; recorded as given with the decision and not verified"`
}

func registerAutopilot(s *mcp.Server, t *tools) {
	yes, no := true, false
	mcp.AddTool(s, &mcp.Tool{Name: "approve_request", Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes, IdempotentHint: true},
		Description: "Autopilot: approve a pending approval when the operator allows every operation it needs (guide topic.autopilot). " +
			"It applies as an approval in the console does: the frozen state is checked and the result is what get_approval returns. " +
			"A failure reported as a conflict, such as stale_approval, is a tool error; get_approval then shows the approval failed. " +
			"agent is your name, recorded with the decision. Approving an approved request again returns it unchanged."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in DecideIn) (*mcp.CallToolResult, ApprovalOut, error) {
			return t.decide(ctx, in, true)
		})
	mcp.AddTool(s, &mcp.Tool{Name: "reject_request", Annotations: &mcp.ToolAnnotations{DestructiveHint: &no, IdempotentHint: true},
		Description: "Autopilot: reject a pending approval when the operator allows every operation it needs (guide topic.autopilot). " +
			"Nothing is applied. agent is your name, recorded with the decision. Rejecting a rejected request again returns it unchanged."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in DecideIn) (*mcp.CallToolResult, ApprovalOut, error) {
			return t.decide(ctx, in, false)
		})
}

func (t *tools) decide(ctx context.Context, in DecideIn, approve bool) (*mcp.CallToolResult, ApprovalOut, error) {
	st := stateOf(ctx)
	a, err := t.svc.DecideAsAgent(ctx, in.ID, approve, core.AgentDecision{Agent: in.Agent, Arrival: st.Arrival})
	// A failed approval returned with an error other than a conflict is a
	// decision that took effect: report it as a result. A conflict (stale,
	// public edit blocked, public stop unconfirmed) is a tool error, as the
	// console answers 409, even though the approval is recorded failed.
	if err != nil && !(a.ID != "" && a.Status == "failed" && !errors.Is(err, core.ErrConflict)) {
		hint := ""
		if errors.Is(err, store.ErrNotFound) {
			hint = "use the approval_id returned by the request"
		}
		return nil, ApprovalOut{}, toolErr(err, hint)
	}
	out := approvalOut(a)
	text := t.approvalText(ctx, a, out, st)
	if out.ResultData != nil && out.ResultData.FailureCode != "" {
		text += "\nFailure code: " + out.ResultData.FailureCode + " (guide refusal." + out.ResultData.FailureCode + ")."
	}
	return result(text, out), out, nil
}

// agentsMayDecide reports whether Autopilot, as in ap, allows agents to
// decide approval a.
func agentsMayDecide(a store.Approval, ap core.Autopilot) bool {
	if !ap.Enabled {
		return false
	}
	need, err := core.AutopilotNeeds(a.Action, a.Params)
	return err == nil && need&^ap.Allowed == 0
}

// checkResultAutopilot is the check_client result while Autopilot is on:
// today's result plus an additive autopilot member.
type checkResultAutopilot struct {
	clientstate.CheckResult
	Autopilot autopilotInfo `json:"autopilot"`
}

type autopilotInfo struct {
	Enabled bool     `json:"enabled"`
	Allowed []string `json:"allowed" jsonschema:"operations agents may decide now"`
	Read    string   `json:"read" jsonschema:"guide item that explains Autopilot"`
}

func autopilotInfoOf(ap core.Autopilot) autopilotInfo {
	info := autopilotInfo{Enabled: ap.Enabled, Allowed: []string{}, Read: "guide topic.autopilot"}
	for _, op := range ap.Allowed.List() {
		info.Allowed = append(info.Allowed, string(op))
	}
	return info
}

// autopilotGuideItems lists every valid guide item while Autopilot is on:
// topic.autopilot after topic.approvals and its refusal pages at the end.
func autopilotGuideItems() []string {
	var items []string
	for _, id := range guideItems() {
		items = append(items, id)
		if id == "topic.approvals" {
			items = append(items, "topic.autopilot")
		}
	}
	for _, c := range runtimeref.AutopilotRefusalCategories() {
		items = append(items, "refusal."+c)
	}
	return items
}
