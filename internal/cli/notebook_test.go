package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const notebookCLIPageID = "p_aaaaaaaaaaaaaaaaaaaaaaaaaa"
const notebookCLIUpdate = `{"op_id":"cli-write-1","ops":[{"op":"create_page","ref":"page","parent":null,"before":null,"title":"Notes"}]}`

type notebookFakeClient struct {
	invoke func(context.Context, string, string, url.Values, any) (string, error)
}

func (f notebookFakeClient) call(ctx context.Context, method, path string, q url.Values, in, out any) (response, error) {
	raw, err := f.invoke(ctx, method, path, q, in)
	if err != nil {
		return response{}, err
	}
	if out != nil {
		if err := json.Unmarshal([]byte(raw), out); err != nil {
			return response{}, err
		}
	}
	return response{Status: 200, Body: []byte(raw)}, nil
}

func notebookTestApp() (*app, *bytes.Buffer, *bytes.Buffer) {
	out, errw := new(bytes.Buffer), new(bytes.Buffer)
	return &app{ctx: context.Background(), out: out, errw: errw}, out, errw
}

func notebookFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ops.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNotebookGetPaging(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		initial url.Values
	}{
		{"historical markdown", url.Values{"page": {notebookCLIPageID}, "format": {"markdown"}, "revision": {"4"}, "limit": {"1"}}},
		{"mapping", url.Values{"op_id": {"cli-write-1"}, "cursor": {"mapping-start"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, out, _ := notebookTestApp()
			a.jsonOut = true
			calls := 0
			c := notebookFakeClient{invoke: func(ctx context.Context, method, path string, q url.Values, in any) (string, error) {
				calls++
				if method != "GET" || path != "/api/flats/notes/notebook" || in != nil {
					t.Fatalf("request = %s %s %v", method, path, in)
				}
				if calls == 1 {
					if q.Encode() != tt.initial.Encode() {
						t.Fatalf("initial query = %v", q)
					}
					return `{"state":{"store":"s","seq":"4"},"markdown":"part one","offset":0,"total_bytes":16,"references":[{"marker":"block"}],"next":"next-page","complete":false}`, nil
				}
				want := url.Values{"cursor": {"next-page"}}
				if tt.initial.Has("op_id") {
					want.Set("op_id", tt.initial.Get("op_id"))
				}
				if q.Encode() != want.Encode() {
					t.Fatalf("continuation retained selectors: %v", q)
				}
				return `{"state":{"store":"s","seq":"4"},"markdown":"part two","offset":8,"total_bytes":16,"next":null,"complete":true}`, nil
			}}
			if err := a.notebookGet(c, "notes", tt.initial); err != nil {
				t.Fatal(err)
			}
			var pages []map[string]any
			if err := json.Unmarshal(out.Bytes(), &pages); err != nil {
				t.Fatal(err)
			}
			if calls != 2 || len(pages) != 2 || pages[0]["offset"] != float64(0) || pages[1]["offset"] != float64(8) || pages[0]["references"] == nil {
				t.Fatalf("pages = %s", out.String())
			}
		})
	}
}

