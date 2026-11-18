package sheet

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// wantCalculating fails unless err is the refusal a caller answers by
// sending the call again, which is unavailable with the reason calculating
// and retry.
func wantCalculating(t *testing.T, what string, err error) {
	t.Helper()
	if werr := refusal(err); werr.Code != "unavailable" || werr.Reason != "calculating" || !werr.Retry {
		t.Fatalf("%s: %v, want unavailable with the reason calculating and retry", what, err)
	}
}

// calcDate returns the calculation date the sheet is at.
func (e *env) calcDate() string {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	return e.in.wb.date.String()
}

// sendCtx sends an action of the person under a context of the caller.
func (p *person) sendCtx(ctx context.Context, n, base uint64, op string) (ackBody, error) {
	raw := "[" + op + "]"
	var decoded []personOp
	if err := strictJSON([]byte(raw), &decoded); err != nil {
		p.e.t.Fatalf("ops JSON %s: %v", raw, err)
	}
	sum := sha256.Sum256([]byte(strconv.FormatUint(base, 10) + "\n" + raw))
	data, err := p.e.in.submit(ctx, p.s, n, base, decoded, sum[:])
	if err != nil {
		return ackBody{}, err
	}
	var ack ackBody
	if err := json.Unmarshal(data, &ack); err != nil {
		p.e.t.Fatal(err)
	}
	return ack, nil
}

const helloNamed = `{"type":"hello","version":1,"name":"Bo","pending":0}`

