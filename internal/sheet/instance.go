// Package sheet is the sheet content type: a collaborative spreadsheet
// held in memory by one Instance per sheet, durable in sheet.db, calculated
// by the formula package and served to browsers over a WebSocket and to
// agents through Read and Update.
package sheet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
	"github.com/gosuda/flats/internal/sqliteuri"
)

// Limits are the host's current per-flat storage limits.
type Limits struct{ DiskQuota int64 }

// Backend supplies host storage without exposing its locks to the writer.
type Backend interface {
	Limits() Limits
	MeasureOther(context.Context) (int64, error)
	PutBlob(ctx context.Context, hash string, data []byte) (bool, error)
	// OpenBlob returns a reader of the blob of hash, which honors ctx and
	// reports an error at the end of a blob whose bytes do not match the
	// hash. The caller closes it.
	OpenBlob(ctx context.Context, hash string) (io.ReadCloser, error)
	Lease(hashes []string) func()
	Exclusive(ctx context.Context, fn func(context.Context, int64) error) error
	OutboxReady()
}

// HistoryBackend supplies retained revisions under host-owned leases. A
// Backend that also implements it enables the History routes.
type HistoryBackend interface {
	Revisions(ctx context.Context, before, limit int) (RevisionPage, error)
	RevisionFiles(ctx context.Context, n int) (files []File, date string, release func(), err error)
	Pin(ctx context.Context, n int, pinned bool) error
	Rollback(ctx context.Context, n int, label string) (int, error)
}

// RevisionInfo is one revision in the History list.
type RevisionInfo struct {
	Number     int           `json:"number"`
	Kind       string        `json:"kind"`
	Authors    []string      `json:"authors"`
	Message    string        `json:"message,omitempty"`
	CreatedAt  time.Time     `json:"created"`
	Pinned     bool          `json:"pinned"`
	RollbackOf int           `json:"rollback_of,omitempty"`
	Tabs       []RevisionTab `json:"tabs"`
}

// RevisionTab is a tab a revision changed (spec rule 51).
type RevisionTab struct {
	ID         uint64 `json:"id"`
	Name       string `json:"name"`   // the tab's name in the revision, or its last name when removed
	Change     string `json:"change"` // "added", "removed" or "changed"
	NameBefore string `json:"name_before,omitempty"`
	NameAfter  string `json:"name_after,omitempty"`
}

// RevisionPage is one page of the History list, newest first.
type RevisionPage struct {
	Revisions []RevisionInfo `json:"revisions"`
	Next      int            `json:"next,omitempty"`
}

// Options supplies initialization and host clock and logging facilities.
type Options struct {
	Slug string
	Now  func() time.Time
	Log  *slog.Logger
	// Location is the host's time zone, which sets the calculation date.
	Location *time.Location
	// ImportStore and ImportSeq are the import cursor flats.db holds for
	// this flat, empty when it has none. Open refuses a sheet.db with that
	// store id whose outbox never reached ImportSeq.
	ImportStore string
	ImportSeq   int64
	// RecalcLimit is the time limit of the recalculation an edit starts.
	// Zero means the default of 10 seconds.
	RecalcLimit time.Duration
	// OnRecalc, when set, is called once for every recalculation with
	// whether an edit started it and how long it took. It is called from
	// the goroutine that calculated, outside the workbook lock, and must
	// not block.
	OnRecalc func(edit bool, took time.Duration)
	// OnLoadError, when set, is called once when the load ends with an
	// error that is not a stop: a store behind the import cursor, a failed
	// preflight or a damaged file. It is called from the load's goroutine,
	// after the load has closed every connection it opened, and must not
	// block. The instance then refuses every read and write with that
	// error until it stops.
	OnLoadError func(error)
	// ScratchDir is where the instance keeps large undo payloads while it
	// runs, outside the flat's disk quota. Empty means the system's
	// temporary directory. The instance removes its files when it stops;
	// the host clears what a crash left behind.
	ScratchDir string
	// ScratchRoot, when set, names that directory in place of ScratchDir
	// each time the instance has to make its own directory in it: before
	// its first scratch file, and again after its directory could not be
	// made or was removed. A host whose storage failed and was repaired
	// serves the next action that way. An error refuses the action that
	// needs the file as unavailable. The writer calls it with the
	// workbook locked, so it must not wait.
	ScratchRoot func() (string, error)
}

// Stats is a snapshot of one instance's counters for host metrics.
type Stats struct {
	Applied, Rejected int64
	Connections       int64
	Recalculations    int64
	Loaded            bool
	// Calculating is set while the sheet loads or its writer is in a
	// recalculation that no edit started. Calls that need the writer are
	// refused as calculating until it ends.
	Calculating bool
	// IdleSince is when the maintenance tick first found the instance with
	// no connection, no open edit group, an empty outbox and no live
	// capture, and zero while it is in use. It is a hint that is up to one
	// tick old: StopIfIdle decides whether the instance can stop.
	IdleSince time.Time
}

