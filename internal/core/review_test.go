package core_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/store"
)

// fakeRuntime runs server flats in-process. Each test sets start to decide
// what an instance of a version does.
type fakeRuntime struct {
	mu     sync.Mutex
	start  func(spec core.RuntimeSpec) (http.Handler, error)
	starts atomic.Int32
}

type fakeInst struct {
	http.Handler
}

func (fakeInst) Stop() {}

func (r *fakeRuntime) Start(_ context.Context, spec core.RuntimeSpec) (core.Instance, error) {
	r.starts.Add(1)
	r.mu.Lock()
	start := r.start
	r.mu.Unlock()
	if start == nil {
		return fakeInst{http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintf(w, "v%d", spec.Version) })}, nil
	}
	h, err := start(spec)
	if err != nil {
		return nil, err
	}
	return fakeInst{h}, nil
}

func (r *fakeRuntime) set(f func(spec core.RuntimeSpec) (http.Handler, error)) {
	r.mu.Lock()
	r.start = f
	r.mu.Unlock()
}

type rtEnv struct {
	*env
	rt *fakeRuntime
}

func newRuntimeEnv(t *testing.T) *rtEnv {
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
	rt := &fakeRuntime{}
	svc, err := core.New(context.Background(), core.Config{DataDir: dir, Store: st, Private: priv, Runtime: rt,
		ConsoleURL: func() string { return "http://console.test" }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close(); priv.Close(); st.Close() })
	return &rtEnv{env: &env{dataDir: dir, svc: svc, priv: priv, st: st}, rt: rt}
}

