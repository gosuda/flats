// Package board runs one board flat (docs/spec/board-TECH.md): its board.db,
// the single apply loop that changes it, the live WebSocket protocol, the
// HTTP routes of the board's origin, uploads, data files and the disk quota
// check. The editing rules themselves live in internal/board/engine; core
// reaches this package through Instance and supplies a Backend.
package board

import (
	"cmp"
	"context"
	"crypto/rand"
	"embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/board/model"
)

//go:embed web/index.html web/*.js web/*.css web/tools/*.js
var webFS embed.FS

// Refusal categories (8.3). Every error the Instance returns to core is an
// *Error with one of them, except context errors.
const (
	CategoryInvalid     = "invalid"
	CategoryConflict    = "conflict"
	CategoryCapacity    = "board_capacity"
	CategoryUnavailable = "unavailable"
	CategoryNotFound    = "not_found"
)

// Error is a refused request.
type Error struct {
	Category string
	Msg      string
}

func (e *Error) Error() string { return e.Category + ": " + e.Msg }

func errorf(category, format string, args ...any) error {
	return &Error{category, fmt.Sprintf(format, args...)}
}

// ErrStopped is returned for work submitted to a stopped instance.
var ErrStopped = &Error{CategoryUnavailable, "the board is not running"}

// asError converts engine refusals into *Error and keeps others.
func asError(err error) error {
	var ae *engine.AgentError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ae):
		return &Error{ae.Category, ae.Msg}
	case errors.Is(err, model.ErrFormat), errors.Is(err, engine.ErrLimit):
		return &Error{CategoryInvalid, err.Error()}
	}
	return err
}

// File is one revision file: a bucket, board.json or an asset (5.2).
type File struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// RevisionInfo is one revision as the History panel lists it.
type RevisionInfo struct {
	Number     int      `json:"number"`
	Kind       string   `json:"kind"`
	Authors    []string `json:"authors"`
	Message    string   `json:"message"`
	At         int64    `json:"at"` // unix seconds
	Pinned     bool     `json:"pinned"`
	Added      int      `json:"added"`
	Changed    int      `json:"changed"`
	Removed    int      `json:"removed"`
	RollbackOf int      `json:"rollback_of,omitempty"`
	// GuestAuthors are up to five guest names this revision credits, and
	// GuestsMore counts the rest (spec rule 25).
	GuestAuthors []string `json:"guest_authors,omitempty"`
	GuestsMore   int      `json:"guests_more,omitempty"`
	// SafePoint marks the protected safe point while anyone can edit.
	SafePoint bool `json:"safe_point,omitempty"`
}

// RevisionPage is one page of history, newest first.
type RevisionPage struct {
	Revisions     []RevisionInfo `json:"revisions"`
	Count         int            `json:"count"`
	Pins          int            `json:"pins"`
	PinQuota      int            `json:"pin_quota"`
	RevisionQuota int            `json:"revision_quota"`
	More          bool           `json:"more"`
}

// Limits are the host settings the board follows.
type Limits struct {
	DiskQuota int64 // per-flat disk quota in bytes; 0 means none
	UploadMax int64 // system.upload_max_bytes
}

