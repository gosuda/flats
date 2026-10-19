package board

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/board/model"
)

// crashCopy copies the board directory as it is on disk at this moment, the
// state a killed process would leave.
func crashCopy(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "board")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{dbName, dbName + "-wal"} {
		data, err := os.ReadFile(filepath.Join(src, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func openCopy(t *testing.T, dir string) *Instance {
	t.Helper()
	core := newFakeCore(filepath.Dir(dir))
	in, created, err := Open(dir, core, Options{Slug: "copy"})
	if err != nil || created {
		t.Fatalf("reopen: created %v, %v", created, err)
	}
	core.in = in
	t.Cleanup(in.Stop)
	return in
}

func aliveOf(t *testing.T, in *Instance) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := in.do(context.Background(), func() error {
		for _, e := range in.st.Alive() {
			out[e.ID] = e.Text
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCrashPointsOfTheBatchPath(t *testing.T) {
	b := newTB(t)
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	boardDir := filepath.Join(b.dir, "board")
	var mu sync.Mutex
	copies := map[string]string{}
	arm := ""
	b.in.hook = func(stage string) {
		mu.Lock()
		defer mu.Unlock()
		if stage == arm && copies[stage] == "" {
			copies[stage] = crashCopy(t, boardDir)
		}
	}
	setArm := func(s string) { mu.Lock(); arm = s; mu.Unlock() }

	before := eid()
	setArm("commit")
	if m := a.batch(1, sticky(before, "not durable yet", 0)); m["t"] != "ack" {
		t.Fatal(m)
	}
	after := eid()
	setArm("committed")
	ack := a.batch(2, sticky(after, "durable", 300))
	setArm("")

	// Killed just before COMMIT: the batch is not there and nobody saw it as saved.
	c1 := openCopy(t, copies["commit"])
	if got := aliveOf(t, c1); len(got) != 0 {
		t.Fatalf("uncommitted batch survived: %v", got)
	}
	// Killed right after COMMIT, before the ack went out: the edit is
	// there, and the resent batch is answered from its receipt.
	c2 := openCopy(t, copies["committed"])
	got := aliveOf(t, c2)
	if got[before] != "not durable yet" || got[after] != "durable" {
		t.Fatalf("after commit %v", got)
	}
	var out engine.Outcome
	_ = c2.do(context.Background(), func() error {
		out = c2.st.ApplyBatch(engine.Batch{Session: a.session, N: 2, Now: 0})
		return nil
	})
	if !out.Replayed || float64(out.AckSeq) != ack["seq"] {
		t.Fatalf("resend after the crash %+v", out)
	}
	// A clean restart keeps everything, including the open edit group.
	b.restart()
	if got := b.alive(); len(got) != 2 {
		t.Fatalf("after restart %v", got)
	}
	if len(b.in.sd.groups) != 1 || !b.in.sd.groups["browser:alice"].Dirty {
		t.Fatalf("groups %v", b.in.sd.groups)
	}
}

func TestOutboxImportAndBoundaries(t *testing.T) {
	b := newTB(t)
	ctx := context.Background()
	if err := b.core.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if len(b.core.revs) != 1 || b.core.revs[0].info.Kind != "created" {
		t.Fatalf("revisions %+v", b.core.revs)
	}
	a := connect(t, b.priv, map[string]any{"editor": "alice", "name": "Alice"}, nil)
	a.snapshot()
	id := eid()
	a.batch(1, sticky(id, "one", 0))
	a.batch(2, map[string]any{"op": "set", "id": id, "inc": 0, "g": "text", "val": "two"})

	// 30 seconds idle closes the group into one person revision.
	b.clock.Add(31 * time.Second)
	if err := b.in.do(ctx, b.in.tick); err != nil {
		t.Fatal(err)
	}
	if err := b.core.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(b.core.revs); n != 2 || b.core.revs[1].info.Kind != "person" || b.core.revs[1].info.Authors[0] != "Alice" {
		t.Fatalf("revisions %+v", b.core.revs)
	}
	if len(b.in.sd.snaps) != 0 || b.in.sd.snapBytes != 0 || len(b.in.sd.outbox) != 0 {
		t.Fatalf("acknowledged snapshots remain: %v %d", b.in.sd.snaps, b.in.sd.snapBytes)
	}

	// An agent write closes the dirty group first, then is one revision.
	a.batch(3, map[string]any{"op": "set", "id": id, "inc": 0, "g": "text", "val": "three"})
	res, err := b.in.UpdateBoard(ctx, UpdateRequest{OpID: "agent-op-1", Label: "Agent", Message: "add",
		Ops: []engine.AgentOp{{Op: "add", Ref: "$s", El: json.RawMessage(`{"type":"sticky","text":"agent"}`)}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.OutboxSeq == 0 || res.IDs["$s"] == "" {
		t.Fatalf("agent result %+v", res)
	}
	d := a.expect("delta")
	for d["by"].(map[string]any)["kind"] != "agent" {
		d = a.expect("delta")
	}
	if err := b.core.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, r := range b.core.revs {
		kinds = append(kinds, r.info.Kind)
	}
	if strings.Join(kinds, ",") != "created,person,person,agent" {
		t.Fatalf("revision kinds %v", kinds)
	}
	if len(b.in.sd.groups) != 0 {
		t.Fatalf("groups after the agent write %v", b.in.sd.groups)
	}
	// The same op id replays; another request with it is a conflict.
	again, err := b.in.UpdateBoard(ctx, UpdateRequest{OpID: "agent-op-1", Label: "Agent", Message: "add",
		Ops: []engine.AgentOp{{Op: "add", Ref: "$s", El: json.RawMessage(`{"type":"sticky","text":"agent"}`)}}}, 0)
	if err != nil || !again.Replayed || again.IDs["$s"] != res.IDs["$s"] {
		t.Fatalf("replay %+v %v", again, err)
	}
	_, err = b.in.UpdateBoard(ctx, UpdateRequest{OpID: "agent-op-1", Label: "Agent", Ops: []engine.AgentOp{{Op: "delete", IDs: []string{id}}}}, 0)
	var be *Error
	if !errors.As(err, &be) || be.Category != CategoryConflict {
		t.Fatalf("reused op id: %v", err)
	}

	// Rollback to the person revision: one rollback revision equal to it,
	// with no new blobs.
	blobs := len(b.core.blobs)
	rev, err := b.core.Rollback(ctx, 2, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	r := b.core.revs[len(b.core.revs)-1]
	if rev != r.info.Number || r.info.Kind != "rollback" || r.info.RollbackOf != 2 {
		t.Fatalf("rollback revision %+v", r.info)
	}
	if !sameFiles(r.files, b.core.revs[1].files) || len(b.core.blobs) != blobs {
		t.Fatalf("rollback files differ or added blobs:\n%v\n%v", r.files, b.core.revs[1].files)
	}
	if got := b.alive(); len(got) != 1 || got[id] != "two" {
		t.Fatalf("after rollback %v", got)
	}
	reset := a.expect("delta")
	for reset["reset"] == nil {
		reset = a.expect("delta")
	}
	if reset["reset"].(map[string]any)["of"] != 2.0 {
		t.Fatalf("reset %v", reset["reset"])
	}
}

func sameFiles(a, b []File) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestImportCrashBeforeAcknowledge(t *testing.T) {
	b := newTB(t)
	ctx := context.Background()
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	a.batch(1, sticky(eid(), "one", 0))
	b.clock.Add(31 * time.Second)
	if err := b.in.do(ctx, b.in.tick); err != nil {
		t.Fatal(err)
	}
	// Store blobs and revisions and advance the cursor, then stop before
	// the acknowledgement.
	entries, err := b.in.Outbox(ctx, 0, 100)
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries %v %v", entries, err)
	}
	b.core.store = b.in.StoreID()
	for _, e := range entries {
		for _, h := range e.Snapshots {
			data, _ := b.in.SnapshotData(ctx, h)
			b.core.PutBlob(ctx, h, data)
		}
		b.core.revs = append(b.core.revs, fakeRev{info: RevisionInfo{Number: len(b.core.revs) + 1, Kind: e.Kind}, files: e.Files})
		b.core.cursor = e.Seq
	}
	b.restart()
	if err := b.core.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if len(b.core.revs) != 2 || len(b.in.sd.outbox) != 0 || len(b.in.sd.snaps) != 0 {
		t.Fatalf("revisions %d, outbox %v, snaps %v", len(b.core.revs), b.in.sd.outbox, b.in.sd.snaps)
	}
	// A rollback that committed before a stop is found by its op id.
	_, c, _ := b.core.RevisionContent(ctx, 1)
	if _, err := b.in.Rollback(ctx, "rollback-op-1", 1, c, "Alice"); err != nil {
		t.Fatal(err)
	}
	b.restart()
	if op, ok, err := b.in.Op(ctx, "rollback-op-1"); err != nil || !ok || op.Kind != "rollback" || op.OutboxSeq == 0 {
		t.Fatalf("op %+v %v %v", op, ok, err)
	}
}

// A Private connection starts the disk measurement, so the first batch
// finds a fresh value instead of a transient refusal and its resend (5.4).
func TestPrivateConnectionMeasuresBeforeTheFirstBatch(t *testing.T) {
	b := newTB(t)
	b.core.setQuota(1 << 30)
	if _, ok := b.in.meas.get(); ok {
		t.Fatal("measured before any connection")
	}
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, ok := b.in.meas.get(); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("opening the connection did not measure")
		}
	}
	if m := a.batch(1, sticky(eid(), "first", 0)); m["t"] != "ack" {
		t.Fatalf("first batch %v", m)
	}
	b.core.data.Lock()
	measured := b.core.measured
	b.core.data.Unlock()
	if measured != 1 {
		t.Fatalf("measured %d times", measured)
	}
	// A second connection while the value is fresh does not measure again.
	c := connect(t, b.priv, map[string]any{"editor": "bob"}, nil)
	c.snapshot()
	if m := c.batch(1, sticky(eid(), "second", 0)); m["t"] != "ack" {
		t.Fatalf("second editor's batch %v", m)
	}
	b.core.data.Lock()
	measured = b.core.measured
	b.core.data.Unlock()
	if measured != 1 {
		t.Fatalf("measured %d times after a second connection", measured)
	}
}

func TestIdleBoardEditAfterTwoMinutes(t *testing.T) {
	b := newTB(t)
	b.core.setQuota(1 << 30)
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	b.clock.Add(3 * time.Minute)
	id := eid()
	var m map[string]any
	for n := 0; n < 20; n++ {
		m = a.batch(1, sticky(id, "after idle", 0))
		if m["t"] == "ack" {
			break
		}
		if m["retry"] != true {
			t.Fatalf("idle edit refused permanently: %v", m)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if m["t"] != "ack" || b.alive()[id] != "after idle" {
		t.Fatalf("idle edit %v", m)
	}
}

func TestDeltaLogTrimThenEditsAccepted(t *testing.T) {
	b := newTB(t)
	b.in.deltaMaxBytes = 32 << 10
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	frame := eid()
	ops := []map[string]any{{"op": "add", "el": map[string]any{"id": frame, "type": "frame", "x": 0, "y": 0, "w": 5000, "h": 5000, "rot": 0, "text": "F"}}}
	for i := 0; i < 300; i++ {
		el := sticky(eid(), "member", float64(i*10))
		el["el"].(map[string]any)["frame"] = frame
		el["refs"] = map[string]any{frame: 0}
		ops = append(ops, el)
	}
	if m := a.batch(1, ops...); m["t"] != "ack" {
		t.Fatal(m)
	}
	first := a.expect("delta")
	n := 2
	for x := 1; x <= 6; x++ {
		m := a.batch(n, map[string]any{"op": "set", "id": frame, "inc": 0, "g": "geom", "carry": "members",
			"val": map[string]any{"x": x * 10, "y": 0, "w": 5000, "h": 5000, "rot": 0}})
		if m["t"] != "ack" {
			t.Fatal(m)
		}
		n++
	}
	if b.in.sd.deltaBytes > 32<<10 || b.in.sd.deltas[0].seq <= int64(first["seq"].(float64)) {
		t.Fatalf("log not trimmed: %d bytes from %d", b.in.sd.deltaBytes, b.in.sd.deltas[0].seq)
	}
	id := eid()
	if m := a.batch(n, sticky(id, "after trim", -500)); m["t"] != "ack" {
		t.Fatal(m)
	}
	// A reconnect from before the trimmed range gets a snapshot.
	r := connect(t, b.priv, map[string]any{"editor": "alice", "session": a.session, "watermark": first["seq"]}, nil)
	_, els, _ := r.snapshot()
	if len(els) != 302 {
		t.Fatalf("%d elements in the snapshot", len(els))
	}
}

func TestPersonBatchOverQuotaIsRefusedBeforeOthersSeeIt(t *testing.T) {
	b := newTB(t)
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	o := connect(t, b.priv, map[string]any{"editor": "olga"}, nil)
	o.snapshot()
	a.batch(1, sticky(eid(), "small", 0))
	o.expect("delta")
	usage := measureUsage(t, b)
	b.core.setQuota(usage + 20<<10)
	big := eid()
	m := a.settle(2, sticky(big, strings.Repeat("x", 4000), 300), sticky(eid(), strings.Repeat("y", 4000), 600),
		sticky(eid(), strings.Repeat("z", 4000), 900), sticky(eid(), strings.Repeat("w", 4000), 1200),
		sticky(eid(), strings.Repeat("v", 4000), 1500), sticky(eid(), strings.Repeat("u", 4000), 1800))
	if m["t"] != "reject" || m["code"] != "disk_quota" || m["retry"] != false {
		t.Fatalf("over-quota batch %v", m)
	}
	if !o.quiet(200*time.Millisecond, isDelta) {
		t.Fatal("the other editor saw a refused batch")
	}
	if _, ok := b.alive()[big]; ok {
		t.Fatal("the refused batch applied")
	}
	// The number is consumed: a resend replays the refusal.
	if m := a.batch(2, sticky(big, "x", 0)); m["code"] != "disk_quota" {
		t.Fatal(m)
	}
	if m := a.settle(3, map[string]any{"op": "set", "id": big, "inc": 0, "g": "text", "val": "x"}); m["t"] != "ack" {
		t.Fatalf("a small edit under the quota: %v", m)
	}
}

// measureUsage computes the quota formula's usage of the board now.
func measureUsage(t *testing.T, b *tb) int64 {
	t.Helper()
	other, err := b.core.MeasureOther(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var u int64
	err = b.in.do(context.Background(), func() error {
		tx, err := b.in.begin()
		if err != nil {
			return err
		}
		defer tx.rollback()
		u, err = tx.usage(other)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestReplacementOverQuotaChangesNothing(t *testing.T) {
	b := newTB(t)
	ctx := context.Background()
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	keep := eid()
	a.batch(1, sticky(keep, "keep", 0))
	a.expect("delta")
	var els []model.Element
	for i := 0; i < 40; i++ {
		els = append(els, model.Element{ID: eid(), Type: model.TypeSticky, Geom: model.Geom{X: float64(i * 210), W: 200, H: 200},
			Z: "V" + string(rune('a'+i%20)), Text: noise(4000, i), Style: model.DefaultStyle(model.TypeSticky)})
	}
	zipData := buildArchive(t, boardFile{Format: 1, Title: "Replaced", Grid: true}, els, nil, nil)
	usage := measureUsage(t, b)
	b.core.setQuota(usage + 64<<10)
	seq := b.in.st.Seq()
	outbox := len(b.in.sd.outbox)
	err := b.core.Exclusive(ctx, func(ctx context.Context, other int64) error {
		_, err := b.in.ImportBoard(ctx, ImportRequest{OpID: "replace-op-1", Mode: "replace", Archive: strings.NewReader(string(zipData)),
			Size: int64(len(zipData)), Label: "Agent"}, other)
		return err
	})
	var be *Error
	if !errors.As(err, &be) || be.Category != CategoryCapacity {
		t.Fatalf("replacement over quota: %v", err)
	}
	if b.in.st.Seq() != seq || len(b.alive()) != 1 || len(b.in.sd.outbox) != outbox || b.in.st.Board().Text != "Test" {
		t.Fatal("a refused replacement changed the board")
	}
	if !a.quiet(200*time.Millisecond, isDelta) {
		t.Fatal("a refused replacement sent a delta")
	}
	// With room, it replaces the board and clients get a reset delta.
	b.core.setQuota(0)
	err = b.core.Exclusive(ctx, func(ctx context.Context, other int64) error {
		_, err := b.in.ImportBoard(ctx, ImportRequest{OpID: "replace-op-2", Mode: "replace", Archive: strings.NewReader(string(zipData)),
			Size: int64(len(zipData)), Label: "Agent"}, other)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.alive(); len(got) != 40 {
		t.Fatalf("after replace %d elements", len(got))
	}
	d := a.expect("delta")
	if d["reset"].(map[string]any)["kind"] != "replace" {
		t.Fatalf("replace delta %v", d["reset"])
	}
}

func TestRollbackIntoOverQuotaThenEditsRefused(t *testing.T) {
	b := newTB(t)
	ctx := context.Background()
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	var ops []map[string]any
	var ids []string
	for i := 0; i < 20; i++ {
		id := eid()
		ids = append(ids, id)
		ops = append(ops, sticky(id, strings.Repeat("big", 1300), float64(i*210)))
	}
	a.batch(1, ops...)
	b.clock.Add(31 * time.Second)
	if err := b.in.do(ctx, b.in.tick); err != nil {
		t.Fatal(err)
	}
	if err := b.core.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	bigRev := b.core.lastRev()
	del := map[string]any{"op": "delete", "ids": ids, "inc_of": map[string]any{}}
	for _, id := range ids {
		del["inc_of"].(map[string]any)[id] = 0
	}
	a.batch(2, del)
	b.clock.Add(31 * time.Second)
	if err := b.in.do(ctx, b.in.tick); err != nil {
		t.Fatal(err)
	}
	if err := b.core.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	b.core.setQuota(measureUsage(t, b) + 4<<10)
	if _, err := b.core.Rollback(ctx, bigRev, "Alice"); err != nil {
		t.Fatalf("a rollback was refused: %v", err)
	}
	if len(b.alive()) != 20 {
		t.Fatal("rollback did not restore")
	}
	snap := a.expect("delta")
	for snap["reset"] == nil {
		snap = a.expect("delta")
	}
	m := a.settle(3, map[string]any{"op": "delete", "ids": ids[:1], "inc_of": map[string]any{ids[0]: snap["changes"].([]any)[0].(map[string]any)["inc"]}})
	if m["code"] != "disk_quota" {
		t.Fatalf("deletion over quota %v", m)
	}
	b.core.setQuota(1 << 30)
	if m := a.settle(4, map[string]any{"op": "set", "id": ids[1], "inc": snap["changes"].([]any)[1].(map[string]any)["inc"], "g": "text", "val": "ok"}); m["t"] != "ack" {
		t.Fatalf("edit after raising the quota %v", m)
	}
}

func TestPublicPoolFullStillAdmitsAnEditor(t *testing.T) {
	b := newTB(t)
	b.in.publicMax = 10
	var viewers []*client
	for i := 0; i < 10; i++ {
		v := connect(t, b.pub, nil, nil)
		v.snapshot()
		viewers = append(viewers, v)
	}
	if _, err := dial(t, b.pub, origin(b.pub)); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("an 11th viewer: %v", err)
	}
	e := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	e.snapshot()
	if m := e.batch(1, sticky(eid(), "editor still edits", 0)); m["t"] != "ack" {
		t.Fatal(m)
	}
	viewers[9].expect("delta")
}

func TestCrashPointsOfReplacement(t *testing.T) {
	b := newTB(t)
	ctx := context.Background()
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	old := eid()
	a.batch(1, sticky(old, "old", 0))
	el := model.Element{ID: eid(), Type: model.TypeSticky, Geom: model.Geom{W: 200, H: 200}, Z: "V", Text: "new", Style: model.DefaultStyle(model.TypeSticky)}
	data := buildArchive(t, boardFile{Format: 1, Title: "New", Grid: true}, []model.Element{el}, nil, nil)
	boardDir := filepath.Join(b.dir, "board")
	copies := map[string]string{}
	b.in.hook = func(stage string) {
		if copies[stage] == "" {
			copies[stage] = crashCopy(t, boardDir)
		}
	}
	err := b.core.Exclusive(ctx, func(ctx context.Context, other int64) error {
		_, err := b.in.ImportBoard(ctx, ImportRequest{OpID: "replace-crash", Mode: "replace", Archive: strings.NewReader(string(data)),
			Size: int64(len(data)), Label: "Agent"}, other)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	before := openCopy(t, copies["commit"])
	if got := aliveOf(t, before); len(got) != 1 || got[old] != "old" {
		t.Fatalf("killed before COMMIT: %v", got)
	}
	if _, ok, _ := before.Op(ctx, "replace-crash"); ok {
		t.Fatal("the op id survived an uncommitted replacement")
	}
	after := openCopy(t, copies["committed"])
	if got := aliveOf(t, after); len(got) != 1 || got[el.ID] != "new" {
		t.Fatalf("killed after COMMIT: %v", got)
	}
	if op, ok, _ := after.Op(ctx, "replace-crash"); !ok || op.OutboxSeq == 0 {
		t.Fatalf("op after the crash %+v %v", op, ok)
	}
}
