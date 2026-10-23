package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestBrowserDisplayLatency measures the spec's display target
// (docs/spec/docs-public-edit-TECH.md 8, row "Limits"): a member's change is
// visible in another member's editor within 200 ms p95. Two Private member
// pages run in separate headless Chromium contexts; page A inserts a marked
// line every 250 ms and page B records when each marker first appears in its
// CodeMirror DOM (testdata/display_latency.mjs). Scenarios:
//
//   - idle: the two members alone;
//   - guests: 20 paced guests editing the same document at full pace;
//   - flood: the guests plus the flood clients of TestGuestLoad.
//
// Only the guests scenario asserts p95 under 200 ms; the others are
// reported. Opt in with FLATS_DOCS_BROWSER_TEST=1 and
// FLATS_PLAYWRIGHT_MODULE; FLATS_DOCS_BROWSER_EVIDENCE keeps the
// screenshots, FLATS_DOCS_DISPLAY_SECONDS changes the 60 s of edits,
// FLATS_DOCS_DISPLAY_GUEST_TEXT=brackets makes the guests type one long
// paragraph of bracketed words instead of plain paragraphs (=destinations
// types unfinished link destinations without spaces instead), and
// FLATS_DOCS_DISPLAY_PROFILE=1 records a CPU profile of page B.
func TestBrowserDisplayLatency(t *testing.T) {
	if os.Getenv("FLATS_DOCS_BROWSER_TEST") != "1" || os.Getenv("FLATS_PLAYWRIGHT_MODULE") == "" {
		t.Skip("opt in with FLATS_DOCS_BROWSER_TEST=1 and FLATS_PLAYWRIGHT_MODULE")
	}
	seconds := 60
	if s := os.Getenv("FLATS_DOCS_DISPLAY_SECONDS"); s != "" {
		if _, err := fmt.Sscan(s, &seconds); err != nil || seconds < 5 {
			t.Fatalf("FLATS_DOCS_DISPLAY_SECONDS=%q", s)
		}
	}
	evidence := os.Getenv("FLATS_DOCS_BROWSER_EVIDENCE")
	if evidence == "" {
		evidence = t.TempDir()
	}
	if err := os.MkdirAll(evidence, 0700); err != nil {
		t.Fatal(err)
	}
	for _, sc := range []struct {
		name          string
		guests, flood bool
	}{{"idle", false, false}, {"guests", true, false}, {"flood", true, true}} {
		t.Run(sc.name, func(t *testing.T) {
			t.Logf("DISPLAY uptime at start: %s", uptime())
			texts := loadTexts()
			a := startAppModule(t, t.TempDir(), docsModule("v1", texts), "v1", map[string]string{})
			a.setAccess(t, "edit", 1)
			// The host's trusted access header, as in TestBrowserCollaboration.
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.Header.Set("X-Flats-Access", "public")
				if c, err := r.Cookie("flats-test-private"); err == nil && c.Value == "1" {
					r.Header.Set("X-Flats-Access", "private")
				}
				a.inst.ServeHTTP(w, r)
			}))
			defer proxy.Close()
			l := newLoadRun(a.srv.URL)
			l.guestText = "words"
			if v := os.Getenv("FLATS_DOCS_DISPLAY_GUEST_TEXT"); v != "" {
				l.guestText = v
			}
			t.Cleanup(func() {
				l.halt()
				l.floods.Wait()
			})
			var guests []*pacedGuest
			if sc.guests {
				for i := 0; i < 20; i++ {
					guests = append(guests, l.pacedGuest(t, i))
				}
				time.Sleep(3 * time.Second)
			}
			if sc.flood {
				l.startFlood(t)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second+3*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "node", "testdata/display_latency.mjs", proxy.URL, evidence, fmt.Sprintf("%s-%d", sc.name, time.Now().Unix()), fmt.Sprint(seconds), "index.md")
			cmd.Env = os.Environ()
			cmd.WaitDelay = 10 * time.Second
			output, err := cmd.CombinedOutput()
			l.halt()
			var result struct {
				Sent, Count, Missing int
				P50, P95, P99, Max   *float64
				Status, Errors       []string
				Responsiveness       map[string]map[string]float64
			}
			found := false
			for _, line := range strings.Split(string(output), "\n") {
				if raw, ok := strings.CutPrefix(line, "RESULT "); ok {
					found = json.Unmarshal([]byte(raw), &result) == nil
				} else if line != "" {
					t.Log(line)
				}
			}
			if err != nil || !found {
				t.Fatalf("display_latency.mjs: %v (result found %v)", err, found)
			}
			ms := func(p *float64) string {
				if p == nil {
					return "-"
				}
				return fmt.Sprintf("%.1fms", *p)
			}
			t.Logf("DISPLAY %s: page main threads (frame gaps in ms, long tasks in ms, document lines): %v", sc.name, result.Responsiveness)
			t.Logf("DISPLAY %s: markers sent %d, seen %d, missing %d, p50 %s, p95 %s, p99 %s, max %s, page status %v, page errors %v",
				sc.name, result.Sent, result.Count, result.Missing, ms(result.P50), ms(result.P95), ms(result.P99), ms(result.Max), result.Status, result.Errors)
			if len(guests) > 0 {
				acked, busy, closed := 0, 0, 0
				for _, g := range guests {
					g.mu.Lock()
					acked += g.acked
					busy += g.busyFrames
					g.mu.Unlock()
					if g.conn.endReason() != "" {
						closed++
					}
				}
				t.Logf("DISPLAY %s: paced guests acked %d updates, busy frames %d, closed %d", sc.name, acked, busy, closed)
			}
			t.Logf("DISPLAY uptime at end: %s", uptime())
			if result.Missing > 0 || len(result.Errors) > 0 {
				t.Errorf("%d markers never appeared in page B; page errors %v", result.Missing, result.Errors)
			}
			if sc.name == "guests" && (result.P95 == nil || *result.P95 >= 200) {
				t.Errorf("display p95 %s under guest load, want under 200ms", ms(result.P95))
			}
		})
	}
}
