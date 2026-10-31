package mcpx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gosuda/flats/internal/cli"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/notebook"
)

type notebookHistoryItem struct {
	Ref           notebook.Ref     `json:"ref"`
	Kind          string           `json:"kind"`
	Title         string           `json:"title"`
	Parent        *notebook.ID     `json:"parent"`
	Name          string           `json:"name"`
	Color         string           `json:"color"`
	Database      notebook.ID      `json:"database"`
	Content       notebook.Content `json:"content"`
	PropertyOrder []notebook.ID    `json:"property_order"`
	ViewOrder     []notebook.ID    `json:"view_order"`
	Definition    struct {
		Name    string `json:"name"`
		Type    string `json:"type"`
		Display struct {
			Layout string `json:"layout"`
		} `json:"display"`
	} `json:"definition"`
	Cells map[notebook.ID]struct {
		Type  string `json:"type"`
		Value any    `json:"value"`
	} `json:"cells"`
}

type notebookHistoryRead struct {
	State    notebook.State        `json:"state"`
	Revision *int                  `json:"revision"`
	Pending  bool                  `json:"pending"`
	Tree     []notebookHistoryItem `json:"tree"`
	Page     *struct {
		Items []notebookHistoryItem `json:"items"`
	} `json:"page"`
	Schema     []notebookHistoryItem `json:"schema"`
	Rows       []notebookHistoryItem `json:"rows"`
	Markdown   string                `json:"markdown"`
	Offset     int                   `json:"offset"`
	TotalBytes int                   `json:"total_bytes"`
	References []struct {
		Ref    notebook.Ref `json:"ref"`
		Marker string       `json:"marker"`
	} `json:"references"`
	Next     *string `json:"next"`
	Complete bool    `json:"complete"`
}

type notebookHistoryFixture struct {
	e        *env
	slug     string
	page     notebook.Ref
	refs     map[string]notebook.AssignedRef
	revision int
	markdown string
	blocks   []notebook.ID
}

