package docs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Generate actual single-character client edits against the server's random seed.
func clientEdits(t *testing.T, welcome map[string]any, count int) []updateFixture {
	t.Helper()
	srv := startYjsTestModule(t, "client-edits.js")
	input, err := json.Marshal(map[string]any{"u": welcome["u"], "count": count})
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.Post(srv.URL, "application/json", bytes.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	output, err := io.ReadAll(r.Body)
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("client edit generation: status %d, error %v: %s", r.StatusCode, err, output)
	}
	var result []updateFixture
	if err = json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	if len(result) != count {
		t.Fatalf("generated %d updates, want %d", len(result), count)
	}
	return result
}

func TestDocumentOwnershipSoak(t *testing.T) {
	full := os.Getenv("FLATS_FULL_SOAK") == "1"
	for _, tc := range []struct {
		name, text string
		count      int
	}{
		{"300KiB", strings.Repeat("a", 300*1024), 3000},
		{"1MiBASCII", strings.Repeat("a", 1024*1024-4096), 200},
		{"1MiBKorean", strings.Repeat("한", (1024*1024-4096)/3), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := tc.count
			if !full {
				count = 60
			}
			a := startApp(t, t.TempDir(), tc.text, "soak-v1")
			x := connect(t, a.srv.URL, "private")
			welcome := x.hello(t, nil)
			updates := clientEdits(t, welcome, count)
			start := time.Now()
			for _, u := range updates {
				edit(t, x, u.ID, u.U)
				// Each ACK also sends a received frame; stay below 60 frames/s.
				time.Sleep(40 * time.Millisecond)
			}
			if a.document(t)["markdown"] != strings.Repeat("x", count)+tc.text {
				t.Fatal("acknowledged text lost")
			}
			x.Close()
			fresh := connect(t, a.srv.URL, "private")
			fresh.hello(t, nil)
			t.Logf("%d single-character edits on %d UTF8 bytes, zero failures, fresh join succeeds: %v", count, len(tc.text), time.Since(start))
		})
	}
}

func TestPublicDocumentReadSoak(t *testing.T) {
	count := 60
	if os.Getenv("FLATS_FULL_SOAK") == "1" {
		count = 200
	}
	text := strings.Repeat("a", 1024*1024)
	a := startApp(t, t.TempDir(), text, "reads-v1")
	a.document(t)
	start := time.Now()
	for i := 0; i < count; i++ {
		status, body, _ := a.get(t, "/_docs/api/document")
		var d map[string]any
		if status != 200 || json.Unmarshal([]byte(body), &d) != nil || d["markdown"] != text {
			t.Fatalf("GET %d: %d", i, status)
		}
	}
	t.Logf("%d public GETs of %d bytes, zero failures: %v", count, len(text), time.Since(start))
}

func TestLargeDisjointActivation(t *testing.T) {
	for _, size := range []int{90, 150, 500, 1024} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			base := "human old\n" + strings.Repeat("a", size*1024-100) + "\nagent old\n"
			data := t.TempDir()
			old := startApp(t, data, base, "merge-v1")
			x := connect(t, old.srv.URL, "private")
			w := x.hello(t, nil)
			edit(t, x, "human", clientEdits(t, w, 1)[0].U)
			x.Close()
			old.stop()
			next := startApp(t, data, strings.Replace(base, "agent old", "agent new", 1), "merge-v2")
			start := time.Now()
			got := next.document(t)
			if got["markdown"] != "x"+strings.Replace(base, "agent old", "agent new", 1) {
				t.Fatal("disjoint human edit replaced")
			}
			if time.Since(start) > 3*time.Second {
				t.Fatal("activation exceeds 3 seconds")
			}
			if c, ok := got["conflicts"].([]any); ok && len(c) != 0 {
				t.Fatal("spurious conflict")
			}
			t.Logf("%d KiB disjoint activation: %v", size, time.Since(start))
		})
	}
}
