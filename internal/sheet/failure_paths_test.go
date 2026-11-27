package sheet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// within fails the test when a signal does not arrive. The signals of these
// tests come from hooks the engine reaches at once, so the bound is only
// there to end a broken run.
func within[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not happen", what)
		panic("unreachable")
	}
}

// TestStateTooOld is the refusal of section 7.5. A sheet collects more
// blank records than it keeps, the maintenance tick removes the older half
// and raises the guard floor, and a range guard with a state below the
// floor is refused as state_too_old: after the collection the engine can no
// longer tell whether a cell of the range was cleared since that state. The
// refusal is for range guards alone, it never lets a guard pass that failed
// before the collection, and it is the same after a restart.
func TestStateTooOld(t *testing.T) {
	// Under the race detector the writes take longer than the interval of
	// the maintenance ticker, which then collects before the test looks.
	heavy(t, "over 100,000 blank records")
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Other"},{"op":"write","tab":"Other","at":"A1","rows":[["still"]]}]}`)
	const rows, cols = 1000, 34
	line := "[" + strings.TrimSuffix(strings.Repeat("1,", cols), ",") + "]"
	block := strings.TrimSuffix(strings.Repeat(line+",", rows), ",")
	for _, at := range []string{"A1", "AI1", "BQ1"} {
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"` + at + `","rows":[` + block + `]}]}`)
	}
	filled := e.state()
	cleared := e.mustAgent(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A1:CX1000"]}]}`).State
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["later"]]}]}`)
	// guarded sends a call with a range guard and a write that touches no
	// guarded cell, with the state the agent read.
	n := 0
	guarded := func(state, guard string) error {
		n++
		_, err := e.agent(fmt.Sprintf(`{"state":%q,"guard":{"ranges":[%q]},"ops":[{"op":"write","tab":"Other","at":"Z%d","rows":[[%d]]}]}`, state, guard, n, n))
		return err
	}
	counts := func() (blanks int, floor uint64) {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		return e.in.wb.count.blanks, e.in.wb.guardFloor
	}

	// Before the collection the stamps of the cleared cells answer. The
	// instance's own ticker can collect first on a slow machine; then
	// there is no "before" to look at.
	if blanks, floor := counts(); floor == 0 {
		if blanks <= maxBlankRecords {
			t.Fatalf("the sheet holds %d blank records, too few for a collection", blanks)
		}
		err := guarded(filled, "Sheet1!B1:B5")
		if _, floor := counts(); floor == 0 {
			e.wantRefusal(err, "conflict", "guard")
			if err := guarded(filled, "Other!A1"); err != nil {
				if _, floor := counts(); floor == 0 {
					t.Fatalf("a guard on an unchanged cell before the collection: %v", err)
				}
			}
		}
	}

	e.tick()
	left, floor := counts()
	if left > maxBlankRecords/2+1000 || floor == 0 {
		t.Fatalf("the collection left %d blank records and the floor %d", left, floor)
	}

	check := func(when string) {
		t.Helper()
		// The guard that failed still fails, now for its age.
		e.wantRefusal(guarded(filled, "Sheet1!B1:B5"), "conflict", "state_too_old")
		// So does a guard that would have passed: the engine cannot tell.
		e.wantRefusal(guarded(filled, "Other!A1"), "conflict", "state_too_old")
		// A state at the floor is not too old, and the stamps that remain
		// answer for it: nothing changed in B1:B5 after the clear, and A1
		// was written after it.
		if err := guarded(cleared, "Sheet1!B1:B5"); err != nil {
			t.Fatalf("%s: a guard with the state of the clear on cells unchanged since: %v", when, err)
		}
		err := guarded(cleared, "Sheet1!A1:B5")
		if werr := refusal(err); err == nil || werr.Code != "conflict" || werr.Reason != "guard" || werr.Name != "Sheet1!A1" {
			t.Fatalf("%s: a guard on a cell written after the state answered %+v, want guard Sheet1!A1", when, werr)
		}
		// The whole-sheet guard and a call without a guard do not read
		// the collected stamps, so the floor does not refuse them.
		_, err = e.agent(fmt.Sprintf(`{"state":%q,"guard":{"unchanged":true},"ops":[{"op":"write","tab":"Other","at":"Y1","rows":[[1]]}]}`, filled))
		e.wantRefusal(err, "conflict", "guard")
		n++
		if _, err := e.agent(fmt.Sprintf(`{"state":%q,"ops":[{"op":"write","tab":"Other","at":"X%d","rows":[[1]]}]}`, filled, n)); err != nil {
			t.Fatalf("%s: a call with an old state and no guard: %v", when, err)
		}
	}
	check("after the collection")
	before := e.seq()
	e.wantRefusal(guarded(filled, "Sheet1!B1:B5"), "conflict", "state_too_old")
	if e.seq() != before {
		t.Fatal("a call refused as state_too_old changed the sheet")
	}
	// The floor is durable.
	e.restart()
	check("after a restart")
}

// TestBaseBehindStructuralLog is the refusal a browser gets when the
// structural log no longer reaches back to the screen its formula was
// typed on (section 8.4): the rebase exchange answers refused, and a
// commit with that base is refused as stale_formula with nothing written.
// The log loses its entries by age here, on the maintenance tick. What the
// durable stamps can still decide is decided: a formula on a tab nobody
// changed is taken, and so is a plain value.
func TestBaseBehindStructuralLog(t *testing.T) {
	e := start(t)
	e.mustAgent(numbers)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Calm"},{"op":"write","tab":"Calm","at":"A1","rows":[[5]]}]}`)
	s := serve(e)
	tab, calm := e.tabID("Sheet1"), e.tabID("Calm")
	alice := s.dial("private", `{"type":"hello","version":1,"name":"Alice","pending":0}`)
	alice.next("welcome")
	alice.watch(1, tab)
	base := e.seq()
	_, row, col := e.ids("D5")
	_, calmRow, calmCol := e.ids("Calm!B1")
	e.person("Bob").do(e.insertRowsOp("Sheet1", 1, 1))
	rebase := func(tab TabID, row RowID, col ColID, text, at string, from uint64) map[string]any {
		alice.send(fmt.Sprintf(`{"type":"rebase","id":3,"tab":%d,"row":%d,"col":%d,"text":%q,"at":%s,"base":"%d","through":"%d"}`, tab, row, col, text, at, from, e.seq()))
		return alice.next("rebased", "fatal")
	}
	// While the log reaches back the formula follows the insert.
	if r := rebase(tab, row, col, "=B5+B6", "[4,3]", base); r["text"] != "=B6+B7" {
		t.Fatalf("rebased while the log reaches back = %v", r)
	}

	// An hour later the tick drops the entry.
	e.clock.Add(structLogAge + time.Minute)
	e.tick()
	e.in.mu.RLock()
	floor, kept := e.in.structs.floor, len(e.in.structs.entries)
	e.in.mu.RUnlock()
	if floor <= base || kept != 0 {
		t.Fatalf("the tick left %d entries and the floor %d, the base is %d", kept, floor, base)
	}

	if r := rebase(tab, row, col, "=B5+B6", "[4,3]", base); r["type"] != "rebased" || r["refused"] != "stale_formula" || r["text"] != nil || r["at"] != nil {
		t.Fatalf("rebase with a base behind the log = %v", r)
	}
	before := e.dump()
	action := func(n int, tab TabID, row RowID, col ColID, text, at string, from uint64) string {
		alice.send(fmt.Sprintf(`{"type":"action","n":"%d","base":"%d","ops":[{"op":"set","tab":%d,"row":%d,"col":%d,"text":%q,"at":%s}]}`, n, from, tab, row, col, text, at))
		ack := alice.next("ack", "fatal")
		return fmt.Sprint(ack["results"])
	}
	if got := action(1, tab, row, col, "=B5+B6", "[4,3]", base); got != "[map[reason:stale_formula status:refused]]" {
		t.Fatalf("a commit with a base behind the log was answered with %s", got)
	}
	if got := e.dump(); got != before {
		t.Fatalf("the refused commit changed the sheet:\n%s", dumpDiff(before, got))
	}
	// A base the log reaches is taken as before.
	if r := rebase(tab, row, col, "=B5+B6", "[5,3]", e.seq()); r["text"] != "=B5+B6" || r["refused"] != nil {
		t.Fatalf("rebase with the current base = %v", r)
	}
	// The stamps of a tab nobody changed rule a change out, so its
	// formula is taken with the old base, as long as it names no tab.
	if r := rebase(calm, calmRow, calmCol, "=A1*2", "[0,1]", base); r["text"] != "=A1*2" || r["refused"] != nil {
		t.Fatalf("rebase on an unchanged tab = %v", r)
	}
	if got := action(2, calm, calmRow, calmCol, "=A1*2", "[0,1]", base); got != "[map[applied:1 status:applied]]" {
		t.Fatalf("a commit on an unchanged tab was answered with %s", got)
	}
	e.want("Calm!B1", "10")
	if r := rebase(calm, calmRow, calmCol, "=Sheet1!A1", "[0,1]", base); r["refused"] != "stale_formula" {
		t.Fatalf("rebase of a formula that reads the changed tab = %v", r)
	}
	// A plain value names its cell by ids and needs no log.
	if got := action(3, tab, row, col, "7", "[4,3]", base); got != "[map[applied:1 status:applied]]" {
		t.Fatalf("a plain value with a base behind the log was answered with %s", got)
	}
	e.want("D6", "7")
}

