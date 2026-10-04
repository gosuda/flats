package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

func TestEnvManagement(t *testing.T) {
	srv, svc := setup(t)
	if _, err := svc.CreateFlat(context.Background(), "envapp", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"/api", "/console/api"} {
		hdr := map[string]string(nil)
		if prefix == "/console/api" {
			hdr = consoleHdr(t, srv)
		}
		base := srv.URL + prefix + "/flats/envapp/env"
		for _, body := range []string{`{}`, `{"value":null}`, `{"value":3}`, `{"value":"config","extra":true}`} {
			if code, out := req(t, "PUT", base+"/MODE", strings.NewReader(body), hdr); code != 400 {
				t.Fatalf("invalid value = %d %v", code, out)
			}
		}
		for _, value := range []string{"staging", ""} {
			if code, out := req(t, "PUT", base+"/MODE", strings.NewReader(`{"value":"`+value+`"}`), hdr); code != 200 || !describesEnvActivation(out["note"]) {
				t.Fatalf("set = %d %v", code, out)
			}
			code, out := req(t, "GET", base, nil, hdr)
			vars, ok := out["env"].([]any)
			if code != 200 || !ok || len(vars) != 1 || vars[0].(map[string]any)["value"] != value || !describesEnvActivation(out["note"]) {
				t.Fatalf("list = %d %v", code, out)
			}
		}
		if code, out := req(t, "DELETE", base+"/MODE", nil, hdr); code != 200 || !describesEnvActivation(out["note"]) {
			t.Fatalf("delete = %d %v", code, out)
		}
		if code, out := req(t, "GET", base, nil, hdr); code != 200 || len(out["env"].([]any)) != 0 {
			t.Fatalf("empty = %d %v", code, out)
		}
	}
	if err := svc.SetSecret(context.Background(), "envapp", "TOKEN", "test-secret", core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	code, out := req(t, "GET", srv.URL+"/api/flats/envapp/env", nil, nil)
	if code != 200 || strings.Contains(envJSON(out), "test-secret") || len(out["env"].([]any)) != 0 {
		t.Fatalf("secret leaked: %v", out)
	}
	if code, out := req(t, "PUT", srv.URL+"/api/flats/envapp/env/TOKEN", strings.NewReader(`{"value":"ordinary"}`), nil); code != 400 {
		t.Fatalf("secret collision = %d %v", code, out)
	}
	if code, out := req(t, "PUT", srv.URL+"/api/flats/envapp/env/DB", strings.NewReader(`{"value":"ordinary"}`), nil); code != 400 {
		t.Fatalf("reserved binding = %d %v", code, out)
	}
	if code, _ := req(t, "GET", srv.URL+"/api/flats/missing/env", nil, nil); code != 404 {
		t.Fatalf("missing flat = %d", code)
	}
}

func envJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// Activation guidance must distinguish a host restart from a worker restart.
func describesEnvActivation(v any) bool {
	note, ok := v.(string)
	return ok && strings.Contains(note, "after any required approval") && strings.Contains(note, "Flats host restart") && strings.Contains(note, "New previews capture current settings") && strings.Contains(note, "automatic worker restarts reuse their captured settings")
}