// Instance owns one sheet: its workbook in memory, its writer and its
// connections.
type Instance struct {
	db       *sql.DB
	rdb      *sql.DB
	path     string
	pageSize int64
	b        Backend
	opts     Options
	slug     string
	storeID  string
	now      func() time.Time
	log      *slog.Logger
	ctx      context.Context
	cancel   context.CancelFunc
	queue    chan *work
	admit    sync.RWMutex
	stop     sync.Once
	workers  sync.WaitGroup
	conns    sync.WaitGroup

	// mu protects the workbook. The writer holds the write lock while it
	// changes memory and until the sheet.db commit returns; captures hold
	// the read lock only while they copy cell records.
	mu      sync.RWMutex
	wb      *Workbook
	structs structLog

	ready   chan struct{} // closed when the workbook is loaded and calculated
	loadErr error
	// refused is set when the load ended with an error that is not a stop
	// (refuseLoad), before ready is closed.
	refused bool
	// attached is closed once db, rdb and storeID are set: in Open, or in
	// the load (openStorage).
	attached chan struct{}
	// usable is closed once sheet.db is attached and has passed its
	// preflight, at the gate in the load, or in Open for a sheet before its
	// first commit, which writes that commit first. Reads outside the
	// writer wait for it (awaitUsable). It stays open after a load that
	// failed before.
	usable chan struct{}

	// prev is closed when the storage of the instance that ran on this
	// sheet.db before is released, and nil when nothing was pending at
	// Open. The load opens nothing before.
	prev <-chan struct{}
	// released is closed when every handle of sheet.db the instance
	// opened is closed, its fields' and an opener's that a stop left
	// (openLog), and prev is closed.
	released chan struct{}
	relMu    sync.Mutex
	openers  int  // openers at work outside the instance's fields
	shut     bool // Shutdown closed the instance's own handles

	// pass is set while the writer is in a recalculation that no edit
	// started, a date change or a rollback. The channel is closed when the
	// pass ends (beginPass).
	pass atomic.Pointer[chan struct{}]
	// dateRun is set while a change of the date that a request asked for
	// is queued or at work (requestDate).
	dateMu  sync.Mutex
	dateRun *dateRun

	// checked holds the tables of sheet.db whose values have been checked
	// (checkTables).
	checked checkedTables

	smu      sync.Mutex
	sessions map[string]*session
	nextOrd  uint32
	scratch  undoScratch // undo payloads outside memory; the writer owns it
	// kept holds the receipts of sessions without a row; the writer owns it.
	kept keptReceipts
	// sessionBytes is the weight of the sessions in memory, and gone the
	// sessions without a row that left to make room (receipts.go).
	sessionBytes int64
	gone         goneSessions

	hub      *hub
	measure  *measure
	captures captureSet
	revs     revisionCache

	applied, rejected, recalcs atomic.Int64
	idleMu                     sync.Mutex
	idleSince                  time.Time
	// inflight counts the requests enter admitted that have not left, and
	// lastUse is the time of the newest use in Unix nanoseconds. StopIfIdle
	// reads both under the write side of admit.
	inflight, lastUse atomic.Int64
	// lastWork and lastPasses describe the newest recalculation: the units
	// its budget counted and the passes it took.
	lastWork, lastPasses atomic.Int64

	// hook lets tests stop at a step of the write path, and fault lets them
	// fail the commit of sheet.db.
	hook  func(step string)
	fault func() error
	// deadline, when a test sets it, gives an action the function that
	// says whether its deadline has passed in place of the clock.
	deadline func(x *txn) func() bool
}

