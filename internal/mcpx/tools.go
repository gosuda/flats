package mcpx

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

const maxFiles = bundle.MaxFiles

type tools struct{ svc *core.Service }

func register(s *mcp.Server, t *tools) {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}
	yes, no := true, false
	write := &mcp.ToolAnnotations{DestructiveHint: &no}
	mcp.AddTool(s, &mcp.Tool{Name: "list_flats", Annotations: ro,
		Description: "List every flat with its visibility, live version and URLs."}, t.listFlats)
	mcp.AddTool(s, &mcp.Tool{Name: "get_flat", Annotations: ro,
		Description: "Show one flat: visibility, live version, private/public URLs and open previews."}, t.getFlat)
	mcp.AddTool(s, &mcp.Tool{Name: "create_flat", Annotations: write,
		Description: "Create an empty private flat. Optional: save_version creates the flat on first save."}, t.createFlat)
	mcp.AddTool(s, &mcp.Tool{Name: "save_version", Annotations: write,
		Description: "Save complete build output as a Private Draft revision (files inline; utf8 for text, base64 for binary). Saving never publishes; deploy=true requests explicit operator approval. For one Markdown document use save_document."}, t.saveVersion)
	mcp.AddTool(s, &mcp.Tool{Name: "save_version_from_dir", Annotations: write,
		Description: "Save a Private Draft from a directory on the Flats host (absolute path). " +
			"Only works when the agent runs on the Flats host itself (loopback); otherwise use save_version or the flats CLI."}, t.saveVersionFromDir)
	mcp.AddTool(s, &mcp.Tool{Name: "list_versions", Annotations: ro,
		Description: "List published versions only, newest first, with the current version marked."}, t.listVersions)
	mcp.AddTool(s, &mcp.Tool{Name: "deploy", Annotations: &mcp.ToolAnnotations{DestructiveHint: &no, IdempotentHint: true},
		Description: "Request operator approval to activate an already published version; version 0 requests publish of current Draft. Nothing changes before approval."}, t.deploy)
	mcp.AddTool(s, &mcp.Tool{Name: "save_draft", Annotations: write,
		Description: "Save complete inline build content as a Private Draft; expected_revision detects conflicting edits. Saving allocates no published number. Optional deploy=true requests pending publication; only successful operator approval later creates vN."}, t.saveVersion)
	mcp.AddTool(s, &mcp.Tool{Name: "get_draft", Annotations: ro,
		Description: "Read the current Private Draft revision and hash."}, t.getDraft)
	mcp.AddTool(s, &mcp.Tool{Name: "publish", Annotations: write,
		Description: "Freeze current Draft revision/hash and request explicit operator approval to publish. No version is created before successful approval."}, t.publish)
	mcp.AddTool(s, &mcp.Tool{Name: "save_document", Annotations: write,
		Description: "Save one Markdown document as a docs Draft, creating the flat when absent. Never publishes. Read get_document first to preserve live edits; read flats://docs/content-types/v1 for docs bundles. Use save_version for multiple documents/assets."}, t.saveDocument)
	mcp.AddTool(s, &mcp.Tool{Name: "get_document", Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: &no},
		Description: "Read exact live Markdown including people's edits from a docs flat, or Current Draft when no docs version runs. doc is an exact Markdown path (default: the entry). Returns hash (whole document); blocks=true also lists live blocks with the hashes update_document guards on. May seed or activate live state; idempotent and non-destructive. conflict (a generation from conflict metadata) returns preserved text. Read before save_document or update_document."}, t.getDocument)
	mcp.AddTool(s, &mcp.Tool{Name: "update_document", Annotations: &mcp.ToolAnnotations{DestructiveHint: &no},
		Description: "Edit the LIVE Markdown of a running docs flat with small guarded operations (replace exact text, replace/delete a block by hash, insert before/after a block, at a section end or document start/end). " +
			"Changes go live immediately with no approval, like a person editing in the browser, and merge with people's concurrent edits. On a Public flat anyone on the internet sees the change at once. " +
			"Read get_document {blocks: true} first. A changed guard, missing or ambiguous find refuses the whole call and changes nothing: re-read and retry. Not a publish; the next publish merges into these edits (topic.docs)."}, t.updateDocument)
	// rollback can replace the flat's data (restore_data), so clients must
	// treat it as destructive and ask before running it.
	mcp.AddTool(s, &mcp.Tool{Name: "rollback", Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes},
		Description: "Request operator approval to activate an earlier published version (default: previous live). " +
			"Code only by default; restore_data=true also REPLACES the flat's current database and FILES with the snapshot taken before the current version was deployed " +
			"(legacy DB-only snapshots keep current FILES); current data is backed up first. Ask the user before restore_data. Nothing changes before operator approval."}, t.rollback)
	mcp.AddTool(s, &mcp.Tool{Name: "open_preview", Annotations: write,
		Description: "Preview a published version, or current Private Draft with version 0, without changing live."}, t.openPreview)
	mcp.AddTool(s, &mcp.Tool{Name: "set_visibility", Annotations: write,
		Description: "Request visibility private or public. BOTH directions require explicit operator approval; ask the user first. Same visibility is unchanged; unpublished flats cannot be Public. Public is not access control."}, t.setVisibility)
	mcp.AddTool(s, &mcp.Tool{Name: "delete_flat", Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes},
		Description: "Request permanent deletion of a flat. Always waits for the operator's approval (returns approval_url)."}, t.deleteFlat)
	mcp.AddTool(s, &mcp.Tool{Name: "get_logs", Annotations: ro,
		Description: "Read a flat's event log: saves, deploys, health checks, runtime output, approvals."}, t.getLogs)
	mcp.AddTool(s, &mcp.Tool{Name: "get_approval", Annotations: ro,
		Description: "Poll an approval request: pending, applying, approved, rejected or failed. result_data reports failure_code and data impact."}, t.getApproval)
	mcp.AddTool(s, &mcp.Tool{Name: "list_env", Annotations: ro,
		Description: "List ordinary app environment variables with values; secret values are never returned. Running workers keep captured values until the next approved activation, host restart or new preview (guide topic.env-secrets)."}, t.listEnv)
	mcp.AddTool(s, &mcp.Tool{Name: "set_env", Annotations: write,
		Description: "Set an ordinary server-only app environment variable. Values are readable by management clients: never store credentials here; secrets are operator-managed. Applies at the next approved activation, host restart or new preview (guide topic.env-secrets)."}, t.setEnv)
	mcp.AddTool(s, &mcp.Tool{Name: "delete_env", Annotations: write,
		Description: "Delete an ordinary app environment variable. Applies at the next approved activation, host restart or new preview (guide topic.env-secrets)."}, t.deleteEnv)
	mcp.AddTool(s, &mcp.Tool{Name: "get_network", Annotations: ro,
		Description: "Read the operator-managed server HTTP(S) origin allowlist. Agents cannot grant network permissions; browser fetch follows browser CORS/CSP."}, t.getNetwork)
	mcp.AddTool(s, &mcp.Tool{Name: "list_secrets", Annotations: ro,
		Description: "List secret names of a flat (values are never returned; only the operator sets them)."}, t.listSecrets)
}

