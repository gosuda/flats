package mcpx

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SavePageIn saves a page: one static HTML document plus its assets.
type SavePageIn struct {
	ExpectedRevision *int     `json:"expected_revision,omitempty" jsonschema:"current Draft revision expected; 0 means no Draft exists; conflicts never overwrite"`
	Slug             string   `json:"slug" jsonschema:"flat slug (created when missing)"`
	HTML             string   `json:"html" jsonschema:"the complete HTML document; saved as index.html"`
	Title            string   `json:"title,omitempty" jsonschema:"display name of the page"`
	Screenshot       string   `json:"screenshot,omitempty" jsonschema:"path of an image in files that the operator's console shows as the thumbnail"`
	Files            []FileIn `json:"files,omitempty" jsonschema:"every asset the page uses (scripts, styles, images, fonts, chart data), no HTML; omit for a self-contained page"`
	Message          string   `json:"message,omitempty" jsonschema:"short note about this version"`
	Deploy           bool     `json:"deploy,omitempty" jsonschema:"request publish approval after saving; never activates directly"`
}

func (t *tools) savePage(ctx context.Context, _ *mcp.CallToolRequest, in SavePageIn) (*mcp.CallToolResult, SaveOut, error) {
	if in.HTML == "" {
		return nil, SaveOut{}, toolErr(refuse(core.ErrInvalid, "html is empty"), "send the complete HTML document of the page")
	}
	var assets []bundle.File
	if len(in.Files) > 0 {
		var err error
		if assets, err = decodeFiles(in.Files); err != nil {
			return nil, SaveOut{}, toolErr(err, "")
		}
	}
	verr := &bundle.ValidationError{}
	for _, f := range assets {
		switch {
		case f.Path == bundle.ManifestName:
			verr.Problems = append(verr.Problems, bundle.Problem{Path: f.Path, Message: "save_page writes flats.json itself", Fix: "remove flats.json from files; use title and screenshot instead"})
		case bundle.IsHTML(f.Path):
			verr.Problems = append(verr.Problems, bundle.Problem{Path: f.Path, Message: "a page has one HTML document, sent as html", Fix: "inline this content into html, or use a website flat (save_draft) for several pages"})
		}
	}
	if len(verr.Problems) > 0 {
		return nil, SaveOut{}, toolErr(verr, "no Draft was saved; fix every listed problem and save again")
	}
	manifest := map[string]string{"type": contenttype.Page}
	if in.Title != "" {
		manifest["name"] = in.Title
	}
	if in.Screenshot != "" {
		manifest["screenshot"] = in.Screenshot
	}
	m, err := json.Marshal(manifest)
	if err != nil {
		return nil, SaveOut{}, toolErr(err, "")
	}
	raw := append([]bundle.File{{Path: "index.html", Data: []byte(in.HTML)}, {Path: bundle.ManifestName, Data: m}}, assets...)
	files, err := bundle.FromFiles(raw, bundle.Limits{MaxBytes: t.svc.UploadLimit()})
	if err != nil {
		return nil, SaveOut{}, toolErr(err, "no Draft was saved; fix every listed problem and call save_page again with the html and every asset")
	}
	return t.save(ctx, in.Slug, files, core.SaveMeta{Message: in.Message}, in.Deploy, in.ExpectedRevision)
}

// pageCreatedText describes a page that create_flat just made.
func pageCreatedText(fi FlatInfo) string {
	rev := 0
	if fi.Draft != nil {
		rev = fi.Draft.Revision
	}
	return fmt.Sprintf("Created unpublished Private page %s with a starter Draft (revision %d). Next: save_page, open_preview with version 0, then publish and wait for operator approval (topic.page).", fi.Slug, rev)
}
