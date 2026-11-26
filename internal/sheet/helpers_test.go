package sheet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// fakeBackend is the host of a test: blob store, revisions and quota.
type fakeBackend struct {
	mu    sync.Mutex
	in    *Instance
	quota int64
	other int64
	// walking, when set, runs in every measurement after it read other.
	walking  func()
	measured int // measurements that read other
	// stuck makes a measurement wait until its context ends.
	stuck  bool
	blobs  map[string][]byte
	revs   []fakeRevision
	cursor int64
	pinned map[int]bool
}

type fakeRevision struct {
	entry OutboxEntry
}

func newBackend() *fakeBackend {
	return &fakeBackend{blobs: map[string][]byte{}, pinned: map[int]bool{}}
}

// entry returns the outbox entry revision n was recorded from.
func (b *fakeBackend) entry(n int) OutboxEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.revs[n-1].entry
}

func (b *fakeBackend) setQuota(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.quota = n
}

func (b *fakeBackend) isPinned(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pinned[n]
}

func (b *fakeBackend) Limits() Limits {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Limits{DiskQuota: b.quota}
}

// MeasureOther reads the usage and then calls walking, when a test set it:
// a walk of the directory that takes time after it has read what it
// reports.
func (b *fakeBackend) MeasureOther(ctx context.Context) (int64, error) {
	b.mu.Lock()
	other, walking, stuck := b.other, b.walking, b.stuck
	b.measured++
	b.mu.Unlock()
	if stuck {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if walking != nil {
		walking()
	}
	return other, nil
}

func (b *fakeBackend) setOther(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.other = n
}

func (b *fakeBackend) PutBlob(_ context.Context, hash string, data []byte) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.blobs[hash]; ok {
		return false, nil
	}
	b.blobs[hash] = append([]byte(nil), data...)
	return true, nil
}

func (b *fakeBackend) ReadBlob(_ context.Context, hash string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.blobs[hash]
	if !ok {
		return nil, fmt.Errorf("blob %s is missing", hash)
	}
	return data, nil
}

// OpenBlob reads a blob as the host's reader does: each read stops with
// ctx.
func (b *fakeBackend) OpenBlob(ctx context.Context, hash string) (io.ReadCloser, error) {
	data, err := b.ReadBlob(ctx, hash)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(ctxReader{ctx, bytes.NewReader(data)}), nil
}

// ctxReader is a reader that ends with its context.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func (b *fakeBackend) Lease([]string) func() { return func() {} }

func (b *fakeBackend) Exclusive(ctx context.Context, fn func(context.Context, int64) error) error {
	b.mu.Lock()
	other := b.other
	b.mu.Unlock()
	return fn(ctx, other)
}

func (b *fakeBackend) OutboxReady() {}

