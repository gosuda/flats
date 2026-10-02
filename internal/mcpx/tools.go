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

	"github.com/oesni/flats/internal/bundle"
	"github.com/oesni/flats/internal/core"
	"github.com/oesni/flats/internal/store"
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
		Description: "Upload build output as a new immutable version (files inline; utf8 for text, base64 for binary). " +
			"Saving does not change what is live unless deploy=true."}, t.saveVersion)
	mcp.AddTool(s, &mcp.Tool{Name: "save_version_from_dir", Annotations: write,
		Description: "Save a version from a directory on the Flats host (absolute path). " +
			"Only works when the agent runs on the Flats host itself (loopback); otherwise use save_version or the flats CLI."}, t.saveVersionFromDir)
	mcp.AddTool(s, &mcp.Tool{Name: "list_versions", Annotations: ro,
		Description: "List saved versions, newest first, with the live one marked."}, t.listVersions)
	mcp.AddTool(s, &mcp.Tool{Name: "deploy", Annotations: &mcp.ToolAnnotations{DestructiveHint: &no, IdempotentHint: true},
		Description: "Make a saved version live after a health check. On failure the previous live version keeps serving."}, t.deploy)
	mcp.AddTool(s, &mcp.Tool{Name: "rollback", Annotations: write,
		Description: "Redeploy an earlier version (default: the version live before the current one)."}, t.rollback)
	mcp.AddTool(s, &mcp.Tool{Name: "open_preview", Annotations: write,
		Description: "Serve a saved version at a temporary private URL without changing live."}, t.openPreview)
	mcp.AddTool(s, &mcp.Tool{Name: "set_visibility", Annotations: write,
		Description: "Change who can open a flat: private, public-unlisted or public-listed. " +
			"Going public needs the operator's approval (returns approval_url); going private applies immediately. " +
			"public-unlisted is NOT access control."}, t.setVisibility)
	mcp.AddTool(s, &mcp.Tool{Name: "delete_flat", Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes},
		Description: "Request permanent deletion of a flat. Always waits for the operator's approval (returns approval_url)."}, t.deleteFlat)
	mcp.AddTool(s, &mcp.Tool{Name: "get_logs", Annotations: ro,
		Description: "Read a flat's event log: saves, deploys, health checks, runtime output, approvals."}, t.getLogs)
	mcp.AddTool(s, &mcp.Tool{Name: "get_approval", Annotations: ro,
		Description: "Poll an approval request (pending, approved, rejected or failed)."}, t.getApproval)
	mcp.AddTool(s, &mcp.Tool{Name: "list_secrets", Annotations: ro,
		Description: "List secret names of a flat (values are never returned; only the operator sets them)."}, t.listSecrets)
}

// --- shared output types ---

// FlatInfo describes a flat.
type FlatInfo struct {
	Slug         string    `json:"slug" jsonschema:"flat identifier, also its private host name"`
	Name         string    `json:"name" jsonschema:"display name"`
	Visibility   string    `json:"visibility" jsonschema:"private, public-unlisted or public-listed"`
	LiveVersion  int       `json:"live_version" jsonschema:"version serving now; 0 when never deployed"`
	Versions     int       `json:"versions" jsonschema:"number of saved versions"`
	PrivateURL   string    `json:"private_url" jsonschema:"tailnet-only URL (serves once a version is deployed)"`
	PublicURL    string    `json:"public_url,omitempty" jsonschema:"internet URL when public"`
	PublicNotice string    `json:"public_notice,omitempty" jsonschema:"what the public visibility means; repeat it to the user"`
	DiskBytes    int64     `json:"disk_bytes" jsonschema:"disk used by versions and data"`
	UpdatedAt    time.Time `json:"updated_at" jsonschema:"last change"`
}

func flatInfo(v core.FlatView) FlatInfo {
	return FlatInfo{Slug: v.Slug, Name: v.Name, Visibility: string(v.Visibility), LiveVersion: v.LiveVersion,
		Versions: v.Versions, PrivateURL: v.PrivateURL, PublicURL: v.PublicURL, PublicNotice: v.PublicNotice,
		DiskBytes: v.DiskBytes, UpdatedAt: v.UpdatedAt}
}