// --- shared output types ---

// FlatInfo describes a flat.
type FlatInfo struct {
	Type            string                  `json:"type" jsonschema:"flat (website) or docs (document)"`
	Publication     string                  `json:"publication"`
	Draft           *DraftInfo              `json:"draft"`
	Providers       []string                `json:"providers"`
	ConnectionState string                  `json:"connection_state,omitempty"`
	Endpoints       []core.ExposureEndpoint `json:"endpoints"`
	Slug            string                  `json:"slug" jsonschema:"flat identifier, also its private host name"`
	Name            string                  `json:"name" jsonschema:"display name"`
	Visibility      string                  `json:"visibility" jsonschema:"private or public"`
	LiveVersion     int                     `json:"live_version" jsonschema:"version serving now; 0 when never deployed"`
	Versions        int                     `json:"versions" jsonschema:"number of successfully published versions"`
	PrivateURL      string                  `json:"private_url" jsonschema:"private Local loopback or permitted Tailscale URL; serves after an approved publish"`
	PrivateState    string                  `json:"private_state,omitempty" jsonschema:"ready when the private URL answers; starting while the node joins the tailnet or waits for its HTTPS certificate (a new flat or preview usually needs 1-2 minutes): check again with get_flat before fetching"`
	PrivateDetail   string                  `json:"private_detail,omitempty" jsonschema:"what the private host is waiting for, when not ready"`
	PublicURL       string                  `json:"public_url,omitempty" jsonschema:"current internet URL when public; fetch only when the matching current endpoint is ready and permitted, not while connection_state is starting"`
	PublicNotice    string                  `json:"public_notice,omitempty" jsonschema:"what the public visibility means; repeat it to the user"`
	PortalListing   string                  `json:"portal_listing" jsonschema:"the flat's Portal relay listing choice set by the operator: default (follow the host setting), hidden or listed"`
	PortalHidden    bool                    `json:"portal_hidden" jsonschema:"true when Flats asks the Portal relays to keep this flat out of their listings (the resolved operator choice, not the relays' observed state; relays apply a change at their next lease renewal, up to about 90 s). Not access control: anyone with the URL can still open a public flat"`
	DiskBytes       int64                   `json:"disk_bytes" jsonschema:"disk used by versions and data"`
	UpdatedAt       time.Time               `json:"updated_at" jsonschema:"last change"`
}

func flatInfo(v core.FlatView) FlatInfo {
	return FlatInfo{Type: v.Type, Publication: v.Publication, Draft: draftPointer(v.Draft), Providers: v.Providers, ConnectionState: v.ConnectionState, Endpoints: v.Endpoints, Slug: v.Slug, Name: v.Name, Visibility: string(v.Visibility), LiveVersion: v.LiveVersion,
		Versions: v.Versions, PrivateURL: v.PrivateURL, PrivateState: v.PrivateState, PrivateDetail: v.PrivateDetail,
		PublicURL: v.PublicURL, PublicNotice: v.PublicNotice, PortalListing: v.PortalListing, PortalHidden: v.PortalHidden,
		DiskBytes: v.DiskBytes, UpdatedAt: v.UpdatedAt}
}

// VersionInfo describes a saved version.
type VersionInfo struct {
	Type      string    `json:"type" jsonschema:"flat (website) or docs (document)"`
	Published bool      `json:"published"`
	Role      string    `json:"role,omitempty"`
	Revision  int       `json:"revision,omitempty"`
	Number    int       `json:"number" jsonschema:"version number"`
	Live      bool      `json:"live" jsonschema:"true when this version is serving"`
	Kind      string    `json:"kind" jsonschema:"static or server"`
	Entry     string    `json:"entry,omitempty" jsonschema:"entry file from the manifest"`
	Health    string    `json:"health,omitempty" jsonschema:"health check path"`
	Files     int       `json:"files" jsonschema:"file count"`
	Size      int64     `json:"size" jsonschema:"total bytes"`
	Hash      string    `json:"hash" jsonschema:"content hash"`
	GitSHA    string    `json:"git_sha,omitempty" jsonschema:"git commit the build came from"`
	GitDirty  bool      `json:"git_dirty,omitempty" jsonschema:"true when the working tree had uncommitted changes"`
	Message   string    `json:"message,omitempty" jsonschema:"note given at save time"`
	Pruned    bool      `json:"pruned,omitempty" jsonschema:"files were removed by the retention policy; cannot be deployed"`
	CreatedAt time.Time `json:"created_at" jsonschema:"save time"`
}

func versionInfo(v store.Version, live int) VersionInfo {
	var m bundle.Manifest
	_ = json.Unmarshal(v.Manifest, &m)
	return VersionInfo{Type: contenttype.FromManifest(v.Manifest), Published: v.Published, Role: v.Role, Revision: v.Revision, Number: v.Number, Live: v.Number > 0 && v.Number == live, Kind: v.Kind, Entry: m.Entry, Health: m.Health,
		Files: v.Files, Size: v.Size, Hash: v.Hash, GitSHA: v.GitSHA, GitDirty: v.GitDirty, Message: v.Message,
		Pruned: v.Pruned, CreatedAt: v.CreatedAt}
}

// DeployInfo is the outcome of a successful deploy or rollback.
type DeployInfo struct {
	Status        string            `json:"status"`
	Approval      *ApprovalOut      `json:"approval,omitempty"`
	ApprovalID    string            `json:"approval_id,omitempty"`
	ApprovalURL   string            `json:"approval_url,omitempty"`
	Message       string            `json:"message,omitempty"`
	Version       int               `json:"version" jsonschema:"version now live"`
	Previous      int               `json:"previous" jsonschema:"version live before (0 = none)"`
	Health        core.HealthResult `json:"health" jsonschema:"pre-deploy health check result"`
	PrivateURL    string            `json:"private_url" jsonschema:"private Local loopback or permitted Tailscale URL; serves after an approved publish"`
	PrivateState  string            `json:"private_state,omitempty" jsonschema:"ready when the private URL answers; starting while the node joins the tailnet or waits for its HTTPS certificate (a new flat or preview usually needs 1-2 minutes): check again with get_flat before fetching"`
	PrivateDetail string            `json:"private_detail,omitempty" jsonschema:"what the private host is waiting for, when not ready"`
	PublicURL     string            `json:"public_url,omitempty" jsonschema:"current internet URL when public; fetch only when the matching current endpoint is ready and permitted, not while connection_state is starting"`
	PublicNotice  string            `json:"public_notice,omitempty" jsonschema:"what the public visibility means; repeat it to the user"`
	Millis        int64             `json:"millis" jsonschema:"deploy duration"`
}

