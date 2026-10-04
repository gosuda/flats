package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEnvCommand(t *testing.T) {
	api, srv := newFakeAPI(t)
	api.handle("PUT /api/flats/blog/env/MODE", 200, `{"name":"MODE","status":"stored"}`)
	for _, value := range []string{"staging", "", "-test"} {
		r := run(t, srv.URL, "", "env", "set", "--", "blog", "MODE", value)
		if r.code != ExitOK || !describesEnvActivation(r.stdout) {
			t.Fatalf("set: %+v", r)
		}
		var body map[string]string
		json.Unmarshal(api.last("PUT /api/flats/blog/env/MODE").body, &body)
		if body["value"] != value {
			t.Fatalf("value = %q, want %q", body["value"], value)
		}
	}
	api.handle("GET /api/flats/blog/env", 200, `{"env":[{"name":"MODE","value":"staging\nsecond"}],"note":"next deploy"}`)
	if r := run(t, srv.URL, "", "env", "ls", "blog"); r.code != ExitOK || !strings.Contains(r.stdout, `MODE="staging\nsecond"`) {
		t.Fatalf("ls: %+v", r)
	}
	api.handle("DELETE /api/flats/blog/env/MODE", 200, `{"deleted":"MODE"}`)
	if r := run(t, srv.URL, "", "env", "rm", "blog", "MODE"); r.code != ExitOK || !describesEnvActivation(r.stdout) {
		t.Fatalf("rm: %+v", r)
	}
	for _, args := range [][]string{{"env"}, {"env", "set", "blog", "MODE"}, {"env", "ls"}, {"env", "unknown"}} {
		if r := run(t, srv.URL, "", args...); r.code != ExitUsage {
			t.Fatalf("invalid %v: %+v", args, r)
		}
	}
}

func describesEnvActivation(note string) bool {
	return strings.Contains(note, "after any required approval") && strings.Contains(note, "Flats host restart") && strings.Contains(note, "New previews capture current settings") && strings.Contains(note, "automatic worker restarts reuse their captured settings")
}
