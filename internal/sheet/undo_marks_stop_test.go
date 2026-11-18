package sheet

import "testing"

// stoppingFit returns a sheet whose write marks fill several pacing steps
// between entries of Alice and Bob that ask about them, with the write
// lock and the sessions' lock held until the test ends.
func stoppingFit(t *testing.T) *env {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Big"}]}`)
	for _, p := range []*person{e.person("Alice"), e.person("Bob")} {
		if ack := p.do(e.insertRowsOp("Sheet1", 1, 1)); status(ack) != "applied" {
			t.Fatalf("an insert: %s", status(ack))
		}
		markCycleIn(e, "Big", 4*paceEvery)
	}
	e.in.mu.Lock()
	e.in.smu.Lock()
	t.Cleanup(func() {
		e.in.undoPool().markWork = nil
		e.in.smu.Unlock()
		e.in.mu.Unlock()
	})
	return e
}

// stopAt makes the instance stop once the fit has looked at n marks and
// entries, and returns how many it looked at in all and when it stopped.
func stopAt(e *env, n int) (work, at *int) {
	work, at = new(int), new(int)
	*at = -1
	e.in.undoPool().markWork = func(k int) {
		*work += k
		if *at < 0 && *work >= n {
			*at = *work
			e.in.cancel()
		}
	}
	return work, at
}

// A fit of the sheet's undo memory that the instance stops looks at no
// more than one pacing step of marks, and the few entries of the stacks,
// after the stop: neither the cleanup it begins with nor the one it ends
// with goes on to look at every mark, and the end does not start a
// cleanup after a stop in the count between. The sheet holds four times
// the marks of a step.
func TestUndoFitStopsWithTheInstance(t *testing.T) {
	marks := func(e *env) int {
		n := 0
		for _, t := range e.in.wb.tabs {
			n += len(t.marks) + len(t.colMarks)
		}
		return n
	}
	t.Run("in the cleanup it begins with", func(t *testing.T) {
		e := stoppingFit(t)
		total := marks(e)
		f := &markFit{in: e.in}
		work, at := stopAt(e, paceEvery+paceEvery/2)
		f.begin()
		if *at < 0 {
			t.Fatalf("the fit looked at %d marks and entries for %d marks and did not reach the stop", *work, total)
		}
		if !f.stopped || f.bytes() != 0 {
			t.Fatalf("the stopped fit is not marked so: stopped %v, %d bytes", f.stopped, f.bytes())
		}
		if front := f.front(); front != nil {
			front.expire(nil)
		}
		f.follow()
		f.finish()
		if after := *work - *at; after > paceEvery+64 || total < 4*paceEvery {
			t.Fatalf("after the stop the fit looked at %d marks of %d", after, total)
		}
	})
	t.Run("in the count between", func(t *testing.T) {
		e := stoppingFit(t)
		total := marks(e)
		f := &markFit{in: e.in}
		f.begin()
		if f.stopped || f.front() == nil {
			t.Fatal("the fit found no entry that asks")
		}
		f.front().expire(nil)
		work, at := stopAt(e, 1)
		f.follow()
		if !f.stopped {
			t.Fatal("the count went on after the stop")
		}
		f.finish()
		if after := *work - *at; after > paceEvery+64 || total < 4*paceEvery {
			t.Fatalf("after the stop the fit looked at %d marks and entries of %d marks", after, total)
		}
	})
}