func deployInfo(r core.DeployResult) DeployInfo {
	return DeployInfo{Version: r.Version, Previous: r.Previous, Health: r.Health, PrivateURL: r.Flat.PrivateURL,
		PrivateState: r.Flat.PrivateState, PrivateDetail: r.Flat.PrivateDetail, PublicURL: r.Flat.PublicURL, PublicNotice: r.Flat.PublicNotice, Millis: r.Millis}
}

func deployText(slug string, d DeployInfo, kind string) string {
	if d.Status == "pending_approval" {
		return actionText(ActionOut{Status: d.Status, Message: d.Message, ApprovalID: d.ApprovalID, ApprovalURL: d.ApprovalURL, Approval: d.Approval})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: version %d of %s is live at %s (health GET %s -> %d in %dms", kind, d.Version, slug, d.PrivateURL, d.Health.Path, d.Health.Status, d.Health.Millis)
	if d.Previous > 0 {
		fmt.Fprintf(&b, "; was version %d", d.Previous)
	}
	b.WriteString(").")
	writePending(&b, d.PrivateState, d.PrivateDetail)
	writePublic(&b, d.PublicURL, d.PublicNotice)
	return b.String()
}

// writePending notes that a private URL does not answer yet.
func writePending(b *strings.Builder, state, detail string) {
	if state == "" || state == "ready" || state == "key-expiring" {
		return
	}
	fmt.Fprintf(b, "\nThe private URL does not answer yet (%s", state)
	if detail != "" {
		fmt.Fprintf(b, ": %s", detail)
	}
	b.WriteString("). Check again with get_flat before fetching it.")
}

func writePublic(b *strings.Builder, url, notice string) {
	if url != "" {
		fmt.Fprintf(b, "\nPublic URL: %s", url)
	}
	if notice != "" {
		fmt.Fprintf(b, "\nNotice: %s", notice)
	}
}

// result builds a tool result whose text is a human summary followed by the
// structured output as JSON (for clients that ignore structuredContent).
func result(text string, out any) *mcp.CallToolResult {
	content := []mcp.Content{&mcp.TextContent{Text: text}}
	if raw, err := json.Marshal(out); err == nil {
		content = append(content, &mcp.TextContent{Text: string(raw)})
	}
	return &mcp.CallToolResult{Content: content}
}

// toolErr turns err into a tool error: the message, every validation problem
// with its fix, the failed health check, and a hint on what to do next.
func toolErr(err error, hint string) error {
	var b strings.Builder
	b.WriteString(err.Error())
	detail := map[string]any{"error": err.Error()}
	var de *core.DeployError
	category := errorCategory(err)
	detail["category"] = category

	if v, ok := bundle.IsValidation(err); ok {
		detail["problems"] = v.Problems
	}
	if errors.As(err, &de) && de.Cause == nil {
		detail["health"] = de.Health
		if de.Health.BodyHead != "" {
			fmt.Fprintf(&b, "\nResponse body starts with: %q", de.Health.BodyHead)
		}
	}
	// Every refusal names its category and the guide page that explains it.
	see := "See guide refusal." + category + "."
	if hint == "" {
		hint = see
	} else {
		hint = strings.TrimRight(hint, ". ") + ". " + see
	}
	b.WriteString("\nHint: " + hint)
	detail["hint"] = hint
	if raw, jerr := json.Marshal(detail); jerr == nil {
		b.WriteString("\n" + string(raw))
	}
	return errors.New(b.String())
}

func notFoundHint(err error, slug string) string {
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Sprintf("there is no flat %q (or the requested item does not exist); call list_flats to see slugs, list_versions for version numbers", slug)
	}
	return ""
}

// --- list_flats / get_flat / create_flat ---

// Empty is the input of tools without arguments.
type Empty struct{}

// FlatsOut lists flats.
type FlatsOut struct {
	Flats []FlatInfo `json:"flats" jsonschema:"every flat"`
}

func (t *tools) listFlats(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, FlatsOut, error) {
	fs, err := t.svc.ListFlats(ctx)
	if err != nil {
		return nil, FlatsOut{}, toolErr(err, "")
	}
	out := FlatsOut{Flats: make([]FlatInfo, 0, len(fs))}
	var b strings.Builder
	if len(fs) == 0 {
		b.WriteString("No flats yet. Call save_version with a slug to create one.")
	} else {
		fmt.Fprintf(&b, "%d flat(s):", len(fs))
	}
	for _, f := range fs {
		fi := flatInfo(f)
		out.Flats = append(out.Flats, fi)
		fmt.Fprintf(&b, "\n- %s (%s): %s, %s, %s", fi.Slug, fi.Name, fi.Visibility, liveText(fi.LiveVersion), fi.PrivateURL)
		if fi.PublicURL != "" {
			fmt.Fprintf(&b, ", public %s (%s)", fi.PublicURL, fi.PublicNotice)
		}
	}
	return result(b.String(), out), out, nil
}

func liveText(n int) string {
	if n == 0 {
		return "not deployed"
	}
	return fmt.Sprintf("live v%d", n)
}

// SlugIn names a flat.
type SlugIn struct {
	Slug string `json:"slug" jsonschema:"flat slug"`
}

// PreviewInfo describes an open preview.
type PreviewInfo struct {
	Target    string    `json:"target"`
	Revision  int       `json:"revision,omitempty"`
	Host      string    `json:"host" jsonschema:"preview host name"`
	URL       string    `json:"url" jsonschema:"temporary private URL"`
	Version   int       `json:"version" jsonschema:"version served"`
	ExpiresAt time.Time `json:"expires_at" jsonschema:"closes at this time unless visited again (or on the next deploy)"`
	State     string    `json:"state,omitempty" jsonschema:"ready when the private URL answers; starting while the node joins the tailnet or waits for its HTTPS certificate (a new flat or preview usually needs 1-2 minutes): check again with get_flat before fetching"`
	Detail    string    `json:"detail,omitempty" jsonschema:"what the preview host is waiting for, when not ready"`
}

func previewInfo(p core.PreviewView) PreviewInfo {
	return PreviewInfo{Target: p.Target, Revision: p.Revision, Host: p.Host, URL: p.URL, Version: p.Version, ExpiresAt: p.ExpiresAt, State: p.State, Detail: p.Detail}
}

// FlatOut is one flat with its previews.
type FlatOut struct {
	Flat     FlatInfo      `json:"flat" jsonschema:"the flat"`
	Previews []PreviewInfo `json:"previews,omitempty" jsonschema:"open previews"`
}

func flatText(fi FlatInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s): %s, %s, %d version(s). Private URL: %s", fi.Slug, fi.Name, fi.Visibility, liveText(fi.LiveVersion), fi.Versions, fi.PrivateURL)
	if fi.LiveVersion == 0 {
		b.WriteString(" (serves after the first approved publish)")
	}
	writePending(&b, fi.PrivateState, fi.PrivateDetail)
	writePublic(&b, fi.PublicURL, fi.PublicNotice)
	if fi.Visibility == "public" {
		fmt.Fprintf(&b, "\nCurrent public connection: %s. A URL alone does not establish readiness; inspect current endpoints before fetching.", fi.ConnectionState)
	}
	return b.String()
}