// VersionInfo describes a saved version.
type VersionInfo struct {
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
	return VersionInfo{Number: v.Number, Live: v.Number == live, Kind: v.Kind, Entry: m.Entry, Health: m.Health,
		Files: v.Files, Size: v.Size, Hash: v.Hash, GitSHA: v.GitSHA, GitDirty: v.GitDirty, Message: v.Message,
		Pruned: v.Pruned, CreatedAt: v.CreatedAt}
}

// DeployInfo is the outcome of a successful deploy or rollback.
type DeployInfo struct {
	Version      int               `json:"version" jsonschema:"version now live"`
	Previous     int               `json:"previous" jsonschema:"version live before (0 = none)"`
	Health       core.HealthResult `json:"health" jsonschema:"pre-deploy health check result"`
	PrivateURL   string            `json:"private_url" jsonschema:"tailnet-only URL"`
	PublicURL    string            `json:"public_url,omitempty" jsonschema:"internet URL when public"`
	PublicNotice string            `json:"public_notice,omitempty" jsonschema:"what the public visibility means; repeat it to the user"`
	Millis       int64             `json:"millis" jsonschema:"deploy duration"`
}

func deployInfo(r core.DeployResult) DeployInfo {
	return DeployInfo{Version: r.Version, Previous: r.Previous, Health: r.Health, PrivateURL: r.Flat.PrivateURL,
		PublicURL: r.Flat.PublicURL, PublicNotice: r.Flat.PublicNotice, Millis: r.Millis}
}

func deployText(slug string, d DeployInfo, kind string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: version %d of %s is live at %s (health GET %s -> %d in %dms", kind, d.Version, slug, d.PrivateURL, d.Health.Path, d.Health.Status, d.Health.Millis)
	if d.Previous > 0 {
		fmt.Fprintf(&b, "; was version %d", d.Previous)
	}
	b.WriteString(").")
	writePublic(&b, d.PublicURL, d.PublicNotice)
	return b.String()
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
	if v, ok := bundle.IsValidation(err); ok {
		detail["problems"] = v.Problems
	}
	var de *core.DeployError
	if errors.As(err, &de) && de.Cause == nil {
		detail["health"] = de.Health
		if de.Health.BodyHead != "" {
			fmt.Fprintf(&b, "\nResponse body starts with: %q", de.Health.BodyHead)
		}
	}
	if hint != "" {
		b.WriteString("\nHint: " + hint)
		detail["hint"] = hint
	}
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
	Host      string    `json:"host" jsonschema:"preview host name"`
	URL       string    `json:"url" jsonschema:"temporary private URL"`
	Version   int       `json:"version" jsonschema:"version served"`
	ExpiresAt time.Time `json:"expires_at" jsonschema:"closes at this time unless visited again (or on the next deploy)"`
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
		b.WriteString(" (serves after the first deploy)")
	}
	writePublic(&b, fi.PublicURL, fi.PublicNotice)
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
			out.Previews = append(out.Previews, PreviewInfo{Host: p.Host, URL: p.URL, Version: p.Version, ExpiresAt: p.ExpiresAt})
			text += fmt.Sprintf("\nPreview of v%d: %s", p.Version, p.URL)
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
	return result("Created private flat "+fi.Slug+". Next: save_version, then deploy.", fi), fi, nil
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
	Slug     string   `json:"slug" jsonschema:"flat slug (created when missing)"`
	Files    []FileIn `json:"files" jsonschema:"every file of the build output"`
	GitSHA   string   `json:"git_sha,omitempty" jsonschema:"git commit of the source"`
	GitDirty bool     `json:"git_dirty,omitempty" jsonschema:"true when the working tree had uncommitted changes"`
	Message  string   `json:"message,omitempty" jsonschema:"short note about this version"`
	Deploy   bool     `json:"deploy,omitempty" jsonschema:"deploy right after saving"`
}

