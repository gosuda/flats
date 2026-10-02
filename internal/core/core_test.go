package core_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oesni/flats/internal/bundle"
	"github.com/oesni/flats/internal/core"
	"github.com/oesni/flats/internal/expose/local"
	"github.com/oesni/flats/internal/store"
	_ "modernc.org/sqlite"
)

type env struct {
	dataDir string
	svc     *core.Service
	priv    *local.Net
	pub     *local.Public
	st      *store.Store
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	priv, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pubNet, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pub := local.NewPublic(pubNet)
	priv.Identity = func(*http.Request) (string, string) { return "op@example.com", "Operator" }
	svc, err := core.New(context.Background(), core.Config{DataDir: dir, Store: st, Private: priv, Public: pub,
		ConsoleURL: func() string { return "http://console.test" }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close(); priv.Close(); pubNet.Close(); st.Close() })
	return &env{dataDir: dir, svc: svc, priv: priv, pub: pub, st: st}
}

func files(kv ...string) []bundle.File {
	var out []bundle.File
	for i := 0; i < len(kv); i += 2 {
		out = append(out, bundle.File{Path: kv[i], Data: []byte(kv[i+1])})
	}
	return out
}

func get(t *testing.T, url string, hdr ...string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestSaveDeployRollback(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	v1, err := e.svc.SaveVersion(ctx, "blog", files("index.html", "<h1>one</h1>"), core.SaveMeta{GitSHA: "abc123", GitDirty: true}, core.ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Number != 1 || v1.GitSHA != "abc123" || !v1.GitDirty || v1.Hash == "" {
		t.Fatalf("unexpected version %+v", v1)
	}
	fv, _ := e.svc.GetFlat(ctx, "blog")
	if fv.Visibility != store.Private || fv.LiveVersion != 0 {
		t.Fatalf("new flat should be private and undeployed: %+v", fv.Flat)
	}
	if e.priv.Serving("blog") {
		t.Fatal("undeployed flat must not be exposed yet")
	}
	res, err := e.svc.Deploy(ctx, "blog", 1, core.ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Health.OK || res.Version != 1 {
		t.Fatalf("bad deploy result %+v", res)
	}
	code, body, _ := get(t, e.priv.URL("blog"))
	if code != 200 || !strings.Contains(body, "one") {
		t.Fatalf("live v1: %d %q", code, body)
	}
	// Saving v2 does not change live.
	if _, err := e.svc.SaveVersion(ctx, "blog", files("index.html", "<h1>two</h1>"), core.SaveMeta{}, core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	if _, body, _ := get(t, e.priv.URL("blog")); !strings.Contains(body, "one") {
		t.Fatalf("save must not change live, got %q", body)
	}
	if _, err := e.svc.Deploy(ctx, "blog", 2, core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	if _, body, _ := get(t, e.priv.URL("blog")); !strings.Contains(body, "two") {
		t.Fatalf("live v2: %q", body)
	}
	start := time.Now()
	rb, err := e.svc.Rollback(ctx, "blog", 0, core.ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	if rb.Version != 1 || time.Since(start) > 10*time.Second {
		t.Fatalf("rollback: %+v in %s", rb, time.Since(start))
	}
	if _, body, _ := get(t, e.priv.URL("blog")); !strings.Contains(body, "one") {
		t.Fatalf("after rollback: %q", body)
	}
}

func TestFailedHealthCheckKeepsLive(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "site", files("index.html", "ok"), core.SaveMeta{}, core.ViaAPI)
	if _, err := e.svc.Deploy(ctx, "site", 1, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	// v2 points health at a missing path.
	_, err := e.svc.SaveVersion(ctx, "site", files("index.html", "broken", "flats.json", `{"health":"/missing"}`), core.SaveMeta{}, core.ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Deploy(ctx, "site", 2, core.ViaAPI)
	var de *core.DeployError
	if !errors.As(err, &de) || de.Health.Status != 404 {
		t.Fatalf("expected health failure, got %v", err)
	}
	if _, body, _ := get(t, e.priv.URL("site")); body != "ok" {
		t.Fatalf("failed deploy must keep v1 live, got %q", body)
	}
	fv, _ := e.svc.GetFlat(ctx, "site")
	if fv.LiveVersion != 1 {
		t.Fatalf("live version = %d", fv.LiveVersion)
	}
}

func TestValidationErrorsAreActionable(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	_, err := e.svc.SaveVersion(ctx, "site", files("dist/app.js", "x", "readme.md", "y"), core.SaveMeta{}, core.ViaAPI)
	v, ok := bundle.IsValidation(err)
	if !ok || len(v.Problems) == 0 || v.Problems[0].Fix == "" {
		t.Fatalf("want validation error with fix, got %v", err)
	}
	if _, err := e.svc.SaveVersion(ctx, "Bad_Slug", files("index.html", "x"), core.SaveMeta{}, core.ViaAPI); err == nil {
		t.Fatal("invalid slug accepted")
	}
}

func TestVisibilityApprovalAndIdentityHeaders(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "who", files("index.html", "hi"), core.SaveMeta{}, core.ViaMCP)
	e.svc.Deploy(ctx, "who", 1, core.ViaMCP)
	r, err := e.svc.SetVisibility(ctx, "who", store.PublicUnlisted, core.ViaMCP, "share demo")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "pending_approval" || !strings.HasPrefix(r.ApprovalURL, "http://console.test/approvals/apr-") {
		t.Fatalf("agent must not go public alone: %+v", r)
	}
	if _, served := e.pub.Hidden("who"); served {
		t.Fatal("flat went public before approval")
	}
	a, err := e.svc.Decide(ctx, r.Approval.ID, true)
	if err != nil || a.Status != "approved" {
		t.Fatalf("approve: %v %+v", err, a)
	}
	hidden, served := e.pub.Hidden("who")
	if !served || !hidden {
		t.Fatalf("unlisted flat should be served hidden: served=%v hidden=%v", served, hidden)
	}
	fv, _ := e.svc.GetFlat(ctx, "who")
	if fv.PublicNotice != core.UnlistedNotice || fv.PublicURL == "" {
		t.Fatalf("unlisted notice missing: %+v", fv)
	}
	// Exposure-reducing change by an agent applies immediately.
	r, err = e.svc.SetVisibility(ctx, "who", store.Private, core.ViaMCP, "")
	if err != nil || r.Status != "done" {
		t.Fatalf("reduce exposure: %v %+v", err, r)
	}
	if _, served := e.pub.Hidden("who"); served {
		t.Fatal("private flat still public")
	}
	// Console changes apply directly (confirm dialog is in the UI).
	r, _ = e.svc.SetVisibility(ctx, "who", store.PublicListed, core.ViaConsole, "")
	if r.Status != "done" {
		t.Fatalf("console change should apply: %+v", r)
	}
	if hidden, served := e.pub.Hidden("who"); !served || hidden {
		t.Fatal("listed flat should be served and visible")
	}
	// Spoofed identity headers are stripped on the public path.
	_, body, _ := get(t, e.pub.URL("who"), "Tailscale-User-Login", "evil@example.com")
	if body != "hi" {
		t.Fatalf("public body %q", body)
	}
}

func TestDeleteNeedsApproval(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "gone", files("index.html", "x"), core.SaveMeta{}, core.ViaMCP)
	e.svc.Deploy(ctx, "gone", 1, core.ViaMCP)
	r, err := e.svc.Delete(ctx, "gone", core.ViaCLI, "cleanup")
	if err != nil || r.Status != "pending_approval" {
		t.Fatalf("delete must wait: %v %+v", err, r)
	}
	if _, err := e.svc.GetFlat(ctx, "gone"); err != nil {
		t.Fatal("flat deleted before approval")
	}
	if _, err := e.svc.Decide(ctx, r.Approval.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetFlat(ctx, "gone"); err != nil {
		t.Fatal("rejected delete removed the flat")
	}
	if _, err := e.svc.Delete(ctx, "gone", core.ViaConsole, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetFlat(ctx, "gone"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("flat should be gone, got %v", err)
	}
	if e.priv.Serving("gone") {
		t.Fatal("deleted flat still served")
	}
}

func TestPreviewLifecycle(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "prev", files("index.html", "live"), core.SaveMeta{}, core.ViaAPI)
	e.svc.Deploy(ctx, "prev", 1, core.ViaAPI)
	e.svc.SaveVersion(ctx, "prev", files("index.html", "candidate"), core.SaveMeta{}, core.ViaAPI)
	p, err := e.svc.OpenPreview(ctx, "prev", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.Host, "prev-") || len(p.Host) != len("prev-")+8 {
		t.Fatalf("preview host %q", p.Host)
	}
	if _, body, _ := get(t, p.URL); body != "candidate" {
		t.Fatalf("preview body %q", body)
	}
	if _, body, _ := get(t, e.priv.URL("prev")); body != "live" {
		t.Fatalf("live changed by preview: %q", body)
	}
	if _, err := e.svc.Deploy(ctx, "prev", 2, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if e.priv.Serving(p.Host) {
		t.Fatal("deploy must close previews")
	}
}

func TestRenameRedirect(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "old-name", files("index.html", "page", "about.html", "about"), core.SaveMeta{}, core.ViaAPI)
	e.svc.Deploy(ctx, "old-name", 1, core.ViaAPI)
	if _, err := e.svc.RenameSlug(ctx, "old-name", "new-name", core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	code, _, h := get(t, e.priv.URL("old-name")+"/about?x=1")
	if code != http.StatusPermanentRedirect || h.Get("Location") != e.priv.URL("new-name")+"/about?x=1" {
		t.Fatalf("redirect: %d %q", code, h.Get("Location"))
	}
	if _, body, _ := get(t, e.priv.URL("new-name")+"/about"); body != "about" {
		t.Fatalf("new name serves %q", body)
	}
}

func TestApprovedDeleteClosesOtherApprovals(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "multi", files("index.html", "x"), core.SaveMeta{}, core.ViaMCP)
	e.svc.Deploy(ctx, "multi", 1, core.ViaMCP)
	vis, _ := e.svc.SetVisibility(ctx, "multi", store.PublicListed, core.ViaMCP, "")
	if vis.Notice != core.ListedNotice {
		t.Fatalf("pending visibility must carry the notice: %+v", vis)
	}
	del, _ := e.svc.Delete(ctx, "multi", core.ViaMCP, "")
	a, err := e.svc.Decide(ctx, del.Approval.ID, true)
	if err != nil || a.Status != "approved" {
		t.Fatalf("approve delete: %v %+v", err, a)
	}
	other, _ := e.svc.GetApproval(ctx, vis.Approval.ID)
	if other.Status != "failed" {
		t.Fatalf("stale approval should close, got %s", other.Status)
	}
}

func TestDeployErrorWhenNothingLive(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "fresh", files("index.html", "x", "flats.json", `{"health":"/nope"}`), core.SaveMeta{}, core.ViaAPI)
	_, err := e.svc.Deploy(ctx, "fresh", 1, core.ViaAPI)
	if err == nil || !strings.Contains(err.Error(), "nothing was live") {
		t.Fatalf("got %v", err)
	}
}

func TestPreDeploySnapshotAndDataRollback(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "datum", files("index.html", "v1"), core.SaveMeta{}, core.ViaAPI)
	e.svc.Deploy(ctx, "datum", 1, core.ViaAPI)
	// Simulate a server flat's database.
	dbPath := filepath.Join(e.dataDir, "flats", "datum", "data", "db.sqlite")
	os.MkdirAll(filepath.Dir(dbPath), 0o700)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE TABLE t (v TEXT)`)
	db.Exec(`INSERT INTO t VALUES ('before')`)
	db.Close()
	e.svc.SaveVersion(ctx, "datum", files("index.html", "v2"), core.SaveMeta{}, core.ViaAPI)
	if _, err := e.svc.Deploy(ctx, "datum", 2, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	snaps, _ := e.svc.Snapshots("datum")
	if len(snaps) != 1 || !strings.HasPrefix(snaps[0], "before-v2-") {
		t.Fatalf("snapshots %v", snaps)
	}
	db, _ = sql.Open("sqlite", dbPath)
	db.Exec(`UPDATE t SET v='after'`)
	db.Close()
	if _, err := e.svc.RollbackWithData(ctx, "datum", 0, true, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	db, _ = sql.Open("sqlite", dbPath)
	var v string
	db.QueryRow(`SELECT v FROM t`).Scan(&v)
	db.Close()
	if v != "before" {
		t.Fatalf("data not restored: %q", v)
	}
	if _, body, _ := get(t, e.priv.URL("datum")); body != "v1" {
		t.Fatalf("code not rolled back: %q", body)
	}
}

func TestThumbnailFallsBackToFavicon(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "icon-site", files("index.html", "x", "favicon.svg", "<svg/>"), core.SaveMeta{}, core.ViaAPI)
	fv, _ := e.svc.GetFlat(ctx, "icon-site")
	if fv.Thumbnail != "" {
		t.Fatalf("no live version yet, want no thumbnail, got %q", fv.Thumbnail)
	}
	e.svc.Deploy(ctx, "icon-site", 1, core.ViaAPI)
	fv, _ = e.svc.GetFlat(ctx, "icon-site")
	if !strings.HasSuffix(fv.Thumbnail, "/versions/1/files/favicon.svg") {
		t.Fatalf("favicon fallback: %q", fv.Thumbnail)
	}
	e.svc.SaveVersion(ctx, "icon-site", files("index.html", "x", "shot.png", "png", "flats.json", `{"screenshot":"shot.png"}`), core.SaveMeta{}, core.ViaAPI)
	fv, _ = e.svc.GetFlat(ctx, "icon-site")
	if !strings.HasSuffix(fv.Thumbnail, "/versions/2/files/shot.png") {
		t.Fatalf("screenshot should win: %q", fv.Thumbnail)
	}
}