func (t *tools) getFlat(ctx context.Context, _ *mcp.CallToolRequest, in SlugIn) (*mcp.CallToolResult, FlatOut, error) {
	f, err := t.svc.GetFlat(ctx, in.Slug)
	if err != nil {
		return nil, FlatOut{}, toolErr(err, notFoundHint(err, in.Slug))
	}
	out := FlatOut{Flat: flatInfo(f)}
	text := flatText(out.Flat)
	if ps, err := t.svc.ListPreviews(ctx, in.Slug); err == nil {
		for _, p := range ps {
			out.Previews = append(out.Previews, previewInfo(p))
			if p.Target == "draft" {
				text += fmt.Sprintf("\nPrivate Draft preview of %s revision %d: %s. Current version is unchanged; access follows loopback or existing tailnet ACL. Expires %s.", in.Slug, p.Revision, p.URL, p.ExpiresAt.Format(time.RFC3339))
			} else {
				text += fmt.Sprintf("\nPreview of v%d: %s", p.Version, p.URL)
			}
			if p.State != "" && p.State != "ready" {
				text += " (" + p.State + ")"
			}
		}
	}
	return result(text, out), out, nil
}

// CreateIn creates a flat.
type CreateIn struct {
	Slug string `json:"slug" jsonschema:"3-54 lowercase letters, digits and single hyphens, starting with a letter; becomes the private host name"`
	Name string `json:"name,omitempty" jsonschema:"display name (default: the slug)"`
}

func (t *tools) createFlat(ctx context.Context, _ *mcp.CallToolRequest, in CreateIn) (*mcp.CallToolResult, FlatInfo, error) {
	f, err := t.svc.CreateFlat(ctx, in.Slug, in.Name, core.ViaMCP)
	if err != nil {
		return nil, FlatInfo{}, toolErr(err, "pick another slug (3-54 lowercase letters, digits and single hyphens, starting with a letter) or use the existing flat")
	}
	fi := flatInfo(f)
	return result("Created unpublished Private flat "+fi.Slug+". Next: save_draft, then publish and wait for operator approval.", fi), fi, nil
}

// --- save_version / save_version_from_dir ---

// FileIn is one inline file.
type FileIn struct {
	Path     string `json:"path" jsonschema:"path relative to the site root, e.g. index.html or assets/app.js"`
	Content  string `json:"content" jsonschema:"file content"`
	Encoding string `json:"encoding,omitempty" jsonschema:"utf8 (default) for text, base64 for binary files"`
}

// SaveIn saves inline files.
type SaveIn struct {
	ExpectedRevision *int     `json:"expected_revision,omitempty" jsonschema:"current Draft revision expected; 0 means no Draft exists; conflicts never overwrite"`
	Slug             string   `json:"slug" jsonschema:"flat slug (created when missing)"`
	Files            []FileIn `json:"files" jsonschema:"every file of the build output"`
	GitSHA           string   `json:"git_sha,omitempty" jsonschema:"git commit of the source"`
	GitDirty         bool     `json:"git_dirty,omitempty" jsonschema:"true when the working tree had uncommitted changes"`
	Message          string   `json:"message,omitempty" jsonschema:"short note about this version"`
	Deploy           bool     `json:"deploy,omitempty" jsonschema:"request publish approval after saving; never activates directly"`
}

// SaveDirIn saves a directory on the Flats host.
type SaveDirIn struct {
	ExpectedRevision *int   `json:"expected_revision,omitempty" jsonschema:"current Draft revision expected; conflicts never overwrite"`
	Slug             string `json:"slug" jsonschema:"flat slug (created when missing)"`
	Dir              string `json:"dir" jsonschema:"absolute path of the build output directory on the Flats host"`
	GitSHA           string `json:"git_sha,omitempty" jsonschema:"git commit of the source"`
	GitDirty         bool   `json:"git_dirty,omitempty" jsonschema:"true when the working tree had uncommitted changes"`
	Message          string `json:"message,omitempty" jsonschema:"short note about this version"`
	Deploy           bool   `json:"deploy,omitempty" jsonschema:"request publish approval after saving; never activates directly"`
}

// SaveOut is a saved (and possibly deployed) version.
type SaveOut struct {
	Draft   DraftInfo   `json:"draft"`
	Version VersionInfo `json:"version" jsonschema:"Draft compatibility object: number 0, role draft, revision; not a published version"`
	Deploy  *DeployInfo `json:"deploy,omitempty" jsonschema:"deploy outcome when deploy=true"`
	// Warnings never block the save (guide topic.design).
	Warnings []bundle.Problem `json:"warnings,omitempty" jsonschema:"non-blocking page-quality warnings with a fix each; the Draft was saved anyway"`
}

func decodeFiles(in []FileIn) ([]bundle.File, error) {
	if len(in) == 0 {
		return nil, &bundle.ValidationError{Problems: []bundle.Problem{{Message: "files is empty", Fix: "send every file of the build output, at least index.html"}}}
	}
	verr := &bundle.ValidationError{}
	files := make([]bundle.File, 0, len(in))
	for _, f := range in {
		switch strings.ToLower(strings.TrimSpace(f.Encoding)) {
		case "", "utf8", "utf-8", "text":
			files = append(files, bundle.File{Path: f.Path, Data: []byte(f.Content)})
		case "base64":
			data, err := decodeBase64(f.Content)
			if err != nil {
				verr.Problems = append(verr.Problems, bundle.Problem{Path: f.Path, Message: "invalid base64: " + err.Error(), Fix: "send standard base64 (RFC 4648, with or without padding)"})
				continue
			}
			files = append(files, bundle.File{Path: f.Path, Data: data})
		default:
			verr.Problems = append(verr.Problems, bundle.Problem{Path: f.Path, Message: fmt.Sprintf("unknown encoding %q", f.Encoding), Fix: `use "utf8" for text or "base64" for binary files`})
		}
	}
	if len(verr.Problems) > 0 {
		return nil, verr
	}
	return files, nil
}

