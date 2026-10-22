package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/board"
	"github.com/gosuda/flats/internal/store"
)

// Anonymous public editing of documents, core side
// (docs/spec/docs-public-edit-TECH.md 2). fakeDocsWorker answers the host
// operations of TECH 3.6 with the request and response shapes the document
// worker implements, so approval, import, pinning and level changes run
// through the real Service and store. The real worker's behavior for these
// operations is tested in internal/apps/docs.

// accessCall is one access host operation the fake worker received.
type accessCall struct {
	Level string `json:"level"`
	Epoch int64  `json:"epoch"`
}

// fakeDocsWorker is the host side of a running document worker.
type fakeDocsWorker struct {
	mu      sync.Mutex
	seq     int64
	entries []docsEntry
	texts   map[string]string
	// source and docs are the current content: its source and documents.
	source string
	docs   []store.RevisionFile
	// dirty is an edit not recorded yet, by guests when guests is set.
	dirty  bool
	guests []string
	// access lists the access operations received; failAccess refuses them.
	access     []accessCall
	failAccess bool
	safePoints int
	// afterSafePoint, when set, runs after a new safe point entry, as a
	// member group closing right after it.
	afterSafePoint func()
	presence       *docsPresence
	// headers are the public access headers of the last page request.
	headers http.Header
	// document is what the document read returns.
	document Document
}

func (f *fakeDocsWorker) record(kind string, authors, guests []string) {
	f.seq++
	f.entries = append(f.entries, docsEntry{Seq: f.seq, CreatedAt: time.Now().UnixMilli(), Kind: kind, Authors: authors, Guests: guests,
		Source: f.source, Documents: slices.Clone(f.docs)})
	f.dirty, f.guests = false, nil
}

// edit changes a document's text as a person or guest would, without
// recording it yet. guestLines marks the lines guests wrote.
func (f *fakeDocsWorker) edit(path, text string, guest string, guestLines [][2]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.editLocked(path, text, guestLines)
	if guest != "" {
		f.guests = append(f.guests, guest)
	}
}

// editLocked is edit for a caller holding f.mu.
func (f *fakeDocsWorker) editLocked(path, text string, guestLines [][2]int) {
	h := hashBytes([]byte(text))
	f.texts[h] = text
	for i := range f.docs {
		if f.docs[i].Path == path {
			f.docs[i] = store.RevisionFile{Path: path, Hash: h, Size: int64(len(text)), GuestLines: &guestLines}
		}
	}
	f.dirty = true
}

func (f *fakeDocsWorker) lastAccess() accessCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.access) == 0 {
		return accessCall{Level: "<none>"}
	}
	return f.access[len(f.access)-1]
}

