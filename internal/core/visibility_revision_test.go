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
	if fp := frozenParams(t, req.Approval); fp["expected_live"] != nil || fp["flat_id"] == nil {
		t.Fatalf("document approval must freeze the flat, not the live version: %s", req.Approval.Params)
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

// Approvals stored before flat IDs froze a document's live version and do
// not name the flat. They keep that rule, which also keeps them from
// applying to another flat of the slug, and a new identical request reuses
// one while it still holds.
func TestLegacyDocsVisibilityApprovalKeepsLiveVersionRule(t *testing.T) {
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
	// What the previous release stored, key included.
	insert := func(id string) store.Approval {
		t.Helper()
		params := visibilityParams{Visibility: "public", From: "private", ExpectedLive: ptr(liveVersionOf(t, s, "legacy-doc")), Providers: providers}
		key, raw, err := frozenKey("legacy-doc", "set_visibility", params)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"expected_live"`) || strings.Contains(string(raw), "flat_id") {
			t.Fatalf("legacy params %s", raw)
		}
		a := store.Approval{ID: id, Flat: "legacy-doc", Action: "set_visibility", Params: raw, Status: "pending", Via: "mcp", RequestedAt: s.now(), IdempotencyKey: key}
		if err := s.st.InsertApproval(ctx, a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	insert("legacy-holds")
	same, err := s.SetVisibility(ctx, "legacy-doc", store.Public, ViaMCP, "")
	if err != nil || same.Approval == nil || same.Approval.ID != "legacy-holds" || same.Notice == "" {
		t.Fatalf("identical request did not reuse the legacy approval: %+v %v", same, err)
	}
	if a, err := s.Decide(ctx, "legacy-holds", true); err != nil || a.Status != "approved" {
		t.Fatalf("legacy approval with its live version %+v %v", a, err)
	}
	back, err := s.SetVisibility(ctx, "legacy-doc", store.Private, ViaMCP, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, back.Approval.ID, true); err != nil {
		t.Fatal(err)
	}

	insert("legacy-stale")
	if _, err := s.SaveDocument(ctx, "legacy-doc", "index.md", "# changed\n", "", SaveMeta{}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.SetVisibility(ctx, "legacy-doc", store.Public, ViaMCP, "")
	if err != nil || fresh.Approval == nil || fresh.Approval.ID == "legacy-stale" {
		t.Fatalf("request after a save reused a stale legacy approval: %+v %v", fresh, err)
	}
	if a, err := s.Decide(ctx, "legacy-stale", true); !errors.Is(err, ErrStaleApproval) || !strings.Contains(a.Result, "the live version") {
		t.Fatalf("legacy approval after a save %+v %v", a, err)
	}
	if a, err := s.Decide(ctx, fresh.Approval.ID, true); err != nil || a.Status != "approved" {
		t.Fatalf("new approval %+v %v", a, err)
	}
}

// A rename moves a pending legacy approval to the new slug with its old key.
// A request on a flat created later under the old slug must not reuse it:
// approving that would change the renamed document.
func TestLegacyDocsVisibilityApprovalNotReusedAcrossRename(t *testing.T) {
	s, _ := docsVisibilityService(t)
	ctx := t.Context()
	saveDocs(t, s, "orig")
	if err := s.SetProviderPermission(ctx, "orig", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	providers, err := s.providerKey(ctx, "orig")
	if err != nil {
		t.Fatal(err)
	}
	live := liveVersionOf(t, s, "orig")
	params := visibilityParams{Visibility: "public", From: "private", ExpectedLive: ptr(live), Providers: providers}
	key, raw, err := frozenKey("orig", "set_visibility", params)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.InsertApproval(ctx, store.Approval{ID: "legacy-renamed", Flat: "orig", Action: "set_visibility", Params: raw, Status: "pending", Via: "mcp", RequestedAt: s.now(), IdempotencyKey: key}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameSlug(ctx, "orig", "moved", ViaMCP); err != nil {
		t.Fatal(err)
	}
	if a, err := s.GetApproval(ctx, "legacy-renamed"); err != nil || a.Flat != "moved" || a.IdempotencyKey != key {
		t.Fatalf("rename did not move the approval with its key: %+v %v", a, err)
	}
	// After the redirect window the old slug can be used again.
	later := time.Now().UTC().Add(s.redirectWindow() + time.Hour)
	s.now = func() time.Time { return later }
	s.syncRedirects(ctx)
	saveDocs(t, s, "orig")
	if err := s.SetProviderPermission(ctx, "orig", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.providerKey(ctx, "orig"); again != providers || liveVersionOf(t, s, "orig") != live {
		t.Fatal("the recreated flat does not reproduce the legacy key; the test proves nothing")
	}
	req, err := s.SetVisibility(ctx, "orig", store.Public, ViaMCP, "")
	if err != nil || req.Approval == nil || req.Approval.ID == "legacy-renamed" || req.Approval.Flat != "orig" {
		t.Fatalf("request on the recreated slug reused the renamed flat's approval: %+v %v", req, err)
	}
	if a, err := s.Decide(ctx, req.Approval.ID, true); err != nil || a.Status != "approved" {
		t.Fatalf("new approval %+v %v", a, err)
	}
	if f, _ := s.st.GetFlat(ctx, "orig"); f.Visibility != store.Public {
		t.Fatal("recreated flat is not public")
	}
	if f, _ := s.st.GetFlat(ctx, "moved"); f.Visibility != store.Private {
		t.Fatal("the renamed document changed visibility")
	}
	if a, _ := s.GetApproval(ctx, "legacy-renamed"); a.Status != "pending" || a.Flat != "moved" {
		t.Fatalf("renamed flat's approval %+v", a)
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
			if fp := frozenParams(t, req.Approval); string(fp["expected_live"]) != "1" || fp["flat_id"] != nil {
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
	if fp := frozenParams(t, req.Approval); fp["expected_live"] != nil || fp["flat_id"] == nil {
		t.Fatalf("design approval must freeze the flat, not the live version: %s", req.Approval.Params)
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
// requested for a deleted flat never applies to a new flat of that slug,
// even one created in the same millisecond, while a rename keeps it valid.
func TestDocsVisibilityApprovalKeepsOtherGuarantees(t *testing.T) {
	s, _ := docsVisibilityService(t)
	ctx := t.Context()
	fixed := time.Now().UTC().Truncate(time.Millisecond)
	s.now = func() time.Time { return fixed }
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
	// gone, and leaves a claimed one alone. Such an approval must still not
	// apply to a later flat of the slug.
	missed := *req.Approval
	missed.ID, missed.Status, missed.IdempotencyKey = "apr-missed", "pending", ""
	if err := s.st.InsertApproval(ctx, missed); err != nil {
		t.Fatal(err)
	}
	saveDocs(t, s, "again")
	if err := s.SetProviderPermission(ctx, "again", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if f, _ := s.st.GetFlat(ctx, "again"); !f.CreatedAt.Equal(req.Approval.RequestedAt) {
		t.Fatal("the recreated flat is not in the request's millisecond; the test proves less")
	}
	a, err := s.Decide(ctx, missed.ID, true)
	if !errors.Is(err, ErrStaleApproval) || !strings.Contains(a.Result, "deleted and created again") {
		t.Fatalf("approval applied to a recreated flat: %+v %v", a, err)
	}
	if f, _ := s.st.GetFlat(ctx, "again"); f.Visibility != store.Private {
		t.Fatal("recreated flat became public")
	}

	// A rename moves the approval with the flat and keeps it valid.
	req, err = s.SetVisibility(ctx, "again", store.Public, ViaMCP, "")
	if err != nil || req.Approval == nil {
		t.Fatalf("request %+v %v", req, err)
	}
	if _, err := s.RenameSlug(ctx, "again", "renamed", ViaMCP); err != nil {
		t.Fatal(err)
	}
	if a, err := s.Decide(ctx, req.Approval.ID, true); err != nil || a.Status != "approved" {
		t.Fatalf("approval after a rename %+v %v", a, err)
	}
	if f, _ := s.st.GetFlat(ctx, "renamed"); f.Visibility != store.Public {
		t.Fatal("renamed flat is not public")
	}
}
