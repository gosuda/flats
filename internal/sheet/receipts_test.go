package sheet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// overQuota starts a sheet with one written cell and a quota it is over.
func overQuota(t *testing.T) *env {
	t.Helper()
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["kept"]]}]}`)
	e.b.importAll(t)
	e.b.setQuota(1)
	if err := e.in.Remeasure(context.Background()); err != nil {
		t.Fatal(err)
	}
	return e
}

// sent is an action as a person sent it, and the hash of the outcome it
// got. A test that kept the outcome would keep alive what it measures.
type sent struct {
	n    uint64
	base uint64
	ops  string
	ack  [sha256.Size]byte
}

// submitRaw sends an action as the connection does and returns the stored
// outcome as it is.
func submitRaw(e *env, s *session, n, base uint64, ops string) (json.RawMessage, error) {
	e.t.Helper()
	var decoded []personOp
	if err := strictJSON([]byte(ops), &decoded); err != nil {
		e.t.Fatalf("ops JSON %s: %v", ops, err)
	}
	sum := sha256.Sum256([]byte(strconv.FormatUint(base, 10) + "\n" + ops))
	return e.in.submit(context.Background(), s, n, base, decoded, sum[:])
}

// kept returns how many receipts a session keeps in memory and the bytes
// their hashes and outcomes keep alive, counted from the receipts.
func (e *env) kept(s *session) (count int, bytes int64) {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	for _, k := range s.recent {
		bytes += int64(cap(k.hash) + cap(k.outcome))
	}
	return len(s.recent), bytes
}

// refusals builds an action of maxActionOps operations on one cell. Refused
// at the quota it gets the largest outcome a session without a row keeps:
// one refusal per operation, about 43 KB.
func (e *env) refusals() string {
	var ops strings.Builder
	ops.WriteByte('[')
	for i := range maxActionOps {
		if i > 0 {
			ops.WriteByte(',')
		}
		ops.WriteString(e.setOp("B2", fmt.Sprint("v", i)))
	}
	ops.WriteByte(']')
	return ops.String()
}

// TestRowlessReceiptsKeepTheNewest sends a session without a row actions
// whose numbers grow by more than one, small and very large, each refused
// at the quota. Memory keeps the newest 64 receipts whatever the numbers
// between them, each answers a resend as it did and a changed request as
// another action under that number, and an older one is expired. When the
// quota is raised, the newest 64 move to sheet.db with the row and answer
// the same from there.
func TestRowlessReceiptsKeepTheNewest(t *testing.T) {
	e := overQuota(t)
	p := e.person("Bob")
	if !p.s.rowless {
		t.Fatal("the session has a row over the quota")
	}
	// Each refusal receipt holds one result per operation. Twenty keep 64
	// of them far within the bytes a session keeps, so the count binds.
	var ops strings.Builder
	ops.WriteByte('[')
	for i := range 20 {
		if i > 0 {
			ops.WriteByte(',')
		}
		ops.WriteString(e.setOp("B2", fmt.Sprint("v", i)))
	}
	ops.WriteByte(']')
	numbers := []uint64{}
	for k := uint64(1); k <= 150; k++ {
		numbers = append(numbers, 65*k)
	}
	for k := uint64(1); k <= 150; k++ {
		numbers = append(numbers, 1<<62+k<<40)
	}
	var history []sent
	for _, n := range numbers {
		base := e.seq()
		ack, err := submitRaw(e, p.s, n, base, ops.String())
		if err != nil {
			t.Fatalf("action %d: %v", n, err)
		}
		if !bytes.Contains(ack, []byte(`"reason":"disk_quota"`)) {
			t.Fatalf("action %d over the quota: %.200s", n, ack)
		}
		history = append(history, sent{n, base, ops.String(), sha256.Sum256(ack)})
		if count, _ := e.kept(p.s); count > receiptCache {
			t.Fatalf("after action %d memory holds %d receipts", n, count)
		}
	}
	if count, _ := e.kept(p.s); count != receiptCache {
		t.Fatalf("memory holds %d receipts", count)
	}

	answers := func(where string) {
		t.Helper()
		newest := history[len(history)-receiptCache:]
		for _, a := range newest {
			got, err := submitRaw(e, p.s, a.n, a.base, a.ops)
			if err != nil || sha256.Sum256(got) != a.ack {
				t.Fatalf("%s: action %d sent again: %.100s, %v", where, a.n, got, err)
			}
			_, err = submitRaw(e, p.s, a.n, a.base, `[`+e.setOp("B2", "other")+`]`)
			if refusal(err).Reason != "action_number" {
				t.Fatalf("%s: another action under number %d: %v", where, a.n, err)
			}
		}
		for _, a := range []sent{history[0], history[len(history)-receiptCache-1]} {
			if _, err := submitRaw(e, p.s, a.n, a.base, a.ops); refusal(err).Reason != "receipt_expired" {
				t.Fatalf("%s: action %d sent again after 64 newer: %v", where, a.n, err)
			}
		}
	}
	answers("in memory")

	e.b.setQuota(1 << 40)
	ack, err := submitRaw(e, p.s, history[len(history)-1].n+1, e.seq(), `[`+e.setOp("C3", "fits")+`]`)
	if err != nil || !bytes.Contains(ack, []byte(`"status":"applied"`)) {
		t.Fatalf("the first action after the quota was raised: %s, %v", ack, err)
	}
	if p.s.rowless {
		t.Fatal("the session has no row after an applied action")
	}
	if count, _ := e.kept(p.s); count != 0 {
		t.Fatalf("memory still holds %d receipts that sheet.db holds", count)
	}
	var stored int
	if err := e.in.rdb.QueryRow(`SELECT count(*) FROM receipts WHERE session=?`, p.s.id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != receiptCache+1 {
		t.Fatalf("sheet.db holds %d receipts of the session, want the %d kept in memory and the new one", stored, receiptCache)
	}
	answers("from sheet.db")
}

// TestRefusalsAnswerAfterTheSessionGetsItsRow refuses an action of a
// session that has no row, then raises the quota so that the next action
// stores the row. The refused action sent again gets its refusal on the
// same connection, on a new one to the running instance and after a
// restart. The same number with other content ends the connection as
// another action under that number.
func TestRefusalsAnswerAfterTheSessionGetsItsRow(t *testing.T) {
	e := overQuota(t)
	s := serve(e)
	tab := e.tabID("Sheet1")
	_, row, col := e.ids("D1")
	action := func(n uint64, base uint64, text string) string {
		return fmt.Sprintf(`{"type":"action","n":"%d","base":"%d","ops":[{"op":"set","tab":%d,"row":%d,"col":%d,"text":%q,"at":[0,3]}]}`,
			n, base, tab, row, col, text)
	}
	carol := s.dial("private", `{"type":"hello","version":1,"name":"Carol","pending":0}`)
	welcome := carol.next("welcome")
	carol.watch(1, tab)
	base := e.seq()
	carol.send(action(1, base, "first"))
	refused := carol.next("ack")
	if fmt.Sprint(refused["results"]) != "[map[reason:disk_quota status:refused]]" {
		t.Fatalf("an edit over the quota: %v", refused)
	}

	e.b.setQuota(1 << 40)
	carol.send(action(2, e.seq(), "fits"))
	if got := carol.next("ack"); fmt.Sprint(got["results"]) != "[map[applied:1 status:applied]]" {
		t.Fatalf("an edit after the quota was raised: %v", got)
	}
	e.want("D1", "fits")
	again := func(c *client, where string) {
		t.Helper()
		c.send(action(1, base, "first"))
		if got := c.next("ack"); !reflect.DeepEqual(got, refused) {
			t.Fatalf("%s: the refused action sent again: %v, want %v", where, got, refused)
		}
	}
	again(carol, "same connection")

	// Another action under the number is told so by the writer, and on the
	// wire the connection ends.
	e.in.smu.Lock()
	sess := e.in.sessions[sessionID(tokenHash(welcome["session"].(string)))]
	e.in.smu.Unlock()
	if _, err := submitRaw(e, sess, 1, base, `[`+e.setOp("D1", "other")+`]`); refusal(err).Reason != "action_number" {
		t.Fatalf("another action under the refused number: %v", err)
	}
	carol.send(action(1, base, "other"))
	if f := carol.next("fatal"); f["code"] != "protocol" {
		t.Fatalf("another action under the refused number on the wire: %v", f)
	}
	e.want("D1", "fits")

	hello := fmt.Sprintf(`{"type":"hello","version":1,"session":%q,"pending":1}`, welcome["session"])
	carol = s.dial("private", hello)
	if w := carol.next("welcome"); w["session_id"] != welcome["session_id"] {
		t.Fatalf("the session did not resume: %v", w)
	}
	carol.watch(1, tab)
	again(carol, "new connection")

	e.restart()
	s = serve(e)
	carol = s.dial("private", hello)
	if w := carol.next("welcome"); w["session_id"] != welcome["session_id"] {
		t.Fatalf("the session did not resume after a restart: %v", w)
	}
	carol.watch(1, tab)
	again(carol, "after a restart")
}

// TestStoredReceiptsKeepTheNewest sends a session with its row actions
// whose numbers grow by more than the number of receipts sheet.db keeps.
// It keeps at least the newest receiptsKept of them by count, and fewer
// than receiptsKept + receiptCache, also across a restart.
func TestStoredReceiptsKeepTheNewest(t *testing.T) {
	e := start(t)
	s, token, err := e.in.newSession(context.Background(), "editor:Ann", "Ann", false)
	if err != nil {
		t.Fatal(err)
	}
	// An operation on a tab that does not exist is ignored, so the action
	// changes nothing and stores its receipt.
	op := `[{"op":"set","tab":999,"row":1,"col":1,"text":"x","at":[0,0]}]`
	var history []sent
	n := uint64(0)
	most := receiptsKept + receiptCache - 1
	for k := 0; k < receiptsKept+300; k++ {
		if k == receiptsKept+150 {
			e.restart()
			if s, err = e.in.resumeSession(context.Background(), token); err != nil {
				t.Fatal(err)
			}
		}
		n += 2 * receiptsKept
		if k >= 20 {
			n++
		}
		base := e.seq()
		ack, err := submitRaw(e, s, n, base, op)
		if err != nil {
			t.Fatalf("action %d: %v", n, err)
		}
		history = append(history, sent{n, base, op, sha256.Sum256(ack)})
		var stored int
		if err := e.in.rdb.QueryRow(`SELECT count(*) FROM receipts WHERE session=?`, s.id).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored < min(len(history), receiptsKept) || stored > most {
			t.Fatalf("after %d actions sheet.db holds %d receipts, want %d to %d", len(history), stored, min(len(history), receiptsKept), most)
		}
	}
	for _, a := range history[len(history)-receiptsKept:] {
		if got, err := submitRaw(e, s, a.n, a.base, a.ops); err != nil || sha256.Sum256(got) != a.ack {
			t.Fatalf("action %d sent again: %s, %v", a.n, got, err)
		}
	}
	for _, a := range history[:len(history)-most] {
		if _, err := submitRaw(e, s, a.n, a.base, a.ops); refusal(err).Reason != "receipt_expired" {
			t.Fatalf("action %d sent again after %d newer: %v", a.n, most, err)
		}
	}
}

// TestSessionInUseOutlivesTheExpiry keeps a connection open for longer than
// sessions are kept without activity, then edits on it. The edit is
// activity, so the session resumes afterwards with the receipt of the
// edit.
func TestSessionInUseOutlivesTheExpiry(t *testing.T) {
	e := start(t)
	s := serve(e)
	tab := e.tabID("Sheet1")
	_, row, col := e.ids("A1")
	c := s.dial("private", `{"type":"hello","version":1,"name":"Eve","pending":0}`)
	welcome := c.next("welcome")
	c.watch(1, tab)
	e.clock.Add(sessionTTL + time.Hour)
	e.tick()
	frame := fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"set","tab":%d,"row":%d,"col":%d,"text":"late","at":[0,0]}]}`, e.seq(), tab, row, col)
	c.send(frame)
	ack := c.next("ack")
	if fmt.Sprint(ack["results"]) != "[map[applied:1 status:applied]]" {
		t.Fatalf("an edit on the open connection: %v", ack)
	}
	c.ws.Close(1000, "")
	c = s.dial("private", fmt.Sprintf(`{"type":"hello","version":1,"session":%q,"pending":1}`, welcome["session"]))
	if w := c.next("welcome", "fatal"); w["type"] != "welcome" || w["session_id"] != welcome["session_id"] {
		t.Fatalf("the session that just edited did not resume: %v", w)
	}
	c.watch(1, tab)
	c.send(frame)
	if got := c.next("ack"); !reflect.DeepEqual(got, ack) {
		t.Fatalf("the edit sent again: %v, want %v", got, ack)
	}
}

