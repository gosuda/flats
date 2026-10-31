package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/notebook"
	"github.com/gosuda/flats/internal/store"
)

func TestHostNotebookRefusalText(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, body string
		want       []string
	}{
		{"permanent", `{"error":"invalid:","category":"invalid","reason":"field","retry":false,"detail":"ops[0]: unknown field"}`, []string{"error: invalid:", "reason: field", "detail: ops[0]: unknown field", "retry: false"}},
		{"retryable", `{"error":"unavailable:read_admission","category":"unavailable","reason":"read_admission","retry":true,"detail":"retry with the same cursor"}`, []string{"reason: read_admission", "detail: retry with the same cursor", "retry: true"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			r := run(t, srv.URL, "", "notebook", "get", "notes")
			if r.code != ExitError || r.stdout != "" {
				t.Fatalf("refusal = %+v", r)
			}
			for _, want := range tt.want {
				if !strings.Contains(r.stderr, want) {
					t.Errorf("stderr %q missing %q", r.stderr, want)
				}
			}
			r = run(t, srv.URL, "", "--json", "notebook", "get", "notes")
			if r.code != ExitError || strings.TrimSpace(r.stdout) != tt.body {
				t.Fatalf("JSON refusal changed: %+v", r)
			}
		})
	}
}

func TestHostNotebookRejectsOversizedFileLocally(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "oversized.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse input safely exercises the old read-all path without an infinite device.
	if err := f.Truncate((8 << 20) + 4*((20<<20+2)/3) + 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	a, _, _ := notebookTestApp()
	c := notebookFakeClient{invoke: func(context.Context, string, string, url.Values, any) (string, error) {
		t.Error("oversized input reached API")
		return `{}`, nil
	}}
	if err := a.notebookApply(c, "notes", path, "", ""); err == nil || !strings.Contains(err.Error(), "--max-input-bytes") {
		t.Fatalf("expected local input-limit refusal, got %v", err)
	}
}

func TestHostNotebookPrunedReplayText(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	priv, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { priv.Close() })
	svc, err := core.New(context.Background(), core.Config{DataDir: filepath.Join(dir, "data"), Store: st, Private: priv, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	srv := httptest.NewServer((&api.Server{Svc: svc}).Handler())
	t.Cleanup(srv.Close)
	path := notebookFile(t, notebookCLIUpdate)
	r := run(t, srv.URL, "", "notebook", "apply", "notes", path)
	if r.code != ExitOK {
		t.Fatalf("first write: %+v", r)
	}
	one := 1
	if err := svc.SetRevisionQuotas(context.Background(), "notes", &one, nil); err != nil {
		t.Fatal(err)
	}
	second := notebookFile(t, strings.ReplaceAll(notebookCLIUpdate, "cli-write-1", "cli-write-2"))
	if r := run(t, srv.URL, "", "notebook", "apply", "notes", second); r.code != ExitOK {
		t.Fatalf("second write: %+v", r)
	}
	r = run(t, srv.URL, "", "--json", "notebook", "apply", "notes", path)
	var replay struct {
		Result   notebook.WriteResult `json:"result"`
		Revision int                  `json:"revision"`
		Pending  bool                 `json:"pending"`
	}
	if r.code != ExitOK || json.Unmarshal([]byte(r.stdout), &replay) != nil || !replay.Result.Replay || replay.Pending || replay.Result.RevisionPending || replay.Revision != 0 {
		t.Fatalf("not a real pruned replay: %+v", r)
	}
	pruned := false
	for _, warning := range replay.Result.Warnings {
		pruned = pruned || warning.Code == "revision_pruned"
	}
	if !pruned {
		t.Fatalf("pruned warning missing: %+v", replay)
	}
	r = run(t, srv.URL, "", "notebook", "apply", "notes", path)
	if r.code != ExitOK || !strings.Contains(r.stdout, "revision pruned") || strings.Contains(r.stdout, "revision pending") || strings.Contains(r.stderr, "import is pending") || !strings.Contains(r.stdout, "already applied") {
		t.Fatalf("pruned replay mislabeled: %+v", r)
	}
	revisions, err := svc.ListRevisions(context.Background(), "notes", 0, 10)
	if err != nil || len(revisions.Revisions) != 1 {
		t.Fatalf("replay changed retained revisions: %+v / %v", revisions, err)
	}
}

func TestHostNotebookInputLimitFlag(t *testing.T) {
	t.Parallel()
	path := notebookFile(t, notebookCLIUpdate)
	for _, tt := range []struct {
		name, limit string
		wantCode    int
	}{
		{"exact boundary", strconv.Itoa(len(notebookCLIUpdate)), ExitOK},
		{"one byte over", strconv.Itoa(len(notebookCLIUpdate) - 1), ExitUsage},
		{"zero", "0", ExitUsage},
		{"negative", "-1", ExitUsage},
		{"sentinel overflow", "9223372036854775807", ExitUsage},
		{"flag overflow", "9223372036854775808", ExitUsage},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				io.WriteString(w, `{"result":{"applied":1},"revision":2,"pending":false}`)
			}))
			defer srv.Close()
			r := run(t, srv.URL, "", "notebook", "apply", "notes", path, "--max-input-bytes", tt.limit)
			if r.code != tt.wantCode {
				t.Fatalf("input limit: %+v", r)
			}
			wantCalls := int32(0)
			if tt.wantCode == ExitOK {
				wantCalls = 1
			}
			if calls.Load() != wantCalls {
				t.Fatalf("API calls = %d, want %d", calls.Load(), wantCalls)
			}
			if tt.wantCode == ExitUsage && !strings.Contains(r.stderr, "--max-input-bytes") {
				t.Fatalf("missing limit guidance: %+v", r)
			}
		})
	}
}