func (f *fakeDocsWorker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	if op, ok := strings.CutPrefix(r.URL.Path, "/_docs/host/revisions/"); ok {
		if r.Header.Get("X-Flats-Access") != "private" || r.Header.Get("X-Flats-Host-Op") != "revisions" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch op {
		case "sync":
			reply(map[string]any{"store": "fake-docs", "ready": len(f.entries), "quiescent": true, "presence": f.presence})
		case "outbox":
			after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
			var out []docsEntry
			for _, e := range f.entries {
				if e.Seq > after {
					out = append(out, e)
				}
			}
			reply(map[string]any{"store": "fake-docs", "entries": out, "more": false})
		case "text":
			text, ok := f.texts[r.URL.Query().Get("hash")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			reply(map[string]any{"text": text})
		case "ack":
			var in struct {
				Through int64 `json:"through"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			f.entries = slices.DeleteFunc(f.entries, func(e docsEntry) bool { return e.Seq <= in.Through })
			reply(map[string]any{"ok": true})
		case "access":
			var in accessCall
			_ = json.NewDecoder(r.Body).Decode(&in)
			if f.failAccess {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			f.access = append(f.access, in)
			reply(in)
		case "safe_point":
			f.safePoints++
			var in struct {
				Prev *struct {
					Source    string               `json:"source"`
					Documents []store.RevisionFile `json:"documents"`
				} `json:"prev"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if f.dirty {
				f.record("person", []string{}, f.guests)
			}
			same := in.Prev != nil && in.Prev.Source == f.source && len(in.Prev.Documents) == len(f.docs)
			for i := 0; same && i < len(f.docs); i++ {
				same = in.Prev.Documents[i].Path == f.docs[i].Path && in.Prev.Documents[i].Hash == f.docs[i].Hash
			}
			if same {
				reply(map[string]any{"seq": 0, "reuse": true})
				return
			}
			f.record("safe_point", []string{}, nil)
			seq := f.seq
			if f.afterSafePoint != nil {
				f.afterSafePoint()
			}
			reply(map[string]any{"seq": seq, "reuse": false})
		case "op":
			reply(map[string]any{"committed": false})
		default:
			reply(map[string]any{"ok": true})
		}
		return
	}
	switch r.URL.Path {
	case "/_docs/api/receipt":
		receiptResponse(w, r)
	case "/_docs/api/document":
		reply(f.document)
	default:
		f.headers = http.Header{}
		for _, k := range []string{"X-Flats-Access", board.PublicAccessHeader, AccessEpochHeader} {
			if v := r.Header.Values(k); len(v) > 0 {
				f.headers[k] = v
			}
		}
		_, _ = io.WriteString(w, "document page")
	}
}

// docsWorkerRuntime starts every docs worker on one fakeDocsWorker. With
// managed set, the live worker is a sleeping managed instance: a request
// or host operation that needs it wakes it through the real preparation.
type docsWorkerRuntime struct {
	worker  *fakeDocsWorker
	managed bool
	// instances are the managed instances started, the live one last.
	instances []*idleTestInstance
}

func (rt *docsWorkerRuntime) Start(_ context.Context, spec RuntimeSpec) (Instance, error) {
	if !rt.managed || !spec.ManagedDocs {
		return contentInstance{rt.worker}, nil
	}
	m := &idleTestInstance{Handler: rt.worker}
	m.wake = func(ctx context.Context, prepare func(context.Context, http.Handler) error) (RuntimeLease, error) {
		if err := prepare(ctx, rt.worker); err != nil {
			return nil, err
		}
		m.lease = &idleTestLease{Handler: rt.worker}
		return m.lease, nil
	}
	rt.instances = append(rt.instances, m)
	return m, nil
}

// sleep puts the live managed worker to sleep, as a clean quiescence does.
func (rt *docsWorkerRuntime) sleep(s *Service) {
	rt.instances[len(rt.instances)-1].lease = nil
	s.state("plan").docsPresence.Store(nil)
}

// newPublicDocs starts a service with a Public network and a running
// document "plan" permitted on Portal, whose worker is fake.
func newPublicDocs(t *testing.T, managed bool) (*Service, *fakeDocsWorker, *docsWorkerRuntime) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(context.Background(), Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}},
		Public: &memPublic{memNet{hosts: map[string]http.Handler{}}}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); st.Close() })
	worker := &fakeDocsWorker{texts: map[string]string{}}
	rt := &docsWorkerRuntime{worker: worker, managed: managed}
	s.cfg.Runtime, s.cfg.DocsApp = rt, testDocsApp()
	v := saveDocs(t, s, "plan")
	worker.mu.Lock()
	worker.source = v.Hash
	for _, p := range []string{"a.md", "index.md"} {
		text := "# " + p
		h := hashBytes([]byte(text))
		worker.texts[h] = text
		worker.docs = append(worker.docs, store.RevisionFile{Path: p, Hash: h, Size: int64(len(text))})
	}
	worker.mu.Unlock()
	if err := s.SetProviderPermission(context.Background(), "plan", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	setAccess(t, s, store.Public, store.AccessView)
	return s, worker, rt
}

