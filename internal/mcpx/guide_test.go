package mcpx

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/core"
)

func TestGuideTool(t *testing.T) {
	e := newEnv(t)
	tools, err := e.local.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range tools.Tools {
		if tool.Name == "guide" {
			found = true
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
				t.Fatal("guide must be read-only and idempotent")
			}
		}
	}
	if !found {
		t.Fatal("tools/list does not show guide")
	}

	var out GuideOut
	text, failed := call(t, e.remote, "guide", map[string]any{"items": []string{"topic.index"}}, &out)
	if failed || len(out.Items) != 1 || out.Items[0].ID != "topic.index" || !strings.Contains(text, "### Route by intent") {
		t.Fatalf("topic.index: %v %s", failed, text)
	}
	if !slices.Equal(out.Available, guideItems()) || !strings.Contains(text, "refusal.conflict") {
		t.Fatal("topic.index must list every item")
	}
	// No items reads the index too.
	if _, failed := call(t, e.local, "guide", map[string]any{}, &out); failed || out.Items[0].ID != "topic.index" {
		t.Fatal("empty guide call must return topic.index")
	}

	// Several items per call; bare names, duplicates and unknown items.
	text, failed = call(t, e.local, "guide", map[string]any{"items": []string{"server", "topic.server.db", "refusal.conflict", "topic.server", "nope"}}, &out)
	var ids []string
	for _, it := range out.Items {
		ids = append(ids, it.ID)
	}
	if failed || !slices.Equal(ids, []string{"topic.server", "topic.server.db", "refusal.conflict"}) {
		t.Fatalf("items = %v (failed %v)", ids, failed)
	}
	if !slices.Equal(out.Unknown, []string{"nope"}) || !slices.Equal(out.Available, guideItems()) ||
		!strings.Contains(text, "Unknown guide items: nope.") || !strings.Contains(text, "topic.preview-verify") {
		t.Fatalf("unknown items must return the valid list: %s", text)
	}
	if !strings.Contains(text, "# topic.server.db\n\n## JavaScript env.DB") || !strings.Contains(text, runtimeref.RefusalIntro) {
		t.Fatal("guide text must carry each page under its id, with the refusal intro")
	}

	// topic.instructions is the fallback copy of the initialization text.
	call(t, e.local, "guide", map[string]any{"items": []string{"topic.instructions"}}, &out)
	if !strings.Contains(out.Items[0].Markdown, e.local.InitializeResult().Instructions) {
		t.Fatal("topic.instructions differs from the initialization instructions")
	}
	flats, err := e.svc.ListFlats(context.Background())
	if err != nil || len(flats) != 0 {
		t.Fatalf("reading the guide changed flat state: %v, %v", flats, err)
	}
}

func TestRefusalPagesCoverEveryCategory(t *testing.T) {
	want := append(slices.Clone(core.ErrorCategories), internalCategory)
	got := runtimeref.RefusalCategories()
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("refusal pages = %v, want one per category %v", got, want)
	}
}

func TestToolErrorsNameCategoryAndRefusalPage(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name     string
		tool     string
		args     map[string]any
		remote   bool
		category string
	}{
		{"missing flat", "get_flat", map[string]any{"slug": "nothing-here"}, false, "not_found"},
		{"negative version", "deploy", map[string]any{"slug": "x-flat", "version": -1}, false, "invalid"},
		{"bad upload", "save_draft", map[string]any{"slug": "x-flat", "files": []any{file("../a", "x", "")}}, false, "invalid"},
		{"empty upload", "save_draft", map[string]any{"slug": "x-flat", "files": []any{}}, false, "invalid"},
		{"remote dir upload", "save_version_from_dir", map[string]any{"slug": "x-flat", "dir": "/tmp"}, true, "forbidden"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cs := e.local
			if c.remote {
				cs = e.remote
			}
			text, failed := call(t, cs, c.tool, c.args, nil)
			if !failed || !strings.Contains(text, `"category":"`+c.category+`"`) || !strings.Contains(text, "See guide refusal."+c.category+".") {
				t.Fatalf("error must name category %s and its refusal page:\n%s", c.category, text)
			}
		})
	}
	if got := errorCategory(context.Canceled); got != internalCategory {
		t.Fatalf("unclassified error category = %q", got)
	}
}

// Instructions are always loaded, so they stay small; details live in topics.
func TestInstructionsStaySmall(t *testing.T) {
	if n := len(instructions(20 << 20)); n > 2300 {
		t.Fatalf("instructions are %d bytes; move details into guide topics", n)
	}
	ins := instructions(20 << 20)
	for _, s := range []string{`guide {"items":["topic.index"]}`, "approval_url", "get_approval", "Never approve your own request",
		"NOT access control", "Never infer state from a URL", "Ask the user before set_visibility, delete_flat or rollback with restore_data",
		"BOTH directions", "refusal.<category>", "topic.instructions"} {
		if !strings.Contains(ins, s) {
			t.Errorf("instructions lost the invariant %q", s)
		}
	}
}

// Phrases from retired behavior must not come back in anything an agent reads.
func TestAgentTextsAvoidRetiredPhrases(t *testing.T) {
	retired := []string{
		"public-listed", "public-unlisted", "listed/unlisted", "returning private is immediate",
		"narrowing exposure", "files is not restored", "snapshots do not include files", "database only",
	}
	e := newEnv(t)
	texts := map[string]string{"instructions": instructions(20 << 20)}
	tools, err := e.local.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		schema, _ := json.Marshal(tool.InputSchema)
		out, _ := json.Marshal(tool.OutputSchema)
		texts["tool "+tool.Name] = tool.Description + string(schema) + string(out)
	}
	for _, id := range guideItems() {
		md, _ := guidePage(id, 20<<20)
		texts[id] = md
	}
	texts["runtime reference"] = runtimeref.Markdown
	texts["content types"] = runtimeref.ContentTypesMarkdown
	srv := httptest.NewServer(LLMsHandler(e.svc, Options{Version: "test"}))
	t.Cleanup(srv.Close)
	_, _, texts["llms-full.txt"] = fetch(t, srv.Client(), "GET", srv.URL+LLMsFullPath)
	skill, err := os.ReadFile("../../plugins/flats/skills/flats-deploy/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	texts["skill"] = string(skill)
	for name, text := range texts {
		lower := strings.ToLower(strings.Join(strings.Fields(text), " "))
		for _, phrase := range retired {
			if strings.Contains(lower, phrase) {
				t.Errorf("%s contains retired phrase %q", name, phrase)
			}
		}
	}
}