func TestHostNotebookBoundsDeviceInput(t *testing.T) {
	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skip("zero device unavailable")
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	r := run(t, srv.URL, "", "notebook", "apply", "notes", "/dev/zero", "--max-input-bytes", "1024")
	if r.code != ExitUsage || !strings.Contains(r.stderr, "exceeds --max-input-bytes (1024)") || calls.Load() != 0 {
		t.Fatalf("unbounded device input was not refused locally: %+v / %d API calls", r, calls.Load())
	}
}

func TestHostNotebookUploadInputLimits(t *testing.T) {
	for _, tt := range []struct {
		name  string
		bytes int
		raise bool
	}{
		{"default host upload limit", 20 << 20, false},
		{"larger host upload limit", 28 << 20, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := base64.StdEncoding.EncodeToString(make([]byte, tt.bytes))
			body := strings.TrimSuffix(notebookCLIUpdate, "}") + `,"assets":[{"ref":"image","data":"` + data + `"}]}`
			path := notebookFile(t, body)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var sent struct {
					Assets []struct {
						Data string `json:"data"`
					} `json:"assets"`
				}
				if err := json.NewDecoder(r.Body).Decode(&sent); err != nil || len(sent.Assets) != 1 || sent.Assets[0].Data != data {
					t.Errorf("upload changed or truncated: %v", err)
				}
				io.WriteString(w, `{"result":{"applied":1},"revision":2,"pending":false}`)
			}))
			defer srv.Close()
			args := []string{"notebook", "apply", "notes", path}
			if tt.raise {
				if r := run(t, srv.URL, "", args...); r.code != ExitUsage || calls.Load() != 0 {
					t.Fatalf("larger input escaped default local limit: %+v", r)
				}
				limit, err := notebook.MaxUpdateBodyBytes(int64(tt.bytes))
				if err != nil {
					t.Fatal(err)
				}
				args = append(args, "--max-input-bytes", strconv.FormatInt(limit, 10))
			}
			if r := run(t, srv.URL, "", args...); r.code != ExitOK || calls.Load() != 1 {
				t.Fatalf("upload input refused or not sent exactly once: %+v / %d API calls", r, calls.Load())
			}
		})
	}
}

func TestHostNotebookRevisionStates(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, body, want string }{
		{"retained", `{"result":{},"revision":7,"pending":false}`, "revision 7"},
		{"host pending", `{"result":{},"revision":0,"pending":true}`, "revision pending"},
		{"writer pending", `{"result":{"revision_pending":true},"revision":0,"pending":false}`, "revision pending"},
		{"unknown", `{"result":{},"revision":0,"pending":false}`, "revision unavailable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, out, errw := notebookTestApp()
			c := notebookFakeClient{invoke: func(context.Context, string, string, url.Values, any) (string, error) {
				return tt.body, nil
			}}
			if err := a.notebookApply(c, "notes", notebookFile(t, notebookCLIUpdate), "", ""); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "Applied notebook notes: "+tt.want+",") || strings.Contains(errw.String(), "import is pending") != (tt.want == "revision pending") {
				t.Fatalf("revision state: %s / %s", out, errw)
			}
		})
	}
}

func TestHostLegacyAPIErrorText(t *testing.T) {
	a, _, errw := notebookTestApp()
	printAPIError(a.errw, newAPIError(response{Status: 400, Body: []byte(`{"error":"bad request","category":"invalid"}`)}))
	if errw.String() != "error: bad request\n" {
		t.Fatalf("legacy output changed: %s", errw)
	}
}

func TestHostNotebookInputLimitHelp(t *testing.T) {
	for _, args := range [][]string{{"help", "notebook"}, {"notebook", "apply", "--help"}} {
		r := run(t, "", "", args...)
		for _, want := range []string{"--max-input-bytes", "8 MiB + 4 * ceil(20 MiB / 3)", "upload_max_bytes", "does not raise the host's"} {
			if r.code != ExitOK || !strings.Contains(r.stdout, want) {
				t.Errorf("help %v missing %q: %+v", args, want, r)
			}
		}
	}
}
