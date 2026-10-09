package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runConn runs the CLI with the given FLATS_URL (empty: unset) and saved
// connection directory.
func runConn(t *testing.T, configDir, flatsURL string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errb, ConfigDir: configDir, Home: t.TempDir(), Launchd: (&fakeLaunchctl{}).run,
		Getenv: func(k string) string {
			if k == "FLATS_URL" {
				return flatsURL
			}
			return ""
		}}
	code := Run(context.Background(), args, env)
	return result{code, out.String(), errb.String()}
}

func TestConnectSavesAndIsUsedByLaterCommands(t *testing.T) {
	api, srv := newFakeAPI(t)
	api.handle("GET /api/flats", 200, `{"flats": []}`)
	dir := t.TempDir()

	// A pasted MCP address is reduced to the console address.
	r := runConn(t, dir, "", "connect", srv.URL+"/mcp/")
	if r.code != 0 || !strings.Contains(r.stdout, "Saved "+srv.URL+".") {
		t.Fatalf("connect: %+v", r)
	}
	b, err := os.ReadFile(filepath.Join(dir, "flats-client", "connection.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved savedConnection
	if err := json.Unmarshal(b, &saved); err != nil || saved.URL != srv.URL {
		t.Fatalf("saved %s %v", b, err)
	}

	// Without FLATS_URL or --url, other commands use the saved host.
	if r := runConn(t, dir, "", "list"); r.code != 0 {
		t.Fatalf("list via saved host: %+v", r)
	}
	if api.last("GET /api/flats").method == "" {
		t.Fatal("list did not reach the saved host")
	}

	r = runConn(t, dir, "", "--json", "connect")
	var shown map[string]string
	if err := json.Unmarshal([]byte(r.stdout), &shown); err != nil || shown["url"] != srv.URL || shown["source"] != "saved" || shown["saved"] != srv.URL {
		t.Fatalf("show: %+v %v", r, err)
	}

	// FLATS_URL and --url still win, in that order.
	r = runConn(t, dir, "http://env.example:1", "connect")
	if !strings.Contains(r.stdout, "http://env.example:1 (from FLATS_URL)") || !strings.Contains(r.stdout, "overrides the saved host "+srv.URL) {
		t.Fatalf("env precedence: %+v", r)
	}
	r = runConn(t, dir, "http://env.example:1", "connect", "--url", "http://flag.example:2")
	if !strings.Contains(r.stdout, "http://flag.example:2 (from --url)") {
		t.Fatalf("flag precedence: %+v", r)
	}

	r = runConn(t, dir, "", "connect", "--clear")
	if r.code != 0 || !strings.Contains(r.stdout, DefaultURL+" (loopback default)") {
		t.Fatalf("clear: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "flats-client", "connection.json")); !os.IsNotExist(err) {
		t.Fatalf("clear left the file: %v", err)
	}
}

func TestConnectRefusesHostsThatDoNotAnswer(t *testing.T) {
	dir := t.TempDir()
	api, srv := newFakeAPI(t)
	api.handle("GET /api/status", 404, `{"error": "not found"}`)
	for _, target := range []string{srv.URL, "http://127.0.0.1:1"} {
		r := runConn(t, dir, "", "connect", target)
		if r.code != ExitError || !strings.Contains(r.stderr, "nothing saved") {
			t.Fatalf("%s: %+v", target, r)
		}
	}
	for _, bad := range []string{"flats.example.ts.net", "ftp://x", "https://u:p@x", "https://x/?a=1"} {
		if r := runConn(t, dir, "", "connect", bad); r.code != ExitUsage {
			t.Fatalf("%s: %+v", bad, r)
		}
	}
	if r := runConn(t, dir, "", "connect", "https://x", "--clear"); r.code != ExitUsage {
		t.Fatalf("url and --clear: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "flats-client")); !os.IsNotExist(err) {
		t.Fatalf("a refused connect wrote state: %v", err)
	}
}

func TestBrokenSavedConnectionFallsBackWithWarning(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "flats-client", "connection.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"url": "not a url"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runConn(t, dir, "", "connect")
	if r.code != 0 || !strings.Contains(r.stdout, DefaultURL+" (loopback default)") || strings.Count(r.stderr, "ignoring the saved connection") != 1 {
		t.Fatalf("%+v", r)
	}
}
