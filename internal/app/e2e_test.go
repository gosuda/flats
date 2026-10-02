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
	h, err := Start(context.Background(), Options{DataDir: t.TempDir(), Listen: "127.0.0.1:0", Network: "local",
		LocalAddr: "127.0.0.1:0", ConsoleHost: "flats", Portal: false, Runtime: false})
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

// TestMVPGate measures the Phase 1 gate on the local network: a 10 MB static
// upload goes live in under 30 s, rollback takes under 10 s, and every flat
// has its own origin.
func TestMVPGate(t *testing.T) {
	h := startLocal(t)
	base := "http://" + h.Addr()
	blob := make([]byte, 10<<20-4096)
	rand.Read(blob) // incompressible: the upload really is ~10 MB
	archive := tarGz(t, map[string][]byte{"index.html": []byte("<h1>v1</h1>"), "media/blob.bin": blob})
	if len(archive) < 10<<20-8192 {
		t.Fatalf("archive is only %d bytes", len(archive))
	}
	start := time.Now()
	var out struct {
		Version struct{ Number int } `json:"version"`
		Deploy  struct {
			Flat struct {
				PrivateURL string `json:"private_url"`
			} `json:"flat"`
			Health struct{ OK bool } `json:"health"`
		} `json:"deploy"`
	}
	code := call(t, "POST", base+"/api/flats/big-site/versions?deploy=1&git_sha=deadbeef", bytes.NewReader(archive), &out)
	if code != 201 || !out.Deploy.Health.OK {
		t.Fatalf("upload+deploy: %d %+v", code, out)
	}
	if c, body := fetch(t, out.Deploy.Flat.PrivateURL); c != 200 || !strings.Contains(body, "v1") {
		t.Fatalf("live: %d %q", c, body)
	}
	uploadToLive := time.Since(start)
	t.Logf("GATE upload(10MB)->live: %s (limit 30s)", uploadToLive)
	if uploadToLive > 30*time.Second {
		t.Fatalf("upload to live took %s", uploadToLive)
	}

	small := tarGz(t, map[string][]byte{"index.html": []byte("<h1>v2</h1>")})
	if code := call(t, "POST", base+"/api/flats/big-site/versions?deploy=1", bytes.NewReader(small), &out); code != 201 {
		t.Fatalf("v2: %d", code)
	}
	if _, body := fetch(t, out.Deploy.Flat.PrivateURL); !strings.Contains(body, "v2") {
		t.Fatalf("v2 not live: %q", body)
	}
	start = time.Now()
	var rb struct{ Version int }
	if code := call(t, "POST", base+"/api/flats/big-site/rollback", strings.NewReader(`{"version":0}`), &rb); code != 200 || rb.Version != 1 {
		t.Fatalf("rollback: %d %+v", code, rb)
	}
	if _, body := fetch(t, out.Deploy.Flat.PrivateURL); !strings.Contains(body, "v1") {
		t.Fatalf("rollback not live: %q", body)
	}
	rollback := time.Since(start)
	t.Logf("GATE rollback: %s (limit 10s)", rollback)
	if rollback > 10*time.Second {
		t.Fatalf("rollback took %s", rollback)
	}

	// Separate origins: every flat and preview has its own host.
	call(t, "POST", base+"/api/flats/second/versions?deploy=1", bytes.NewReader(small), &out)
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
