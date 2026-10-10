package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type SaveDocumentIn struct {
	Slug     string `json:"slug"`
	Markdown string `json:"markdown"`
	Title    string `json:"title,omitempty"`
	Path     string `json:"path,omitempty" jsonschema:"relative .md or .markdown path; default index.md; complete Draft replacement"`
	Message  string `json:"message,omitempty"`
}

func (t *tools) saveDocument(ctx context.Context, _ *mcp.CallToolRequest, in SaveDocumentIn) (*mcp.CallToolResult, SaveOut, error) {
	if in.Path == "" {
		in.Path = "index.md"
	}
	if !fs.ValidPath(in.Path) || !bundle.IsMarkdown(in.Path) {
		return nil, SaveOut{}, toolErr(fmt.Errorf("%w: path must be a relative .md or .markdown path", core.ErrInvalid), "use index.md or a path inside the bundle")
	}
	manifest := map[string]string{"type": "docs"}
	if in.Title != "" {
		manifest["name"] = in.Title
	}
	b, err := json.Marshal(manifest)
	if err != nil {
		return nil, SaveOut{}, toolErr(err, "")
	}
	return t.save(ctx, in.Slug, []bundle.File{{Path: in.Path, Data: []byte(in.Markdown)}, {Path: bundle.ManifestName, Data: b}}, core.SaveMeta{Message: in.Message}, false, nil)
}

type GetDocumentIn struct {
	Conflict    int64  `json:"conflict,omitempty" jsonschema:"positive generation from private conflict metadata; retrieve preserved Markdown"`
	Slug        string `json:"slug"`
	Doc         string `json:"doc,omitempty" jsonschema:"exact Markdown path, default entry"`
	Blocks      bool   `json:"blocks,omitempty" jsonschema:"also list live Markdown blocks with the hashes update_document guards on, 1,000 per page"`
	BlockOffset int    `json:"block_offset,omitempty" jsonschema:"with blocks: first block index of the page (blocks_total says how many exist)"`
}

func (t *tools) getDocument(ctx context.Context, _ *mcp.CallToolRequest, in GetDocumentIn) (*mcp.CallToolResult, core.Document, error) {
	var out core.Document
	var err error
	if in.Conflict != 0 {
		out, err = t.svc.GetDocumentConflict(ctx, in.Slug, in.Doc, in.Conflict)
	} else if in.Blocks {
		out, err = t.svc.GetDocumentBlocks(ctx, in.Slug, in.Doc, in.BlockOffset)
	} else {
		out, err = t.svc.GetDocument(ctx, in.Slug, in.Doc)
	}
	if err != nil {
		return nil, core.Document{}, toolErr(err, "read get_flat for type and Current Draft; doc must be a Markdown path in the bundle")
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
		hint := "read get_document {slug, doc, blocks: true} again and retry against the current text"
		switch core.ErrorCategory(err) {
		case "not_deployed":
			hint = "no docs version is running; use save_document and publish (topic.docs)"
		case "not_docs":
			hint = "update_document edits docs flats only; check get_flat"
		case "invalid":
			hint = "fix the operation named in the error (topic.docs)"
		case "document_capacity":
			hint = "make the edit smaller or split it into several calls"
		case "not_found", "document_not_found":
			hint = notFoundHint(err, in.Slug)
		}
		return nil, core.DocumentEdit{}, toolErr(err, hint)
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
