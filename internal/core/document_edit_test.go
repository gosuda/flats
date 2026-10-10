package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/store"
)

// committedEdits are receipt ids the fake app committed; receiptsDown makes
// its receipt lookup fail.
var (
	committedEdits sync.Map
	receiptsDown   atomic.Bool
	receiptsStall  atomic.Bool
)

// receiptResponse is the fake docs app's host-only receipt lookup.
func receiptResponse(w http.ResponseWriter, r *http.Request) {
	if receiptsStall.Load() {
		<-r.Context().Done()
		return
	}
	if r.Header.Get("X-Flats-Host-Op") != "edit" || receiptsDown.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if v, ok := committedEdits.Load(r.URL.Query().Get("id")); ok {
		_ = json.NewEncoder(w).Encode(map[string]any{"committed": true, "seq": v})
		return
	}
	_, _ = io.WriteString(w, `{"committed":false}`)
}

// editResponse is the fake docs app's edit route: the find text selects the
// outcome, so the host's routing and error mapping are tested in isolation.
func editResponse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("X-Flats-Access") != "private" || r.Header.Get("X-Flats-Host-Op") != "edit" {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"code":"readonly","message":"host only"}`)
		return
	}
	var in struct {
		Ops      []DocumentEditOp `json:"ops"`
		IfHash   string           `json:"if_hash"`
		ID       string           `json:"id"`
		Deadline int64            `json:"deadline_ms"`
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
	case "lost", "dark":
		// Committed, then the response was lost.
		committedEdits.Store(in.ID, 9)
		status = 502
	case "late":
		// The response is lost; the worker commits shortly afterwards,
		// still before the edit's deadline.
		go func(id string) {
			time.Sleep(50 * time.Millisecond)
			committedEdits.Store(id, 9)
		}(in.ID)
		status = 502
	case "garbled":
		committedEdits.Store(in.ID, 9)
		_, _ = io.WriteString(w, "{not json")
		return
	}
	if !strings.HasPrefix(in.ID, "agent:") || len(in.ID) != len("agent:")+32 || in.Deadline <= time.Now().UnixMilli() {
		w.WriteHeader(http.StatusBadRequest)
		return
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
	window, margin := editCommitWindow, editSettleMargin
	editCommitWindow, editSettleMargin = 300*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { editCommitWindow, editSettleMargin = window, margin })
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
	for find, want := range map[string]string{"conflict": "edit_conflict", "invalid": "invalid", "capacity": "document_capacity", "boom": "unavailable", "rolledback": "unavailable", "missing": "document_not_found", "lost": "unavailable", "garbled": "unavailable", "late": "unavailable"} {
		_, err := s.UpdateDocument(ctx, "notes", "", op(find), ptr(strings.Repeat("a", 64)), ViaMCP)
		if ErrorCategory(err) != want {
			t.Fatal(find, err)
		}
		if want == "edit_conflict" && !strings.Contains(err.Error(), "op 1: conflict "+strings.Repeat("a", 64)) {
			t.Fatal("app message or if_hash lost", err)
		}
		wantText := map[string]string{"boom": "did not apply and can no longer commit", "rolledback": "nothing changed", "lost": "applied at seq 9", "garbled": "applied at seq 9", "late": "applied at seq 9"}[find]
		if wantText != "" && !strings.Contains(err.Error(), wantText) {
			t.Fatal("outcome of a failed edit misstated", find, err)
		}
	}
	// A lost response whose receipt cannot be checked is reported as unknown.
	receiptsDown.Store(true)
	if _, err := s.UpdateDocument(ctx, "notes", "", op("dark"), nil, ViaMCP); !strings.Contains(fmt.Sprint(err), "may or may not have applied") {
		t.Fatal(err)
	}
	receiptsDown.Store(false)
	// A receipt lookup that runs out of time still leaves its warning.
	receiptsStall.Store(true)
	receiptLookupTimeout = 50 * time.Millisecond
	if _, err := s.UpdateDocument(ctx, "notes", "", op("dark"), nil, ViaMCP); !strings.Contains(fmt.Sprint(err), "may or may not have applied") {
		t.Fatal(err)
	}
	receiptsStall.Store(false)
	receiptLookupTimeout = 5 * time.Second
	// Uncertain outcomes are always logged, with what the receipt showed.
	events, err := s.Events(ctx, "notes", "document", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var warned []string
	for _, e := range events {
		if e.Level == "warn" {
			warned = append(warned, e.Message)
		}
	}
	joined := strings.Join(warned, "\n")
	if len(warned) != 6 || strings.Count(joined, "committed at seq 9, but its response was lost") != 3 || !strings.Contains(joined, "did not apply (docs app returned HTTP 500; no receipt after its commit deadline)") || strings.Count(joined, "outcome unknown") != 2 {
		t.Fatal(warned)
	}
	// Changing edits are logged with a summary and never the text.
	events, err = s.Events(ctx, "notes", "document", 0, 20)
	var infos []store.Event
	for _, e := range events {
		if e.Level == "info" {
			infos = append(infos, e)
		}
	}
	events = infos
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
	// A lost response on a Public flat still says the change may be public.
	_, err = s.UpdateDocument(ctx, "notes", "", op("lost"), nil, ViaMCP)
	var outcome *EditOutcomeError
	if !errors.As(err, &outcome) || outcome.Outcome != EditApplied || outcome.Seq != 9 || !strings.Contains(err.Error(), LiveEditPublicNotice) || ErrorCategory(err) != "unavailable" {
		t.Fatal(err)
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
