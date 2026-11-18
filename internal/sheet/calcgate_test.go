package sheet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// heldPasses keeps the passes of chosen instances at their check points,
// as passes with minutes of work left would be: they go on pausing there,
// and so take their turns at the gate, until the test lets them finish.
type heldPasses struct {
	mu   sync.Mutex
	all  bool
	held map[*Instance]bool
	at   map[*Instance]bool // instances whose pass reached a check point
	open bool
}

func holdPasses(t *testing.T) *heldPasses {
	t.Helper()
	h := &heldPasses{held: map[*Instance]bool{}, at: map[*Instance]bool{}}
	hold := func(turn *calcTurn) bool {
		in := turn.in
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.open || in == nil || !(h.all || h.held[in]) {
			return false
		}
		h.at[in] = true
		return true
	}
	if !calcHold.CompareAndSwap(nil, &hold) {
		t.Fatal("another test holds passes")
	}
	t.Cleanup(func() {
		h.release()
		calcHold.Store(nil)
	})
	return h
}

func (h *heldPasses) hold(in *Instance) {
	h.mu.Lock()
	h.held[in] = true
	h.mu.Unlock()
}

// holdAll keeps the pass of every instance, also of one that is opened
// later.
func (h *heldPasses) holdAll() {
	h.mu.Lock()
	h.all = true
	h.mu.Unlock()
}

// release lets every held pass finish.
func (h *heldPasses) release() {
	h.mu.Lock()
	h.open = true
	h.mu.Unlock()
}

// reached waits until the passes of n instances are at a check point.
func (h *heldPasses) reached(t *testing.T, n int) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Millisecond) {
		h.mu.Lock()
		at := len(h.at)
		h.mu.Unlock()
		if at >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d passes reached a check point, want %d", at, n)
		}
	}
}

// gateState returns the free slots of the gate and the passes in line.
func gateState() (free, line int) {
	calcSlots.mu.Lock()
	defer calcSlots.mu.Unlock()
	return calcSlots.free, len(calcSlots.line)
}

// gateSettles waits until the gate has the given state.
func gateSettles(t *testing.T, what string, free, line int) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Millisecond) {
		f, l := gateState()
		if f == free && l == line {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the gate has %d free slots and %d passes in line, want %d and %d", what, f, l, free, line)
		}
	}
}

// gateSlots gives the host-wide gate n slots for the rest of the test, so
// that a test does not depend on the processors of the machine. A host
// with up to three processors has one slot and a larger one has more
// (section 10.5). The gate must be idle, and is idle again before it gets
// its slots back.
func gateSlots(t *testing.T, n int) {
	t.Helper()
	g := calcSlots
	g.mu.Lock()
	was := g.slots
	idle := g.free == g.slots && len(g.line) == 0
	if idle {
		g.slots, g.free = n, n
	}
	g.mu.Unlock()
	if !idle {
		t.Fatal("the gate is in use")
	}
	// Registered before any instance of the test, so it runs after their
	// stops. A pass that still holds a slot then keeps the count right for
	// the tests that follow.
	t.Cleanup(func() {
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Millisecond) {
			free, line := gateState()
			if free == n && line == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("after the test the gate has %d free slots of %d and %d passes in line", free, n, line)
				break
			}
		}
		g.mu.Lock()
		g.slots, g.free = was, g.free+was-n
		g.mu.Unlock()
	})
}

// gateSizes are the gates a test of the gate runs with: the one slot of a
// small host, where a second pass stands in line, and more than one.
var gateSizes = []int{1, 3}

const longRows, longSum = 6000, "18003000"

// longSheet returns a sheet whose passes reach check points: a load reads
// 6,000 cells and sums them, and so does a change of the date, whose
// formula reads nothing before the day after the test's first day.
func longSheet(t *testing.T) *env {
	t.Helper()
	e := start(t)
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":%d}]}`, longRows-newTabRows))
	var b strings.Builder
	for i := 1; i <= longRows; i++ {
		fmt.Fprintf(&b, "[%d],", i)
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + strings.TrimSuffix(b.String(), ",") + `]},
		{"op":"write","tab":"Sheet1","at":"B1","rows":[["=IF(TODAY()>DATE(2026,11,13),SUM(A1:A6000),-1)","=SUM(A1:A6000)"]]}]}`)
	e.want("B1", "-1")
	e.want("C1", longSum)
	return e
}

