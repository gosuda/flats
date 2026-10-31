package notebook

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

// openNotebookFixture makes the asynchronous first measurement explicit in test setup.
func openNotebookFixture(t *testing.T, dir string, b Backend, opts Options) (*Instance, bool, error) {
	t.Helper()
	in, created, err := Open(dir, b, opts)
	if err != nil {
		return in, created, err
	}
	t.Cleanup(in.Stop)
	if err := in.Remeasure(t.Context()); err != nil {
		t.Fatalf("initial measurement: %v", err)
	}
	return in, created, nil
}

func TestWriterReadSnapshotDoesNotBlockWriter(t *testing.T) {
	f := newWriterFixture(t)
	pinned, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- f.in.ReadTx(context.Background(), func(tx *sql.Tx, seq Sequence) error {
			r, err := loadRecord(context.Background(), tx, f.page.ID)
			if err != nil {
				return err
			}
			if seq != 1 || r.Item.Data.(Page).Title != "First" {
				return errors.New("bad initial snapshot")
			}
			close(pinned)
			<-release
			r, err = loadRecord(context.Background(), tx, f.page.ID)
			if err != nil {
				return err
			}
			if r.Item.Data.(Page).Title != "First" {
				return errors.New("mixed snapshot state")
			}
			if _, err := tx.ExecContext(context.Background(), `UPDATE meta SET seq='99'`); err == nil {
				return errors.New("read-only transaction accepted a write")
			}
			return nil
		})
	}()
	<-pinned
	ack, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, writerAction(f.page, "Concurrent"))
	if err != nil || ack.Summary.Applied != 1 {
		t.Fatalf("writer blocked/refused: %+v %v", ack, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := writerTitle(t, f.in, f.page); got != "Concurrent" {
		t.Fatal(got)
	}
}

func TestWriterStopCancelsReadersAndJoinsAdmission(t *testing.T) {
	f := newWriterFixture(t)
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- f.in.ReadTx(context.Background(), func(tx *sql.Tx, _ Sequence) error {
			close(started)
			<-f.in.ctx.Done()
			// database/sql rolls the transaction back from its own goroutine
			// after the context ends, so one more read can still succeed.
			deadline := time.Now().Add(5 * time.Second)
			for {
				_, err := loadRecord(context.Background(), tx, f.page.ID)
				if err != nil || time.Now().After(deadline) {
					return err
				}
				time.Sleep(time.Millisecond)
			}
		})
	}()
	<-started
	var wg sync.WaitGroup
	for range 30 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = f.in.Barrier(context.Background()) }()
	}
	f.in.Shutdown("unavailable")
	wg.Wait()
	if err := <-done; err == nil {
		t.Fatal("stopped read retained a transaction")
	}
	writerReason(t, f.in.Barrier(t.Context()), "stopped", true)
}

func TestWriterInterruptedInitialization(t *testing.T) {
	dir := t.TempDir()
	s, _, err := openStore(dir + "/" + dbName)
	if err != nil {
		t.Fatal(err)
	}
	store, err := s.state(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	in, created, err := openNotebookFixture(t, dir, &writerBackend{}, Options{Slug: "notes", Title: "Recovered init"})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Stop()
	if !created || in.StoreID() != store.Store || writerCount(t, in, "pages") != 1 {
		t.Fatal("initialization was not resumed")
	}
	entries, err := in.Outbox(t.Context(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Kind != "created" {
		t.Fatalf("created entries=%+v", entries)
	}
	var seq string
	if err := in.readers.QueryRowContext(t.Context(), `SELECT seq FROM meta`).Scan(&seq); err != nil || seq != "1" {
		t.Fatalf("state=%s %v", seq, err)
	}
}

func TestWriterReadPoolPressureDoesNotEnterWriter(t *testing.T) {
	f := newWriterFixture(t)
	hold := make(chan struct{})
	started := make(chan struct{}, 8)
	done := make(chan error, 9)
	for range 8 {
		go func() {
			done <- f.in.ReadTx(context.Background(), func(*sql.Tx, Sequence) error { started <- struct{}{}; <-hold; return nil })
		}()
	}
	for range 8 {
		<-started
	}
	ctx, cancel := context.WithCancel(t.Context())
	go func() { done <- f.in.ReadTx(ctx, func(*sql.Tx, Sequence) error { return nil }) }()
	defer func() {
		cancel()
		close(hold)
		for range 9 {
			<-done
		}
	}()
	deadline := time.Now().Add(time.Second)
	for f.in.readers.Stats().WaitCount == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.in.readers.Stats().WaitCount == 0 {
		t.Fatal("ninth reader never reached pool pressure")
	}
	barrier, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if err := f.in.Barrier(barrier); err != nil {
		t.Fatalf("read pool pressure blocked writer: %v", err)
	}
}

func TestWriterReadCancellationBeforeBarrier(t *testing.T) {
	f := newWriterFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	err := f.in.ReadTx(ctx, func(*sql.Tx, Sequence) error { called = true; return nil })
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("cancelled barrier: called=%t %v", called, err)
	}
	deadline, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := f.in.Barrier(deadline); err != nil {
		t.Fatal(err)
	}
}