// TestPublicSnapshotStartRate is the limit of eight Public snapshot starts
// per second (section 12.2), stepped by an injected clock. A ninth start
// in the second is held back: its connection gets no frame of the tab, an
// edit made meanwhile is part of the snapshot it gets later, and other
// connections go on. Private connections are not counted. When the second
// has passed, eight more starts are let through.
func TestPublicSnapshotStartRate(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	held, retry := make(chan struct{}), make(chan struct{})
	realNow, realPause := publicStartNow, publicStartPause
	publicStartNow = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	publicStartPause = func(ctx context.Context) bool {
		select {
		case held <- struct{}{}:
		case <-ctx.Done():
			return false
		}
		select {
		case <-retry:
			return true
		case <-ctx.Done():
			return false
		}
	}
	t.Cleanup(func() { publicStartNow, publicStartPause = realNow, realPause })
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}

	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["before"]]}]}`)
	s := serve(e)
	tab := e.tabID("Sheet1")
	key := cellKey(e, "A1")
	viewers := make([]*client, publicStartsPerS)
	for i := range viewers {
		viewers[i] = s.dial("public", helloV1)
		viewers[i].next("welcome")
		if _, cells := viewers[i].watch(1, tab); cells[key][2] != "before" {
			t.Fatalf("viewer %d: A1 = %v", i, cells[key])
		}
	}

	ninth := s.dial("public", helloV1)
	ninth.next("welcome")
	ninth.send(fmt.Sprintf(`{"type":"watch","generation":1,"tab":%d}`, tab))
	within(t, "holding the ninth start back", held)

	// While it is held back the sheet changes, a person opens the tab,
	// and a viewer that has its snapshot gets the delta.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["during"]]}]}`)
	person := s.dial("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	person.next("welcome")
	if _, cells := person.watch(1, tab); cells[key][2] != "during" {
		t.Fatalf("a private connection got A1 = %v while a Public start was held back", cells[key])
	}
	if d := viewers[0].next("delta"); d["cells"] == nil {
		t.Fatalf("a viewer with a snapshot got the delta %v", d)
	}

	// The same instant, and an instant short of a second later: still held.
	retry <- struct{}{}
	within(t, "holding the ninth start back at the same instant", held)
	advance(time.Second - time.Millisecond)
	retry <- struct{}{}
	within(t, "holding the ninth start back just short of a second", held)

	// A second after the eight starts it is let through.
	advance(time.Millisecond)
	retry <- struct{}{}
	head, cells := ninth.tab()
	if head["seq"] != fmt.Sprint(e.seq()) || cells[key][2] != "during" {
		t.Fatalf("the start that was held back got seq %v and A1 = %v, the sheet is at %d", head["seq"], cells[key], e.seq())
	}
	// Up to its snapshot the connection got nothing of the tab: welcome,
	// and deltas that name the tab as touched without its cells.
	for _, f := range ninth.seen {
		if f["type"] == "tab_head" {
			break
		}
		if kind := f["type"]; (kind != "welcome" && kind != "delta") || f["cells"] != nil || f["widths"] != nil {
			t.Fatalf("before its snapshot the connection that was held back got %v", f)
		}
	}

	// The limit counts again from here: the snapshot above and seven more
	// are this second's eight, and the next one is held back.
	for i := 0; i < publicStartsPerS-1; i++ {
		if head, _ := viewers[i].watch(2, tab); head["generation"] != float64(2) {
			t.Fatalf("viewer %d: head %v", i, head)
		}
	}
	viewers[7].send(fmt.Sprintf(`{"type":"watch","generation":2,"tab":%d}`, tab))
	within(t, "holding a ninth start of the next second back", held)
	advance(time.Second)
	retry <- struct{}{}
	if head, _ := viewers[7].tab(); head["generation"] != float64(2) {
		t.Fatalf("the last viewer: head %v", head)
	}
}

