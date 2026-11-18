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
	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/sheet"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func registerSheet(s *mcp.Server, t *tools) {
	no := false
	mcp.AddTool(s, &mcp.Tool{Name: "get_sheet", InputSchema: sheetReadSchema(), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &no},
		Description: "Read a sheet: without tab the tab list, with tab the cells of a range (default: the used range) as typed values, with formulas as {formula, value} or {formula, error}. include adds shown text, formats or widths; format csv returns CSV. Every read returns state, which update_sheet needs. A page holds at most limit cells; pass next.cursor as cursor alone for the next page. Read retained revisions with get_revision. Guide topic.sheet."}, t.getSheet)
	mcp.AddTool(s, t.describe(&mcp.Tool{Name: "update_sheet", InputSchema: sheetUpdateSchema(), Annotations: &mcp.ToolAnnotations{DestructiveHint: &no},
		Description: "Apply write, clear, format, widths, row and column, sort, tab and import_csv operations to a sheet in order, atomically, as one revision. state names the read the call is based on; a conflict means read again and resend. Creates a sheet on a new or untyped slug (that call needs no state). Optional guard refuses the call if the sheet or the named ranges changed. op_id (1-128 ASCII letters, digits, . _ : -) makes a matching resend safe. Applies immediately: no Draft, publish or approval; Public viewers see changes now. Visibility and deletion still follow approval policy. Guide topic.sheet."}), t.updateSheet)
}

// sheetReadQuery turns the selectors of a tool call into the core query.
func sheetReadQuery(in map[string]json.RawMessage) (string, int, []byte, error) {
	var slug string
	if err := json.Unmarshal(in["slug"], &slug); err != nil || slug == "" {
		return "", 0, nil, errors.New("slug is required")
	}
	values := url.Values{}
	for key, raw := range in {
		switch key {
		case "slug":
		case "tab", "range", "format", "cursor":
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				return slug, 0, nil, fmt.Errorf("%s must be a string", key)
			}
			values.Set(key, text)
		case "include":
			var parts []string
			if err := json.Unmarshal(raw, &parts); err != nil || len(parts) == 0 {
				return slug, 0, nil, errors.New("include must be a list of shown, formats or widths")
			}
			values.Set(key, strings.Join(parts, ","))
		default:
			values.Set(key, string(raw))
		}
	}
	rev, query, err := api.SheetQuery(values)
	return slug, rev, query, err
}

func (t *tools) getSheet(ctx context.Context, _ *mcp.CallToolRequest, in map[string]json.RawMessage) (*mcp.CallToolResult, map[string]any, error) {
	slug, _, query, err := sheetReadQuery(in)
	if err != nil {
		return nil, nil, sheetToolErr(&sheet.WriteError{Code: "invalid", Detail: err.Error()}, slug)
	}
	raw, err := t.svc.GetSheet(ctx, slug, 0, query)
	if err != nil {
		return nil, nil, sheetToolErr(err, slug)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, nil, toolErr(err, "the sheet returned an invalid response; inspect get_logs")
	}
	return result("Sheet "+slug+"."+sheetMoreText(out), out), out, nil
}

func sheetMoreText(out map[string]any) string {
	if next, _ := out["next"].(map[string]any); next != nil {
		return " More: pass next.cursor as cursor alone."
	}
	return ""
}

