package docs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

func pngBytes(t *testing.T, shade uint8, side int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	for i := range img.Pix {
		img.Pix[i] = shade + uint8(i%251)
	}
	img.Set(0, 0, color.RGBA{shade, shade, shade, 255})
	var b bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.NoCompression}).Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// pasteImage posts an image as the document page does.
func pasteImage(h http.Handler, doc string, data []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://host-doc.localhost/_docs/api/images?doc="+url.QueryEscape(doc)+"&name=Kim", bytes.NewReader(data))
	req.Header.Set("X-Flats-Docs", "1")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func pasted(t *testing.T, rec *httptest.ResponseRecorder) core.DocumentImage {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatal("paste", rec.Code, rec.Body.String())
	}
	var out core.DocumentImage
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func refusedPaste(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var out struct {
		Code, Message string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != status || out.Code != code || out.Message == "" {
		t.Fatalf("got %d %s, want %d %s", rec.Code, rec.Body.String(), status, code)
	}
}

// flatState is what a refused or repeated paste must leave alone.
func flatState(t *testing.T, s *core.Service) (live int, revisions []store.Revision) {
	t.Helper()
	ctx := t.Context()
	f, err := s.GetFlat(ctx, "host-doc")
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.ListRevisions(ctx, "host-doc", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	return f.LiveVersion, page.Revisions
}

func requireUnchanged(t *testing.T, s *core.Service, live int, revisions []store.Revision) {
	t.Helper()
	gotLive, gotRevisions := flatState(t, s)
	if gotLive != live || len(gotRevisions) != len(revisions) {
		t.Fatalf("live version %d→%d, revisions %d→%d", live, gotLive, len(revisions), len(gotRevisions))
	}
}

func TestHostPastedImageJoinsTheBundle(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, private, _ := hostService(t)
	manifest := []byte(`{"type":"docs","name":"Shared","entry":"index.md"}`)
	sibling := []byte("a sibling asset")
	if _, err := s.SaveVersion(ctx, "host-doc", []bundle.File{
		{Path: "flats.json", Data: manifest},
		{Path: "index.md", Data: []byte(initialText)},
		{Path: "notes/guide.md", Data: []byte("# Guide\n")},
		{Path: "data.txt", Data: sibling},
	}, core.SaveMeta{}, core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	// Live text a person wrote, which the saved source does not hold.
	insert := func(text string) {
		t.Helper()
		if _, err := s.UpdateDocument(ctx, "host-doc", "index.md", []core.DocumentEditOp{{Op: "insert", At: "end", Text: text}}, nil, core.ViaAPI); err != nil {
			t.Fatal(err)
		}
	}
	insert("Live before the image.")
	liveBefore, _ := flatState(t, s)

	data := pngBytes(t, 40, 8)
	image := pasted(t, pasteImage(private.handler("host-doc"), "index.md", data))
	if !image.Added || !strings.HasPrefix(image.Reference, "images/") || !strings.HasSuffix(image.Reference, ".png") || image.Path != image.Reference {
		t.Fatalf("image %+v", image)
	}
	live, revisions := flatState(t, s)
	if live == liveBefore {
		t.Fatal("the bundle did not change")
	}
	if r := revisions[0]; r.Message != "added image "+image.Path || len(r.Authors) != 1 || r.Authors[0] != "Kim" {
		t.Fatalf("revision %+v", r)
	}

	// The new handler serves it, inert, and the rest of the flat is intact.
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		private.handler("host-doc").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://host-doc.localhost/"+path, nil))
		return rec
	}
	served := get(image.Path)
	if served.Code != 200 || !bytes.Equal(served.Body.Bytes(), data) || served.Header().Get("Content-Type") != "image/png" ||
		!strings.Contains(served.Header().Get("Content-Security-Policy"), "sandbox") || served.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("served image", served.Code, served.Header())
	}
	if rec := get("data.txt"); rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), sibling) {
		t.Fatal("sibling", rec.Code)
	}
	doc, err := s.GetDocument(ctx, "host-doc", "index.md")
	if err != nil || !strings.Contains(doc.Markdown, "Live before the image.") {
		t.Fatalf("live text: %q %v", doc.Markdown, err)
	}
	_, files, err := s.GetRevision(ctx, "host-doc", revisions[0].Number)
	if err != nil {
		t.Fatal(err)
	}
	inRevision := false
	for _, f := range files {
		inRevision = inRevision || f.Path == image.Path
	}
	if !inRevision {
		t.Fatalf("revision files %+v", files)
	}

	// The same bytes change nothing, and live text written since stays.
	insert("Live after the image.")
	live, revisions = flatState(t, s)
	again := pasted(t, pasteImage(private.handler("host-doc"), "index.md", data))
	if again.Added || again.Reference != image.Reference {
		t.Fatalf("again %+v", again)
	}
	requireUnchanged(t, s, live, revisions)

	// The file being there already does not make any path a document.
	refusedPaste(t, pasteImage(private.handler("host-doc"), "missing.md", data), http.StatusNotFound, "not_found")
	requireUnchanged(t, s, live, revisions)

	// A nested document gets a nested file with the same relative reference.
	nested := pasted(t, pasteImage(private.handler("host-doc"), "notes/guide.md", data))
	if !nested.Added || nested.Reference != image.Reference || nested.Path != "notes/"+image.Reference {
		t.Fatalf("nested %+v", nested)
	}
	if rec := get(nested.Path); rec.Code != 200 {
		t.Fatal("nested image", rec.Code)
	}
	doc, err = s.GetDocument(ctx, "host-doc", "index.md")
	if err != nil || !strings.Contains(doc.Markdown, "Live before the image.") || !strings.Contains(doc.Markdown, "Live after the image.") {
		t.Fatalf("live text after a second save: %q %v", doc.Markdown, err)
	}
}