func revisionsOf(t *testing.T, s *Service) []store.Revision {
	t.Helper()
	revs, err := s.st.ListRevisions(context.Background(), "plan", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return revs
}

func accessSeq(t *testing.T, s *Service) int64 {
	t.Helper()
	f, err := s.st.GetFlat(context.Background(), "plan")
	if err != nil {
		t.Fatal(err)
	}
	return f.AccessSeq
}

// A document becomes editable by anyone only through an approval: the
// approval has the worker record a safe point, imports it pinned, commits
// the level and then tells the worker the level and its epoch. Stopping
// tells it the new pair; enabling again on unchanged text reuses the safe
// point, which stays pinned while the level is edit.
func TestDocsPublicEditApproval(t *testing.T) {
	ctx := context.Background()
	s, worker, _ := newPublicDocs(t, false)
	if v, access := flatAccess(t, s); v != store.Public || access != store.AccessView {
		t.Fatalf("start: %s %q", v, access)
	}
	if fv, _ := s.GetFlat(ctx, "plan"); fv.PublicAccess != store.AccessView || fv.PublicPresence == nil {
		t.Fatalf("a Public document reports its level: %q %+v", fv.PublicAccess, fv.PublicPresence)
	}
	r, err := s.SetVisibilityAccess(ctx, "plan", store.Public, store.AccessEdit, ViaMCP, "")
	if err != nil || r.Status != "pending_approval" || r.Notice != PendingPublicEditDocsNotice {
		t.Fatalf("request: %+v %v", r, err)
	}
	if worker.safePoints != 0 || len(revisionsOf(t, s)) != 0 {
		t.Fatal("a request recorded a safe point")
	}
	if got, err := s.Decide(ctx, r.Approval.ID, true); err != nil || got.Status != "approved" {
		t.Fatalf("approve: %+v %v", got, err)
	}
	sp := safePointRev(t, s)
	revs := revisionsOf(t, s)
	if sp == 0 || len(revs) != 1 || revs[0].Number != sp || revs[0].Kind != "safe_point" || !revs[0].Pinned {
		t.Fatalf("safe point %d, revisions %+v", sp, revs)
	}
	if got := worker.lastAccess(); got != (accessCall{store.AccessEdit, accessSeq(t, s)}) {
		t.Fatalf("worker access = %+v, seq %d", got, accessSeq(t, s))
	}
	fv, err := s.GetFlat(ctx, "plan")
	if err != nil || fv.PublicAccess != store.AccessEdit || fv.PublicNotice != PublicEditDocsNotice {
		t.Fatalf("view: %q %q %v", fv.PublicAccess, fv.PublicNotice, err)
	}
	page, err := s.ListRevisions(ctx, "plan", 0, 100)
	if err != nil || !page.Revisions[0].SafePoint {
		t.Fatalf("list marks the safe point: %+v %v", page.Revisions, err)
	}
	if rev, _, err := s.GetRevision(ctx, "plan", sp); err != nil || !rev.SafePoint {
		t.Fatalf("get marks the safe point: %+v %v", rev, err)
	}
	if err := s.PinRevision(ctx, "plan", sp, false); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "these documents") {
		t.Fatalf("unpin the safe point: %v", err)
	}
	two := 2
	if err := s.SetRevisionQuotas(ctx, "plan", &two, nil); err != nil {
		t.Fatal(err)
	}
	zero := 0
	if err := s.SetRevisionQuotas(ctx, "plan", &zero, nil); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "these documents") {
		t.Fatalf("unlimited revision quota at edit: %v", err)
	}

	// Stop guest editing.
	setAccess(t, s, store.Public, store.AccessView)
	if got := worker.lastAccess(); got != (accessCall{store.AccessView, accessSeq(t, s)}) {
		t.Fatalf("after stop: worker access = %+v, seq %d", got, accessSeq(t, s))
	}
	if err := s.PinRevision(ctx, "plan", sp, false); err != nil {
		t.Fatalf("unpin after stop: %v", err)
	}
	// Enabling again on unchanged text pins the same safe point again.
	setAccess(t, s, store.Public, store.AccessEdit)
	if safePointRev(t, s) != sp || len(revisionsOf(t, s)) != 1 || !slices.Equal(pinnedRevisions(t, s), []int{sp}) {
		t.Fatalf("re-enable: safe point %d, revisions %+v", safePointRev(t, s), revisionsOf(t, s))
	}
	// After an edit the next enable records a new safe point; the edit is
	// recorded first.
	setAccess(t, s, store.Public, store.AccessView)
	worker.edit("index.md", "# changed", "", nil)
	setAccess(t, s, store.Public, store.AccessEdit)
	revs = revisionsOf(t, s)
	if len(revs) != 3 || revs[0].Kind != "safe_point" || revs[1].Kind != "person" || safePointRev(t, s) != revs[0].Number {
		t.Fatalf("enable after an edit: safe point %d, revisions %+v", safePointRev(t, s), revs)
	}
	setAccess(t, s, store.Private, "")
	if got := worker.lastAccess(); got != (accessCall{"", accessSeq(t, s)}) {
		t.Fatalf("after private: worker access = %+v", got)
	}
}