// shortBound is what a small pass of one sheet may take while the passes
// of other sheets occupy the gate. The gate hands a slot on after 10 ms
// for each pass ahead in line; the rest is room for a busy machine.
const shortBound = 2 * time.Second

func timed(t *testing.T, what string, fn func()) {
	t.Helper()
	started := time.Now()
	fn()
	took := time.Since(started)
	t.Logf("%s took %v", what, took)
	if took > shortBound {
		t.Fatalf("%s took %v while long passes held the gate; it may take %v", what, took, shortBound)
	}
}

// TestLongPassesTakeTurns starts more long valid passes than the gate has
// slots, on sheets of their own, and keeps them at work. An unrelated
// small sheet then loads, takes a date change, serves a revision and rolls
// back, each within a short bound: the long passes hand their slots on.
// When the long passes are let go they finish with the right results, and
// the gate has every slot back.
func TestLongPassesTakeTurns(t *testing.T) {
	ctx := context.Background()
	slots := calcSlots.slots
	gateSettles(t, "before the test", slots, 0)
	held := holdPasses(t)
	long := make([]*env, slots+2)
	for i := range long {
		long[i] = longSheet(t)
		held.hold(long[i].in)
	}
	// The date changes of the long sheets start, by a read each, and stay
	// at work. A read does not wait for the change: it is refused as
	// calculating.
	started := make([]chan error, len(long))
	for i, e := range long {
		e.clock.Add(24 * time.Hour)
		started[i] = make(chan error, 1)
		go func() {
			_, err := e.in.Read(ctx, ReadQuery{Tab: "Sheet1", Range: "B1:C1", Include: []string{"shown"}})
			started[i] <- err
		}()
	}
	held.reached(t, len(long))
	gateSettles(t, "with the long passes at work", 0, len(long)-slots)
	parked := calcSlots.parks.Load()
	if parked < 2 {
		t.Fatalf("%d slots were handed on while %d passes shared %d slots", parked, len(long), slots)
	}

	// The small sheet. Should its passes not get a slot, the long passes
	// are let go after a while, so that the test ends and reports it.
	guard := time.AfterFunc(10*shortBound, held.release)
	defer guard.Stop()
	var small *env
	timed(t, "the first load of a new sheet", func() { small = start(t) })
	small.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"=SUM(A1:A3)","=TODAY()"],[2],[3]]}]}`)
	rev := small.b.importAll(t)
	small.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[10]]}]}`)
	small.b.importAll(t)
	timed(t, "a load", small.restart)
	small.want("B1", "15")
	small.clock.Add(24 * time.Hour)
	timed(t, "a date change", func() {
		out := small.read(ReadQuery{Tab: "Sheet1", Range: "C1", Include: []string{"shown"}})
		if out["date"] != "2026-11-14" || fmt.Sprint(out["shown"]) != "[[2026-11-14]]" {
			t.Fatalf("after the date change the small sheet reads %v on %v", out["shown"], out["date"])
		}
	})
	timed(t, "a revision read", func() {
		out := small.readRevision(rev, ReadQuery{Tab: "Sheet1", Range: "B1", Include: []string{"shown"}})
		if fmt.Sprint(out["shown"]) != "[[6]]" {
			t.Fatalf("the revision reads %v", out["shown"])
		}
	})
	timed(t, "a rollback", func() {
		if _, err := small.b.Rollback(ctx, rev, "Ann"); err != nil {
			t.Fatal(err)
		}
	})
	small.want("B1", "6")

	// All of that went through the gate while every slot was taken: the
	// long passes gave slots up for it, and none of them has finished.
	if got := calcSlots.parks.Load() - parked; got < 5 {
		t.Fatalf("the long passes handed %d slots on for five small passes", got)
	}
	for i, e := range long {
		wantCalculating(t, fmt.Sprintf("the read that started the long pass %d", i), within(t, "the read that started a long pass", started[i]))
		if got := e.calcDate(); got != "2026-11-13" {
			t.Fatalf("the long pass %d ended while it was held: the sheet is at %s", i, got)
		}
	}
	small.in.Stop()
	gateSettles(t, "after the small passes", 0, len(long)-slots)

	// The long passes finish, each with its own result.
	held.release()
	for i, e := range long {
		data, err := calculated(func() (json.RawMessage, error) {
			return e.in.Read(ctx, ReadQuery{Tab: "Sheet1", Range: "B1:C1", Include: []string{"shown"}})
		})
		if err != nil {
			t.Fatalf("the long pass %d: %v", i, err)
		}
		if out := string(data); !strings.Contains(out, `"date":"2026-11-14"`) || !strings.Contains(out, `[["`+longSum+`","`+longSum+`"]]`) {
			t.Fatalf("the long pass %d left %s", i, out)
		}
		e.want("B1", longSum)
	}
	gateSettles(t, "after every pass", slots, 0)
}

