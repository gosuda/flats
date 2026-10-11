package docs

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The client loads Mermaid from a content-hashed path that the worker serves
// by importing the separate dist module on demand.
func TestMermaidAssetServedLazily(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	_, client, _ := a.get(t, "/_docs/assets/client.js")
	url := regexp.MustCompile(`/_docs/assets/mermaid-([0-9a-f]{16})\.js`).FindStringSubmatch(client)
	if url == nil {
		t.Fatal("client does not reference the Mermaid asset")
	}
	start := time.Now()
	status, body, h := a.getHeaders(t, url[0], http.Header{"Accept-Encoding": {"identity"}})
	elapsed := time.Since(start)
	if status != 200 {
		t.Fatalf("%d %.200s", status, body)
	}
	sum := sha256.Sum256([]byte(body))
	if hex.EncodeToString(sum[:])[:16] != url[1] {
		t.Fatal("served Mermaid does not match its content hash")
	}
	if !strings.Contains(body, "mermaid") || h.Get("Content-Type") != "text/javascript; charset=utf-8" ||
		h.Get("Cache-Control") != "public, max-age=31536000, immutable" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal(h)
	}
	t.Logf("first Mermaid response: %d bytes in %s", len(body), elapsed)
	// The worker keeps serving documents after loading it.
	if m := a.document(t); m["markdown"] != initialText {
		t.Fatal(m)
	}
	if status, _, _ := a.get(t, "/_docs/assets/mermaid-0000000000000000.js"); status != 404 {
		t.Fatal("stale Mermaid path", status)
	}
}

// Execute the actual QuickJS worker: binary response serialization must preserve
// every gzip byte, including values that are not valid UTF-8.
func TestMermaidAssetEncoding(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	_, clientJS, _ := a.get(t, "/_docs/assets/client.js")
	asset := regexp.MustCompile(`/_docs/assets/mermaid-([0-9a-f]{16})\.js`).FindStringSubmatch(clientJS)
	if asset == nil {
		t.Fatal("missing Mermaid URL")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	for _, tc := range []struct {
		name, accept, encoding string
		status                 int
	}{
		{"absent", "", "", 200},
		{"identity", "identity", "", 200},
		{"gzip", "gzip", "gzip", 200},
		{"browser", "gzip, deflate, br", "gzip", 200},
		{"gzip refused", "gzip;q=0", "", 200},
		{"identity preferred", "gzip;q=0.4, identity;q=0.8", "", 200},
		{"gzip preferred", "gzip;q=0.8, identity;q=0.4", "gzip", 200},
		{"case and whitespace", "GZip; q=1.000, identity; q=0.100", "gzip", 200},
		{"wildcard", "*", "gzip", 200},
		{"explicit overrides wildcard", "gzip;q=0, *;q=1", "", 200},
		{"identity forbidden", "gzip, identity;q=0", "gzip", 200},
		{"unsupported coding", "br", "", 200},
		{"no acceptable coding", "gzip;q=0, identity;q=0", "", 406},
		{"wildcard forbidden", "*;q=0", "", 406},
		{"gzip overrides wildcard", "*;q=0, gzip;q=0.7", "gzip", 200},
		{"identity overrides wildcard", "*;q=0, identity;q=1", "", 200},
		{"invalid quality", "gzip;q=oops", "", 200},
		{"unknown parameter", "gzip; extension=value", "gzip", 200},
		{"extension preserves quality", "gzip;q=0.9; extension=value, identity;q=0.1", "gzip", 200},
		{"extension preserves refusal", "gzip;q=0; extension=value", "", 200},
		{"invalid quality with extension", "gzip;q=oops; extension=value", "", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, a.srv.URL+asset[0], nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.accept != "" {
				req.Header.Set("Accept-Encoding", tc.accept)
			}
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != tc.status || res.Header.Get("Content-Encoding") != tc.encoding || res.Header.Get("Vary") != "Accept-Encoding" {
				t.Fatalf("status %d, headers %v", res.StatusCode, res.Header)
			}
			if tc.status != 200 {
				return
			}
			if res.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
				t.Fatal(res.Header)
			}
			var reader io.Reader = res.Body
			if tc.encoding == "gzip" {
				gz, err := gzip.NewReader(reader)
				if err != nil {
					t.Fatal(err)
				}
				defer gz.Close()
				reader = gz
			}
			body, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(body)
			if hex.EncodeToString(sum[:])[:16] != asset[1] {
				t.Fatal("decoded asset does not match content hash")
			}
		})
	}
	t.Run("head", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodHead, a.srv.URL+asset[0], nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Accept-Encoding", "gzip")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != 200 || len(body) != 0 || res.Header.Get("Content-Encoding") != "gzip" || res.Header.Get("Vary") != "Accept-Encoding" {
			t.Fatalf("status %d, body %d, headers %v", res.StatusCode, len(body), res.Header)
		}
	})
}
