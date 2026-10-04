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

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/store"
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

func decideDeploy(t *testing.T, svc *core.Service, slug string, version int) error {
	t.Helper()
	ctx := context.Background()
	_, err := svc.Deploy(ctx, slug, version, core.ViaAPI)
	var pending *core.PendingApproval
	if !errors.As(err, &pending) || pending.Approval == nil {
		if err == nil {
			t.Fatal("deploy applied without approval")
		}
		return err
	}
	_, err = svc.Decide(ctx, pending.Approval.ID, true)
	return err
}

func publish(t *testing.T, svc *core.Service, slug string) core.FlatView {
	t.Helper()
	if err := decideDeploy(t, svc, slug, 0); err != nil {
		t.Fatalf("publish %s: %v", slug, err)
	}
	f, err := svc.GetFlat(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func permitPortal(t *testing.T, svc *core.Service, slug string) {
	t.Helper()
	if err := svc.SetProviderPermission(context.Background(), slug, store.ProviderPortal, true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
}

func decideRollback(t *testing.T, svc *core.Service, slug string, to int, restore bool) error {
	t.Helper()
	ctx := context.Background()
	_, err := svc.RollbackWithData(ctx, slug, to, restore, core.ViaAPI)
	var pending *core.PendingApproval
	if !errors.As(err, &pending) || pending.Approval == nil {
		return err
	}
	_, err = svc.Decide(ctx, pending.Approval.ID, true)
	return err
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
	if v1.Number != 0 || v1.Revision != 1 || v1.Published || v1.Role != "draft" || v1.GitSHA != "abc123" || !v1.GitDirty || v1.Hash == "" {
		t.Fatalf("unexpected draft %+v", v1)
	}
	fv, _ := e.svc.GetFlat(ctx, "blog")
	if fv.Visibility != store.Private || fv.LiveVersion != 0 {
		t.Fatalf("new flat should be private and undeployed: %+v", fv.Flat)
	}
	if e.priv.Serving("blog") {
		t.Fatal("undeployed flat must not be exposed yet")
	}
	res := publish(t, e.svc, "blog")
	if res.LiveVersion != 1 || res.Publication != "published" {
		t.Fatalf("bad publish result %+v", res)
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
	if f := publish(t, e.svc, "blog"); f.LiveVersion != 2 {
		t.Fatalf("live %d", f.LiveVersion)
	}
	if _, body, _ := get(t, e.priv.URL("blog")); !strings.Contains(body, "two") {
		t.Fatalf("live v2: %q", body)
	}
	start := time.Now()
	if err := decideRollback(t, e.svc, "blog", 0, false); err != nil {
		t.Fatal(err)
	}
	if f, _ := e.svc.GetFlat(ctx, "blog"); f.LiveVersion != 1 || time.Since(start) > 10*time.Second {
		t.Fatalf("rollback live %d in %s", f.LiveVersion, time.Since(start))
	}
	if _, body, _ := get(t, e.priv.URL("blog")); !strings.Contains(body, "one") {
		t.Fatalf("after rollback: %q", body)
	}
}

func TestFailedHealthCheckKeepsLive(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "site", files("index.html", "ok"), core.SaveMeta{}, core.ViaAPI)
	if f := publish(t, e.svc, "site"); f.LiveVersion != 1 {
		t.Fatal(f.LiveVersion)
	}
	// v2 points health at a missing path.
	_, err := e.svc.SaveVersion(ctx, "site", files("index.html", "broken", "flats.json", `{"health":"/missing"}`), core.SaveMeta{}, core.ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	err = decideDeploy(t, e.svc, "site", 0)
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
	publish(t, e.svc, "who")
	permitPortal(t, e.svc, "who")
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
	if !served || hidden {
		t.Fatalf("public flat should be served and visible: served=%v hidden=%v", served, hidden)
	}
	fv, _ := e.svc.GetFlat(ctx, "who")
	if fv.PublicNotice != core.PublicAccessNotice || fv.PublicURL == "" {
		t.Fatalf("public notice missing: %+v", fv)
	}
	// Making a flat private also waits for approval, including for an agent.
	r, err = e.svc.SetVisibility(ctx, "who", store.Private, core.ViaMCP, "")
	if err != nil || r.Status != "pending_approval" {
		t.Fatalf("private transition: %v %+v", err, r)
	}
	if _, err := e.svc.Decide(ctx, r.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, served := e.pub.Hidden("who"); served {
		t.Fatal("private flat still public")
	}
	// The console cannot skip approval either.
	r, _ = e.svc.SetVisibility(ctx, "who", store.PublicListed, core.ViaConsole, "")
	if r.Status != "pending_approval" {
		t.Fatalf("console change should wait: %+v", r)
	}
	if _, err := e.svc.Decide(ctx, r.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	if hidden, served := e.pub.Hidden("who"); !served || hidden {
		t.Fatal("public flat should be served and visible")
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
	publish(t, e.svc, "gone")
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
	publish(t, e.svc, "prev")
	e.svc.SaveVersion(ctx, "prev", files("index.html", "candidate"), core.SaveMeta{}, core.ViaAPI)
	p, err := e.svc.OpenPreview(ctx, "prev", 0)
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
	publish(t, e.svc, "prev")
	if e.priv.Serving(p.Host) {
		t.Fatal("deploy must close previews")
	}
}

func TestRenameRedirect(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "old-name", files("index.html", "page", "about.html", "about"), core.SaveMeta{}, core.ViaAPI)
	publish(t, e.svc, "old-name")
	if _, err := e.svc.RenameSlug(ctx, "old-name", "new-name", core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	code, _, h := get(t, e.priv.URL("old-name")+"/about?x=1")
	if code != http.StatusTemporaryRedirect || h.Get("Location") != e.priv.URL("new-name")+"/about?x=1" {
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
	publish(t, e.svc, "multi")
	permitPortal(t, e.svc, "multi")
	vis, err := e.svc.SetVisibility(ctx, "multi", store.PublicListed, core.ViaMCP, "")
	if err != nil || vis.Notice != core.PendingPublicAccessNotice || strings.Contains(vis.Notice, "This flat is public:") {
		t.Fatalf("pending visibility must carry the notice: %v %+v", err, vis)
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
	err := decideDeploy(t, e.svc, "fresh", 0)
	if err == nil || !strings.Contains(err.Error(), "nothing was live") {
		t.Fatalf("got %v", err)
	}
}

func TestPreDeploySnapshotAndDataRollback(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "datum", files("index.html", "v1"), core.SaveMeta{}, core.ViaAPI)
	publish(t, e.svc, "datum")
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
	publish(t, e.svc, "datum")
	snaps, _ := e.svc.Snapshots("datum")
	if len(snaps) != 1 || !strings.HasPrefix(snaps[0], "before-v2-") {
		t.Fatalf("snapshots %v", snaps)
	}
	db, _ = sql.Open("sqlite", dbPath)
	db.Exec(`UPDATE t SET v='after'`)
	db.Close()
	if err := decideRollback(t, e.svc, "datum", 0, true); err != nil {
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
	publish(t, e.svc, "icon-site")
	fv, _ = e.svc.GetFlat(ctx, "icon-site")
	if !strings.HasSuffix(fv.Thumbnail, "/versions/1/files/favicon.svg") {
		t.Fatalf("favicon fallback: %q", fv.Thumbnail)
	}
	e.svc.SaveVersion(ctx, "icon-site", files("index.html", "x", "shot.png", "png", "flats.json", `{"screenshot":"shot.png"}`), core.SaveMeta{}, core.ViaAPI)
	fv, _ = e.svc.GetFlat(ctx, "icon-site")
	if !strings.HasSuffix(fv.Thumbnail, "/versions/1/files/favicon.svg") {
		t.Fatalf("draft save must not change the published thumbnail: %q", fv.Thumbnail)
	}
	publish(t, e.svc, "icon-site")
	fv, _ = e.svc.GetFlat(ctx, "icon-site")
	if !strings.HasSuffix(fv.Thumbnail, "/versions/2/files/shot.png") {
		t.Fatalf("screenshot should win: %q", fv.Thumbnail)
	}
}

func TestPageViewsCountHTMLRequests(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "counted", files("index.html", "x", "app.js", "js"), core.SaveMeta{}, core.ViaAPI)
	publish(t, e.svc, "counted")
	for i := 0; i < 3; i++ {
		get(t, e.priv.URL("counted")+"/")
	}
	get(t, e.priv.URL("counted")+"/app.js") // assets are not page views
	pv, err := e.svc.PageViews(ctx, "counted", 7)
	if err != nil || len(pv) != 1 || pv[0].Count != 3 {
		t.Fatalf("page views %+v %v", pv, err)
	}
}

func TestSecondRestoreUndoesFirst(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	dbPath := filepath.Join(e.dataDir, "flats", "epoch", "data", "db.sqlite")
	exec := func(q string) {
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	read := func() string {
		db, _ := sql.Open("sqlite", dbPath)
		defer db.Close()
		var v string
		db.QueryRow(`SELECT group_concat(v) FROM t`).Scan(&v)
		return v
	}
	e.svc.SaveVersion(ctx, "epoch", files("index.html", "v1"), core.SaveMeta{}, core.ViaAPI)
	publish(t, e.svc, "epoch")
	os.MkdirAll(filepath.Dir(dbPath), 0o700)
	exec(`CREATE TABLE t (v TEXT)`)
	exec(`INSERT INTO t VALUES ('a')`)
	e.svc.SaveVersion(ctx, "epoch", files("index.html", "v2"), core.SaveMeta{}, core.ViaAPI)
	publish(t, e.svc, "epoch")         // snapshot before-v2 = [a]
	exec(`INSERT INTO t VALUES ('b')`) // live data [a,b]
	if err := decideRollback(t, e.svc, "epoch", 1, true); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "a" {
		t.Fatalf("first restore: %q", got)
	}
	// Rolling forward to v2 with restore_data undoes the restore.
	if err := decideRollback(t, e.svc, "epoch", 2, true); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "a,b" {
		t.Fatalf("second restore should bring back the data replaced by the first, got %q", got)
	}
}