// TestActionNumbersSheetDBStores sends the largest action number sheet.db
// stores, and one above it. The first is an action like any other; the
// second is no action, refused at once and never answered as storage that
// is unavailable for a while, also for a session without a row that would
// keep it in memory.
func TestActionNumbersSheetDBStores(t *testing.T) {
	e := start(t)
	s, token, err := e.in.newSession(context.Background(), "editor:Ann", "Ann", false)
	if err != nil {
		t.Fatal(err)
	}
	p := &person{e: e, s: s}
	op := `[` + e.setOp("A1", "x") + `]`
	if _, err := submitRaw(e, p.s, math.MaxInt64+1, e.seq(), op); refusal(err).Code != "invalid" || refusal(err).Reason != "action" {
		t.Fatalf("an action number above what sheet.db stores: %v", err)
	}
	ack, err := submitRaw(e, p.s, math.MaxInt64, e.seq(), op)
	if err != nil || !bytes.Contains(ack, []byte(`"status":"applied"`)) {
		t.Fatalf("the largest action number: %s, %v", ack, err)
	}
	e.restart()
	if got, err := e.in.resumeSession(context.Background(), token); err != nil || got.maxN != math.MaxInt64 {
		t.Fatalf("after a restart: %v, %v", got, err)
	}

	e = overQuota(t)
	q := e.person("Bob")
	if _, err := submitRaw(e, q.s, math.MaxInt64+1, e.seq(), op); refusal(err).Code != "invalid" {
		t.Fatalf("a session without a row: %v", err)
	}
	if count, _ := e.kept(q.s); count != 0 {
		t.Fatalf("memory keeps %d receipts of numbers sheet.db cannot store", count)
	}
}

