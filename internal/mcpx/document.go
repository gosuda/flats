package mcpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type SaveDocumentIn struct {
	Slug          string   `json:"slug"`
	Markdown      *string  `json:"markdown,omitempty" jsonschema:"required for a single-document save; an explicit empty string intentionally clears the document; omit only for files with replace_bundle true"`
	Files         []FileIn `json:"files,omitempty" jsonschema:"complete docs bundle, including flats.json with type docs; requires replace_bundle true"`
	ReplaceBundle bool     `json:"replace_bundle,omitempty" jsonschema:"explicitly replace every document and asset with files; omitted files are deleted"`
	Title         string   `json:"title,omitempty"`
	Path          string   `json:"path,omitempty" jsonschema:"relative .md or .markdown path; default index.md; only this document changes; other files and settings are preserved"`
	Message       string   `json:"message,omitempty"`
}

func (t *tools) saveDocument(ctx context.Context, _ *mcp.CallToolRequest, in SaveDocumentIn) (*mcp.CallToolResult, SaveOut, error) {
	if in.ReplaceBundle || in.Files != nil {
		if !in.ReplaceBundle || len(in.Files) == 0 || in.Markdown != nil || in.Path != "" || in.Title != "" {
			return nil, SaveOut{}, toolErr(fmt.Errorf("%w: files requires replace_bundle=true and cannot be combined with markdown, path or title", core.ErrInvalid), "")
		}
		raw, err := decodeFiles(in.Files)
		if err != nil {
			return nil, SaveOut{}, toolErr(err, "")
		}
		files, err := bundle.FromFiles(raw, bundle.Limits{MaxBytes: t.svc.UploadLimit()})
		if err != nil {
			return nil, SaveOut{}, toolErr(err, "")
		}
		if !docsBundle(files) {
			return nil, SaveOut{}, toolErr(fmt.Errorf("%w: save_document files must be a docs bundle", core.ErrInvalid), "include flats.json with type docs")
		}
		return t.save(ctx, in.Slug, files, core.SaveMeta{Message: in.Message}, nil)
	}
	if in.Markdown == nil {
		return nil, SaveOut{}, toolErr(fmt.Errorf("%w: markdown is required for a single-document save", core.ErrInvalid), "provide markdown (an empty string intentionally clears the document), or send files with replace_bundle=true")
	}
	if in.Path == "" {
		in.Path = "index.md"
	}
	if !fs.ValidPath(in.Path) || !bundle.IsMarkdown(in.Path) {
		return nil, SaveOut{}, toolErr(fmt.Errorf("%w: path must be a relative .md or .markdown path", core.ErrInvalid), "use index.md or a path inside the bundle")
	}
	v, err := t.svc.SaveDocument(ctx, in.Slug, in.Path, *in.Markdown, in.Title, core.SaveMeta{Message: in.Message}, core.ViaMCP)
	return t.savedDocument(ctx, in.Slug, in.Path, v, err)
}

func (t *tools) savedDocument(ctx context.Context, slug, path string, v store.Version, saveErr error) (*mcp.CallToolResult, SaveOut, error) {
	res, out, err := t.saved(ctx, slug, nil, v, saveErr, true)
	if err != nil {
		return res, out, err
	}
	out.Document = path
	details := fmt.Sprintf("Saved path: %s.", path)
	var m bundle.Manifest
	if json.Unmarshal(v.Manifest, &m) == nil && m.Entry != "" {
		out.Entry = m.Entry
		details += fmt.Sprintf(" Entry document: %s; get_document without doc reads that entry.", m.Entry)
	}
	return result(savedDocumentText(slug, out.Flat, details), out), out, nil
}

func savedDocumentText(slug string, flat *FlatInfo, details ...string) string {
	if flat != nil {
		return documentText("Saved document", *flat, details...)
	}
	text := fmt.Sprintf("Saved document %s. This save succeeded, but current metadata is unavailable. Read get_flat to check its current state; do not repeat this save.", slug)
	for _, detail := range details {
		text += " " + detail
	}
	return text
}