func TestHostPastedImageRefusals(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, private, _ := hostService(t)
	h := private.handler("host-doc")
	live, revisions := flatState(t, s)
	data := pngBytes(t, 90, 8)

	t.Run("not an accepted image", func(t *testing.T) {
		for name, body := range map[string][]byte{
			"empty": {},
			"text":  []byte("not an image"),
			"svg":   []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
			"cut":   data[:len(data)/2],
		} {
			if rec := pasteImage(h, "index.md", body); rec.Code != http.StatusBadRequest {
				t.Error(name, rec.Code, rec.Body.String())
			}
		}
	})
	t.Run("not a document of the flat", func(t *testing.T) {
		refusedPaste(t, pasteImage(h, "missing.md", data), http.StatusNotFound, "not_found")
		refusedPaste(t, pasteImage(h, "../index.md", data), http.StatusBadRequest, "invalid")
		refusedPaste(t, pasteImage(h, "flats.json", data), http.StatusBadRequest, "invalid")
	})
	write := func(mutate func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://host-doc.localhost/_docs/api/images?doc=index.md", bytes.NewReader(data))
		req.Header.Set("X-Flats-Docs", "1")
		mutate(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	t.Run("not from the page", func(t *testing.T) {
		refusedPaste(t, write(func(r *http.Request) { r.Header.Del("X-Flats-Docs") }), http.StatusForbidden, "forbidden")
		refusedPaste(t, write(func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }), http.StatusForbidden, "forbidden")
		refusedPaste(t, write(func(r *http.Request) { r.Header.Set("Origin", "http://other.example") }), http.StatusForbidden, "forbidden")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://host-doc.localhost/_docs/api/images?doc=index.md", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatal("GET", rec.Code)
		}
	})
	requireUnchanged(t, s, live, revisions)

	t.Run("in the way", func(t *testing.T) {
		name := imageNameFor(t, h, data)
		// A later save can put other bytes at that path, or a file where the
		// directory goes.
		if _, err := s.SaveVersion(ctx, "host-doc", []bundle.File{
			{Path: "flats.json", Data: []byte(`{"type":"docs"}`)},
			{Path: "index.md", Data: []byte(initialText)},
			{Path: name, Data: []byte("other bytes")},
		}, core.SaveMeta{}, core.ViaMCP); err != nil {
			t.Fatal(err)
		}
		refusedPaste(t, pasteImage(private.handler("host-doc"), "index.md", data), http.StatusConflict, "conflict")
		if _, err := s.SaveVersion(ctx, "host-doc", []bundle.File{
			{Path: "flats.json", Data: []byte(`{"type":"docs"}`)},
			{Path: "index.md", Data: []byte(initialText)},
			{Path: "images", Data: []byte("a file named images")},
		}, core.SaveMeta{}, core.ViaMCP); err != nil {
			t.Fatal(err)
		}
		refusedPaste(t, pasteImage(private.handler("host-doc"), "index.md", data), http.StatusConflict, "conflict")
	})
}

