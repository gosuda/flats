package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
)

// coreImage is what the fake core holds at a crash moment: its blobs,
// revisions and import cursor are durable outside board.db, so a killed
// process leaves them as they were.
func coreImage(f *fakeCore) *fakeCore {
	f.data.Lock()
	defer f.data.Unlock()
	c := newFakeCore("")
	c.blobs = maps.Clone(f.blobs)
	c.revs = slices.Clone(f.revs)
	c.cursor, c.store = f.cursor, f.store
	return c
}

// openCrash opens the board.db copy dir with core as it was at the crash.
func openCrash(t *testing.T, dir string, core *fakeCore) *Instance {
	t.Helper()
	core.dir = filepath.Dir(dir)
	in, created, err := Open(dir, core, Options{Slug: "copy", tickEvery: time.Hour})
	if err != nil || created {
		t.Fatalf("reopen: created %v, %v", created, err)
	}
	core.in = in
	t.Cleanup(in.Stop)
	return in
}

// checkRevisions checks that no imported revision is empty: each one's
// files differ from the previous revision's.
func checkRevisions(t *testing.T, core *fakeCore) {
	t.Helper()
	core.data.Lock()
	defer core.data.Unlock()
	for i := 1; i < len(core.revs); i++ {
		if sameFiles(core.revs[i].files, core.revs[i-1].files) && core.revs[i].info.Kind != "rollback" {
			t.Fatalf("revision %d (%s) is empty: same files as revision %d", core.revs[i].info.Number, core.revs[i].info.Kind, core.revs[i-1].info.Number)
		}
	}
}

// crashArm records a crash copy (board.db and its WAL, plus the core) at
// each armed stage of the next commit that reaches it.
type crashArm struct {
	mu     sync.Mutex
	armed  map[string]bool
	copies map[string]string
	cores  map[string]*fakeCore
}

func newCrashArm(t *testing.T, b *tb) *crashArm {
	a := &crashArm{armed: map[string]bool{}, copies: map[string]string{}, cores: map[string]*fakeCore{}}
	b.in.hook = func(stage string) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if !a.armed[stage] {
			return
		}
		delete(a.armed, stage)
		a.copies[stage] = crashCopy(t, filepath.Join(b.dir, "board"))
		a.cores[stage] = coreImage(b.core)
	}
	return a
}

// set arms stages, dropping earlier copies.
func (a *crashArm) set(stages ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	clear(a.armed)
	for _, s := range stages {
		a.armed[s] = true
		delete(a.copies, s)
		delete(a.cores, s)
	}
}

func (a *crashArm) take(t *testing.T, stage string) (string, *fakeCore) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.copies[stage] == "" {
		t.Fatalf("no crash copy at %s", stage)
	}
	return a.copies[stage], a.cores[stage]
}