func saveServer(t *testing.T, e *env, slugName, manifestExtra string) int {
	t.Helper()
	m := `{"kind":"server"` + manifestExtra + `}`
	v, err := e.svc.SaveVersion(context.Background(), slugName, files("flats.json", m, "server.js", "export default {}"), core.SaveMeta{}, core.ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	return v.Number
}

func dbPath(e *env, slugName string) string {
	return filepath.Join(e.dataDir, "flats", slugName, "data", "db.sqlite")
}

func execDB(t *testing.T, path string, stmts ...string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func queryDB(t *testing.T, path, q string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// Regression (SEC-3): secret values must not reach the health result or the
// stored runtime logs, raw or encoded.
func TestSecretsRedactedFromHealthAndLogs(t *testing.T) {
	ctx := context.Background()
	e := newRuntimeEnv(t)
	const secret = "TOPSECRET-123"
	e.rt.set(func(spec core.RuntimeSpec) (http.Handler, error) {
		key := spec.Env["API_KEY"]
		spec.Log("info", "leak:"+key)
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			spec.Log("info", fmt.Sprintf("json:%q", key))
			fmt.Fprint(w, "ok:"+key)
		}), nil
	})
	saveServer(t, e.env, "sec-flat", "")
	if err := e.svc.SetSecret(ctx, "sec-flat", "API_KEY", secret, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Deploy(ctx, "sec-flat", 1, core.ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Health.BodyHead, secret) || !strings.Contains(res.Health.BodyHead, "[redacted]") {
		t.Fatalf("body_head leaks the secret: %q", res.Health.BodyHead)
	}
	evs, _ := e.svc.Events(ctx, "sec-flat", "", 0, 1000)
	for _, ev := range evs {
		if strings.Contains(ev.Message, secret) || strings.Contains(string(ev.Data), secret) {
			t.Fatalf("event leaks the secret: %+v", ev)
		}
	}
	// A failing start must not leak it either (worker stderr is in the error).
	e.rt.set(func(spec core.RuntimeSpec) (http.Handler, error) {
		return nil, errors.New("worker exited: stderr: " + spec.Env["API_KEY"])
	})
	_, err = e.svc.Deploy(ctx, "sec-flat", 1, core.ViaMCP)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("start error leaks the secret: %v", err)
	}
}

// Regression (correctness C4, spec F1): the pre-deploy snapshot is taken
// before the candidate starts, and a failed server deploy names it.
func TestSnapshotBeforeCandidateStarts(t *testing.T) {
	ctx := context.Background()
	e := newRuntimeEnv(t)
	e.rt.set(func(spec core.RuntimeSpec) (http.Handler, error) {
		execDB(t, filepath.Join(spec.DataDir, "db.sqlite"), `CREATE TABLE IF NOT EXISTS log (v TEXT)`, fmt.Sprintf(`INSERT INTO log VALUES ('started v%d')`, spec.Version))
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if spec.Version == 3 {
				w.WriteHeader(500)
			}
		}), nil
	})
	saveServer(t, e.env, "snap", "")
	saveServer(t, e.env, "snap", "")
	saveServer(t, e.env, "snap", "")
	if _, err := e.svc.Deploy(ctx, "snap", 1, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Deploy(ctx, "snap", 2, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	snaps, _ := e.svc.Snapshots("snap")
	if len(snaps) != 1 || !strings.HasPrefix(snaps[0], "before-v2-") {
		t.Fatalf("snapshots %v", snaps)
	}
	rows := queryDB(t, filepath.Join(e.dataDir, "flats", "snap", "snapshots", snaps[0]), `SELECT v FROM log`)
	if strings.Join(rows, ",") != "started v1" {
		t.Fatalf("before-v2 snapshot must not contain anything v2 did, got %v", rows)
	}
	_, err := e.svc.Deploy(ctx, "snap", 3, core.ViaAPI)
	var de *core.DeployError
	if !errors.As(err, &de) || !strings.Contains(err.Error(), "may have changed") || !strings.Contains(err.Error(), "failed-v3-") {
		t.Fatalf("failed server deploy must warn about live data and name the snapshot: %v", err)
	}
}

// Regression (correctness C1, spec F3, interfaces F5): a restore_data
// rollback whose target is missing or fails its check leaves the data alone.
func TestRestoreDataBadTargetKeepsData(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "datum", files("index.html", "v1"), core.SaveMeta{}, core.ViaAPI)
	e.svc.Deploy(ctx, "datum", 1, core.ViaAPI)
	db := dbPath(e, "datum")
	execDB(t, db, `CREATE TABLE t (v TEXT)`, `INSERT INTO t VALUES ('before')`)
	e.svc.SaveVersion(ctx, "datum", files("index.html", "v2"), core.SaveMeta{}, core.ViaAPI)
	if _, err := e.svc.Deploy(ctx, "datum", 2, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	execDB(t, db, `UPDATE t SET v='precious-after'`)
	e.svc.SaveVersion(ctx, "datum", files("index.html", "v3", "flats.json", `{"health":"/missing"}`), core.SaveMeta{}, core.ViaAPI)
	for _, to := range []int{99, 3} {
		if _, err := e.svc.RollbackWithData(ctx, "datum", to, true, core.ViaMCP); err == nil {
			t.Fatalf("rollback to %d succeeded", to)
		}
		if got := queryDB(t, db, `SELECT v FROM t`); len(got) != 1 || got[0] != "precious-after" {
			t.Fatalf("failed rollback to %d changed the data: %v", to, got)
		}
		if _, body, _ := get(t, e.priv.URL("datum")); body != "v2" {
			t.Fatalf("after failed rollback to %d live serves %q", to, body)
		}
	}
	// A successful restore keeps the replaced data as a snapshot.
	if _, err := e.svc.RollbackWithData(ctx, "datum", 1, true, core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	if got := queryDB(t, db, `SELECT v FROM t`); got[0] != "before" {
		t.Fatalf("data not restored: %v", got)
	}
	snaps, _ := e.svc.Snapshots("datum")
	if len(snaps) == 0 || !strings.HasPrefix(snaps[0], "before-v1-") {
		t.Fatalf("no backup of the replaced data: %v", snaps)
	}
	backup := queryDB(t, filepath.Join(e.dataDir, "flats", "datum", "snapshots", snaps[0]), `SELECT v FROM t`)
	if backup[0] != "precious-after" {
		t.Fatalf("backup holds %v", backup)
	}
}

// Regression (correctness C1): when the target fails only after the data was
// swapped, the current database comes back and the old version restarts.
func TestRestoreDataFailureAfterSwapPutsDataBack(t *testing.T) {
	ctx := context.Background()
	e := newRuntimeEnv(t)
	saveServer(t, e.env, "swap", "")
	saveServer(t, e.env, "swap", "")
	db := dbPath(e.env, "swap")
	if _, err := e.svc.Deploy(ctx, "swap", 1, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	execDB(t, db, `CREATE TABLE t (v TEXT)`, `INSERT INTO t VALUES ('old')`)
	if _, err := e.svc.Deploy(ctx, "swap", 2, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	execDB(t, db, `UPDATE t SET v='current'`)
	// Version 1 passes its trial run but cannot start on the live data.
	var v1Starts atomic.Int32
	e.rt.set(func(spec core.RuntimeSpec) (http.Handler, error) {
		if spec.Version == 1 && v1Starts.Add(1) > 1 {
			return nil, errors.New("cannot start")
		}
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintf(w, "v%d", spec.Version) }), nil
	})
	_, err := e.svc.RollbackWithData(ctx, "swap", 1, true, core.ViaAPI)
	if err == nil || !strings.Contains(err.Error(), "put back") {
		t.Fatalf("want a failure that says the data was put back, got %v", err)
	}
	if got := queryDB(t, db, `SELECT v FROM t`); len(got) != 1 || got[0] != "current" {
		t.Fatalf("data after failed restore: %v", got)
	}
	if _, body, _ := get(t, e.priv.URL("swap")); body != "v2" {
		t.Fatalf("old version not restarted: %q", body)
	}
}

// Regression (correctness C3, interfaces F2): ClosePreview only closes
// previews, and a later deploy re-serves a host that went away.
func TestClosePreviewOnlyClosesPreviews(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "redep", files("index.html", "one"), core.SaveMeta{}, core.ViaAPI)
	e.svc.Deploy(ctx, "redep", 1, core.ViaAPI)
	for _, host := range []string{"redep", "whatever", "flats"} {
		if err := e.svc.ClosePreview(ctx, host); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("ClosePreview(%s) = %v, want ErrNotFound", host, err)
		}
	}
	if !e.priv.Serving("redep") {
		t.Fatal("ClosePreview stopped a live flat")
	}
	p, err := e.svc.OpenPreview(ctx, "redep", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ClosePreview(ctx, p.Host); err != nil || e.priv.Serving(p.Host) {
		t.Fatalf("real preview not closed: %v", err)
	}
	// The host goes away behind the service's back: a deploy brings it back.
	e.priv.Stop("redep")
	e.svc.SaveVersion(ctx, "redep", files("index.html", "two"), core.SaveMeta{}, core.ViaAPI)
	if _, err := e.svc.Deploy(ctx, "redep", 2, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, body, _ := get(t, e.priv.URL("redep")); body != "two" {
		t.Fatalf("redeploy did not re-serve the host: %q", body)
	}
}

// Regression (correctness C5): an undeployed rename does not record a
// redirect, and a redirect from a deployed rename reserves the old slug and
// never shadows a flat after a restart.
func TestRenameRedirectReservation(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "alpha", files("index.html", "old-alpha"), core.SaveMeta{}, core.ViaAPI)
	if _, err := e.svc.RenameSlug(ctx, "alpha", "beta", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SaveVersion(ctx, "alpha", files("index.html", "new-alpha"), core.SaveMeta{}, core.ViaAPI); err != nil {
		t.Fatalf("the slug of an undeployed rename should be free: %v", err)
	}
	e.svc.Deploy(ctx, "alpha", 1, core.ViaAPI)
	e.svc.Deploy(ctx, "beta", 1, core.ViaAPI)
	svc2, err := core.New(ctx, core.Config{DataDir: e.dataDir, Store: e.st, Private: e.priv, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	if code, body, _ := get(t, e.priv.URL("alpha")); code != 200 || body != "new-alpha" {
		t.Fatalf("after restart alpha: %d %q", code, body)
	}
	if r := svc2.Redirects(); r["alpha"] != "" {
		t.Fatalf("a redirect shadows the flat alpha after restart: %v", r)
	}
	// A deployed rename reserves its old slug.
	e.svc.SaveVersion(ctx, "gamma", files("index.html", "g"), core.SaveMeta{}, core.ViaAPI)
	e.svc.Deploy(ctx, "gamma", 1, core.ViaAPI)
	if _, err := e.svc.RenameSlug(ctx, "gamma", "delta", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateFlat(ctx, "gamma", "", core.ViaAPI); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("redirected slug must be reserved, got %v", err)
	}
	// ...but the flat itself may take it back.
	if _, err := e.svc.RenameSlug(ctx, "delta", "gamma", core.ViaAPI); err != nil {
		t.Fatalf("renaming back: %v", err)
	}
	if code, body, _ := get(t, e.priv.URL("gamma")); code != 200 || body != "g" {
		t.Fatalf("gamma after renaming back: %d %q", code, body)
	}
}

// Regression (correctness C7): a second rename keeps the first redirect for
// its whole window, pointing at the newest slug.
func TestDoubleRenameKeepsFirstRedirect(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "aaa", files("index.html", "x"), core.SaveMeta{}, core.ViaAPI)
	e.svc.Deploy(ctx, "aaa", 1, core.ViaAPI)
	if _, err := e.svc.RenameSlug(ctx, "aaa", "bbb", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RenameSlug(ctx, "bbb", "ccc", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	e.svc.Sweep(ctx)
	r := e.svc.Redirects()
	if r["aaa"] != "ccc" || r["bbb"] != "ccc" {
		t.Fatalf("redirects after sweep: %v", r)
	}
	code, _, h := get(t, e.priv.URL("aaa")+"/p")
	if code != http.StatusTemporaryRedirect || h.Get("Location") != e.priv.URL("ccc")+"/p" {
		t.Fatalf("aaa: %d %q", code, h.Get("Location"))
	}
}

// Regression (correctness C9): concurrent first saves of a new slug both
// succeed.
func TestConcurrentFirstSave(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("racy-%d", i)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for j := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[j] = e.svc.SaveVersion(ctx, name, files("index.html", "x"), core.SaveMeta{}, core.ViaAPI)
			}()
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", i, err)
			}
		}
	}
}

// Regression (correctness C10): page views counted before a rename survive.
func TestRenameKeepsPageViews(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "seen", files("index.html", "x"), core.SaveMeta{}, core.ViaAPI)
	e.svc.Deploy(ctx, "seen", 1, core.ViaAPI)
	for i := 0; i < 3; i++ {
		get(t, e.priv.URL("seen")+"/")
	}
	if _, err := e.svc.RenameSlug(ctx, "seen", "seen-two", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	pv, err := e.svc.PageViews(ctx, "seen-two", 7)
	if err != nil || len(pv) != 1 || pv[0].Count != 3 {
		t.Fatalf("page views after rename: %+v %v", pv, err)
	}
}

func TestTopPagesFlushAndRename(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, err := e.svc.SaveVersion(ctx, "traffic", files("index.html", "x"), core.SaveMeta{}, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Deploy(ctx, "traffic", 1, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/?token=private", "/?token=another", "/notes", "/asset.css"} {
		get(t, e.priv.URL("traffic")+path)
	}
	if _, err := e.svc.RenameSlug(ctx, "traffic", "traffic-new", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	pages, err := e.svc.TopPages(ctx, "traffic-new", 7)
	if err != nil || len(pages) != 2 || pages[0].Path != "/" || pages[0].Count != 2 || pages[1].Path != "/notes" {
		t.Fatalf("top pages: %+v, %v", pages, err)
	}
	pages, err = e.svc.TopPages(ctx, "traffic-new", 7)
	if err != nil || pages[0].Count != 2 {
		t.Fatalf("flush counted twice: %+v, %v", pages, err)
	}
}

// Regression (correctness C8): previews of server flats work with *.db user
// files, are capped and respect the disk quota without leaving copies.
func TestServerPreviewData(t *testing.T) {
	ctx := context.Background()
	e := newRuntimeEnv(t)
	saveServer(t, e.env, "pvflat", "")
	e.svc.Deploy(ctx, "pvflat", 1, core.ViaAPI)
	files := filepath.Join(e.dataDir, "flats", "pvflat", "data", "files")
	os.MkdirAll(files, 0o700)
	os.WriteFile(filepath.Join(files, "notes.db"), []byte("hello"), 0o600)
	var hosts []string
	for i := 0; i < 5; i++ {
		p, err := e.svc.OpenPreview(ctx, "pvflat", 1)
		if err != nil {
			t.Fatalf("preview %d: %v", i, err)
		}
		hosts = append(hosts, p.Host)
	}
	if _, err := e.svc.OpenPreview(ctx, "pvflat", 1); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("sixth preview: %v", err)
	}
	for _, h := range hosts {
		e.svc.ClosePreview(ctx, h)
	}
	if _, err := e.svc.UpdateSettings(ctx, map[string]string{core.SetDiskQuotaBytes: "10"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.OpenPreview(ctx, "pvflat", 1); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("preview over quota: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.dataDir, "flats", "pvflat", "previews")); len(entries) != 0 {
		t.Fatalf("preview copies left behind: %d", len(entries))
	}
}

// Regression (spec F4): settings are validated before they are stored.
func TestSettingsValidation(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	for k, v := range map[string]string{
		core.SetPortalRelays:   "ftp://bad host",
		core.SetPortalDiscover: "maybe",
		core.SetPortalMaxRelay: "0",
		core.SetEventsKeep:     "0",
		"nope":                 "1",
	} {
		if _, err := e.svc.UpdateSettings(ctx, map[string]string{k: v}); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("%s=%q: want ErrInvalid, got %v", k, v, err)
		}
	}
	all, _ := e.svc.Settings(ctx)
	if all[core.SetPortalRelays] != "" {
		t.Fatalf("invalid relay stored: %q", all[core.SetPortalRelays])
	}
	all, err := e.svc.UpdateSettings(ctx, map[string]string{core.SetPortalRelays: " relay.example.com , https://relay.example.com/ ", core.SetPortalDiscover: "FALSE", core.SetPortalMaxRelay: "5"})
	if err != nil {
		t.Fatal(err)
	}
	if all[core.SetPortalRelays] != "https://relay.example.com" || all[core.SetPortalDiscover] != "false" || all[core.SetPortalMaxRelay] != "5" {
		t.Fatalf("normalized settings: %v", all)
	}
}

// Regression (spec F7): the log of a flat is trimmed to events_keep and
// runtime log lines are rate limited.
func TestEventsPrunedAndRuntimeLogsLimited(t *testing.T) {
	ctx := context.Background()
	e := newRuntimeEnv(t)
	if _, err := e.svc.UpdateSettings(ctx, map[string]string{core.SetEventsKeep: "10"}); err != nil {
		t.Fatal(err)
	}
	e.svc.CreateFlat(ctx, "chatty", "", core.ViaAPI)
	for i := 0; i < 250; i++ {
		e.svc.Event(ctx, "chatty", "info", "test", fmt.Sprint(i), nil)
	}
	e.svc.Sweep(ctx)
	evs, _ := e.svc.Events(ctx, "chatty", "", 0, 1000)
	if len(evs) != 10 || evs[9].Message != "249" {
		t.Fatalf("after sweep %d events (last %q)", len(evs), evs[len(evs)-1].Message)
	}

	if _, err := e.svc.UpdateSettings(ctx, map[string]string{core.SetEventsKeep: "5000"}); err != nil {
		t.Fatal(err)
	}
	e.rt.set(func(spec core.RuntimeSpec) (http.Handler, error) {
		for i := 0; i < 500; i++ {
			spec.Log("info", "line")
		}
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), nil
	})
	saveServer(t, e.env, "logger", "")
	if _, err := e.svc.Deploy(ctx, "logger", 1, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	e.svc.Sweep(ctx)
	rt, _ := e.svc.Events(ctx, "logger", "runtime", 0, 1000)
	if len(rt) > 60 || !strings.Contains(rt[len(rt)-1].Message, "dropped") {
		t.Fatalf("%d runtime events stored; last %q", len(rt), rt[len(rt)-1].Message)
	}
}

// Regression (spec F10): keep_versions counts versions besides the live one.
func TestRetentionKeepsNPlusLive(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.UpdateSettings(ctx, map[string]string{core.SetKeepVersions: "3"})
	for i := 1; i <= 6; i++ {
		e.svc.SaveVersion(ctx, "ret", files("index.html", fmt.Sprint(i)), core.SaveMeta{}, core.ViaAPI)
		if _, err := e.svc.Deploy(ctx, "ret", i, core.ViaAPI); err != nil {
			t.Fatal(err)
		}
	}
	vs, _ := e.svc.ListVersions(ctx, "ret")
	kept := 0
	for _, v := range vs {
		if !v.Pruned {
			kept++
		}
	}
	if kept != 4 {
		t.Fatalf("kept %d versions, want 3 plus the live one", kept)
	}
}

// Regression (interfaces F3): redeploying the live version does not break
// the default rollback target.
func TestRollbackAfterRedeployOfLive(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "redo", files("index.html", "one"), core.SaveMeta{}, core.ViaAPI)
	e.svc.SaveVersion(ctx, "redo", files("index.html", "two"), core.SaveMeta{}, core.ViaAPI)
	for _, v := range []int{1, 2, 2} {
		if _, err := e.svc.Deploy(ctx, "redo", v, core.ViaAPI); err != nil {
			t.Fatal(err)
		}
	}
	rb, err := e.svc.Rollback(ctx, "redo", 0, core.ViaAPI)
	if err != nil || rb.Version != 1 {
		t.Fatalf("rollback after redeploy: %+v %v", rb, err)
	}
}

// Regression (interfaces F6): per-flat reads report unknown flats.
func TestReadsOfUnknownFlat(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, err := e.svc.Events(ctx, "nope", "", 0, 10); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Events: %v", err)
	}
	if _, err := e.svc.Deployments(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Deployments: %v", err)
	}
	if _, err := e.svc.ListPreviews(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ListPreviews: %v", err)
	}
	if _, err := e.svc.PageViews(ctx, "nope", 7); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("PageViews: %v", err)
	}
	e.svc.CreateFlat(ctx, "empty", "", core.ViaAPI)
	ds, _ := e.svc.Deployments(ctx, "empty")
	pv, _ := e.svc.PageViews(ctx, "empty", 7)
	if ds == nil || pv == nil {
		t.Errorf("empty results should be empty lists, not nil")
	}
}