// SaveDirIn saves a directory on the Flats host.
type SaveDirIn struct {
	Slug     string `json:"slug" jsonschema:"flat slug (created when missing)"`
	Dir      string `json:"dir" jsonschema:"absolute path of the build output directory on the Flats host"`
	GitSHA   string `json:"git_sha,omitempty" jsonschema:"git commit of the source"`
	GitDirty bool   `json:"git_dirty,omitempty" jsonschema:"true when the working tree had uncommitted changes"`
	Message  string `json:"message,omitempty" jsonschema:"short note about this version"`
	Deploy   bool   `json:"deploy,omitempty" jsonschema:"deploy right after saving"`
}

// SaveOut is a saved (and possibly deployed) version.
type SaveOut struct {
	Version VersionInfo `json:"version" jsonschema:"the saved version"`
	Deploy  *DeployInfo `json:"deploy,omitempty" jsonschema:"deploy outcome when deploy=true"`
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
	return t.save(ctx, in.Slug, files, core.SaveMeta{GitSHA: in.GitSHA, GitDirty: in.GitDirty, Message: in.Message}, in.Deploy)
}

func (t *tools) saveVersionFromDir(ctx context.Context, _ *mcp.CallToolRequest, in SaveDirIn) (*mcp.CallToolResult, SaveOut, error) {
	if !isLoopback(ctx) {
		return nil, SaveOut{}, toolErr(errors.New("save_version_from_dir only accepts callers on the Flats host itself (loopback), and this request came over the network"),
			"send the files inline with save_version, or run `flats deploy <dir>` with the Flats CLI")
	}
	if !filepath.IsAbs(in.Dir) {
		return nil, SaveOut{}, toolErr(fmt.Errorf("dir %q is not an absolute path", in.Dir), "pass the absolute path of the build output directory, e.g. /Users/me/project/dist")
	}
	// Resolve a symlinked root (e.g. dist -> build); FromDir still refuses
	// links inside the tree.
	dir, err := filepath.EvalSymlinks(in.Dir)
	if err != nil {
		return nil, SaveOut{}, toolErr(fmt.Errorf("dir %q is not a readable directory: %w", in.Dir, err), "build the site first and pass its output directory")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, SaveOut{}, toolErr(fmt.Errorf("dir %q is not a readable directory", in.Dir), "build the site first and pass its output directory")
	}
	files, err := bundle.FromDir(dir, bundle.Limits{MaxBytes: t.svc.UploadLimit()})
	if err != nil {
		return nil, SaveOut{}, toolErr(err, "fix every listed problem in the directory and call save_version_from_dir again")
	}
	return t.save(ctx, in.Slug, files, core.SaveMeta{GitSHA: in.GitSHA, GitDirty: in.GitDirty, Message: in.Message}, in.Deploy)
}

func (t *tools) save(ctx context.Context, slug string, files []bundle.File, meta core.SaveMeta, deploy bool) (*mcp.CallToolResult, SaveOut, error) {
	v, err := t.svc.SaveVersion(ctx, slug, files, meta, core.ViaMCP)
	if err != nil {
		hint := "no version was saved; fix the problem above and save again"
		if _, ok := bundle.IsValidation(err); ok {
			hint = "no version was saved; fix every listed problem and save again"
		}
		return nil, SaveOut{}, toolErr(err, hint)
	}
	f, _ := t.svc.GetFlat(ctx, slug)
	out := SaveOut{Version: versionInfo(v, f.LiveVersion)}
	if !deploy {
		text := fmt.Sprintf("Saved version %d of %s (%d files, %d bytes). Live is unchanged (%s). Next: deploy version %d, or open_preview to check it first.",
			v.Number, slug, v.Files, v.Size, liveText(f.LiveVersion), v.Number)
		return result(text, out), out, nil
	}
	res, err := t.svc.Deploy(ctx, slug, v.Number, core.ViaMCP)
	if err != nil {
		keeps := "Nothing was live before, so the flat is still not deployed."
		if f.LiveVersion > 0 {
			keeps = fmt.Sprintf("The previous live version %d keeps serving.", f.LiveVersion)
		}
		return nil, SaveOut{}, toolErr(fmt.Errorf("saved version %d of %s, but its deploy failed: %w", v.Number, slug, err),
			fmt.Sprintf("%s Fix the build and save again, use open_preview with version %d to inspect it, or get_logs for details.", keeps, v.Number))
	}
	d := deployInfo(res)
	out.Version.Live = true
	out.Deploy = &d
	text := fmt.Sprintf("Saved version %d (%d files, %d bytes).\n%s", v.Number, v.Files, v.Size, deployText(slug, d, "Deployed"))
	return result(text, out), out, nil
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
	Version int    `json:"version" jsonschema:"saved version number to make live"`
}