// importAll imports the outbox as core does: store new blobs, record the
// revisions, acknowledge. It returns the number of the newest revision.
func (b *fakeBackend) importAll(t testing.TB) int {
	t.Helper()
	ctx := context.Background()
	in := b.in
	if err := in.Barrier(ctx); err != nil {
		t.Fatal(err)
	}
	entries, err := in.Outbox(ctx, b.cursor, 0)
	if err != nil {
		t.Fatal(err)
	}
	var stored int64
	for _, e := range entries {
		for _, hash := range e.NewHashes {
			data, err := in.SnapshotData(ctx, hash)
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != hash {
				t.Fatalf("snapshot %s has hash %s", hash, got)
			}
			if created, _ := b.PutBlob(ctx, hash, data); created {
				stored += int64(len(data))
			}
		}
		for _, f := range e.Files {
			if _, err := b.ReadBlob(ctx, f.Hash); err != nil {
				t.Fatalf("revision file %s: %v", f.Path, err)
			}
		}
		b.mu.Lock()
		b.revs = append(b.revs, fakeRevision{e})
		b.cursor = e.Seq
		b.mu.Unlock()
	}
	if len(entries) > 0 {
		if err := in.Acknowledge(ctx, b.cursor, stored); err != nil {
			t.Fatal(err)
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.revs)
}

func (b *fakeBackend) Revisions(_ context.Context, before, limit int) (RevisionPage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var page RevisionPage
	for n := len(b.revs); n >= 1 && len(page.Revisions) < limit; n-- {
		if before != 0 && n >= before {
			continue
		}
		e := b.revs[n-1].entry
		page.Revisions = append(page.Revisions, RevisionInfo{Number: n, Kind: e.Kind, Authors: e.Authors, Message: e.Message,
			CreatedAt: time.Unix(e.CreatedAt, 0).UTC(), Pinned: b.pinned[n], Tabs: []RevisionTab{}})
	}
	return page, nil
}

func (b *fakeBackend) RevisionFiles(_ context.Context, n int) ([]File, string, func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n < 1 || n > len(b.revs) {
		return nil, "", nil, &WriteError{Code: "not_found", Reason: "revision"}
	}
	e := b.revs[n-1].entry
	return e.Files, e.CalcDate, func() {}, nil
}

func (b *fakeBackend) Pin(_ context.Context, n int, pinned bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pinned[n] = pinned
	return nil
}

func (b *fakeBackend) Rollback(ctx context.Context, n int, label string) (int, error) {
	files, _, release, err := b.RevisionFiles(ctx, n)
	if err != nil {
		return 0, err
	}
	defer release()
	b.mu.Lock()
	in := b.in
	b.mu.Unlock()
	if err := in.Rollback(ctx, files, label, "rollback-"+strconv.Itoa(n)+"-"+strconv.FormatInt(time.Now().UnixNano(), 36)); err != nil {
		return 0, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.revs) + 1, nil
}

// clock is a settable clock for an instance.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type env struct {
	t     testing.TB
	dir   string
	b     *fakeBackend
	in    *Instance
	clock *clock
	opts  Options
}

// start opens a sheet in a new directory and waits until it is loaded.
func start(t testing.TB, mods ...func(*Options)) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir(), b: newBackend(), clock: newClock()}
	e.opts = Options{Slug: "sheet", Now: e.clock.Now, Location: time.UTC}
	for _, mod := range mods {
		mod(&e.opts)
	}
	e.open()
	t.Cleanup(func() { e.in.Stop() })
	return e
}

// unhurried gives the actions of an instance an hour instead of the
// default 10 s. A test whose actions must apply, or be refused for another
// reason than the time, starts with it: a busy machine can stretch a large
// action past 10 s of the clock.
func unhurried(o *Options) { o.RecalcLimit = time.Hour }

func (e *env) open() {
	e.t.Helper()
	in, _, err := Open(e.dir, e.b, e.opts)
	if err != nil {
		e.t.Fatal(err)
	}
	e.in, e.b.in = in, in
	if err := in.await(context.Background(), 0); err != nil {
		e.t.Fatal(err)
	}
}

// setFault sets the hook that fails the commit of sheet.db. The writer reads it
// in every commit, also in the commit of a maintenance tick that no call of
// the test orders, so the test sets it with the instance's lock held.
func (e *env) setFault(f func() error) {
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	e.in.fault = f
}

// restart stops the instance and opens its directory again.
func (e *env) restart() {
	e.t.Helper()
	e.in.Stop()
	e.open()
}

func (e *env) seq() uint64 {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	return e.in.wb.seq
}

func (e *env) state() string {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	return stateToken(e.in.wb.store, e.in.wb.seq)
}

// agent sends an update_sheet call with the current state unless the JSON
// carries one.
func (e *env) agent(call string) (WriteResult, error) {
	e.t.Helper()
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(call), &probe); err != nil {
		e.t.Fatalf("call JSON: %v", err)
	}
	if _, ok := probe["state"]; !ok {
		probe["state"], _ = json.Marshal(e.state())
	} else if string(probe["state"]) == `""` {
		delete(probe, "state")
	}
	raw, _ := json.Marshal(probe)
	u, err := DecodeUpdate(raw)
	if err != nil {
		return WriteResult{}, err
	}
	return e.in.Update(context.Background(), u, e.b.other)
}

