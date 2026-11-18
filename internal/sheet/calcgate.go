package sheet

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// calcQuantum is how long a recalculation that no edit starts keeps its
// slot while another one waits for it (technical design, section 10.5).
const calcQuantum = 10 * time.Millisecond

// loadCheckCells is the number of stored cells a load reads between two
// looks at the gate. The index build, the ordering and the evaluation look
// at it at the budget's interval of 4,096 units and every passStride
// formulas.
const loadCheckCells = 4096

// calcGate is the host-wide gate of recalculations that no edit starts: a
// load, a change of the calculation date, a rollback and a revision view,
// and of the build of the revision a tab recovery copies from.
// It lets max(1, GOMAXPROCS/2) of them run at once, so half of the
// processors stay free for other work of the host (section 10.5).
//
// The passes take turns. A pass that waits stands in one line, first come
// first served. A pass that runs gives its slot to the first in line once
// it has held it for calcQuantum, and goes to the end of the line itself.
// So a pass of minutes does not keep a slot from the short pass of another
// sheet for longer than a quantum for every pass ahead in line, and no
// pass starves: each one in line is at most the others away from its next
// slot.
//
// A pass holds no lock while it waits in line: not the gate's, not a
// workbook's. Its context ends it there, as it ends it at work.
type calcGate struct {
	mu    sync.Mutex
	slots int
	free  int
	line  []*calcTurn
	// waiting is len(line), for a running pass to read without the lock.
	waiting atomic.Int32
	// parks counts the slots that were given up for another pass.
	parks atomic.Int64
}

func newCalcGate(slots int) *calcGate { return &calcGate{slots: slots, free: slots} }

var calcSlots = newCalcGate(max(1, runtime.GOMAXPROCS(0)/2))

// calcTurn is one pass at the gate. Only the goroutine of the pass uses it.
type calcTurn struct {
	g       *calcGate
	in      *Instance
	ctx     context.Context
	slot    chan struct{} // receives the slot the gate hands over
	holding bool
	since   time.Time     // when the pass got the slot it holds
	waited  time.Duration // in line after it had given a slot up
}

// enter waits in line for a slot. ok is false when ctx ended first; the
// pass then holds nothing. The caller calls leave when the pass is over,
// however it ended.
func (g *calcGate) enter(in *Instance, ctx context.Context) (t *calcTurn, ok bool) {
	t = &calcTurn{g: g, in: in, ctx: ctx, slot: make(chan struct{}, 1)}
	return t, t.take()
}

func (t *calcTurn) take() bool {
	g := t.g
	g.mu.Lock()
	if g.free > 0 && len(g.line) == 0 {
		g.free--
		g.mu.Unlock()
		t.holding, t.since = true, time.Now()
		return true
	}
	g.line = append(g.line, t)
	g.waiting.Store(int32(len(g.line)))
	g.mu.Unlock()
	select {
	case <-t.slot:
		t.holding, t.since = true, time.Now()
		return true
	case <-t.ctx.Done():
	}
	g.mu.Lock()
	i := slices.Index(g.line, t)
	if i >= 0 {
		g.line = cut(g.line, i, i+1)
		g.waiting.Store(int32(len(g.line)))
	}
	g.mu.Unlock()
	if i < 0 {
		// The slot was handed over in the same moment: it goes on.
		<-t.slot
		g.release()
	}
	return false
}

// release gives a slot to the first pass in line, or back to the gate.
func (g *calcGate) release() {
	g.mu.Lock()
	if len(g.line) == 0 {
		g.free++
		g.mu.Unlock()
		return
	}
	next := g.line[0]
	g.line = cut(g.line, 0, 1)
	g.waiting.Store(int32(len(g.line)))
	g.mu.Unlock()
	next.slot <- struct{}{}
}

// leave ends the pass at the gate.
func (t *calcTurn) leave() {
	if t.holding {
		t.holding = false
		t.g.release()
	}
}