// A pending edit approval of a document fails as stale after visibility
// and level went round to the same pair, and records no safe point.
func TestDocsPublicEditApprovalRoundTripIsStale(t *testing.T) {
	ctx := context.Background()
	s, worker, _ := newPublicDocs(t, false)
	a := requestAccess(t, s, store.Public, store.AccessEdit, ViaMCP)
	setAccess(t, s, store.Private, "")
	setAccess(t, s, store.Public, store.AccessEdit)
	setAccess(t, s, store.Public, store.AccessView)
	calls := worker.safePoints
	got, err := s.Decide(ctx, a.ID, true)
	if !errors.Is(err, ErrStaleApproval) || got.Status != "failed" {
		t.Fatalf("round trip approval = %+v %v", got, err)
	}
	if _, access := flatAccess(t, s); access != store.AccessView || worker.safePoints != calls {
		t.Fatalf("a stale approval changed the document: %q, %d safe point calls", access, worker.safePoints-calls)
	}
}

// Missing quotas fail a document's approval before the worker records
// anything, and a full pin quota without a reusable safe point too.
func TestDocsPublicEditPreflightBlocks(t *testing.T) {
	ctx := context.Background()
	s, worker, _ := newPublicDocs(t, false)
	if _, err := s.UpdateSettings(ctx, map[string]string{SetRevisionQuota: "0"}); err != nil {
		t.Fatal(err)
	}
	status, err := s.PublicEditPreflight(ctx, "plan")
	if err != nil || len(status.Problems) != 1 || status.Problems[0].Code != ProblemRevisionQuotaUnlimited || !strings.Contains(status.Problems[0].Message, "documents'") {
		t.Fatalf("preflight: %+v %v", status, err)
	}
	a := requestAccess(t, s, store.Public, store.AccessEdit, ViaMCP)
	if got, _ := s.Decide(ctx, a.ID, true); got.Status != "failed" {
		t.Fatalf("approval with an unlimited revision quota: %+v", got)
	}
	if _, access := flatAccess(t, s); access != store.AccessView || worker.safePoints != 0 {
		t.Fatalf("blocked approval changed the flat: %q, %d safe points", access, worker.safePoints)
	}
	if _, err := s.UpdateSettings(ctx, map[string]string{SetRevisionQuota: "10", SetPinQuota: "1"}); err != nil {
		t.Fatal(err)
	}
	// With the earlier safe point pinned and every pin in use, the preflight
	// leaves the comparison to the worker; when the text changed, the worker
	// records a new safe point, which is imported unpinned, and the approval
	// fails with pin_quota_full without changing the level.
	setAccess(t, s, store.Public, store.AccessEdit)
	sp := safePointRev(t, s)
	setAccess(t, s, store.Public, store.AccessView)
	worker.edit("a.md", "# other", "", nil)
	worker.mu.Lock()
	worker.record("person", []string{"Pat"}, nil)
	worker.mu.Unlock()
	if err := s.catchUpDocsNow(ctx); err != nil {
		t.Fatal(err)
	}
	status, err = s.PublicEditPreflight(ctx, "plan")
	if err != nil || len(status.Problems) != 0 || status.SafePoint == nil || *status.SafePoint != sp {
		t.Fatalf("preflight with full pins: %+v %v", status, err)
	}
	before := worker.safePoints
	a = requestAccess(t, s, store.Public, store.AccessEdit, ViaMCP)
	got, _ := s.Decide(ctx, a.ID, true)
	if got.Status != "failed" || !strings.Contains(got.Result, "pin") || worker.safePoints != before+1 {
		t.Fatalf("full pins: %+v, safe point calls %d -> %d", got, before, worker.safePoints)
	}
	if _, access := flatAccess(t, s); access != store.AccessView {
		t.Fatalf("a failed approval changed the level to %q", access)
	}
}

