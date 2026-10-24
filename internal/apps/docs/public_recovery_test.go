package docs

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

// The documents of the recovery test: one with a Mermaid block and a link,
// one plain.
const (
	recoveryIndex = "# Plan\n\nSee [the guide](https://example.com/guide) first.\n\n```mermaid\ngraph TD\n  A[Start] --> B[Done]\n```\n\nBase line\n"
	recoveryNotes = "# Notes\n\nFirst note\nSecond note\n"
)

var recoveryDocs = []string{"index.md", "notes.md"}

// recoveryService runs the real core service with the real document worker
// for a document flat "plan" holding recoveryDocs, Public on Portal at the
// view level.
func recoveryService(t *testing.T) (*core.Service, *captureNet, *captureNet) {
	t.Helper()
	ctx := t.Context()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "host.db"))
	if err != nil {
		t.Fatal(err)
	}
	private, public := &captureNet{handlers: map[string]http.Handler{}}, &captureNet{handlers: map[string]http.Handler{}}
	s, err := core.New(ctx, core.Config{DataDir: dir, Store: st, Private: private, Public: public,
		Runtime: manager(t), DocsApp: core.DocsApp{FS: FS(), Hash: Hash(), Entry: Entry, ContentModule: ContentModule},
		Logf: t.Logf})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); st.Close() })
	files := []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)},
		{Path: "index.md", Data: []byte(recoveryIndex)}, {Path: "notes.md", Data: []byte(recoveryNotes)}}
	if _, err := s.SaveVersion(ctx, "plan", files, core.SaveMeta{}, core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProviderPermission(ctx, "plan", store.ProviderPortal, true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	res, err := s.SetVisibilityAccess(ctx, "plan", store.Public, store.AccessView, core.ViaMCP, "test")
	approveAction(t, s, res, err)
	return s, private, public
}

// liveTexts is every recovery document's live text, read through core as
// get_document reads it.
func liveTexts(t *testing.T, s *core.Service) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, doc := range recoveryDocs {
		d, err := s.GetDocument(t.Context(), "plan", doc)
		if err != nil {
			t.Fatal(doc, err)
		}
		out[doc] = d.Markdown
	}
	return out
}

// revisionTexts is every recovery document's text in revision n.
func revisionTexts(t *testing.T, s *core.Service, n int) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, doc := range recoveryDocs {
		b, _, err := s.RevisionFileContent(t.Context(), "plan", n, doc)
		if err != nil {
			t.Fatal(n, doc, err)
		}
		out[doc] = string(b)
	}
	return out
}

// revisionFileSet is revision n's files as path to hash, without guest
// line marks.
func revisionFileSet(t *testing.T, s *core.Service, n int) map[string]string {
	t.Helper()
	_, files, err := s.GetRevision(t.Context(), "plan", n)
	if err != nil {
		t.Fatal(n, err)
	}
	out := map[string]string{}
	for _, f := range files {
		out[f.Path] = f.Hash
	}
	return out
}

func sameTexts(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	for _, doc := range recoveryDocs {
		if want[doc] == "" {
			t.Fatalf("%s: the expected %s is empty", what, doc)
		}
		if got[doc] != want[doc] {
			t.Fatalf("%s: %s\n got  %q\n want %q", what, doc, got[doc], want[doc])
		}
	}
}

// pageRead reads a document through a route handler as its page does; core
// sets the route's access headers.
func pageRead(t *testing.T, h http.Handler, doc string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://plan.localhost/_docs/api/document?doc="+doc, nil))
	var d core.Document
	if err := json.Unmarshal(rec.Body.Bytes(), &d); rec.Code != 200 || err != nil {
		t.Fatal("page read", doc, rec.Code, rec.Body.String(), err)
	}
	return d.Markdown
}

// history calls the document's History route on the Private route, as its
// History panel does.
func history(t *testing.T, h http.Handler, method, path, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(method, "http://plan.localhost/_docs/api/revisions"+path, strings.NewReader(body))
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Flats-Docs", "1")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); rec.Code != 200 || err != nil {
		t.Fatalf("%s %s = %d %s %v", method, path, rec.Code, rec.Body.String(), err)
	}
	return out
}

