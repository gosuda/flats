package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func startLocal(t *testing.T) *Host {
	t.Helper()
	o := localOptions(t.TempDir())
	o.Overrides["host.server_runtime"] = "false"
	h, err := Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func tarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(data)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func call(t *testing.T, method, u string, body io.Reader, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, u, body)
	req.Header.Set("X-Flats-Client", "cli")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: %d %s", method, u, resp.StatusCode, b)
		}
	}
	return resp.StatusCode
}

func fetch(t *testing.T, u string) (int, string) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestMVPGate keeps the original size, latency and origin checks while requiring
// an explicit operator decision before uploaded or rollback bytes become live.
func TestMVPGate(t *testing.T) {
	h, operator := operatorHost(t)
	base := "http://" + h.Addr()
	blob := make([]byte, 10<<20-4096)
	rand.Read(blob) // incompressible: the upload really is ~10 MB
	archive := tarGz(t, map[string][]byte{"index.html": []byte("<h1>v1</h1>"), "media/blob.bin": blob})
	if len(archive) < 10<<20-8192 {
		t.Fatalf("archive is only %d bytes", len(archive))
	}
	approve := func(result map[string]any) {
		t.Helper()
		approval, ok := result["approval"].(map[string]any)
		if !ok || result["status"] != "pending_approval" {
			t.Fatalf("missing pending result: %+v", result)
		}
		var decision struct{ Status string }
		code := operatorCall(t, h, operator, "POST", "/console/api/approvals/"+approval["id"].(string)+"/approve", strings.NewReader("{}"), &decision)
		if code != 200 || decision.Status != "approved" {
			t.Fatalf("decision: %d %+v", code, decision)
		}
	}
	currentURL := func() string {
		t.Helper()
		var flat struct {
			PrivateURL string `json:"private_url"`
		}
		if code := call(t, "GET", base+"/api/flats/big-site", nil, &flat); code != 200 {
			t.Fatalf("flat status %d", code)
		}
		return flat.PrivateURL
	}
	start := time.Now()
	var pending map[string]any
	code := call(t, "POST", base+"/api/flats/big-site/versions?deploy=1&git_sha=deadbeef", bytes.NewReader(archive), &pending)
	if code != 202 {
		t.Fatalf("upload pending status: %d %+v", code, pending)
	}
	if c, _ := fetch(t, h.Private.URL("big-site")); c == 200 {
		t.Fatal("upload became live before approval")
	}
	approve(pending)
	liveURL := currentURL()
	if c, body := fetch(t, liveURL); c != 200 || !strings.Contains(body, "v1") {
		t.Fatalf("live: %d %q", c, body)
	}
	uploadToLive := time.Since(start)
	t.Logf("GATE upload(10MB)+explicit decision->live: %s (limit 30s)", uploadToLive)
	if uploadToLive > 30*time.Second {
		t.Fatalf("upload to live took %s", uploadToLive)
	}
	small := tarGz(t, map[string][]byte{"index.html": []byte("<h1>v2</h1>")})
	if code := call(t, "POST", base+"/api/flats/big-site/versions?deploy=1", bytes.NewReader(small), &pending); code != 202 {
		t.Fatalf("v2 pending: %d", code)
	}
	if _, body := fetch(t, liveURL); !strings.Contains(body, "v1") {
		t.Fatalf("pending v2 replaced current: %q", body)
	}
	approve(pending)
	if _, body := fetch(t, liveURL); !strings.Contains(body, "v2") {
		t.Fatalf("v2 not live: %q", body)
	}
	start = time.Now()
	if code := call(t, "POST", base+"/api/flats/big-site/rollback", strings.NewReader(`{"version":1}`), &pending); code != 202 {
		t.Fatalf("rollback pending: %d %+v", code, pending)
	}
	if _, body := fetch(t, liveURL); !strings.Contains(body, "v2") {
		t.Fatalf("pending rollback replaced current: %q", body)
	}
	approve(pending)
	if _, body := fetch(t, liveURL); !strings.Contains(body, "v1") {
		t.Fatalf("rollback not live: %q", body)
	}
	rollback := time.Since(start)
	t.Logf("GATE rollback+explicit decision: %s (limit 10s)", rollback)
	if rollback > 10*time.Second {
		t.Fatalf("rollback took %s", rollback)
	}

	// Separate origins: every flat and preview has its own host.
	if code := call(t, "POST", base+"/api/flats/second/versions?deploy=1", bytes.NewReader(small), &pending); code != 202 {
		t.Fatalf("second flat pending: %d", code)
	}
	approve(pending)
	var list struct {
		Flats []struct {
			Slug       string `json:"slug"`
			PrivateURL string `json:"private_url"`
		} `json:"flats"`
	}
	call(t, "GET", base+"/api/flats", nil, &list)
	origins := map[string]string{}
	for _, f := range list.Flats {
		u, _ := url.Parse(f.PrivateURL)
		origin := u.Scheme + "://" + u.Host
		if prev, dup := origins[origin]; dup {
			t.Fatalf("flats %s and %s share origin %s", prev, f.Slug, origin)
		}
		origins[origin] = f.Slug
	}
	if len(origins) != 2 {
		t.Fatalf("want 2 origins, got %v", origins)
	}
	// The console host name is reserved.
	if code := call(t, "POST", base+"/api/flats", strings.NewReader(`{"slug":"flats"}`), nil); code != 400 {
		t.Fatalf("reserved slug accepted: %d", code)
	}
	fmt.Fprintln(io.Discard)
}