// RollbackIn rolls back.
type RollbackIn struct {
	Slug    string `json:"slug" jsonschema:"flat slug"`
	Version int    `json:"version,omitempty" jsonschema:"version to go back to (default: the one live before the current one)"`
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
		hint = "nothing is live yet, so there is nothing to roll back; deploy a saved version first"
	} else if errors.Is(err, core.ErrConflict) {
		hint = "call list_versions to pick a version"
	}
	return toolErr(err, hint)
}

func (t *tools) deploy(ctx context.Context, _ *mcp.CallToolRequest, in DeployIn) (*mcp.CallToolResult, DeployInfo, error) {
	if in.Version <= 0 {
		return nil, DeployInfo{}, toolErr(errors.New("version must be a positive version number"), "call list_versions to see saved versions")
	}
	res, err := t.svc.Deploy(ctx, in.Slug, in.Version, core.ViaMCP)
	if err != nil {
		return nil, DeployInfo{}, t.deployErr(ctx, in.Slug, err)
	}
	d := deployInfo(res)
	return result(deployText(in.Slug, d, "Deployed"), d), d, nil
}

func (t *tools) rollback(ctx context.Context, _ *mcp.CallToolRequest, in RollbackIn) (*mcp.CallToolResult, DeployInfo, error) {
	if in.Version < 0 {
		return nil, DeployInfo{}, toolErr(errors.New("version must not be negative"), "omit version to roll back to the previous live version")
	}
	res, err := t.svc.Rollback(ctx, in.Slug, in.Version, core.ViaMCP)
	if err != nil {
		return nil, DeployInfo{}, t.deployErr(ctx, in.Slug, err)
	}
	d := deployInfo(res)
	return result(deployText(in.Slug, d, "Rolled back"), d), d, nil
}

// PreviewIn opens a preview.
type PreviewIn struct {
	Slug    string `json:"slug" jsonschema:"flat slug"`
	Version int    `json:"version" jsonschema:"saved version number to preview (live is not changed)"`
}

func (t *tools) openPreview(ctx context.Context, _ *mcp.CallToolRequest, in PreviewIn) (*mcp.CallToolResult, PreviewInfo, error) {
	if in.Version <= 0 {
		return nil, PreviewInfo{}, toolErr(errors.New("version must be a positive version number"), "call list_versions to see saved versions")
	}
	p, err := t.svc.OpenPreview(ctx, in.Slug, in.Version)
	if err != nil {
		return nil, PreviewInfo{}, toolErr(err, notFoundHint(err, in.Slug))
	}
	out := PreviewInfo{Host: p.Host, URL: p.URL, Version: p.Version, ExpiresAt: p.ExpiresAt}
	text := fmt.Sprintf("Preview of %s version %d: %s (private, tailnet only). Live is unchanged. The preview closes on the next deploy of %s or when unused until %s.",
		in.Slug, p.Version, p.URL, in.Slug, p.ExpiresAt.Format(time.RFC3339))
	return result(text, out), out, nil
}

// --- visibility, delete, approvals ---

// VisibilityIn changes visibility.
type VisibilityIn struct {
	Slug       string `json:"slug" jsonschema:"flat slug"`
	Visibility string `json:"visibility" jsonschema:"private, public-unlisted (not access control: anyone with the URL can open it) or public-listed"`
	Reason     string `json:"reason,omitempty" jsonschema:"why, shown to the operator in the approval request"`
}

