package core

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/store"
)

// docsVisibilityService runs documents on the fake docs runtime behind a
// lifecycle network that records the public route it opens.
func docsVisibilityService(t *testing.T) (*Service, *lifecycleNetwork) {
	t.Helper()
	s, _ := newTestService(t)
	s.cfg.Runtime, s.cfg.DocsApp = &contentRuntime{}, testDocsApp()
	n := &lifecycleNetwork{policy: "configured", state: "ready"}
	s.cfg.Lifecycle = n
	return s, n
}

func liveVersionOf(t *testing.T, s *Service, slug string) int {
	t.Helper()
	f, err := s.st.GetFlat(t.Context(), slug)
	if err != nil {
		t.Fatal(err)
	}
	return f.LiveVersion
}

func frozenParams(t *testing.T, a *store.Approval) map[string]json.RawMessage {
	t.Helper()
	var p map[string]json.RawMessage
	if err := json.Unmarshal(a.Params, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// Revision model rule 2: a visibility approval of a document covers the
// flat, not one revision. Saves and live edits while it is pending do not
// make it stale, and approving it exposes the content as it is then.
func TestDocsVisibilityApprovalSurvivesContentChanges(t *testing.T) {
	s, n := docsVisibilityService(t)
	ctx := t.Context()
	saveDocs(t, s, "handbook")
	if err := s.SetProviderPermission(ctx, "handbook", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	req, err := s.SetVisibility(ctx, "handbook", store.Public, ViaMCP, "share")
	if err != nil || req.Approval == nil {
		t.Fatalf("request %+v %v", req, err)
	}
	if _, ok := frozenParams(t, req.Approval)["expected_live"]; ok {
		t.Fatalf("document approval froze the live version: %s", req.Approval.Params)
	}
	first := liveVersionOf(t, s, "handbook")

	// A single-document save and a whole-bundle save each run a new
	// internal version.
	if _, err := s.SaveDocument(ctx, "handbook", "index.md", "# changed\n", "", SaveMeta{}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveVersion(ctx, "handbook", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs","name":"Title"}`)}, {Path: "index.md", Data: []byte("# rewritten")}, {Path: "more.md", Data: []byte("# more")}}, SaveMeta{}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	// A live edit changes the text in place.
	with := "edited"
	if _, err := s.UpdateDocument(ctx, "handbook", "", []DocumentEditOp{{Op: "replace", Find: "rewritten", With: &with}}, nil, ViaMCP); err != nil {
		t.Fatal(err)
	}
	last := liveVersionOf(t, s, "handbook")
	if last == first {
		t.Fatal("saves did not run a new internal version; the test proves nothing")
	}

	// The same request while it is pending is the same approval.
	again, err := s.SetVisibility(ctx, "handbook", store.Public, ViaMCP, "share")
	if err != nil || again.Approval == nil || again.Approval.ID != req.Approval.ID {
		t.Fatalf("repeated request %+v %v", again, err)
	}
	if n.public != nil {
		t.Fatal("public route opened before approval")
	}

	a, err := s.Decide(ctx, req.Approval.ID, true)
	if err != nil || a.Status != "approved" {
		t.Fatalf("approval after content changes: %+v %v", a, err)
	}
	f, _ := s.st.GetFlat(ctx, "handbook")
	if f.Visibility != store.Public || f.LiveVersion != last {
		t.Fatalf("flat after approval %+v", f)
	}
	if cur := s.state("handbook").cur.Load(); cur == nil || cur.version.Number != last {
		t.Fatal("approval did not expose the current content")
	}
	if n.public == nil {
		t.Fatal("no public route")
	}
	rec := httptest.NewRecorder()
	n.public.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || rec.Body.String() != "public" {
		t.Fatalf("public route %d %q", rec.Code, rec.Body.String())
	}

	// Back to Private: also not stale after a save.
	toPrivate, err := s.SetVisibility(ctx, "handbook", store.Private, ViaMCP, "")
	if err != nil || toPrivate.Approval == nil {
		t.Fatalf("private request %+v %v", toPrivate, err)
	}
	if _, err := s.SaveDocument(ctx, "handbook", "index.md", "# public edit\n", "", SaveMeta{}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	if liveVersionOf(t, s, "handbook") == last {
		t.Fatal("save did not run a new internal version")
	}
	if a, err := s.Decide(ctx, toPrivate.Approval.ID, true); err != nil || a.Status != "approved" {
		t.Fatalf("private approval %+v %v", a, err)
	}
	if f, _ := s.st.GetFlat(ctx, "handbook"); f.Visibility != store.Private || n.public != nil {
		t.Fatalf("private apply %+v", f)
	}
}

// Approvals stored before this rule froze the document's live version. They
// apply by the new rule, and still check the other frozen state.
func TestLegacyDocsVisibilityApprovalIgnoresLiveVersion(t *testing.T) {
	s, _ := docsVisibilityService(t)
	ctx := t.Context()
	saveDocs(t, s, "legacy-doc")
	if err := s.SetProviderPermission(ctx, "legacy-doc", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	providers, err := s.providerKey(ctx, "legacy-doc")
	if err != nil {
		t.Fatal(err)
	}
	insert := func(id string, live int) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"visibility": "public", "from": "private", "expected_live": live, "providers": providers})
		if err := s.st.InsertApproval(ctx, store.Approval{ID: id, Flat: "legacy-doc", Action: "set_visibility", Params: raw, Status: "pending", Via: "mcp", RequestedAt: s.now()}); err != nil {
			t.Fatal(err)
		}
	}
	insert("legacy-policy", liveVersionOf(t, s, "legacy-doc"))
	insert("legacy-doc-vis", liveVersionOf(t, s, "legacy-doc"))
	if _, err := s.SaveDocument(ctx, "legacy-doc", "index.md", "# changed\n", "", SaveMeta{}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	// Provider permissions stay frozen.
	if err := s.SetProviderPermission(ctx, "legacy-doc", store.ProviderFunnel, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if a, err := s.Decide(ctx, "legacy-policy", true); !errors.Is(err, ErrStaleApproval) || !strings.Contains(a.Result, "provider permissions") {
		t.Fatalf("provider change %+v %v", a, err)
	}
	if err := s.SetProviderPermission(ctx, "legacy-doc", store.ProviderFunnel, false, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if a, err := s.Decide(ctx, "legacy-doc-vis", true); err != nil || a.Status != "approved" {
		t.Fatalf("legacy approval %+v %v", a, err)
	}
}

// Websites and pages keep the Version model: their visibility approval
// covers the live version, and a deploy while it is pending makes it stale.
func TestVersionModelVisibilityApprovalStillFreezesLiveVersion(t *testing.T) {
	for _, typ := range []string{"website", "page"} {
		t.Run(typ, func(t *testing.T) {
			s, _ := newTestService(t)
			s.cfg.Lifecycle = &lifecycleNetwork{policy: "configured", state: "ready"}
			ctx := t.Context()
			save := func(body string) {
				t.Helper()
				files := []bundle.File{{Path: "index.html", Data: []byte(body)}}
				if typ == "page" {
					files = pageFiles(body)
				}
				if _, err := s.SaveVersion(ctx, "versioned", files, SaveMeta{}, ViaAPI); err != nil {
					t.Fatal(err)
				}
				lifecycleApprove(t, s, lifecycleRequest(t, s, "versioned"))
			}
			save("one")
			if err := s.SetProviderPermission(ctx, "versioned", store.ProviderPortal, true, ViaConsole); err != nil {
				t.Fatal(err)
			}
			req, err := s.SetVisibility(ctx, "versioned", store.Public, ViaAPI, "")
			if err != nil || req.Approval == nil {
				t.Fatalf("request %+v %v", req, err)
			}
			if live, ok := frozenParams(t, req.Approval)["expected_live"]; !ok || string(live) != "1" {
				t.Fatalf("frozen params %s", req.Approval.Params)
			}
			save("two")
			if liveVersionOf(t, s, "versioned") != 2 {
				t.Fatal("second version is not live")
			}
			a, err := s.Decide(ctx, req.Approval.ID, true)
			if !errors.Is(err, ErrStaleApproval) || a.Status != "failed" || !strings.Contains(a.Result, "the live version") {
				t.Fatalf("stale %+v %v", a, err)
			}
			if f, _ := s.st.GetFlat(ctx, "versioned"); f.Visibility != store.Private {
				t.Fatal("stale approval changed visibility")
			}
		})
	}
}

// A design's shell version never changes, so design edits never made its
// visibility approval stale; it also freezes no live version now.
func TestDesignVisibilityApprovalSurvivesWrites(t *testing.T) {
	s, _ := newTestService(t)
	s.cfg.Lifecycle = &lifecycleNetwork{policy: "configured", state: "ready"}
	ctx := t.Context()
	newDesign(t, s, "studio")
	if err := s.SetProviderPermission(ctx, "studio", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	live := liveVersionOf(t, s, "studio")
	req, err := s.SetVisibility(ctx, "studio", store.Public, ViaAPI, "")
	if err != nil || req.Approval == nil {
		t.Fatalf("request %+v %v", req, err)
	}
	if _, ok := frozenParams(t, req.Approval)["expected_live"]; ok {
		t.Fatalf("design approval froze the live version: %s", req.Approval.Params)
	}
	if _, err := s.WriteDesign(ctx, "studio", DesignWrite{Files: []DesignFile{{Path: "a.html", Data: []byte("<p>changed</p>")}}}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	if liveVersionOf(t, s, "studio") != live {
		t.Fatal("a design write changed the shell version")
	}
	if a, err := s.Decide(ctx, req.Approval.ID, true); err != nil || a.Status != "approved" {
		t.Fatalf("design approval %+v %v", a, err)
	}
	if f, _ := s.st.GetFlat(ctx, "studio"); f.Visibility != store.Public {
		t.Fatal("design is not public")
	}
}

// A document that has never run cannot become Public, and an approval
// requested for a deleted flat never applies to a new flat of that slug.
func TestDocsVisibilityApprovalKeepsOtherGuarantees(t *testing.T) {
	s, _ := docsVisibilityService(t)
	ctx := t.Context()
	saveDocsDraft(t, s, "never-ran")
	if r, err := s.SetVisibility(ctx, "never-ran", store.Public, ViaMCP, ""); !errors.Is(err, ErrConflict) || r.Approval != nil {
		t.Fatalf("unpublished document made public: %+v %v", r, err)
	}

	saveDocs(t, s, "again")
	if err := s.SetProviderPermission(ctx, "again", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	req, err := s.SetVisibility(ctx, "again", store.Public, ViaMCP, "")
	if err != nil || req.Approval == nil {
		t.Fatalf("request %+v %v", req, err)
	}
	del, err := s.Delete(ctx, "again", ViaMCP, "")
	if err != nil || del.Approval == nil {
		t.Fatalf("delete request %+v %v", del, err)
	}
	if _, err := s.Decide(ctx, del.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.GetApproval(ctx, req.Approval.ID); a.Status != "failed" {
		t.Fatalf("deletion left the visibility approval %s", a.Status)
	}
	// Deletion fails pending approvals best effort, after the flat row is
	// gone. One it missed must still not apply to a later flat of the slug.
	missed := *req.Approval
	missed.ID, missed.Status, missed.IdempotencyKey = "apr-missed", "pending", ""
	if err := s.st.InsertApproval(ctx, missed); err != nil {
		t.Fatal(err)
	}
	later := time.Now().UTC().Add(time.Second)
	s.now = func() time.Time { return later }
	saveDocs(t, s, "again")
	if err := s.SetProviderPermission(ctx, "again", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	a, err := s.Decide(ctx, missed.ID, true)
	if !errors.Is(err, ErrStaleApproval) || !strings.Contains(a.Result, "deleted and created again") {
		t.Fatalf("approval applied to a recreated flat: %+v %v", a, err)
	}
	if f, _ := s.st.GetFlat(ctx, "again"); f.Visibility != store.Private {
		t.Fatal("recreated flat became public")
	}
}