// holdCalculations takes every slot of the host-wide gate of
// recalculations that no edit starts, as passes that never give a slot up
// would, so that a load, a rollback or a revision view waits where it
// would calculate. The returned function gives the slots back.
func holdCalculations(t *testing.T) (release func()) {
	t.Helper()
	var turns []*calcTurn
	var once sync.Once
	release = func() {
		once.Do(func() {
			for _, turn := range turns {
				turn.leave()
			}
		})
	}
	t.Cleanup(release)
	for len(turns) < calcSlots.slots {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		turn, ok := calcSlots.enter(nil, ctx)
		cancel()
		if !ok {
			t.Fatal("a calculation slot was not free")
		}
		turns = append(turns, turn)
	}
	return release
}

func kindsOf(frames []map[string]any) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = fmt.Sprint(f["type"])
	}
	return out
}

// TestStatusWhileLoading connects while the workbook is still being loaded
// and calculated (section 10.5). The connection is told that the sheet is
// calculating, and nothing else, until the load ends: then it is told so,
// welcomed at the loaded sequence, and the watch it sent meanwhile gets the
// whole tab with the calculated values. An agent call in that time is
// refused as calculating and can be sent again.
func TestStatusWhileLoading(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[20,"=A1*2+1"]]}]}`)
	want := e.seq()
	tab := e.tabID("Sheet1")
	a1, b1 := cellKey(e, "A1"), cellKey(e, "B1")
	e.in.Stop()

	release := holdCalculations(t)
	in, _, err := Open(e.dir, e.b, e.opts)
	if err != nil {
		t.Fatal(err)
	}
	e.in, e.b.in = in, in
	s := serve(e)
	conns := map[string]*client{
		"private": s.dial("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`),
		"public":  s.dial("public", helloV1),
	}
	for route, c := range conns {
		c.send(fmt.Sprintf(`{"type":"watch","generation":1,"tab":%d}`, tab))
		if f := c.next("status", "welcome", "tab_head", "fatal"); f["type"] != "status" || f["calculating"] != true {
			t.Fatalf("%s: a connection during the load got %v first", route, f)
		}
	}
	// An agent does not wait for the load.
	if _, err := e.in.Read(context.Background(), ReadQuery{Tab: "Sheet1"}); err == nil || refusal(err).Code != "unavailable" || refusal(err).Reason != "calculating" || !refusal(err).Retry {
		t.Fatalf("a read during the load answered %v, want unavailable calculating", err)
	}
	u, err := DecodeUpdate([]byte(`{"ops":[{"op":"add_tab","name":"Early"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.in.Update(context.Background(), u, 0); err == nil || refusal(err).Code != "unavailable" || refusal(err).Reason != "calculating" || !refusal(err).Retry {
		t.Fatalf("a call during the load answered %v, want unavailable calculating", err)
	}

	release()
	for route, c := range conns {
		if f := c.next("status", "welcome", "tab_head", "fatal"); f["type"] != "status" || f["calculating"] != false {
			t.Fatalf("%s: after the load the connection got %v", route, f)
		}
		welcome := c.next("welcome", "tab_head", "fatal")
		if welcome["type"] != "welcome" || welcome["seq"] != fmt.Sprint(want) || len(welcome["tabs"].([]any)) != 1 {
			t.Fatalf("%s: welcome = %v", route, welcome)
		}
		head, cells := c.tab()
		if head["seq"] != fmt.Sprint(want) || cells[a1][2] != float64(20) || cells[b1][2] != float64(41) {
			t.Fatalf("%s: the tab arrived at %v with A1 = %v and B1 = %v", route, head["seq"], cells[a1], cells[b1])
		}
		// Nothing was sent between the two status frames, and nothing of
		// the sheet before the second one.
		// A private connection also learns who is present, which is no
		// part of the sheet.
		kinds := slices.DeleteFunc(kindsOf(c.seen), func(kind string) bool { return kind == "presence" && route == "private" })
		if len(kinds) < 4 || !slices.Equal(kinds[:4], []string{"status", "status", "welcome", "tab_head"}) || kinds[len(kinds)-1] != "tab_done" {
			t.Fatalf("%s: the connection got %v", route, kinds)
		}
	}
	if e.seq() != want {
		t.Fatalf("the refused call changed the sheet: sequence %d", e.seq())
	}
}

// TestStatusWhileRollbackCalculates rolls back while the calculation of
// the revision has to wait. Open connections are told that the sheet is
// calculating. Until the rollback is in place they get the sheet as it is:
// a watch gets the live tab whole, and no frame carries the revision. Then
// reset arrives, and after it the end of the calculation.
func TestStatusWhileRollbackCalculates(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["old","=LEN(A1)"]]}]}`)
	target := e.b.importAll(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["newer"]]}]}`)
	e.b.importAll(t)
	s := serve(e)
	tab := e.tabID("Sheet1")
	a1, b1 := cellKey(e, "A1"), cellKey(e, "B1")
	conns := map[string]*client{
		"private": s.dial("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`),
		"public":  s.dial("public", helloV1),
	}
	for _, c := range conns {
		c.next("welcome")
	}
	live := e.seq()

	release := holdCalculations(t)
	done := make(chan error, 1)
	go func() {
		_, err := e.b.Rollback(context.Background(), target, "Ann")
		done <- err
	}()
	for route, c := range conns {
		if f := c.next("status", "reset", "delta"); f["type"] != "status" || f["calculating"] != true {
			t.Fatalf("%s: during the rollback the connection got %v first", route, f)
		}
		head, cells := c.watch(1, tab)
		if head["seq"] != fmt.Sprint(live) || cells[a1][2] != "newer" || cells[b1][2] != float64(5) {
			t.Fatalf("%s: a watch during the rollback got seq %v, A1 = %v, B1 = %v", route, head["seq"], cells[a1], cells[b1])
		}
	}
	if e.seq() != live {
		t.Fatalf("the sequence moved to %d before the rollback calculated", e.seq())
	}

	release()
	if err := within(t, "the rollback", done); err != nil {
		t.Fatal(err)
	}
	for route, c := range conns {
		if f := c.next("reset", "status", "delta", "tab_head"); f["type"] != "reset" || f["seq"] != fmt.Sprint(live+1) {
			t.Fatalf("%s: after the rollback the connection got %v", route, f)
		}
		if f := c.next("status", "delta", "tab_head"); f["type"] != "status" || f["calculating"] != false {
			t.Fatalf("%s: after reset the connection got %v", route, f)
		}
		head, cells := c.watch(2, tab)
		if head["seq"] != fmt.Sprint(live+1) || cells[a1][2] != "old" || cells[b1][2] != float64(3) {
			t.Fatalf("%s: after the rollback the tab is at %v with A1 = %v and B1 = %v", route, head["seq"], cells[a1], cells[b1])
		}
	}
}

