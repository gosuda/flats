package docs

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
)

// dbBytes is the worker database's size as the disk quota check counts it.
func dbBytes(t *testing.T, data string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(data, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var pages, size int64
	if err := db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("PRAGMA page_size").Scan(&size); err != nil {
		t.Fatal(err)
	}
	return pages * size
}

// walReserve is the write-ahead log's bound at rest, which the worker
// reserves (revisions.js walReserve), for the default 4 KiB pages: 64
// frames.
const walReserve = 32 + 64*(4096+24)

func TestDiskQuotaReservesTheEditsRevision(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	a := startApp(t, data, initialText, "v1")
	// Grow the document while no quota applies, then import everything.
	body, _ := json.Marshal(map[string]any{"ops": []any{map[string]any{"op": "insert", "at": "end", "text": strings.Repeat("Long paragraph text. ", 2000)}}})
	if status, res := a.postEdit(t, hostEdit, string(body)); status != 200 {
		t.Fatal("edit", status, res)
	}
	status, sync := a.hostOp(t, "POST", "sync", map[string]any{})
	if status != 200 {
		t.Fatal("sync", status, sync)
	}
	entries := a.outbox(t)
	if status, out := a.hostOp(t, "POST", "ack", map[string]any{"store": sync["store"], "through": entries[len(entries)-1].Seq, "budget": map[string]any{"quota": 0, "other": 0, "ttl_ms": 60000}}); status != 200 {
		t.Fatal("ack", status, out)
	}
	before := a.document(t)["markdown"].(string)
	budget := func(quota, other int64) {
		t.Helper()
		if status, out := a.hostOp(t, "POST", "budget", map[string]any{"quota": quota, "other": other, "ttl_ms": 60000}); status != 200 {
			t.Fatal("budget", status, out)
		}
	}
	for _, bad := range []map[string]any{{"quota": -1, "other": 0, "ttl_ms": 1}, {"quota": 1, "other": 0}, {"quota": 1.5, "other": 0, "ttl_ms": 1}} {
		if status, _ := a.hostOp(t, "POST", "budget", bad); status != 400 {
			t.Fatal("invalid budget accepted", bad, status)
		}
	}
	// A person's small edit whose live bytes fit is refused when the
	// revision its group will store (the whole document, kept by the worker
	// and by the host) does not.
	const other, slack = 1 << 20, 64 << 10
	budget(walReserve+dbBytes(t, data)+other+slack, other)
	x := connect(t, a.srv.URL, "private")
	x.hello(t, nil)
	x.send(t, map[string]any{"t": "update", "id": "quota", "u": fixture(t).Independent})
	x.rejected(t, "disk_quota")
	if a.document(t)["markdown"] != before {
		t.Fatal("refused edit changed the live text")
	}
	// With room for that revision the same edit commits.
	budget(walReserve+dbBytes(t, data)+other+slack+512<<10, other)
	y := connect(t, a.srv.URL, "private")
	y.hello(t, nil)
	edit(t, y, "quota", fixture(t).Independent)
	edited := a.document(t)["markdown"].(string)
	if edited == before {
		t.Fatal("admitted edit did not commit")
	}
	// A live agent edit is checked the same way and changes nothing.
	count := len(a.outbox(t))
	budget(1, 0)
	status, res := a.postEdit(t, hostEdit, ops(map[string]any{"op": "insert", "at": "end", "text": "Agent note."}))
	if status != 507 || res["code"] != "disk_quota" || !strings.Contains(res["message"].(string), "over its 1-byte disk quota") {
		t.Fatal("agent edit over quota", status, res)
	}
	if a.document(t)["markdown"] != edited || len(a.outbox(t)) != count {
		t.Fatal("refused agent edit changed the document or its history")
	}
	// A budget the host did not refresh in time admits nothing: the edit is
	// a transient failure, so the editor reconnects and resends it.
	if status, out := a.hostOp(t, "POST", "budget", map[string]any{"quota": 0, "other": 0, "ttl_ms": 1}); status != 200 {
		t.Fatal("budget", status, out)
	}
	time.Sleep(10 * time.Millisecond)
	z := connect(t, a.srv.URL, "private")
	z.hello(t, nil)
	z.send(t, map[string]any{"t": "update", "id": "expired", "u": fixture(t).Updates[1].U})
	z.closed(t, 1012, "resync")
	if a.document(t)["markdown"] != edited {
		t.Fatal("edit under an expired budget committed")
	}
	if status, _ := a.postEdit(t, hostEdit, ops(map[string]any{"op": "insert", "at": "end", "text": "Agent note."})); status != 503 {
		t.Fatal("agent edit under an expired budget", status)
	}
	// Quota 0 turns the check off.
	budget(0, 0)
	if status, res := a.postEdit(t, hostEdit, ops(map[string]any{"op": "insert", "at": "end", "text": "Agent note."})); status != 200 {
		t.Fatal("edit without quota", status, res)
	}
}

func TestHostDiskQuotaRefusesLiveEditsButNotRollback(t *testing.T) {
	t.Parallel()
	s, private, _ := hostService(t)
	ctx := t.Context()
	page, err := s.ListRevisions(ctx, "host-doc", 0, 10)
	if err != nil || len(page.Revisions) == 0 {
		t.Fatal(page, err)
	}
	first := page.Revisions[len(page.Revisions)-1].Number
	srv := httptest.NewServer(private.handler("host-doc"))
	defer srv.Close()
	editor := connect(t, srv.URL, "private")
	editor.hello(t, nil)
	edit(t, editor, "before-quota", fixture(t).Independent)
	live, err := s.GetDocument(ctx, "host-doc", "")
	if err != nil {
		t.Fatal(err)
	}
	// The settings change reaches the running document at once.
	if _, err := s.UpdateSettings(ctx, map[string]string{core.SetDiskQuotaBytes: "1"}); err != nil {
		t.Fatal(err)
	}
	other := connect(t, srv.URL, "private")
	other.hello(t, nil)
	other.send(t, map[string]any{"t": "update", "id": "over-quota", "u": fixture(t).Updates[1].U})
	other.rejected(t, "disk_quota")
	ops := []core.DocumentEditOp{{Op: "insert", At: "end", Text: "Agent note."}}
	_, err = s.UpdateDocument(ctx, "host-doc", "", ops, nil, core.ViaMCP)
	if !errors.Is(err, core.ErrDiskQuota) || core.ErrorCategory(err) != "document_capacity" || !strings.Contains(err.Error(), "nothing changed") {
		t.Fatal("update_document over quota", err)
	}
	after, err := s.GetDocument(ctx, "host-doc", "")
	if err != nil || after.Hash != live.Hash || after.Seq != live.Seq {
		t.Fatal("refused update_document changed the live text", after, err)
	}
	// The refused edit recorded nothing, not even the person's edit before
	// it, which stays in their open group.
	if again, err := s.ListRevisions(ctx, "host-doc", 0, 10); err != nil || again.Revisions[0].Number != page.Revisions[0].Number {
		t.Fatal("revisions after a refused edit", again, err)
	}
	// A rollback adds only metadata and is never refused for disk quota.
	if _, err := s.RollbackRevision(ctx, "host-doc", first, "Ada", core.ViaMCP); err != nil {
		t.Fatal("rollback over quota", err)
	}
	if doc, err := s.GetDocument(ctx, "host-doc", ""); err != nil || doc.Markdown != initialText {
		t.Fatal("rollback text", doc, err)
	}
	// Raising the quota lets edits through again.
	if _, err := s.UpdateSettings(ctx, map[string]string{core.SetDiskQuotaBytes: "0"}); err != nil {
		t.Fatal(err)
	}
	if out, err := s.UpdateDocument(ctx, "host-doc", "", ops, nil, core.ViaMCP); err != nil || !out.Changed {
		t.Fatal("update_document without quota", out, err)
	}
}

func TestIdleDocumentKeepsItsWriteAheadLog(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	a := startApp(t, data, initialText, "v1")
	wal := filepath.Join(data, "db.sqlite-wal")
	budget := map[string]any{"quota": 1 << 30, "other": 1000, "ttl_ms": 60000}
	tick := func() {
		t.Helper()
		if status, out := a.hostOp(t, "POST", "sync", map[string]any{}); status != 200 {
			t.Fatal("sync", status, out)
		}
		if status, out := a.hostOp(t, "POST", "budget", budget); status != 200 {
			t.Fatal("budget", status, out)
		}
	}
	tick()
	fi, err := os.Stat(wal)
	if err != nil {
		t.Fatal(err)
	}
	// The host's ticks on an idle document write nothing: an unchanged
	// budget is not rewritten while it is far from expiring.
	for range 20 {
		tick()
	}
	if again, err := os.Stat(wal); err != nil || again.Size() != fi.Size() {
		t.Fatal("idle ticks grew the write-ahead log", fi.Size(), again, err)
	}
	// Edits grow it; a checkpoint truncates it.
	x := connect(t, a.srv.URL, "private")
	x.hello(t, nil)
	edit(t, x, "wal", fixture(t).Independent)
	if status, out := a.hostOp(t, "POST", "checkpoint", map[string]any{}); status != 200 || out["busy"] != false {
		t.Fatal("checkpoint", status, out)
	}
	if fi, err := os.Stat(wal); err != nil || fi.Size() != 0 {
		t.Fatal("write-ahead log not truncated", fi, err)
	}
}

func TestLargeTransactionsDoNotKeepTheirWriteAheadLog(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	a := startApp(t, data, strings.Repeat("Seed paragraph text.\n\n", 20_000), "v1")
	if status, out := a.hostOp(t, "POST", "budget", map[string]any{"quota": 1 << 30, "other": 0, "ttl_ms": 60000}); status != 200 {
		t.Fatal("budget", status, out)
	}
	wal := filepath.Join(data, "db.sqlite-wal")
	walSize := func() int64 {
		fi, err := os.Stat(wal)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}
	if a.document(t)["markdown"] == "" || walSize() > walReserve {
		t.Fatal("seeding a large document kept its log", walSize())
	}
	// Large agent edits write their update, their revision text and, once
	// the log rows pass 512 KiB, a compacted checkpoint of the whole state.
	for i := range 3 {
		body, _ := json.Marshal(map[string]any{"ops": []any{map[string]any{"op": "insert", "at": "end", "text": strings.Repeat("Added text. ", 15_000)}}})
		if status, res := a.postEdit(t, hostEdit, string(body)); status != 200 {
			t.Fatal("edit", i, status, res)
		}
		if size := walSize(); size > walReserve {
			t.Fatal("edit", i, "left a write-ahead log of", size)
		}
	}
}
