package docs

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestVisitorHealthIsCheapAndCannotBlockCommit(t *testing.T) {
	a := startApp(t, t.TempDir(), strings.Repeat("a", 100*1024), "v1")
	x := connect(t, a.srv.URL, "private")
	x.hello(t, nil)
	db, err := sql.Open("sqlite", filepath.Join(a.data, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	for _, access := range []string{"public", "private", ""} {
		start := time.Now()
		status, body, _ := a.getHeaders(t, "/_docs/healthz", http.Header{"X-Flats-Access": {access}})
		if status != 200 || body != "ok" || time.Since(start) > 500*time.Millisecond {
			t.Fatal("visitor health reserved writer", access, status, body)
		}
	}
	if _, err = db.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			access := []string{"public", "private"}[i%2]
			for {
				select {
				case <-done:
					return
				default:
				}
				req, _ := http.NewRequest("GET", a.srv.URL+"/_docs/healthz", nil)
				req.Header.Set("X-Flats-Access", access)
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					failures <- err
					return
				}
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
				if res.StatusCode != 200 {
					failures <- fmt.Errorf("health %d", res.StatusCode)
					return
				}
			}
		}(i)
	}
	defer func() {
		close(done)
		wg.Wait()
		close(failures)
		for err := range failures {
			t.Error(err)
		}
	}()
	for _, u := range fixture(t).Compact[:15] {
		start := time.Now()
		edit(t, x, u.ID, u.U)
		if time.Since(start) > time.Second {
			t.Fatal("health loops exceeded 1 s commit budget")
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func TestConflictPrivacyAndRollbackStorageBudgets(t *testing.T) {
	for _, tc := range []struct {
		name       string
		characters int
	}{{"count", 90000}, {"bytes", 100000}} {
		t.Run(tc.name, func(t *testing.T) { testConflictRollback(t, tc.characters) })
	}
}
func testConflictRollback(t *testing.T, characters int) {
	data := t.TempDir()
	texts := []string{strings.Repeat("한", characters), strings.Repeat("글", characters)}
	if characters == 90000 {
		texts = []string{strings.Repeat("a", characters), strings.Repeat("b", characters)}
	}
	// Exceed the retained line bound to force a conflict independently of text size.
	for i := range texts {
		texts[i] += strings.Repeat("\n", 16001)
	}
	a := startApp(t, data, texts[0], "cycle-v1")
	a.document(t)
	db, err := sql.Open("sqlite", filepath.Join(data, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var generations []float64
	for i := 0; i < 12; i++ {
		x := connect(t, a.srv.URL, "private")
		x.hello(t, nil)
		var viewer *socket
		if i == 0 {
			viewer = connect(t, a.srv.URL, "public")
			w := viewer.hello(t, nil)
			if _, ok := w["conflicts"]; ok {
				t.Fatal("public welcome conflict metadata")
			}
		}
		u := fixture(t).Compact[i]
		edit(t, x, u.ID, u.U)
		x.inbox = nil
		next := startApp(t, data, texts[(i+1)%2], fmt.Sprintf("cycle-v%d", (i+1)%2+1))
		got := next.document(t)
		conflicts := got["conflicts"].([]any)
		if len(conflicts) == 0 || len(conflicts) > 8 {
			t.Fatal("metadata cap", len(conflicts))
		}
		for _, c := range conflicts {
			if _, ok := c.(map[string]any)["markdown"]; ok {
				t.Fatal("routine response contains full conflict text")
			}
		}
		generation := conflicts[0].(map[string]any)["generation"].(float64)
		generations = append(generations, generation)
		for j, c := range conflicts {
			if c.(map[string]any)["generation"] != generations[len(generations)-1-j] {
				t.Fatal("retained older conflict instead of newest")
			}
		}
		status, body, _ := next.get(t, "/_docs/api/document")
		var public map[string]any
		if status != 200 || json.Unmarshal([]byte(body), &public) != nil {
			t.Fatal(status, body)
		}
		if _, ok := public["conflicts"]; ok {
			t.Fatal("conflicts exposed publicly")
		}
		path := fmt.Sprintf("/_docs/api/conflict?generation=%.0f", generation)
		if status, _, _ := next.get(t, path); status != 403 {
			t.Fatal("public conflict text accessible", status)
		}
		status, body, _ = next.getHeaders(t, path, http.Header{"X-Flats-Access": {"private"}})
		var preserved map[string]any
		if status != 200 || json.Unmarshal([]byte(body), &preserved) != nil || !strings.Contains(fmt.Sprint(preserved["markdown"]), texts[i%2]) {
			t.Fatal("private text not preserved", status)
		}
		// The open editor learns about activation on catch-up without being stopped.
		x.send(t, map[string]any{"t": "ping"})
		x.next(t, "pong")
		m := x.next(t, "conflicts")
		if len(m["conflicts"].([]any)) == 0 {
			t.Fatal("private editor notice missing")
		}
		if viewer != nil {
			viewer.send(t, map[string]any{"t": "ping"})
			viewer.next(t, "pong")
			for _, m := range viewer.inbox {
				if m["t"] == "conflicts" {
					t.Fatal("public viewer received conflict notice")
				}
			}
			viewer.Close()
		}
		var count, bytes, hashes int
		if err = db.QueryRow("SELECT count(*),coalesce(sum(length(CAST(markdown AS BLOB))),0) FROM flats_docs_conflicts WHERE doc='index.md'").Scan(&count, &bytes); err != nil {
			t.Fatal(err)
		}
		if count > 8 || bytes > 2*1024*1024 {
			t.Fatalf("rollback storage exceeded budget: %d records %d bytes", count, bytes)
		}
		db.QueryRow("SELECT count(*) FROM flats_docs_activations WHERE doc='index.md'").Scan(&hashes)
		if hashes != 2 {
			t.Fatal("test did not revisit two hashes", hashes)
		}
		if i == 11 && ((characters == 90000 && count != 8) || (characters == 100000 && count != 6)) {
			t.Fatal("retention did not keep newest fitting records", count)
		}
		t.Logf("rollback cycle %d: %d conflicts / %d UTF8 bytes", i+1, count, bytes)
		if i == 11 {
			status, _, _ := next.getHeaders(t, fmt.Sprintf("/_docs/api/conflict?generation=%.0f", generations[0]), http.Header{"X-Flats-Access": {"private"}})
			if status != 404 {
				t.Fatal("evicted conflict remains accessible", status)
			}
		}
		x.Close()
		a.stop()
		a = next
	}
}

func TestRejectedTextMutationReloadsWithoutDisconnectingPeers(t *testing.T) {
	text := strings.Repeat("a", 1024*1024-10)
	a := startApp(t, t.TempDir(), text, "v1")
	bad, good := connect(t, a.srv.URL, "private"), connect(t, a.srv.URL, "private")
	bad.hello(t, nil)
	good.hello(t, nil)
	bad.send(t, map[string]any{"t": "update", "id": "too-much-text", "u": fixture(t).Independent})
	bad.rejected(t, "capacity")
	good.send(t, map[string]any{"t": "ping"})
	good.next(t, "pong")
	if a.document(t)["markdown"] != text {
		t.Fatal("rejection persisted text")
	}
	u := fixture(t).Compact[0]
	edit(t, good, u.ID, u.U)
	got := a.document(t)["markdown"].(string)
	if len(got) != len(text)+2 || strings.Contains(got, "Person") {
		t.Fatal("rejected text remained in shared cache")
	}
}
