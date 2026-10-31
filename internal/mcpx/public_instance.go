package mcpx

import (
	"context"
	"encoding/json"
	"strings"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/clientstate"
	"github.com/gosuda/flats/internal/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func publicNetworkLabel(id string) string {
	switch id {
	case "portal":
		return "Portal"
	case "tailscale-funnel":
		return "Tailscale Funnel"
	case "zrok":
		return "zrok"
	case "cloudflare-quick":
		return "Cloudflare quick tunnel"
	case "netbird-expose":
		return "NetBird Expose"
	case "proxy-public":
		return "Public reverse proxy"
	default:
		return id
	}
}

const publicCreationNote = " Public instance mode is on: new flats are created Public, view only, on the default public network without visibility approval. Existing flats keep their visibility. Each save of a Public website or page created by this mode publishes the next version without approval. Flats created before the mode keep approved create_version and deploy. The create_flat page starter is not published. Private links are disabled; read guide topic.public-instance."

var publicToolReplacements = []struct{ tool, old, new string }{
	{"create_flat", "empty private website flat", "empty Public website flat"},
	{"create_flat", "a Private document", "a Public document"},
	{"save_draft", "a Private Draft revision", "a Draft revision (private previews disabled)"},
	{"save_draft", "Saving never creates a version or deployment; call create_version separately to request approval.", "Saving publishes Public flats created by this mode; older flats keep version and deployment approvals."},
	{"save_draft_from_dir", "a Private Draft", "a Draft (private previews disabled)"},
	{"save_page", "as a Private Draft", "as a Draft (private previews disabled)"},
	{"save_page", "Saving never creates a version or deployment; call create_version separately to request approval.", "Saving publishes Public pages created by this mode; older pages keep version and deployment approvals."},
	{"save_document", "Private unless the operator already made it Public", "Public on creation; existing flats keep their visibility"},
	{"save_design", "Private unless the operator made it Public", "Public on creation; existing flats keep their visibility"},
}

func publicInstanceDescription(name, desc string) string {
	if name == "open_preview" {
		return "Disabled while public instance mode is on: previews are private links. Returns public_instance_on. Inspect saved content through MCP instead (guide topic.public-instance)."
	}
	if name == "set_visibility" {
		desc = strings.Replace(desc, "unpublished flats cannot be Public", "unpublished flats cannot be made Public through this tool", 1)
		return desc + " Public instance mode does not bypass later visibility approvals. Make Private remains possible, but then the flat has no working link while private links are disabled (guide topic.public-instance)."
	}
	for _, r := range publicToolReplacements {
		if name == r.tool {
			desc = strings.Replace(desc, r.old, r.new, 1)
		}
	}
	switch name {
	case "create_flat", "save_draft", "save_draft_from_dir", "save_page", "save_document", "save_design", "update_board", "import_board":
		desc += publicCreationNote
	}
	return desc
}

func linkLabel(fi FlatInfo) string {
	if fi.privateLinksOff {
		return "Public link"
	}
	return "Private URL"
}

// linkText selects the address for summaries without changing off-mode text.
// The flag comes from the same FlatView as the address, not a later settings read.
func linkText(fi FlatInfo) string {
	if !fi.privateLinksOff {
		return fi.PrivateURL
	}
	text := fi.PublicURL
	if fi.PublicURL == "" || (fi.ConnectionState != "" && fi.ConnectionState != "ready" && fi.ConnectionState != "key-expiring") {
		state := fi.ConnectionState
		if state == "" {
			state = "unavailable"
		}
		detail := ""
		for _, ep := range fi.Endpoints {
			if core.PublicProvider(ep.Provider) && ep.State == fi.ConnectionState {
				detail = ep.Detail
				break
			}
		}
		if detail != "" {
			state += ": " + detail
		}
		if text != "" {
			text += "; "
		}
		text += "the public link is not open yet (" + state + ")"
	}
	if fi.PublicNotice != "" {
		text += "\nNotice: " + fi.PublicNotice
	}
	return text
}

type publicInstanceInfo struct {
	Enabled bool   `json:"enabled"`
	Network string `json:"network"`
	Read    string `json:"read"`
}

type checkResultPublicInstance struct {
	clientstate.CheckResult
	PublicInstance publicInstanceInfo `json:"public_instance"`
}

type checkResultBoth struct {
	clientstate.CheckResult
	Autopilot      autopilotInfo      `json:"autopilot"`
	PublicInstance publicInstanceInfo `json:"public_instance"`
}

func publicInstanceInfoOf(st *runtimeref.PublicInstanceState) publicInstanceInfo {
	return publicInstanceInfo{Enabled: true, Network: st.Network, Read: "guide topic.public-instance"}
}

func registerPublicClient(s *mcp.Server, tool *mcp.Tool, svc *core.Service, autopilot bool) {
	if autopilot {
		mcp.AddTool(s, tool, func(ctx context.Context, _ *mcp.CallToolRequest, in clientstate.CheckInput) (*mcp.CallToolResult, checkResultBoth, error) {
			res, err := clientstate.Check(clientManifest(ctx, svc), in)
			if err != nil {
				return nil, checkResultBoth{}, err
			}
			st := stateOf(ctx)
			out := checkResultBoth{CheckResult: res, Autopilot: autopilotInfoOf(st.Autopilot), PublicInstance: publicInstanceInfoOf(st.Public)}
			content, err := json.Marshal(out)
			if err != nil {
				return nil, out, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(content)}}}, out, nil
		})
		return
	}
	mcp.AddTool(s, tool, func(ctx context.Context, _ *mcp.CallToolRequest, in clientstate.CheckInput) (*mcp.CallToolResult, checkResultPublicInstance, error) {
		res, err := clientstate.Check(clientManifest(ctx, svc), in)
		if err != nil {
			return nil, checkResultPublicInstance{}, err
		}
		out := checkResultPublicInstance{CheckResult: res, PublicInstance: publicInstanceInfoOf(stateOf(ctx).Public)}
		content, err := json.Marshal(out)
		if err != nil {
			return nil, out, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(content)}}}, out, nil
	})
}
