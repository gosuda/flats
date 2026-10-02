package portal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestE2E publishes a temporary hello page through a real relay.
// Run with FLATS_PORTAL_E2E=1; it needs internet access.
func TestE2E(t *testing.T) {
	if os.Getenv("FLATS_PORTAL_E2E") != "1" {
		t.Skip("set FLATS_PORTAL_E2E=1 to publish a temporary page through https://s-h.day")
	}
	var rnd [4]byte
	rand.Read(rnd[:])
	slug := "flats-e2e-" + hex.EncodeToString(rnd[:])
	marker := "hello from " + slug

	// SDK goroutines may log after the test returns; t.Logf would panic then.
	var done atomic.Bool
	t.Cleanup(func() { done.Store(true) })
	logf := func(f string, a ...any) {
		if !done.Load() {
			t.Logf(f, a...)
		}
	}
	n, err := New(Config{Dir: t.TempDir(), Relays: []string{"https://s-h.day"}, Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()

	start := time.Now()
	// Start unlisted so the test page never appears in relay listings.
	if _, err := n.Serve(t.Context(), slug, hello(marker), true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	urls, err := n.WaitReady(ctx, slug)
	if err != nil {
		t.Fatalf("not ready after %v: %v; status %+v", time.Since(start), err, n.Status())
	}
	t.Logf("time to ready: %v, urls %v", time.Since(start), urls)
	pub := n.URL(slug)
	if !strings.HasPrefix(pub, "https://"+slug+".") {
		t.Fatalf("URL = %q", pub)
	}
	if hi := n.Status().Hosts[0]; hi.State != "ready" || hi.URL != pub {
		t.Fatalf("status = %+v", hi)
	}

	c := &http.Client{Timeout: 15 * time.Second}
	body, code, err := fetch(c, pub)
	if err != nil || code != 200 || body != marker {
		t.Fatalf("GET %s = %d %q %v", pub, code, body, err)
	}
	t.Logf("GET %s ok after %v", pub, time.Since(start))

	// Listing changes only reach the relay at the next renewal (~90 s), so a
	// short toggle never actually lists the page.
	if err := n.SetHidden(slug, false); err != nil {
		t.Fatal(err)
	}
	if err := n.SetHidden(slug, true); err != nil {
		t.Fatal(err)
	}

	stopAt := time.Now()
	if err := n.Stop(slug); err != nil {
		t.Fatal(err)
	}
	t.Logf("Stop took %v", time.Since(stopAt))
	if n.URL(slug) != "" {
		t.Fatal("URL after Stop")
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		body, code, err := fetch(c, pub)
		if err != nil || code != 200 || body != marker {
			t.Logf("after Stop: GET = %d %v", code, err)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("public URL still serves after Stop")
		}
		time.Sleep(time.Second)
	}
}

func fetch(c *http.Client, u string) (string, int, error) {
	resp, err := c.Get(u)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return string(b), resp.StatusCode, err
}
