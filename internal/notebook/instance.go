package notebook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Limits are the host's current per-flat storage limits.
type Limits struct{ DiskQuota, UploadMax int64 }

// Backend supplies host storage without exposing its locks to the writer.
type Backend interface {
	Limits() Limits
	MeasureOther(context.Context) (int64, error)
	PutBlob(context.Context, string, []byte) (bool, error)
	ReadBlob(context.Context, string) ([]byte, error)
	Lease([]string) func()
	Exclusive(context.Context, func(context.Context, int64) error) error
	OutboxReady()
}

// Options supplies initialization and host clock/logging facilities.
type Options struct {
	Slug   string
	Title  string
	Now    func() time.Time
	Log    *slog.Logger
	Spills *SpillManager
}

// WriteError distinguishes permanent refusals from retryable admission failures.
type WriteError struct {
	Code   string `json:"code"`
	Reason string `json:"reason,omitempty"`
	Retry  bool   `json:"retry"`
	Detail string `json:"detail,omitempty"`
	cause  error
}

func (e *WriteError) Error() string { return e.Code + ":" + e.Reason }
func (e *WriteError) Unwrap() error { return e.cause }

func unavailable(reason string) error {
	return &WriteError{Code: "unavailable", Reason: reason, Retry: true}
}

// Instance owns one notebook's writer and independent read connections.
type Instance struct {
	db         *store
	readers    *sql.DB
	b          Backend
	slug       string
	storeID    string
	now        func() time.Time
	log        *slog.Logger
	ctx        context.Context
	cancel     context.CancelFunc
	queue      chan *writerWork
	done       chan struct{}
	admit      sync.RWMutex
	stop       sync.Once
	workers    sync.WaitGroup
	measure    *directoryMeasure
	hook       func(string, *sql.Tx)
	spills     *SpillManager
	spillDir   string
	live       *liveHub
	transports sync.WaitGroup
}

// Open initializes a notebook, or resumes its durable state after a restart.
func Open(dir string, b Backend, opts Options) (*Instance, bool, error) {
	if b == nil {
		return nil, false, fmt.Errorf("notebook backend is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	s, fresh, err := openStore(filepath.Join(dir, dbName))
	if err != nil {
		return nil, false, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	in := &Instance{db: s, b: b, slug: opts.Slug, now: opts.Now, log: opts.Log, ctx: ctx, cancel: cancel,
		queue: make(chan *writerWork, 128), done: make(chan struct{})}
	fail := func(err error) (*Instance, bool, error) {
		cancel()
		if in.readers != nil {
			err = errors.Join(err, in.readers.Close())
		}
		return nil, false, errors.Join(err, s.close())
	}
	state, err := s.state(ctx)
	if err != nil {
		return fail(err)
	}
	in.storeID = state.Store
	// A crash after schema creation but before the first content commit must
	// finish initialization rather than adopt an empty notebook.
	created := fresh || state.Seq == 0
	if created {
		err = s.transaction(ctx, func(tx *sql.Tx) error {
			result, err := applyEdit(ctx, tx, editOptions{Seq: 1, Now: in.now()}, []Operation{
				CreatePageOp{CreateFields: CreateFields{Ref: "first"}, Title: opts.Title},
			})
			if err != nil {
				return err
			}
			if len(result.Refs) != 1 {
				return fmt.Errorf("notebook initialization did not create its page")
			}
			if _, err := tx.ExecContext(ctx, `UPDATE meta SET seq='1' WHERE singleton=1`); err != nil {
				return err
			}
			if err := syncContentFiles(ctx, tx, 1); err != nil {
				return err
			}
			_, err = recordOutbox(ctx, tx, "created", "notebook created", "", nil, in.now(), true)
			return err
		})
		if err != nil {
			return fail(err)
		}
	}
	u := url.URL{Scheme: "file", Path: s.path}
	in.readers, err = sql.Open("sqlite", u.String()+"?mode=ro&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)")
	if err != nil {
		return fail(err)
	}
	in.readers.SetMaxOpenConns(8)
	in.readers.SetMaxIdleConns(8)
	if err := in.readers.PingContext(ctx); err != nil {
		return fail(err)
	}
	if err := in.initLiveTracking(ctx); err != nil {
		return fail(err)
	}
	manager := opts.Spills
	if manager == nil {
		in.spillDir, err = os.MkdirTemp("", "flats-notebook-spills-")
		if err != nil {
			return fail(err)
		}
		manager = NewSpillManager(in.spillDir, 0, in.now)
		context.AfterFunc(in.ctx, func() { _ = manager.Close() })
	}
	manager.mu.Lock()
	err = manager.initErr
	if manager.closed {
		err = errSpillUnavailable
	}
	manager.mu.Unlock()
	if err != nil {
		return fail(err)
	}
	in.spills = manager.forInstance()
	in.useHistorySpillManager(in.spills)
	in.live = newLiveHub()
	in.measure = newDirectoryMeasure(in)
	in.workers.Add(2)
	go in.runWriter()
	go in.measure.run()
	in.measure.refresh()
	if created {
		in.b.OutboxReady()
	}
	return in, created, nil
}

// Stop refuses new work, cancels readers, and waits before releasing storage.
func (in *Instance) Stop() { in.Shutdown("") }

// Shutdown terminates this instance; transport owners cancel on in.ctx.
func (in *Instance) Shutdown(code string) {
	in.stop.Do(func() {
		in.admit.Lock()
		in.closeLive(code)
		in.cancel()
		in.admit.Unlock()
		in.workers.Wait()
		in.transports.Wait()
		if err := os.RemoveAll(in.spillDir); err != nil {
			in.log.Warn("notebook scratch cleanup", "err", err)
		}
		if err := errors.Join(in.readers.Close(), in.db.close()); err != nil {
			in.log.Warn("notebook close", "code", code, "err", err)
		}
	})
}

// StoreID is the immutable live-store identity, not a revision identifier.
func (in *Instance) StoreID() string { return in.storeID }

// ReadTx pins a snapshot at a writer barrier, then releases the writer while
// the caller consumes its consistent read-only transaction.
func (in *Instance) ReadTx(ctx context.Context, fn func(*sql.Tx, Sequence) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(in.ctx, cancel)
	defer stop()
	conn, err := in.readers.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var tx *sql.Tx
	var seq Sequence
	err = in.execute(ctx, &writerWork{barrier: func() error {
		var err error
		tx, err = conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT seq FROM meta WHERE singleton=1`).Scan(&raw); err != nil {
			return errors.Join(err, tx.Rollback())
		}
		n, err := parseDBDecimal(raw)
		seq = Sequence(n)
		return err
	}})
	if err != nil {
		if tx != nil {
			_ = tx.Rollback()
		}
		return err
	}
	defer tx.Rollback()
	return fn(tx, seq)
}

// Barrier waits for all work admitted before this call to commit.
func (in *Instance) Barrier(ctx context.Context) error {
	return in.execute(ctx, &writerWork{barrier: func() error { return nil }})
}

// Remeasure refreshes host usage outside the writer after host-side changes.
func (in *Instance) Remeasure(ctx context.Context) error { return in.measure.remeasure(ctx) }
