package mcpx

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
)

const testPage = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Sales Report</title><link rel="icon" href="icon.svg"></head><body><canvas id="c"></canvas><script src="chart.js"></script></body></html>`

func TestCreateAndSavePage(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var created FlatInfo
	text, failed := call(t, e.local, "create_flat", map[string]any{"slug": "report", "name": "Sales Report", "type": "page"}, &created)
	if failed || created.Type != "page" || created.LiveVersion != 0 || created.Publication != "unpublished" || created.Draft == nil || created.Draft.Revision != 1 || !strings.Contains(text, "starter Draft (revision 1)") || !strings.Contains(text, "save_page") {
		t.Fatalf("create page: %s %+v", text, created)
	}

	// A page with assets and a screenshot, requesting publish approval.
	png := base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G'})
	var saved saveFixtureOut
	text, failed = call(t, e.local, "save_page", map[string]any{"slug": "report", "html": testPage, "title": "Sales Report", "screenshot": "shot.png", "expected_revision": 1, "message": "chart",
		"files": []any{file("chart.js", "new Chart()", ""), file("icon.svg", "<svg/>", ""), file("shot.png", png, "base64")}, "deploy": true}, &saved)
	if failed || saved.Draft.Revision != 2 || saved.Draft.Type != "page" || saved.Draft.Kind != "static" || saved.Flat != nil || saved.Deploy == nil || saved.Deploy.Status != "pending_approval" {
		t.Fatalf("save page: %s %+v", text, saved)
	}
	if len(saved.Warnings) != 0 {
		t.Fatalf("clean page warned: %+v", saved.Warnings)
	}
	f, _ := e.svc.GetFlat(ctx, "report")
	if f.LiveVersion != 0 {
		t.Fatalf("saving published the page: %+v", f.Flat)
	}
	e.approve(t, saved.Deploy.ApprovalID)
	f, _ = e.svc.GetFlat(ctx, "report")
	if f.Type != "page" || f.LiveVersion != 0 || f.Versions != 1 {
		t.Fatalf("saved page version: type %q live %d versions %d", f.Type, f.LiveVersion, f.Versions)
	}
	var dep DeployInfo
	if text, failed := call(t, e.local, "deploy", map[string]any{"slug": "report", "version": 1}, &dep); failed || dep.Status != "pending_approval" {
		t.Fatalf("deploy page pending: %s %+v", text, dep)
	}
	e.approve(t, dep.ApprovalID)
	f, _ = e.svc.GetFlat(ctx, "report")
	if f.Type != "page" || f.LiveVersion != 1 || f.Thumbnail == "" {
		t.Fatalf("published page: type %q live %d thumbnail %q", f.Type, f.LiveVersion, f.Thumbnail)
	}
	if code, body := get(t, f.PrivateURL); code != 200 || string(body) != testPage {
		t.Fatalf("served page: %d %q", code, body)
	}
	if _, body := get(t, f.PrivateURL+"/chart.js"); string(body) != "new Chart()" {
		t.Fatalf("served asset: %q", body)
	}

	// A self-contained page needs no files; omitted and empty both work,
	// and each save replaces the whole Draft.
	for i, args := range []map[string]any{{"slug": "report", "html": "<title>Two</title>"}, {"slug": "report", "html": "<title>Three</title>", "files": []any{}}} {
		var out saveFixtureOut
		if text, failed := call(t, e.local, "save_page", args, &out); failed || out.Draft.Revision != 3+i || out.Draft.Files != 2 {
			t.Fatalf("self-contained save %d: %s %+v", i, text, out)
		}
	}
	// External render-time data is a warning, not a refusal.
	var warned saveFixtureOut
	if text, failed := call(t, e.local, "save_page", map[string]any{"slug": "report", "html": `<title>Live</title><script>fetch("https://api.example.com/sales.json")</script>`}, &warned); failed || !strings.Contains(text, "loads data from https://api.example.com") {
		t.Fatalf("data warning: %s %+v", text, warned)
	}
	if _, body := get(t, f.PrivateURL); string(body) != testPage {
		t.Fatalf("Draft saves changed the live page: %q", body)
	}
}

func TestSavePageRefusals(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"empty html", map[string]any{"slug": "pg1", "html": ""}, "html is empty"},
		{"html asset", map[string]any{"slug": "pg1", "html": "<title>P</title>", "files": []any{file("more.html", "<p>x</p>", "")}}, "one HTML document"},
		{"manifest asset", map[string]any{"slug": "pg1", "html": "<title>P</title>", "files": []any{file("flats.json", "{}", "")}}, "writes flats.json itself"},
		{"missing screenshot", map[string]any{"slug": "pg1", "html": "<title>P</title>", "screenshot": "shot.png"}, "screenshot file is missing"},
		{"bad encoding", map[string]any{"slug": "pg1", "html": "<title>P</title>", "files": []any{file("a.png", "x", "hex")}}, "unknown encoding"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, failed := call(t, e.local, "save_page", tt.args, nil)
			if !failed || !strings.Contains(text, tt.want) || !strings.Contains(text, `"category":"invalid"`) {
				t.Fatalf("got %v %s", failed, text)
			}
			if _, err := e.svc.GetFlat(context.Background(), "pg1"); err == nil {
				t.Fatal("refused save created the flat")
			}
		})
	}
	// expected_revision conflicts never overwrite.
	call(t, e.local, "save_page", map[string]any{"slug": "pg1", "html": "<title>One</title>"}, nil)
	if text, failed := call(t, e.local, "save_page", map[string]any{"slug": "pg1", "html": "<title>Two</title>", "expected_revision": 0}, nil); !failed || !strings.Contains(text, `"category":"conflict"`) {
		t.Fatalf("revision conflict: %s", text)
	}
	// A generic save without the page manifest is refused with the fix.
	text, failed := call(t, e.local, "save_draft", map[string]any{"slug": "pg1", "files": []any{file("index.html", "<title>Site</title>", "")}}, nil)
	if !failed || !strings.Contains(text, `"category":"conflict"`) || !strings.Contains(text, "is a page and this save is a website") {
		t.Fatalf("website into page: %s", text)
	}
	if text, failed := call(t, e.local, "save_draft", map[string]any{"slug": "pg1", "files": []any{file("index.html", "<title>Page</title>", ""), file("flats.json", `{"type":"page"}`, "")}}, nil); failed {
		t.Fatalf("page manifest save: %s", text)
	}
	if text, failed := call(t, e.local, "create_flat", map[string]any{"slug": "q", "type": "slides"}, nil); !failed || !strings.Contains(text, "flat, docs, page, design or board") {
		t.Fatalf("unknown type: %s", text)
	}
}