// TestRequestsDuringADateChange keeps the date change of a sheet at work
// and sends it what a sheet gets meanwhile. Nothing waits for the pass. A
// browser is told that the sheet calculates, a fresh agent read and an
// agent write are refused as calculating, the host's acknowledgement is
// too, and a request that is cancelled or runs out of time stops waiting.
// What was read before stays readable, from the previous state.
func TestRequestsDuringADateChange(t *testing.T) {
	e := longSheet(t)
	s := serve(e)
	tab := e.tabID("Sheet1")
	ctx := context.Background()
	editor := s.dial("private", helloV1)
	token, _ := editor.next("welcome")["session"].(string)
	// A paged read begins ten seconds before midnight, so its capture is
	// still kept when the date has changed.
	e.clock.Add(15*time.Hour - 10*time.Second)
	first := e.read(ReadQuery{Tab: "Sheet1", Range: "A1:B1", Limit: 1})
	next, _ := first["next"].(map[string]any)
	if next == nil {
		t.Fatalf("the first page has no cursor: %v", first)
	}
	day := e.calcDate()

	held := holdPasses(t)
	held.hold(e.in)
	guard := time.AfterFunc(10*shortBound, held.release)
	defer guard.Stop()
	e.clock.Add(20 * time.Second)
	timed(t, "the read that meets the new date", func() {
		_, err := e.in.Read(ctx, ReadQuery{Tab: "Sheet1", Range: "B1"})
		wantCalculating(t, "a fresh read", err)
	})
	held.reached(t, 1)
	timed(t, "the notice to an open connection", func() {
		if f := editor.next("status", "delta"); f["type"] != "status" || f["calculating"] != true {
			t.Fatalf("an open connection got %v during the date change", f)
		}
	})
	timed(t, "a read of the tab list", func() {
		_, err := e.in.Read(ctx, ReadQuery{})
		wantCalculating(t, "a read of the tab list", err)
	})
	timed(t, "a read as CSV", func() {
		_, err := e.in.Read(ctx, ReadQuery{Tab: "Sheet1", Range: "B1", Format: "csv"})
		wantCalculating(t, "a read as CSV", err)
	})
	timed(t, "the next page of the earlier read", func() {
		page := e.read(ReadQuery{Cursor: next["cursor"].(string)})
		if got := fmt.Sprint(page["rows"]); !strings.Contains(got, "value:-1") {
			t.Fatalf("the next page reads %s, want the previous value of B1", got)
		}
	})
	timed(t, "an agent write", func() {
		_, err := e.agent(`{"op_id":"late","ops":[{"op":"write","tab":"Sheet1","at":"E1","rows":[["late"]]}]}`)
		wantCalculating(t, "an agent write", err)
	})
	timed(t, "the acknowledgement of the host", func() {
		wantCalculating(t, "an acknowledgement", e.in.Acknowledge(ctx, 0, 0))
	})
	timed(t, "a rollback a page asks for", func() {
		req, _ := http.NewRequest(http.MethodPost, s.server.URL+"/private/_sheet/api/rollback", strings.NewReader(fmt.Sprintf(`{"revision":1,"session":%q}`, token)))
		req.Header.Set("X-Flats-Sheet", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out WriteError
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != http.StatusServiceUnavailable || out.Reason != "calculating" || !out.Retry {
			t.Fatalf("a rollback during the date change: status %d, %+v", resp.StatusCode, out)
		}
	})
	timed(t, "a request that is cancelled", func() {
		ctx, cancel := context.WithCancel(ctx)
		stop := time.AfterFunc(20*time.Millisecond, cancel)
		defer stop.Stop()
		if err := e.in.Barrier(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled request ended with %v", err)
		}
	})
	timed(t, "a request that runs out of time", func() {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		if err := e.in.Barrier(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a request past its deadline ended with %v", err)
		}
	})
	// Requests that come and go for as long as the pass runs take no
	// place in the writer's queue, which keeps room for those that follow.
	for i := 0; i < 2*cap(e.in.queue); i++ {
		ctx, cancel := context.WithTimeout(ctx, time.Millisecond)
		if err := e.in.Barrier(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("request %d that ran out of time ended with %v", i, err)
		}
		cancel()
	}
	if n := len(e.in.queue); n > 2 {
		t.Fatalf("%d requests that left are still in the writer's queue", n)
	}
	var late *client
	timed(t, "the notice to a new Private connection", func() {
		late = s.dial("private", helloNamed)
		if f := late.next("status", "welcome", "fatal"); f["type"] != "status" || f["calculating"] != true {
			t.Fatalf("a new Private connection got %v first", f)
		}
	})
	b1 := cellKey(e, "B1")
	timed(t, "a new Public connection", func() {
		c := s.dial("public", helloV1)
		c.next("welcome")
		if f := c.next("status", "tab_head", "fatal"); f["type"] != "status" || f["calculating"] != true {
			t.Fatalf("a new Public connection got %v after its welcome", f)
		}
		_, cells := c.watch(1, tab)
		if cell := cells[b1]; len(cell) < 3 || cell[2] != float64(-1) {
			t.Fatalf("a Public watch reads B1 = %v during the date change", cell)
		}
	})
	if got := e.calcDate(); got != day {
		t.Fatalf("the date moved to %s while its pass was held", got)
	}

	held.release()
	if f := editor.next("status"); f["calculating"] != false {
		t.Fatalf("after the date change an open connection got %v", f)
	}
	if f := late.next("status", "welcome", "fatal"); f["type"] != "status" || f["calculating"] != false {
		t.Fatalf("after the date change the new connection got %v", f)
	}
	late.next("welcome")
	out := e.read(ReadQuery{Tab: "Sheet1", Range: "B1", Include: []string{"shown"}})
	if got := fmt.Sprint(out["shown"]); got != "[["+longSum+"]]" {
		t.Fatalf("after the date change B1 shows %s", got)
	}
	// The refused write was not applied, and its op_id is free.
	e.want("E1", "")
	e.mustAgent(`{"op_id":"late","ops":[{"op":"write","tab":"Sheet1","at":"E1","rows":[["late"]]}]}`)
	e.want("E1", "late")
}

// TestWithdrawnWorkNeverRuns has work in the writer's queue when a date
// change begins: an action whose request is then cancelled, an action
// whose request stays, and an acknowledgement of the host. The cancelled
// action and the acknowledgement leave the queue with a definite answer
// and are not run when the pass has ended. The other action applies. The
// cancelled action's number stays free, and the action sent again applies
// once.
func TestWithdrawnWorkNeverRuns(t *testing.T) {
	e := longSheet(t)
	ctx := context.Background()
	alice, bob := e.person("Alice"), e.person("Bob")
	opA, opB, base := e.setOp("E1", "kept"), e.setOp("F1", "stays"), e.seq()
	held := holdPasses(t)
	held.hold(e.in)
	guard := time.AfterFunc(10*shortBound, held.release)
	defer guard.Stop()

	// The writer is kept at other work while the date change and what
	// follows it enter the queue.
	at, gate := make(chan struct{}), make(chan struct{})
	go e.in.execute(ctx, &work{run: func() error {
		close(at)
		<-gate
		return nil
	}})
	within(t, "the work that keeps the writer", at)
	e.clock.Add(24 * time.Hour)
	_, err := e.in.Read(ctx, ReadQuery{Tab: "Sheet1", Range: "B1"})
	wantCalculating(t, "the read that meets the new date", err)
	cancelled, cancel := context.WithCancel(ctx)
	left, stayed, acked := make(chan error, 1), make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := alice.sendCtx(cancelled, 1, base, opA)
		left <- err
	}()
	go func() {
		ack, err := bob.sendCtx(ctx, 1, base, opB)
		if err == nil && status(ack) != "applied" {
			err = fmt.Errorf("the action was %s", status(ack))
		}
		stayed <- err
	}()
	// Were it to run, the acknowledgement would empty the outbox.
	go func() { acked <- e.in.Acknowledge(ctx, 1<<40, 0) }()
	for deadline := time.Now().Add(30 * time.Second); len(e.in.queue) != 4; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d works are queued, want the date change and three more", len(e.in.queue))
		}
	}
	close(gate)
	held.reached(t, 1)

	timed(t, "the action whose request is cancelled", func() {
		cancel()
		if err := within(t, "the cancelled action", left); !errors.Is(err, context.Canceled) {
			t.Fatalf("the cancelled action ended with %v", err)
		}
	})
	timed(t, "the acknowledgement of the host", func() {
		wantCalculating(t, "an acknowledgement", within(t, "the acknowledgement", acked))
	})
	select {
	case err := <-stayed:
		t.Fatalf("the action whose request stays ended during the pass: %v", err)
	default:
	}
	var outbox int
	if err := e.in.rdb.QueryRow(`SELECT count(*) FROM outbox`).Scan(&outbox); err != nil || outbox == 0 {
		t.Fatalf("the outbox holds %d entries before the pass ends, %v", outbox, err)
	}

	held.release()
	if err := within(t, "the action whose request stays", stayed); err != nil {
		t.Fatal(err)
	}
	e.want("F1", "stays")
	e.want("E1", "")
	if got := alice.s.durable.Load(); got != 0 {
		t.Fatalf("the withdrawn action left the session at number %d", got)
	}
	var after int
	if err := e.in.rdb.QueryRow(`SELECT count(*) FROM outbox`).Scan(&after); err != nil || after != outbox {
		t.Fatalf("the outbox holds %d entries after the pass, want the %d the withdrawn acknowledgement left, %v", after, outbox, err)
	}
	base = e.seq()
	ack, err := alice.sendCtx(ctx, 1, base, opA)
	if err != nil || status(ack) != "applied" {
		t.Fatalf("the action sent again: %s, %v", status(ack), err)
	}
	seq := e.seq()
	again, err := alice.sendCtx(ctx, 1, base, opA)
	if err != nil || status(again) != "applied" || e.seq() != seq {
		t.Fatalf("the action sent a third time: %s, %v, sequence %d, want %d", status(again), err, e.seq(), seq)
	}
	e.want("E1", "kept")
}

