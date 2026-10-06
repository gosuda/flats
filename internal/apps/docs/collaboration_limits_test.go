package docs

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFullRewriteActivation(t *testing.T) {
	for _, n := range []int{100, 400, 2000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			var base, target strings.Builder
			for i := 0; i < n; i++ {
				fmt.Fprintf(&base, "old line %d\n", i)
				fmt.Fprintf(&target, "rewritten line %d\n", i)
			}
			data := t.TempDir()
			old := startApp(t, data, base.String(), "rewrite-v1")
			x := connect(t, old.srv.URL, "private")
			x.hello(t, nil)
			// An independent Yjs client inserts a concurrent human edit.
			edit(t, x, "human", fixture(t).Independent)
			live := old.document(t)["markdown"]
			next := startApp(t, data, target.String(), "rewrite-v2")
			start := time.Now()
			if status, body, _ := next.getHeaders(t, "/_docs/healthz", http.Header{"X-Flats-Health": {"1"}}); status != 200 {
				t.Fatal(status, body)
			}
			elapsed := time.Since(start)
			if elapsed > 5*time.Second {
				t.Fatalf("activation took %v", elapsed)
			}
			got := next.document(t)
			if got["markdown"] != target.String() {
				t.Fatal("rewrite did not activate")
			}
			conflicts := got["conflicts"].([]any)
			if len(conflicts) != 1 {
				t.Fatal("conflict metadata missing", conflicts)
			}
			generation := conflicts[0].(map[string]any)["generation"].(float64)
			status, body, _ := next.getHeaders(t, fmt.Sprintf("/_docs/api/conflict?generation=%.0f", generation), http.Header{"X-Flats-Access": {"private"}})
			var preserved map[string]any
			if status != 200 || json.Unmarshal([]byte(body), &preserved) != nil || preserved["markdown"] != live {
				t.Fatal("live text not recoverable")
			}
			status, body, headers := next.getHeaders(t, fmt.Sprintf("/_docs/api/conflict?generation=%.0f&view=1", generation), http.Header{"X-Flats-Access": {"private"}})
			if status != 200 || body != live || headers.Get("Content-Type") != "text/plain; charset=utf-8" || headers.Get("X-Content-Type-Options") != "nosniff" || headers.Get("Cache-Control") != "no-store" || headers.Get("Content-Security-Policy") == "" {
				t.Fatal("unsafe conflict view", status, headers)
			}
			y := connect(t, next.srv.URL, "private")
			y.hello(t, nil)
			y.send(t, map[string]any{"t": "ping"})
			y.next(t, "pong")
			if again := next.document(t)["conflicts"].([]any); len(again) != 1 {
				t.Fatal("read duplicated conflict")
			}
			t.Logf("%d-line full rewrite + human edit: health activation %v; API and WebSocket working", n, elapsed)
		})
	}
}