// calculated calls fn again while the sheet answers that it calculates, as
// an agent sends a refused call again. A call that meets a new date is
// refused when the date change takes longer than the call waits, and a
// refused call was not applied.
func calculated[T any](fn func() (T, error)) (T, error) {
	for deadline := time.Now().Add(10 * time.Minute); ; time.Sleep(5 * time.Millisecond) {
		v, err := fn()
		if err == nil || refusal(err).Reason != "calculating" || time.Now().After(deadline) {
			return v, err
		}
	}
}

func (e *env) mustAgent(call string) WriteResult {
	e.t.Helper()
	res, err := calculated(func() (WriteResult, error) { return e.agent(call) })
	if err != nil {
		e.t.Fatalf("agent call: %v", err)
	}
	return res
}

func refusal(err error) *WriteError {
	var werr *WriteError
	if errors.As(err, &werr) {
		return werr
	}
	return &WriteError{Code: "error", Reason: fmt.Sprint(err)}
}

// at resolves an address such as "Budget!B2" or "B2" (first tab).
func (e *env) at(addr string) (*Tab, RowID, ColID) {
	e.t.Helper()
	r, err := formula.ParseRange(addr)
	if err != nil {
		e.t.Fatalf("address %q: %v", addr, err)
	}
	wb := e.in.wb
	t := wb.tabs[0]
	if r.Tab != "" {
		if t = wb.tab(r.Tab); t == nil {
			e.t.Fatalf("no tab %q", r.Tab)
		}
	}
	if int(r.First.Row) >= t.rows.len() || int(r.First.Col) >= t.cols.len() {
		e.t.Fatalf("address %q is outside the tab", addr)
	}
	return t, RowID(t.rows.ids[r.First.Row]), ColID(t.cols.ids[r.First.Col])
}

// shown returns what a cell shows under its number format (spec rule 6).
func (wb *Workbook) shown(c *Cell) string {
	return string(formula.AppendShow(nil, c.val, showKind(c.content, c.f), wb.styles.format(c.style).Number))
}

// shown returns what the cell at an address shows.
func (e *env) shown(addr string) string {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t, row, col := e.at(addr)
	c := t.cell(row, col)
	if c == nil {
		return ""
	}
	return e.in.wb.shown(c)
}

// text returns the content of the cell as the formula bar shows it.
func (e *env) text(addr string) string {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t, row, col := e.at(addr)
	c := t.cell(row, col)
	if c == nil {
		return ""
	}
	pos, _ := t.posOf(row, col)
	if c.f != nil {
		return c.f.Text(pos)
	}
	return formula.EditText(c.content, pos)
}

func (e *env) want(addr, shown string) {
	e.t.Helper()
	if got := e.shown(addr); got != shown {
		e.t.Errorf("%s shows %q, want %q", addr, got, shown)
	}
}

func (e *env) wantText(addr, text string) {
	e.t.Helper()
	if got := e.text(addr); got != text {
		e.t.Errorf("%s holds %q, want %q", addr, got, text)
	}
}

// person is a simulated editor: a session that sends actions.
type person struct {
	e *env
	s *session
	n uint64
}

func (e *env) person(name string) *person {
	e.t.Helper()
	s, _, err := e.in.newSession(context.Background(), "editor:"+name, name, false)
	if err != nil {
		e.t.Fatal(err)
	}
	return &person{e: e, s: s}
}

// act sends one action with the given base and returns its ack.
func (p *person) act(base uint64, ops ...string) ackBody {
	p.e.t.Helper()
	ack, err := p.try(base, ops...)
	if err != nil {
		p.e.t.Fatalf("action: %v", err)
	}
	return ack
}

func (p *person) try(base uint64, ops ...string) (ackBody, error) {
	p.e.t.Helper()
	p.n++
	return p.send(p.n, base, ops...)
}

