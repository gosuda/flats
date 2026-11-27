package sheet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// measured returns the usage formula and the pages it reserves for the
// next recording, on the state as it is.
func (e *env) measured() (total, future int64) {
	e.t.Helper()
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	err := e.in.transaction(context.Background(), func(tx *sql.Tx) error {
		var err error
		total, future, err = e.in.quotaUsage(context.Background(), tx, e.in.wb, 0, 0)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return total, future
}

// wantReserved fails when the reserve of a dirty row file is smaller than
// the bytes its next encoding takes (technical design, section 16.5).
// It returns the bytes reserved and the bytes of the encoding.
func (e *env) wantReserved(when string) (reserved, encoded int64) {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	wb := e.in.wb
	plan, err := wb.planFiles()
	if err != nil {
		e.t.Fatal(err)
	}
	for key, reserve := range wb.files.chunks {
		reserved += reserve
		if size := plan.encoded[key]; reserve < size {
			e.t.Errorf("%s: row file %d/%d has a reserve of %d bytes and encodes to %d", when, key.tab, key.first, reserve, size)
		}
	}
	for _, size := range plan.encoded {
		encoded += size
	}
	if reserved < encoded {
		e.t.Errorf("%s: the dirty row files reserve %d bytes and encode to %d", when, reserved, encoded)
	}
	return reserved, encoded
}

// TestQuotaProbeKeepsReserve writes a value back unchanged into a large
// row file next to the quota, where the writer measures by recording the
// boundary in a savepoint. The file stays reserved in full, a changed
// write after it is reserved on top, the probe leaves nothing in sheet.db,
// and the group closes within the quota after a restart.
func TestQuotaProbeKeepsReserve(t *testing.T) {
	e := start(t)
	text := strings.Repeat("r", 20<<10)
	rows := make([]string, 40)
	for i := range rows {
		rows[i] = fmt.Sprintf(`[%q]`, text)
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + strings.Join(rows, ",") + `]}]}`)
	revisions := e.b.importAll(t)
	alice := e.person("Alice")
	// The first write back makes the row file dirty far from any quota.
	if ack := alice.set("A1", text); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	e.wantReserved("after a write back without a quota")
	total, future := e.measured()
	quota := total + future*e.in.pageSize/2
	e.b.setQuota(quota)
	if err := e.in.Remeasure(context.Background()); err != nil {
		t.Fatal(err)
	}
	count := func(table string) (n int) {
		t.Helper()
		if err := e.in.rdb.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	outbox, snapshots := count("outbox"), count("snapshots")
	// Within one recording reservation of the quota the writer probes.
	if ack := alice.set("A2", text); status(ack) != "applied" {
		t.Fatalf("a write back next to the quota: %s", status(ack))
	}
	// The probe leaves the exact size as the reserve; without it the
	// reserve would be the recorded size and what the two writes added.
	if reserved, encoded := e.wantReserved("after the probed write back"); reserved != encoded || encoded < int64(40*len(text)) {
		t.Fatalf("the writer did not probe: %d bytes reserved for an encoding of %d", reserved, encoded)
	}
	e.wantStored("after the probed write back")
	if count("outbox") != outbox || count("snapshots") != snapshots {
		t.Fatalf("the probe left %d outbox entries and %d snapshots, were %d and %d", count("outbox"), count("snapshots"), outbox, snapshots)
	}
	// A small changed write into the same file is reserved on top.
	if ack := alice.set("B1", "changed"); status(ack) != "applied" {
		t.Fatalf("a small edit after the probe: %s", status(ack))
	}
	e.wantReserved("after a changed write")
	e.wantStored("after a changed write")
	e.restart()
	e.wantReserved("after the restart")
	// The acknowledged group closes within the quota.
	e.clock.Add(groupIdle + time.Second)
	e.tick()
	if got := e.b.importAll(t); got != revisions+1 || e.b.entry(got).Kind != "person" {
		t.Fatalf("%d revisions after the group closed, were %d", got, revisions)
	}
	var onDisk int64
	for _, name := range []string{dbName, dbName + "-wal"} {
		if info, err := os.Stat(filepath.Join(e.dir, name)); err == nil {
			onDisk += info.Size()
		}
	}
	e.b.mu.Lock()
	for _, data := range e.b.blobs {
		onDisk += int64(len(data))
	}
	e.b.mu.Unlock()
	if onDisk > quota {
		t.Fatalf("%d bytes on disk pass the quota %d after the group closed", onDisk, quota)
	}
	e.want("B1", "changed")
}

// TestAcknowledgeCountsStoredBytesOnce fails the commit of an import
// acknowledgement. The bytes the import stored are added to the usage of
// the flat's directory once, when the acknowledgement that is sent again
// commits, and not for the attempt that failed or for a later repeat.
func TestAcknowledgeCountsStoredBytesOnce(t *testing.T) {
	e := start(t)
	e.person("Alice").set("A1", "1")
	e.clock.Add(groupIdle + time.Second)
	e.tick()
	ctx := context.Background()
	entries, err := e.in.Outbox(ctx, 0, 0)
	if err != nil || len(entries) == 0 {
		t.Fatalf("the outbox holds %d entries: %v", len(entries), err)
	}
	through := entries[len(entries)-1].Seq
	if err := e.in.measure.remeasure(ctx); err != nil {
		t.Fatal(err)
	}
	before, ok := e.in.measure.get()
	if !ok {
		t.Fatal("no measurement")
	}
	usage := func() int64 {
		t.Helper()
		n, ok := e.in.measure.get()
		if !ok {
			t.Fatal("no measurement")
		}
		return n - before
	}
	e.in.mu.Lock()
	e.in.fault = func() error { return errors.New("injected commit failure") }
	e.in.mu.Unlock()
	if err := e.in.Acknowledge(ctx, through, 1000); err == nil {
		t.Fatal("the acknowledgement committed")
	}
	if got := usage(); got != 0 {
		t.Fatalf("an acknowledgement that did not commit added %d bytes", got)
	}
	e.in.mu.Lock()
	e.in.fault = nil
	e.in.mu.Unlock()
	for i := 0; i < 2; i++ {
		if err := e.in.Acknowledge(ctx, through, 1000); err != nil {
			t.Fatal(err)
		}
		if got := usage(); got != 1000 {
			t.Fatalf("after acknowledgement %d the usage grew by %d bytes", i+1, got)
		}
	}
}

// slowMeasurement starts a measurement of the flat's directory, as the
// background refresh does, and holds it after it has read the usage it will
// report. It returns when the measurement is held; finish lets it end and
// waits for it. Other measurements are not held.
func slowMeasurement(t *testing.T, e *env) (finish func()) {
	t.Helper()
	// The measurement an instance starts with has read the usage, so it
	// is not the one that is held.
	for deadline := time.Now().Add(10 * time.Second); ; runtime.Gosched() {
		e.b.mu.Lock()
		measured := e.b.measured
		e.b.mu.Unlock()
		if measured > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the instance did not measure its directory")
		}
	}
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var taken atomic.Bool
	e.b.mu.Lock()
	e.b.walking = func() {
		if taken.CompareAndSwap(false, true) {
			close(held)
			<-release
		}
	}
	e.b.mu.Unlock()
	go func() { done <- e.in.measure.remeasure(context.Background()) }()
	<-held
	return func() {
		t.Helper()
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// TestMeasurementKeepsNewerAccounting overlaps a slow measurement of the
// flat's directory with an import. The measurement read the directory
// before the import stored its blob, and ends after it. The blob's bytes
// stay counted, so an edit that would pass the quota with them is refused
// (spec rule 57).
func TestMeasurementKeepsNewerAccounting(t *testing.T) {
	const blob = 1 << 30
	setup := func(t *testing.T) (*env, *person, int64) {
		e := start(t)
		alice := e.person("Alice")
		alice.set("A1", "1")
		e.clock.Add(groupIdle + time.Second)
		e.tick()
		entries, err := e.in.Outbox(context.Background(), 0, 0)
		if err != nil || len(entries) == 0 {
			t.Fatalf("the outbox holds %d entries: %v", len(entries), err)
		}
		// Room for the sheet as it is, and not for the blob beside it.
		e.b.setQuota(blob / 2)
		if err := e.in.Remeasure(context.Background()); err != nil {
			t.Fatal(err)
		}
		if ack := alice.set("A2", "2"); status(ack) != "applied" {
			t.Fatalf("an edit before the import: %s", status(ack))
		}
		return e, alice, entries[len(entries)-1].Seq
	}
	refused := func(t *testing.T, e *env, alice *person) {
		t.Helper()
		seq := e.seq()
		if ack := alice.set("A3", "3"); status(ack) != "refused:disk_quota" || e.seq() != seq {
			t.Fatalf("an edit beside the imported blob: %s", status(ack))
		}
	}
	t.Run("an acknowledgement during the measurement", func(t *testing.T) {
		e, alice, through := setup(t)
		finish := slowMeasurement(t, e)
		// The import stores the blob, commits and acknowledges.
		e.b.setOther(blob)
		if err := e.in.Acknowledge(context.Background(), through, blob); err != nil {
			t.Fatal(err)
		}
		refused(t, e, alice)
		finish()
		refused(t, e, alice)
		// A measurement that begins after the import counts the blob once.
		if err := e.in.Remeasure(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got, ok := e.in.measure.get(); !ok || got != blob {
			t.Fatalf("the usage beside the sheet is %d, want %d", got, int64(blob))
		}
	})
	t.Run("the measurement after a failed import", func(t *testing.T) {
		e, alice, _ := setup(t)
		finish := slowMeasurement(t, e)
		// The import stores the blob and fails before it acknowledges, and
		// the host has the sheet measure again.
		e.b.setOther(blob)
		if err := e.in.Remeasure(context.Background()); err != nil {
			t.Fatal(err)
		}
		refused(t, e, alice)
		finish()
		refused(t, e, alice)
		if got, ok := e.in.measure.get(); !ok || got != blob {
			t.Fatalf("the usage beside the sheet is %d, want %d", got, int64(blob))
		}
	})
	t.Run("a failed measurement after a slow one began", func(t *testing.T) {
		e, alice, _ := setup(t)
		finish := slowMeasurement(t, e)
		e.b.setOther(-1)
		if err := e.in.Remeasure(context.Background()); err == nil {
			t.Fatal("a measurement of a negative usage succeeded")
		}
		finish()
		// The older result does not stand in for the one that failed.
		if _, ok := e.in.measure.get(); ok {
			t.Fatal("an older measurement replaced a newer one that failed")
		}
		if _, err := alice.try(e.seq(), e.setOp("A3", "3")); refusal(err).Reason != "measurement" {
			t.Fatalf("an edit without a measurement: %v", err)
		}
	})
}

// TestBackgroundMeasurementIsBounded lets the walk of the flat's directory
// hang. The background measurement ends at its time limit as a failed one,
// so the instance has no measurement: an edit and a new session are refused
// as unavailable, to be sent again, and nothing is taken as zero usage.
// When the directory answers again the next measurement counts.
func TestBackgroundMeasurementIsBounded(t *testing.T) {
	limit := measureLimit
	measureLimit = 50 * time.Millisecond
	t.Cleanup(func() { measureLimit = limit })
	e := start(t)
	ctx := context.Background()
	e.b.setQuota(1 << 40)
	if err := e.in.Remeasure(ctx); err != nil {
		t.Fatal(err)
	}
	alice := e.person("Alice")
	if ack := alice.set("A1", "1"); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	hang := func(stuck bool) {
		e.b.mu.Lock()
		e.b.stuck = stuck
		e.b.mu.Unlock()
		e.in.measure.refresh()
		for deadline := time.Now().Add(10 * time.Second); ; runtime.Gosched() {
			if _, valid := e.in.measure.get(); valid != stuck {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the measurement is valid = %v while the walk hangs = %v", stuck, stuck)
			}
			e.in.measure.refresh()
		}
	}
	hang(true)
	base, op := e.seq(), e.setOp("A2", "2")
	_, err := alice.try(base, op)
	if werr := refusal(err); err == nil || werr.Code != "unavailable" || werr.Reason != "measurement" || !werr.Retry || e.seq() != base {
		t.Fatalf("an edit without a measurement: %v", err)
	}
	_, _, err = e.in.newSession(ctx, "editor", "Bob", false)
	if werr := refusal(err); err == nil || werr.Code != "unavailable" || werr.Reason != "measurement" || !werr.Retry {
		t.Fatalf("a new session without a measurement: %v", err)
	}
	hang(false)
	if ack, err := alice.send(alice.n, base, op); err != nil || status(ack) != "applied" {
		t.Fatalf("the edit sent again: %v %s", err, status(ack))
	}
}

// TestSessionsAtTheQuota opens a sheet whose flat is at its disk quota, and
// then over it (spec rule 57: every edit is refused, reading goes on).
//
//	the flat             a new Private connection                sheet.db
//	at the quota         gets a session, reads; edit refused     one sessions row
//	over the quota       gets a session, reads; edit refused     no row, no page
//	  the same, again    resumes while the instance runs         no row, no page
//	  quota raised       its next edit applies                   the row, with it
//	  instance restarted its token has expired                   -
//
// A person who had a session resumes it over the quota, and a Public viewer
// reads. Twenty new connections over the quota add nothing to sheet.db.
func TestSessionsAtTheQuota(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["kept",2,"=B1*2"]]}]}`)
	e.b.importAll(t)
	s := serve(e)
	tab := e.tabID("Sheet1")
	reads := func(c *client) {
		t.Helper()
		if _, cells := c.watch(1, tab); len(cells) < 3 {
			t.Fatalf("the snapshot holds %d cells", len(cells))
		}
	}
	count := func(query string, args ...any) (n int64) {
		t.Helper()
		if err := e.in.rdb.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	_, row, col := e.ids("D1")
	edit := func(c *client, n int, text string) string {
		t.Helper()
		c.send(fmt.Sprintf(`{"type":"action","n":"%d","base":"%d","ops":[{"op":"set","tab":%d,"row":%d,"col":%d,"text":%q,"at":[0,3]}]}`,
			n, e.seq(), tab, row, col, text))
		return fmt.Sprint(c.next("ack")["results"])
	}
	const refused, applied = "[map[reason:disk_quota status:refused]]", "[map[applied:1 status:applied]]"
	earlier := s.dial("private", `{"type":"hello","version":1,"name":"Alice","pending":0}`)
	token := earlier.next("welcome")["session"].(string)
	earlier.ws.Close(1000, "")

	// At the quota: the sheet as it is fits, and nothing more.
	total, _ := e.measured()
	e.b.setQuota(total)
	if err := e.in.Remeasure(context.Background()); err != nil {
		t.Fatal(err)
	}
	sessions := count(`SELECT count(*) FROM sessions`)
	bob := s.dial("private", `{"type":"hello","version":1,"name":"Bob","pending":0}`)
	if w := bob.next("welcome"); w["session"] == nil {
		t.Fatalf("at the quota a person gets no session: %v", w)
	}
	reads(bob)
	if got := edit(bob, 1, strings.Repeat("q", 30<<10)); got != refused {
		t.Fatalf("an edit at the quota: %s", got)
	}
	bob.ws.Close(1000, "")
	if got := count(`SELECT count(*) FROM sessions`); got != sessions+1 {
		t.Fatalf("at the quota %d sessions were stored for one connection", got-sessions)
	}

	// Over the quota.
	e.b.setQuota(1)
	sessions, receipts, pages := count(`SELECT count(*) FROM sessions`), count(`SELECT count(*) FROM receipts`), count(`PRAGMA page_count`)
	unchanged := func(when string) {
		t.Helper()
		if got := count(`SELECT count(*) FROM sessions`); got != sessions {
			t.Fatalf("%s: %d sessions were stored over the quota", when, got-sessions)
		}
		if got := count(`SELECT count(*) FROM receipts`); got != receipts {
			t.Fatalf("%s: %d receipts were stored over the quota", when, got-receipts)
		}
		if got := count(`PRAGMA page_count`); got != pages {
			t.Fatalf("%s: sheet.db grew by %d pages over the quota", when, got-pages)
		}
	}
	for i := 0; i < 20; i++ {
		c := s.dial("private", fmt.Sprintf(`{"type":"hello","version":1,"name":"New %d","pending":0}`, i))
		if w := c.next("welcome"); w["session"] == nil || w["access"] != "private" {
			t.Fatalf("a new Private connection over the quota: %v", w)
		}
		reads(c)
		if got := edit(c, 1, "x"); got != refused {
			t.Fatalf("an edit over the quota: %s", got)
		}
		c.ws.Close(1000, "")
	}
	unchanged("twenty new connections")
	carol := s.dial("private", `{"type":"hello","version":1,"name":"Carol","pending":0}`)
	welcome := carol.next("welcome")
	reads(carol)
	for n := 1; n <= 3; n++ {
		if got := edit(carol, n, fmt.Sprint("carol ", n)); got != refused {
			t.Fatalf("edit %d over the quota: %s", n, got)
		}
	}
	// The same action sent again gets the answer it got, also on a new
	// connection of the session.
	if got := edit(carol, 3, "carol 3"); got != refused {
		t.Fatalf("a refused edit sent again: %s", got)
	}
	carol.ws.Close(1000, "")
	carol = s.dial("private", fmt.Sprintf(`{"type":"hello","version":1,"session":%q,"pending":0}`, welcome["session"]))
	if w := carol.next("welcome"); w["session_id"] != welcome["session_id"] {
		t.Fatalf("a session without a row did not resume: %v", w)
	}
	reads(carol)
	// It counts as a connection like any other: the instance is in use.
	if e.in.StopIfIdle(0) {
		t.Fatal("the instance stopped under an open connection")
	}
	alice := s.dial("private", fmt.Sprintf(`{"type":"hello","version":1,"session":%q,"pending":0}`, token))
	if w := alice.next("welcome"); w["session_id"] == nil || w["access"] != "private" {
		t.Fatalf("a person with a session over the quota: %v", w)
	}
	reads(alice)
	viewer := s.dial("public", helloV1)
	if w := viewer.next("welcome"); w["access"] != "public" {
		t.Fatalf("a Public viewer over the quota: %v", w)
	}
	reads(viewer)
	unchanged("in the end")

	// The quota is raised: the same connection edits, and the session has
	// its row from that action on.
	e.b.setQuota(1 << 40)
	if got := edit(carol, 4, "fits"); got != applied {
		t.Fatalf("an edit after the quota was raised: %s", got)
	}
	e.want("D1", "fits")
	id := welcome["session_id"]
	if rows, max := count(`SELECT count(*) FROM sessions WHERE id=?`, id), count(`SELECT coalesce(max(max_n),0) FROM sessions WHERE id=?`, id); rows != 1 || max != 4 {
		t.Fatalf("after its first applied action the session has %d rows with number %d", rows, max)
	}
	// The three refusals it kept in memory are stored with the row.
	if got := count(`SELECT count(*) FROM receipts WHERE session=?`, id); got != 4 {
		t.Fatalf("the session has %d stored receipts", got)
	}
	if got := edit(carol, 5, "again"); got != applied {
		t.Fatalf("the next edit: %s", got)
	}

	// A session that never got its row does not outlive the instance.
	e.b.setQuota(1)
	dave := s.dial("private", `{"type":"hello","version":1,"name":"Dave","pending":0}`)
	lost := dave.next("welcome")["session"].(string)
	dave.ws.Close(1000, "")
	e.restart()
	s = serve(e)
	dave = s.dial("private", fmt.Sprintf(`{"type":"hello","version":1,"session":%q,"pending":0}`, lost))
	if f := dave.next("fatal"); f["code"] != "session_expired" {
		t.Fatalf("a session without a row after a restart: %v", f)
	}
	carol = s.dial("private", fmt.Sprintf(`{"type":"hello","version":1,"session":%q,"pending":0}`, welcome["session"]))
	if w := carol.next("welcome"); w["session_id"] != id {
		t.Fatalf("a session with its row after a restart: %v", w)
	}
}

// A resume stores the time of its session only when the disk quota admits
// the write, as it admits the receipt of an action. While a reader holds
// the log, as the snapshot of a backup does, no checkpoint takes frames
// out of it, so every stored resume makes the -wal file longer. Resumes
// with advancing times are stored while the quota has room, which the
// row's time and the file's growth show. From the first one the quota
// refuses, the file keeps its size through fifty more, the row keeps its
// time, sheet.db and the log stay within the quota on disk, and each
// resume still returns the session. Without a measurement of the
// directory a resume stores nothing either. With room again a resume is
// stored, and after a restart the token resumes the session with that
// time.
func TestResumesStoreTheirTimeWithinTheQuota(t *testing.T) {
	ctx := context.Background()
	e := start(t, unhurried)
	// The log is longer than the least the usage formula counts for it.
	row := `["` + strings.Repeat("a", 8000) + `"],`
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + strings.TrimSuffix(strings.Repeat(row, 60), ",") + `]}]}`)
	e.b.importAll(t)
	held, token, err := e.in.newSession(ctx, "editor:ann", "ann", false)
	if err != nil {
		t.Fatal(err)
	}
	e.in.release(held)
	db, wal := filepath.Join(e.dir, dbName), filepath.Join(e.dir, dbName+"-wal")
	size := func(path string) int64 {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	storedAt := func() (at int64) {
		t.Helper()
		if err := e.in.rdb.QueryRow(`SELECT last_at FROM sessions WHERE token_hash=?`, tokenHash(token)).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	// The measurement of the directory is taken again at each new time,
	// so that a resume stores nothing for the quota's reason only.
	measuring := true
	resume := func(when string) {
		t.Helper()
		e.clock.Add(time.Second)
		if measuring {
			if err := e.in.Remeasure(ctx); err != nil {
				t.Fatal(err)
			}
		}
		s, err := e.in.resumeSession(ctx, token)
		if err != nil {
			t.Fatalf("%s: the resume: %v", when, err)
		}
		if s.editor != "editor:ann" || s.rowless {
			t.Fatalf("%s: the resume returned the session of %q, without a row: %v", when, s.editor, s.rowless)
		}
		e.in.release(s)
	}
	if frame := e.in.pageSize + 24; size(wal) <= 32+64*frame {
		t.Fatalf("the log of %d bytes is not longer than the least the formula counts", size(wal))
	}

	reader := rawDB(t, db)
	defer reader.Close()
	tx, err := reader.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM meta`).Scan(&n); err != nil {
		t.Fatal(err)
	}

	// Room for the sheet as it is and a few stored resumes.
	total, _ := e.measured()
	quota := total + 128<<10
	e.b.setQuota(quota)
	if err := e.in.Remeasure(ctx); err != nil {
		t.Fatal(err)
	}
	stored := 0
	for {
		at, logged := storedAt(), size(wal)
		resume("with room")
		if storedAt() == at {
			if size(wal) != logged {
				t.Fatalf("a resume that stored no time left the log at %d bytes, from %d", size(wal), logged)
			}
			break
		}
		if size(wal) <= logged {
			t.Fatalf("a stored resume left the log at %d bytes, from %d", size(wal), logged)
		}
		if stored++; stored > 1000 {
			t.Fatalf("the quota of %d bytes still admits a resume after %d, with a log of %d bytes", quota, stored, size(wal))
		}
	}
	if stored == 0 {
		t.Fatal("no resume was stored while the quota had room")
	}
	at, logged := storedAt(), size(wal)
	for i := 0; i < 50; i++ {
		resume("over the quota")
	}
	if got := size(wal); got != logged || storedAt() != at {
		t.Fatalf("fifty resumes over the quota left the log at %d bytes, from %d, and the stored time moved: %v", got, logged, storedAt() != at)
	}
	if used := size(db) + size(wal); used > quota {
		t.Fatalf("sheet.db and its log hold %d bytes, over the quota of %d", used, quota)
	}

	// Without a measurement nothing says whether the write fits.
	e.b.setQuota(1 << 40)
	e.b.setOther(-1)
	if err := e.in.Remeasure(ctx); err == nil {
		t.Fatal("a measurement of a negative usage succeeded")
	}
	measuring = false
	resume("without a measurement")
	if got := size(wal); got != logged || storedAt() != at {
		t.Fatalf("a resume without a measurement left the log at %d bytes, from %d, and the stored time moved: %v", got, logged, storedAt() != at)
	}

	// Room again.
	e.b.setOther(0)
	measuring = true
	resume("with room again")
	last := storedAt()
	if last != e.clock.Now().Unix() || size(wal) <= logged {
		t.Fatalf("with room again the stored time is %d at %d, and the log holds %d bytes, from %d", last, e.clock.Now().Unix(), size(wal), logged)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	e.restart()
	if got := storedAt(); got != last {
		t.Fatalf("after the restart the stored time is %d, want %d", got, last)
	}
	resume("after the restart")
	if got := storedAt(); got != e.clock.Now().Unix() {
		t.Fatalf("the resume after the restart stored the time %d at %d", got, e.clock.Now().Unix())
	}
}
