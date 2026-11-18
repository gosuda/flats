package sheet

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestRecalcObserver checks what the host learns of each recalculation:
// its duration and whether an edit started it.
func TestRecalcObserver(t *testing.T) {
	var mu sync.Mutex
	var seen []bool
	var total time.Duration
	take := func() (string, time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		out := fmt.Sprint(seen)
		seen = nil
		return out, total
	}
	e := start(t, func(o *Options) {
		o.OnRecalc = func(edit bool, took time.Duration) {
			mu.Lock()
			seen, total = append(seen, edit), total+took
			mu.Unlock()
		}
	})
	if got, _ := take(); got != "[false]" {
		t.Fatalf("the load reported %s", got)
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"=A1*2","=TODAY()"]]}]}`)
	alice := e.person("Alice")
	alice.set("A1", "2")
	if got, _ := take(); got != "[true true]" {
		t.Fatalf("an agent call and a person's edit reported %s", got)
	}
	// A format alone calculates nothing; a refused edit calculated.
	tab, row, col := e.ids("A1")
	alice.do(jsonf(`{"op":"format","tab":%d,"rows":[[%d,1]],"cols":[[%d,1]],"set":{"bold":true}}`, tab, row, col))
	if _, err := e.agent(`{"ops":[{"op":"write","tab":"Sheet1","at":"D1","rows":[["=SUM(A1:K90910)"]]}]}`); refusal(err).Limit != "formula_reads" {
		t.Fatal(err)
	}
	if got, _ := take(); got != "[true]" {
		t.Fatalf("a format and a refused call reported %s", got)
	}
	rev := e.b.importAll(t)
	// The date change, a revision view and a rollback are not edits.
	e.clock.Add(24 * time.Hour)
	e.read(ReadQuery{Tab: "Sheet1"})
	e.readRevision(rev, ReadQuery{Tab: "Sheet1"})
	if _, err := e.b.Rollback(context.Background(), rev, "Alice"); err != nil {
		t.Fatal(err)
	}
	if got, _ := take(); got != "[false false false]" {
		t.Fatalf("a date change, a revision view and a rollback reported %s", got)
	}
	e.restart()
	got, elapsed := take()
	if got != "[false]" || elapsed < 0 {
		t.Fatalf("the reload reported %s, %v in all", got, elapsed)
	}
	const duration = time.Millisecond
	e.in.observeCalc(true, duration)
	if got, after := take(); got != "[true]" || after-elapsed != duration {
		t.Fatalf("the explicit observation reported %s in %v, want [true] in %v", got, after-elapsed, duration)
	}
	if n := testing.AllocsPerRun(100, func() { e.in.observeCalc(true, time.Millisecond) }); n != 0 {
		t.Fatalf("reporting a recalculation allocates %v times", n)
	}
}