func TestNotebookGetIncomplete(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, body string }{
		{"missing completion", `{"next":null}`},
		{"missing continuation", `{"complete":false,"next":null}`},
		{"unexpected continuation", `{"complete":true,"next":"cursor"}`},
		{"repeated cursor", `{"complete":false,"next":"cursor"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, _, _ := notebookTestApp()
			c := notebookFakeClient{invoke: func(context.Context, string, string, url.Values, any) (string, error) { return tt.body, nil }}
			if err := a.notebookGet(c, "notes", url.Values{}); err == nil {
				t.Fatal("accepted incomplete or looping read")
			}
		})
	}
}

func TestNotebookApply(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, body, id  string
		pending, replay bool
	}{
		{"existing id", notebookCLIUpdate, "", false, false},
		{"generated id", strings.Replace(notebookCLIUpdate, `"op_id":"cli-write-1",`, "", 1), "", true, false},
		{"explicit id", strings.Replace(notebookCLIUpdate, `"op_id":"cli-write-1",`, "", 1), "retry-1", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, out, errw := notebookTestApp()
			calls := 0
			c := notebookFakeClient{invoke: func(ctx context.Context, method, path string, q url.Values, in any) (string, error) {
				calls++
				raw, _ := json.Marshal(in)
				var body map[string]any
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Fatal(err)
				}
				id, _ := body["op_id"].(string)
				if id == "" || !strings.Contains(errw.String(), id) || body["slug"] != nil || body["message"] != "history note" || method != "POST" || path != "/api/flats/notes/notebook" || len(q) != 0 {
					t.Fatalf("write = %s %s %v %s; stderr=%s", method, path, q, raw, errw.String())
				}
				if tt.id != "" && id != tt.id {
					t.Fatalf("id = %s", id)
				}
				result := map[string]any{"result": map[string]any{"state": map[string]any{"store": "s", "seq": "1"}, "revision_pending": tt.pending, "replay": tt.replay, "applied": 1, "mapping": map[string]any{"complete": true, "total": 1}, "warnings": []any{}}, "pending": tt.pending, "revision": 2}
				encoded, _ := json.Marshal(result)
				return string(encoded), nil
			}}
			if err := a.notebookApply(c, "notes", notebookFile(t, tt.body), "history note", tt.id); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || !strings.Contains(out.String(), "Applied notebook notes") {
				t.Fatalf("output = %s", out.String())
			}
			if tt.pending && (!strings.Contains(out.String(), "revision pending") || !strings.Contains(errw.String(), "edit committed")) {
				t.Fatalf("pending = %s %s", out.String(), errw.String())
			}
			if tt.replay && !strings.Contains(out.String(), "already applied") {
				t.Fatalf("replay = %s", out.String())
			}
		})
	}
}

func TestNotebookApplyAdmission(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, body, id string }{
		{"null", "null", ""}, {"empty", "", ""}, {"array", "[]", ""},
		{"duplicate", `{"op_id":"x","op_id":"y","ops":[]}`, ""},
		{"slug", `{"slug":"notes","ops":[]}`, ""},
		{"unknown", `{"ops":[],"bogus":true}`, ""},
		{"trailing", notebookCLIUpdate + ` {}`, ""},
		{"invalid op id", strings.Replace(notebookCLIUpdate, "cli-write-1", "bad id", 1), ""},
		{"mismatched id", notebookCLIUpdate, "another"},
		{"browser-only", strings.Replace(notebookCLIUpdate, `"title":"Notes"`, `"title":"Notes","unit_id":"browser"`, 1), ""},
		{"bare mutation", `{"op_id":"x","ops":[{"op":"delete_page","page":"` + notebookCLIPageID + `"}]}`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, _, _ := notebookTestApp()
			c := notebookFakeClient{invoke: func(context.Context, string, string, url.Values, any) (string, error) {
				t.Fatal("invalid body reached API")
				return "", nil
			}}
			var usage usageError
			if err := a.notebookApply(c, "notes", notebookFile(t, tt.body), "", tt.id); !errors.As(err, &usage) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestNotebookCommandRoundTrip(t *testing.T) {
	t.Parallel()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/flats/notes/notebook" || r.Header.Get("X-Flats-Client") != "cli" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			raw, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(raw), `"op_id":"cli-write-1"`) {
				t.Errorf("body=%s", raw)
			}
			io.WriteString(w, `{"result":{"replay":true,"applied":1},"revision":2,"pending":false}`)
			return
		}
		io.WriteString(w, `{"state":{"store":"s","seq":"1"},"tree":[],"complete":true,"next":null}`)
	}))
	defer srv.Close()
	for _, args := range [][]string{{"--json", "notebook", "get", "notes", "--limit", "1"}, {"--json", "notebook", "apply", "notes", notebookFile(t, notebookCLIUpdate)}} {
		var out, errw bytes.Buffer
		code := Run(context.Background(), append([]string{"--url", srv.URL}, args...), Env{Stdout: &out, Stderr: &errw, ConfigDir: t.TempDir(), HTTP: srv.Client()})
		if code != 0 || !json.Valid(out.Bytes()) {
			t.Fatalf("command = %d %s %s", code, out.String(), errw.String())
		}
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestNotebookCommandAdmission(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		args []string
	}{
		{"missing", []string{"notebook"}},
		{"archive", []string{"notebook", "import", "notes", "archive.zip"}},
		{"limit", []string{"notebook", "get", "notes", "--limit", "0"}},
		{"cursor selector", []string{"notebook", "get", "notes", "--cursor", "x", "--page", notebookCLIPageID}},
		{"duplicate flag", []string{"notebook", "get", "notes", "--limit", "1", "--limit", "2"}},
		{"wrong kind", []string{"notebook", "get", "notes", "--page", "d_aaaaaaaaaaaaaaaaaaaaaaaaaa"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			code := Run(context.Background(), tt.args, Env{Stdout: &out, Stderr: &errw, ConfigDir: t.TempDir()})
			if code != ExitUsage {
				t.Fatalf("code=%d out=%s err=%s", code, out.String(), errw.String())
			}
		})
	}
}