func (p *person) send(n, base uint64, ops ...string) (ackBody, error) {
	p.e.t.Helper()
	raw := "[" + strings.Join(ops, ",") + "]"
	var decoded []personOp
	if err := strictJSON([]byte(raw), &decoded); err != nil {
		p.e.t.Fatalf("ops JSON %s: %v", raw, err)
	}
	sum := sha256.Sum256([]byte(strconv.FormatUint(base, 10) + "\n" + raw))
	data, err := p.e.in.submit(context.Background(), p.s, n, base, decoded, sum[:])
	if err != nil {
		return ackBody{}, err
	}
	var ack ackBody
	if err := json.Unmarshal(data, &ack); err != nil {
		p.e.t.Fatal(err)
	}
	return ack, nil
}

// do sends an action against the current state.
func (p *person) do(ops ...string) ackBody {
	p.e.t.Helper()
	return p.act(p.e.seq(), ops...)
}

// setOp builds a set operation for an address as the screen shows it now.
func (e *env) setOp(addr, text string) string {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t, row, col := e.at(addr)
	pos, _ := t.posOf(row, col)
	data, _ := json.Marshal(text)
	return fmt.Sprintf(`{"op":"set","tab":%d,"row":%d,"col":%d,"text":%s,"at":[%d,%d]}`, t.id, row, col, data, pos.Row, pos.Col)
}

func (p *person) set(addr, text string) ackBody {
	p.e.t.Helper()
	return p.do(p.e.setOp(addr, text))
}

// ids returns the tab id and the row and column id of an address.
func (e *env) ids(addr string) (TabID, RowID, ColID) {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t, row, col := e.at(addr)
	return t.id, row, col
}

func (e *env) tabID(name string) TabID {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t := e.in.wb.tab(name)
	if t == nil {
		e.t.Fatalf("no tab %q", name)
	}
	return t.id
}

func status(ack ackBody) string {
	parts := make([]string, len(ack.Results))
	for i, r := range ack.Results {
		parts[i] = r.Status
		if r.Reason != "" {
			parts[i] += ":" + r.Reason
		}
	}
	return strings.Join(parts, ",")
}

