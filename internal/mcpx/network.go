package mcpx

import (
	"context"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type NetworkOut struct {
	Origins []string `json:"origins" jsonschema:"operator-permitted exact server HTTP(S) origins"`
	Note    string   `json:"note" jsonschema:"permission ownership and activation rules"`
}

func (t *tools) getNetwork(ctx context.Context, _ *mcp.CallToolRequest, in SlugIn) (*mcp.CallToolResult, NetworkOut, error) {
	policy, err := t.svc.NetworkPolicy(ctx, in.Slug)
	if err != nil {
		return nil, NetworkOut{}, toolErr(err, notFoundHint(err, in.Slug))
	}
	if policy.Origins == nil {
		policy.Origins = []string{}
	}
	out := NetworkOut{Origins: policy.Origins, Note: "Only the operator grants JavaScript server fetch origins in the console or with `flats network set` on the Flats host. Empty denies server fetch; at most 32 exact public HTTP:80/HTTPS:443 origins, no wildcards. Changes apply on the next deploy/redeploy, rollback or data restoration after any required approval, Flats host restart, or new preview. Running workers and automatic restarts retain captured grants; after clearing, redeploy to revoke live access. Browser fetch follows browser CORS/CSP independently."}
	return result(fmt.Sprintf("%s: %v\n%s", in.Slug, out.Origins, out.Note), out), out, nil
}
