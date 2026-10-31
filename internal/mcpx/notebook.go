package mcpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/notebook"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type notebookService interface {
	CreateNotebook(context.Context, string, string, core.Via) (core.FlatView, error)
	GetNotebook(context.Context, string, int, []byte) (json.RawMessage, error)
	UpdateNotebook(context.Context, string, []byte, core.Via) (core.NotebookWriteResult, error)
	UploadLimit() int64
}

func (t *tools) notebooks() notebookService {
	if t.notebook != nil {
		return t.notebook
	}
	return t.svc
}

func registerNotebook(s *mcp.Server, t *tools) {
	no := false
	mcp.AddTool(s, &mcp.Tool{Name: "get_notebook", InputSchema: notebookReadSchema(), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &no},
		Description: "Read a notebook's page tree, a page's blocks and database schemas, or database rows. Select a saved view or ad-hoc sort/filter, not both; format markdown includes mutation-ready references. Pass next as cursor alone for the same captured state; expired cursors require a fresh read. For unavailable admission, repeat the same read with retry_hint set to the returned admission ticket before expiry. With op_id and cursor, read the immutable write-result mapping instead. revision reads retained content. Read before editing; guide topic.notebook."}, t.getNotebook)
	mcp.AddTool(s, &mcp.Tool{Name: "update_notebook", InputSchema: notebookUpdateSchema(), Annotations: &mcp.ToolAnnotations{DestructiveHint: &no},
		Description: "Apply typed page, block, database, property, option, row and view operations, images and markdown_to_blocks atomically as one revision. Creates a notebook on a new or untyped slug. Existing mutation references require id and inc; temporary names refer only to earlier creations. Optional guards protect live state, including people's pending edits. op_id (1-128 ASCII letters, digits, . _ : -) makes matching resends safe. Applies immediately: no Draft, publish or approval; Public viewers see changes now. Visibility and deletion still follow approval policy. Guide topic.notebook."}, t.updateNotebook)
}

func notebookReadQuery(in map[string]json.RawMessage) (string, int, []byte, error) {
	var slug string
	if err := json.Unmarshal(in["slug"], &slug); err != nil || slug == "" {
		return "", 0, nil, errors.New("slug is required")
	}
	values := url.Values{}
	for key, raw := range in {
		if key == "slug" {
			continue
		}
		switch key {
		case "cursor", "op_id", "format", "retry_hint":
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				return "", 0, nil, fmt.Errorf("%s must be a string", key)
			}
			values.Set(key, text)
		case "page", "database", "view":
			var text string
			if json.Unmarshal(raw, &text) == nil {
				values.Set(key, text)
			} else {
				values.Set(key, string(raw))
			}
		default:
			values.Set(key, string(raw))
		}
	}
	rev, query, err := api.NotebookQuery(values)
	return slug, rev, query, err
}

func (t *tools) getNotebook(ctx context.Context, _ *mcp.CallToolRequest, in map[string]json.RawMessage) (*mcp.CallToolResult, map[string]any, error) {
	slug, rev, query, err := notebookReadQuery(in)
	if err != nil {
		return nil, nil, notebookToolErr(&notebook.WriteError{Code: "invalid", Detail: err.Error()}, slug)
	}
	raw, err := t.notebooks().GetNotebook(ctx, slug, rev, query)
	if err != nil {
		return nil, nil, notebookToolErr(err, slug)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, nil, toolErr(err, "the notebook returned an invalid response; inspect get_logs")
	}
	text := "Notebook " + slug + "."
	if rev > 0 {
		text += " Retained revision " + strconv.Itoa(rev) + "."
	}
	if out["next"] != nil && out["next"] != "" {
		text += " More: pass next as cursor alone (also retain op_id for a mapping)."
	}
	if out["pending"] == true {
		text += " Live edits are not all recorded as revisions yet."
	}
	return result(text, out), out, nil
}

func (t *tools) updateNotebook(ctx context.Context, _ *mcp.CallToolRequest, in map[string]json.RawMessage) (*mcp.CallToolResult, map[string]any, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, nil, err
	}
	update, err := notebook.DecodeUpdate(raw)
	if err != nil {
		return nil, nil, notebookToolErr(&notebook.WriteError{Code: "invalid", Detail: err.Error()}, "")
	}
	remaining := t.notebooks().UploadLimit()
	for _, asset := range update.Assets {
		if int64(len(asset.Data)) > remaining {
			return nil, nil, notebookToolErr(&notebook.WriteError{Code: "invalid", Reason: "asset_limit", Detail: "total decoded image bytes exceed upload_max_bytes"}, update.Slug)
		}
		remaining -= int64(len(asset.Data))
	}
	delete(in, "slug")
	body, err := json.Marshal(in)
	if err != nil {
		return nil, nil, err
	}
	res, err := t.notebooks().UpdateNotebook(ctx, update.Slug, body, core.ViaMCP)
	if err != nil {
		return nil, nil, notebookToolErr(err, update.Slug)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Result, &out); err != nil {
		return nil, nil, toolErr(err, "the write may already be committed; reconcile with get_notebook, do not use a new op_id")
	}
	if res.Created {
		out["created"] = true
	}
	out["revision_pending"] = res.Pending
	if !res.Pending && res.Revision > 0 {
		out["revision"] = res.Revision
	} else {
		out["revision"] = nil
	}
	text := "Applied the call to notebook " + update.Slug + "."
	if out["replay"] == true {
		text = "op_id " + update.OpID + " was already applied; returned its original result without another edit."
	} else if res.Created {
		text = "Created notebook " + update.Slug + " and applied the call."
	}
	if res.Pending {
		text += " Warning: committed and live, but revision import is pending. Do not repeat the write with a new op_id."
	} else if res.Revision > 0 {
		text += fmt.Sprintf(" Revision %d.", res.Revision)
	}
	text += " There is no publish step."
	if t.svc != nil {
		if f, err := t.svc.GetFlat(ctx, update.Slug); err == nil && string(f.Visibility) == "public" {
			text += " This notebook is Public: everyone on the internet sees the change now."
		}
	}
	return result(text, out), out, nil
}