func (t *tools) updateSheet(ctx context.Context, req *mcp.CallToolRequest, _ map[string]json.RawMessage) (*mcp.CallToolResult, map[string]any, error) {
	// The sheet reads the arguments as the client sent them. The decoded
	// map is not used: it holds one of two fields of the same name, and
	// the call is refused for having two.
	update, err := sheet.DecodeUpdate(req.Params.Arguments)
	if err != nil {
		return nil, nil, sheetToolErr(err, "")
	}
	slug := update.Slug
	if slug == "" {
		return nil, nil, sheetToolErr(&sheet.WriteError{Code: "invalid", Detail: "slug is required"}, "")
	}
	res, err := t.svc.UpdateSheetCall(ctx, slug, update, core.ViaMCP)
	if err != nil {
		return nil, nil, sheetToolErr(err, slug)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Result, &out); err != nil {
		return nil, nil, toolErr(err, "the write may already be committed; read the sheet with get_sheet before any resend")
	}
	text := "Applied the call to sheet " + slug + "."
	if out["replay"] == true {
		text = "This op_id was already applied; returned its original result without another write."
	} else if res.Created {
		text = "Created sheet " + slug + " and applied the call."
	}
	if res.Pending {
		text += " Warning: committed and live, but its revision is not recorded yet. Do not repeat the write."
	} else if res.Revision > 0 {
		text += fmt.Sprintf(" Revision %d.", res.Revision)
	}
	if n, _ := out["formula_errors"].(float64); n > 0 {
		text += fmt.Sprintf(" %d formulas of this call show an error; see errors.", int(n))
	}
	text += " There is no publish step."
	if res.Flat != nil {
		// The call that creates a sheet says where it opens (spec rule 53).
		fi := flatInfo(*res.Flat)
		out["flat"] = fi
		text += fmt.Sprintf(" It is live, %s, at %s", fi.Visibility, linkText(fi))
		if !strings.HasSuffix(text, ".") {
			text += "."
		}
	}
	if f, err := t.svc.GetFlat(ctx, slug); err == nil && string(f.Visibility) == "public" {
		text += " This sheet is Public: everyone on the internet sees the change now."
	}
	return result(text, out), out, nil
}

// sheetCategory is the refusal category of a sheet error, or "" when err is
// no sheet refusal.
func sheetCategory(err error) string {
	var refusal *sheet.WriteError
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	if errors.Is(err, core.ErrNotSheet) {
		return "not_sheet"
	}
	return ""
}

func sheetToolErr(err error, slug string) error {
	// A host error without a sheet refusal gets the refusal the management
	// API answers for it: every unavailable sheet call can be retried.
	var known *sheet.WriteError
	if !errors.As(err, &known) && errors.Is(err, core.ErrUnavailable) {
		err = fmt.Errorf("%w: %w", err, &sheet.WriteError{Code: "unavailable", Reason: "sheet_unavailable", Retry: true})
	}
	category := sheetCategory(err)
	if category == "" {
		category = errorCategory(err)
	}
	hint := "nothing changed; correct the named field (guide topic.sheet)"
	switch category {
	case "invalid":
	case "not_sheet":
		hint = "sheet tools need a sheet; check get_flat or create_flat {type: sheet}"
	case "conflict":
		hint = "nothing changed; read the sheet again with get_sheet and resend the call with the new state. For op_id_reuse use a new op_id"
	case "sheet_capacity":
		hint = "nothing changed; the named limit or the disk quota is reached. Write less, or ask the operator to raise the disk quota or free history through revision quotas and pins"
	case "unavailable":
		hint = "nothing changed; retry the same call with the same op_id when retry is true; inspect get_flat and get_logs for a stopped sheet"
	case "not_found":
		hint = "check get_flat and list_revisions; the sheet, the tab or the retained revision does not exist"
	case "forbidden":
		hint = "access denied; do not retry through a less restricted route"
	default:
		return toolErr(err, notFoundHint(err, slug))
	}
	detail := map[string]any{"error": err.Error(), "category": category, "hint": hint + ". See guide refusal." + category + "."}
	var refusal *sheet.WriteError
	if errors.As(err, &refusal) {
		detail["reason"], detail["retry"] = refusal.Reason, refusal.Retry
		for name, value := range map[string]string{"limit": refusal.Limit, "name": refusal.Name, "detail": refusal.Detail} {
			if value != "" {
				detail[name] = value
			}
		}
		if refusal.Max != 0 || refusal.Value != 0 {
			detail["max"], detail["value"] = refusal.Max, refusal.Value
		}
	}
	raw, _ := json.Marshal(detail)
	return errors.New(err.Error() + "\nHint: " + detail["hint"].(string) + "\n" + string(raw))
}