func (e *env) read(q ReadQuery) map[string]any {
	e.t.Helper()
	data, err := calculated(func() (json.RawMessage, error) { return e.in.Read(context.Background(), q) })
	if err != nil {
		e.t.Fatalf("read: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		e.t.Fatalf("read result %s: %v", data, err)
	}
	return out
}

func jsonf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

func (e *env) axisIDs(tab string, rows bool, from, count int) string {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t := e.in.wb.tab(tab)
	if t == nil {
		e.t.Fatalf("no tab %q", tab)
	}
	a := &t.cols
	if rows {
		a = &t.rows
	}
	data, _ := json.Marshal(idRuns(a.ids[from-1 : from-1+count]))
	return string(data)
}

// rowsOf returns the id runs of count rows from the 1-based row number.
func (e *env) rowsOf(tab string, from, count int) string { return e.axisIDs(tab, true, from, count) }

// colsOf returns the id runs of count columns from the 1-based number.
func (e *env) colsOf(tab string, from, count int) string { return e.axisIDs(tab, false, from, count) }

func (e *env) insertRowsOp(tab string, before, count int) string {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t := e.in.wb.tab(tab)
	return fmt.Sprintf(`{"op":"insert_rows","tab":%d,"beside":%d,"side":"before","count":%d}`, t.id, t.rows.ids[before-1], count)
}

func (e *env) insertColsOp(tab string, before, count int) string {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t := e.in.wb.tab(tab)
	return fmt.Sprintf(`{"op":"insert_cols","tab":%d,"beside":%d,"side":"before","count":%d}`, t.id, t.cols.ids[before-1], count)
}

func (e *env) deleteRowsOp(tab string, from, count int) string {
	return fmt.Sprintf(`{"op":"delete_rows","tab":%d,"ids":%s}`, e.tabID(tab), e.rowsOf(tab, from, count))
}

func (e *env) deleteColsOp(tab string, from, count int) string {
	return fmt.Sprintf(`{"op":"delete_cols","tab":%d,"ids":%s}`, e.tabID(tab), e.colsOf(tab, from, count))
}

// sortOp sorts count rows from the 1-based row by the 1-based column.
func (e *env) sortOp(tab string, from, count, by int, desc bool) string {
	e.t.Helper()
	rows := e.rowsOf(tab, from, count)
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t := e.in.wb.tab(tab)
	return fmt.Sprintf(`{"op":"sort","tab":%d,"rows":%s,"by":%d,"desc":%v}`, t.id, rows, t.cols.ids[by-1], desc)
}

// column returns what the cells of a column show, top down.
func (e *env) column(tab, col string, from, to int) []string {
	e.t.Helper()
	out := make([]string, 0, to-from+1)
	for row := from; row <= to; row++ {
		out = append(out, e.shown(fmt.Sprintf("%s!%s%d", tab, col, row)))
	}
	return out
}

func (e *env) wantRefusal(err error, code, reason string) {
	e.t.Helper()
	werr := refusal(err)
	if err == nil || werr.Code != code || (werr.Reason != reason && werr.Limit != reason) {
		e.t.Errorf("refusal = %v, want %s %s", err, code, reason)
	}
}

// recover restores a tab of a revision for a person.
func (p *person) recover(revision int, tab TabID) (TabID, error) {
	files, _, release, err := p.e.b.RevisionFiles(context.Background(), revision)
	if err != nil {
		return 0, err
	}
	defer release()
	return p.e.in.recoverTab(context.Background(), p.s, "", files, tab)
}

func (e *env) tabNames() []string {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	names := make([]string, len(e.in.wb.tabs))
	for i, t := range e.in.wb.tabs {
		names[i] = t.name
	}
	return names
}

func parseDay(date string) (time.Time, error) {
	day, err := time.Parse("2006-01-02", date)
	return day.Add(9 * time.Hour), err
}

// sameJSON compares two JSON values, numbers at 15 significant digits.
func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return len(a) == 0 && string(b) == "null" || string(a) == string(b)
	}
	fx, okx := x.(float64)
	fy, oky := y.(float64)
	if okx && oky {
		return strconv.FormatFloat(fx, 'g', 15, 64) == strconv.FormatFloat(fy, 'g', 15, 64)
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return string(ja) == string(jb)
}

// tick runs the maintenance tick on the writer, as the ticker would.
func (e *env) tick() {
	e.t.Helper()
	err := e.in.execute(context.Background(), &work{run: func() error {
		e.in.maintain()
		return nil
	}})
	if err != nil {
		e.t.Fatal(err)
	}
}

