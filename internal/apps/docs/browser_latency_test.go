package docs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
// pages run in separate headless Chromium contexts; page A inserts a marker
// every 250 ms and page B records when each marker first appears in its
// CodeMirror DOM (testdata/display_latency.mjs). Scenarios:
//
//   - idle: the two members alone;
//   - guests: 20 paced guests editing the same document at full pace;
//   - flood: the guests with the flood clients of TestGuestLoad, which fill
//     the guest and viewer pools and flood Public ingress;
//   - visible-words, visible-brackets, visible-destinations: the guests'
//     growing text stays in page B's viewport while B times the markers,
//     typed as plain paragraphs, one long paragraph of bracketed words, or
//     unfinished link destinations without spaces.
//
// Every scenario ends by stopping the guests, letting their updates settle,
// and comparing both pages' editor text with the server's committed
// Markdown, which must hold every acknowledged guest update, so a dropped
// update fails. guests and flood assert display p95
// under 200 ms; the visible scenarios assert every round trip into page B's
// main thread (timed every 2 s) under 200 ms. All assert every marker seen.
//
// Opt in with FLATS_DOCS_BROWSER_TEST=1 and FLATS_PLAYWRIGHT_MODULE;
// FLATS_DOCS_BROWSER_EVIDENCE keeps the screenshots,
// FLATS_DOCS_DISPLAY_SECONDS changes the 60 s of edits,
// FLATS_DOCS_DISPLAY_GUEST_TEXT changes the guests' text in guests and flood
// (words by default; brackets, destinations), and
// FLATS_DOCS_DISPLAY_PROFILE=1 records a CPU profile of page B.
func TestBrowserDisplayLatency(t *testing.T) {
	skipWithoutBrowser(t)
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
	text := "words"
	if v := os.Getenv("FLATS_DOCS_DISPLAY_GUEST_TEXT"); v != "" {
		text = v
	}
	for _, sc := range []struct {
		name          string
		guests, flood bool
		mode, text    string
	}{
		{"idle", false, false, "top", ""},
		{"guests", true, false, "top", text},
		{"flood", true, true, "top", text},
		{"visible-words", true, false, "visible", "words"},
		{"visible-brackets", true, false, "visible", "brackets"},
		{"visible-destinations", true, false, "visible", "destinations"},
	} {
		t.Run(sc.name, func(t *testing.T) {
			t.Logf("DISPLAY uptime at start: %s", uptime())
			a := startAppModule(t, t.TempDir(), docsModule("v1", loadTexts()), "v1", map[string]string{})
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
			l.guestText = sc.text
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
			// settle stops the flood and the guests' typing and waits for
			// their updates to be acknowledged.
			settle := func() {
				l.halt()
				l.floods.Wait()
				for _, g := range guests {
					g.stopTyping.Store(true)
				}
				start := time.Now()
				open, all := l.unacked(guests)
				for open > 0 && time.Since(start) < time.Minute {
					time.Sleep(100 * time.Millisecond)
					open, all = l.unacked(guests)
				}
				t.Logf("DISPLAY %s: guests settled after %v, unacknowledged %d (%d on open sockets)", sc.name, time.Since(start).Round(time.Millisecond), all, open)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second+4*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "node", "testdata/display_latency.mjs", proxy.URL, evidence,
				fmt.Sprintf("%s-%d", sc.name, time.Now().Unix()), fmt.Sprint(seconds), "index.md", sc.mode)
			cmd.Env = os.Environ()
			cmd.WaitDelay = 10 * time.Second
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			out, pw := io.Pipe()
			cmd.Stdout, cmd.Stderr = pw, pw
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				err := cmd.Wait()
				pw.Close()
				done <- err
			}()
			var result struct {
				Sent, Count, Missing               int
				P50, P95, P99, Max                 *float64
				Status, Errors                     []string
				Responsiveness                     map[string]map[string]float64
				ProbeMax                           float64
				GuestInView                        int
				Converged                          bool
				Server, A, B                       int
				AFirstDifference, BFirstDifference int
			}
			found := false
			lines := bufio.NewScanner(out)
			lines.Buffer(make([]byte, 1<<20), 1<<20)
			for lines.Scan() {
				line := lines.Text()
				switch {
				case line == "EDITS-DONE":
					settle()
					io.WriteString(stdin, "settled\n")
					stdin.Close()
				case strings.HasPrefix(line, "RESULT "):
					found = json.Unmarshal([]byte(strings.TrimPrefix(line, "RESULT ")), &result) == nil
				default:
					t.Log(line)
				}
			}
			err = <-done
			l.halt()
			if err != nil || !found {
				t.Fatalf("display_latency.mjs: %v (result found %v)", err, found)
			}
			ms := func(p *float64) string {
				if p == nil {
					return "-"
				}
				return fmt.Sprintf("%.1fms", *p)
			}
			t.Logf("DISPLAY %s: page main threads (frame gaps in ms, long tasks in ms, document lines): %v; probe max %.0fms; guest words in B's viewport %d",
				sc.name, result.Responsiveness, result.ProbeMax, result.GuestInView)
			t.Logf("DISPLAY %s: markers sent %d, seen %d, missing %d, p50 %s, p95 %s, p99 %s, max %s, page status %v, page errors %v",
				sc.name, result.Sent, result.Count, result.Missing, ms(result.P50), ms(result.P95), ms(result.P99), ms(result.Max), result.Status, result.Errors)
			t.Logf("DISPLAY %s: converged %v (server %d, page A %d, page B %d characters)", sc.name, result.Converged, result.Server, result.A, result.B)
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
			// Every guest update acknowledged before this read is in the
			// server's text, which both pages showed exactly.
			readAt := time.Now()
			final := a.markdown(t, "index.md")
			checked, lost := 0, []string{}
			for _, g := range guests {
				g.mu.Lock()
				for _, x := range g.written {
					if x.acked.IsZero() || !x.acked.Before(readAt) {
						continue
					}
					checked++
					if !strings.Contains(final, x.text) {
						lost = append(lost, x.id)
					}
				}
				g.mu.Unlock()
			}
			t.Logf("DISPLAY %s: %d acknowledged guest updates checked in the server's text, missing %d", sc.name, checked, len(lost))
			if len(lost) > 0 {
				t.Errorf("acknowledged guest updates missing from the server's text: %v", lost[:min(len(lost), 10)])
			}
			t.Logf("DISPLAY uptime at end: %s", uptime())
			if result.Missing > 0 || result.Count == 0 || len(result.Errors) > 0 {
				t.Errorf("%d of %d markers never appeared in page B; page errors %v", result.Missing, result.Sent, result.Errors)
			}
			if !result.Converged {
				t.Errorf("the pages' text differs from the server's (server %d, page A %d, page B %d characters)", result.Server, result.A, result.B)
			}
			if (sc.name == "guests" || sc.name == "flood") && (result.P95 == nil || *result.P95 >= 200) {
				t.Errorf("display p95 %s, want under 200ms", ms(result.P95))
			}
			if sc.mode == "visible" {
				if result.ProbeMax >= 200 {
					t.Errorf("a round trip into page B took %.0fms, want under 200ms", result.ProbeMax)
				}
				if result.GuestInView == 0 {
					t.Error("no guest text in page B's viewport")
				}
			}
		})
	}
}
