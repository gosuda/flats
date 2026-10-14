package mcpx

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/design"
	"github.com/gosuda/flats/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func registerDesign(s *mcp.Server, t *tools) {
	no := false
	write := &mcp.ToolAnnotations{DestructiveHint: &no}
	mcp.AddTool(s, &mcp.Tool{Name: "save_design", Annotations: write,
		Description: "Write files of a design (a canvas of HTML artboards people and agents edit live; guide topic.canvas), creating it when absent. " +
			"One call is one revision and applies at once, Private unless the operator made it Public: no Draft, publish or approval. " +
			"Files you do not name stay as they are; list paths to remove in delete. canvas.json is the index (title, boards with x/y/w/h, order, notes)."}, t.saveDesign)
	mcp.AddTool(s, &mcp.Tool{Name: "get_design", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		Description: "Read a design: without path, its canvas, file list (path, size, hash, gen), revision and each artboard's tweaks; with path, one file's content. Read it before changing files people may have edited."}, t.getDesign)
	mcp.AddTool(s, &mcp.Tool{Name: "list_revisions", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		Description: "List a design's revisions, newest first (at most 100 per call; pass next as before for older ones), with kind, authors, message, pinned and changed paths."}, t.listRevisions)
	mcp.AddTool(s, &mcp.Tool{Name: "get_revision", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		Description: "Read one revision of a design: its metadata and file list, or with path one file as it was then."}, t.getRevision)
	mcp.AddTool(s, &mcp.Tool{Name: "pin_revision", Annotations: write,
		Description: "Pin or unpin a revision of a design. Pinned revisions are never deleted automatically; a full pin quota refuses new pins. No approval needed."}, t.pinRevision)
	mcp.AddTool(s, &mcp.Tool{Name: "rollback_revision", Annotations: write,
		Description: "Restore a design to an earlier revision. It creates a new revision equal to that one (history is kept, and it can be undone the same way) and applies at once. No approval needed."}, t.rollbackRevision)
}

// SaveDesignIn writes design files.
type SaveDesignIn struct {
	Slug             string   `json:"slug" jsonschema:"design slug (created when missing)"`
	Files            []FileIn `json:"files,omitempty" jsonschema:"files to write: canvas.json, .html artboards and assets; utf8 for text, base64 for binary"`
	Delete           []string `json:"delete,omitempty" jsonschema:"paths to remove"`
	Message          string   `json:"message,omitempty" jsonschema:"short note shown in the design's history"`
	Title            string   `json:"title,omitempty" jsonschema:"flat display name when this call creates the design"`
	ExpectedRevision *int     `json:"expected_revision,omitempty" jsonschema:"latest revision you read; refused if the design changed since"`
}

