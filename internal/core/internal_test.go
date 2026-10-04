package core

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/store"
)

func noRedact(s string) string { return s }

// Regression (SEC-5, spec F8): a health path that is not a valid request
// target used to panic in httptest.NewRequest, outside the recovered
// goroutine, and take the whole server down.
func TestHealthCheckBadPathNeverPanics(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	for _, p := range []string{"/a b", "/%zz", "/x\x00y", "//evil.example/", "relative", "/a\tb"} {
		res := healthCheck(ok, p, noRedact)
		if res.OK || !strings.Contains(res.Error, "invalid health path") {
			t.Errorf("health %q: want a failed check with a clear error, got %+v", p, res)
		}
	}
	if res := healthCheck(ok, "/ok?x=1", noRedact); !res.OK {
		t.Fatalf("valid path failed: %+v", res)
	}
	if res := healthCheck(ok, "", noRedact); !res.OK || res.Path != "/" {
		t.Fatalf("empty path should default to /: %+v", res)
	}
	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	if res := healthCheck(panicky, "/", noRedact); res.OK || !strings.Contains(res.Error, "panicked") {
		t.Fatalf("panicking handler: %+v", res)
	}
}

// Regression (SEC-3): the health body and errors are redacted, including
// encoded forms, before truncation.
func TestRedactorAndHealthBody(t *testing.T) {
	const secret = "TOPSECRET-123"
	redact := newRedactor(map[string]string{"API_KEY": secret, "EMPTY": ""})
	in := "raw " + secret + " b64 " + base64.StdEncoding.EncodeToString([]byte(secret)) + " q " + strings.ReplaceAll(secret, "-", "%2D")
	out := redact(in)
	if strings.Contains(out, secret) || strings.Contains(out, base64.StdEncoding.EncodeToString([]byte(secret))) {
		t.Fatalf("secret survived redaction: %q", out)
	}
	if redact("plain text") != "plain text" {
		t.Fatal("redactor changed text without secrets")
	}
	// The secret straddles the 300-byte cut: no prefix of it may remain.
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 295) + secret))
	})
	res := healthCheck(h, "/", redact)
	if strings.Contains(res.BodyHead, "TOPS") {
		t.Fatalf("partial secret in body_head: %q", res.BodyHead)
	}
}

// Regression (correctness C6): a redirect whose target URL is not known yet
// answered a 308 to itself; it is now a 503, and a temporary 307 otherwise.
func TestRedirectHandler(t *testing.T) {
	s := &Service{}
	rec := httptest.NewRecorder()
	s.redirectHandler(func() string { return "" }).ServeHTTP(rec, httptest.NewRequest("GET", "/cart?id=1", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Location") != "" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("empty target: %d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
	rec = httptest.NewRecorder()
	s.redirectHandler(func() string { return "https://new.example/" }).ServeHTTP(rec, httptest.NewRequest("GET", "/cart?id=1", nil))
	if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != "https://new.example/cart?id=1" {
		t.Fatalf("redirect: %d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
}

// Regression (correctness C8): only data/db.sqlite is a database; a user
// file named *.db is copied as it is.
func TestSnapshotDataCopiesLookalikeFiles(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()+"/copy"
	if err := writeFile(src+"/files/notes.db", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(src+"/files/x.sqlite", "not sqlite"); err != nil {
		t.Fatal(err)
	}
	if err := snapshotData(src, dst); err != nil {
		t.Fatalf("snapshotData: %v", err)
	}
	if b, _ := readFile(dst + "/files/notes.db"); b != "hello" {
		t.Fatalf("notes.db copy = %q", b)
	}
}

func writeFile(p, data string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(data), 0o600)
}

func readFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}

// memNet is a minimal PrivateNet for tests inside the package.
type memNet struct {
	mu    sync.Mutex
	hosts map[string]http.Handler
}

func (n *memNet) Serve(_ context.Context, host string, h http.Handler, _ bool) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.hosts[host] = h
	return n.URL(host), nil
}

func (n *memNet) Stop(host string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.hosts, host)
	return nil
}

func (n *memNet) URL(host string) string { return "http://" + host + ".test" }
func (n *memNet) Close() error           { return nil }

// Status reports every served host as starting, like a tailnet node waiting
// for its HTTPS certificate.
func (n *memNet) Status() NetStatus {
	n.mu.Lock()
	defer n.mu.Unlock()
	st := NetStatus{Kind: "mem", Enabled: true}
	for h := range n.hosts {
		st.Hosts = append(st.Hosts, HostInfo{Host: h, URL: n.URL(h), State: "starting", Detail: "waiting for its HTTPS certificate"})
	}
	return st
}

func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(context.Background(), Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); st.Close() })
	return s, dir
}