// Backend is what an Instance needs from core. Core implements it for one
// board flat. Methods may be called from any goroutine but never from the
// apply loop, except Limits and OutboxReady, which must not block.
type Backend interface {
	// Limits returns the current disk quota and upload limit.
	Limits() Limits
	// MeasureOther returns the bytes of the flat directory without
	// board/board.db and its -wal and -shm files (5.4 "other"). It runs
	// in a helper goroutine and must not take the flat lock.
	MeasureOther(ctx context.Context) (int64, error)
	// PutBlob stores data as revisions/blobs/<hash> (hash is its sha256
	// hex) unless that blob exists, and reports whether it wrote new bytes.
	PutBlob(ctx context.Context, hash string, data []byte) (stored bool, err error)
	// ReadBlob reads a stored blob under a read lease.
	ReadBlob(ctx context.Context, hash string) ([]byte, error)
	// RevisionAsset reports whether a retained revision of this flat holds
	// assets/<hash>.<ext>, and that path (7.2).
	RevisionAsset(ctx context.Context, hash string) (path string, ok bool, err error)
	// Revisions lists history newest first, before revision before (0:
	// from the newest), with the counts of board_rev_stats. Core imports
	// the outbox first.
	Revisions(ctx context.Context, before, limit int) (RevisionPage, error)
	// RevisionContent reads retained revision n (ContentFromFiles helps).
	RevisionContent(ctx context.Context, n int) (RevisionInfo, engine.Content, error)
	// HeldRevisionContent reads retained revision n like RevisionContent,
	// keeping the read lease on its files (chosen under the flat lock)
	// until release is called, so the blob sweep cannot remove a blob the
	// caller still uses (5.5). release may be called more than once.
	HeldRevisionContent(ctx context.Context, n int) (c engine.Content, release func(), err error)
	// Lease takes read leases on stored blobs until release is called
	// (5.5). The caller chooses hashes the board protects at that moment
	// (in the apply loop), so the sweep, which reads those references
	// through the apply loop, either keeps them or sees the lease. It must
	// not block. release may be called more than once.
	Lease(hashes []string) (release func())
	// Pin pins or unpins revision n.
	Pin(ctx context.Context, n int, pinned bool) error
	// Rollback rolls the board back to revision n (4.11): core takes the
	// flat lock and the hold, reads n, calls Instance.Rollback and imports.
	// It returns the new revision number (0 when the import is pending).
	Rollback(ctx context.Context, n int, label string) (int, error)
	// Exclusive runs fn under the flat lock with a fresh measurement of
	// "other", then imports the outbox synchronously.
	Exclusive(ctx context.Context, fn func(ctx context.Context, other int64) error) error
	// OutboxReady tells core that the outbox has entries to import. It must
	// not block.
	OutboxReady()
}

// Options configure an Instance.
type Options struct {
	Slug string
	// Title is the title of a new board.
	Title string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log receives background failures; nil means slog.Default().
	Log *slog.Logger
	// PublicAccess is the stored public access level of the flat: "" when
	// it is not Public, "view" or "edit". SetPublicAccess changes it later.
	PublicAccess string

	tickEvery time.Duration // tests stop the ticker this way
}

// Instance runs one board. Create it with Open; it serves HTTP and the
// WebSocket, and core calls its methods for agent work, imports of the
// outbox and history.
type Instance struct {
	slug string
	dir  string
	b    Backend
	now  func() time.Time
	log  *slog.Logger

	ctx     context.Context
	cancel  context.CancelFunc
	stopMu  sync.Mutex
	stopped atomic.Bool
	reqs    sync.WaitGroup // HTTP handlers, including WebSocket readers
	loopWG  sync.WaitGroup

	items chan *item
	db    *store
	st    *engine.State // owned by the apply loop
	sd    *side         // owned by the apply loop
	// snapCur is the snapshot state of the current seq (owned by the apply
	// loop; every commit and load clears it).
	snapCur *snapData
	meas    *measurer

	hub     *hub
	assets  *assetView
	snaps   *pageCache
	shares  *shareList
	storeID string

	// Limits that tests lower.
	privateMax, publicMax int
	privateQueue          int64
	publicStarts          *startLimiter
	deltaMaxEntries       int
	deltaMaxBytes         int64
	deltaMaxAge           int64
	resetsMax             int
	tickEvery             time.Duration
	ticks                 int
	// hook, for crash tests, runs in the apply loop just before and just
	// after each COMMIT ("commit", "committed").
	hook func(stage string)

	// Public access (TECH public edit 6.1): the level and the access epoch
	// are owned by the apply loop; levelNow mirrors the level for readers
	// outside it.
	level    string
	epoch    int64
	levelNow atomic.Value // string
	pub      publicLimits
	viewers  viewerFrames
}

// Open opens the board in dir (flats/<slug>/board), creating board.db with
// a forced created outbox entry when it does not exist or an earlier
// creation was interrupted before it committed. created reports that it
// did.
func Open(dir string, b Backend, opts Options) (in *Instance, created bool, err error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	in = &Instance{
		slug: opts.Slug, dir: dir, b: b, now: now, log: log.With("board", opts.Slug),
		ctx: ctx, cancel: cancel, items: make(chan *item, 256),
		hub: newHub(), assets: newAssetView(), snaps: newPageCache(), shares: &shareList{},
		privateMax: 64, publicMax: 256, privateQueue: privateQueueMax, publicStarts: newStartLimiter(8),
		deltaMaxEntries: 20000, deltaMaxBytes: 16 << 20, deltaMaxAge: 3600, resetsMax: 1000,
		tickEvery: cmp.Or(opts.tickEvery, 5*time.Second),
	}
	in.meas = newMeasurer(in)
	in.pub = newPublicLimits()
	if validLevel(opts.PublicAccess) {
		in.level = opts.PublicAccess
	}
	in.levelNow.Store(in.level)
	db, created, err := openStore(filepath.Join(dir, dbName))
	if err != nil {
		cancel()
		return nil, false, err
	}
	in.db = db
	if created {
		if err := in.initialize(opts.Title); err != nil {
			db.close()
			cancel()
			return nil, false, err
		}
	}
	if err := in.load(); err != nil {
		db.close()
		cancel()
		return nil, false, err
	}
	in.loopWG.Add(1)
	go in.loop()
	go in.meas.refresher()
	return in, created, nil
}

