package runtimeref

import (
	"fmt"
	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/slug"
)

// Instructions are sent at initialization. They keep only the first move and
// the invariants that must always be in context; everything else is a guide
// topic (docs/agent), read on demand.
func Instructions(uploadLimit int64) string {
	return fmt.Sprintf(`Flats hosts websites, small server apps, HTML pages and Markdown documents ("flats") on the operator's own machine. Each flat has a slug (%d-%d characters: lowercase letters, digits and single hyphens, starting with a letter) and a Private URL through Local loopback or an explicitly permitted Tailscale, NetBird or reverse proxy route.

First move: call check_client, then guide {"items":["topic.index"]}. Read topic.client-update for client refresh. The index routes to topics; request several per call. For an existing flat, call get_flat before changing it. Server code: read guide topic.server (the complete contract is also resource %s and get_runtime_reference). Documents: guide topic.docs (also %s and get_content_types). A call Flats refuses names its category; guide refusal.<category> explains the fix. Clients that drop these instructions can read guide topic.instructions.

Always:
- Saving a website or page (save_draft, save_draft_from_dir, save_page) only changes a Private Draft. Creating or deleting a version, activation, rollback, data restore, visibility changes in BOTH directions and delete_flat wait for explicit operator approval and change nothing before it. Exception: documents (docs flats) have no Draft, versions or publish: create_flat with type docs, save_document and update_document change the running document at once, without approval; it is Private unless the operator approved Public, where everyone sees changes immediately.
- Give the user approval_url exactly as returned and poll get_approval. Never approve your own request or imitate the console; MCP has no approval tool.
- Ask the user before set_visibility, delete_flat or rollback with restore_data.
- Public is NOT access control: anyone on the internet can open a ready Public route. Private follows loopback, the tailnet or NetBird access rules, or the operator's reverse proxy; it is not owner-only.
- Never infer state from a URL. publication, live_version, visibility and connection state are separate fields. Report a version live only after get_flat shows it current with a ready endpoint and you fetched the page.
- Upload complete build output, not sources: at most %d bytes total per upload (operator-configurable) and %d files.`, slug.MinLen, slug.MaxLen, URI, ContentTypesURI, uploadLimit, bundle.MaxFiles)
}