// sheetRevisionQuery reports whether a get_revision call reads sheet content
// and returns its selectors. tab, range, include and the json and csv
// formats belong to sheets only; a call with only cursor or limit reads
// sheet content when the flat is a sheet. On a flat of another type limit
// alone is an error of a call that selects nothing. An empty include is
// passed on as given, so the sheet's reader refuses it as for get_sheet.
func (t *tools) sheetRevisionQuery(ctx context.Context, in RevisionIn) (map[string]json.RawMessage, bool, error) {
	named := in.Tab != "" || in.Range != "" || in.Include != nil || in.Format == "json" || in.Format == "csv"
	if !named {
		other := in.Page != nil || in.Database != nil || in.View != nil || in.Sort != nil || in.Filter != nil || in.Format != ""
		if other || (in.Cursor == "" && in.Limit == 0) || t.svc == nil {
			return nil, false, nil
		}
		f, err := t.svc.GetFlat(ctx, in.Slug)
		if err != nil {
			return nil, false, nil
		}
		if f.Type != contenttype.Sheet {
			if in.Cursor == "" {
				// The schema admits limit alone for sheets. No other type
				// reads a revision by it.
				return nil, false, refuse(core.ErrInvalid, "limit alone reads the tab list of a sheet revision, and %s is no sheet: omit limit for the revision's files, give path for one file, or give the notebook selectors with it", in.Slug)
			}
			return nil, false, nil
		}
	}
	if in.Path != "" {
		return nil, true, errors.New("sheet selectors cannot be combined with path")
	}
	if in.Revision <= 0 {
		return nil, true, errors.New("revision must be positive")
	}
	fields := map[string]any{"slug": in.Slug, "revision": in.Revision}
	for _, item := range []struct {
		name    string
		value   any
		present bool
	}{
		{"tab", in.Tab, in.Tab != ""}, {"range", in.Range, in.Range != ""}, {"format", in.Format, in.Format != ""},
		{"include", in.Include, in.Include != nil}, {"limit", in.Limit, in.Limit != 0}, {"cursor", in.Cursor, in.Cursor != ""},
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

// getSheetRevision reads sheet content of a retained revision, calculated
// with the date the revision closed with.
func (t *tools) getSheetRevision(ctx context.Context, in RevisionIn, query map[string]json.RawMessage) (*mcp.CallToolResult, RevisionOut, error) {
	slug, rev, raw, err := sheetReadQuery(query)
	if err != nil {
		return nil, RevisionOut{}, sheetToolErr(&sheet.WriteError{Code: "invalid", Detail: err.Error()}, in.Slug)
	}
	data, err := t.svc.GetSheet(ctx, slug, rev, raw)
	if err != nil {
		return nil, RevisionOut{}, sheetToolErr(err, slug)
	}
	var content map[string]any
	if err := json.Unmarshal(data, &content); err != nil {
		return nil, RevisionOut{}, toolErr(err, "inspect get_logs for the historical read failure")
	}
	out := RevisionOut{Sheet: content}
	return result("Sheet "+slug+" as of revision "+strconv.Itoa(rev)+"."+sheetMoreText(content), out), out, nil
}

// sheetRevisionSchema adds the sheet selectors of get_revision to the
// notebook's branches.
func sheetRevisionSchema() map[string]any {
	schema := notebookRevisionSchema()
	props := schema["properties"].(map[string]any)
	selectors := sheetSchemaSelectors()
	branch := map[string]any{"slug": props["slug"], "revision": props["revision"]}
	for name, selector := range selectors {
		branch[name] = selector
	}
	sheetBranch := notebookSchemaObject(branch, "tab", "range", "format", "include", "limit")
	// A call with none of these is the metadata read of the path branch.
	sheetBranch["anyOf"] = []any{
		map[string]any{"required": []string{"tab"}}, map[string]any{"required": []string{"format"}}, map[string]any{"required": []string{"include"}},
		map[string]any{"required": []string{"limit"}},
	}
	sheetBranch["dependentRequired"] = map[string]any{"range": []string{"tab"}}
	schema["oneOf"] = append(schema["oneOf"].([]any), sheetBranch)
	props["tab"], props["range"], props["include"] = selectors["tab"], selectors["range"], selectors["include"]
	props["format"] = notebookSchemaEnum("structured", "markdown", "json", "csv")
	props["limit"] = sheetSchemaInteger(1, 10000)
	return schema
}

func sheetHintForType(err error, hint string) string {
	if errors.Is(err, core.ErrSheetUnversioned) {
		return "sheets have no Draft, versions, previews or deployment; use get_sheet, update_sheet and the revision tools (guide topic.sheet)"
	}
	return hint
}
