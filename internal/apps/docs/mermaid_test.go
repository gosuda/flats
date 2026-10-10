package docs

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The client loads Mermaid from a content-hashed path that the worker serves
// by importing the separate dist module on demand.
func TestMermaidAssetServedLazily(t *testing.T) {
	a := startApp(t, t.TempDir(), initialText, "v1")
	_, client, _ := a.get(t, "/_docs/assets/client.js")
	url := regexp.MustCompile(`/_docs/assets/mermaid-([0-9a-f]{16})\.js`).FindStringSubmatch(client)
	if url == nil {
		t.Fatal("client does not reference the Mermaid asset")
	}
	start := time.Now()
	status, body, h := a.get(t, url[0])
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
