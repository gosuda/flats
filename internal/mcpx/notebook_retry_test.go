package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/notebook"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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

func (*notebookRetryService) CreateNotebook(context.Context, string, string, core.Via) (core.FlatView, error) {
	return core.FlatView{}, fmt.Errorf("unexpected create")
}

func TestRetryMCPContention(t *testing.T) {
	s := newNotebookRetryService(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "retry-test", Version: "1"}, nil)
	registerNotebook(server, &tools{notebook: s})
	h := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	t.Cleanup(h.Close)
	client := connect(t, h.URL)
	listed, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	advertised := false
	for _, tool := range listed.Tools {
		if tool.Name != "get_notebook" {
			continue
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema map[string]any
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		advertised = strings.Contains(string(raw), `"retry_hint"`) && strings.Contains(tool.Description, "retry_hint")
		resolved := notebookResolveSchema(t, schema)
		for _, args := range []map[string]any{
			{"slug": "first", "retry_hint": "ticket"},
			{"slug": "first", "page": notebookTestPage, "retry_hint": "ticket"},
			{"slug": "first", "database": notebookTestDatabase, "sort": []any{}, "retry_hint": "ticket"},
			{"slug": "first", "database": notebookTestDatabase, "view": "v_aaaaaaaaaaaaaaaaaaaaaaaaaa", "retry_hint": "ticket"},
		} {
			if err := resolved.Validate(args); err != nil {
				t.Fatalf("advertised schema rejects retry: %v", err)
			}
		}
	}
	if !advertised {
		t.Fatal("get_notebook does not advertise retry_hint")
	}
	assertNotebookRetryContention(t, s, func(slug, ticket string) notebookRetryReply {
		args := map[string]any{"slug": slug, "limit": 1}
		if ticket != "" {
			args["retry_hint"] = ticket
		}
		var out notebookRetryReply
		text, bad := call(t, client, "get_notebook", args, &out)
		if bad {
			_, raw, ok := strings.Cut(text, "\n{")
			if !ok || json.Unmarshal([]byte("{"+raw), &out) != nil {
				t.Fatalf("missing structured refusal: %s", text)
			}
		}
		return out
	})
	for _, tt := range []struct {
		name   string
		fields map[string]any
	}{
		{"empty", map[string]any{"retry_hint": ""}},
		{"number", map[string]any{"retry_hint": 1}},
		{"null", map[string]any{"retry_hint": nil}},
		{"cursor", map[string]any{"retry_hint": "ticket", "cursor": "cursor"}},
		{"mapping", map[string]any{"retry_hint": "ticket", "cursor": "cursor", "op_id": "write"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.fields["slug"] = "first"
			if text, bad := call(t, client, "get_notebook", tt.fields, nil); !bad {
				t.Fatalf("accepted invalid retry query: %s", text)
			}
		})
	}
}

func TestRetryMCPPreservesTicket(t *testing.T) {
	const ticket = "opaque +/=?&% ticket"
	raw, err := json.Marshal(map[string]any{"slug": "first", "retry_hint": ticket})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	_, _, query, err := notebookReadQuery(fields)
	var decoded struct {
		RetryHint string `json:"retry_hint"`
	}
	if err != nil || json.Unmarshal(query, &decoded) != nil || decoded.RetryHint != ticket {
		t.Fatalf("ticket changed: %s %v", query, err)
	}
}
