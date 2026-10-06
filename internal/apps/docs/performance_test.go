package docs

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

func Test100KiBDocumentTimings(t *testing.T) {
	text := "# Timing\n" + strings.Repeat("a", 100*1024-len("# Timing\n"))
	start := time.Now()
	a := startApp(t, t.TempDir(), text, "perf-v1")
	cold := time.Since(start)
	start = time.Now()
	a.document(t)
	seed := time.Since(start)
	clients := make([]*socket, 3)
	start = time.Now()
	for i := range clients {
		clients[i] = connect(t, a.srv.URL, "private")
		clients[i].hello(t, nil)
	}
	open := time.Since(start)
	updates := fixture(t).Compact
	latency := make([]time.Duration, 15)
	for i := range latency {
		start = time.Now()
		edit(t, clients[0], updates[i].ID, updates[i].U)
		for _, peer := range clients[1:] {
			peer.next(t, "update")
		}
		latency[i] = time.Since(start)
		time.Sleep(60 * time.Millisecond)
	}
	sort.Slice(latency, func(i, j int) bool { return latency[i] < latency[j] })
	if latency[len(latency)-1] >= 90*time.Millisecond {
		t.Fatalf("100 KiB edit p95 %v exceeds 90 ms", latency[len(latency)-1])
	}
	t.Logf("100 KiB ASCII Markdown / 3 clients / 15 incremental edits, loopback: worker start %v; HTTP seed %v; 3 joins %v; edit COMMIT+ACK+2 peers p50 %v / p95 %v", cold.Round(time.Millisecond), seed.Round(time.Microsecond), open.Round(time.Millisecond), latency[len(latency)/2].Round(time.Microsecond), latency[len(latency)-1].Round(time.Microsecond))
}

func TestMaximumSeed(t *testing.T) {
	text := strings.Repeat("a", 1024*1024)
	a := startApp(t, t.TempDir(), text, "max-v1")
	start := time.Now()
	if got := a.document(t)["markdown"]; got != text {
		t.Fatal("maximum seed did not roundtrip")
	}
	t.Logf("1 MiB source seed + API read: %v", time.Since(start))
}

func TestLargeDocumentEditAndPingBudgets(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"small", strings.Repeat("a", 1024)},
		{"ASCII", strings.Repeat("a", 1024*1024-1024)},
		{"Korean", strings.Repeat("한", (1024*1024-1024)/3)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := startApp(t, t.TempDir(), tc.text, "large-v1")
			a.document(t)
			x := connect(t, a.srv.URL, "private")
			x.hello(t, nil)
			edits, pings := []time.Duration{}, []time.Duration{}
			for _, u := range fixture(t).Compact[:35] {
				start := time.Now()
				edit(t, x, u.ID, u.U)
				edits = append(edits, time.Since(start))
				start = time.Now()
				x.send(t, map[string]any{"t": "ping"})
				x.next(t, "pong")
				pings = append(pings, time.Since(start))
				time.Sleep(30 * time.Millisecond)
			}
			sort.Slice(edits, func(i, j int) bool { return edits[i] < edits[j] })
			sort.Slice(pings, func(i, j int) bool { return pings[i] < pings[j] })
			ep, pp := edits[33], pings[33]
			t.Logf("%s %d UTF8 bytes / 35 accepted edits: ACK p50 %v p95 %v; ping p50 %v p95 %v", tc.name, len(tc.text), edits[17], ep, pings[17], pp)
			if ep > 300*time.Millisecond || edits[34] > time.Second {
				t.Fatalf("edit latency budget exceeded: p95 %v max %v", ep, edits[34])
			}
			if pp > 25*time.Millisecond || pings[34] > 100*time.Millisecond {
				t.Fatalf("size independent ping budget exceeded: p95 %v max %v", pp, pings[34])
			}
			got := a.document(t)["markdown"].(string)
			if !strings.Contains(got, tc.text) || !strings.Contains(got, "34,") {
				t.Fatal("acknowledged edits lost")
			}
		})
	}
}
func TestMaximumRewriteActivationBudget(t *testing.T) {
	for _, tc := range []struct{ name, base, target string }{
		{"ASCII", strings.Repeat("a", 1024*1024-1024), strings.Repeat("b", 1024*1024)},
		{"Korean", strings.Repeat("한", (1024*1024-1024)/3), strings.Repeat("글", 1024*1024/3) + "a"},
		{"lines", strings.Repeat("old line abcdefghijklmnopqrstuvwxyz\n", 30000)[:1024*1024-1024], strings.Repeat("new line zyxwvutsrqponmlkjihgfedcba\n", 30000)[:1024*1024]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := t.TempDir()
			old := startApp(t, data, tc.base, "max-v1")
			old.document(t)
			x := connect(t, old.srv.URL, "private")
			x.hello(t, nil)
			edit(t, x, "human", fixture(t).Independent)
			x.Close()
			old.stop()
			next := startApp(t, data, tc.target, "max-v2")
			start := time.Now()
			status, body, _ := next.getHeaders(t, "/_docs/healthz", http.Header{"X-Flats-Health": {"1"}})
			elapsed := time.Since(start)
			t.Logf("%s maximum rewrite %d bytes: host activation %v", tc.name, len(tc.target), elapsed)
			if status != 200 {
				t.Fatal(status, body)
			}
			if elapsed > 3*time.Second {
				t.Fatalf("maximum activation exceeds 3 s budget: %v", elapsed)
			}
			if next.document(t)["markdown"] != tc.target {
				t.Fatal(fmt.Sprint("target not activated"))
			}
		})
	}
}