// TestStopEndsAPass stops an instance whose pass is at work, and one whose
// pass waits in line. Neither stop waits for the pass to finish, and the
// gate keeps no slot and no place in line for them. Both run with a gate
// of several slots and with a gate of one.
func TestStopEndsAPass(t *testing.T) {
	for _, how := range []string{"at work", "in line", "at work with one slot", "in line with one slot"} {
		t.Run(how, func(t *testing.T) {
			slots := gateSizes[1]
			how, one := strings.CutSuffix(how, " with one slot")
			if one {
				slots = gateSizes[0]
			}
			gateSlots(t, slots)
			e := longSheet(t)
			e.in.Stop()
			held := holdPasses(t)
			held.holdAll()
			release := func() {}
			if how == "in line" {
				release = holdCalculations(t)
			}
			in, _, err := Open(e.dir, e.b, e.opts)
			if err != nil {
				t.Fatal(err)
			}
			e.in, e.b.in = in, in
			if how == "at work" {
				held.reached(t, 1)
				gateSettles(t, "with the load at work", slots-1, 0)
			} else {
				gateSettles(t, "with the load in line", 0, 1)
			}
			timed(t, "the stop", in.Stop)
			if err := in.await(context.Background(), 0); refusal(err).Reason != "load" && refusal(err).Reason != "stopped" {
				t.Fatalf("after the stop the load reports %v", err)
			}
			release()
			gateSettles(t, "after the stop", slots, 0)
			// The sheet is whole: it loads when its pass may finish.
			held.release()
			e.open()
			e.want("C1", longSum)
		})
	}
}