func decodeBase64(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	if strings.HasSuffix(s, "=") {
		return base64.StdEncoding.DecodeString(s)
	}
	return base64.RawStdEncoding.DecodeString(s)
}

func (t *tools) saveVersion(ctx context.Context, _ *mcp.CallToolRequest, in SaveIn) (*mcp.CallToolResult, SaveOut, error) {
	raw, err := decodeFiles(in.Files)
	if err != nil {
		return nil, SaveOut{}, toolErr(err, "")
	}
	files, err := bundle.FromFiles(raw, bundle.Limits{MaxBytes: t.svc.UploadLimit()})
	if err != nil {
		return nil, SaveOut{}, toolErr(err, "fix every listed problem and call save_version again with the complete file list")
	}
	return t.save(ctx, in.Slug, files, core.SaveMeta{GitSHA: in.GitSHA, GitDirty: in.GitDirty, Message: in.Message}, in.Deploy, in.ExpectedRevision)
}

func (t *tools) saveVersionFromDir(ctx context.Context, _ *mcp.CallToolRequest, in SaveDirIn) (*mcp.CallToolResult, SaveOut, error) {
	if !isLoopback(ctx) {
		return nil, SaveOut{}, toolErr(refuse(core.ErrForbidden, "save_version_from_dir only accepts callers on the Flats host itself (loopback), and this request came over the network"),
			"send the files inline with save_version, or run `flats deploy <dir>` with the Flats CLI")
	}
	if !filepath.IsAbs(in.Dir) {
		return nil, SaveOut{}, toolErr(refuse(core.ErrInvalid, "dir %q is not an absolute path", in.Dir), "pass the absolute path of the build output directory, e.g. /path/to/project/dist")
	}
	// Resolve a symlinked root (e.g. dist -> build); FromDir still refuses
	// links inside the tree.
	dir, err := filepath.EvalSymlinks(in.Dir)
	if err != nil {
		return nil, SaveOut{}, toolErr(refuse(core.ErrInvalid, "dir %q is not a readable directory: %w", in.Dir, err), "build the site first and pass its output directory")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, SaveOut{}, toolErr(refuse(core.ErrInvalid, "dir %q is not a readable directory", in.Dir), "build the site first and pass its output directory")
	}
	files, err := bundle.FromDir(dir, bundle.Limits{MaxBytes: t.svc.UploadLimit()})
	if err != nil {
		return nil, SaveOut{}, toolErr(err, "fix every listed problem in the directory and call save_version_from_dir again")
	}
	return t.save(ctx, in.Slug, files, core.SaveMeta{GitSHA: in.GitSHA, GitDirty: in.GitDirty, Message: in.Message}, in.Deploy, in.ExpectedRevision)
}

func (t *tools) save(ctx context.Context, slug string, files []bundle.File, meta core.SaveMeta, deploy bool, expected *int) (*mcp.CallToolResult, SaveOut, error) {
	if expected != nil {
		if *expected < 0 {
			return nil, SaveOut{}, toolErr(refuse(core.ErrInvalid, "expected_revision must be nonnegative"), "read get_draft and retry with its revision")
		}
		meta.ExpectedRevision, meta.CheckRevision = *expected, true
	}
	v, err := t.svc.SaveVersion(ctx, slug, files, meta, core.ViaMCP)
	if err != nil {
		hint := "no Draft was saved; fix the problem above and save again"
		if _, ok := bundle.IsValidation(err); ok {
			hint = "no Draft was saved; fix every listed problem and save again"
		}
		return nil, SaveOut{}, toolErr(err, hint)
	}
	f, _ := t.svc.GetFlat(ctx, slug)
	draft, err := t.svc.GetDraft(ctx, slug)
	if err != nil {
		return nil, SaveOut{}, toolErr(err, "read get_draft to inspect the saved content")
	}
	out := SaveOut{Version: versionInfo(v, f.LiveVersion), Draft: draftInfo(draft), Warnings: t.lint(files)}
	text := fmt.Sprintf("Saved Private Draft revision %d of %s (%d files, %d bytes). Current version is unchanged (%s).", v.Revision, slug, v.Files, v.Size, liveText(f.LiveVersion))
	warnings := warningsText(out.Warnings)
	if !deploy {
		return result(text+" Next: open_preview with version 0, or publish to request operator approval."+warnings, out), out, nil
	}
	res, err := t.svc.RequestPublish(ctx, slug, v.Revision, v.Hash, core.ViaMCP)
	if err != nil {
		return nil, SaveOut{}, toolErr(err, "Draft was saved; read get_draft and request publish approval again")
	}
	d := pendingDeploy(res)
	out.Deploy = &d
	return result(text+"\n"+deployText(slug, d, "Publish requested")+warnings, out), out, nil
}

// lint checks the files as SaveVersion stored them: it normalizes its input
// with bundle.FromFiles again, which can strip a second wrapping directory.
func (t *tools) lint(files []bundle.File) []bundle.Problem {
	saved, err := bundle.FromFiles(files, bundle.Limits{MaxBytes: t.svc.UploadLimit()})
	if err != nil {
		return nil
	}
	return bundle.Lint(saved)
}

// warningsText lists save warnings after the save summary.
func warningsText(ws []bundle.Problem) string {
	if len(ws) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nWarnings (%d, non-blocking; the Draft was saved). Fix them and save again; see guide topic.design:", len(ws))
	for _, w := range ws {
		b.WriteString("\n- ")
		if w.Path != "" {
			b.WriteString(w.Path + ": ")
		}
		b.WriteString(w.Message + " (fix: " + w.Fix + ")")
	}
	return b.String()
}

// --- versions, deploy, rollback, preview ---

// VersionsOut lists versions.
type VersionsOut struct {
	LiveVersion int           `json:"live_version" jsonschema:"version serving now; 0 when never deployed"`
	Versions    []VersionInfo `json:"versions" jsonschema:"newest first"`
}

func (t *tools) listVersions(ctx context.Context, _ *mcp.CallToolRequest, in SlugIn) (*mcp.CallToolResult, VersionsOut, error) {
	f, err := t.svc.GetFlat(ctx, in.Slug)
	if err != nil {
		return nil, VersionsOut{}, toolErr(err, notFoundHint(err, in.Slug))
	}
	vs, err := t.svc.ListVersions(ctx, in.Slug)
	if err != nil {
		return nil, VersionsOut{}, toolErr(err, notFoundHint(err, in.Slug))
	}
	out := VersionsOut{LiveVersion: f.LiveVersion, Versions: make([]VersionInfo, 0, len(vs))}
	var b strings.Builder
	fmt.Fprintf(&b, "%s has %d version(s), %s.", in.Slug, len(vs), liveText(f.LiveVersion))
	for _, v := range vs {
		vi := versionInfo(v, f.LiveVersion)
		out.Versions = append(out.Versions, vi)
		fmt.Fprintf(&b, "\n- v%d %s %d files %d bytes %s", vi.Number, vi.Kind, vi.Files, vi.Size, vi.CreatedAt.Format(time.RFC3339))
		if vi.Live {
			b.WriteString(" [live]")
		}
		if vi.Pruned {
			b.WriteString(" [pruned]")
		}
		if vi.GitSHA != "" {
			b.WriteString(" git " + vi.GitSHA)
			if vi.GitDirty {
				b.WriteString(" (dirty)")
			}
		}
		if vi.Message != "" {
			b.WriteString(" - " + vi.Message)
		}
	}
	return result(b.String(), out), out, nil
}

