package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/notebook"
)

type notebookRetryBackend struct{}

func (notebookRetryBackend) Limits() notebook.Limits                     { return notebook.Limits{} }
func (notebookRetryBackend) MeasureOther(context.Context) (int64, error) { return 0, nil }
func (notebookRetryBackend) PutBlob(context.Context, string, []byte) (bool, error) {
	return false, fmt.Errorf("unexpected blob write")
}
func (notebookRetryBackend) ReadBlob(context.Context, string) ([]byte, error) {
	return nil, fmt.Errorf("unexpected blob read")
}
func (notebookRetryBackend) Lease([]string) func() { return func() {} }
func (notebookRetryBackend) Exclusive(ctx context.Context, fn func(context.Context, int64) error) error {
	return fn(ctx, 0)
}
func (notebookRetryBackend) OutboxReady() {}

type notebookRetryService struct {
	instances map[string]*notebook.Instance
	manager   *notebook.SpillManager
	seconds   atomic.Int64
}

func newNotebookRetryService(t *testing.T) *notebookRetryService {
	t.Helper()
	s := &notebookRetryService{instances: map[string]*notebook.Instance{}}
	now := func() time.Time { return time.Unix(1_000_000+s.seconds.Load(), 0) }
	s.manager = notebook.NewSpillManager(t.TempDir(), 1, now)
	t.Cleanup(func() {
		if err := s.manager.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, slug := range []string{"first", "second"} {
		in, _, err := notebook.Open(t.TempDir(), notebookRetryBackend{}, notebook.Options{Slug: slug, Title: slug, Now: now, Spills: s.manager})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(in.Stop)
		s.instances[slug] = in
	}
	return s
}

func (s *notebookRetryService) GetNotebook(ctx context.Context, slug string, revision int, query []byte) (json.RawMessage, error) {
	if revision != 0 {
		return nil, fmt.Errorf("unexpected historical read")
	}
	return s.instances[slug].Read(ctx, query, false)
}

func (s *notebookRetryService) hold(t *testing.T) *notebook.Spill {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	spill, err := s.manager.Capture(ctx, s.instances["first"].ReadTx, notebook.Scope{Tree: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := spill.Close(); err != nil {
			t.Error(err)
		}
	})
	return spill
}

type notebookRetryReply struct {
	Category string `json:"category"`
	Reason   string `json:"reason"`
	Retry    bool   `json:"retry"`
	Detail   string `json:"detail"`
	Complete bool   `json:"complete"`
	Tree     []struct {
		Title string `json:"title"`
	} `json:"tree"`
}

func assertNotebookRetryContention(t *testing.T, s *notebookRetryService, read func(string, string) notebookRetryReply) {
	t.Helper()
	hint := func(slug, ticket string, position int) string {
		t.Helper()
		out := read(slug, ticket)
		if out.Category != "unavailable" || out.Reason != "spill_admission" || !out.Retry {
			t.Fatalf("expected retryable admission refusal: %+v", out)
		}
		var got struct {
			Admission string `json:"admission"`
			Position  int    `json:"position"`
			Expires   int64  `json:"expires_in_ms"`
		}
		if err := json.Unmarshal([]byte(out.Detail), &got); err != nil || got.Admission == "" || got.Position != position || got.Expires <= 0 {
			t.Fatalf("retry hint = %+v (%v), want position %d", got, err, position)
		}
		if ticket != "" && got.Admission != ticket {
			t.Fatalf("retry changed ticket: %q != %q", got.Admission, ticket)
		}
		return got.Admission
	}
	success := func(slug, ticket string) {
		t.Helper()
		out := read(slug, ticket)
		if out.Category != "" || !out.Complete || len(out.Tree) != 1 || out.Tree[0].Title != slug {
			t.Fatalf("ticket did not return its notebook: %+v", out)
		}
	}
	expired := func(slug, ticket string) {
		t.Helper()
		out := read(slug, ticket)
		if out.Category != "conflict" || out.Reason != "read_expired" || out.Retry {
			t.Fatalf("invalid ticket did not require a fresh read: %+v", out)
		}
	}
	active := s.hold(t)
	slugs := []string{"first", "second", "first"}
	tickets := make([]string, len(slugs))
	for i, slug := range slugs {
		tickets[i] = hint(slug, "", i+1)
	}
	for i := len(slugs) - 1; i >= 0; i-- {
		hint(slugs[i], tickets[i], i+1)
	}
	expired("second", tickets[0])
	if err := active.Close(); err != nil {
		t.Fatal(err)
	}
	fresh := hint("first", "", 4)
	for _, ticket := range tickets {
		if fresh == ticket {
			t.Fatal("new request reused another caller's ticket")
		}
	}
	hint("first", tickets[2], 3)
	hint("second", tickets[1], 2)
	success("first", tickets[0])
	hint("first", tickets[2], 2)
	success("second", tickets[1])
	success("first", tickets[2])
	success("first", fresh)
	expired("first", tickets[0])
	expired("first", "unknown-ticket")
	active = s.hold(t)
	ticket := hint("second", "", 1)
	s.seconds.Add(11)
	expired("second", ticket)
	if err := active.Close(); err != nil {
		t.Fatal(err)
	}
	success("second", "")
}

func TestRetryCLIContention(t *testing.T) {
	s := newNotebookRetryService(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/flats/{slug}/notebook", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Flats-Client") != "cli" {
			t.Error("request did not pass through CLI transport")
		}
		revision, query, err := api.NotebookQuery(r.URL.Query())
		if err != nil {
			t.Errorf("CLI sent invalid query: %v", err)
			http.Error(w, err.Error(), 400)
			return
		}
		raw, err := s.GetNotebook(r.Context(), r.PathValue("slug"), revision, query)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			var refusal *notebook.WriteError
			if !errors.As(err, &refusal) {
				t.Errorf("unexpected read error: %v", err)
				http.Error(w, err.Error(), 500)
				return
			}
			code := http.StatusServiceUnavailable
			if refusal.Code == "conflict" {
				code = http.StatusConflict
			}
			w.WriteHeader(code)
			if err := json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "category": refusal.Code, "reason": refusal.Reason, "retry": refusal.Retry, "detail": refusal.Detail}); err != nil {
				t.Error(err)
			}
			return
		}
		if _, err := w.Write(raw); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	configDir := t.TempDir()
	assertNotebookRetryContention(t, s, func(slug, ticket string) notebookRetryReply {
		args := []string{"--url", server.URL, "--json", "notebook", "get", slug, "--limit", "1"}
		if ticket != "" {
			args = append(args, "--retry-hint", ticket)
		}
		var out, errw bytes.Buffer
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		code := Run(ctx, args, Env{Stdout: &out, Stderr: &errw, ConfigDir: configDir, HTTP: server.Client()})
		if code == ExitOK {
			var pages []notebookRetryReply
			if err := json.Unmarshal(out.Bytes(), &pages); err != nil || len(pages) != 1 {
				t.Fatalf("CLI reply = %s (%v)", out.String(), err)
			}
			return pages[0]
		}
		var reply notebookRetryReply
		if code != ExitError || json.Unmarshal(out.Bytes(), &reply) != nil {
			t.Fatalf("CLI refusal = %d %s %s", code, out.String(), errw.String())
		}
		return reply
	})
}

