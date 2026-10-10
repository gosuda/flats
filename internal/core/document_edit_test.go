package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/store"
)

// editResponse is the fake docs app's edit route: the find text selects the
// outcome, so the host's routing and error mapping are tested in isolation.
func editResponse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("X-Flats-Access") != "private" || r.Header.Get("X-Flats-Host-Op") != "edit" {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"code":"readonly","message":"host only"}`)
		return
	}
	var in struct {
		Ops    []DocumentEditOp `json:"ops"`
		IfHash string           `json:"if_hash"`
	}
	b, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(b, &in); err != nil || len(in.Ops) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	status, code := 200, ""
	switch in.Ops[0].Find {
	case "conflict":
		status, code = 409, "edit_conflict"
	case "invalid":
		status, code = 400, "invalid"
	case "capacity":
		status, code = 422, "capacity"
	case "boom":
		status = 500
	case "rolledback":
		status, code = 503, "unavailable"
	case "missing":
		status = 404
	}
	w.WriteHeader(status)
	if status != 200 {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": "op 1: " + in.Ops[0].Find + " " + in.IfHash})
		return
	}
	_ = json.NewEncoder(w).Encode(DocumentEdit{Format: 1, Doc: r.URL.Query().Get("doc") + "index.md", SeqBefore: 7, Seq: 8, Chain: "c", Hash: strings.Repeat("b", 64), Bytes: 42, Changed: in.Ops[0].Find != "same", ID: "agent:x",
		Ops: []DocumentEditChange{{Op: in.Ops[0].Op, Line: 3, Removed: 4, Inserted: 9}}})
}

func TestUpdateDocumentRoutingAndAudit(t *testing.T) {
	s, _ := newTestService(t)
	s.cfg.Runtime = &contentRuntime{}
	s.cfg.DocsApp = testDocsApp()
	ctx := context.Background()
	op := func(find string) []DocumentEditOp {
		with := "x"
		return []DocumentEditOp{{Op: "replace", Find: find, With: &with}}
	}
	if _, err := s.UpdateDocument(ctx, "absent", "", op("a"), nil, ViaMCP); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.CreateFlat(ctx, "empty", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDocument(ctx, "empty", "", op("a"), nil, ViaMCP); !errors.Is(err, ErrNotDeployed) {
		t.Fatal(err)
	}
	if _, err := s.SaveVersion(ctx, "website", []bundle.File{{Path: "index.html", Data: []byte("web")}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDocument(ctx, "website", "", op("a"), nil, ViaMCP); !errors.Is(err, ErrNotDocs) {
		t.Fatal("website Draft", err)
	}
	if _, err := approvedInternalDeploy(t, s, ctx, "website", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDocument(ctx, "website", "", op("a"), nil, ViaMCP); !errors.Is(err, ErrNotDocs) {
		t.Fatal("live website", err)
	}
	saveDocs(t, s, "website")
	if _, err := s.UpdateDocument(ctx, "website", "", op("a"), nil, ViaMCP); !errors.Is(err, ErrNotDeployed) || !strings.Contains(err.Error(), "save_document and publish") {
		t.Fatal("docs Draft over a live website", err)
	}
	saveDocs(t, s, "notes")
	if _, err := s.UpdateDocument(ctx, "notes", "", op("a"), nil, ViaMCP); !errors.Is(err, ErrNotDeployed) {
		t.Fatal("unpublished docs", err)
	}
	if _, err := approvedInternalDeploy(t, s, ctx, "notes", 0); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]DocumentEditOp{nil, make([]DocumentEditOp, MaxDocumentEditOps+1)} {
		if _, err := s.UpdateDocument(ctx, "notes", "", bad, nil, ViaMCP); !errors.Is(err, ErrInvalid) {
			t.Fatal(len(bad), err)
		}
	}
	if _, err := s.UpdateDocument(ctx, "notes", "../x.md", op("a"), nil, ViaMCP); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := s.UpdateDocument(ctx, "notes", "", []DocumentEditOp{{Op: "insert", At: "end", Text: strings.Repeat("x", maxDocumentEditBody)}}, nil, ViaMCP); !errors.Is(err, ErrDocumentCapacity) {
		t.Fatal("oversized request", err)
	}
	// A caller whose context ends right after the commit still gets the audit.
	cancelled, cancelNow := context.WithCancel(ctx)
	s.cfg.Runtime.(*contentRuntime).afterEdit = cancelNow
	if _, err := s.UpdateDocument(cancelled, "notes", "", op("ok"), nil, ViaMCP); err != nil {
		t.Fatal(err)
	}
	s.cfg.Runtime.(*contentRuntime).afterEdit = nil
	out, err := s.UpdateDocument(ctx, "notes", "", op("ok"), nil, ViaMCP)
	if err != nil || out.Seq != 8 || out.Source != "live" || out.PublicNotice != "" || !out.Changed {
		t.Fatal(out, err)
	}
	if out, err := s.UpdateDocument(ctx, "notes", "", op("same"), nil, ViaMCP); err != nil || out.Changed {
		t.Fatal(out, err)
	}
	for find, want := range map[string]string{"conflict": "edit_conflict", "invalid": "invalid", "capacity": "document_capacity", "boom": "unavailable", "rolledback": "unavailable", "missing": "document_not_found"} {
		_, err := s.UpdateDocument(ctx, "notes", "", op(find), ptr(strings.Repeat("a", 64)), ViaMCP)
		if ErrorCategory(err) != want {
			t.Fatal(find, err)
		}
		if want == "edit_conflict" && !strings.Contains(err.Error(), "op 1: conflict "+strings.Repeat("a", 64)) {
			t.Fatal("app message or if_hash lost", err)
		}
		if find == "boom" && !strings.Contains(err.Error(), "may or may not have applied") || find == "rolledback" && !strings.Contains(err.Error(), "nothing changed") {
			t.Fatal("outcome of a failed edit misstated", err)
		}
	}
	// Only changing edits are logged, with a summary and never the text.
	events, err := s.Events(ctx, "notes", "document", 0, 10)
	if err != nil || len(events) != 2 || events[0].Message != "live edit of index.md via mcp: 1 op(s) (replace at line 3 -4/+9); seq 7 -> 8; now 42 bytes" {
		t.Fatal(events, err)
	}
	// Public flats carry the live public notice.
	f, err := s.st.GetFlat(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	f.Visibility = store.Public
	if err := s.st.UpdateFlat(ctx, f); err != nil {
		t.Fatal(err)
	}
	if out, err := s.UpdateDocument(ctx, "notes", "", op("ok"), nil, ViaMCP); err != nil || out.PublicNotice != LiveEditPublicNotice {
		t.Fatal(out, err)
	}
}

func TestUpdateDocumentRateLimit(t *testing.T) {
	s, _ := newTestService(t)
	s.cfg.Runtime = &contentRuntime{}
	s.cfg.DocsApp = testDocsApp()
	ctx := context.Background()
	saveDocs(t, s, "notes")
	if _, err := approvedInternalDeploy(t, s, ctx, "notes", 0); err != nil {
		t.Fatal(err)
	}
	with := "x"
	limited := 0
	for range 3 * documentEditRate {
		if _, err := s.UpdateDocument(ctx, "notes", "", []DocumentEditOp{{Op: "replace", Find: "ok", With: &with}}, nil, ViaMCP); errors.Is(err, ErrUnavailable) {
			limited++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if limited == 0 || limited > documentEditRate+1 {
		t.Fatal("rate limit", limited)
	}
}

func TestEditSummaryIsBounded(t *testing.T) {
	e := DocumentEdit{Doc: "a.md", SeqBefore: 1, Seq: 2, Bytes: 3}
	for range 12 {
		e.Ops = append(e.Ops, DocumentEditChange{Op: "insert", Line: 1, Inserted: 2})
	}
	got := editSummary(e, ViaMCP)
	if strings.Count(got, "insert at line") != 8 || !strings.Contains(got, ", …); seq 1 -> 2; now 3 bytes") {
		t.Fatal(got)
	}
}

func ptr[T any](v T) *T { return &v }

// Explicit empty guards reach the docs app (which refuses them) instead of
// being dropped into an unguarded edit.
func TestUpdateDocumentKeepsExplicitEmptyGuards(t *testing.T) {
	ops := []DocumentEditOp{{Op: "insert", At: "end", Text: "x", Nth: ptr(0)}}
	body, err := json.Marshal(struct {
		Ops    []DocumentEditOp `json:"ops"`
		IfHash *string          `json:"if_hash,omitempty"`
	}{ops, ptr("")})
	if err != nil || !strings.Contains(string(body), `"if_hash":""`) || !strings.Contains(string(body), `"nth":0`) {
		t.Fatal(string(body), err)
	}
}

// A requested but empty outline survives decoding and encoding with its
// zero metadata, so callers can tell it from no outline.
func TestEmptyOutlineRoundTrip(t *testing.T) {
	var d Document
	if err := json.Unmarshal([]byte(`{"format":1,"doc":"index.md","markdown":"","blocks":[],"blocks_total":0,"block_offset":0}`), &d); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d)
	for _, want := range []string{`"blocks":[]`, `"blocks_total":0`, `"block_offset":0`} {
		if !strings.Contains(string(b), want) {
			t.Fatal(string(b))
		}
	}
	b, _ = json.Marshal(Document{Format: 1, Doc: "index.md"})
	if strings.Contains(string(b), "blocks") {
		t.Fatal(string(b))
	}
}