// DeployIn deploys a version.
type DeployIn struct {
	Slug    string `json:"slug" jsonschema:"flat slug"`
	Version int    `json:"version" jsonschema:"published version to activate after approval; 0 requests publish of current Draft"`
}

// RollbackIn rolls back.
type RollbackIn struct {
	Slug        string `json:"slug" jsonschema:"flat slug"`
	Version     int    `json:"version,omitempty" jsonschema:"version to go back to (default: the one live before the current one)"`
	RestoreData bool   `json:"restore_data,omitempty" jsonschema:"server and docs flats: replace the current database and FILES with the snapshot taken before the current version was deployed (legacy DB-only snapshots keep FILES), after backing the current data up; writes since that deploy stop being live. Default false keeps data as it is. Frozen for explicit operator approval."`
}

func (t *tools) deployErr(ctx context.Context, slug string, err error) error {
	hint := notFoundHint(err, slug)
	var de *core.DeployError
	if errors.As(err, &de) {
		hint = "read the health result, fix the build and save a new version; open_preview helps to inspect a version without touching live"
		if de.Cause != nil {
			hint = "the version could not be started; fix the build (or its flats.json) and save a new version; get_logs has details"
		}
		if f, ferr := t.svc.GetFlat(ctx, slug); ferr == nil && f.LiveVersion == 0 {
			hint = "nothing was live before, so the flat is still not deployed; " + hint
		}
	} else if errors.Is(err, core.ErrNotDeployed) {
		hint = "nothing is live yet, so there is nothing to roll back; request publish of a Draft and wait for successful operator approval first"
	} else if errors.Is(err, core.ErrConflict) {
		hint = "call list_versions to pick a version"
	}
	return toolErr(err, hint)
}

func (t *tools) deploy(ctx context.Context, _ *mcp.CallToolRequest, in DeployIn) (*mcp.CallToolResult, DeployInfo, error) {
	if in.Version < 0 {
		return nil, DeployInfo{}, toolErr(refuse(core.ErrInvalid, "version must be nonnegative"), "0 requests current Draft publication; positive numbers activate published versions")
	}
	res, err := t.svc.Deploy(ctx, in.Slug, in.Version, core.ViaMCP)
	var pending *core.PendingApproval
	if errors.As(err, &pending) {
		d := pendingDeploy(pending.ActionResult)
		return result(deployText(in.Slug, d, "Requested"), d), d, nil
	}
	if err != nil {
		return nil, DeployInfo{}, t.deployErr(ctx, in.Slug, err)
	}
	d := deployInfo(res)
	return result(deployText(in.Slug, d, "Deployed"), d), d, nil
}

func (t *tools) rollback(ctx context.Context, _ *mcp.CallToolRequest, in RollbackIn) (*mcp.CallToolResult, DeployInfo, error) {
	if in.Version < 0 {
		return nil, DeployInfo{}, toolErr(refuse(core.ErrInvalid, "version must not be negative"), "omit version to roll back to the previous live version")
	}
	res, err := t.svc.RollbackWithData(ctx, in.Slug, in.Version, in.RestoreData, core.ViaMCP)
	var pending *core.PendingApproval
	if errors.As(err, &pending) {
		d := pendingDeploy(pending.ActionResult)
		return result(deployText(in.Slug, d, "Requested"), d), d, nil
	}
	if err != nil {
		return nil, DeployInfo{}, t.deployErr(ctx, in.Slug, err)
	}
	d := deployInfo(res)
	return result(deployText(in.Slug, d, "Rolled back"), d), d, nil
}

// PreviewIn opens a preview.
type PreviewIn struct {
	Target  string `json:"target,omitempty" jsonschema:"draft or version; draft requires version 0"`
	Slug    string `json:"slug" jsonschema:"flat slug"`
	Version int    `json:"version" jsonschema:"published version to preview; 0 previews current Private Draft (live is unchanged)"`
}

func (t *tools) openPreview(ctx context.Context, _ *mcp.CallToolRequest, in PreviewIn) (*mcp.CallToolResult, PreviewInfo, error) {
	if in.Version < 0 || (in.Target != "" && in.Target != "draft" && in.Target != "version") || (in.Target == "draft" && in.Version != 0) || (in.Target == "version" && in.Version == 0) {
		return nil, PreviewInfo{}, toolErr(refuse(core.ErrInvalid, "preview requires draft with version 0, or a positive published version"), "call get_draft or list_versions")
	}
	p, err := t.svc.OpenPreview(ctx, in.Slug, in.Version)
	if err != nil {
		return nil, PreviewInfo{}, toolErr(err, notFoundHint(err, in.Slug))
	}
	out := previewInfo(p)
	text := fmt.Sprintf("Preview of %s version %d: %s (Private via loopback or existing tailnet ACL). Live is unchanged. The preview closes on the next deploy of %s or when unused until %s.",
		in.Slug, p.Version, p.URL, in.Slug, p.ExpiresAt.Format(time.RFC3339))
	if p.State != "" && p.State != "ready" {
		text += " Its host is still " + p.State
		if p.Detail != "" {
			text += " (" + p.Detail + ")"
		}
		text += "; check get_flat for its state before fetching or sharing the URL."
	}
	return result(text, out), out, nil
}

// --- visibility, delete, approvals ---

// VisibilityIn changes visibility.
type VisibilityIn struct {
	Slug       string `json:"slug" jsonschema:"flat slug"`
	Visibility string `json:"visibility" jsonschema:"private or public"`
	Reason     string `json:"reason,omitempty" jsonschema:"why, shown to the operator in the approval request"`
}

// DeleteIn requests deletion.
type DeleteIn struct {
	Slug   string `json:"slug" jsonschema:"flat slug"`
	Reason string `json:"reason,omitempty" jsonschema:"why, shown to the operator in the approval request"`
}