// catchUpDocsNow imports what the worker recorded, skipping the history
// read throttle.
func (s *Service) catchUpDocsNow(ctx context.Context) error {
	s.docsCatchUps.Delete("plan")
	return s.catchUpDocs(ctx, "plan")
}

// The safe point is pinned in the import that inserts it, so retention in
// that import keeps it with a revision quota of 1 and a member entry after
// it. When edits since a pinned safe point make the worker record a new one
// and every pin is in use, it is imported unpinned and the approval fails
// with pin_quota_full, leaving the level unchanged.
func TestDocsPublicEditSafePointPinnedInImport(t *testing.T) {
	ctx := context.Background()
	s, worker, _ := newPublicDocs(t, false)
	if _, err := s.UpdateSettings(ctx, map[string]string{SetRevisionQuota: "1", SetPinQuota: "1"}); err != nil {
		t.Fatal(err)
	}
	worker.edit("a.md", "# member", "", nil)
	worker.afterSafePoint = func() {
		worker.editLocked("index.md", "# after", nil)
		worker.record("person", []string{"Pat"}, nil)
	}
	setAccess(t, s, store.Public, store.AccessEdit)
	worker.afterSafePoint = nil
	sp := safePointRev(t, s)
	revs := revisionsOf(t, s)
	if sp == 0 || !slices.Equal(pinnedRevisions(t, s), []int{sp}) || len(revs) != 2 || revs[0].Number != sp+1 || revs[1].Number != sp {
		t.Fatalf("safe point %d pins %v revisions %+v", sp, pinnedRevisions(t, s), revs)
	}
	setAccess(t, s, store.Public, store.AccessView)
	worker.edit("a.md", "# again", "", nil)
	a := requestAccess(t, s, store.Public, store.AccessEdit, ViaMCP)
	got, _ := s.Decide(ctx, a.ID, true)
	if got.Status != "failed" || !strings.Contains(got.Result, "pin") {
		t.Fatalf("approval with every pin in use: %+v", got)
	}
	if _, access := flatAccess(t, s); access != store.AccessView || safePointRev(t, s) != sp || !slices.Equal(pinnedRevisions(t, s), []int{sp}) {
		t.Fatalf("failed approval changed the flat: %q, safe point %d, pins %v", access, safePointRev(t, s), pinnedRevisions(t, s))
	}
}