// listed is History's page of revisions by number.
func listed(t *testing.T, h http.Handler) map[int]map[string]any {
	t.Helper()
	out := map[int]map[string]any{}
	for _, raw := range history(t, h, "GET", "?limit=100", "")["revisions"].([]any) {
		r := raw.(map[string]any)
		out[int(r["number"].(float64))] = r
	}
	return out
}

func revisionsAfter(t *testing.T, s *core.Service, after int) []store.Revision {
	t.Helper()
	page, err := s.ListRevisions(t.Context(), "plan", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Revision
	for _, r := range page.Revisions {
		if r.Number > after {
			out = append(out, r)
		}
	}
	return out
}

// wipe joins doc on the Public route at base as a guest named name and
// deletes every character of its text with one update covering each
// client's whole clock range in the server's state vector.
func wipe(t *testing.T, base, doc, name string, id int) *socket {
	t.Helper()
	g := mustConnect(t, base, doc, nil)
	g.send(t, map[string]any{"t": "hello", "v": 1, "doc": doc, "sv": "AA==", "guest": map[string]any{"name": name}})
	w := g.next(t, "welcome")
	if me, _ := w["me"].(map[string]any); me["kind"] != "guest" || w["access"] != "edit" || w["readonly"] != false {
		t.Fatalf("guest welcome on %s: %v", doc, w)
	}
	sv, _ := w["sv"].(string)
	if sv == "" {
		t.Fatalf("guest welcome without a state vector: %v", w)
	}
	var dels []yDel
	for c, clock := range stateVector(t, sv) {
		if clock > 0 {
			dels = append(dels, yDel{c, 0, clock})
		}
	}
	if len(dels) == 0 {
		t.Fatalf("%s has no text to delete", doc)
	}
	acked(t, g.update(t, fmt.Sprintf("wipe-%d", id), yUpdate(nil, dels)))
	return g
}

// sees reads x's frames until an update carries text: a restore sends its
// text to connected pages as one inserted string.
func sees(t *testing.T, x *socket, what, text string) {
	t.Helper()
	for {
		u := x.next(t, "update")
		b, err := base64.StdEncoding.DecodeString(u["u"].(string))
		if err != nil {
			t.Fatal(what, err)
		}
		if bytes.Contains(b, []byte(text)) {
			return
		}
	}
}

// TestDocsPublicEditRecoveryAfterHostileGuests is the Recovery criterion of
// docs/spec/docs-public-edit.md with the real core and the real document
// worker: guests on the Public route delete all text of every document;
// History lists the pinned safe point and the revision from just before
// the session, and the operator restores each of them exactly, first while
// anyone can still edit and again after Stop guest editing.
func TestDocsPublicEditRecoveryAfterHostileGuests(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, private, public := recoveryService(t)
	priv := httptest.NewServer(private.handler("plan"))
	defer priv.Close()

	// A member edits before anyone can edit.
	m := mustConnect(t, priv.URL, "notes.md", nil)
	m.send(t, map[string]any{"t": "hello", "v": 1, "doc": "notes.md", "sv": "AA==", "name": "Page name", "nick": "Pat", "editor": "recovery-member-editor-1"})
	m.next(t, "welcome")
	acked(t, m.update(t, "m1", yInsert(4100, "Member line before sharing\n")))

	// Anyone can edit: the approval closes the member's group and records
	// the pinned safe point.
	res, err := s.SetVisibilityAccess(ctx, "plan", store.Public, store.AccessEdit, core.ViaConsole, "test")
	approveAction(t, s, res, err)
	if f, err := s.GetFlat(ctx, "plan"); err != nil || f.PublicAccess != store.AccessEdit {
		t.Fatalf("level after the approval: %+v %v", f.PublicAccess, err)
	}
	pubHandler := public.handler("plan")
	if pubHandler == nil {
		t.Fatal("no Public route")
	}
	pub := httptest.NewServer(pubHandler)
	defer pub.Close()
	sp := 0
	for n, r := range listed(t, private.handler("plan")) {
		if r["safe_point"] == true {
			if sp != 0 {
				t.Fatalf("two safe points: %d and %d", sp, n)
			}
			sp = n
		}
	}
	if sp == 0 {
		t.Fatal("History marks no safe point")
	}
	wantSafe := revisionTexts(t, s, sp)
	if !strings.Contains(wantSafe["notes.md"], "Member line before sharing") || wantSafe["index.md"] != recoveryIndex {
		t.Fatalf("safe point texts %q", wantSafe)
	}

	// Work after the safe point: the member edits again and an agent
	// update closes the member's group, so the revision just before the
	// guests' session differs from the safe point.
	acked(t, m.update(t, "m2", yAfter(4100, 27, "Member line after sharing\n")))
	with := "Base line, updated by the agent"
	if _, err := s.UpdateDocument(ctx, "plan", "index.md", []core.DocumentEditOp{{Op: "replace", Find: "Base line", With: &with}}, nil, core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	after := revisionsAfter(t, s, sp)
	if len(after) != 2 || after[0].Kind != "agent" || after[1].Kind != "person" || len(after[1].Authors) == 0 || len(after[1].GuestAuthors) != 0 {
		t.Fatalf("revisions after the safe point %+v", after)
	}
	member, before := after[1].Number, after[0].Number
	wantBefore := revisionTexts(t, s, before)
	sameTexts(t, "live text before the session", liveTexts(t, s), wantBefore)
	if wantBefore["index.md"] == wantSafe["index.md"] || wantBefore["notes.md"] == wantSafe["notes.md"] {
		t.Fatalf("the revision before the session does not differ from the safe point in every document: %q %q", wantBefore, wantSafe)
	}
	safeFiles, beforeFiles := revisionFileSet(t, s, sp), revisionFileSet(t, s, before)
	m.Close()
	// A member keeps notes.md open through the session.
	watcher := mustConnect(t, priv.URL, "notes.md", nil)
	watcher.send(t, map[string]any{"t": "hello", "v": 1, "doc": "notes.md", "sv": "AA==", "name": "Page name", "nick": "Kim", "editor": "recovery-member-editor-2"})
	watcher.next(t, "welcome")

	// The hostile session: two guests delete all text of every document.
	g1 := wipe(t, pub.URL, "index.md", "Mallory", 1)
	g2 := wipe(t, pub.URL, "notes.md", "Eve", 2)
	for doc, text := range liveTexts(t, s) {
		if text != "" {
			t.Fatalf("%s keeps text after the hostile session: %q", doc, text)
		}
	}

	// History, while anyone can still edit, lists the safe point (pinned
	// and marked), the member's revision and the one just before the
	// session.
	l := listed(t, private.handler("plan"))
	if r := l[sp]; r == nil || r["pinned"] != true || r["safe_point"] != true || r["kind"] != "safe_point" {
		t.Fatalf("safe point %d in History: %v", sp, r)
	}
	if l[before] == nil || l[member] == nil {
		t.Fatalf("History lists %v, want revisions %d and %d", l, member, before)
	}

	// The operator restores the revision from just before the session from
	// History. The rollback first records the guests' open group.
	out := history(t, private.handler("plan"), "POST", fmt.Sprintf("/%d/rollback", before), `{}`)
	restored := int(out["revision"].(float64))
	sameTexts(t, "live text after restoring the revision before the session", liveTexts(t, s), wantBefore)
	sameTexts(t, "rollback revision", revisionTexts(t, s, restored), wantBefore)
	if got := revisionFileSet(t, s, restored); !maps.Equal(got, beforeFiles) {
		t.Fatalf("rollback revision files %v, want %v", got, beforeFiles)
	}
	// Pages still connected receive the restored text: the guests' and the
	// member's.
	sees(t, g1, "guest on index.md", wantBefore["index.md"])
	sees(t, g2, "guest on notes.md", wantBefore["notes.md"])
	sees(t, watcher, "member on notes.md", wantBefore["notes.md"])
	for _, doc := range recoveryDocs {
		if got := pageRead(t, private.handler("plan"), doc); got != wantBefore[doc] {
			t.Fatalf("a fresh member read of %s after the restore: %q", doc, got)
		}
		if got := pageRead(t, public.handler("plan"), doc); got != wantBefore[doc] {
			t.Fatalf("a fresh public read of %s after the restore: %q", doc, got)
		}
	}
	guestRevisions := func(from, to int) []store.Revision {
		t.Helper()
		var out []store.Revision
		for _, r := range revisionsAfter(t, s, from) {
			if r.Number < to && len(r.GuestAuthors) > 0 {
				out = append(out, r)
			}
		}
		return out
	}
	checkGuestSession := func(what string, revs []store.Revision, names ...string) {
		t.Helper()
		credited := map[string]bool{}
		empty := false
		for _, r := range revs {
			if !r.GuestOnly || len(r.Authors) != 0 {
				t.Fatalf("%s: guest revision %+v credits a member", what, r)
			}
			for _, n := range r.GuestAuthors {
				credited[n] = true
			}
			texts := revisionTexts(t, s, r.Number)
			if texts["index.md"] == "" && texts["notes.md"] == "" {
				empty = true
			}
		}
		for _, n := range names {
			if !credited[n] {
				t.Fatalf("%s: %q is not credited in %+v", what, n, revs)
			}
		}
		if !empty {
			t.Fatalf("%s: no guest revision holds the emptied documents: %+v", what, revs)
		}
	}
	checkGuestSession("first session", guestRevisions(before, restored), "Mallory", "Eve")
	l = listed(t, private.handler("plan"))
	if l[sp] == nil || l[sp]["pinned"] != true || l[before] == nil || l[restored] == nil || l[restored]["kind"] != "rollback" {
		t.Fatalf("History after the first restore: %v", l)
	}
	for _, g := range []*socket{g1, g2} {
		g.Close()
	}

	// A second hostile session, then Stop guest editing: its forced
	// boundary records the guests' group, and the operator restores the
	// safe point.
	wipe(t, pub.URL, "index.md", "Mallory", 3)
	wipe(t, pub.URL, "notes.md", "Mallory", 4)
	for doc, text := range liveTexts(t, s) {
		if text != "" {
			t.Fatalf("%s keeps text after the second hostile session: %q", doc, text)
		}
	}
	res, err = s.SetVisibilityAccess(ctx, "plan", store.Public, store.AccessView, core.ViaConsole, "test")
	approveAction(t, s, res, err)
	l = listed(t, private.handler("plan"))
	if l[sp] == nil || l[sp]["pinned"] != true || l[before] == nil {
		t.Fatalf("History after Stop guest editing: %v", l)
	}
	// History reads reach the worker at most every two seconds, so the
	// group Stop recorded is listed within a few seconds; far sooner than
	// the 30-second idle close, so it is Stop's forced boundary.
	var second []store.Revision
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if second = guestRevisions(restored, 1<<30); len(second) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("History lists no revision of the second guest session after Stop guest editing")
		}
	}
	checkGuestSession("second session", second, "Mallory")
	out = history(t, private.handler("plan"), "POST", fmt.Sprintf("/%d/rollback", sp), `{}`)
	final := int(out["revision"].(float64))
	sameTexts(t, "live text after restoring the safe point", liveTexts(t, s), wantSafe)
	sameTexts(t, "safe point rollback revision", revisionTexts(t, s, final), wantSafe)
	if got := revisionFileSet(t, s, final); !maps.Equal(got, safeFiles) {
		t.Fatalf("safe point rollback files %v, want %v", got, safeFiles)
	}
	for _, doc := range recoveryDocs {
		if got := pageRead(t, private.handler("plan"), doc); got != wantSafe[doc] {
			t.Fatalf("a fresh member read of %s after restoring the safe point: %q", doc, got)
		}
	}
	if rev, _, err := s.GetRevision(ctx, "plan", sp); err != nil || !rev.Pinned {
		t.Fatalf("safe point %d after recovery: %+v %v", sp, rev, err)
	}
}