func notebookCategory(err error) string {
	var refusal *notebook.WriteError
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	if errors.Is(err, core.ErrNotNotebook) {
		return "not_notebook"
	}
	return core.ErrorCategory(err)
}

func notebookToolErr(err error, slug string) error {
	category := notebookCategory(err)
	hint := "nothing changed; correct the named field (guide topic.notebook)"
	switch category {
	case "invalid":
	case "not_notebook":
		hint = "notebook tools need a notebook; check get_flat or create_flat {type: notebook}"
	case "conflict":
		hint = "nothing changed; read get_notebook again from the first page and reconcile current references, guards or op_id content; expired result mappings cannot be reconstructed from live content"
	case "notebook_capacity":
		hint = "nothing changed; inspect the scoped item limit or disk quota; the operator can raise the disk quota or free history storage through revision quotas and pins"
	case "unavailable":
		hint = "retry only when retry is true, preserving the same op_id and request; inspect get_flat and get_logs for a stopped notebook"
	case "not_found":
		hint = "check get_flat and list_revisions; the selected notebook, page or retained revision does not exist"
	case "forbidden":
		hint = "access denied; do not retry through a less restricted route"
	default:
		return toolErr(err, notFoundHint(err, slug))
	}
	detail := map[string]any{"error": err.Error(), "category": category, "hint": hint + ". See guide refusal." + category + "."}
	var refusal *notebook.WriteError
	if errors.As(err, &refusal) {
		detail["reason"], detail["retry"], detail["detail"] = refusal.Reason, refusal.Retry, refusal.Detail
	}
	raw, _ := json.Marshal(detail)
	return errors.New(err.Error() + "\nHint: " + detail["hint"].(string) + "\n" + string(raw))
}

func notebookRevisionQuery(in RevisionIn) (map[string]json.RawMessage, bool, error) {
	selected := in.Page != nil || in.Database != nil || in.Format != "" || in.Cursor != "" || in.View != nil || in.Sort != nil || in.Filter != nil || in.Limit != 0
	if !selected {
		return nil, false, nil
	}
	if in.Path != "" {
		return nil, true, errors.New("notebook selectors cannot be combined with path")
	}
	if in.Revision <= 0 {
		return nil, true, errors.New("revision must be positive")
	}
	fields := map[string]any{"slug": in.Slug}
	if in.Cursor == "" {
		fields["revision"] = in.Revision
	}
	for _, item := range []struct {
		name    string
		value   any
		present bool
	}{
		{"page", in.Page, in.Page != nil}, {"database", in.Database, in.Database != nil}, {"view", in.View, in.View != nil},
		{"sort", in.Sort, in.Sort != nil}, {"filter", in.Filter, in.Filter != nil}, {"format", in.Format, in.Format != ""}, {"cursor", in.Cursor, in.Cursor != ""}, {"limit", in.Limit, in.Limit != 0},
	} {
		if item.present {
			fields[item.name] = item.value
		}
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, true, err
	}
	var query map[string]json.RawMessage
	err = json.Unmarshal(raw, &query)
	return query, true, err
}

func notebookCreateSchema() map[string]any {
	return notebookSchemaObject(map[string]any{"slug": notebookSchemaString(), "name": notebookSchemaString(), "type": notebookSchemaEnum("flat", "page", "docs", "design", "board", "notebook")}, "name", "type")
}

func notebookRevisionSchema() map[string]any {
	read := notebookReadSchema()
	props := read["properties"].(map[string]any)
	delete(props, "op_id")
	delete(props, "retry_hint")
	props["path"] = notebookSchemaString()
	var branches []any
	for _, value := range read["oneOf"].([]any)[1:5] {
		branch := value.(map[string]any)
		delete(branch["properties"].(map[string]any), "retry_hint")
		branch["properties"].(map[string]any)["revision"] = props["revision"]
		required := branch["required"].([]string)
		branch["required"] = append(required, "revision")
		branches = append(branches, branch)
	}
	branches = append(branches, notebookSchemaObject(map[string]any{"slug": props["slug"], "revision": props["revision"], "path": props["path"]}, "path"))
	read["oneOf"] = branches
	read["required"] = []string{"slug", "revision"}
	return read
}

func notebookHintForType(err error, hint string) string {
	if strings.Contains(err.Error(), "notebook") {
		return "notebooks have no Draft, versions, previews or deployment; use get_notebook, update_notebook and the revision tools (guide topic.notebook)"
	}
	return hint
}
