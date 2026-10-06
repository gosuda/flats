package docs

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
	_ "modernc.org/sqlite"
)

var appGeneration atomic.Int64

const initialText = "# Shared\n\nBase line\nAgent line\n"

type app struct {
	srv  *httptest.Server
	inst core.Instance
	data string
	pid  int
}

func module(text, version string, removed ...bool) string {
	sum := sha256.Sum256([]byte(text))
	value := map[string]any{"format": 1, "hash": version, "entry": "index.md", "title": "Shared document", "documents": []any{map[string]any{"path": "index.md", "hash": hex.EncodeToString(sum[:]), "text": text}, map[string]any{"path": "nested/other.md", "hash": "other", "text": "# Other\n"}}, "assets": []string{}}
	if len(removed) > 0 && removed[0] {
		value["documents"] = value["documents"].([]any)[:1]
	}
	b, _ := json.Marshal(value)
	return "export default " + string(b) + ";\n"
}
func startApp(t *testing.T, data, text, version string, removed ...bool) *app {
	return startAppWithEnvironment(t, data, text, version, map[string]string{}, removed...)
}
func startAppWithEnvironment(t *testing.T, data, text, version string, environment map[string]string, removed ...bool) *app {
	t.Helper()
	dir := t.TempDir()
	err := fs.WalkDir(FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			b, e := fs.ReadFile(FS(), path)
			if e != nil {
				return e
			}
			return os.WriteFile(filepath.Join(dir, path), b, 0600)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, ContentModule), []byte(module(text, version, removed...)), 0600); err != nil {
		t.Fatal(err)
	}
	number := 1
	if n, e := strconv.Atoi(strings.TrimPrefix(version, "v")); e == nil {
		number = n
	}
	var workerPID atomic.Int64
	inst, err := manager(t).Start(context.Background(), core.RuntimeSpec{Flat: "docs", Version: number, Generation: appGeneration.Add(1), Env: environment, Dir: dir, Entry: Entry, DataDir: data, Log: func(level, msg string) {
		if raw, ok := strings.CutPrefix(msg, "docs test worker pid:"); ok {
			if pid, err := strconv.Atoi(raw); err == nil {
				workerPID.Store(int64(pid))
			}
			return
		}
		t.Log(level, msg)
	}})
	if err != nil {
		t.Fatal(err)
	}
	a := &app{srv: httptest.NewServer(inst), inst: inst, data: data, pid: int(workerPID.Load())}
	t.Cleanup(a.stop)
	return a
}
func (a *app) stop() {
	if a.inst != nil {
		a.srv.Close()
		a.inst.Stop()
		a.inst = nil
	}
}
func (a *app) get(t *testing.T, path string) (int, string, http.Header) {
	t.Helper()
	return a.getHeaders(t, path, nil)
}
func (a *app) getHeaders(t *testing.T, path string, headers http.Header) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest("GET", a.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if headers != nil {
		req.Header = headers
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b), r.Header
}
func (a *app) document(t *testing.T) map[string]any {
	t.Helper()
	status, b, _ := a.getHeaders(t, "/_docs/api/document", http.Header{"X-Flats-Access": {"private"}})
	if status != 200 {
		t.Fatalf("document: %d %s", status, b)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(b), &m); err != nil {
		t.Fatal(err)
	}
	return m
}
func known(m map[string]any) map[string]any {
	return map[string]any{"epoch": m["epoch"], "seq": m["seq"], "chain": m["chain"]}
}

type updateFixture struct {
	ID string `json:"id"`
	U  string `json:"u"`
}
type fixtures struct {
	Seed        string
	Updates     []updateFixture
	Independent string
	Compact     []updateFixture
}