// TestFailedCommitKeepsRowlessReceipts lets the commit of a session's
// actions fail while it has no row, once for an action that would drop the
// oldest receipt from memory and once for the action that would store the
// row.
// Neither takes anything the session kept, and the numbers stay free.
func TestFailedCommitKeepsRowlessReceipts(t *testing.T) {
	e := overQuota(t)
	p := e.person("Bob")
	op := `[` + e.setOp("B2", "x") + `]`
	var history []sent
	for k := uint64(1); k <= receiptCache; k++ {
		base := e.seq()
		ack, err := submitRaw(e, p.s, 1000*k, base, op)
		if err != nil {
			t.Fatal(err)
		}
		history = append(history, sent{1000 * k, base, op, sha256.Sum256(ack)})
	}
	answers := func(where string) {
		t.Helper()
		for _, a := range history[len(history)-receiptCache:] {
			if got, err := submitRaw(e, p.s, a.n, a.base, a.ops); err != nil || sha256.Sum256(got) != a.ack {
				t.Fatalf("%s: action %d sent again: %.100s, %v", where, a.n, got, err)
			}
		}
	}
	fail := errors.New("injected commit failure")
	e.in.fault = func() error { return fail }
	next := uint64(1000*receiptCache + 1)
	if _, err := submitRaw(e, p.s, next, e.seq(), op); refusal(err).Reason != "commit" {
		t.Fatalf("an action whose commit fails: %v", err)
	}
	e.in.fault = nil
	answers("after a failed refusal")

	e.b.setQuota(1 << 40)
	e.in.fault = func() error { return fail }
	if _, err := submitRaw(e, p.s, next, e.seq(), op); refusal(err).Reason != "commit" {
		t.Fatalf("an applied action whose commit fails: %v", err)
	}
	e.in.fault = nil
	if !p.s.rowless {
		t.Fatal("the session has a row after its commit failed")
	}
	var stored int
	if err := e.in.rdb.QueryRow(`SELECT count(*) FROM receipts WHERE session=?`, p.s.id).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("a failed commit left %d receipts, %v", stored, err)
	}
	answers("after a failed first row")
	ack, err := submitRaw(e, p.s, next, e.seq(), op)
	if err != nil || !bytes.Contains(ack, []byte(`"status":"applied"`)) {
		t.Fatalf("the action sent again: %s, %v", ack, err)
	}
	// The row took every receipt memory kept.
	answers("from sheet.db")
}