// Regression (correctness C2): two renames to one target that both passed
// the existence check must not move one flat's directory under another slug.
// Holding the target's lock makes both renames wait at the same point; the
// one that goes second must notice the target now exists.
func TestConcurrentRenameSameTarget(t *testing.T) {
	ctx := context.Background()
	s, dir := newTestService(t)
	for i := 0; i < 20; i++ {
		a, c, b := fmt.Sprintf("src-a%d", i), fmt.Sprintf("src-c%d", i), fmt.Sprintf("dst-b%d", i)
		if _, err := s.SaveVersion(ctx, a, []bundle.File{{Path: "index.html", Data: []byte("page-a")}}, SaveMeta{}, ViaAPI); err != nil {
			t.Fatal(err)
		}
		if _, err := approvedInternalDeploy(t, s, ctx, a, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateFlat(ctx, c, "", ViaAPI); err != nil {
			t.Fatal(err)
		}
		unlock := s.lock(b)
		var wg sync.WaitGroup
		// a queues for the target first, so it wins; c then finds b taken.
		for _, from := range []string{a, c} {
			wg.Add(1)
			go func() { defer wg.Done(); s.RenameSlug(ctx, from, b, ViaAPI) }()
			time.Sleep(10 * time.Millisecond)
		}
		unlock()
		wg.Wait()
		fb, err := s.st.GetFlat(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		if fb.LiveVersion == 1 {
			if _, err := os.Stat(filepath.Join(dir, "flats", b, "versions", "1")); err != nil {
				t.Fatalf("round %d: %s is live but its files are gone", i, b)
			}
		}
		for _, other := range []string{a, c} {
			if _, err := s.st.GetFlat(ctx, other); err == nil {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, "flats", other)); err == nil {
				t.Fatalf("round %d: files ended up under the unused slug %s", i, other)
			}
		}
	}
}

func TestShortSecretsAreRedacted(t *testing.T) {
	r := newRedactor(map[string]string{"TOKEN": "s3c", "EMPTY": ""})
	if got := r("token s3c"); got != "token "+redacted {
		t.Fatalf("got %q", got)
	}
	if got := r("ordinary configuration"); got != "ordinary configuration" {
		t.Fatalf("got %q", got)
	}
}

func TestFailedSnapshotsDoNotEvictRegularOnes(t *testing.T) {
	s := &Service{cfg: Config{DataDir: t.TempDir()}}
	dir := s.snapshotDir("app")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < keepSnapshots; i++ {
		writeFile(filepath.Join(dir, fmt.Sprintf("before-v%d-%d.sqlite", i+1, 1000+i)), "x")
	}
	for i := 0; i < 8; i++ {
		writeFile(filepath.Join(dir, fmt.Sprintf("failed-v%d-%d.sqlite", 50+i, 5000+i)), "x")
		s.pruneSnapshots("app")
	}
	snaps, _ := s.Snapshots("app")
	regular, failed := 0, 0
	for _, n := range snaps {
		if strings.HasPrefix(n, "failed-") {
			failed++
		} else {
			regular++
		}
	}
	if regular != keepSnapshots || failed != keepFailedSnapshots {
		t.Fatalf("regular=%d failed=%d: %v", regular, failed, snaps)
	}
}

// Regression (real-tailnet gate): a private host that does not answer yet is
// reported with the flat and the preview, so agents wait instead of fetching.
func TestViewsReportHostState(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	if _, err := s.SaveVersion(ctx, "pend", []bundle.File{{Path: "index.html", Data: []byte("x")}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	f, err := s.GetFlat(ctx, "pend")
	if err != nil {
		t.Fatal(err)
	}
	if f.PrivateState != "" {
		t.Errorf("undeployed flat has host state %q", f.PrivateState)
	}
	r, err := approvedInternalDeploy(t, s, ctx, "pend", 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.Flat.PrivateState != "starting" || !strings.Contains(r.Flat.PrivateDetail, "certificate") {
		t.Errorf("deploy result host state %q %q", r.Flat.PrivateState, r.Flat.PrivateDetail)
	}
	p, err := s.OpenPreview(ctx, "pend", 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "starting" {
		t.Errorf("preview state %q", p.State)
	}
	ps, err := s.ListPreviews(ctx, "pend")
	if err != nil || len(ps) != 1 || ps[0].State != "starting" {
		t.Errorf("listed previews %+v %v", ps, err)
	}
}

func approvedInternalDeploy(t *testing.T, s *Service, ctx context.Context, slug string, n int) (DeployResult, error) {
	t.Helper()
	_, err := s.Deploy(ctx, slug, n, ViaAPI)
	var p *PendingApproval
	if !errors.As(err, &p) {
		return DeployResult{}, err
	}
	_, err = s.Decide(ctx, p.Approval.ID, true)
	if err != nil {
		return DeployResult{}, err
	}
	f, err := s.GetFlat(ctx, slug)
	return DeployResult{Flat: f, Version: f.LiveVersion}, err
}

func TestDurationOfSaturates(t *testing.T) {
	if d := durationOf(2, 24*time.Hour); d != 48*time.Hour {
		t.Fatalf("2 days = %s", d)
	}
	for _, n := range []int64{1<<53 - 1, math.MaxInt64 / int64(time.Second)} {
		if d := durationOf(n+1, time.Second); d != math.MaxInt64 {
			t.Fatalf("%d seconds = %s, want the longest duration", n+1, d)
		}
	}
}