// TestDroppedRevisionBuildEnds asks for more revisions of a sheet than its
// cache keeps while their builds are under way. The build of the workbook
// that leaves the cache ends at once; it does not go on for a result that
// nobody can get.
//
// It runs with a gate of one slot and with a larger one, and in each with
// the builds at work and with the builds in line. Two builds at work hold
// two slots, or take turns on one slot with the other in line. Builds in
// line stand behind passes that hold every slot, so the build that is
// dropped has never had one.
func TestDroppedRevisionBuildEnds(t *testing.T) {
	for _, slots := range gateSizes {
		for _, how := range []string{"at work", "in line"} {
			t.Run(fmt.Sprintf("%d slots, builds %s", slots, how), func(t *testing.T) {
				gateSlots(t, slots)
				e := longSheet(t)
				revs := make([]int, 3)
				for i := range revs {
					e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"D1","rows":[[%d]]}]}`, i))
					revs[i] = e.b.importAll(t)
				}
				// What the gate holds while two builds are alive.
				free, line := max(slots-2, 0), max(2-slots, 0)
				finish := func() {}
				if how == "at work" {
					held := holdPasses(t)
					held.holdAll()
					finish = held.release
				} else {
					free, line = 0, 2
					finish = holdCalculations(t)
				}
				ask := func(n int) *revisionBook {
					t.Helper()
					files, date, release, err := e.b.RevisionFiles(context.Background(), n)
					if err != nil {
						t.Fatal(err)
					}
					defer release()
					_, err = e.in.ReadRevision(context.Background(), n, files, date, ReadQuery{Tab: "Sheet1", Range: "C1"})
					e.wantRefusal(err, "unavailable", "calculating")
					e.in.revs.mu.Lock()
					defer e.in.revs.mu.Unlock()
					for _, b := range e.in.revs.list {
						if b.n == n {
							return b
						}
					}
					t.Fatalf("revision %d is not being built", n)
					return nil
				}
				first := ask(revs[0])
				e.clock.Add(time.Second)
				ask(revs[1])
				gateSettles(t, "with two builds", free, line)
				select {
				case <-first.ready:
					t.Fatalf("the first build ended before it was dropped: %v", first.err)
				default:
				}
				e.clock.Add(time.Second)
				ask(revs[2])
				within(t, "the end of the dropped build", first.ready)
				if first.err == nil || first.wb != nil {
					t.Fatalf("the dropped build ended with a workbook: %v", first.err)
				}
				gateSettles(t, "after the dropped build", free, line)
				finish()
				out := e.readRevision(revs[2], ReadQuery{Tab: "Sheet1", Range: "C1:D1", Include: []string{"shown"}})
				if fmt.Sprint(out["shown"]) != "[["+longSum+" 2]]" {
					t.Fatalf("the newest revision reads %v", out["shown"])
				}
				gateSettles(t, "after every build", slots, 0)
			})
		}
	}
}

// TestReadersDuringALongDateChange keeps the date change of a sheet at
// work and reads the sheet meanwhile, as the design promises its readers
// the previous state, whole, until the swap: a watch of a connection that
// is open, the first Public snapshot of an access epoch, which rebuilds
// the format list under the write lock, a watch after it, and a download.
// The pass holds no lock that any of them waits for.
func TestReadersDuringALongDateChange(t *testing.T) {
	e := longSheet(t)
	s := serve(e)
	tab := e.tabID("Sheet1")
	// A Private connection needs a session, which the writer stores: it is
	// open before the writer starts the pass.
	editor := s.dial("private", helloV1)
	editor.next("welcome")
	held := holdPasses(t)
	held.hold(e.in)
	e.clock.Add(24 * time.Hour)
	day := e.calcDate()
	// The read starts the date change and does not wait for it.
	_, err := e.in.Read(context.Background(), ReadQuery{Tab: "Sheet1", Range: "B1"})
	wantCalculating(t, "the read that meets the new date", err)
	held.reached(t, 1)
	b1 := cellKey(e, "B1")
	previous := func(what string, cells map[string][]any) {
		t.Helper()
		if cell := cells[b1]; len(cell) < 3 || cell[2] != float64(-1) {
			t.Fatalf("%s reads B1 = %v during the date change, want the previous value", what, cell)
		}
	}
	guard := time.AfterFunc(4*shortBound, held.release)
	defer guard.Stop()
	timed(t, "a Private watch", func() {
		_, cells := editor.watch(1, tab)
		previous("a Private watch", cells)
	})
	timed(t, "the first Public snapshot", func() {
		c := s.dial("public", helloV1)
		c.next("welcome")
		_, cells := c.watch(1, tab)
		previous("a Public watch", cells)
	})
	timed(t, "a watch after the Public snapshot", func() {
		_, cells := editor.watch(2, tab)
		previous("a second Private watch", cells)
	})
	timed(t, "a download", func() {
		res, err := http.Get(fmt.Sprintf("%s/private/_sheet/csv?tab=%d", s.server.URL, tab))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "1,-1,"+longSum+"\r\n") {
			t.Fatalf("the download: status %d, %.60q", res.StatusCode, body)
		}
	})
	if got := e.calcDate(); got != day {
		t.Fatalf("the date moved to %s while its pass was held", got)
	}
	held.release()
	e.read(ReadQuery{Tab: "Sheet1", Range: "B1"})
	e.want("B1", longSum)
}