// Imported entries credit guests apart from members and agents, mark
// guest-only revisions and keep each document's guest lines, which
// get_revision returns for every document.
func TestDocsImportGuestCreditsAndLines(t *testing.T) {
	ctx := context.Background()
	s, worker, _ := newPublicDocs(t, false)
	worker.edit("index.md", "# guest\ntext", "Mina", [][2]int{{1, 2}, {0, 3}})
	worker.mu.Lock()
	worker.record("person", []string{}, []string{"Mina", "Mina", "Jo"})
	worker.entries[len(worker.entries)-1].GuestsMore = 4
	worker.editLocked("a.md", "# member", nil)
	worker.record("person", []string{"Pat"}, []string{"Mina"})
	worker.mu.Unlock()
	if err := s.catchUpDocsNow(ctx); err != nil {
		t.Fatal(err)
	}
	revs := revisionsOf(t, s)
	if len(revs) != 2 {
		t.Fatalf("revisions %+v", revs)
	}
	guest, mixed := revs[1], revs[0]
	if !guest.GuestOnly || !slices.Equal(guest.GuestAuthors, []string{"Mina", "Jo"}) || guest.GuestsMore != 4 || len(guest.Authors) != 0 {
		t.Fatalf("guest revision %+v", guest)
	}
	if mixed.GuestOnly || !slices.Equal(mixed.Authors, []string{"Pat"}) || !slices.Equal(mixed.GuestAuthors, []string{"Mina"}) {
		t.Fatalf("mixed revision %+v", mixed)
	}
	_, files, err := s.GetRevision(ctx, "plan", guest.Number)
	if err != nil {
		t.Fatal(err)
	}
	lines := map[string][][2]int{}
	for _, f := range files {
		if f.GuestLines != nil {
			lines[f.Path] = *f.GuestLines
		}
	}
	if !slices.Equal(lines["index.md"], [][2]int{{1, 2}}) || lines["a.md"] == nil || len(lines["a.md"]) != 0 || lines["images/a.txt"] != nil {
		t.Fatalf("guest lines %v", lines)
	}
	b, _ := json.Marshal(files)
	if !strings.Contains(string(b), `"path":"a.md","hash"`) || !strings.Contains(string(b), `"guest_lines":[]`) || strings.Count(string(b), "guest_lines") != 2 {
		t.Fatalf("revision files JSON %s", b)
	}
}

// get_document passes the worker's guest marks through; a document without
// guest text reports none.
func TestDocsGetDocumentGuestMarks(t *testing.T) {
	ctx := context.Background()
	s, worker, _ := newPublicDocs(t, false)
	worker.mu.Lock()
	worker.document = Document{Format: 1, Doc: "index.md", Markdown: "a\nb\nc", GuestText: true, GuestLines: [][2]int{{2, 3}, {5, 4}}}
	worker.mu.Unlock()
	d, err := s.GetDocument(ctx, "plan", "")
	if err != nil || !d.GuestText || !slices.Equal(d.GuestLines, [][2]int{{2, 3}}) {
		t.Fatalf("document %+v %v", d, err)
	}
	worker.mu.Lock()
	worker.document = Document{Format: 1, Doc: "index.md", Markdown: "a"}
	worker.mu.Unlock()
	d, err = s.GetDocument(ctx, "plan", "")
	b, _ := json.Marshal(d)
	if err != nil || d.GuestText || !strings.Contains(string(b), `"guest_text":false,"guest_lines":[]`) {
		t.Fatalf("document without guests %s %v", b, err)
	}
}