// TestCrashPointsOfGroupCloseAndAgentWrite kills the process before and
// after the commit that closes an edit group into an outbox entry, and
// before and after an agent write's commit (board-TECH 14 Crashes). After a
// restart the revision count and content and board.db match, there is no
// empty revision, and every edit acknowledged as saved exists.
func TestCrashPointsOfGroupCloseAndAgentWrite(t *testing.T) {
	ctx := context.Background()
	b := newTB(t)
	if err := b.core.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	a := connect(t, b.priv, map[string]any{"editor": "alice", "name": "Alice"}, nil)
	a.snapshot()
	id := eid()
	if m := a.batch(1, sticky(id, "saved", 0)); m["t"] != "ack" {
		t.Fatal(m)
	}
	arm := newCrashArm(t, b)

	// The group closes on the tick 30 seconds later.
	b.clock.Add(31 * time.Second)
	arm.set("commit", "committed")
	if err := b.in.do(ctx, b.in.tick); err != nil {
		t.Fatal(err)
	}
	beforeClose, beforeCore := arm.take(t, "commit")
	afterTick, afterCore := arm.take(t, "committed")

	// Killed before the group-close commit: the edit is there, the group
	// is still dirty, the outbox has no entry yet; the next tick records
	// it as one person revision.
	c1 := openCrash(t, beforeClose, beforeCore)
	if got := aliveOf(t, c1); got[id] != "saved" {
		t.Fatalf("acknowledged edit lost before the group commit: %v", got)
	}
	var dirty bool
	_ = c1.do(ctx, func() error { dirty = c1.sd.groups["browser:alice"].Dirty; return nil })
	if !dirty {
		t.Fatal("the open edit group was lost")
	}
	if n := len(mustOutbox(t, c1)); n != 0 {
		t.Fatalf("outbox before the group commit has %d entries", n)
	}
	if err := c1.do(ctx, c1.tick); err != nil {
		t.Fatal(err)
	}
	if err := beforeCore.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if kinds := revKinds(beforeCore); !slices.Equal(kinds, []string{"created", "person"}) {
		t.Fatalf("revisions after restart %v", kinds)
	}
	checkRevisions(t, beforeCore)

	// Killed after the group-close commit, before core imported it: the
	// entry is in the outbox once and imports as one revision.
	c2 := openCrash(t, afterTick, afterCore)
	if got := aliveOf(t, c2); got[id] != "saved" {
		t.Fatalf("acknowledged edit lost after the group commit: %v", got)
	}
	if n := len(mustOutbox(t, c2)); n != 1 {
		t.Fatalf("outbox after the group commit has %d entries", n)
	}
	if err := afterCore.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if err := afterCore.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if kinds := revKinds(afterCore); !slices.Equal(kinds, []string{"created", "person"}) {
		t.Fatalf("revisions after restart %v", kinds)
	}
	checkRevisions(t, afterCore)
	// The person revision holds the edit.
	_, content, err := afterCore.RevisionContent(ctx, 2)
	if err != nil || len(content.Elements) != 1 || content.Elements[0].Text != "saved" {
		t.Fatalf("revision 2 content %+v %v", content.Elements, err)
	}

	// An agent write: killed before its commit it is not there and its op
	// id is unknown, so the agent's retry applies it; killed after, the
	// edit and its op are there and a retry replays the same result.
	if err := b.core.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	req := UpdateRequest{OpID: "crash-agent-1", Label: "Agent",
		Ops: []engine.AgentOp{{Op: "add", Ref: "$s", El: json.RawMessage(`{"type":"sticky","text":"agent"}`)}}}
	arm.set("commit", "committed")
	res, err := b.in.UpdateBoard(ctx, req, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"commit", "committed"} {
		dir, core := arm.take(t, stage)
		in := openCrash(t, dir, core)
		got := aliveOf(t, in)
		_, has := got[res.IDs["$s"]]
		_, known, err := in.Op(ctx, req.OpID)
		if err != nil {
			t.Fatal(err)
		}
		if stage == "commit" && (has || known) {
			t.Fatalf("killed before the agent commit: element %v, op %v", has, known)
		}
		if stage == "committed" && (!has || !known) {
			t.Fatalf("killed after the agent commit: element %v, op %v", has, known)
		}
		again, err := in.UpdateBoard(ctx, req, 0)
		if err != nil {
			t.Fatalf("%s: retry after restart: %v", stage, err)
		}
		if stage == "committed" && (!again.Replayed || again.IDs["$s"] != res.IDs["$s"]) {
			t.Fatalf("retry after a committed write %+v", again)
		}
		if stage == "commit" && again.Replayed {
			t.Fatalf("retry after an uncommitted write replayed %+v", again)
		}
		if err := core.importOutbox(ctx); err != nil {
			t.Fatal(err)
		}
		kinds := revKinds(core)
		if n := len(kinds); kinds[n-1] != "agent" || slices.Index(kinds, "agent") != n-1 {
			t.Fatalf("%s: revisions after restart %v", stage, kinds)
		}
		checkRevisions(t, core)
		if n := len(aliveOf(t, in)); n != 2 {
			t.Fatalf("%s: %d elements after the retry", stage, n)
		}
	}
}