func TestRetryCLIFlags(t *testing.T) {
	const ticket = "opaque +/=?&% ticket"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("retry_hint") != ticket || r.URL.Query().Has("retry-hint") {
			t.Errorf("ticket changed: %v", r.URL.Query())
		}
		fmt.Fprint(w, `{"complete":true,"next":null,"tree":[]}`)
	}))
	t.Cleanup(server.Close)
	var out, errw bytes.Buffer
	configDir := t.TempDir()
	env := Env{Stdout: &out, Stderr: &errw, ConfigDir: configDir, HTTP: server.Client()}
	args := []string{"--url", server.URL, "--json", "notebook", "get", "notes", "--retry-hint", ticket}
	if code := Run(t.Context(), args, env); code != ExitOK || calls != 1 {
		t.Fatalf("flag = %d %s %s", code, out.String(), errw.String())
	}
	for _, tt := range []struct {
		name string
		args []string
	}{
		{"empty", []string{"--retry-hint", ""}},
		{"repeated", []string{"--retry-hint", "one", "--retry-hint", "two"}},
		{"cursor", []string{"--retry-hint", "one", "--cursor", "cursor"}},
		{"mapping", []string{"--retry-hint", "one", "--cursor", "cursor", "--op-id", "write"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out.Reset()
			errw.Reset()
			args := append([]string{"--url", server.URL, "notebook", "get", "notes"}, tt.args...)
			if code := Run(t.Context(), args, env); code != ExitUsage || calls != 1 {
				t.Fatalf("invalid flags = %d %s %s, calls %d", code, out.String(), errw.String(), calls)
			}
		})
	}
	out.Reset()
	errw.Reset()
	if code := Run(t.Context(), []string{"notebook", "get", "--help"}, env); code != ExitOK || !strings.Contains(out.String()+errw.String(), "retry-hint") {
		t.Fatalf("retry flag missing from help: %d %s %s", code, out.String(), errw.String())
	}
}

func TestRetryCLIContinuationDropsTicket(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if calls == 1 {
			if len(q) != 2 || q.Get("retry_hint") != "ticket" || q.Get("limit") != "1" {
				t.Errorf("initial query = %v", q)
			}
			fmt.Fprint(w, `{"complete":false,"next":"captured","tree":[]}`)
			return
		}
		if len(q) != 1 || q.Get("cursor") != "captured" {
			t.Errorf("continuation retained admission ticket: %v", q)
		}
		fmt.Fprint(w, `{"complete":true,"next":null,"tree":[]}`)
	}))
	t.Cleanup(server.Close)
	var out, errw bytes.Buffer
	args := []string{"--url", server.URL, "--json", "notebook", "get", "notes", "--limit", "1", "--retry-hint", "ticket"}
	code := Run(t.Context(), args, Env{Stdout: &out, Stderr: &errw, ConfigDir: t.TempDir(), HTTP: server.Client()})
	var pages []notebookRetryReply
	if code != ExitOK || calls != 2 || json.Unmarshal(out.Bytes(), &pages) != nil || len(pages) != 2 {
		t.Fatalf("continued retry = %d %s %s, calls %d", code, out.String(), errw.String(), calls)
	}
}