// SaveDesignOut reports a design write.
type SaveDesignOut struct {
	Revision   int      `json:"revision"`
	Changed    []string `json:"changed"`
	Created    bool     `json:"created,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
	PrivateURL string   `json:"private_url,omitempty"`
	Flat       FlatInfo `json:"flat"`
}

func (t *tools) saveDesign(ctx context.Context, _ *mcp.CallToolRequest, in SaveDesignIn) (*mcp.CallToolResult, SaveDesignOut, error) {
	var files []core.DesignFile
	if len(in.Files) > 0 {
		decoded, err := decodeFiles(in.Files)
		if err != nil {
			return nil, SaveDesignOut{}, toolErr(err, "nothing was saved; fix the files and save again")
		}
		for _, f := range decoded {
			files = append(files, core.DesignFile{Path: f.Path, Data: f.Data})
		}
	}
	res, err := t.svc.WriteDesign(ctx, in.Slug, core.DesignWrite{Files: files, Delete: in.Delete, Message: in.Message, ExpectedRevision: in.ExpectedRevision, Name: in.Title}, core.ViaMCP)
	if err != nil {
		hint := "nothing was saved; fix the problem above and save again"
		if _, ok := bundle.IsValidation(err); ok {
			hint = "nothing was saved; fix every listed problem and save again (guide topic.canvas)"
		}
		return nil, SaveDesignOut{}, toolErr(err, hint)
	}
	f, _ := t.svc.GetFlat(ctx, in.Slug)
	out := SaveDesignOut{Revision: res.Revision, Changed: res.Changed, Created: res.Created, PrivateURL: f.PrivateURL, Flat: flatInfo(f), Warnings: designWarnings(files)}
	if out.Changed == nil {
		out.Changed = []string{}
	}
	var b strings.Builder
	switch {
	case res.Created:
		fmt.Fprintf(&b, "Created design %s (revision %d). It is live, %s, at %s; there is no publish step.", in.Slug, res.Revision, f.Visibility, f.PrivateURL)
	case len(res.Changed) == 0:
		fmt.Fprintf(&b, "Nothing changed; design %s stays at revision %d.", in.Slug, res.Revision)
	default:
		fmt.Fprintf(&b, "Saved revision %d of design %s (%d files changed). It is live at %s.", res.Revision, in.Slug, len(res.Changed), f.PrivateURL)
	}
	if f.Visibility.Public() {
		b.WriteString(" This design is Public: everyone on the internet sees the change now.")
	}
	writePending(&b, f.PrivateState, f.PrivateDetail)
	for _, w := range out.Warnings {
		b.WriteString("\nWarning: " + w)
	}
	return result(b.String(), out), out, nil
}

// designWarnings is a light lint of written artboards.
func designWarnings(files []core.DesignFile) []string {
	var out []string
	for _, f := range files {
		if !design.IsArtboardPath(f.Path) {
			continue
		}
		low := strings.ToLower(string(f.Data))
		if !strings.Contains(low, "<title") {
			out = append(out, f.Path+": no <title>; name the screen for accessibility")
		}
		if !strings.Contains(low, `name="viewport"`) && !strings.Contains(low, "name=viewport") {
			out = append(out, f.Path+": no viewport meta; a fill artboard or a direct open on a phone will not scale")
		}
		if len(out) >= 20 {
			break
		}
	}
	if lw := bundle.Lint(toBundle(files)); len(lw) > 0 {
		for _, w := range lw {
			if strings.Contains(w.Message, "version") || strings.Contains(w.Message, "CDN") {
				out = append(out, w.Path+": "+w.Message)
			}
		}
	}
	return out
}

func toBundle(files []core.DesignFile) []bundle.File {
	out := make([]bundle.File, 0, len(files))
	for _, f := range files {
		out = append(out, bundle.File{Path: f.Path, Data: f.Data})
	}
	return out
}

// GetDesignIn reads a design.
type GetDesignIn struct {
	Slug string `json:"slug" jsonschema:"design slug"`
	Path string `json:"path,omitempty" jsonschema:"one file to read; omit for the canvas and file list"`
}

// DesignFileOut is one file's content.
type DesignFileOut struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
	Hash     string `json:"hash"`
	Gen      int64  `json:"gen,omitempty"`
	Size     int64  `json:"size"`
}

// GetDesignOut is a design's state or one file.
type GetDesignOut struct {
	State *design.State  `json:"state,omitempty"`
	File  *DesignFileOut `json:"file,omitempty"`
}

func fileOut(p string, data []byte, f design.SnapFile) *DesignFileOut {
	out := &DesignFileOut{Path: p, Hash: f.Hash, Gen: f.Gen, Size: int64(len(data)), Encoding: "utf8"}
	if utf8.Valid(data) {
		out.Content = string(data)
	} else {
		out.Content, out.Encoding = base64.StdEncoding.EncodeToString(data), "base64"
	}
	return out
}

func (t *tools) getDesign(ctx context.Context, _ *mcp.CallToolRequest, in GetDesignIn) (*mcp.CallToolResult, GetDesignOut, error) {
	if in.Path != "" {
		data, f, err := t.svc.DesignFileContent(ctx, in.Slug, in.Path)
		if err != nil {
			return nil, GetDesignOut{}, toolErr(err, "call get_design without path for the file list")
		}
		out := GetDesignOut{File: fileOut(in.Path, data, f)}
		return result(fmt.Sprintf("%s of design %s (%d bytes, gen %d).", in.Path, in.Slug, len(data), f.Gen), out), out, nil
	}
	st, err := t.svc.GetDesign(ctx, in.Slug)
	if err != nil {
		return nil, GetDesignOut{}, toolErr(err, "create a design with create_flat {type: design} or save_design")
	}
	out := GetDesignOut{State: st}
	return result(fmt.Sprintf("Design %s at revision %d: %d artboards, %d files.", in.Slug, st.Revision, len(st.Canvas.Boards), len(st.Files)), out), out, nil
}

// ListRevisionsIn lists revisions.
type ListRevisionsIn struct {
	Slug   string `json:"slug" jsonschema:"design slug"`
	Before int    `json:"before,omitempty" jsonschema:"list revisions older than this number (pass the previous call's next)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"at most this many (1-100, default 50)"`
}