func TestCapacityRejectionKeepsOtherEditor(t *testing.T) {
	a := startApp(t, t.TempDir(), initialText, "v1")
	bad := connect(t, a.srv.URL, "private")
	bad.hello(t, nil)
	good := connect(t, a.srv.URL, "private")
	good.hello(t, nil)
	db, err := sql.Open("sqlite", filepath.Join(a.data, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("UPDATE flats_docs_documents SET seq=100000 WHERE doc='index.md'"); err != nil {
		t.Fatal(err)
	}
	bad.send(t, map[string]any{"t": "update", "id": "capacity", "u": fixture(t).Independent})
	bad.rejected(t, "capacity")
	good.send(t, map[string]any{"t": "ping"})
	good.next(t, "pong")
	if a.document(t)["markdown"] != initialText {
		t.Fatal("rejected update changed committed state")
	}
}

func TestPublicPresenceIsPrivate(t *testing.T) {
	a := startApp(t, t.TempDir(), initialText, "v1")
	editor := connect(t, a.srv.URL, "private")
	editor.hello(t, nil)
	editor.send(t, map[string]any{"t": "aw", "u": "AWgBEXsidXNlciI6IkZvcmdlZCJ9"})
	editor.next(t, "aw")
	viewer := connect(t, a.srv.URL, "public")
	viewer.hello(t, nil)
	editor.send(t, map[string]any{"t": "aw", "u": "AWgBEXsidXNlciI6IkZvcmdlZCJ9"})
	editor.next(t, "aw")
	viewer.send(t, map[string]any{"t": "ping"})
	viewer.next(t, "pong")
	for _, m := range viewer.inbox {
		if m["t"] == "aw" {
			t.Fatal("public viewer received editor identity", m)
		}
	}
	viewer.send(t, map[string]any{"t": "aw", "u": "AWgBEXsidXNlciI6IkZvcmdlZCJ9"})
	viewer.rejected(t, "readonly")
	editor.send(t, map[string]any{"t": "ping"})
	editor.next(t, "pong")
	for _, m := range editor.inbox {
		if m["t"] == "aw" {
			t.Fatal("public presence reached editor", m)
		}
	}
}

func TestPublicReadsDoNotReserveWriter(t *testing.T) {
	a := startApp(t, t.TempDir(), initialText, "v1")
	a.document(t)
	viewer := connect(t, a.srv.URL, "public")
	viewer.hello(t, nil)
	db, err := sql.Open("sqlite", filepath.Join(a.data, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer db.Exec("ROLLBACK")
	if a.document(t)["markdown"] != initialText {
		t.Fatal("read failed while writer reserved")
	}
	viewer.send(t, map[string]any{"t": "ping"})
	viewer.next(t, "pong")
}

func TestHealthActivatesEveryDocumentAtomically(t *testing.T) {
	data := t.TempDir()
	old := startApp(t, data, initialText, "v1")
	old.document(t)
	db, err := sql.Open("sqlite", filepath.Join(data, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("CREATE TRIGGER health_failure BEFORE INSERT ON flats_docs_documents WHEN NEW.doc='nested/other.md' BEGIN SELECT RAISE(ABORT,'health activation failure'); END"); err != nil {
		t.Fatal(err)
	}
	next := startApp(t, data, strings.ReplaceAll(initialText, "Agent", "New agent"), "v2")
	status, _, _ := next.getHeaders(t, "/_docs/healthz", http.Header{"X-Flats-Health": {"1"}})
	if status != 500 {
		t.Fatal("health did not check other document", status)
	}
	if old.document(t)["markdown"] != initialText {
		t.Fatal("failed health left partial activation")
	}
}

func TestPublicCapacityDoesNotConsumeEditors(t *testing.T) {
	a := startApp(t, t.TempDir(), initialText, "v1")
	for i := 0; i < 10; i++ {
		connect(t, a.srv.URL, "public").hello(t, nil)
	}
	overflow := connect(t, a.srv.URL, "public")
	overflow.closed(t, 1008, "connection capacity")
	editor := connect(t, a.srv.URL, "private")
	editor.hello(t, nil)
	edit(t, editor, "editor", fixture(t).Independent)
}

// Large envelopes are within the frame and connection budgets, but two of
// them exhaust the shared room budget if rejected writes are charged to it.
func TestUnauthorizedUpdatesPreserveWriterBudget(t *testing.T) {
	for _, tc := range []struct {
		name, access, rejection string
		joined                  bool
	}{
		{"public-before-hello", "public", "invalid_update", false},
		{"private-before-hello", "private", "invalid_update", false},
		{"joined-public", "public", "readonly", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := startApp(t, t.TempDir(), initialText, "v1")
			editor := connect(t, a.srv.URL, "private")
			editor.hello(t, nil)
			attackers := []*socket{connect(t, a.srv.URL, tc.access), connect(t, a.srv.URL, tc.access)}
			for _, attacker := range attackers {
				if tc.joined {
					attacker.hello(t, nil)
				}
			}
			for _, attacker := range attackers {
				attacker.send(t, map[string]any{"t": "update", "id": "unauthorized", "u": fixture(t).Independent, "padding": strings.Repeat("x", 500*1024)})
				attacker.rejected(t, tc.rejection)
			}
			if a.document(t)["markdown"] != initialText {
				t.Fatal("unauthorized update changed document")
			}
			edit(t, editor, "authorized", fixture(t).Independent)
			if !strings.Contains(a.document(t)["markdown"].(string), "Person 한국어 🙂") {
				t.Fatal("authorized edit was not persisted")
			}
		})
	}
}

func TestConnectionInputLimitPreservesOtherWriter(t *testing.T) {
	a := startApp(t, t.TempDir(), initialText, "v1")
	bad := connect(t, a.srv.URL, "private")
	bad.hello(t, nil)
	good := connect(t, a.srv.URL, "private")
	good.hello(t, nil)
	// Non-update messages must still debit the early per-connection budget.
	for i := 0; i < 2; i++ {
		bad.send(t, map[string]any{"t": "ping", "padding": strings.Repeat("x", 500*1024)})
		bad.next(t, "pong")
	}
	bad.send(t, map[string]any{"t": "ping", "padding": strings.Repeat("x", 500*1024)})
	bad.rejected(t, "capacity")
	edit(t, good, "other-editor", fixture(t).Independent)
}

func TestAuthorizedUpdatesConsumeWriterBudget(t *testing.T) {
	a := startApp(t, t.TempDir(), initialText, "v1")
	first := connect(t, a.srv.URL, "private")
	first.hello(t, nil)
	second := connect(t, a.srv.URL, "private")
	second.hello(t, nil)
	first.send(t, map[string]any{"t": "update", "id": "first", "u": fixture(t).Independent, "padding": strings.Repeat("x", 500*1024)})
	first.next(t, "ack")
	second.next(t, "update")
	second.send(t, map[string]any{"t": "update", "id": "second", "u": fixture(t).Independent, "padding": strings.Repeat("x", 500*1024)})
	second.rejected(t, "capacity")
	first.send(t, map[string]any{"t": "ping"})
	first.next(t, "pong")
}

func TestRejectedRoomOtherWorkerCompaction(t *testing.T) {
	data := t.TempDir()
	a := startApp(t, data, initialText, "v1")
	observer := connect(t, a.srv.URL, "private")
	observer.hello(t, nil)
	writer := connect(t, a.srv.URL, "private")
	writer.hello(t, nil)
	f := fixture(t)
	for _, u := range f.Compact[:5] {
		edit(t, writer, u.ID, u.U)
		observer.next(t, "update")
	}
	// Receipt mismatch rejects after activation has loaded the shared room,
	// invalidating doc while retaining its last committed position.
	writer.send(t, map[string]any{"t": "update", "id": f.Compact[0].ID, "u": f.Independent})
	writer.rejected(t, "invalid_update")
	other := startApp(t, data, initialText, "v1")
	remote := connect(t, other.srv.URL, "private")
	remote.hello(t, nil)
	for _, u := range f.Compact[5:] {
		edit(t, remote, u.ID, u.U)
		time.Sleep(30 * time.Millisecond)
	}
	observer.send(t, map[string]any{"t": "ping"})
	reset := observer.next(t, "update")
	if reset["seq"] != float64(70) {
		t.Fatalf("expected full reset through compaction, got %v", reset)
	}
	observer.next(t, "pong")
	// A full update is substantially larger than a single incremental update.
	if len(reset["u"].(string)) < len(f.Compact[69].U)*2 {
		t.Fatal("incremental update sent as reset")
	}
}

func TestInternalVMErrorResync(t *testing.T) {
	a := startApp(t, t.TempDir(), initialText, "v1")
	editor := connect(t, a.srv.URL, "private")
	editor.hello(t, nil)
	edit(t, editor, "accepted", fixture(t).Independent)
	other := connect(t, a.srv.URL, "private", "nested/other.md")
	other.send(t, map[string]any{"t": "hello", "v": 1, "doc": "nested/other.md", "sv": "AA=="})
	other.next(t, "welcome")
	db, err := sql.Open("sqlite", filepath.Join(a.data, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// A host error carrying an OOM message must escape the app handler.
	if _, err = db.Exec("CREATE TRIGGER vm_failure BEFORE INSERT ON flats_docs_updates BEGIN SELECT RAISE(ABORT,'out of memory'); END"); err != nil {
		t.Fatal(err)
	}
	editor.send(t, map[string]any{"t": "update", "id": "failed", "u": fixture(t).Updates[1].U})
	editor.closed(t, 1012, "resync")
	// Only VM replacement closes the untouched second room.
	other.closed(t, 1011, "server error")
	if _, err = db.Exec("DROP TRIGGER vm_failure"); err != nil {
		t.Fatal(err)
	}
	fresh := connect(t, a.srv.URL, "private")
	fresh.hello(t, nil)
	edit(t, fresh, "recovered", fixture(t).Updates[1].U)
	got := a.document(t)["markdown"].(string)
	if !strings.Contains(got, "Person 한국어 🙂") || !strings.Contains(got, "Peer 🌍") {
		t.Fatal("resync lost committed text")
	}
}