// A Public route has no such route, at either public access level, even when
// the request claims to be private.
func TestHostPublicRouteCannotPasteImages(t *testing.T) {
	t.Parallel()
	for _, level := range []string{store.AccessView, store.AccessEdit} {
		t.Run(level, func(t *testing.T) {
			t.Parallel()
			s, _, public := hostService(t)
			if err := s.SetProviderPermission(t.Context(), "host-doc", store.ProviderPortal, true, core.ViaConsole); err != nil {
				t.Fatal(err)
			}
			res, err := s.SetVisibilityAccess(t.Context(), "host-doc", store.Public, level, core.ViaMCP, "test")
			approveAction(t, s, res, err)
			live, revisions := flatState(t, s)
			req := httptest.NewRequest(http.MethodPost, "http://host-doc.localhost/_docs/api/images?doc=index.md", bytes.NewReader(pngBytes(t, 70, 8)))
			req.Header.Set("X-Flats-Docs", "1")
			req.Header.Set("X-Flats-Access", "private")
			rec := httptest.NewRecorder()
			public.handler("host-doc").ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatal(rec.Code, rec.Body.String())
			}
			requireUnchanged(t, s, live, revisions)
		})
	}
}

// failingHealth makes the isolated health trial of a candidate fail while it
// is on, leaving the real worker of the running document alone.
type failingHealth struct {
	core.Runtime
	on   atomic.Bool
	hits atomic.Int64
}

type failingHealthInstance struct {
	core.Instance
	owner *failingHealth
}

func (rt *failingHealth) Start(ctx context.Context, spec core.RuntimeSpec) (core.Instance, error) {
	inst, err := rt.Runtime.Start(ctx, spec)
	if err != nil || spec.ManagedDocs {
		return inst, err
	}
	return failingHealthInstance{inst, rt}, nil
}

func (in failingHealthInstance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_docs/healthz" && in.owner.on.Load() {
		in.owner.hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	in.Instance.ServeHTTP(w, r)
}

// A candidate that starts but fails its health check refuses the image. The
// real worker of the running document must keep serving and accepting edits,
// with its text and saved files as they were, and a later paste must work.
func TestHostPastedImageFailedHealthKeepsTheRunningDocument(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	rt := &failingHealth{Runtime: manager(t)}
	s, private, _ := hostServiceWith(t, rt)
	insert := func(text string) {
		t.Helper()
		if _, err := s.UpdateDocument(ctx, "host-doc", "index.md", []core.DocumentEditOp{{Op: "insert", At: "end", Text: text}}, nil, core.ViaAPI); err != nil {
			t.Fatal("the running document refused an edit:", err)
		}
	}
	text := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		private.handler("host-doc").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://host-doc.localhost/_docs/api/document?doc=index.md", nil))
		var out struct{ Markdown string }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 {
			t.Fatal("the running document did not answer:", rec.Code, rec.Body.String())
		}
		return out.Markdown
	}
	files := func() map[string]string {
		t.Helper()
		page, err := s.ListRevisions(ctx, "host-doc", 0, 1)
		if err != nil {
			t.Fatal(err)
		}
		_, list, err := s.GetRevision(ctx, "host-doc", page.Revisions[0].Number)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, f := range list {
			out[f.Path] = f.Hash
		}
		return out
	}
	insert("Live before the failure.")
	before := text()
	liveBefore, _ := flatState(t, s)
	filesBefore := files()

	data := pngBytes(t, 120, 8)
	sum := sha256.Sum256(data)
	path := "images/" + hex.EncodeToString(sum[:])[:16] + ".png"
	rt.on.Store(true)
	refusedPaste(t, pasteImage(private.handler("host-doc"), "index.md", data), http.StatusServiceUnavailable, "unavailable")
	rt.on.Store(false)
	if rt.hits.Load() == 0 {
		t.Fatal("the candidate's health check never ran")
	}

	liveAfter, revisions := flatState(t, s)
	if liveAfter != liveBefore {
		t.Fatalf("live version %d→%d", liveBefore, liveAfter)
	}
	for _, r := range revisions {
		if strings.HasPrefix(r.Message, "added image") {
			t.Fatalf("a refused image has a revision: %+v", r)
		}
	}
	if got := text(); got != before {
		t.Fatalf("live text changed: %q", got)
	}
	for p, hash := range filesBefore {
		if files()[p] != hash {
			t.Fatalf("file %s changed", p)
		}
	}
	rec := httptest.NewRecorder()
	private.handler("host-doc").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://host-doc.localhost/"+path, nil))
	if rec.Code == http.StatusOK && bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatal("the refused image is served")
	}

	// The worker is the same live one: an edit commits and a new reader sees it.
	insert("Live after the failure.")
	if got := text(); !strings.Contains(got, "Live before the failure.") || !strings.Contains(got, "Live after the failure.") {
		t.Fatalf("live text after the failure: %q", got)
	}
	// Nothing of the failed attempt blocks or joins a later one.
	image := pasted(t, pasteImage(private.handler("host-doc"), "index.md", data))
	if !image.Added || image.Path != path {
		t.Fatalf("image %+v", image)
	}
	if got := text(); !strings.Contains(got, "Live after the failure.") {
		t.Fatalf("live text after the retry: %q", got)
	}
}