// TestRequestsDuringARollback keeps the calculation of a rollback at work.
// The live sheet stays readable, a new connection is told that the sheet
// calculates, and an agent write and the host's acknowledgement are
// refused as calculating.
func TestRequestsDuringARollback(t *testing.T) {
	e := longSheet(t)
	target := e.b.importAll(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"E1","rows":[["newer"]]}]}`)
	e.b.importAll(t)
	s := serve(e)
	ctx := context.Background()
	held := holdPasses(t)
	held.hold(e.in)
	guard := time.AfterFunc(10*shortBound, held.release)
	defer guard.Stop()
	done := make(chan error, 1)
	go func() {
		_, err := e.b.Rollback(ctx, target, "Ann")
		done <- err
	}()
	held.reached(t, 1)
	var late *client
	timed(t, "the notice to a new Private connection", func() {
		late = s.dial("private", helloNamed)
		if f := late.next("status", "welcome", "fatal"); f["type"] != "status" || f["calculating"] != true {
			t.Fatalf("a new Private connection got %v first", f)
		}
	})
	timed(t, "an agent read", func() {
		out := e.read(ReadQuery{Tab: "Sheet1", Range: "E1", Include: []string{"shown"}})
		if got := fmt.Sprint(out["shown"]); got != "[[newer]]" {
			t.Fatalf("during the rollback E1 shows %s", got)
		}
	})
	timed(t, "an agent write", func() {
		_, err := e.agent(`{"ops":[{"op":"write","tab":"Sheet1","at":"F1","rows":[["late"]]}]}`)
		wantCalculating(t, "an agent write", err)
	})
	timed(t, "the acknowledgement of the host", func() {
		wantCalculating(t, "an acknowledgement", e.in.Acknowledge(ctx, 0, 0))
	})
	held.release()
	if err := within(t, "the rollback", done); err != nil {
		t.Fatal(err)
	}
	if f := late.next("status", "welcome", "fatal"); f["type"] != "status" || f["calculating"] != false {
		t.Fatalf("after the rollback the new connection got %v", f)
	}
	late.next("welcome")
	e.want("E1", "")
	e.want("F1", "")
}

// slowFormulas is fewer than half of passStride, so no loop of a date
// change looks at its deadline by the count of the formulas
// (numbersHeld), and the first check point of the pass comes in its
// evaluation.
const slowFormulas, slowTexts = 30, 20

// slowDateSheet returns a sheet whose next change of the date is real work:
// formulas that each match a wildcard pattern against 20 texts of 32,000
// characters once the test's first day is over. Until then every formula
// is 0 and costs nothing.
func slowDateSheet(t *testing.T) *env {
	t.Helper()
	e := start(t)
	text := `["` + strings.Repeat("a", 32000) + `"],`
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + strings.TrimSuffix(strings.Repeat(text, slowTexts), ",") + `]}]}`)
	formula := fmt.Sprintf(`["=IF(TODAY()>DATE(2026,11,13),COUNTIF(A$1:A$%d,\"*a?b*\"),0)"],`, slowTexts)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[` + strings.TrimSuffix(strings.Repeat(formula, slowFormulas), ",") + `]}]}`)
	e.want("B1", "0")
	return e
}

