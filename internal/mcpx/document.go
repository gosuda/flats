package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"

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
	Conflict int64  `json:"conflict,omitempty" jsonschema:"positive generation from private conflict metadata; retrieve preserved Markdown"`
	Slug     string `json:"slug"`
	Doc      string `json:"doc,omitempty" jsonschema:"exact Markdown path, default entry"`
}

func (t *tools) getDocument(ctx context.Context, _ *mcp.CallToolRequest, in GetDocumentIn) (*mcp.CallToolResult, core.Document, error) {
	var out core.Document
	var err error
	if in.Conflict != 0 {
		out, err = t.svc.GetDocumentConflict(ctx, in.Slug, in.Doc, in.Conflict)
	} else {
		out, err = t.svc.GetDocument(ctx, in.Slug, in.Doc)
	}
	if err != nil {
		return nil, core.Document{}, toolErr(err, "read get_flat for type and Current Draft; doc must be a Markdown path in the bundle")
	}
	return result(out.Markdown, out), out, nil
}