// TestRowlessReceiptsKeepBytes sends one session without a row refusals
// with the largest outcome such a session gets. Memory keeps fewer than 64
// of them, within keptSessionBytes, by their bytes and by the live heap.
// The kept ones answer as they did, the dropped ones are expired.
func TestRowlessReceiptsKeepBytes(t *testing.T) {
	e := overQuota(t)
	p := e.person("Bob")
	ops := e.refusals()
	var history []sent
	size := 0
	before := liveHeap()
	for k := uint64(1); k <= receiptCache; k++ {
		base := e.seq()
		ack, err := submitRaw(e, p.s, 7*k, base, ops)
		if err != nil || !bytes.Contains(ack, []byte(`"reason":"disk_quota"`)) {
			t.Fatalf("action %d: %.100s, %v", 7*k, ack, err)
		}
		history = append(history, sent{7 * k, base, ops, sha256.Sum256(ack)})
		size = len(ack)
		if _, kept := e.kept(p.s); kept > keptSessionBytes {
			t.Fatalf("after action %d memory keeps %d bytes of receipts", 7*k, kept)
		}
	}
	grew := liveHeap() - before
	count, kept := e.kept(p.s)
	t.Logf("%d refusals of %d bytes: memory keeps %d of them, %d bytes, and the live heap grew by %d", len(history), size, count, kept, grew)
	if count >= receiptCache || count < keptSessionBytes/(size+keptOverhead+size/4)-1 {
		t.Fatalf("memory keeps %d receipts of %d bytes", count, size)
	}
	if limit := int64(keptSessionBytes) * 3 / 2; grew > limit {
		t.Fatalf("the live heap grew by %d bytes, more than %d", grew, limit)
	}
	for i, a := range history {
		got, err := submitRaw(e, p.s, a.n, a.base, a.ops)
		switch {
		case i >= len(history)-count:
			if err != nil || sha256.Sum256(got) != a.ack {
				t.Fatalf("kept action %d sent again: %.100s, %v", a.n, got, err)
			}
		case refusal(err).Reason != "receipt_expired":
			t.Fatalf("dropped action %d sent again: %v", a.n, err)
		}
	}
}