// TestRealDateChangeHoldsNoRequest changes the date of a sheet whose pass
// is real calculation, and holds the pass at the first check point its
// wildcard matching brings it to, as a pass with minutes of such work left
// would be. A read is refused as calculating at once and again later, the
// open browser is told and keeps the previous state, new connections are
// told, a cancelled request returns, and a stop of the instance ends the
// pass where it is held: a stop that waited for the pass would wait until
// the test lets it go.
func TestRealDateChangeHoldsNoRequest(t *testing.T) {
	e := slowDateSheet(t)
	s := serve(e)
	tab := e.tabID("Sheet1")
	ctx := context.Background()
	editor := s.dial("private", helloV1)
	editor.next("welcome")
	day := e.calcDate()
	held := holdPasses(t)
	held.hold(e.in)
	// Should a request wait for the pass, the pass is let go after a
	// minute, so that the test ends and reports it.
	guard := time.AfterFunc(time.Minute, held.release)
	defer guard.Stop()
	e.clock.Add(24 * time.Hour)
	timed(t, "the read that meets the new date", func() {
		_, err := e.in.Read(ctx, ReadQuery{Tab: "Sheet1", Range: "B1"})
		wantCalculating(t, "a fresh read", err)
	})
	held.reached(t, 1)
	timed(t, "the notice to the open connection", func() {
		if f := editor.next("status", "delta"); f["type"] != "status" || f["calculating"] != true {
			t.Fatalf("the open connection got %v during the date change", f)
		}
	})
	timed(t, "a second read", func() {
		_, err := e.in.Read(ctx, ReadQuery{Tab: "Sheet1", Range: "B1"})
		wantCalculating(t, "a second read", err)
	})
	timed(t, "a request that is cancelled", func() {
		ctx, cancel := context.WithCancel(ctx)
		stop := time.AfterFunc(20*time.Millisecond, cancel)
		defer stop.Stop()
		if err := e.in.Barrier(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled request ended with %v", err)
		}
	})
	b1 := cellKey(e, "B1")
	timed(t, "a watch of the open connection", func() {
		_, cells := editor.watch(1, tab)
		if cell := cells[b1]; len(cell) < 3 || cell[2] != float64(0) {
			t.Fatalf("a watch reads B1 = %v during the date change, want the previous value", cell)
		}
	})
	timed(t, "the notice to a new Private connection", func() {
		c := s.dial("private", helloNamed)
		if f := c.next("status", "welcome", "fatal"); f["type"] != "status" || f["calculating"] != true {
			t.Fatalf("a new Private connection got %v first", f)
		}
	})
	timed(t, "the notice to a new Public connection", func() {
		c := s.dial("public", helloV1)
		c.next("welcome")
		if f := c.next("status", "tab_head", "fatal"); f["type"] != "status" || f["calculating"] != true {
			t.Fatalf("a new Public connection got %v after its welcome", f)
		}
	})
	if got := e.calcDate(); got != day {
		t.Fatalf("the date change ended while its pass was held: the sheet is at %s", got)
	}
	e.in.Stop()
	if !guard.Stop() {
		t.Fatal("a request or the stop waited until the pass was let go")
	}
	if got := e.in.wb.date.String(); got != day {
		t.Fatalf("the stopped sheet is at %s, want its previous date %s", got, day)
	}
	// The budget looks at the deadline once its work reaches its interval:
	// a pass held with less work was held before its wildcard matching.
	work := e.in.lastWork.Load()
	t.Logf("the stopped pass had counted %d units of work", work)
	if work < budgetCheck {
		t.Fatalf("the stopped pass counted %d units of work: it was held at a check point before its evaluation", work)
	}
}

