package docs

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Generate actual single-character client edits against the server's random seed.
func clientEdits(t *testing.T, welcome map[string]any, count int) []updateFixture {
	t.Helper()
	code := `import * as Y from 'yjs';const d=new Y.Doc();d.clientID=900;let input='';for await(const chunk of process.stdin)input+=chunk;Y.applyUpdate(d,Buffer.from(input,'base64')); const result=[];for(let i=0;i<Number(process.argv[1]);i++){const sv=Y.encodeStateVector(d);d.getText('markdown').insert(0,'x');result.push({id:'soak-'+i,u:Buffer.from(Y.encodeStateAsUpdate(d,sv)).toString('base64')});}console.log(JSON.stringify(result));`
	cmd := exec.Command("node", "--input-type=module", "-e", code, fmt.Sprint(count))
	cmd.Dir = "_web"
	cmd.Stdin = strings.NewReader(welcome["u"].(string))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err, string(output))
	}
	var result []updateFixture
	if err = json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
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