// imageNameFor learns the path an image goes to by adding it.
func imageNameFor(t *testing.T, h http.Handler, data []byte) string {
	t.Helper()
	return pasted(t, pasteImage(h, "index.md", data)).Path
}

func TestHostPastedImageLimits(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, private, _ := hostService(t)
	set := func(key string, value int) {
		t.Helper()
		if _, err := s.UpdateSettings(ctx, map[string]string{key: strconv.Itoa(value)}); err != nil {
			t.Fatal(err)
		}
	}
	h := func() http.Handler { return private.handler("host-doc") }
	first, second := pngBytes(t, 10, 64), pngBytes(t, 20, 64)
	// One image over the upload limit.
	set(core.SetUploadMaxBytes, len(first)-1)
	live, revisions := flatState(t, s)
	refusedPaste(t, pasteImage(h(), "index.md", first), http.StatusRequestEntityTooLarge, "too_large")
	requireUnchanged(t, s, live, revisions)
	// Each image fits, but the second would put the bundle over it.
	set(core.SetUploadMaxBytes, len(first)+len(initialText)+1024)
	pasted(t, pasteImage(h(), "index.md", first))
	live, revisions = flatState(t, s)
	rec := pasteImage(h(), "index.md", second)
	refusedPaste(t, rec, http.StatusRequestEntityTooLarge, "bundle_full")
	if !strings.Contains(rec.Body.String(), "upload_max_bytes") {
		t.Fatal(rec.Body.String())
	}
	requireUnchanged(t, s, live, revisions)
	// Over the disk quota.
	set(core.SetUploadMaxBytes, 20<<20)
	set(core.SetDiskQuotaBytes, 1)
	refusedPaste(t, pasteImage(h(), "index.md", second), http.StatusInsufficientStorage, "disk_quota")
	requireUnchanged(t, s, live, revisions)
	set(core.SetDiskQuotaBytes, 0)
	pasted(t, pasteImage(h(), "index.md", second))
}

// Opt-in: pasting in a real browser, against a real host.
func TestBrowserImagePaste(t *testing.T) {
	if os.Getenv("FLATS_DOCS_BROWSER_TEST") != "1" {
		t.Skip("opt in with FLATS_DOCS_BROWSER_TEST=1 and FLATS_PLAYWRIGHT_MODULE")
	}
	s, private, public := hostService(t)
	if err := s.SetProviderPermission(t.Context(), "host-doc", store.ProviderPortal, true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	res, err := s.SetVisibilityAccess(t.Context(), "host-doc", store.Public, store.AccessEdit, core.ViaMCP, "test")
	approveAction(t, s, res, err)
	// The handlers change with each version, so look them up per request.
	behind := func(n *captureNet) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.handler("host-doc").ServeHTTP(w, r)
		}))
		t.Cleanup(srv.CloseClientConnections)
		t.Cleanup(srv.Close)
		return srv
	}
	privateSrv, publicSrv := behind(private), behind(public)
	cmd := exec.Command("node", "testdata/image_paste.mjs", privateSrv.URL, publicSrv.URL, browserEvidence(t, "image-paste"))
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatal(err)
	}
}