// countingBackend counts the revision files an instance reads and runs a
// function at each read.
type countingBackend struct {
	*fakeBackend
	reads  int
	onRead func()
}

func (b *countingBackend) OpenBlob(ctx context.Context, hash string) (io.ReadCloser, error) {
	b.reads++
	b.onRead()
	return b.fakeBackend.OpenBlob(ctx, hash)
}

// TestCancelledRollbackStopsReading cancels the request of a rollback
// while the writer reads the first file of the revision. The rollback ends
// there. It reads no further file, and the sheet stays as it was.
func TestCancelledRollbackStopsReading(t *testing.T) {
	e := longSheet(t)
	target := e.b.importAll(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"E1","rows":[["newer"]]}]}`)
	e.b.importAll(t)
	files, _, release, err := e.b.RevisionFiles(context.Background(), target)
	if err != nil || len(files) < 3 {
		t.Fatalf("the revision has %d files, %v", len(files), err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	counting := &countingBackend{fakeBackend: e.b, onRead: cancel}
	e.in.Stop()
	in, _, err := Open(e.dir, counting, e.opts)
	if err != nil {
		t.Fatal(err)
	}
	e.in, e.b.in = in, in
	if err := in.await(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	seq := e.seq()
	if err := e.in.Rollback(ctx, files, "Ann", "rollback-cancelled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled rollback ended with %v", err)
	}
	if counting.reads != 1 {
		t.Fatalf("the rollback read %d of %d files after its request was cancelled at the first", counting.reads, len(files))
	}
	if e.seq() != seq {
		t.Fatalf("the cancelled rollback moved the sheet to sequence %d", e.seq())
	}
	e.want("E1", "newer")
}

// TestHostCallsDuringALoad opens a sheet whose load has to wait and sends
// it what the host sends. An acknowledgement and a rollback are refused as
// calculating, and a call with a deadline ends at it. After the load the
// same acknowledgement applies.
func TestHostCallsDuringALoad(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[20,"=A1*2+1"]]}]}`)
	target := e.b.importAll(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[30]]}]}`)
	files, _, done, err := e.b.RevisionFiles(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	e.in.Stop()

	release := holdCalculations(t)
	in, _, err := Open(e.dir, e.b, e.opts)
	if err != nil {
		t.Fatal(err)
	}
	e.in, e.b.in = in, in
	ctx := context.Background()
	timed(t, "an acknowledgement during the load", func() {
		wantCalculating(t, "an acknowledgement", in.Acknowledge(ctx, 1<<40, 0))
	})
	timed(t, "a rollback during the load", func() {
		wantCalculating(t, "a rollback", in.Rollback(ctx, files, "Ann", "rollback-early"))
	})
	timed(t, "a call with a deadline during the load", func() {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		if err := in.Barrier(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a call past its deadline ended with %v", err)
		}
	})
	if !in.Stats().Calculating {
		t.Fatal("the loading sheet does not report that it calculates")
	}
	release()
	if err := in.await(ctx, 0); err != nil {
		t.Fatal(err)
	}
	e.want("A1", "30")
	var outbox int
	if err := in.rdb.QueryRow(`SELECT count(*) FROM outbox`).Scan(&outbox); err != nil || outbox == 0 {
		t.Fatalf("the refused acknowledgement left %d outbox entries, %v", outbox, err)
	}
	if err := in.Acknowledge(ctx, 1<<40, 0); err != nil {
		t.Fatal(err)
	}
	if in.Stats().Calculating {
		t.Fatal("the loaded sheet still reports that it calculates")
	}
}

// TestPagesThatLeaveStopWaiting opens Private connections while the sheet
// loads and while a date change is at work, and closes them before they
// are admitted. Nothing reads such a connection until its welcome, so the
// pings of the connection find that the page left. The connections end
// and give their places back, long before the load or the pass ends.
func TestPagesThatLeaveStopWaiting(t *testing.T) {
	ping := livePing
	livePing = 20 * time.Millisecond
	t.Cleanup(func() { livePing = ping })
	e := longSheet(t)
	s := serve(e)
	leave := func(what string) {
		t.Helper()
		c := s.dial("private", helloNamed)
		if f := c.next("status", "welcome", "fatal"); f["type"] != "status" || f["calculating"] != true {
			t.Fatalf("%s: the connection got %v first", what, f)
		}
		c.ws.Close(1000, "")
		timed(t, what, func() {
			for deadline := time.Now().Add(shortBound); e.in.hub.count() != 0; time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("%s: %d connections remain", what, e.in.hub.count())
				}
			}
		})
	}

	e.in.Stop()
	release := holdCalculations(t)
	in, _, err := Open(e.dir, e.b, e.opts)
	if err != nil {
		t.Fatal(err)
	}
	e.in, e.b.in = in, in
	leave("a page that leaves during the load")
	if !in.calculating() {
		t.Fatal("the load ended while it was held")
	}
	release()
	if err := in.await(context.Background(), 0); err != nil {
		t.Fatal(err)
	}

	held := holdPasses(t)
	held.hold(e.in)
	guard := time.AfterFunc(10*shortBound, held.release)
	defer guard.Stop()
	e.clock.Add(24 * time.Hour)
	_, err = e.in.Read(context.Background(), ReadQuery{Tab: "Sheet1", Range: "B1"})
	wantCalculating(t, "the read that meets the new date", err)
	held.reached(t, 1)
	for deadline := time.Now().Add(shortBound); !e.in.hub.calc.Load(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the browsers were not told that the sheet calculates")
		}
	}
	leave("a page that leaves while it waits for its session")
	if !e.in.calculating() {
		t.Fatal("the date change ended while it was held")
	}
}