// TestRowlessReceiptsKeepSheetBytes opens more sessions without a row than
// the sheet's bound holds receipts for, as reloads over the quota do, and
// closes each after its refusals. Together they keep at most keptSheetBytes,
// by their bytes and by the live heap. The sheet drops its oldest first,
// each kept receipt answers as it did, and the dropped ones are expired.
func TestRowlessReceiptsKeepSheetBytes(t *testing.T) {
	e := overQuota(t)
	ops := e.refusals()
	type kept struct {
		p *person
		a sent
	}
	var history []kept
	var people []*person
	size := 0
	before := liveHeap()
	for i := range 20 {
		p := e.person(fmt.Sprint("Reader ", i))
		people = append(people, p)
		for k := uint64(1); k <= 30; k++ {
			base := e.seq()
			ack, err := submitRaw(e, p.s, 1000*k, base, ops)
			if err != nil || !bytes.Contains(ack, []byte(`"reason":"disk_quota"`)) {
				t.Fatalf("action %d: %.100s, %v", 1000*k, ack, err)
			}
			history = append(history, kept{p, sent{1000 * k, base, ops, sha256.Sum256(ack)}})
			size = len(ack)
		}
		var total int64
		for _, q := range people {
			_, b := e.kept(q.s)
			total += b
		}
		if total > keptSheetBytes {
			t.Fatalf("with %d sessions memory keeps %d bytes of receipts", len(people), total)
		}
	}
	grew := liveHeap() - before
	t.Logf("%d sessions with %d refusals of %d bytes: the live heap grew by %d", len(people), len(history), size, grew)
	if limit := int64(keptSheetBytes) * 5 / 4; grew > limit {
		t.Fatalf("the live heap grew by %d bytes, more than %d", grew, limit)
	}
	// Each session keeps its newest receipts. The sheet drops its oldest
	// first, so a session opened later keeps at least as many, and the first
	// ones keep none.
	held, last := 0, 0
	counts := map[*session]int{}
	for i, q := range people {
		c, _ := e.kept(q.s)
		if c < last {
			t.Fatalf("session %d keeps %d receipts, the one before it %d", i, c, last)
		}
		counts[q.s], last, held = c, c, held+c
	}
	if counts[people[0].s] != 0 || held < keptSheetBytes/(size+keptOverhead+size/4)-1 {
		t.Fatalf("the sheet keeps %d receipts of %d bytes, the first session %d", held, size, counts[people[0].s])
	}
	for i, h := range history {
		got, err := submitRaw(e, h.p.s, h.a.n, h.a.base, h.a.ops)
		switch {
		case h.a.n > uint64(1000*(30-counts[h.p.s])):
			if err != nil || sha256.Sum256(got) != h.a.ack {
				t.Fatalf("kept action %d of %d sent again: %.100s, %v", h.a.n, i, got, err)
			}
		case refusal(err).Reason != "receipt_expired":
			t.Fatalf("dropped action %d of %d sent again: %v", h.a.n, i, err)
		}
	}
	// A session that expires lets go of what it kept.
	for _, q := range people {
		q.leave()
	}
	e.clock.Add(sessionTTL + time.Hour)
	e.in.smu.Lock()
	for _, q := range people {
		q.s.left = e.clock.Now().Add(-sessionTTL - time.Minute)
	}
	e.in.smu.Unlock()
	e.tick()
	e.in.mu.RLock()
	left := e.in.kept.bytes
	e.in.mu.RUnlock()
	if left != 0 {
		t.Fatalf("expired sessions still keep %d bytes", left)
	}
}

