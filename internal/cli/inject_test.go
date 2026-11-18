package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// `flats secret set --origin --header` and `flats secret ls` of docs/spec/injected-fetch-secrets.md (rules 25 and 28).
func TestSecretSetInjected(t *testing.T) {
	api, srv := newFakeAPI(t)
	api.handle("PUT /api/flats/blog/secrets/API_TOKEN", 200, `{"name": "API_TOKEN", "status": "stored; redeploy to apply", "inject": {"origin": "https://api.example.com", "header": "Authorization"}}`)
	const value = "Bearer cli-injected-value"
	for _, args := range [][]string{
		{"secret", "set", "blog", "API_TOKEN", "--origin", "https://api.example.com"},
		{"secret", "set", "blog", "API_TOKEN", "--header", "Authorization"},
	} {
		if r := run(t, srv.URL, value+"\n", args...); r.code != ExitUsage || !strings.Contains(r.stderr, "--origin and --header go together") {
			t.Fatalf("%v: exit %d %s", args, r.code, r.stderr)
		}
	}
	api.mu.Lock()
	sent := len(api.reqs)
	api.mu.Unlock()
	if sent != 0 {
		t.Fatalf("a usage error sent %d requests", sent)
	}
	r := run(t, srv.URL, value+"\n", "secret", "set", "blog", "API_TOKEN", "--origin", "https://api.example.com", "--header", "Authorization")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	for _, part := range []string{"injected secret", "Authorization header", "https://api.example.com", "not in env", "flats network set blog"} {
		if !strings.Contains(r.stdout, part) {
			t.Errorf("the output lacks %q: %s", part, r.stdout)
		}
	}
	if strings.Contains(r.stdout+r.stderr, value) {
		t.Fatal("the value was printed")
	}
	var body struct {
		Value  string `json:"value"`
		Inject struct {
			Origin string `json:"origin"`
			Header string `json:"header"`
		} `json:"inject"`
	}
	_ = json.Unmarshal(api.last("PUT /api/flats/blog/secrets/API_TOKEN").body, &body)
	if body.Value != value || body.Inject.Origin != "https://api.example.com" || body.Inject.Header != "Authorization" {
		t.Fatalf("sent %+v", body)
	}

	api.handle("GET /api/flats/blog/secrets", 200, `{"secrets": [{"name": "API_TOKEN", "updated_at": "2026-10-01T00:00:00Z", "inject": {"origin": "https://api.example.com", "header": "Authorization"}}, {"name": "PLAIN", "updated_at": "2026-10-01T00:00:00Z"}]}`)
	r = run(t, srv.URL, "", "secret", "ls", "blog")
	if r.code != 0 || !strings.Contains(r.stdout, "INJECTED") || !strings.Contains(r.stdout, "Authorization to https://api.example.com") {
		t.Fatalf("ls: %d %s", r.code, r.stdout)
	}
	for _, line := range strings.Split(r.stdout, "\n") {
		if strings.HasPrefix(line, "PLAIN") && !strings.HasSuffix(strings.TrimSpace(line), "-") {
			t.Errorf("the row of a readable secret: %q", line)
		}
	}
	r = run(t, srv.URL, "", "secret", "--help")
	if !strings.Contains(r.stdout+r.stderr, "--origin URL --header NAME") {
		t.Errorf("the help text lacks the flags: %s %s", r.stdout, r.stderr)
	}
}