// ActionOut is the outcome of an action that may need approval.
type ActionOut struct {
	Approval    *ApprovalOut `json:"approval,omitempty"`
	Status      string       `json:"status" jsonschema:"done, or pending_approval (nothing changed yet)"`
	Message     string       `json:"message" jsonschema:"what happened"`
	ApprovalID  string       `json:"approval_id,omitempty" jsonschema:"poll it with get_approval"`
	ApprovalURL string       `json:"approval_url,omitempty" jsonschema:"console link for the operator; give it to the user verbatim"`
	Visibility  string       `json:"visibility,omitempty" jsonschema:"visibility now in effect"`
	PublicURL   string       `json:"public_url,omitempty" jsonschema:"current internet URL when public; fetch only when the matching current endpoint is ready and permitted, not while connection_state is starting"`
	Notice      string       `json:"notice,omitempty" jsonschema:"access consequence to repeat to the user; pending notices describe what approval would do"`
}

func actionOut(r core.ActionResult) ActionOut {
	out := ActionOut{Status: r.Status, Message: r.Message, ApprovalURL: r.ApprovalURL, Notice: r.Notice}
	if r.Approval != nil {
		out.ApprovalID = r.Approval.ID
		a := approvalOut(*r.Approval)
		out.Approval = &a
	}
	if r.Flat != nil {
		out.Visibility = string(r.Flat.Visibility)
		out.PublicURL = r.Flat.PublicURL
		if out.Notice == "" {
			out.Notice = r.Flat.PublicNotice
		}
	}
	return out
}

func actionText(out ActionOut) string {
	var b strings.Builder
	b.WriteString(out.Message)
	if out.Status == "pending_approval" {
		fmt.Fprintf(&b, "\nStatus: pending_approval (id %s). Give the operator this link: %s\nPoll with get_approval.", out.ApprovalID, out.ApprovalURL)
	}
	writePublic(&b, out.PublicURL, out.Notice)
	return b.String()
}

func noticeFor(v store.Visibility) string {
	switch v {
	case store.Public:
		return core.PublicAccessNotice
	case store.PublicUnlisted:
		return core.UnlistedNotice
	case store.PublicListed:
		return core.ListedNotice
	}
	return ""
}

const pendingPublicNotice = "If approved, this flat becomes Public: anyone on the internet can open it. A domain or URL is not what makes it public."

func pendingNoticeFor(v store.Visibility) string {
	if v.Canonical() == store.Public {
		return pendingPublicNotice
	}
	return ""
}

func (t *tools) setVisibility(ctx context.Context, _ *mcp.CallToolRequest, in VisibilityIn) (*mcp.CallToolResult, ActionOut, error) {
	vis := store.Visibility(in.Visibility).Canonical()
	res, err := t.svc.SetVisibility(ctx, in.Slug, vis, core.ViaMCP, in.Reason)
	if err != nil {
		hint := notFoundHint(err, in.Slug)
		if !vis.Valid() {
			hint = "visibility must be private or public"
		}
		return nil, ActionOut{}, toolErr(err, hint)
	}
	out := actionOut(res)
	if out.Status == "pending_approval" {
		out.Notice = pendingNoticeFor(vis)
	} else if out.Notice == "" {
		out.Notice = noticeFor(vis)
	}
	return result(actionText(out), out), out, nil
}

func (t *tools) deleteFlat(ctx context.Context, _ *mcp.CallToolRequest, in DeleteIn) (*mcp.CallToolResult, ActionOut, error) {
	res, err := t.svc.Delete(ctx, in.Slug, core.ViaMCP, in.Reason)
	if err != nil {
		return nil, ActionOut{}, toolErr(err, notFoundHint(err, in.Slug))
	}
	out := actionOut(res)
	return result(actionText(out), out), out, nil
}

// ApprovalIn names an approval.
type ApprovalIn struct {
	ID string `json:"id" jsonschema:"approval id (apr-...)"`
}

// ApprovalOut is an approval request.
type ApprovalOut struct {
	DecidedBy    string                  `json:"decided_by,omitempty"`
	AuthorizedAt *time.Time              `json:"authorized_at,omitempty"`
	Via          string                  `json:"via"`
	ResultData   *core.ApprovalExecution `json:"result_data,omitempty"`
	ID           string                  `json:"id" jsonschema:"approval id"`
	Flat         string                  `json:"flat" jsonschema:"flat slug"`
	Action       string                  `json:"action" jsonschema:"publish, activate, rollback, set_visibility, restore_data or delete"`
	Params       map[string]any          `json:"params,omitempty" jsonschema:"requested change"`
	Status       string                  `json:"status" jsonschema:"pending, applying, approved, rejected or failed"`
	Reason       string                  `json:"reason,omitempty" jsonschema:"reason given with the request"`
	Result       string                  `json:"result,omitempty" jsonschema:"outcome once decided"`
	RequestedAt  time.Time               `json:"requested_at" jsonschema:"request time"`
	DecidedAt    *time.Time              `json:"decided_at,omitempty" jsonschema:"decision time"`
}

func (t *tools) getApproval(ctx context.Context, _ *mcp.CallToolRequest, in ApprovalIn) (*mcp.CallToolResult, ApprovalOut, error) {
	a, err := t.svc.GetApproval(ctx, in.ID)
	if err != nil {
		hint := ""
		if errors.Is(err, store.ErrNotFound) {
			hint = "use the approval_id returned by set_visibility or delete_flat"
		}
		return nil, ApprovalOut{}, toolErr(err, hint)
	}
	out := approvalOut(a)
	text := fmt.Sprintf("Approval %s (%s on %s): %s.", a.ID, a.Action, a.Flat, a.Status)
	if a.Status == "pending" {
		text += " Waiting for the operator to decide in the Flats console."
	} else if a.Status == "applying" {
		text += " The operator approved it; the operation is still applying."
	}
	if a.Result != "" {
		text += " Result: " + a.Result
	}
	if a.Action == "set_visibility" && a.Status == "approved" {
		if n := noticeFor(store.Visibility(fmt.Sprint(out.Params["visibility"]))); n != "" {
			text += "\nNotice: " + n
		}
	}
	return result(text, out), out, nil
}

// --- logs, secrets ---

const maxLogEvents = 1000

// LogsIn reads events.
type LogsIn struct {
	Slug  string `json:"slug" jsonschema:"flat slug"`
	Kind  string `json:"kind,omitempty" jsonschema:"only this kind: deploy, health, runtime, version, preview, visibility, approval, ..."`
	After int64  `json:"after,omitempty" jsonschema:"only events with id greater than this, oldest first (use next_after from the previous call); omitted: the newest events"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum events (default 200, max 1000)"`
}

// EventInfo is one log event.
type EventInfo struct {
	ID      int64     `json:"id" jsonschema:"event id"`
	Time    time.Time `json:"time" jsonschema:"event time"`
	Level   string    `json:"level" jsonschema:"info, warn or error"`
	Kind    string    `json:"kind" jsonschema:"event kind"`
	Message string    `json:"message" jsonschema:"event text"`
	Data    string    `json:"data,omitempty" jsonschema:"extra data as JSON"`
}