// Public requests of a document carry the level and its epoch; history
// routes stay refused on a Public route at edit, and forged headers are
// replaced.
func TestDocsPublicRequestHeaders(t *testing.T) {
	s, worker, _ := newPublicDocs(t, false)
	setAccess(t, s, store.Public, store.AccessEdit)
	seq := accessSeq(t, s)
	h := s.siteHandler("plan", true)
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Flats-Access", "private")
	r.Header.Set(AccessEpochHeader, "1")
	h.ServeHTTP(httptest.NewRecorder(), r)
	worker.mu.Lock()
	got := worker.headers
	worker.mu.Unlock()
	if got.Get("X-Flats-Access") != "public" || got.Get(board.PublicAccessHeader) != store.AccessEdit || got.Get(AccessEpochHeader) != strconv.FormatInt(seq, 10) {
		t.Fatalf("public headers %v (seq %d)", got, seq)
	}
	for _, p := range []string{"/_docs/api/revisions", "/_docs/api/revisions/1", "/_docs/api/revisions/1/pin"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s on the Public route: %d", p, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	s.siteHandler("plan", false).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	worker.mu.Lock()
	got = worker.headers
	worker.mu.Unlock()
	if got.Get("X-Flats-Access") != "private" || got.Get(board.PublicAccessHeader) != "" || got.Get(AccessEpochHeader) != "" {
		t.Fatalf("private headers %v", got)
	}
}

// A sleeping document is woken for the safe point; a level that drops while
// it sleeps changes only the stored level, which the next wake applies
// before any request reaches the worker.
func TestDocsPublicEditSleepingWorker(t *testing.T) {
	s, worker, rt := newPublicDocs(t, true)
	rt.sleep(s)
	m := rt.instances[len(rt.instances)-1]
	wakes := m.wakes
	setAccess(t, s, store.Public, store.AccessEdit)
	if m.wakes != wakes+1 || safePointRev(t, s) == 0 {
		t.Fatalf("wakes %d -> %d, safe point %d", wakes, m.wakes, safePointRev(t, s))
	}
	if got := worker.lastAccess(); got != (accessCall{store.AccessEdit, accessSeq(t, s)}) {
		t.Fatalf("access after the approval %+v", got)
	}
	rt.sleep(s)
	calls := len(worker.access)
	setAccess(t, s, store.Public, store.AccessView)
	if _, access := flatAccess(t, s); access != store.AccessView || len(worker.access) != calls || m.wakes != wakes+1 {
		t.Fatalf("stop while asleep: %q, %d access calls, %d wakes", access, len(worker.access)-calls, m.wakes-wakes)
	}
	rec := httptest.NewRecorder()
	s.siteHandler("plan", true).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || m.wakes != wakes+2 {
		t.Fatalf("wake by request: %d, wakes %d", rec.Code, m.wakes-wakes)
	}
	if got := worker.lastAccess(); got != (accessCall{store.AccessView, accessSeq(t, s)}) {
		t.Fatalf("wake applied %+v, seq %d", got, accessSeq(t, s))
	}
	// The pair reached the worker before the request did.
	if worker.headers.Get(AccessEpochHeader) != strconv.FormatInt(accessSeq(t, s), 10) {
		t.Fatalf("request headers %v", worker.headers)
	}
}

// A running document that does not take Stop guest editing is restarted;
// the stored level stands.
func TestDocsPublicEditStopRestartsUnresponsiveWorker(t *testing.T) {
	ctx := context.Background()
	s, worker, _ := newPublicDocs(t, false)
	setAccess(t, s, store.Public, store.AccessEdit)
	before := s.currentDeployment("plan")
	worker.mu.Lock()
	worker.failAccess = true
	worker.mu.Unlock()
	a := requestAccess(t, s, store.Public, store.AccessView, ViaConsole)
	got, err := s.Decide(ctx, a.ID, true)
	if err != nil || got.Status != "approved" || !strings.Contains(got.Result, "document restarted") {
		t.Fatalf("stop: %+v %v", got, err)
	}
	if _, access := flatAccess(t, s); access != store.AccessView {
		t.Fatalf("level %q", access)
	}
	if after := s.currentDeployment("plan"); after == nil || after == before {
		t.Fatal("the worker was not restarted")
	}
	if !idleEventContains(t, s, "plan", "did not apply public access") {
		t.Fatal("missing event")
	}
}

// Presence counts come from the worker's sync and are reported while they
// are fresh and the worker is awake; a sleeping worker reports none.
func TestDocsPublicPresence(t *testing.T) {
	ctx := context.Background()
	s, worker, rt := newPublicDocs(t, true)
	now := time.Now()
	s.now = func() time.Time { return now }
	report := func(guests, viewers int) {
		worker.mu.Lock()
		worker.presence = &docsPresence{Guests: guests, Viewers: viewers, At: now.UnixMilli()}
		worker.mu.Unlock()
		unlock := s.lock("plan")
		defer unlock()
		ctx, release, err := s.acquireDocsLocked(ctx, "plan", s.currentDeployment("plan"))
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		if _, err := s.syncDocsLocked(ctx, "plan"); err != nil {
			t.Fatal(err)
		}
	}
	presence := func() PublicPresence {
		fv, err := s.GetFlat(ctx, "plan")
		if err != nil || fv.PublicPresence == nil {
			t.Fatalf("presence %+v %v", fv.PublicPresence, err)
		}
		return *fv.PublicPresence
	}
	report(2, 5)
	if p := presence(); p != (PublicPresence{2, 5}) {
		t.Fatalf("presence %+v", p)
	}
	// Unchanged counts stay reported while the worker refreshes them.
	for range 3 {
		now = now.Add(20 * time.Second)
		report(2, 5)
	}
	if p := presence(); p != (PublicPresence{2, 5}) {
		t.Fatalf("presence after a minute %+v", p)
	}
	now = now.Add(31 * time.Second)
	if p := presence(); p != (PublicPresence{}) {
		t.Fatalf("stale presence %+v", p)
	}
	report(1, 1)
	rt.sleep(s)
	if p := presence(); p != (PublicPresence{}) {
		t.Fatalf("sleeping presence %+v", p)
	}
}

// An offline configuration edit that leaves documents at edit without a
// quota returns them to view only at startup.
func TestDocsPublicEditStartupDowngrade(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newPublicDocs(t, false)
	setAccess(t, s, store.Public, store.AccessEdit)
	if err := s.st.ContentTx(ctx, "plan", func(tx *store.ContentTx) error {
		zero := 0
		return tx.SetQuotas(ctx, &zero, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.downgradeEdit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, access := flatAccess(t, s); access != store.AccessView {
		t.Fatalf("level %q", access)
	}
	if !idleEventContains(t, s, "plan", "the documents stay Public, view only") {
		t.Fatal("missing event")
	}
}

// A document import runs the board's retention: guest-only revisions keep
// at most half of the revision quota, member revisions the rest.
func TestDocsGuestOnlyRetention(t *testing.T) {
	ctx := context.Background()
	s, worker, _ := newPublicDocs(t, false)
	if _, err := s.UpdateSettings(ctx, map[string]string{SetRevisionQuota: "4"}); err != nil {
		t.Fatal(err)
	}
	worker.mu.Lock()
	for i := range 8 {
		worker.editLocked("index.md", fmt.Sprintf("# %d", i), [][2]int{{1, 1}})
		if i == 3 || i == 5 {
			worker.record("person", []string{"Pat"}, nil)
		} else {
			worker.record("person", []string{}, []string{"Mina"})
		}
	}
	worker.mu.Unlock()
	if err := s.catchUpDocsNow(ctx); err != nil {
		t.Fatal(err)
	}
	guests, members := 0, 0
	for _, r := range revisionsOf(t, s) {
		if r.GuestOnly {
			guests++
		} else {
			members++
		}
	}
	if guests != 2 || members != 2 {
		t.Fatalf("kept %d guest-only and %d member revisions", guests, members)
	}
}

// Re-enabling on text that returned to the earlier safe point reuses it
// even when that safe point is not the newest revision and every pin is
// in use (rule 6).
func TestDocsSafePointReusedAfterRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, worker, _ := newPublicDocs(t, false)
	setAccess(t, s, store.Public, store.AccessEdit)
	sp := safePointRev(t, s)
	one := 1
	if err := s.SetRevisionQuotas(ctx, "plan", nil, &one); err != nil {
		t.Fatal(err)
	}
	setAccess(t, s, store.Public, store.AccessView)
	worker.mu.Lock()
	path, orig := worker.docs[0].Path, worker.texts[worker.docs[0].Hash]
	worker.mu.Unlock()
	worker.edit(path, "# changed", "", nil)
	worker.mu.Lock()
	worker.record("person", []string{"Ada"}, nil)
	worker.mu.Unlock()
	worker.edit(path, orig, "", nil)
	worker.mu.Lock()
	worker.record("person", []string{"Ada"}, nil)
	worker.mu.Unlock()
	if st, err := s.PublicEditPreflight(ctx, "plan"); err != nil || len(st.Problems) != 0 {
		t.Fatalf("preflight: %+v %v", st, err)
	}
	setAccess(t, s, store.Public, store.AccessEdit)
	if safePointRev(t, s) != sp || !slices.Equal(pinnedRevisions(t, s), []int{sp}) {
		t.Fatalf("reuse after a round trip: safe point %d, pins %v, revisions %+v", safePointRev(t, s), pinnedRevisions(t, s), revisionsOf(t, s))
	}
	for _, r := range revisionsOf(t, s) {
		if r.Kind == "safe_point" && r.Number != sp {
			t.Fatalf("a second safe point was recorded: %+v", revisionsOf(t, s))
		}
	}
}