// Slug returns the board's slug.
func (in *Instance) Slug() string { return in.slug }

// StoreID is the random id of this board.db's outbox (5.3), for the import
// cursor content_state.import_store.
func (in *Instance) StoreID() string { return in.storeID }

// Stop refuses new requests, closes every socket, waits for the handlers
// and the apply loop, and closes board.db.
func (in *Instance) Stop() { in.Shutdown("") }

// Shutdown is Stop that first tells every open canvas why with a fatal
// code: deleted, renamed or type_changed (4.8). An empty code closes
// sockets without a fatal message, as on a route stop.
func (in *Instance) Shutdown(code string) {
	in.stopMu.Lock()
	if in.stopped.Load() {
		in.stopMu.Unlock()
		return
	}
	in.stopped.Store(true)
	in.stopMu.Unlock()
	in.hub.closeAll(code)
	in.cancel()
	done := make(chan struct{})
	go func() { in.reqs.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	in.loopWG.Wait()
	in.snaps.clear()
	in.db.close()
}

// enter admits a request unless the instance stopped.
func (in *Instance) enter() bool {
	in.stopMu.Lock()
	defer in.stopMu.Unlock()
	if in.stopped.Load() {
		return false
	}
	in.reqs.Add(1)
	return true
}

func private(r *http.Request) bool { return r.Header.Get("X-Flats-Access") == "private" }

// ServeHTTP routes the board's origin (section 7).
func (in *Instance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !in.enter() {
		http.Error(w, "this board is not available right now", http.StatusServiceUnavailable)
		return
	}
	defer in.reqs.Done()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(in.ctx, cancel)
	defer stop()
	r = r.WithContext(ctx)
	p := r.URL.Path
	switch {
	case p == "/":
		in.serveShell(w, r)
	case strings.HasPrefix(p, "/_board/app/"):
		in.serveApp(w, r, strings.TrimPrefix(p, "/_board/app/"))
	case p == "/_board/ws":
		in.serveWS(w, r)
	case strings.HasPrefix(p, "/_board/a/"):
		in.serveAsset(w, r, strings.TrimPrefix(p, "/_board/a/"))
	case p == "/_board/api/assets":
		in.serveUpload(w, r)
	case p == "/_board/api/revisions" || strings.HasPrefix(p, "/_board/api/revisions/"):
		in.serveRevisions(w, r, strings.TrimPrefix(strings.TrimPrefix(p, "/_board/api/revisions"), "/"))
	case p == "/_board/api/recover":
		in.serveRecover(w, r)
	case p == "/_board/export.zip":
		in.serveExport(w, r)
	case p == "/_board/api/import":
		in.serveImport(w, r)
	default:
		http.NotFound(w, r)
	}
}

const idChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// newID returns prefix plus 10 random base62 characters (an element or
// group id).
func newID(prefix string) string {
	var b [10]byte
	out := make([]byte, 0, len(prefix)+10)
	out = append(out, prefix...)
	for i := 0; i < 10; {
		if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
			panic(err)
		}
		for _, c := range b {
			if c < 248 && i < 10 { // 248 = 4 × 62: no modulo bias
				out = append(out, idChars[c%62])
				i++
			}
		}
	}
	return string(out)
}

// randomToken returns 128 random bits in hex.
func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b)
}

// do runs fn in the apply loop and waits for it.
func (in *Instance) do(ctx context.Context, fn func() error) error {
	it := &item{fn: fn, done: make(chan error, 1)}
	select {
	case in.items <- it:
	case <-ctx.Done():
		return ctx.Err()
	case <-in.ctx.Done():
		return ErrStopped
	}
	select {
	case err := <-it.done:
		return err
	case <-in.ctx.Done():
		// The loop answers drained items before it exits.
		select {
		case err := <-it.done:
			return err
		case <-time.After(time.Second):
			return ErrStopped
		}
	}
}