// LogsOut lists events oldest first.
type LogsOut struct {
	Events    []EventInfo `json:"events" jsonschema:"events, oldest first"`
	NextAfter int64       `json:"next_after" jsonschema:"pass as after to get newer events"`
}

func (t *tools) getLogs(ctx context.Context, _ *mcp.CallToolRequest, in LogsIn) (*mcp.CallToolResult, LogsOut, error) {
	if _, err := t.svc.GetFlat(ctx, in.Slug); err != nil {
		return nil, LogsOut{}, toolErr(err, notFoundHint(err, in.Slug))
	}
	evs, err := t.svc.Events(ctx, in.Slug, in.Kind, in.After, min(in.Limit, maxLogEvents))
	if err != nil {
		return nil, LogsOut{}, toolErr(err, "")
	}
	out := LogsOut{Events: make([]EventInfo, 0, len(evs)), NextAfter: in.After}
	var b strings.Builder
	fmt.Fprintf(&b, "%d event(s) for %s.", len(evs), in.Slug)
	for _, e := range evs {
		out.Events = append(out.Events, EventInfo{ID: e.ID, Time: e.Time, Level: e.Level, Kind: e.Kind, Message: e.Message, Data: string(e.Data)})
		out.NextAfter = max(out.NextAfter, e.ID)
		fmt.Fprintf(&b, "\n#%d %s %s [%s] %s", e.ID, e.Time.Format(time.RFC3339), e.Level, e.Kind, e.Message)
	}
	return result(b.String(), out), out, nil
}

// SecretsOut lists secret names.
type SecretsOut struct {
	Secrets []core.SecretInfo `json:"secrets" jsonschema:"secret names and update times"`
	Note    string            `json:"note" jsonschema:"how secrets work"`
}

func (t *tools) listSecrets(ctx context.Context, _ *mcp.CallToolRequest, in SlugIn) (*mcp.CallToolResult, SecretsOut, error) {
	secs, err := t.svc.SecretNames(ctx, in.Slug)
	if err != nil {
		return nil, SecretsOut{}, toolErr(err, notFoundHint(err, in.Slug))
	}
	out := SecretsOut{Secrets: secs, Note: "Values are never returned. The operator sets them in the console or with `flats secret set`; server flats receive them as env values. Changes apply to live on the next deploy/redeploy, rollback or data restoration (after any required approval), or Flats host restart. New previews capture current settings; automatic worker restarts reuse their captured settings."}
	if out.Secrets == nil {
		out.Secrets = []core.SecretInfo{}
	}
	names := make([]string, 0, len(secs))
	for _, s := range secs {
		names = append(names, s.Name)
	}
	text := fmt.Sprintf("%s has %d secret(s): %s\n%s", in.Slug, len(secs), strings.Join(names, ", "), out.Note)
	return result(text, out), out, nil
}

// DraftInfo is working content metadata, separate from published version IDs.
type DraftInfo struct {
	Type        string    `json:"type" jsonschema:"flat (website) or docs (document)"`
	Flat        string    `json:"flat"`
	Revision    int       `json:"revision"`
	Hash        string    `json:"hash"`
	BaseVersion int       `json:"base_version"`
	Dirty       bool      `json:"dirty"`
	Size        int64     `json:"size"`
	Files       int       `json:"files"`
	Kind        string    `json:"kind"`
	GitSHA      string    `json:"git_sha,omitempty"`
	GitDirty    bool      `json:"git_dirty"`
	Message     string    `json:"message,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
	CreatedAt   time.Time `json:"created_at"`
}

func draftInfo(d store.Draft) DraftInfo {
	return DraftInfo{Type: contenttype.FromManifest(d.Manifest), Flat: d.Flat, Revision: d.Revision, Hash: d.Hash, BaseVersion: d.BaseVersion, Dirty: d.Dirty, Size: d.Size, Files: d.Files, Kind: d.Kind, GitSHA: d.GitSHA, GitDirty: d.GitDirty, Message: d.Message, UpdatedAt: d.UpdatedAt, CreatedAt: d.CreatedAt}
}
func draftPointer(d *store.Draft) *DraftInfo {
	if d == nil {
		return nil
	}
	out := draftInfo(*d)
	return &out
}
func (t *tools) getDraft(ctx context.Context, _ *mcp.CallToolRequest, in SlugIn) (*mcp.CallToolResult, DraftInfo, error) {
	d, err := t.svc.GetDraft(ctx, in.Slug)
	if err != nil {
		return nil, DraftInfo{}, toolErr(err, "save complete build content with save_draft first")
	}
	out := draftInfo(d)
	return result(fmt.Sprintf("Private Draft revision %d; no publication is implied.", d.Revision), out), out, nil
}

type PublishIn struct {
	Slug     string `json:"slug"`
	Revision int    `json:"revision,omitempty" jsonschema:"current Draft revision; zero selects current"`
	Hash     string `json:"hash,omitempty" jsonschema:"optional expected content hash"`
}

func (t *tools) publish(ctx context.Context, _ *mcp.CallToolRequest, in PublishIn) (*mcp.CallToolResult, ActionOut, error) {
	if in.Revision < 0 {
		return nil, ActionOut{}, toolErr(refuse(core.ErrInvalid, "revision must be nonnegative"), "read get_draft")
	}
	r, err := t.svc.RequestPublish(ctx, in.Slug, in.Revision, in.Hash, core.ViaMCP)
	if err != nil {
		return nil, ActionOut{}, toolErr(err, "read current Draft and request approval again")
	}
	out := actionOut(r)
	return result(actionText(out), out), out, nil
}
func pendingDeploy(r core.ActionResult) DeployInfo {
	out := DeployInfo{Status: r.Status, ApprovalURL: r.ApprovalURL, Message: r.Message}
	if r.Approval != nil {
		a := approvalOut(*r.Approval)
		out.Approval = &a
		out.ApprovalID = a.ID
	}
	return out
}
func approvalOut(a store.Approval) ApprovalOut {
	out := ApprovalOut{DecidedBy: a.DecidedBy, AuthorizedAt: a.AuthorizedAt, Via: a.Via, ID: a.ID, Flat: a.Flat, Action: a.Action, Status: a.Status, Reason: a.Reason, Result: a.Result, RequestedAt: a.RequestedAt, DecidedAt: a.DecidedAt}
	_ = json.Unmarshal(a.Params, &out.Params)
	if len(a.ResultData) > 0 {
		var data core.ApprovalExecution
		if json.Unmarshal(a.ResultData, &data) == nil {
			out.ResultData = &data
		}
	}
	return out
}