// keptList walks the sheet's kept receipts and fails unless the links,
// the byte total and each session's list agree. It returns the receipts.
func (e *env) keptList() []*keptReceipt {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	l := &e.in.kept
	var all []*keptReceipt
	var total int64
	seen := map[*session]int{}
	var prev *keptReceipt
	for k := l.head; k != nil; k = k.next {
		if k.prev != prev {
			e.t.Fatalf("receipt %d links back to the wrong one", k.n)
		}
		if i := seen[k.s]; i >= len(k.s.recent) || k.s.recent[i] != k {
			e.t.Fatalf("receipt %d is not where its session keeps it", k.n)
		}
		seen[k.s]++
		total += k.size()
		all, prev = append(all, k), k
	}
	if l.tail != prev || total != l.bytes {
		e.t.Fatalf("the list ends at the wrong receipt or counts %d bytes for %d", l.bytes, total)
	}
	for s, n := range seen {
		if n != len(s.recent) {
			e.t.Fatalf("a session keeps %d receipts, the list %d", len(s.recent), n)
		}
	}
	return all
}

// TestFailedCommitKeepsSheetReceipts fills the sheet's bound with receipts
// of several sessions without a row, then lets the commit of a refusal
// fail that would drop the oldest receipts of other sessions. Everything
// the sheet kept is back, in its order, and still answers.
func TestFailedCommitKeepsSheetReceipts(t *testing.T) {
	e := overQuota(t)
	ops := e.refusals()
	var people []*person
	for i := 0; ; i++ {
		p := e.person(fmt.Sprint("Reader ", i))
		people = append(people, p)
		for k := uint64(1); k <= 25; k++ {
			if _, err := submitRaw(e, p.s, k, e.seq(), ops); err != nil {
				t.Fatal(err)
			}
		}
		// The sheet has begun to drop the receipts of the first session.
		if first, _ := e.kept(people[0].s); i > 0 {
			if second, _ := e.kept(people[1].s); first < second {
				break
			}
		}
	}
	before := e.keptList()
	last := e.person("Late")
	e.in.fault = func() error { return errors.New("injected commit failure") }
	for range 3 {
		if _, err := submitRaw(e, last.s, 1, e.seq(), ops); refusal(err).Reason != "commit" {
			t.Fatalf("an action whose commit fails: %v", err)
		}
	}
	e.in.fault = nil
	if after := e.keptList(); !slices.Equal(after, before) {
		t.Fatalf("the sheet keeps %d receipts after the failed commits, %d before", len(after), len(before))
	}
	ack, err := submitRaw(e, last.s, 1, e.seq(), ops)
	if err != nil || !bytes.Contains(ack, []byte(`"reason":"disk_quota"`)) {
		t.Fatalf("the action sent again: %.100s, %v", ack, err)
	}
	if after := e.keptList(); len(after) == 0 || after[0] == before[0] {
		t.Fatal("the committed refusal dropped nothing of the oldest session")
	}
}