func mustOutbox(t *testing.T, in *Instance) []OutboxEntry {
	t.Helper()
	entries, err := in.Outbox(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func revKinds(f *fakeCore) []string {
	f.data.Lock()
	defer f.data.Unlock()
	var out []string
	for _, r := range f.revs {
		out = append(out, r.info.Kind)
	}
	return out
}

// TestCrashPointsOfRollback kills the process before and after a
// rollback's commit (board-TECH 14 Crashes, 4.11).
func TestCrashPointsOfRollback(t *testing.T) {
	ctx := context.Background()
	b := newTB(t)
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	id := eid()
	a.batch(1, sticky(id, "first", 0))
	b.clock.Add(31 * time.Second)
	if err := b.in.do(ctx, b.in.tick); err != nil {
		t.Fatal(err)
	}
	if err := b.core.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	target := b.core.lastRev()
	other := eid()
	a.batch(2, map[string]any{"op": "set", "id": id, "inc": 0, "g": "text", "val": "second"}, sticky(other, "later", 300))
	b.clock.Add(31 * time.Second)
	if err := b.in.do(ctx, b.in.tick); err != nil {
		t.Fatal(err)
	}
	if err := b.core.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	revsBefore := len(revKinds(b.core))
	_, c, err := b.core.RevisionContent(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	targetRev, _ := b.core.rev(target)

	arm := newCrashArm(t, b)
	arm.set("commit", "committed")
	if _, err := b.in.Rollback(ctx, "rollback-crash", target, c, "Alice"); err != nil {
		t.Fatal(err)
	}
	copies, cores := map[string]string{}, map[string]*fakeCore{}
	for _, stage := range []string{"commit", "committed"} {
		copies[stage], cores[stage] = arm.take(t, stage)
	}

	// Killed before COMMIT: the board is as before and the op is unknown,
	// so core settles the hold as never committed.
	before := openCrash(t, copies["commit"], cores["commit"])
	if got := aliveOf(t, before); got[id] != "second" || got[other] != "later" {
		t.Fatalf("killed before the rollback commit: %v", got)
	}
	if _, ok, _ := before.Op(ctx, "rollback-crash"); ok {
		t.Fatal("the rollback op survived an uncommitted rollback")
	}
	if n := len(mustOutbox(t, before)); n != 0 {
		t.Fatalf("outbox after an uncommitted rollback has %d entries", n)
	}

	// Killed after COMMIT: the board equals the target revision, the op is
	// known with its entry, and the import records exactly one rollback
	// revision with the target's files.
	after := openCrash(t, copies["committed"], cores["committed"])
	if got := aliveOf(t, after); len(got) != 1 || got[id] != "first" {
		t.Fatalf("killed after the rollback commit: %v", got)
	}
	op, ok, err := after.Op(ctx, "rollback-crash")
	if err != nil || !ok || op.Kind != "rollback" || op.OutboxSeq == 0 {
		t.Fatalf("rollback op after the crash %+v %v %v", op, ok, err)
	}
	core := cores["committed"]
	if err := core.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if err := core.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	kinds := revKinds(core)
	if len(kinds) != revsBefore+1 || kinds[len(kinds)-1] != "rollback" {
		t.Fatalf("revisions after the crash %v (before %d)", kinds, revsBefore)
	}
	last, _ := core.rev(core.lastRev())
	if last.info.RollbackOf != target || !sameFiles(last.files, targetRev.files) {
		t.Fatalf("rollback revision %+v files differ from revision %d", last.info, target)
	}
	checkRevisions(t, core)
	// The same op again (core's retry after the restart) replays.
	res, err := after.Rollback(ctx, "rollback-crash", target, c, "Alice")
	if err != nil || !res.Replayed {
		t.Fatalf("rollback retry %+v %v", res, err)
	}
}

// TestWALBusyIsRetriedAndSurvivesACrash holds a reader on board.db so
// the WAL truncation after a commit is busy (board-TECH 5.4, 14 Crashes):
// the busy result is recorded in meta.wal_truncate and retried on every
// later commit, a crash in that state loses nothing, and once the reader is
// gone the next commit truncates the log.
func TestWALBusyIsRetriedAndSurvivesACrash(t *testing.T) {
	ctx := context.Background()
	b := newTB(t)
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	if m := a.batch(1, sticky(eid(), "warm", 0)); m["t"] != "ack" {
		t.Fatal(m)
	}
	path := filepath.Join(b.dir, "board", dbName)
	u := url.URL{Scheme: "file", Path: path}
	reader, err := sql.Open("sqlite", u.String()+"?_pragma=busy_timeout(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	reader.SetMaxOpenConns(1)
	rtx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := rtx.QueryRowContext(ctx, `SELECT count(*) FROM meta`).Scan(&n); err != nil {
		t.Fatal(err)
	}

	// A batch large enough to put more than 64 frames in the log.
	var ops []map[string]any
	ids := map[string]string{}
	for i := 0; i < 120; i++ {
		id := eid()
		ids[id] = noise(2000, i)
		ops = append(ops, sticky(id, ids[id], float64(i*210)))
	}
	start := time.Now()
	if m := batchWithin(a, 30*time.Second, 2, ops...); m["t"] != "ack" {
		t.Fatal(m)
	}
	t.Logf("a commit with a busy log answered after %v", time.Since(start))
	pending := func(in *Instance) (bool, string) {
		var p bool
		var v string
		_ = in.do(ctx, func() error {
			p = in.sd.walPending
			return in.db.db.QueryRowContext(ctx, `SELECT coalesce((SELECT value FROM meta WHERE key = 'wal_truncate'), '')`).Scan(&v)
		})
		return p, v
	}
	if p, _ := pending(b.in); !p {
		t.Fatal("a busy truncation was not recorded as pending")
	}
	walSize := func() int64 {
		fi, err := os.Stat(path + "-wal")
		if err != nil {
			return 0
		}
		return fi.Size()
	}
	if walSize() == 0 {
		t.Fatal("the log was truncated under a reader")
	}
	// The next commit records the pending truncation in meta (and retries).
	start = time.Now()
	if m := batchWithin(a, 30*time.Second, 3, sticky(eid(), "while busy", 0)); m["t"] != "ack" {
		t.Fatal(m)
	}
	t.Logf("the next commit with a busy log answered after %v", time.Since(start))
	if p, v := pending(b.in); !p || v != "1" {
		t.Fatalf("pending %v, meta.wal_truncate %q", p, v)
	}

	// Killed in this state: everything acknowledged is there, and the
	// reopened board still knows the truncation is pending.
	copyDir := crashCopy(t, filepath.Join(b.dir, "board"))
	c := openCrash(t, copyDir, coreImage(b.core))
	got := aliveOf(t, c)
	for id, text := range ids {
		if got[id] != text {
			t.Fatalf("element %s lost after a crash with a busy log", id)
		}
	}
	if len(got) != 122 {
		t.Fatalf("%d elements after the crash", len(got))
	}
	if p, _ := pending(c); p {
		// The reopened copy has no reader, so its load truncates at once.
		t.Fatal("the reopened board did not truncate its log")
	}

	// The reader goes away; the next commit truncates and clears the mark.
	if err := rtx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if m := a.batch(4, sticky(eid(), "after the reader", 0)); m["t"] != "ack" {
		t.Fatal(m)
	}
	if p, _ := pending(b.in); p {
		t.Fatal("the truncation is still pending after the reader left")
	}
	if s := walSize(); s != 0 {
		t.Fatalf("the log is %d bytes after the retried truncation", s)
	}
	// The cleared mark is persisted with the next commit.
	if m := a.batch(5, sticky(eid(), "one more", 0)); m["t"] != "ack" {
		t.Fatal(m)
	}
	if _, v := pending(b.in); v != "" {
		t.Fatalf("meta.wal_truncate %q after the truncation", v)
	}
}

// batchWithin is client.batch with a longer wait for the answer.
func batchWithin(c *client, d time.Duration, n int, ops ...map[string]any) map[string]any {
	c.t.Helper()
	c.send(map[string]any{"t": "ops", "n": n, "ops": ops})
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		m, ok := c.next(func(m map[string]any) bool {
			return (m["t"] == "ack" || m["t"] == "reject") && int(m["n"].(float64)) == n
		})
		if ok {
			return m
		}
	}
	c.t.Fatalf("no answer to batch %d within %v", n, d)
	return nil
}
