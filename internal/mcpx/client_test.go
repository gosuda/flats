package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gosuda/flats/internal/clientstate"
)

func TestClientModeCompatibility(t *testing.T) {
	for _, ap := range []bool{false, true} {
		for _, public := range []bool{false, true} {
			t.Run(fmt.Sprintf("autopilot=%v/public=%v", ap, public), func(t *testing.T) {
				e := newEnv(t)
				if ap {
					setAutopilot(t, e, "")
				}
				if public {
					publicInstanceOn(t, e)
				}
				cs := connect(t, e.localURL)
				var manifest clientstate.Manifest
				if text, failed := call(t, cs, "get_client_skill", map[string]any{}, &manifest); failed {
					t.Fatal(text)
				}
				base, err := clientstate.Check(manifest, clientstate.CheckInput{})
				if err != nil {
					t.Fatal(err)
				}
				// These value-field shapes are the four pre-refactor wire contracts.
				apInfo := autopilotInfo{Enabled: true, Allowed: []string{}, Read: "guide topic.autopilot"}
				piInfo := publicInstanceInfo{Enabled: true, Network: "portal", Read: "guide topic.public-instance"}
				var want any = base
				switch {
				case ap && public:
					want = struct {
						clientstate.CheckResult
						Autopilot      autopilotInfo      `json:"autopilot"`
						PublicInstance publicInstanceInfo `json:"public_instance"`
					}{CheckResult: base, Autopilot: apInfo, PublicInstance: piInfo}
				case ap:
					want = struct {
						clientstate.CheckResult
						Autopilot autopilotInfo `json:"autopilot"`
					}{CheckResult: base, Autopilot: apInfo}
				case public:
					want = struct {
						clientstate.CheckResult
						PublicInstance publicInstanceInfo `json:"public_instance"`
					}{CheckResult: base, PublicInstance: piInfo}
				}
				wantJSON, err := json.Marshal(want)
				if err != nil {
					t.Fatal(err)
				}
				res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "check_client", Arguments: map[string]any{}})
				if err != nil {
					t.Fatal(err)
				}
				if res.IsError || len(res.Content) != 1 {
					t.Fatalf("check_client: %+v", res)
				}
				text, ok := res.Content[0].(*mcp.TextContent)
				if !ok || text.Text != string(wantJSON) {
					t.Fatalf("check_client text = %+v, want %s", res.Content, wantJSON)
				}
				var wantStructured any
				if err := json.Unmarshal(wantJSON, &wantStructured); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(res.StructuredContent, wantStructured) {
					t.Fatalf("structured result = %+v, want %+v", res.StructuredContent, wantStructured)
				}
				wantSchema, err := jsonschema.ForType(reflect.TypeOf(want), nil)
				if err != nil {
					t.Fatal(err)
				}
				wantSchemaJSON, err := json.Marshal(wantSchema)
				if err != nil {
					t.Fatal(err)
				}
				tools, err := cs.ListTools(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				for _, tool := range tools.Tools {
					if tool.Name != "check_client" {
						continue
					}
					got, err := json.Marshal(tool.OutputSchema)
					if err != nil {
						t.Fatal(err)
					}
					var gotSchema, expectedSchema any
					if err := json.Unmarshal(got, &gotSchema); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(wantSchemaJSON, &expectedSchema); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(gotSchema, expectedSchema) {
						t.Fatalf("schema = %s, want %s", got, wantSchemaJSON)
					}
					return
				}
				t.Fatal("check_client missing")
			})
		}
	}
}

func TestClientTools(t *testing.T) {
	e := newEnv(t)
	tools, err := e.remote.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, tool := range tools.Tools {
		if tool.Name == "check_client" || tool.Name == "get_client_skill" {
			count++
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
				t.Fatal("client tools must be read-only")
			}
		}
	}
	if count != 2 {
		t.Fatal("client tools missing")
	}
	var out clientstate.CheckResult
	text, failed := call(t, e.remote, "check_client", map[string]any{"cli_present": false, "bootstrap_present": false}, &out)
	if failed {
		t.Fatal(text)
	}
	if out.CLIStatus != "not_applicable" || out.BootstrapStatus != "not_applicable" || out.GuideStatus != "unknown" || out.Skill != "" {
		t.Fatalf("bad direct HTTP result: %+v", out)
	}
	if !strings.HasPrefix(out.ServerURL, "http://") {
		t.Fatalf("no request origin: %q", out.ServerURL)
	}
	var skill clientstate.Manifest
	text, failed = call(t, e.remote, "get_client_skill", map[string]any{}, &skill)
	if failed {
		t.Fatal(text)
	}
	if err := clientstate.ValidateManifest(skill); err != nil {
		t.Fatal(err)
	}
	if skill.BootstrapSHA256 != out.BootstrapSHA256 || skill.GuideRevision != out.GuideRevision {
		t.Fatal("tools disagree on manifest")
	}
	var guideOut GuideOut
	text, failed = call(t, e.remote, "guide", map[string]any{"items": []string{"topic.client-update"}}, &guideOut)
	if failed || guideOut.GuideRevision != out.GuideRevision || !strings.Contains(text, out.GuideRevision) {
		t.Fatalf("guide revision mismatch: %s", text)
	}
	text, failed = call(t, e.remote, "check_client", map[string]any{"cli_present": false, "client_contract": 1}, &out)
	if !failed {
		t.Fatalf("accepted conflicting identity: %s", text)
	}
}
