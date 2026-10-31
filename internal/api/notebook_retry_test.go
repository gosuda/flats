package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
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

func (*notebookRetryService) UploadLimit() int64 { return 0 }
func (*notebookRetryService) UpdateNotebook(context.Context, string, []byte, core.Via) (core.NotebookWriteResult, error) {
	return core.NotebookWriteResult{}, fmt.Errorf("unexpected write")
}

func TestRetryManagementContention(t *testing.T) {
	s := newNotebookRetryService(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/flats/{slug}/notebook", func(w http.ResponseWriter, r *http.Request) { getNotebook(w, r, s) })
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 3 * time.Second
	assertNotebookRetryContention(t, s, func(slug, ticket string) notebookRetryReply {
		q := url.Values{"limit": {"1"}}
		if ticket != "" {
			q.Set("retry_hint", ticket)
		}
		response, err := client.Get(server.URL + "/api/flats/" + slug + "/notebook?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var out notebookRetryReply
		if err := json.NewDecoder(response.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		want := http.StatusOK
		switch out.Category {
		case "unavailable":
			want = http.StatusServiceUnavailable
		case "conflict":
			want = http.StatusConflict
		}
		if response.StatusCode != want {
			t.Fatalf("HTTP %d: %+v", response.StatusCode, out)
		}
		return out
	})
}

func TestRetryManagementQuery(t *testing.T) {
	const ticket = "opaque +/=?&% ticket"
	_, raw, err := NotebookQuery(url.Values{"limit": {"1"}, "retry_hint": {ticket}})
	var query struct {
		RetryHint string `json:"retry_hint"`
	}
	if err != nil || json.Unmarshal(raw, &query) != nil || query.RetryHint != ticket {
		t.Fatalf("ticket changed: %s %v", raw, err)
	}
	for _, tt := range []struct {
		name  string
		query url.Values
	}{
		{"empty", url.Values{"retry_hint": {""}}},
		{"repeated", url.Values{"retry_hint": {"one", "two"}}},
		{"cursor", url.Values{"cursor": {"cursor"}, "retry_hint": {ticket}}},
		{"mapping", url.Values{"cursor": {"cursor"}, "op_id": {"write"}, "retry_hint": {ticket}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := NotebookQuery(tt.query); err == nil {
				t.Fatal("accepted invalid retry query")
			}
		})
	}
}