// collectBlanks runs one maintenance tick at a limit of blank records, 0
// for the sheet's own, and then raises the limit to what the tick left.
// The instance also ticks by itself every few seconds, and each tick
// removes the older half again while more than the limit are left
// (technical design, section 7.5). A test that compares the sheet before
// and after an action would otherwise see that tick among the action's
// changes. The raised limit holds until the next call.
func (e *env) collectBlanks(limit int) {
	e.t.Helper()
	// The writer runs one tick at a time, so the instance's own cannot
	// come between this one and the new limit.
	err := e.in.execute(context.Background(), &work{run: func() error {
		e.in.mu.Lock()
		e.in.wb.collected.limit = limit
		e.in.mu.Unlock()
		e.in.maintain()
		e.in.mu.Lock()
		wb := e.in.wb
		wb.collected.limit = max(wb.count.blanks, wb.blankLimit())
		e.in.mu.Unlock()
		return nil
	}})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) readRevision(n int, q ReadQuery) map[string]any {
	e.t.Helper()
	files, date, release, err := e.b.RevisionFiles(context.Background(), n)
	if err != nil {
		e.t.Fatal(err)
	}
	defer release()
	var data json.RawMessage
	// The revision workbook is built in the background; a reader that
	// comes too early is refused as calculating and retries.
	for {
		data, err = e.in.ReadRevision(context.Background(), n, files, date, q)
		if err == nil {
			break
		}
		if werr := refusal(err); werr.Reason != "calculating" {
			e.t.Fatalf("read revision %d: %v", n, err)
		}
		e.in.revs.mu.Lock()
		var ready chan struct{}
		for _, b := range e.in.revs.list {
			if b.n == n {
				ready = b.ready
			}
		}
		e.in.revs.mu.Unlock()
		if ready != nil {
			<-ready
		}
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func firstCell(out map[string]any) any {
	rows, _ := out["rows"].([]any)
	if len(rows) == 0 {
		return nil
	}
	row, _ := rows[0].([]any)
	if len(row) == 0 {
		return nil
	}
	return row[0]
}

// fullSize says that the full-size memory and volume cases run: the ones
// that write hundreds of MiB of undo payloads, hold a sheet at one of the
// largest limits or measure the reference sheet. FLATS_SHEET_FULL=1 sets
// it, as one lane of CI does (scripts/ci-test.sh). Where a case has a
// smaller size that takes the same path, that size runs by default.
var fullSize = os.Getenv("FLATS_SHEET_FULL") == "1"

// full skips a full-size case unless FLATS_SHEET_FULL=1. Like heavy, it
// never runs in short mode or under the race detector.
func full(t *testing.T, what string) {
	t.Helper()
	if !fullSize || testing.Short() || raceEnabled {
		t.Skip(what + "; set FLATS_SHEET_FULL=1 to run it, without -short and without the race detector")
	}
}

// sized returns the size of a case: the full one when the full-size cases
// run, and the smaller one of the default run otherwise.
func sized(full, small int) int {
	if fullSize {
		return full
	}
	return small
}

// measures says whether a run can assert what the heap holds, what a step
// allocates or how long it takes. The race detector keeps shadow memory
// for every allocation and slows every memory access several times over,
// so under it such a number says nothing about the code. A test that has
// functional assertions beside them runs under the race detector, at its
// smaller sizes, and leaves only those assertions out.
const measures = !raceEnabled

// slow skips a test that takes long in short mode. Unlike heavy it lets
// the test run under the race detector.
func slow(t *testing.T, what string) {
	t.Helper()
	if testing.Short() {
		t.Skip(what + "; not run in short mode")
	}
}

// raced returns small under the race detector and n otherwise: the size
// for a case whose functional assertions hold at any size.
func raced(n, small int) int {
	if raceEnabled {
		return small
	}
	return n
}

// heavy skips a test that builds one of the largest fixtures in short mode
// and under the race detector, where a full recalculation is slow enough
// to meet the edit time limit. These tests run in the ordinary test run.
func heavy(t *testing.T, what string) {
	t.Helper()
	if testing.Short() || raceEnabled {
		t.Skip(what + "; not run in short mode or under the race detector")
	}
}

// sheetRead is the part of a Read result the tests look at.
type sheetRead struct {
	Tabs []struct {
		ID   uint64 `json:"id"`
		Name string `json:"name"`
		Rows int    `json:"rows"`
		Cols int    `json:"cols"`
	} `json:"tabs"`
	Rows    [][]json.RawMessage `json:"rows"`
	Shown   [][]string          `json:"shown"`
	Formats []struct {
		Range  string          `json:"range"`
		Format json.RawMessage `json:"format"`
	} `json:"formats"`
}

// readSheet reads the live sheet through Read, as an agent does.
func (e *env) readSheet(q ReadQuery) sheetRead {
	e.t.Helper()
	data, err := e.in.Read(context.Background(), q)
	if err != nil {
		e.t.Fatalf("read %+v: %v", q, err)
	}
	var out sheetRead
	if err := json.Unmarshal(data, &out); err != nil {
		e.t.Fatalf("read %+v: %v in %s", q, err, data)
	}
	return out
}

// formatOf is the format a read reports for one cell, as JSON.
func (e *env) formatOf(tab, addr string) string {
	e.t.Helper()
	for _, f := range e.readSheet(ReadQuery{Tab: tab, Range: addr, Include: []string{"formats"}}).Formats {
		return string(f.Format)
	}
	return "{}"
}
