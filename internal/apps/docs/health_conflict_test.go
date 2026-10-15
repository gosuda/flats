package docs

import (
	"database/sql"
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
	t.Parallel()
	for _, tc := range []struct {
		name       string
		characters int
	}{{"count", 90000}, {"bytes", 100000}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testConflictRollback(t, tc.characters)
		})
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
	for i := 0; i < 12; i++ {
		x := connect(t, a.srv.URL, "private")
		x.hello(t, nil)
		var viewer *socket
		if i == 0 {
			viewer = connect(t, a.srv.URL, "public")
			viewer.hello(t, nil)
		}
		u := fixture(t).Compact[i]
		edit(t, x, u.ID, u.U)
		x.inbox = nil
		next := startApp(t, data, texts[(i+1)%2], fmt.Sprintf("cycle-v%d", (i+1)%2+1))
		got := next.document(t)
		if _, ok := got["conflicts"]; ok {
			t.Fatal("documents no longer report conflict records")
		}
		// The live text the switch replaced is the newest person entry, in
		// the shape of the version before it.
		entries := next.outbox(t)
		var person revEntry
		for _, e := range entries {
			if e.Kind == "person" {
				person = e
			}
		}
		if person.Kind == "" || person.Source != fmt.Sprintf("cycle-v%d", i%2+1) || !strings.Contains(next.revisionText(t, person.hash("index.md")), texts[i%2]) {
			t.Fatal("private text not kept in a revision", person)
		}
		if last := entries[len(entries)-1]; last.Kind != "agent" || last.Source != fmt.Sprintf("cycle-v%d", (i+1)%2+1) || last.hash("index.md") != digestOf(got["markdown"].(string)) {
			t.Fatalf("activation entry %+v", last)
		}
		if status, _, _ := next.getHeaders(t, "/_docs/host/revisions/outbox", http.Header{"X-Flats-Access": {"public"}}); status != 403 {
			t.Fatal("public outbox access", status)
		}
		// The open editor keeps working across the switch.
		x.send(t, map[string]any{"t": "ping"})
		x.next(t, "pong")
		if viewer != nil {
			viewer.send(t, map[string]any{"t": "ping"})
			viewer.next(t, "pong")
			viewer.Close()
		}
		var conflicts, hashes int
		db.QueryRow("SELECT count(*) FROM flats_docs_conflicts").Scan(&conflicts)
		if conflicts != 0 {
			t.Fatal("the merge still writes conflict records", conflicts)
		}
		db.QueryRow("SELECT count(*) FROM flats_docs_activations WHERE doc='index.md'").Scan(&hashes)
		if hashes != 2 {
			t.Fatal("test did not revisit two hashes", hashes)
		}
		x.Close()
		a.stop()
		a = next
	}
	// Acknowledging every entry leaves only the recorded texts.
	entries := a.outbox(t)
	_, out := a.hostOp(t, "POST", "sync", map[string]any{})
	if status, res := a.hostOp(t, "POST", "ack", map[string]any{"store": out["store"], "through": entries[len(entries)-1].Seq}); status != 200 {
		t.Fatal("ack", status, res)
	}
	var kept int
	db.QueryRow("SELECT count(*) FROM flats_docs_texts").Scan(&kept)
	if kept > 2 {
		t.Fatal("acknowledged texts remain", kept)
	}
}

func TestRejectedTextMutationReloadsWithoutDisconnectingPeers(t *testing.T) {
	t.Parallel()
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