func fixture(t *testing.T) fixtures {
	t.Helper()
	b, err := os.ReadFile("testdata/updates.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixtures
	if err = json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}
func edit(t *testing.T, s *socket, id, u string) map[string]any {
	t.Helper()
	s.send(t, map[string]any{"t": "update", "id": id, "u": u})
	return s.next(t, "ack")
}
func TestAppRoutesAndSeed(t *testing.T) {
	a := startApp(t, t.TempDir(), initialText, "v1")
	if status, b, _ := a.getHeaders(t, "/_docs/healthz", http.Header{"X-Flats-Health": {"1"}}); status != 200 || b != "ok" {
		t.Fatal(status, b)
	}
	if _, err := os.Stat(filepath.Join(a.data, "db.sqlite")); err != nil {
		t.Fatal("health did not activate documents", err)
	}
	if status, b, _ := a.get(t, "/_docs/api/documents"); status != 200 || !strings.Contains(b, "nested/other.md") {
		t.Fatal(status, b)
	}
	m := a.document(t)
	if m["markdown"] != initialText || m["source"] != "live" {
		t.Fatal(m)
	}
	for _, path := range []string{"/", "/index.md", "/nested/other", "/_docs/assets/client.js", "/_docs/assets/client.css"} {
		if status, b, h := a.get(t, path); status != 200 {
			t.Fatalf("%s: %d %.100s", path, status, b)
		} else if path == "/" {
			if !strings.Contains(h.Get("Content-Security-Policy"), "script-src 'self'") || h.Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal(h)
			}
		}
	}
	for _, path := range []string{"/missing", "/_docs/api/document?doc=removed.md", "/_docs/nope"} {
		if status, _, _ := a.get(t, path); status != 404 {
			t.Fatal(path, status)
		}
	}
}
func TestConcurrentCommitResendAndPersistence(t *testing.T) {
	data := t.TempDir()
	a := startApp(t, data, initialText, "v1")
	f := fixture(t)
	x, y := connect(t, a.srv.URL, "private"), connect(t, a.srv.URL, "private")
	wx := x.hello(t, nil)
	y.hello(t, nil)
	// Standalone Yjs clients concurrently insert independent content.
	x.send(t, map[string]any{"t": "update", "id": "person", "u": f.Independent})
	y.send(t, map[string]any{"t": "update", "id": "peer", "u": f.Updates[1].U})
	ack := x.next(t, "ack")
	event := y.next(t, "update")
	if event["seq"] != ack["seq"] || event["chain"] != ack["chain"] {
		t.Fatal(event, ack)
	}
	y.next(t, "ack")
	x.next(t, "update")
	m := a.document(t)
	text := m["markdown"].(string)
	for _, value := range []string{"Person 한국어 🙂", "Peer 🌍", "Base line"} {
		if !strings.Contains(text, value) {
			t.Fatal(text)
		}
	}
	before := m["seq"]
	dup := edit(t, x, "person", f.Independent)
	if dup["seq"] != ack["seq"] || a.document(t)["seq"] != before {
		t.Fatal("duplicate changed seq")
	}
	// Sequence observed in ACK is already committed and queryable.
	if before.(float64) < ack["seq"].(float64) {
		t.Fatal("ACK before commit")
	}
	x.Close()
	y.Close()
	a.stop()
	a = startApp(t, data, initialText, "v1")
	if a.document(t)["markdown"] != text {
		t.Fatal("restart lost edit")
	}
	z := connect(t, a.srv.URL, "private")
	z.hello(t, known(wx))
	edit(t, z, "person", f.Independent)
	z.send(t, map[string]any{"t": "update", "id": "person", "u": f.Updates[1].U})
	z.rejected(t, "invalid_update")
}
func TestReadonlyAndInvalidFrames(t *testing.T) {
	a := startApp(t, t.TempDir(), initialText, "v1")
	f := fixture(t)
	for _, access := range []string{"public", "", "unknown"} {
		s := connect(t, a.srv.URL, access)
		if w := s.hello(t, nil); w["readonly"] != true {
			t.Fatal(w)
		}
		s.send(t, map[string]any{"t": "update", "id": "forbidden", "u": f.Independent})
		s.rejected(t, "readonly")
	}
	if a.document(t)["markdown"] != initialText {
		t.Fatal("read-only write")
	}
	for i, value := range []string{`not json`, `{"t":"update","id":"x","u":"!!!!"}`, strings.Repeat("x", 512*1024+1)} {
		s := connect(t, a.srv.URL, "private")
		s.hello(t, nil)
		if err := s.write([]byte(value)); err != nil {
			t.Fatal(err)
		}
		s.rejected(t, []string{"invalid_update", "invalid_update", "capacity"}[i])
	}
}
func TestCompactionAndActivationOverlap(t *testing.T) {
	data := t.TempDir()
	old := startApp(t, data, initialText, "v1")
	f := fixture(t)
	x := connect(t, old.srv.URL, "private")
	welcome := x.hello(t, nil)
	edit(t, x, "person", f.Independent)
	for _, u := range f.Compact {
		edit(t, x, u.ID, u.U)
		time.Sleep(55 * time.Millisecond)
	}
	text := old.document(t)["markdown"].(string)
	db, err := sql.Open("sqlite", filepath.Join(data, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var cp, rows int
	if err = db.QueryRow("SELECT seq FROM flats_docs_checkpoints WHERE doc='index.md'").Scan(&cp); err != nil {
		t.Fatal(err)
	}
	db.QueryRow("SELECT count(*) FROM flats_docs_updates WHERE doc='index.md'").Scan(&rows)
	if cp < 64 || rows >= 64 {
		t.Fatal(cp, rows)
	}
	fresh := connect(t, old.srv.URL, "private")
	fresh.hello(t, known(welcome))
	if old.document(t)["markdown"] != text {
		t.Fatal("compaction lost content")
	}
	edit(t, fresh, "compact-0", f.Compact[0].U)
	newer := startApp(t, data, strings.Replace(initialText, "Agent line", "Agent updated", 1), "v2")
	n := connect(t, newer.srv.URL, "private")
	n.hello(t, nil)
	merged := newer.document(t)["markdown"].(string)
	if !strings.Contains(merged, "Person 한국어 🙂") || !strings.Contains(merged, "Agent updated") {
		t.Fatal(merged)
	}
	// Old worker catches up without reapplying its old activation. Both append to one authority.
	edit(t, x, "old-overlap", f.Updates[1].U)
	n.send(t, map[string]any{"t": "ping"})
	n.next(t, "pong")
	if newer.document(t)["markdown"] != old.document(t)["markdown"] {
		t.Fatal("overlap diverged")
	}
	newer.stop()
	old.stop()
	again := startApp(t, data, strings.Replace(initialText, "Agent line", "Agent updated", 1), "v2")
	if again.document(t)["markdown"] != merged && !strings.Contains(again.document(t)["markdown"].(string), "Peer 🌍") {
		t.Fatal("restart compact/merge")
	}
}
func TestDivergedAfterOlderDataRestore(t *testing.T) {
	data := t.TempDir()
	a := startApp(t, data, initialText, "v1")
	a.document(t)
	a.stop()
	snapshot, err := os.ReadFile(filepath.Join(data, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	a = startApp(t, data, initialText, "v1")
	x := connect(t, a.srv.URL, "private")
	w := x.hello(t, nil)
	ack := edit(t, x, "person", fixture(t).Independent)
	k := known(ack)
	k["epoch"] = w["epoch"]
	x.Close()
	a.stop()
	os.Remove(filepath.Join(data, "db.sqlite-wal"))
	os.Remove(filepath.Join(data, "db.sqlite-shm"))
	if err = os.WriteFile(filepath.Join(data, "db.sqlite"), snapshot, 0600); err != nil {
		t.Fatal(err)
	}
	a = startApp(t, data, initialText, "v1")
	s := connect(t, a.srv.URL, "private")
	s.send(t, map[string]any{"t": "hello", "v": 1, "doc": "index.md", "sv": "AA==", "known": k})
	s.next(t, "diverged")
	if a.document(t)["markdown"] != initialText {
		t.Fatal("restore polluted")
	}
}
func TestFailedAppendDoesNotAckOrPersist(t *testing.T) {
	a := startApp(t, t.TempDir(), initialText, "v1")
	s := connect(t, a.srv.URL, "private")
	s.hello(t, nil)
	db, err := sql.Open("sqlite", filepath.Join(a.data, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("CREATE TRIGGER reject_update BEFORE INSERT ON flats_docs_updates BEGIN SELECT RAISE(ABORT,'injected write failure'); END"); err != nil {
		t.Fatal(err)
	}
	s.send(t, map[string]any{"t": "update", "id": "fail", "u": fixture(t).Independent})
	s.closed(t, 1012, "resync")
	if a.document(t)["markdown"] != initialText {
		t.Fatal("uncommitted mutation leaked")
	}
	if _, err = db.Exec("DROP TRIGGER reject_update"); err != nil {
		t.Fatal(err)
	}
	again := connect(t, a.srv.URL, "private")
	again.hello(t, nil)
	edit(t, again, "fail", fixture(t).Independent)
	if !strings.Contains(a.document(t)["markdown"].(string), "Person") {
		t.Fatal("resync failed")
	}
}

func TestPresenceIdentityAndRemoval(t *testing.T) {
	a := startApp(t, t.TempDir(), initialText, "v1")
	x := connect(t, a.srv.URL, "private")
	w := x.hello(t, nil)
	if w["you"].(map[string]any)["verified"] != false {
		t.Fatal(w)
	}
	y := connect(t, a.srv.URL, "private")
	y.hello(t, nil)
	// Awareness fixture with forged user metadata; server rewrites it to the connection identity.
	x.send(t, map[string]any{"t": "aw", "u": "AWgBEXsidXNlciI6IkZvcmdlZCJ9"})
	event := y.next(t, "aw")
	if event["u"] == "AWgBEXsidXNlciI6IkZvcmdlZCJ9" {
		t.Fatal("identity was not rewritten")
	}
	x.Close()
	removed := y.next(t, "aw")
	if removed["u"] == event["u"] {
		t.Fatal("presence not removed")
	}
}

func TestFailedCommitDoesNotAckOrPersist(t *testing.T) {
	a := startApp(t, t.TempDir(), initialText, "v1")
	s := connect(t, a.srv.URL, "private")
	s.hello(t, nil)
	db, err := sql.Open("sqlite", filepath.Join(a.data, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statements := []string{"CREATE TABLE commit_parent(k INTEGER PRIMARY KEY)", "CREATE TABLE commit_child(k INTEGER REFERENCES commit_parent(k) DEFERRABLE INITIALLY DEFERRED)", "CREATE TRIGGER fail_commit AFTER INSERT ON flats_docs_updates BEGIN INSERT INTO commit_child VALUES(999); END"}
	for _, q := range statements {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.send(t, map[string]any{"t": "update", "id": "commit-fail", "u": fixture(t).Independent})
	s.closed(t, 1012, "resync")
	if a.document(t)["markdown"] != initialText {
		t.Fatal("COMMIT failure leaked an edit")
	}
	var count int
	if err = db.QueryRow("SELECT count(*) FROM flats_docs_receipts WHERE id='commit-fail'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("COMMIT failure left receipt")
	}
	db.Exec("DROP TRIGGER fail_commit")
	again := connect(t, a.srv.URL, "private")
	again.hello(t, nil)
	edit(t, again, "commit-fail", fixture(t).Independent)
}

func TestIdleOlderWorkerCannotApplyObsoleteSource(t *testing.T) {
	data := t.TempDir()
	old := startApp(t, data, "# Obsolete\nold source\n", "v1")
	newer := startApp(t, data, "# Active\nnew source\n", "v2")
	expected := newer.document(t)["markdown"]
	// The old hash has never been observed: a visited-hash set alone is insufficient.
	x := connect(t, old.srv.URL, "private")
	x.hello(t, nil)
	edit(t, x, "overlap-idle", fixture(t).Independent)
	got := newer.document(t)["markdown"].(string)
	if !strings.Contains(got, expected.(string)) || strings.Contains(got, "Obsolete") {
		t.Fatal("idle old worker applied obsolete source", got)
	}
}
func TestRemovedDocumentHiddenKeepsStoredState(t *testing.T) {
	data := t.TempDir()
	old := startApp(t, data, initialText, "v1")
	if status, _, _ := old.get(t, "/_docs/api/document?doc=nested/other.md"); status != 200 {
		t.Fatal(status)
	}
	old.stop()
	newer := startApp(t, data, initialText, "v2", true)
	newer.document(t)
	if status, _, _ := newer.get(t, "/_docs/api/document?doc=nested/other.md"); status != 404 {
		t.Fatal(status)
	}
	if status, body, _ := newer.get(t, "/_docs/api/documents"); status != 200 || strings.Contains(body, "nested/other.md") {
		t.Fatal(status, body)
	}
	db, err := sql.Open("sqlite", filepath.Join(data, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err = db.QueryRow("SELECT count(*) FROM flats_docs_documents WHERE doc='nested/other.md'").Scan(&count); err != nil || count != 1 {
		t.Fatal("stored state lost", count, err)
	}
}

func TestAcknowledgedEditSurvivesKilledWorker(t *testing.T) {
	data := t.TempDir()
	a := startApp(t, data, initialText, "v1")
	x := connect(t, a.srv.URL, "private")
	x.hello(t, nil)
	edit(t, x, "crash-ack", fixture(t).Independent)
	if a.pid <= 0 {
		t.Fatal("test worker PID unavailable")
	}
	if err := syscall.Kill(a.pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	a.stop()
	restarted := startApp(t, data, initialText, "v1")
	if !strings.Contains(restarted.document(t)["markdown"].(string), "Person 한국어 🙂") {
		t.Fatal("acknowledged edit lost after SIGKILL")
	}
	z := connect(t, restarted.srv.URL, "private")
	z.hello(t, nil)
	edit(t, z, "crash-ack", fixture(t).Independent)
}

func TestRollbackReappliesObservedHashOnce(t *testing.T) {
	data := t.TempDir()
	v1 := startApp(t, data, initialText, "v1")
	x := connect(t, v1.srv.URL, "private")
	x.hello(t, nil)
	edit(t, x, "human", fixture(t).Independent)
	v2 := startApp(t, data, strings.Replace(initialText, "Agent line", "Agent v2", 1), "v2")
	if got := v2.document(t)["markdown"].(string); !strings.Contains(got, "Agent v2") || !strings.Contains(got, "Person 한국어 🙂") {
		t.Fatal(got)
	}
	rollback := startApp(t, data, initialText, "v1")
	got := rollback.document(t)
	text := got["markdown"].(string)
	if !strings.Contains(text, "Agent line") || strings.Contains(text, "Agent v2") || !strings.Contains(text, "Person 한국어 🙂") {
		t.Fatal("rollback failed", got)
	}
	// Both draining workers can catch up, but neither can merge its source.
	for _, a := range []*app{v1, v2, rollback} {
		if next := a.document(t); next["markdown"] != text || next["seq"] != got["seq"] {
			t.Fatal("overlap applied source twice", next, got)
		}
	}
}

func TestDocsResponsesIgnoreApplicationEnvironment(t *testing.T) {
	environment := map[string]string{"MODE": "ordinary-doc-canary", "TOKEN": "secret-doc-canary"}
	a := startAppWithEnvironment(t, t.TempDir(), initialText, "env-v1", environment)
	for _, access := range []string{"private", "public"} {
		for _, path := range []string{"/", "/_docs/healthz", "/_docs/api/document", "/content.js"} {
			status, body, headers := a.getHeaders(t, path, http.Header{"X-Flats-Access": {access}})
			if path != "/content.js" && status != 200 {
				t.Fatalf("%s: %d %s", path, status, body)
			}
			if path == "/content.js" && status != 404 {
				t.Fatalf("content module exposed: %d", status)
			}
			for _, value := range environment {
				if strings.Contains(body, value) || strings.Contains(fmt.Sprint(headers), value) {
					t.Fatalf("environment leaked from %s", path)
				}
			}
		}
	}
}