// DeleteIn requests deletion.
type DeleteIn struct {
	Slug   string `json:"slug" jsonschema:"flat slug"`
	Reason string `json:"reason,omitempty" jsonschema:"why, shown to the operator in the approval request"`
}

// ActionOut is the outcome of an action that may need approval.
type ActionOut struct {
	Status      string `json:"status" jsonschema:"done, or pending_approval (nothing changed yet)"`
	Message     string `json:"message" jsonschema:"what happened"`
	ApprovalID  string `json:"approval_id,omitempty" jsonschema:"poll it with get_approval"`
	ApprovalURL string `json:"approval_url,omitempty" jsonschema:"console link for the operator; give it to the user verbatim"`
	Visibility  string `json:"visibility,omitempty" jsonschema:"visibility now in effect"`
	PublicURL   string `json:"public_url,omitempty" jsonschema:"internet URL when public"`
	Notice      string `json:"notice,omitempty" jsonschema:"what the public visibility means; repeat it to the user"`
}

func actionOut(r core.ActionResult) ActionOut {
	out := ActionOut{Status: r.Status, Message: r.Message, ApprovalURL: r.ApprovalURL, Notice: r.Notice}
	if r.Approval != nil {
		out.ApprovalID = r.Approval.ID
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
	case store.PublicUnlisted:
		return core.UnlistedNotice
	case store.PublicListed:
		return core.ListedNotice
	}
	return ""
}

func (t *tools) setVisibility(ctx context.Context, _ *mcp.CallToolRequest, in VisibilityIn) (*mcp.CallToolResult, ActionOut, error) {
	vis := store.Visibility(in.Visibility)
	res, err := t.svc.SetVisibility(ctx, in.Slug, vis, core.ViaMCP, in.Reason)
	if err != nil {
		hint := notFoundHint(err, in.Slug)
		if !vis.Valid() {
			hint = "visibility must be private, public-unlisted or public-listed"
		}
		return nil, ActionOut{}, toolErr(err, hint)
	}
	out := actionOut(res)
	if out.Notice == "" {
		out.Notice = noticeFor(vis) // pending requests: what the requested visibility will mean
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
	ID          string            `json:"id" jsonschema:"approval id"`
	Flat        string            `json:"flat" jsonschema:"flat slug"`
	Action      string            `json:"action" jsonschema:"set_visibility or delete"`
	Params      map[string]string `json:"params,omitempty" jsonschema:"requested change"`
	Status      string            `json:"status" jsonschema:"pending, approved, rejected or failed"`
	Reason      string            `json:"reason,omitempty" jsonschema:"reason given with the request"`
	Result      string            `json:"result,omitempty" jsonschema:"outcome once decided"`
	RequestedAt time.Time         `json:"requested_at" jsonschema:"request time"`
	DecidedAt   *time.Time        `json:"decided_at,omitempty" jsonschema:"decision time"`
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
	out := ApprovalOut{ID: a.ID, Flat: a.Flat, Action: a.Action, Status: a.Status, Reason: a.Reason, Result: a.Result,
		RequestedAt: a.RequestedAt, DecidedAt: a.DecidedAt}
	_ = json.Unmarshal(a.Params, &out.Params)
	text := fmt.Sprintf("Approval %s (%s on %s): %s.", a.ID, a.Action, a.Flat, a.Status)
	if a.Status == "pending" {
		text += " Waiting for the operator to decide in the Flats console."
	}
	if a.Result != "" {
		text += " Result: " + a.Result
	}
	if a.Action == "set_visibility" && a.Status == "approved" {
		if n := noticeFor(store.Visibility(out.Params["visibility"])); n != "" {
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
	out := SecretsOut{Secrets: secs, Note: "Values are never returned. The operator sets them in the console or with `flats secret set`; changes apply on the next deploy, as env values of a server flat."}
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