// pause is called by the pass at its check points. When the pass has held
// its slot for calcQuantum and another pass waits, it hands the slot over
// and waits in line for the next one. It reports false when the pass has
// to stop because its context ended, whether it was at work or in line.
func (t *calcTurn) pause() bool {
	if t.ctx.Err() != nil {
		return false
	}
	if t.g.waiting.Load() == 0 || time.Since(t.since) < calcQuantum {
		return true
	}
	t.holding = false
	t.g.parks.Add(1)
	t.g.release()
	parked := time.Now()
	ok := t.take()
	t.waited += time.Since(parked)
	return ok
}

// recalc runs a recalculation of the pass. The time it reports is the time
// at work: the time the pass stood in line meanwhile is not in it.
func (t *calcTurn) recalc(wb *Workbook, req calcReq) (calcOut, *WriteError) {
	waited := t.waited
	out, err := wb.recalc(req)
	out.took -= t.waited - waited
	return out, err
}

// calcHold lets a test keep a pass at a check point, as a pass with more
// work would be, until the test lets it go on. The pass keeps pausing
// there. It is nil outside tests.
var calcHold atomic.Pointer[func(t *calcTurn) bool]

// HoldPass is calcHold for the tests of the host. It keeps the first pass
// of in that reaches a check point at that point until release is called,
// and lets every other pass go on. The held pass keeps pausing, so it hands
// its slot on and ends with its stop or its request. reached is closed
// once the pass is held. It fails while another hold is set.
func HoldPass(in *Instance) (reached <-chan struct{}, release func(), err error) {
	var mu sync.Mutex
	var held *calcTurn
	open, at := false, make(chan struct{})
	hold := func(t *calcTurn) bool {
		if t.in != in {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		if !open && held == nil {
			held = t
			close(at)
		}
		return !open && t == held
	}
	if !calcHold.CompareAndSwap(nil, &hold) {
		return nil, nil, errors.New("sheet: another test holds passes")
	}
	release = sync.OnceFunc(func() {
		mu.Lock()
		open = true
		mu.Unlock()
		calcHold.CompareAndSwap(&hold, nil)
	})
	return at, release, nil
}

// check is the check point of a pass. It reports false when the pass has
// to stop.
func (t *calcTurn) check() bool {
	for t.pause() {
		hold := calcHold.Load()
		if hold == nil || !(*hold)(t) {
			return true
		}
		time.Sleep(50 * time.Microsecond)
	}
	return false
}

// budget returns a budget for a part of the pass. It has no deadline: its
// check, every 4,096 units of work and wherever a loop of the pass asks
// for one, is the pass's check point, and it counts as exceeded only once
// the pass has to stop.
func (t *calcTurn) budget() *formula.Budget {
	return formula.NewBudget(func() bool { return !t.check() })
}

type calcTurnKey struct{}

// context returns the context of the pass for work that looks at the gate
// through calcPause.
func (t *calcTurn) context() context.Context {
	return context.WithValue(t.ctx, calcTurnKey{}, t)
}

// calcPause is a check point for work that got the context of a pass. It
// reports false when the pass has to stop; ctx has ended then. Under any
// other context it does nothing.
func calcPause(ctx context.Context) bool {
	if t, _ := ctx.Value(calcTurnKey{}).(*calcTurn); t != nil {
		return t.check()
	}
	return true
}

// passContext returns a context for a pass a request started: it ends with
// the request and with the instance. Its Err reads the stop of the
// instance itself, so a check made after Stop sees it at once; Done closes
// once a forward of the stop has run, which a wait may see a little later.
func (in *Instance) passContext(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(in.ctx, func() {
		if hold := passForward.Load(); hold != nil {
			(*hold)()
		}
		cancel()
	})
	return passCtx{ctx, in.ctx}, func() {
		stop()
		cancel()
	}
}

// passCtx is the context of a pass: Err reports the end of the request or
// of the instance, whichever it finds first.
type passCtx struct {
	context.Context
	instance context.Context
}

func (c passCtx) Err() error {
	if err := c.Context.Err(); err != nil {
		return err
	}
	return c.instance.Err()
}

// passForward lets a test hold the forward of a stop to a pass context.
// It is nil outside tests.
var passForward atomic.Pointer[func()]