// TestRevisionOverReadLimitIsNotServed gives the host a revision whose
// files hold a formula that reads more cells than one formula may: no
// stored state does, so the files are damaged. A read of the revision and
// the History view are refused; neither serves the uncalculated cells.
func TestRevisionOverReadLimitIsNotServed(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"=SUM(A1:A9)"]]}]}`)
	rev := e.b.importAll(t)
	files, date, release, err := e.b.RevisionFiles(context.Background(), rev)
	if err != nil {
		t.Fatal(err)
	}
	release()
	e.b.mu.Lock()
	set := &revisionSet{files: files, data: e.b.blobs}
	set = set.with(axesPath(1), []byte(`{"cols":[[1,26]],"rows":[[1,100000]],"widths":{}}`))
	set = set.with(chunkPrefix(1, 0)+"0.json", []byte(`{"rows":[{"cells":{"1":{"v":1},"2":{"f":"=SUM({R[0]C[-1]:R[99999]C[24]})"}},"id":1}],"styles":[]}`))
	e.b.blobs = set.data
	e.b.revs = append(e.b.revs, fakeRevision{entry: OutboxEntry{Kind: "agent", CalcDate: date, Files: set.files}})
	damaged := len(e.b.revs)
	e.b.mu.Unlock()
	if _, err := loadRevision(set.files, set.read, nil); err != nil {
		t.Fatalf("the files load: %v", err)
	}
	for {
		_, err := e.in.ReadRevision(context.Background(), damaged, set.files, date, ReadQuery{Tab: "Sheet1", Range: "A1:B1"})
		if werr := refusal(err); err != nil && werr.Reason == "calculating" {
			continue
		}
		e.wantRefusal(err, "unavailable", "revision")
		break
	}
	s := serve(e)
	for _, path := range []string{"", "/tabs/1", "/tabs/1/cell?row=1&col=2"} {
		for {
			req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/private/_sheet/api/revisions/%d%s", s.server.URL, damaged, path), nil)
			req.Header.Set("X-Flats-Sheet", "1")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var out map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&out)
			resp.Body.Close()
			if out["reason"] == "calculating" {
				continue
			}
			if resp.StatusCode != http.StatusServiceUnavailable || out["reason"] != "revision" {
				t.Errorf("History %q of the damaged revision: %d %v", path, resp.StatusCode, out)
			}
			break
		}
	}
	// The revision before it is served as ever.
	if out := e.readRevision(rev, ReadQuery{Tab: "Sheet1", Range: "B1"}); firstCell(out) == nil {
		t.Fatalf("the sound revision reads %v", out)
	}
}