type GetDocumentIn struct {
	Slug        string `json:"slug"`
	Doc         string `json:"doc,omitempty" jsonschema:"exact Markdown path, default entry"`
	Blocks      bool   `json:"blocks,omitempty" jsonschema:"also list live Markdown blocks with the hashes update_document guards on, 1,000 per page"`
	BlockOffset int    `json:"block_offset,omitempty" jsonschema:"with blocks: first block index of the page (blocks_total says how many exist)"`
}

func (t *tools) getDocument(ctx context.Context, _ *mcp.CallToolRequest, in GetDocumentIn) (*mcp.CallToolResult, core.Document, error) {
	var out core.Document
	var err error
	if in.Blocks {
		out, err = t.svc.GetDocumentBlocks(ctx, in.Slug, in.Doc, in.BlockOffset)
	} else {
		out, err = t.svc.GetDocument(ctx, in.Slug, in.Doc)
	}
	if err != nil {
		return nil, core.Document{}, toolErr(err, "read get_flat for type; doc must be a Markdown path in the document bundle (omit doc to read the entry)")
	}
	return result(out.Markdown, out), out, nil
}

type UpdateDocumentIn struct {
	Slug   string                `json:"slug"`
	Doc    string                `json:"doc,omitempty" jsonschema:"exact Markdown path, default entry"`
	Ops    []core.DocumentEditOp `json:"ops" jsonschema:"1 to 32 operations, applied in order to the live text; all apply or none do"`
	IfHash *string               `json:"if_hash,omitempty" jsonschema:"the whole-document hash from get_document; any change since refuses the edit. Required when an op uses nth"`
}

func (t *tools) updateDocument(ctx context.Context, _ *mcp.CallToolRequest, in UpdateDocumentIn) (*mcp.CallToolResult, core.DocumentEdit, error) {
	out, err := t.svc.UpdateDocument(ctx, in.Slug, in.Doc, in.Ops, in.IfHash, core.ViaMCP)
	if err != nil {
		return nil, core.DocumentEdit{}, toolErr(err, editHint(err, in.Slug))
	}
	var b strings.Builder
	if out.Changed {
		fmt.Fprintf(&b, "Edited live %s: seq %d -> %d, %d bytes, hash %s.", out.Doc, out.SeqBefore, out.Seq, out.Bytes, out.Hash)
	} else {
		fmt.Fprintf(&b, "No change to live %s: the operations matched but left the text as it was (seq %d).", out.Doc, out.Seq)
	}
	for i, op := range out.Ops {
		fmt.Fprintf(&b, "\n%d. %s at line %d: -%d/+%d characters", i+1, op.Op, op.Line, op.Removed, op.Inserted)
	}
	writePublic(&b, "", out.PublicNotice)
	return result(b.String(), out), out, nil
}

// editHint tells the caller what to do after a refused or failed live edit.
// After a lost response it depends on whether the edit is live.
func editHint(err error, slug string) string {
	hint := "read get_document {slug, doc, blocks: true} again and retry against the current text"
	switch core.ErrorCategory(err) {
	case "not_deployed":
		hint = "the document is not running; save it with save_document, which starts it at once (topic.docs)"
	case "not_docs":
		hint = "update_document edits docs flats only; check get_flat"
	case "invalid":
		hint = "fix the operation named in the error (topic.docs)"
	case "document_capacity":
		hint = "make the edit smaller or split it into several calls"
		if errors.Is(err, core.ErrDiskQuota) {
			hint = "the flat is at its disk quota, which also holds the edit's revision; tell the user: the operator must raise the per-flat disk quota"
		}
	case "unavailable":
		hint = "wait a second and retry"
		var outcome *core.EditOutcomeError
		if errors.As(err, &outcome) {
			hint = map[string]string{
				core.EditApplied:    fmt.Sprintf("the edit is live at seq %d; do not repeat it; read get_document to continue", outcome.Seq),
				core.EditNotApplied: "nothing changed; send the same call again",
				core.EditUnknown:    "read get_document and check whether the edit is there before retrying",
			}[outcome.Outcome]
		}
	case "not_found", "document_not_found":
		hint = notFoundHint(err, slug)
	}
	return hint
}

func docsBundle(files []bundle.File) bool {
	m, err := bundle.ParseManifest(files)
	return err == nil && m.Type == "docs"
}