// Regression (interfaces F7): of a concurrent approve and reject, exactly
// one wins, and the flat's exposure matches the recorded outcome.
func TestApproveRejectRace(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "race", files("index.html", "x"), core.SaveMeta{}, core.ViaAPI)
	e.svc.Deploy(ctx, "race", 1, core.ViaAPI)
	for i := 0; i < 20; i++ {
		r, err := e.svc.SetVisibility(ctx, "race", store.PublicUnlisted, core.ViaMCP, "")
		if err != nil || r.Approval == nil {
			t.Fatalf("round %d: %v %+v", i, err, r)
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for j, approve := range []bool{true, false} {
			wg.Add(1)
			go func() { defer wg.Done(); _, errs[j] = e.svc.Decide(ctx, r.Approval.ID, approve) }()
		}
		wg.Wait()
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("round %d: want exactly one decision, got %v / %v", i, errs[0], errs[1])
		}
		a, _ := e.svc.GetApproval(ctx, r.Approval.ID)
		_, public := e.pub.Hidden("race")
		if (a.Status == "approved") != public {
			t.Fatalf("round %d: approval %s but public=%v", i, a.Status, public)
		}
		if public {
			if _, err := e.svc.SetVisibility(ctx, "race", store.Private, core.ViaConsole, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Regression (interfaces F8): errors carry a kind the API maps with
// errors.Is instead of matching message text.
func TestErrorKinds(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.CreateFlat(ctx, "kinds", "", core.ViaAPI)
	_, dup := e.svc.CreateFlat(ctx, "kinds", "", core.ViaAPI)
	_, console := e.svc.CreateFlat(ctx, "other", "", core.ViaConsole)
	_, badSlug := e.svc.CreateFlat(ctx, "Bad_Slug", "", core.ViaAPI)
	_, badVis := e.svc.SetVisibility(ctx, "kinds", "secret", core.ViaAPI, "")
	agentSecret := e.svc.SetSecret(ctx, "kinds", "K", "v", core.ViaMCP)
	badSecret := e.svc.SetSecret(ctx, "kinds", "lower", "v", core.ViaConsole)
	for name, c := range map[string]struct {
		err  error
		kind error
	}{
		"duplicate create": {dup, core.ErrConflict},
		"console create":   {console, core.ErrForbidden},
		"bad slug":         {badSlug, core.ErrInvalid},
		"bad visibility":   {badVis, core.ErrInvalid},
		"agent secret":     {agentSecret, core.ErrForbidden},
		"bad secret name":  {badSecret, core.ErrInvalid},
	} {
		if !errors.Is(c.err, c.kind) {
			t.Errorf("%s: %v is not %v", name, c.err, c.kind)
		}
	}
	if !strings.Contains(dup.Error(), "already exists") {
		t.Errorf("message changed: %v", dup)
	}

	// Public exposure disabled on this host.
	dir := t.TempDir()
	st, _ := store.Open(filepath.Join(dir, "flats.db"))
	defer st.Close()
	svc, err := core.New(ctx, core.Config{DataDir: dir, Store: st, Private: e.priv, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	svc.CreateFlat(ctx, "nopub", "", core.ViaAPI)
	if _, err := svc.SetVisibility(ctx, "nopub", store.PublicListed, core.ViaConsole, ""); !errors.Is(err, core.ErrUnavailable) {
		t.Errorf("public disabled: %v", err)
	}
}

// RestoreSnapshot puts a named snapshot back under the live version and
// keeps the replaced data (the recovery path a failed deploy points at).
func TestRestoreSnapshot(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.SaveVersion(ctx, "named", files("index.html", "v1"), core.SaveMeta{}, core.ViaAPI)
	e.svc.Deploy(ctx, "named", 1, core.ViaAPI)
	db := dbPath(e, "named")
	execDB(t, db, `CREATE TABLE t (v TEXT)`, `INSERT INTO t VALUES ('good')`)
	e.svc.SaveVersion(ctx, "named", files("index.html", "v2"), core.SaveMeta{}, core.ViaAPI)
	e.svc.Deploy(ctx, "named", 2, core.ViaAPI)
	execDB(t, db, `UPDATE t SET v='damaged'`)
	if _, err := e.svc.RestoreSnapshot(ctx, "named", "../flats.db", core.ViaConsole); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("bad name: %v", err)
	}
	snaps, _ := e.svc.Snapshots("named")
	if _, err := e.svc.RestoreSnapshot(ctx, "named", snaps[0], core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if got := queryDB(t, db, `SELECT v FROM t`); got[0] != "good" {
		t.Fatalf("data %v", got)
	}
	if _, body, _ := get(t, e.priv.URL("named")); body != "v2" {
		t.Fatalf("live %q", body)
	}
	snaps, _ = e.svc.Snapshots("named")
	if !strings.HasPrefix(snaps[0], "before-restore-") {
		t.Fatalf("replaced data not kept: %v", snaps)
	}
	// The live version's own snapshot still drives restore_data rollbacks.
	if _, err := e.svc.RollbackWithData(ctx, "named", 1, true, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
}