// Open initializes a sheet, or resumes its durable state after a restart.
// A new sheet gets one empty tab named Sheet1 and its first revision. The
// workbook is checked, loaded and calculated in the background, and Open
// does not wait for any of it: until that ends, browsers see that the
// sheet is calculating and agent calls are refused as unavailable
// (technical design, section 10.5).
func Open(dir string, b Backend, opts Options) (*Instance, bool, error) {
	if b == nil {
		return nil, false, fmt.Errorf("sheet backend is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Location == nil {
		opts.Location = time.Local
	}
	if opts.RecalcLimit <= 0 {
		opts.RecalcLimit = defaultRecalcLimit
	}
	path, err := filepath.Abs(filepath.Join(dir, dbName))
	if err != nil {
		return nil, false, fmt.Errorf("sheet.db path: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	in := &Instance{path: path, b: b, opts: opts, slug: opts.Slug, now: opts.Now, log: opts.Log,
		ctx: ctx, cancel: cancel, queue: make(chan *work, 128), ready: make(chan struct{}),
		usable: make(chan struct{}), attached: make(chan struct{}), released: make(chan struct{}),
		prev: pendingRelease(path), sessions: map[string]*session{}, hub: newHub()}
	in.measure = &measure{in: in, wake: make(chan struct{}, 1)}
	in.touch()
	// A log that a crash left is read by the load, at the gate (openLog),
	// and so is a sheet.db that an instance stopped before still holds:
	// the load opens it once that instance released it.
	logged := in.prev != nil
	if !logged {
		var err error
		if logged, err = hasLog(path); err != nil {
			cancel()
			close(in.released)
			return nil, false, err
		}
	}
	created := false
	if !logged {
		db, fresh, pageSize, err := openDB(path)
		if err == nil {
			if created, err = in.attachDB(ctx, db, fresh, pageSize); err != nil {
				err = errors.Join(err, closeDB(path, in.db, in.rdb))
			}
		}
		if err != nil {
			cancel()
			close(in.released)
			return nil, false, err
		}
	}
	in.workers.Add(3)
	go in.load()
	go in.runWriter()
	go in.measure.run()
	in.measure.refresh()
	if created {
		in.b.OutboxReady()
	}
	return in, created, nil
}

// hasLog reports whether sheet.db has a -wal file beside it. A stopped
// instance leaves one only when its log was longer than walTruncateBytes
// (closeDB), or when its load refused the sheet (refuseLoad).
func hasLog(path string) (bool, error) {
	switch _, err := os.Stat(path + "-wal"); {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("sheet.db stat: %w", err)
	}
}

// attachDB makes db the instance's database: it checks the row of meta and
// the import cursor, writes the first commit of a sheet without one, and
// opens the reading connections. It reports whether it wrote the first
// commit. On failure it leaves db and what it opened in in.db and in.rdb,
// which its caller closes: Open as a stop does (closeDB), the load as its
// refusal does (refuseLoad). It leaves attached and usable open then.
func (in *Instance) attachDB(ctx context.Context, db *sql.DB, fresh bool, pageSize int64) (created bool, err error) {
	in.db, in.pageSize = db, pageSize
	// The row of meta is checked whole before anything of it is used, and
	// the outbox's sequence before the import cursor is compared with it;
	// the load checks the rest.
	var seq uint64
	var store sql.NullString
	var fault int64
	if err := db.QueryRowContext(ctx, `SELECT `+storedValues("meta", "", "store", "seq")+`,`+storedFault("meta", "")+
		` FROM meta WHERE singleton=1`).Scan(&store, &seq, &fault); err != nil {
		return false, fmt.Errorf("sheet.db state: %w", err)
	}
	if err := faultError("meta", fault); err != nil {
		return false, err
	}
	in.storeID = store.String
	in.checked.mark("meta")
	if err := checkImportCursor(ctx, db, in.storeID, in.opts); err != nil {
		return false, err
	}
	in.checked.mark("sqlite_sequence")
	// A crash after the schema but before the first content commit must
	// finish initialization rather than adopt a sheet without a tab. The
	// first commit is the only one that stores rows beside meta, so there
	// are none to check before it writes.
	created = fresh || seq == 0
	if created && !fresh {
		if err := checkUnwritten(ctx, db); err != nil {
			return false, err
		}
	}
	if created {
		// Such a sheet.db is a schema and a row of meta, so its preflight
		// is short, and it runs before the first commit writes beside
		// them. The load checks every other sheet.db at the gate.
		if err := checkNew(ctx, db); err != nil {
			return false, err
		}
		if err := in.initialize(ctx); err != nil {
			return false, err
		}
	}
	in.rdb, err = sql.Open("sqlite", sqliteuri.File(in.path)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return false, err
	}
	in.rdb.SetMaxOpenConns(4)
	if err := in.rdb.PingContext(ctx); err != nil {
		return false, err
	}
	close(in.attached)
	if created {
		close(in.usable)
	}
	return created, nil
}

// openStorage opens sheet.db in the load when Open did not, because an
// instance that ran on it before had not released it or because it has a
// log (openLog). It reports whether the load handed its turn at the gate
// to an opener that a stop left (openLog).
func (in *Instance) openStorage(ctx context.Context, turn *calcTurn) (handed bool, err error) {
	select {
	case <-in.attached:
		return false, nil
	default:
	}
	logged, err := hasLog(in.path)
	if err != nil {
		return false, err
	}
	if logged {
		return in.openLog(ctx, turn)
	}
	db, fresh, pageSize, err := openDB(in.path)
	if err != nil {
		return false, err
	}
	created, err := in.attachDB(ctx, db, fresh, pageSize)
	if err == nil && created {
		in.b.OutboxReady()
	}
	return false, err
}

// openLog opens a sheet.db whose -wal file a crash left, at the load's
// check points. The writer bounds that log only by the disk quota, which
// counts it (pendingWAL). A reader that holds it, such as the snapshot of
// a backup, keeps every commit made meanwhile in it, and a rollback
// rewrites the whole sheet in one commit. So Open does not read it. The log
// is copied for the probe at the check points (privateCopy), and SQLite
// then reads it to recover it, once for the probe and once for the
// instance's connection, which it cannot interrupt. A stop does not wait
// for that. The load hands its turn at the gate to the opener, which keeps
// the slot until SQLite returns, then closes what it opened, without a
// checkpoint of a long log (closeDB), and leaves the gate. Until then the
// instance's storage is not released (AwaitReleased). It creates no file.
func (in *Instance) openLog(ctx context.Context, turn *calcTurn) (handed bool, err error) {
	copied, err := privateCopy(in.path, func() bool { return loadPoint(ctx, "log") })
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, fmt.Errorf("sheet.db inspect: %w", err)
	}
	type opened struct {
		db       *sql.DB
		fresh    bool
		pageSize int64
		err      error
	}
	done := make(chan opened, 1)
	var mu sync.Mutex
	left := false // the load stopped waiting, and the opener has its turn
	in.holdStorage()
	go func() {
		defer in.dropStorage()
		var o opened
		version, err := probe(sqliteuri.File(copied) + "?mode=ro&_pragma=busy_timeout(5000)")
		os.RemoveAll(filepath.Dir(copied))
		switch {
		case err != nil:
			o.err = err
		case ctx.Err() != nil:
			o.err = ctx.Err()
		default:
			if fn := logOpening.Load(); fn != nil {
				(*fn)(in)
			}
			o.db, o.fresh, o.pageSize, o.err = openInspected(in.path, version, false)
		}
		mu.Lock()
		if !left {
			done <- o
			mu.Unlock()
			return
		}
		mu.Unlock()
		if o.db != nil {
			if err := closeDB(in.path, o.db, nil); err != nil {
				in.log.Warn("sheet close", "slug", in.slug, "err", err)
			}
		}
		turn.leave()
	}()
	var o opened
	select {
	case o = <-done:
	case <-ctx.Done():
		mu.Lock()
		select {
		case o = <-done:
		default:
			left = true
		}
		mu.Unlock()
		if left {
			return true, ctx.Err()
		}
	}
	if o.err != nil {
		return false, o.err
	}
	created, err := in.attachDB(ctx, o.db, o.fresh, o.pageSize)
	if err != nil {
		return false, err
	}
	if created {
		in.b.OutboxReady()
	}
	return false, nil
}

// AwaitChecked waits until the instance has opened sheet.db and its load
// has checked it (preflight), which a call of the host that reads the
// store needs first: until then StoreID is "" and the outbox is not read.
// It returns the load's refusal when the load ended without, the error of
// ctx when ctx ends first, and unavailable "stopped" when the instance
// stops.
func (in *Instance) AwaitChecked(ctx context.Context) error { return in.awaitUsable(ctx, 0) }

// awaitUsable waits until sheet.db is attached and has passed its
// preflight (usable). It returns the load's refusal once the load ended
// without, unavailable "stopped" once the instance stops, the error of ctx
// when ctx ends first, and, when patience is positive and passes first,
// unavailable "calculating". An outcome that is already known is
// returned before ctx is looked at.
func (in *Instance) awaitUsable(ctx context.Context, patience time.Duration) error {
	// The outcome is one observation: the end of the load and the stop are
	// read first, and usable after them. usable closes before ready, so
	// once the load has ended usable says how: a load that succeeds
	// between the reads is seen as usable, never as a failure. A load that
	// refused the sheet after its preflight closed the connections, so its
	// refusal comes first.
	settled := func() (error, bool) {
		loaded, stopped := isClosed(in.ready), in.ctx.Err() != nil
		if fn := readinessRead.Load(); fn != nil {
			(*fn)(in)
		}
		switch {
		case loaded && in.refused:
			return in.loadRefusal(), true
		case isClosed(in.usable):
			return nil, true
		case loaded:
			// The load ended without attaching or checking sheet.db.
			return in.loadRefusal(), true
		case stopped:
			return unavailable("stopped"), true
		}
		return nil, false
	}
	if err, ok := settled(); ok {
		return err
	}
	var timeout <-chan time.Time
	if patience > 0 {
		timer := time.NewTimer(patience)
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case <-in.usable:
	case <-in.ready:
	case <-in.ctx.Done():
	case <-ctx.Done():
		if err, ok := settled(); ok {
			return err
		}
		return ctx.Err()
	case <-timeout:
		if err, ok := settled(); ok {
			return err
		}
		return unavailable("calculating")
	}
	err, _ := settled()
	return err
}

// readinessRead lets a test hold awaitUsable between its reads of the
// load's end and of usable. It is nil outside tests.
var readinessRead atomic.Pointer[func(in *Instance)]

// loadRefusal is the refusal of every call that needs the workbook or
// sheet.db after the load failed. A refusal of sheet.db as a development
// build's names its cause.
func (in *Instance) loadRefusal() error {
	if problem := (*WriteError)(nil); errors.As(in.loadErr, &problem) && problem.Reason == "load" {
		return problem
	}
	return &WriteError{Code: "unavailable", Reason: "load", Retry: true, cause: in.loadErr}
}

// releases holds, by the path of sheet.db, the newest stopped instance
// whose storage is not released yet. Open makes the next instance on that
// path wait for it (prev).
var releases = struct {
	sync.Mutex
	m map[string]*Instance
}{m: map[string]*Instance{}}

// pendingRelease returns the channel that is closed when the storage of
// the instances stopped on path is released, or nil when none is pending.
func pendingRelease(path string) <-chan struct{} {
	releases.Lock()
	defer releases.Unlock()
	if prev := releases.m[path]; prev != nil {
		return prev.released
	}
	return nil
}

// AwaitReleased waits until no instance stopped on the sheet in dir still
// holds a handle of its sheet.db, or until ctx ends. A rename or a delete
// of the directory waits for it, since a handle can keep a file from being
// renamed or removed, and an opener that a stop left closes its handle
// only once SQLite returns from recovering a log.
func AwaitReleased(ctx context.Context, dir string) error {
	path, err := filepath.Abs(filepath.Join(dir, dbName))
	if err != nil {
		return err
	}
	pending := pendingRelease(path)
	if pending == nil {
		return nil
	}
	select {
	case <-pending:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// holdStorage counts an opener of sheet.db at work outside the instance's
// fields. The load calls it, which Shutdown waits for, so it never runs
// after Shutdown.
func (in *Instance) holdStorage() {
	in.relMu.Lock()
	in.openers++
	in.relMu.Unlock()
}

// dropStorage ends an opener that holdStorage counted, once it closed what
// it opened.
func (in *Instance) dropStorage() {
	in.relMu.Lock()
	in.openers--
	last := in.shut && in.openers == 0
	in.relMu.Unlock()
	if last {
		in.releaseStorage()
	}
}

// shutStorage marks the instance's own handles closed. A release that has
// to wait for an opener or for prev is filed under the path first, so an
// Open that follows the stop waits for it.
func (in *Instance) shutStorage() {
	in.relMu.Lock()
	in.shut = true
	idle := in.openers == 0
	if !idle || !isClosed(in.prev) {
		releases.Lock()
		releases.m[in.path] = in
		releases.Unlock()
	}
	in.relMu.Unlock()
	if idle {
		in.releaseStorage()
	}
}

// releaseStorage closes released once prev is closed and drops the
// instance from releases.
func (in *Instance) releaseStorage() {
	finish := func() {
		releases.Lock()
		if releases.m[in.path] == in {
			delete(releases.m, in.path)
		}
		releases.Unlock()
		close(in.released)
	}
	if isClosed(in.prev) {
		finish()
		return
	}
	go func() {
		<-in.prev
		finish()
	}()
}

// isClosed reports whether ch is nil or closed.
func isClosed(ch <-chan struct{}) bool {
	if ch == nil {
		return true
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// HoldLogs is logOpening for the tests of the host: it keeps the opener of
// every load that opens a sheet.db with a log a crash left where SQLite
// starts to recover the log, until release is called. reached receives
// each instance held. It fails while another hold is set.
func HoldLogs() (reached <-chan *Instance, release func(), err error) {
	at, let := make(chan *Instance, 16), make(chan struct{})
	hold := func(in *Instance) {
		select {
		case at <- in:
		default:
		}
		<-let
	}
	if !logOpening.CompareAndSwap(nil, &hold) {
		return nil, nil, errors.New("sheet: another test holds logs")
	}
	release = sync.OnceFunc(func() {
		logOpening.CompareAndSwap(&hold, nil)
		close(let)
	})
	return at, release, nil
}

// HoldReadiness is readinessRead for the tests of the host: it keeps every
// wait for a sheet's readiness (AwaitChecked) between its read of the
// load's end and its read of usable, until release is called. reached
// receives each instance held. It fails while another hold is set.
func HoldReadiness() (reached <-chan *Instance, release func(), err error) {
	at, let := make(chan *Instance, 16), make(chan struct{})
	hold := func(in *Instance) {
		select {
		case at <- in:
		default:
		}
		<-let
	}
	if !readinessRead.CompareAndSwap(nil, &hold) {
		return nil, nil, errors.New("sheet: another test holds readiness")
	}
	release = sync.OnceFunc(func() {
		readinessRead.CompareAndSwap(&hold, nil)
		close(let)
	})
	return at, release, nil
}

// logOpening lets a test hold the opener of openLog where SQLite starts to
// recover the log for the instance's connection. It is nil outside tests.
var logOpening atomic.Pointer[func(in *Instance)]

// initialize writes the first state of a new sheet: one empty tab named
// Sheet1, at sequence 1, with its "created" revision.
func (in *Instance) initialize(ctx context.Context) error {
	wb := newWorkbook(in.storeID)
	wb.seq = 1
	wb.date = in.today()
	wb.placeTab(wb.newTab("Sheet1", newTabRows, newTabCols, 1), 0)
	wb.files.manifest = true
	wb.files.axes[wb.tabs[0].id] = true
	return in.transaction(ctx, func(tx *sql.Tx) error {
		if err := stageMeta(ctx, tx, wb); err != nil {
			return err
		}
		if err := stageTab(ctx, tx, wb.tabs[0]); err != nil {
			return err
		}
		_, _, err := in.recordBoundary(ctx, tx, wb, boundary{kind: "created", message: "sheet created", force: true})
		return err
	})
}

func (in *Instance) today() formula.Date {
	y, m, d := in.now().In(in.opts.Location).Date()
	return formula.DateOf(y, int(m), d)
}

// load reads sheet.db, builds the index and evaluates every formula before
// the instance serves anything. It runs at the host-wide gate of
// recalculations that no edit starts, and takes turns there with the
// passes of other sheets: while it checks sheet.db (preflight), reads the
// cells, builds the index and evaluates. It holds no lock; the workbook is
// its own until it is done. A stop of the instance ends it at its next
// check point; one after its last recalculation lets it publish the
// workbook, which the writer reads only after ready. It waits for the
// instance that ran on sheet.db before to release it (prev) before it
// enters the gate.
func (in *Instance) load() {
	defer in.workers.Done()
	// OnLoadError hears of a refusal once the load has left the gate and
	// readers see the refusal.
	defer func() {
		if in.refused && in.opts.OnLoadError != nil {
			in.opts.OnLoadError(in.loadErr)
		}
	}()
	defer close(in.ready)
	if in.prev != nil {
		select {
		case <-in.prev:
		case <-in.ctx.Done():
			in.loadErr = unavailable("stopped")
			return
		}
	}
	turn, ok := calcSlots.enter(in, in.ctx)
	handed := false
	defer func() {
		if !handed {
			turn.leave()
		}
	}()
	if !ok {
		in.loadErr = unavailable("stopped")
		return
	}
	ctx := turn.context()
	handed, err := in.openStorage(ctx, turn)
	if err == nil {
		err = in.preflightLoad(ctx)
	}
	var wb *Workbook
	if err == nil {
		wb, err = loadWorkbook(ctx, in.rdb)
	}
	if err != nil {
		if in.ctx.Err() != nil {
			in.loadErr = unavailable("stopped")
			return
		}
		in.refuseLoad(err)
		return
	}
	// The load checked every table.
	in.checked.markAll()
	// A build that ends early leaves the index empty and stale, and a
	// recalculation builds a stale index again before it evaluates, so no
	// partial index is ever evaluated. The load ends here all the same.
	budget := turn.budget()
	var out calcOut
	cerr := wb.buildDeps(turn.budget())
	if cerr == nil {
		out, cerr = turn.recalc(wb, calcReq{all: true, budget: budget})
	}
	in.noteCalc(budget, out.passes)
	in.observeCalc(false, out.took)
	if cerr != nil {
		if in.ctx.Err() != nil {
			in.loadErr = unavailable("stopped")
			return
		}
		// A stored state meets the read limits; one that does not is damaged.
		in.refuseLoad(cerr)
		return
	}
	if fn := loadPublishing.Load(); fn != nil {
		(*fn)(in)
	}
	wb.apply(out, nil, 0)
	wb.settle()
	in.mu.Lock()
	in.wb = wb
	in.structs.clear(wb.seq)
	in.hub.start(wb.seq)
	in.mu.Unlock()
}

// refuseLoad ends a load that failed for another reason than a stop: err
// answers every read and write until the instance stops. The connections
// the load opened close without a checkpoint, whatever the length of the
// log (closeConns), so sheet.db and its -wal file keep their bytes, and the
// instance holds no handle of them. A failure of that close is a warning:
// the connections are closed all the same.
func (in *Instance) refuseLoad(err error) {
	in.loadErr, in.refused = err, true
	in.log.Error("sheet load", "slug", in.slug, "err", err)
	if cerr := closeConns(in.db, in.rdb, true); cerr != nil {
		in.log.Warn("sheet close", "slug", in.slug, "err", cerr)
	}
}

// loadPublishing lets a test hold a load whose recalculation succeeded
// before it publishes the workbook. It is nil outside tests.
var loadPublishing atomic.Pointer[func(in *Instance)]

// preflightLoad runs the preflight of sheet.db at the load's check points,
// unless Open ran it.
func (in *Instance) preflightLoad(ctx context.Context) error {
	select {
	case <-in.usable:
		return nil
	default:
	}
	err := preflight(ctx, in.rdb, func(phase string) bool { return loadPoint(ctx, phase) })
	if fn := preflightEnded.Load(); fn != nil {
		(*fn)(in, err)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	close(in.usable)
	return nil
}

// preflightEnded lets a test see how the preflight of a load ended. It is
// nil outside tests.
var preflightEnded atomic.Pointer[func(in *Instance, err error)]

// settle rebuilds what readers must never build: the rows of each tab in
// position order. The writer calls it before it releases the lock.
func (wb *Workbook) settle() {
	for _, t := range wb.tabs {
		t.rowsByPos()
	}
}

// await waits until the workbook is loaded. An agent call does not wait
// long: it is refused as calculating and can be retried.
func (in *Instance) await(ctx context.Context, patience time.Duration) error {
	select {
	case <-in.ready:
	default:
		var timeout <-chan time.Time
		if patience > 0 {
			timer := time.NewTimer(patience)
			defer timer.Stop()
			timeout = timer.C
		}
		select {
		case <-in.ready:
		case <-ctx.Done():
			return ctx.Err()
		case <-in.ctx.Done():
			return unavailable("stopped")
		case <-timeout:
			return unavailable("calculating")
		}
	}
	if in.loadErr != nil {
		return in.loadRefusal()
	}
	return nil
}

// calculating reports whether the sheet is in a recalculation that no edit
// started, which is its load, or a date change or a rollback of its writer.
func (in *Instance) calculating() bool { return in.passOver() != nil }

// passOver returns the channel that is closed when the recalculation the
// sheet is in ends, its load or the writer's pass, or nil when it is in
// none.
func (in *Instance) passOver() <-chan struct{} {
	select {
	case <-in.ready:
	default:
		return in.ready
	}
	if over := in.pass.Load(); over != nil {
		return *over
	}
	return nil
}

// beginPass marks a recalculation of the writer that no edit started, a
// date change or a rollback, and returns the function that ends it. The
// writer takes no other work meanwhile, so nothing may wait for it as for
// an edit (execute), and the browsers are told that the sheet calculates
// once the pass has lasted for tell, those that wait for their session
// included (hub.calculating).
func (in *Instance) beginPass(tell time.Duration) (end func()) {
	over := make(chan struct{})
	in.pass.Store(&over)
	var mu sync.Mutex
	done := false
	notice := time.AfterFunc(tell, func() {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			in.hub.calculating(true)
		}
	})
	return func() {
		notice.Stop()
		mu.Lock()
		done = true
		mu.Unlock()
		in.hub.calculating(false)
		in.pass.Store(nil)
		close(over)
	}
}

// touch records a use of the instance.
func (in *Instance) touch() { in.lastUse.Store(in.now().UnixNano()) }

// enter admits a request that is not writer work: a read, an HTTP request
// or a connection. It is refused as stopped once the instance stops, and
// counts as use until leave.
func (in *Instance) enter() error {
	in.admit.RLock()
	defer in.admit.RUnlock()
	if in.ctx.Err() != nil {
		return unavailable("stopped")
	}
	in.inflight.Add(1)
	in.touch()
	return nil
}

func (in *Instance) leave() {
	in.touch()
	in.inflight.Add(-1)
}

// settleWait is how long StopIfIdle waits for the writer to reach its
// probe. A writer that takes longer is at work, so the instance is in use.
const settleWait = 100 * time.Millisecond

// StopIfIdle stops the instance if nothing uses it and nothing has used it
// for at least idle, and reports whether the instance is stopped when the
// call returns.
//
// In use means any of: a connection, a request or a read in flight, queued
// or running writer work, an open edit group, an outbox entry the host has
// not acknowledged, a capture or a revision workbook that has not expired,
// a workbook that is still loading, or a use newer than idle. Every
// admitted request, read, write and host call counts as a use when it
// starts, and requests and reads again when they end.
//
// The check and the stop are one step. They run under the lock that admits
// every request, after the writer has finished all work admitted before, so
// no request can slip between them: a caller either is admitted first and
// keeps the instance running, or finds it stopped and gets unavailable with
// the reason "stopped". A capture a read just created is therefore never
// dropped by this call. The host opens a new instance for a refused caller.
func (in *Instance) StopIfIdle(idle time.Duration) bool {
	if in.ctx.Err() != nil {
		in.Shutdown("")
		return true
	}
	// Most calls end here, without the lock.
	if !in.unused(idle) {
		return false
	}
	in.admit.Lock()
	stop := in.ctx.Err() == nil && in.unused(idle) && in.settled()
	if stop {
		in.hub.closeAll("")
		in.cancel()
	}
	stopped := in.ctx.Err() != nil
	in.admit.Unlock()
	if stopped {
		in.Shutdown("")
	}
	return stopped
}

// unused reports whether nothing in memory uses the instance and its last
// use is at least idle old.
func (in *Instance) unused(idle time.Duration) bool {
	select {
	case <-in.ready:
	default:
		return false
	}
	now := in.now()
	return in.loadErr == nil && in.pass.Load() == nil && in.inflight.Load() == 0 && len(in.queue) == 0 && in.hub.count() == 0 &&
		!in.captures.live(now) && !in.revs.live(now) && now.Sub(time.Unix(0, in.lastUse.Load())) >= idle
}

// unsettledQuery counts what an instance still has to hand to the host:
// open edit groups and outbox entries.
const unsettledQuery = `SELECT (SELECT count(*) FROM groups)+(SELECT count(*) FROM outbox)`

// settled reports whether the writer has finished everything it was given
// and left no open edit group and no outbox entry. The caller holds the
// write side of admit, so nothing new can be queued: a probe at the end of
// the queue runs when the work before it is done.
func (in *Instance) settled() bool {
	var clean atomic.Bool
	probe := &work{ctx: context.Background(), done: make(chan error, 1)}
	probe.run = func() error {
		var busy int
		err := in.db.QueryRowContext(in.ctx, unsettledQuery).Scan(&busy)
		clean.Store(err == nil && busy == 0)
		return err
	}
	select {
	case in.queue <- probe:
	default:
		return false
	}
	timer := time.NewTimer(settleWait)
	defer timer.Stop()
	select {
	case err := <-probe.done:
		return err == nil && clean.Load()
	case <-timer.C:
		// The probe leaves the queue, or runs now and changes nothing.
		probe.withdraw()
		return false
	}
}

// Stop refuses new work, closes connections and waits before releasing
// storage.
func (in *Instance) Stop() { in.Shutdown("") }

// Shutdown terminates the instance. code is sent to browsers as the reason:
// "deleted" and "renamed" tell them that the sheet is gone from its address.
func (in *Instance) Shutdown(code string) {
	in.stop.Do(func() {
		in.admit.Lock()
		in.hub.closeAll(code)
		in.cancel()
		in.admit.Unlock()
		in.workers.Wait()
		in.conns.Wait()
		in.captures.close()
		in.scratch.close()
		// A load that did not open sheet.db left neither set, and one that
		// refused it closed both.
		if in.db != nil && !in.refused {
			if err := closeDB(in.path, in.db, in.rdb); err != nil {
				in.log.Warn("sheet close", "code", code, "err", err)
			}
		}
		in.shutStorage()
	})
}

// StoreID is the immutable identity of this sheet.db, not a revision.
// It is "" while the load opens a sheet.db with a log a crash left.
func (in *Instance) StoreID() string {
	select {
	case <-in.attached:
		return in.storeID
	default:
		return ""
	}
}

// Barrier waits for all work admitted before this call to commit.
func (in *Instance) Barrier(ctx context.Context) error {
	return in.execute(ctx, &work{run: func() error { return nil }})
}

// Remeasure refreshes host usage after host-side changes.
func (in *Instance) Remeasure(ctx context.Context) error { return in.measure.remeasure(ctx) }

// WithdrawPublic closes every Public connection and response and drops
// every cached snapshot. Core calls it when a route's access changes.
func (in *Instance) WithdrawPublic() { in.hub.withdrawPublic() }

// Stats returns the instance's counters.
func (in *Instance) Stats() Stats {
	loaded := false
	select {
	case <-in.ready:
		loaded = in.loadErr == nil
	default:
	}
	in.idleMu.Lock()
	idle := in.idleSince
	in.idleMu.Unlock()
	return Stats{Applied: in.applied.Load(), Rejected: in.rejected.Load(), Connections: in.hub.count(),
		Recalculations: in.recalcs.Load(), Loaded: loaded, Calculating: in.calculating(), IdleSince: idle}
}

func (in *Instance) transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := in.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sheet.db begin: %w", err)
	}
	if err := fn(tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if err := in.commit(tx); err != nil {
		return fmt.Errorf("sheet.db commit: %w", err)
	}
	return nil
}

// commit commits tx, a transaction of the writing connection, and then
// bounds the log: once it holds more than walTruncateBytes, it is
// checkpointed and truncated, as SQLite's automatic checkpoint, which the
// connection does not run (openDB), would have done in the commit. A
// commit after the instance stopped, the final boundary of its open edit
// groups, starts no checkpoint and leaves the log as it is; the next open
// recovers it at the gate. A checkpoint that runs when the stop comes
// ends as truncateWAL says.
func (in *Instance) commit(tx *sql.Tx) error {
	in.step("commit")
	if in.fault != nil {
		if err := in.fault(); err != nil {
			return errors.Join(err, tx.Rollback())
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	in.step("committed")
	if in.ctx.Err() == nil {
		in.boundLog()
	}
	return nil
}

// boundLog checkpoints and truncates the log when it holds more than
// walTruncateBytes. A busy log is left for the next commit.
func (in *Instance) boundLog() {
	if info, err := os.Stat(in.path + "-wal"); err == nil && info.Size() > walTruncateBytes {
		in.step("checkpoint")
		if err := in.truncateWAL(); err != nil && in.ctx.Err() == nil {
			in.log.Warn("sheet WAL maintenance", "err", err)
		}
	}
}

func (in *Instance) noteCalc(b *formula.Budget, passes int) {
	in.recalcs.Add(1)
	in.lastWork.Store(b.Used())
	in.lastPasses.Store(int64(passes))
}

func (in *Instance) step(name string) {
	if in.hook != nil {
		in.hook(name)
	}
}