func newNotebookHistoryFixture(t *testing.T) notebookHistoryFixture {
	t.Helper()
	e := newEnv(t)
	const slug = "historical-notes"
	if _, err := e.svc.CreateNotebook(t.Context(), slug, "Historical notes", core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	initial := integratedNotebookGet(t, e, slug, map[string]any{})
	page := initial.Tree[0].Ref
	tmp := func(name string) map[string]string { return map[string]string{"tmp": name} }
	place := func(page any) map[string]any { return map[string]any{"page": page, "parent": nil, "before": nil} }
	ops := []any{
		map[string]any{"op": "set_page_title", "page": page, "title": "Earlier page"},
		map[string]any{"op": "create_page", "ref": "child", "parent": page, "before": nil, "title": "Earlier child"},
		map[string]any{"op": "create_page", "ref": "grandchild", "parent": tmp("child"), "before": nil, "title": "Earlier grandchild"},
		map[string]any{"op": "create_page", "ref": "peer", "parent": nil, "before": nil, "title": "Earlier peer"},
		map[string]any{"op": "create_database", "ref": "db", "block_ref": "owner", "place": place(tmp("child")), "title": "Earlier database", "title_property_name": "Task"},
		map[string]any{"op": "create_property", "ref": "status", "database": tmp("db"), "definition": map[string]any{"name": "Earlier status", "type": "select"}},
		map[string]any{"op": "create_option", "ref": "open", "property": tmp("status"), "name": "Earlier open", "color": "blue"},
		map[string]any{"op": "set_view", "view": tmp("db.view"), "definition": map[string]any{"name": "Earlier view", "display": map[string]any{"layout": "table", "visible": []any{tmp("db.title"), tmp("status")}, "widths": []any{}}, "sort": []any{}, "filter": []any{}}},
	}
	for i := range 3 {
		ops = append(ops, map[string]any{"op": "create_row", "ref": fmt.Sprintf("row%d", i), "database": tmp("db"), "before": nil, "cells": []any{
			map[string]any{"property": tmp("db.title"), "value": fmt.Sprintf("Earlier row %d", i)},
			map[string]any{"property": tmp("status"), "value": tmp("open")},
		}})
	}
	texts := []string{}
	for i := range 6 {
		text := fmt.Sprintf("Paragraph %d ", i) + strings.Repeat("가", 7900)
		texts = append(texts, text)
		ops = append(ops, map[string]any{"op": "create_block", "ref": fmt.Sprintf("block%d", i), "place": place(page), "content": map[string]any{"kind": "paragraph", "text": []any{map[string]any{"text": text, "marks": []any{}}}}})
	}
	written, err := e.svc.UpdateNotebook(t.Context(), slug, integratedNotebookJSON(t, map[string]any{"op_id": "historical-fixture", "ops": ops}), core.ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	var result notebook.WriteResult
	if err := json.Unmarshal(written.Result, &result); err != nil || written.Pending || written.Revision == 0 || !result.Mapping.Complete {
		t.Fatalf("fixture write: %+v %v", written, err)
	}
	f := notebookHistoryFixture{e: e, slug: slug, page: page, refs: result.Refs, revision: written.Revision}
	for i, text := range texts {
		id := f.refs[fmt.Sprintf("block%d", i)].Ref.ID
		f.blocks = append(f.blocks, id)
		f.markdown += "<!-- flats:block " + string(id) + " -->\n" + text + "\n\n"
	}
	currentOps := []any{
		map[string]any{"op": "set_page_title", "page": page, "title": "Current page"},
		map[string]any{"op": "set_page_title", "page": f.refs["child"].Ref, "title": "Current child"},
		map[string]any{"op": "set_database_title", "database": f.refs["db"].Ref, "title": "Current database"},
		map[string]any{"op": "set_property", "property": f.refs["status"].Ref, "definition": map[string]any{"name": "Current status", "type": "select"}},
		map[string]any{"op": "set_option", "option": f.refs["open"].Ref, "name": "Current open", "color": "green"},
		map[string]any{"op": "set_view", "view": f.refs["db.view"].Ref, "definition": map[string]any{"name": "Current view", "display": map[string]any{"layout": "table", "visible": []any{f.refs["db.title"].Ref}, "widths": []any{}}, "sort": []any{}, "filter": []any{}}},
		map[string]any{"op": "set_cell", "row": f.refs["row0"].Ref, "cell": map[string]any{"property": f.refs["db.title"].Ref, "value": "Current row"}},
		map[string]any{"op": "set_block", "block": f.refs["block0"].Ref, "content": map[string]any{"kind": "paragraph", "text": []any{map[string]any{"text": "Current paragraph", "marks": []any{}}}}},
	}
	changed, err := e.svc.UpdateNotebook(t.Context(), slug, integratedNotebookJSON(t, map[string]any{"op_id": "current-fixture", "ops": currentOps}), core.ViaMCP)
	if err != nil || changed.Pending || changed.Revision <= f.revision {
		t.Fatalf("current write: %+v %v", changed, err)
	}
	return f
}

func readNotebookHistoryHTTP(t *testing.T, f notebookHistoryFixture, query url.Values) notebookHistoryRead {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), "GET", f.e.management.URL+"/api/flats/"+f.slug+"/notebook?"+query.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.e.management.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %v = %d %s", query, resp.StatusCode, raw)
	}
	var out notebookHistoryRead
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func collectNotebookHistory(t *testing.T, first notebookHistoryRead, read func(string) notebookHistoryRead) []notebookHistoryRead {
	t.Helper()
	pages := []notebookHistoryRead{first}
	seen := map[string]bool{}
	for !pages[len(pages)-1].Complete {
		previous := pages[len(pages)-1]
		if previous.Next == nil || *previous.Next == "" || seen[*previous.Next] || len(pages) > 64 {
			t.Fatal("missing or looping historical continuation")
		}
		cursor := *previous.Next
		seen[cursor] = true
		next := read(cursor)
		if len(pages) == 1 && !reflect.DeepEqual(next, read(cursor)) {
			t.Fatal("cursor replay changed its page")
		}
		pages = append(pages, next)
	}
	return pages
}

type notebookHistoryTransport struct {
	base    http.RoundTripper
	queries []url.Values
}

func (tr *notebookHistoryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	tr.queries = append(tr.queries, r.URL.Query())
	return tr.base.RoundTrip(r)
}

func TestHistoryAdapterTraversal(t *testing.T) {
	for _, adapter := range []string{"API", "CLI", "MCP"} {
		t.Run(adapter, func(t *testing.T) {
			f := newNotebookHistoryFixture(t)
			for _, scope := range []string{"tree", "page", "database", "markdown"} {
				t.Run(scope, func(t *testing.T) {
					query := url.Values{"revision": {strconv.Itoa(f.revision)}, "limit": {"1"}}
					switch scope {
					case "page":
						query.Set("page", string(f.refs["child"].Ref.ID))
					case "database":
						query.Set("database", string(f.refs["db"].Ref.ID))
					case "markdown":
						query.Set("page", string(f.page.ID))
						query.Set("format", "markdown")
					}
					var pages []notebookHistoryRead
					switch adapter {
					case "API":
						pages = collectNotebookHistory(t, readNotebookHistoryHTTP(t, f, query), func(cursor string) notebookHistoryRead {
							return readNotebookHistoryHTTP(t, f, url.Values{"cursor": {cursor}})
						})
					case "MCP":
						args := map[string]any{"slug": f.slug, "revision": f.revision, "limit": 1}
						for _, key := range []string{"page", "database", "format"} {
							if query.Has(key) {
								args[key] = query.Get(key)
							}
						}
						read := func(args map[string]any) notebookHistoryRead {
							var out notebookHistoryRead
							if text, bad := call(t, f.e.local, "get_notebook", args, &out); bad {
								t.Fatalf("get_notebook: %s", text)
							}
							return out
						}
						pages = collectNotebookHistory(t, read(args), func(cursor string) notebookHistoryRead { return read(map[string]any{"slug": f.slug, "cursor": cursor}) })
					case "CLI":
						args := []string{"--url", f.e.management.URL, "--json", "notebook", "get", f.slug, "--revision", strconv.Itoa(f.revision), "--limit", "1"}
						for _, key := range []string{"page", "database", "format"} {
							if query.Has(key) {
								args = append(args, "--"+key, query.Get(key))
							}
						}
						var out, errw bytes.Buffer
						tr := &notebookHistoryTransport{base: f.e.management.Client().Transport}
						code := cli.Run(t.Context(), args, cli.Env{Stdout: &out, Stderr: &errw, ConfigDir: t.TempDir(), HTTP: &http.Client{Transport: tr}})
						if code != 0 {
							t.Fatalf("CLI exit %d after %d requests (%d output bytes): %s", code, len(tr.queries), out.Len(), errw.String())
						}
						if err := json.Unmarshal(out.Bytes(), &pages); err != nil {
							t.Fatalf("CLI JSON: %v", err)
						}
						if len(tr.queries) != len(pages) {
							t.Fatalf("CLI made %d calls for %d pages", len(tr.queries), len(pages))
						}
						if tr.queries[0].Encode() != query.Encode() {
							t.Fatalf("CLI initial query = %v, want %v", tr.queries[0], query)
						}
						for i, q := range tr.queries[1:] {
							if len(q) != 1 || !q.Has("cursor") || pages[i].Next == nil || q.Get("cursor") != *pages[i].Next {
								t.Fatalf("CLI continuation retained selectors or skipped a cursor: %v", q)
							}
						}
					}
					assertNotebookHistoryTraversal(t, f, scope, pages)
				})
			}
		})
	}
}

func assertNotebookHistoryTraversal(t *testing.T, f notebookHistoryFixture, scope string, pages []notebookHistoryRead) {
	t.Helper()
	if len(pages) < 2 {
		t.Fatalf("historical %s did not paginate", scope)
	}
	items := map[notebook.ID]notebookHistoryItem{}
	rows := []notebookHistoryItem{}
	var markdown strings.Builder
	references := []notebook.ID{}
	for i, page := range pages {
		if page.Revision == nil || *page.Revision != f.revision || page.Pending || page.State != pages[0].State || page.State.Store == "" {
			t.Fatalf("page %d lost historical state/revision: %+v", i, page)
		}
		last := i == len(pages)-1
		if page.Complete != last || (page.Next == nil) != last {
			t.Fatalf("page %d completion = %t, next=%v", i, page.Complete, page.Next)
		}
		var part []notebookHistoryItem
		switch scope {
		case "tree":
			part = page.Tree
		case "page":
			if page.Page == nil || len(page.Rows) != 0 {
				t.Fatal("page traversal lost its scope or included database rows")
			}
			part = page.Page.Items
		case "database":
			part = append(slices.Clone(page.Schema), page.Rows...)
			rows = append(rows, page.Rows...)
		case "markdown":
			if page.Offset != markdown.Len() || page.TotalBytes != len(f.markdown) || !utf8.ValidString(page.Markdown) {
				t.Fatalf("Markdown chunk %d has invalid byte offsets/UTF-8", i)
			}
			markdown.WriteString(page.Markdown)
			for _, ref := range page.References {
				if ref.Ref.Inc == 0 {
					t.Fatal("Markdown reference lacks incarnation")
				}
				references = append(references, ref.Ref.ID)
			}
		}
		for _, item := range part {
			if item.Ref.ID == "" || item.Ref.Inc == 0 {
				t.Fatal("historical item lacks a mutable reference")
			}
			if _, exists := items[item.Ref.ID]; exists {
				t.Fatalf("duplicate historical item %s", item.Ref.ID)
			}
			items[item.Ref.ID] = item
		}
	}
	want := []notebook.ID{}
	switch scope {
	case "tree":
		want = []notebook.ID{f.page.ID, f.refs["child"].Ref.ID, f.refs["grandchild"].Ref.ID, f.refs["peer"].Ref.ID}
		if items[f.page.ID].Title != "Earlier page" || items[f.refs["child"].Ref.ID].Title != "Earlier child" {
			t.Fatal("historical tree contains current titles")
		}
		for _, pair := range [][2]notebook.ID{{f.refs["child"].Ref.ID, f.page.ID}, {f.refs["grandchild"].Ref.ID, f.refs["child"].Ref.ID}} {
			if items[pair[0]].Parent == nil || *items[pair[0]].Parent != pair[1] {
				t.Fatal("historical tree lost nested page ownership")
			}
		}
	case "page", "database":
		for _, key := range []string{"db", "db.title", "db.view", "status", "open"} {
			want = append(want, f.refs[key].Ref.ID)
		}
		db := items[f.refs["db"].Ref.ID]
		if db.Title != "Earlier database" || !slices.Equal(db.PropertyOrder, []notebook.ID{f.refs["db.title"].Ref.ID, f.refs["status"].Ref.ID}) || !slices.Equal(db.ViewOrder, []notebook.ID{f.refs["db.view"].Ref.ID}) {
			t.Fatal("historical database schema/order was replaced by current state")
		}
		if item := items[f.refs["db.title"].Ref.ID]; item.Definition.Name != "Task" || item.Definition.Type != "title" {
			t.Fatal("historical title property missing")
		}
		if item := items[f.refs["status"].Ref.ID]; item.Definition.Name != "Earlier status" || item.Definition.Type != "select" {
			t.Fatal("historical property definition missing")
		}
		if item := items[f.refs["open"].Ref.ID]; item.Name != "Earlier open" || item.Color != "blue" {
			t.Fatal("historical option missing")
		}
		if item := items[f.refs["db.view"].Ref.ID]; item.Kind != "view" || item.Definition.Name != "Earlier view" || item.Definition.Display.Layout != "table" {
			t.Fatal("historical saved view missing or replaced by current view")
		}
		if scope == "page" {
			want = append(want, f.refs["child"].Ref.ID, f.refs["owner"].Ref.ID)
		} else {
			if len(rows) != 3 {
				t.Fatalf("database returned %d rows, want 3", len(rows))
			}
			for i, row := range rows {
				id := f.refs[fmt.Sprintf("row%d", i)].Ref.ID
				want = append(want, id)
				if row.Ref.ID != id || row.Database != f.refs["db"].Ref.ID || row.Cells[f.refs["db.title"].Ref.ID].Value != fmt.Sprintf("Earlier row %d", i) || row.Cells[f.refs["status"].Ref.ID].Value != string(f.refs["open"].Ref.ID) {
					t.Fatalf("historical row %d changed contents/order: %+v", i, row)
				}
			}
		}
	case "markdown":
		if markdown.String() != f.markdown {
			t.Fatalf("historical Markdown differs: got %d bytes, want %d", markdown.Len(), len(f.markdown))
		}
		wantRefs := append([]notebook.ID{f.page.ID}, f.blocks...)
		slices.Sort(wantRefs)
		slices.Sort(references)
		if !slices.Equal(references, wantRefs) {
			t.Fatalf("Markdown references = %v, want %v", references, wantRefs)
		}
		return
	}
	got := []notebook.ID{}
	for id := range items {
		got = append(got, id)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("historical %s item census = %v, want %v", scope, got, want)
	}
}