func (t *tools) listRevisions(ctx context.Context, _ *mcp.CallToolRequest, in ListRevisionsIn) (*mcp.CallToolResult, design.RevisionPage, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	page, err := t.svc.ListRevisions(ctx, in.Slug, in.Before, limit)
	if err != nil {
		return nil, design.RevisionPage{}, toolErr(err, "list_revisions works for designs")
	}
	text := fmt.Sprintf("%d revisions of %s (pins %d of %s, keeps %s unpinned).", len(page.Revisions), in.Slug, page.Pins, quotaText(page.PinQuota), quotaText(page.RevisionQuota))
	if page.Next > 0 {
		text += fmt.Sprintf(" More: before=%d.", page.Next)
	}
	return result(text, page), page, nil
}

func quotaText(n int) string {
	if n <= 0 {
		return "unlimited"
	}
	return fmt.Sprint(n)
}

// RevisionIn names one revision.
type RevisionIn struct {
	Slug     string `json:"slug" jsonschema:"design slug"`
	Revision int    `json:"revision" jsonschema:"revision number"`
	Path     string `json:"path,omitempty" jsonschema:"one file of that revision to read"`
}

// RevisionOut is one revision.
type RevisionOut struct {
	Revision store.Revision       `json:"revision"`
	Files    []store.RevisionFile `json:"files,omitempty"`
	File     *DesignFileOut       `json:"file,omitempty"`
}

func (t *tools) getRevision(ctx context.Context, _ *mcp.CallToolRequest, in RevisionIn) (*mcp.CallToolResult, RevisionOut, error) {
	r, files, err := t.svc.GetRevision(ctx, in.Slug, in.Revision)
	if err != nil {
		return nil, RevisionOut{}, toolErr(err, "call list_revisions for existing revision numbers")
	}
	out := RevisionOut{Revision: r}
	if in.Path == "" {
		out.Files = files
		return result(fmt.Sprintf("Revision %d of %s: %d files.", r.Number, in.Slug, len(files)), out), out, nil
	}
	snap, err := t.svc.DesignSnapshot(ctx, in.Slug, in.Revision)
	if err != nil {
		return nil, RevisionOut{}, toolErr(err, "")
	}
	defer snap.Release()
	f, ok := snap.File(in.Path)
	if !ok {
		return nil, RevisionOut{}, toolErr(refuse(store.ErrNotFound, "revision %d has no file %q", in.Revision, in.Path), "omit path for the file list")
	}
	data, err := snap.Read(in.Path)
	if err != nil {
		return nil, RevisionOut{}, toolErr(err, "")
	}
	out.File = fileOut(in.Path, data, f)
	return result(fmt.Sprintf("%s as of revision %d of %s.", in.Path, r.Number, in.Slug), out), out, nil
}

// PinRevisionIn pins a revision.
type PinRevisionIn struct {
	Slug     string `json:"slug" jsonschema:"design slug"`
	Revision int    `json:"revision" jsonschema:"revision number"`
	Pinned   bool   `json:"pinned" jsonschema:"true to pin, false to unpin"`
}

// ActionDone reports a revision action.
type ActionDone struct {
	Status   string `json:"status"`
	Revision int    `json:"revision,omitempty"`
}

func (t *tools) pinRevision(ctx context.Context, _ *mcp.CallToolRequest, in PinRevisionIn) (*mcp.CallToolResult, ActionDone, error) {
	if err := t.svc.PinRevision(ctx, in.Slug, in.Revision, in.Pinned); err != nil {
		return nil, ActionDone{}, toolErr(err, "list_revisions shows pins and the pin quota")
	}
	out := ActionDone{Status: "done", Revision: in.Revision}
	verb := "Pinned"
	if !in.Pinned {
		verb = "Unpinned"
	}
	return result(fmt.Sprintf("%s revision %d of %s.", verb, in.Revision, in.Slug), out), out, nil
}

// RollbackRevisionIn restores a revision.
type RollbackRevisionIn struct {
	Slug     string `json:"slug" jsonschema:"design slug"`
	Revision int    `json:"revision" jsonschema:"revision to restore"`
}

func (t *tools) rollbackRevision(ctx context.Context, _ *mcp.CallToolRequest, in RollbackRevisionIn) (*mcp.CallToolResult, ActionDone, error) {
	n, err := t.svc.RollbackRevision(ctx, in.Slug, in.Revision, "Agent", core.ViaMCP)
	if err != nil {
		return nil, ActionDone{}, toolErr(err, "call list_revisions for existing revision numbers")
	}
	out := ActionDone{Status: "done", Revision: n}
	return result(fmt.Sprintf("Restored revision %d of %s as new revision %d. It is live now.", in.Revision, in.Slug, n), out), out, nil
}
