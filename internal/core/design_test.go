package core

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/design"
	"github.com/gosuda/flats/internal/store"
)

func canvasJSON(boards ...string) []byte {
	b := map[string]any{}
	for i, p := range boards {
		b[p] = map[string]any{"x": i * 1400, "y": 0, "w": 1280, "h": 800}
	}
	out, _ := json.Marshal(map[string]any{"title": "Test", "boards": b, "order": boards})
	return out
}

const artboard = `<!doctype html><html><head><title>A</title><script type="application/json" data-flats-tweaks>{"accent":{"type":"color","value":"#ffffff"}}</script></head><body><h1>Hello</h1><p>World</p></body></html>`

func newDesign(t *testing.T, s *Service, slug string) {
	t.Helper()
	_, err := s.WriteDesign(context.Background(), slug, DesignWrite{Files: []DesignFile{
		{Path: "canvas.json", Data: canvasJSON("a.html")}, {Path: "a.html", Data: []byte(artboard)},
	}}, ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
}

func headFile(t *testing.T, s *Service, slug, p string) ([]byte, design.SnapFile) {
	t.Helper()
	b, f, err := s.DesignFileContent(context.Background(), slug, p)
	if err != nil {
		t.Fatal(err)
	}
	return b, f
}

func TestDesignCreateWriteAndServe(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newDesign(t, s, "studio")
	f, err := s.GetFlat(ctx, "studio")
	if err != nil || f.Type != "design" || f.LiveVersion == 0 || f.Publication != "published" {
		t.Fatalf("flat = %+v %v", f, err)
	}
	res, err := s.WriteDesign(ctx, "studio", DesignWrite{Files: []DesignFile{{Path: "b.html", Data: []byte("<p>B</p>")}, {Path: "canvas.json", Data: canvasJSON("a.html", "b.html")}}, Message: "add b"}, ViaMCP)
	if err != nil || res.Revision != 2 || strings.Join(res.Changed, ",") != "b.html,canvas.json" {
		t.Fatalf("write = %+v %v", res, err)
	}
	// Files not named stay.
	if _, err := s.WriteDesign(ctx, "studio", DesignWrite{Files: []DesignFile{{Path: "b.html", Data: []byte("<p>B2</p>")}}}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	if b, _ := headFile(t, s, "studio", "a.html"); string(b) != artboard {
		t.Fatalf("a.html changed: %s", b)
	}
	// Serving: private canvas shell, sandboxed files, read-only public.
	h := s.siteHandler("studio", false)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/a.html", nil))
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("direct private file: %d %v", rec.Code, rec.Header())
	}
	if !strings.Contains(rec.Body.String(), "--accent:#ffffff") || strings.Contains(rec.Body.String(), "bridge.js") {
		t.Fatalf("direct open must apply tweaks without the bridge: %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/_design/api/state", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"visibility":"private"`) {
		t.Fatalf("state must carry the flat's visibility: %d %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest("POST", "/_design/api/ops", strings.NewReader(`{"editor":"e1","op":{"op":"note.add","gen":1,"fields":{"x":1,"y":2,"text":"hi"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Flats-Design", "1")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site op = %d %s", rec.Code, rec.Body)
	}
}

func TestDesignBundlePathsRefused(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	manifest := []byte(`{"type":"design"}`)
	if _, err := s.SaveVersion(ctx, "fresh", []bundle.File{{Path: "flats.json", Data: manifest}}, SaveMeta{}, ViaMCP); !errors.Is(err, ErrDesignUnversioned) {
		t.Fatalf("design manifest on a new slug: %v", err)
	}
	newDesign(t, s, "studio")
	if _, err := s.SaveVersion(ctx, "studio", []bundle.File{{Path: "index.html", Data: []byte("x")}}, SaveMeta{}, ViaMCP); !errors.Is(err, ErrTypeChange) {
		t.Fatalf("website into design: %v", err)
	}
	if _, err := s.ListVersions(ctx, "studio"); !errors.Is(err, ErrDesignUnversioned) {
		t.Fatalf("list versions: %v", err)
	}
}

func TestDesignOpsGroupsAndGenerations(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	clock := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	newDesign(t, s, "studio")
	src, a := headFile(t, s, "studio", "a.html")
	n, err := design.NodeOf(src, 0)
	if err != nil {
		t.Fatal(err)
	}
	op := design.Op{Kind: "text.set", Path: "a.html", Gen: a.Gen, Node: 0, Tag: "h1", InnerHash: n.InnerHash, HTML: "Hi <b>there</b>"}
	clock = clock.Add(time.Minute)
	if err := s.ApplyDesignOp(ctx, "studio", "e1", "Person", op); err != nil {
		t.Fatal(err)
	}
	// An edit marks the flat as updated.
	if f, err := s.GetFlat(ctx, "studio"); err != nil || !f.UpdatedAt.Equal(clock) {
		t.Fatalf("updated_at after an edit = %v, want %v (%v)", f.UpdatedAt, clock, err)
	}
	// Not yet a revision: the group is open.
	page, _ := s.ListRevisions(ctx, "studio", 0, 10)
	if len(page.Revisions) != 1 {
		t.Fatalf("revisions before close = %d", len(page.Revisions))
	}
	clock = clock.Add(31 * time.Second)
	s.CloseDueGroups(ctx)
	page, _ = s.ListRevisions(ctx, "studio", 0, 10)
	if len(page.Revisions) != 2 || page.Revisions[0].Kind != "person" || page.Revisions[0].Changes.Changed[0] != "a.html" {
		t.Fatalf("revisions after close = %+v", page.Revisions)
	}
	// An agent replaces a.html with identical text: a stale op is refused.
	_, a = headFile(t, s, "studio", "a.html")
	if _, err := s.WriteDesign(ctx, "studio", DesignWrite{Files: []DesignFile{{Path: "a.html", Data: []byte(strings.Replace(artboard, "<p>World</p>", "<p>World!</p>", 1))}}}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	stale := design.Op{Kind: "tweak.set", Path: "a.html", Gen: a.Gen, Name: "accent", Value: json.RawMessage(`"#000000"`), BaseValue: json.RawMessage(`"#ffffff"`)}
	if err := s.ApplyDesignOp(ctx, "studio", "e1", "Person", stale); !errors.Is(err, design.ErrStale) {
		t.Fatalf("stale gen accepted: %v", err)
	}
	// An open group closes before an agent write, as one revision.
	_, a = headFile(t, s, "studio", "a.html")
	stale.Gen = a.Gen
	if err := s.ApplyDesignOp(ctx, "studio", "e2", "Pat", stale); err != nil {
		t.Fatal(err)
	}
	before, _ := s.ListRevisions(ctx, "studio", 0, 50)
	if _, err := s.WriteDesign(ctx, "studio", DesignWrite{Files: []DesignFile{{Path: "c.txt", Data: []byte("c")}}}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	after, _ := s.ListRevisions(ctx, "studio", 0, 50)
	if len(after.Revisions) != len(before.Revisions)+2 || after.Revisions[1].Kind != "person" || after.Revisions[1].Authors[0] != "Pat" {
		t.Fatalf("group not closed before agent write: %+v", after.Revisions[:2])
	}
}

func TestDesignRollbackRetentionAndPins(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newDesign(t, s, "studio")
	for i := 0; i < 4; i++ {
		if _, err := s.WriteDesign(ctx, "studio", DesignWrite{Files: []DesignFile{{Path: "n.txt", Data: []byte{byte('a' + i)}}}}, ViaMCP); err != nil {
			t.Fatal(err)
		}
	}
	one := 1
	if err := s.SetRevisionQuotas(ctx, "studio", &one, nil); err != nil {
		t.Fatal(err)
	}
	page, _ := s.ListRevisions(ctx, "studio", 0, 50)
	if len(page.Revisions) != 1 || page.Revisions[0].Number != 5 {
		t.Fatalf("quota 1 kept %+v", page.Revisions)
	}
	two := 2
	if err := s.SetRevisionQuotas(ctx, "studio", &two, &one); err != nil {
		t.Fatal(err)
	}
	if err := s.PinRevision(ctx, "studio", 5, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteDesign(ctx, "studio", DesignWrite{Files: []DesignFile{{Path: "n.txt", Data: []byte("z")}}}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	if err := s.PinRevision(ctx, "studio", 6, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("full pin quota: %v", err)
	}
	// Rollback to the pinned 5: content equals 5 and every file gets a new
	// generation; it adds no blob.
	_, before := headFile(t, s, "studio", "a.html")
	n, err := s.RollbackRevision(ctx, "studio", 5, "Agent", ViaMCP)
	if err != nil || n != 7 {
		t.Fatalf("rollback = %d %v", n, err)
	}
	if b, _ := headFile(t, s, "studio", "n.txt"); string(b) != "d" {
		t.Fatalf("rolled back content = %q", b)
	}
	if _, after := headFile(t, s, "studio", "a.html"); after.Gen == before.Gen {
		t.Fatal("rollback kept the generation of an unchanged file")
	}
	r, _, err := s.GetRevision(ctx, "studio", 7)
	if err != nil || r.Kind != "rollback" || r.RollbackOf != 5 {
		t.Fatalf("rollback revision = %+v %v", r, err)
	}
	// Unpinned revisions over the quota of 2 are gone; the pin stays.
	page, _ = s.ListRevisions(ctx, "studio", 0, 50)
	var nums []int
	for _, r := range page.Revisions {
		nums = append(nums, r.Number)
	}
	if len(nums) != 3 || nums[0] != 7 || nums[2] != 5 {
		t.Fatalf("kept %v", nums)
	}
}

func TestDesignSweepAndAdoption(t *testing.T) {
	ctx := context.Background()
	s, dir := newTestService(t)
	newDesign(t, s, "studio")
	if _, err := s.WriteDesign(ctx, "studio", DesignWrite{Files: []DesignFile{{Path: "big.txt", Data: []byte(strings.Repeat("x", 1000))}}}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteDesign(ctx, "studio", DesignWrite{Delete: []string{"big.txt"}}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	zero := 1
	if err := s.SetRevisionQuotas(ctx, "studio", &zero, nil); err != nil {
		t.Fatal(err)
	}
	blob := s.blobPath("studio", hashBytes([]byte(strings.Repeat("x", 1000))))
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(blob, old, old)
	s.sweepDesignBlobs(ctx, "studio")
	if _, err := os.Stat(blob); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreferenced blob kept: %v", err)
	}
	// An untyped flat (empty, or left by an interrupted creation) becomes a
	// design on its first design write.
	if _, err := s.CreateFlat(ctx, "empty", "", ViaMCP); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteDesign(ctx, "empty", DesignWrite{}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	if f, _ := s.GetFlat(ctx, "empty"); f.Type != "design" {
		t.Fatalf("adopted type = %q", f.Type)
	}
	_ = dir
	_ = filepath.Join
	_ = io.EOF
	_ = store.ErrNotFound
}

// The canvas loads artboards through capabilities: CORS only under a live
// capability, revision files only for private access, and a stale text
// pick is refused.
func TestDesignCapabilitiesAndNodes(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newDesign(t, s, "studio")
	priv := s.siteHandler("studio", false)
	// A Public route sets X-Flats-Access: public; call the live design
	// instance that way (the flat itself stays Private).
	inst := s.state("studio").cur.Load().inst.(*design.Instance)
	pub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Flats-Access", "public")
		inst.ServeHTTP(w, r)
	})
	post := func(h http.Handler, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Flats-Design", "1")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	get := func(h http.Handler, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}
	var sess struct{ Cap string }
	rec := post(priv, "/_design/api/session", `{}`)
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &sess) != nil || len(sess.Cap) != 32 {
		t.Fatalf("session = %d %s", rec.Code, rec.Body)
	}
	rec = get(priv, "/_design/c/"+sess.Cap+"/a.html")
	if rec.Code != 200 || rec.Header().Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(rec.Body.String(), "bridge.js") || !strings.Contains(rec.Body.String(), `data-flats-node="0"`) {
		t.Fatalf("cap file = %d %v", rec.Code, rec.Header())
	}
	if rec := get(priv, "/_design/c/"+strings.Repeat("0", 32)+"/a.html"); rec.Code != 404 {
		t.Fatalf("unknown cap = %d", rec.Code)
	}
	// A head capability does not read revisions; a public request cannot get
	// a revision capability; the private revision capability works.
	if rec := post(pub, "/_design/api/session", `{"revision":1}`); rec.Code != 403 {
		t.Fatalf("public revision cap = %d", rec.Code)
	}
	var rs struct{ Cap string }
	_ = json.Unmarshal(post(priv, "/_design/api/session", `{"revision":1}`).Body.Bytes(), &rs)
	if rec := get(priv, "/_design/c/"+rs.Cap+"/a.html"); rec.Code != 200 || strings.Contains(rec.Body.String(), "bridge.js") {
		t.Fatalf("revision file = %d", rec.Code)
	}
	if rec := get(pub, "/_design/c/"+rs.Cap+"/a.html"); rec.Code != 404 {
		t.Fatalf("revision cap on a public route = %d", rec.Code)
	}
	// Public is read-only.
	if rec := post(pub, "/_design/api/ops", `{"editor":"e","op":{"op":"note.add","gen":1,"fields":{"x":1,"y":1,"text":"x"}}}`); rec.Code != 403 {
		t.Fatalf("public op = %d", rec.Code)
	}
	if rec := get(pub, "/_design/api/revisions"); rec.Code != 403 {
		t.Fatalf("public history = %d", rec.Code)
	}
	// A pick against an old generation is refused.
	_, a := headFile(t, s, "studio", "a.html")
	if rec := get(priv, "/_design/api/node?path=a.html&node=0&gen="+fmt.Sprint(a.Gen)); rec.Code != 200 {
		t.Fatalf("node = %d %s", rec.Code, rec.Body)
	}
	if _, err := s.WriteDesign(ctx, "studio", DesignWrite{Files: []DesignFile{{Path: "a.html", Data: []byte(strings.Replace(artboard, "<h1>", "<h2>New</h2><h1>", 1))}}}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	if rec := get(priv, "/_design/api/node?path=a.html&node=0&gen="+fmt.Sprint(a.Gen)); rec.Code != 409 {
		t.Fatalf("stale pick = %d", rec.Code)
	}
	// Export applies tweaks without host editing code.
	rec = get(priv, "/_design/export.zip")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("export = %d", rec.Code)
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name != "a.html" {
			continue
		}
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		if !strings.Contains(string(b), "--accent:#ffffff") || strings.Contains(string(b), "bridge.js") || strings.Contains(string(b), "data-flats-node") {
			t.Fatalf("exported artboard: %s", b)
		}
	}
}

// An agent write that changes nothing still closes open groups and reports
// the revision that closing created.
func TestDesignNoopWriteReportsClosedGroup(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newDesign(t, s, "studio")
	_, c := headFile(t, s, "studio", "canvas.json")
	op := design.Op{Kind: "note.add", Gen: c.Gen, ID: "n1", Fields: map[string]json.RawMessage{"x": json.RawMessage("1"), "y": json.RawMessage("2"), "text": json.RawMessage(`"hi"`)}}
	if err := s.ApplyDesignOp(ctx, "studio", "e1", "Pat", op); err != nil {
		t.Fatal(err)
	}
	res, err := s.WriteDesign(ctx, "studio", DesignWrite{Files: []DesignFile{{Path: "a.html", Data: []byte(artboard)}}}, ViaMCP)
	if err != nil || res.Revision != 2 || len(res.Changed) != 0 {
		t.Fatalf("noop write = %+v %v", res, err)
	}
	two := 2
	if _, err := s.WriteDesign(ctx, "studio", DesignWrite{Files: []DesignFile{{Path: "x.txt", Data: []byte("x")}}, ExpectedRevision: &two}, ViaMCP); err != nil {
		t.Fatalf("expected_revision from the noop result: %v", err)
	}
}

func TestDesignExpectedRevisionOnMissingDesign(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	three := 3
	if _, err := s.WriteDesign(ctx, "gone", DesignWrite{Files: []DesignFile{{Path: "x.txt", Data: []byte("x")}}, ExpectedRevision: &three}, ViaMCP); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale write recreated a design: %v", err)
	}
	if _, err := s.GetFlat(ctx, "gone"); err == nil {
		t.Fatal("flat was created")
	}
}

func TestDesignExpectedRevisionSeesPendingEdits(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newDesign(t, s, "studio")
	_, c := headFile(t, s, "studio", "canvas.json")
	op := design.Op{Kind: "note.add", Gen: c.Gen, ID: "n1", Fields: map[string]json.RawMessage{"x": json.RawMessage("1"), "y": json.RawMessage("2"), "text": json.RawMessage(`"hi"`)}}
	if err := s.ApplyDesignOp(ctx, "studio", "e1", "Pat", op); err != nil {
		t.Fatal(err)
	}
	one := 1
	if _, err := s.WriteDesign(ctx, "studio", DesignWrite{Files: []DesignFile{{Path: "x.txt", Data: []byte("x")}}, ExpectedRevision: &one}, ViaMCP); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending edits not guarded: %v", err)
	}
}

func TestKeptRenameJournalRefusesDesignStorageWrites(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newDesign(t, s, "studio")
	if _, err := s.WriteDesign(ctx, "studio", DesignWrite{Files: []DesignFile{{Path: "big.txt", Data: []byte(strings.Repeat("y", 1000))}}}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteDesign(ctx, "studio", DesignWrite{Delete: []string{"big.txt"}}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	one := 1
	if err := s.SetRevisionQuotas(ctx, "studio", &one, nil); err != nil {
		t.Fatal(err)
	}
	blob := s.blobPath("studio", hashBytes([]byte(strings.Repeat("y", 1000))))
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(blob, old, old)
	op := storageOperation{Kind: "rename", From: "old-studio", To: "studio"}
	if err := s.beginStorageOperation(op); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteDesign(ctx, "studio", DesignWrite{Files: []DesignFile{{Path: "c.html", Data: []byte("<p>C</p>")}}}, ViaMCP); !errors.Is(err, ErrConflict) {
		t.Fatalf("write while the rename journal is kept: %v", err)
	}
	src, a := headFile(t, s, "studio", "a.html")
	n, err := design.NodeOf(src, 0)
	if err != nil {
		t.Fatal(err)
	}
	edit := design.Op{Kind: "text.set", Path: "a.html", Gen: a.Gen, Node: 0, Tag: "h1", InnerHash: n.InnerHash, HTML: "Hi"}
	if err := s.ApplyDesignOp(ctx, "studio", "e1", "Person", edit); !errors.Is(err, ErrConflict) {
		t.Fatalf("edit while the rename journal is kept: %v", err)
	}
	// The files under this slug may belong to the other flat: no sweep.
	s.sweepDesignBlobs(ctx, "studio")
	if _, err := os.Stat(blob); err != nil {
		t.Fatalf("sweep deleted a blob while the rename journal is kept: %v", err)
	}
	if _, err := s.GetDesign(ctx, "studio"); err != nil {
		t.Fatalf("read while the rename journal is kept: %v", err)
	}
	if err := s.recoverStorageOperations(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyDesignOp(ctx, "studio", "e1", "Person", edit); err != nil {
		t.Fatalf("edit after recovery: %v", err)
	}
}
