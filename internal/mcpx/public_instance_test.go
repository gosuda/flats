package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func publicInstanceOn(t *testing.T, e *env) {
	t.Helper()
	if _, err := e.svc.SetPermittedNetworks([]string{"portal", "zrok"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdateSettings(t.Context(), map[string]string{core.SetDefaultPublic: "portal"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetPublicInstance(t.Context(), true, "portal", "test"); err != nil {
		t.Fatal(err)
	}
}

func TestPublicInstanceServers(t *testing.T) {
	for _, ap := range []bool{false, true} {
		t.Run(fmt.Sprintf("autopilot=%v", ap), func(t *testing.T) {
			e := newEnv(t)
			if ap {
				setAutopilot(t, e, "")
			}
			off := connect(t, e.localURL)
			offTools, err := off.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			publicInstanceOn(t, e)
			cs := connect(t, e.localURL)
			ins := cs.InitializeResult().Instructions
			if ins != runtimeref.ServerInstructions(e.svc.UploadLimit(), ap, true) || len(ins) > 2300 {
				t.Fatalf("instructions: %s", ins)
			}
			onTools, err := cs.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(offTools.Tools) != len(onTools.Tools) {
				t.Fatal("mode added tools")
			}
			offByName := map[string]*mcp.Tool{}
			for _, tool := range offTools.Tools {
				offByName[tool.Name] = tool
			}
			for _, tool := range onTools.Tools {
				before := offByName[tool.Name]
				if tool.Description != publicInstanceDescription(tool.Name, before.Description) {
					t.Errorf("%s description overlay", tool.Name)
				}
				if !reflect.DeepEqual(tool.InputSchema, before.InputSchema) {
					t.Errorf("%s input schema changed", tool.Name)
				}
				if tool.Name != "check_client" && !reflect.DeepEqual(tool.OutputSchema, before.OutputSchema) {
					t.Errorf("%s output schema changed", tool.Name)
				}
			}
			for _, r := range publicToolReplacements {
				if n := strings.Count(offByName[r.tool].Description, r.old); n != 1 {
					t.Errorf("%s anchor %q occurs %d times", r.tool, r.old, n)
				}
			}
			var check map[string]any
			text, failed := call(t, cs, "check_client", map[string]any{}, &check)
			pi, ok := check["public_instance"].(map[string]any)
			if failed || !ok || pi["enabled"] != true || pi["network"] != "portal" || pi["read"] != "guide topic.public-instance" {
				t.Fatalf("check_client: %s", text)
			}
			_, hasAP := check["autopilot"]
			if hasAP != ap {
				t.Fatal("Autopilot member")
			}
			var index GuideOut
			call(t, cs, "guide", map[string]any{}, &index)
			if !slices.Contains(index.Available, "topic.public-instance") || !slices.Contains(index.Available, "refusal.public_instance_on") {
				t.Fatalf("missing guide items: %v", index.Available)
			}
			for _, id := range index.Available {
				var out GuideOut
				text, failed := call(t, cs, "guide", map[string]any{"items": []string{id}}, &out)
				if failed || len(out.Items) != 1 || out.GuideRevision != index.GuideRevision {
					t.Fatalf("%s: %s", id, text)
				}
				if id == "topic.public-instance" && (!strings.Contains(text, "Portal (`portal`)") || strings.Contains(text, "{{network}}")) {
					t.Fatal(text)
				}
				if id == "topic.instructions" && !strings.Contains(text, ins) {
					t.Fatal("guide instructions differ")
				}
			}
			var page GuideOut
			call(t, cs, "guide", map[string]any{"items": []string{"topic.public-instance"}}, &page)
			if _, err := e.svc.UpdateSettings(t.Context(), map[string]string{core.SetDefaultPublic: "zrok"}); err != nil {
				t.Fatal(err)
			}
			var changed GuideOut
			call(t, cs, "guide", map[string]any{"items": []string{"topic.public-instance"}}, &changed)
			if changed.GuideRevision == page.GuideRevision || !strings.Contains(changed.Items[0].Markdown, "zrok (`zrok`)") {
				t.Fatalf("request network not refreshed: %+v", changed)
			}
			if text, failed := call(t, cs, "open_preview", map[string]any{"slug": "not-even-created", "version": 0}, nil); !failed || !strings.Contains(text, `"category":"public_instance_on"`) {
				t.Fatal(text)
			}
			if _, err := e.svc.SetPublicInstance(t.Context(), false, "", "test"); err != nil {
				t.Fatal(err)
			}
			var offPage GuideOut
			call(t, cs, "guide", map[string]any{"items": []string{"topic.public-instance", "refusal.public_instance_on"}}, &offPage)
			if len(offPage.Unknown) != 2 || strings.Contains(strings.Join(offPage.Available, ","), "public-instance") || strings.Contains(strings.Join(offPage.Available, ","), "public_instance_on") {
				t.Fatalf("off guide: %+v", offPage)
			}
			check = nil
			call(t, cs, "check_client", map[string]any{}, &check)
			if _, ok := check["public_instance"]; ok {
				t.Fatal("off member leaked")
			}
		})
	}
}

func TestPublicInstanceResults(t *testing.T) {
	e := newEnvRuntime(t)
	publicInstanceOn(t, e)
	cs := connect(t, e.localURL)
	for _, tc := range []struct {
		name     string
		args     map[string]any
		document bool
	}{
		{"create_flat", map[string]any{"slug": "new-doc", "type": "docs"}, true},
		{"save_document", map[string]any{"slug": "saved-doc", "markdown": "# Hello"}, true},
		{"create_flat", map[string]any{"slug": "new-site"}, false},
		{"create_flat", map[string]any{"slug": "new-page", "type": "page"}, false},
		{"create_flat", map[string]any{"slug": "new-design", "type": "design"}, true},
		{"create_flat", map[string]any{"slug": "new-board", "type": "board"}, true},
	} {
		t.Run(tc.args["slug"].(string), func(t *testing.T) {
			text, failed := call(t, cs, tc.name, tc.args, nil)
			if failed {
				t.Fatal(text)
			}
			f, err := e.svc.GetFlat(t.Context(), tc.args["slug"].(string))
			if err != nil {
				t.Fatal(err)
			}
			summary := strings.Split(text, "\n{")[0]
			if strings.Contains(summary, "Private URL:") || strings.Contains(summary, "open_preview with") || strings.Contains(summary, "unpublished Private") {
				t.Fatal(summary)
			}
			if tc.document {
				if f.PublicURL == "" || !strings.Contains(summary, f.PublicURL) || f.PublicNotice == "" || !strings.Contains(summary, f.PublicNotice) {
					t.Fatalf("missing public link/notice: %s (%+v)", summary, f)
				}
			} else if !strings.Contains(summary, "the public link is not open yet (unavailable)") {
				t.Fatal(summary)
			}
			if !f.PrivateLinksOff || f.PrivateURL != "" {
				t.Fatalf("private view: %+v", f)
			}
		})
	}
	for _, name := range []string{"list_flats", "get_flat"} {
		args := map[string]any{}
		if name == "get_flat" {
			args["slug"] = "saved-doc"
		}
		text, failed := call(t, cs, name, args, nil)
		if failed || strings.Contains(text, "Private URL:") {
			t.Fatal(text)
		}
	}
	text, failed := call(t, cs, "save_draft", map[string]any{"slug": "save-site", "files": []any{file("index.html", "hello", "")}}, nil)
	if failed || strings.Contains(text, "Next: open_preview") {
		t.Fatal(text)
	}
}

func TestPublicInstanceLinkText(t *testing.T) {
	for _, tc := range []struct {
		name string
		fi   FlatInfo
		want string
	}{
		{"off", FlatInfo{PrivateURL: "private", PublicURL: "public", PublicNotice: "notice"}, "private"},
		{"on ready", FlatInfo{privateLinksOff: true, PublicURL: "public", PublicNotice: "notice"}, "public\nNotice: notice"},
		{"on no route", FlatInfo{privateLinksOff: true, ConnectionState: "unavailable"}, "the public link is not open yet (unavailable)"},
		{"on failure with URL", FlatInfo{privateLinksOff: true, PublicURL: "https://pending.example", ConnectionState: "starting", PublicNotice: "notice", Endpoints: []core.ExposureEndpoint{{Provider: core.ProviderPortal, State: "starting", Detail: "waiting for relay"}}}, "https://pending.example; the public link is not open yet (starting: waiting for relay)\nNotice: notice"},
		{"on failure", FlatInfo{privateLinksOff: true, ConnectionState: "starting", Endpoints: []core.ExposureEndpoint{{Provider: core.ProviderPortal, State: "starting", Detail: "waiting for relay"}}}, "the public link is not open yet (starting: waiting for relay)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := linkText(tc.fi); got != tc.want {
				t.Fatalf("%q, want %q", got, tc.want)
			}
		})
	}
	fi := FlatInfo{privateLinksOff: true, Slug: "hidden", Visibility: "private", ConnectionState: "unavailable"}
	if text := flatText(fi); !strings.Contains(text, "the public link is not open yet") || strings.Contains(text, "Private URL:") {
		t.Fatal(text)
	}
	d := deployInfo(core.DeployResult{Version: 2, Flat: core.FlatView{PrivateLinksOff: true, PublicURL: "https://public.example", PublicNotice: "notice", ConnectionState: "ready"}})
	if text := deployText("site", d, "Deployed"); !strings.Contains(text, "https://public.example") || !strings.Contains(text, "notice") || strings.Contains(text, "private URL") {
		t.Fatal(text)
	}
}

func TestPublicInstanceServerCache(t *testing.T) {
	e := newEnv(t)
	c := &serverCache{svc: e.svc, version: "test"}
	old := map[serverModes]*mcp.Server{}
	for _, ap := range []bool{false, true} {
		for _, pi := range []bool{false, true} {
			key := serverModes{ap, pi}
			old[key] = c.get(ap, pi)
			if c.get(ap, pi) != old[key] {
				t.Fatal("server was not cached")
			}
		}
	}
	if len(c.servers) != 4 {
		t.Fatalf("%d servers", len(c.servers))
	}
	if _, err := e.svc.UpdateSettings(t.Context(), map[string]string{core.SetUploadMaxBytes: "8192"}); err != nil {
		t.Fatal(err)
	}
	for key, before := range old {
		if c.get(key.autopilot, key.public) == before {
			t.Fatalf("stale server: %+v", key)
		}
	}
}

func TestPublicInstanceRequestSnapshot(t *testing.T) {
	e := newEnv(t)
	publicInstanceOn(t, e)
	req := httptest.NewRequest("GET", "http://localhost/mcp", nil)
	ctx := withState(req, e.svc)
	if _, err := e.svc.SetPublicInstance(t.Context(), false, "", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdateSettings(t.Context(), map[string]string{core.SetDefaultPublic: "zrok"}); err != nil {
		t.Fatal(err)
	}
	st := stateOf(ctx)
	if st.Public == nil || st.Public.Network != "portal" {
		t.Fatal("snapshot changed")
	}
	m := clientManifest(ctx, e.svc)
	want := runtimeref.EffectiveGuideRevision(e.svc.UploadLimit(), nil, st.Public)
	if m.GuideRevision != want {
		t.Fatal("manifest ignored snapshot")
	}
	out := guide([]string{"topic.public-instance"}, e.svc.UploadLimit(), nil, st.Public)
	b, _ := json.Marshal(out)
	if out.GuideRevision != want || !strings.Contains(string(b), "Portal (`portal`)") {
		t.Fatal(string(b))
	}
	if stateOf(context.Background()).Public != nil {
		t.Fatal("default state is on")
	}
}
