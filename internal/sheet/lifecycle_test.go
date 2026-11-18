package sheet

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// unwrittenCrash returns what a crash leaves of a sheet whose open wrote
// its schema and the row of meta, at sequence 0, but not its first commit:
// sheet.db and its -wal file, without the -shm index.
func unwrittenCrash(t *testing.T) *env {
	t.Helper()
	src := t.TempDir()
	db, fresh, _, err := openDB(filepath.Join(src, dbName))
	if err != nil || !fresh {
		t.Fatal(fresh, err)
	}
	dir := t.TempDir()
	for _, name := range []string{dbName, dbName + "-wal"} {
		copyFile(t, filepath.Join(src, name), filepath.Join(dir, name))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, dbName+"-wal")); err != nil || info.Size() == 0 {
		t.Fatalf("the crash left no log: %v", err)
	}
	clock := newClock()
	return &env{t: t, dir: dir, b: newBackend(), clock: clock, opts: Options{Slug: "sheet", Now: clock.Now, Location: time.UTC}}
}

// holdOpeners holds every opener of a log where SQLite starts to recover
// it (logOpening), until the returned function lets them go. reached
// receives each instance held.
func holdOpeners(t *testing.T) (reached <-chan *Instance, let func()) {
	t.Helper()
	at, open := make(chan *Instance, 16), make(chan struct{})
	hold := func(in *Instance) {
		at <- in
		<-open
	}
	if !logOpening.CompareAndSwap(nil, &hold) {
		t.Fatal("another test holds logs")
	}
	let = sync.OnceFunc(func() {
		logOpening.CompareAndSwap(&hold, nil)
		close(open)
	})
	t.Cleanup(let)
	return at, let
}

func heldOpener(t *testing.T, reached <-chan *Instance, in *Instance) {
	t.Helper()
	select {
	case got := <-reached:
		if got != in {
			t.Fatal("another load opened a log")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the load opened no log")
	}
}

// stopsInTime stops in and fails the test unless the stop returns while
// whatever the test holds stays held.
func stopsInTime(t *testing.T, in *Instance) {
	t.Helper()
	stopped := make(chan struct{})
	go func() {
		in.Stop()
		close(stopped)
	}()
	gateSettlesWith(t, "stop", func() bool {
		select {
		case <-stopped:
			return true
		default:
			return false
		}
	})
}

// A read outside the writer runs only once sheet.db is attached and
// checked. A crash before the first commit leaves a schema at sequence 0
// with a log, which the load opens and then writes the first commit to.
// While that commit is held, stored calls and the outbox are refused as
// calculating and read nothing; after it they answer. When the commit
// fails, every later read is refused with the load's failure.
func TestReadsWaitForTheFirstCommitOfARecoveredSheet(t *testing.T) {
	ownTempDir(t)
	ctx := context.Background()
	stored := func() (Update, error) {
		return DecodeUpdate([]byte(`{"state":"s_000000000000.1","op_id":"early","ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["early"]]}]}`))
	}
	reads := func(t *testing.T, in *Instance) []error {
		t.Helper()
		u, err := stored()
		if err != nil {
			t.Fatal(err)
		}
		errs := make([]error, 8)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i%2 == 0 {
					_, errs[i] = in.Outbox(ctx, 0, 10)
				} else {
					_, errs[i] = in.Update(ctx, u, 0)
				}
			}()
		}
		wg.Wait()
		return errs
	}
	for _, failing := range []bool{false, true} {
		t.Run(fmt.Sprintf("failing %v", failing), func(t *testing.T) {
			e := unwrittenCrash(t)
			reached, let := holdOpeners(t)
			in, _, err := Open(e.dir, e.b, e.opts)
			if err != nil {
				t.Fatal(err)
			}
			e.in, e.b.in = in, in
			defer in.Stop()
			heldOpener(t, reached, in)
			// The opener is held, and the load waits for it, so neither
			// reads these yet.
			committing, finish := make(chan struct{}), make(chan struct{})
			// A failure while the first commit is held lets it go before
			// the deferred stop waits for the load.
			letFinish := sync.OnceFunc(func() { close(finish) })
			defer letFinish()
			if failing {
				in.fault = func() error { return errors.New("the disk is full") }
			} else {
				var once sync.Once
				in.hook = func(step string) {
					if step == "commit" {
						once.Do(func() {
							close(committing)
							<-finish
						})
					}
				}
			}
			let()
			if failing {
				if err := in.await(ctx, 0); refusal(err).Reason != "load" {
					t.Fatalf("the load answered %v", err)
				}
				for round := 0; round < 2; round++ {
					for i, err := range reads(t, in) {
						if refusal(err).Reason != "load" {
							t.Fatalf("read %d after the failed first commit answered %v", i, err)
						}
					}
				}
				if in.StoreID() != "" {
					t.Fatal("the failed load left a store")
				}
				return
			}
			<-committing
			for i, err := range reads(t, in) {
				if refusal(err).Reason != "calculating" {
					t.Fatalf("read %d during the first commit answered %v", i, err)
				}
			}
			letFinish()
			if err := in.await(ctx, 0); err != nil {
				t.Fatal(err)
			}
			entries, err := in.Outbox(ctx, 0, 10)
			if err != nil || len(entries) != 1 || entries[0].Kind != "created" {
				t.Fatalf("the outbox after the first commit: %+v, %v", entries, err)
			}
			e.mustAgent(`{"op_id":"after","ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["after"]]}]}`)
			if got := e.text("A1"); got != "after" {
				t.Fatalf("A1 holds %q", got)
			}
		})
	}
}

// A stop during the recovery of a log does not wait for SQLite, but the
// recovery keeps its slot at the gate until SQLite returns, and the
// sheet's storage stays held until the opener closed what it opened. With
// one slot and with several, loads that reach their recovery and are
// stopped there, one after another, never run more recoveries at once
// than the gate has slots: the next load waits in line, and is stopped
// there. Let go, every opener leaves the gate, every sheet's storage is
// released, and every sheet opens again with every commit.
func TestStopsDuringRecoveryKeepTheirSlots(t *testing.T) {
	ownTempDir(t)
	ctx := context.Background()
	for _, slots := range gateSizes {
		t.Run(fmt.Sprintf("%d slots", slots), func(t *testing.T) {
			gateSlots(t, slots)
			sheets := make([]*env, slots+2)
			texts := make([]string, len(sheets))
			for i := range sheets {
				sheets[i], texts[i] = loggedSheet(t, "logged", 2*copyChunk)
			}
			var mu sync.Mutex
			active, most := 0, 0
			reached, let := make(chan *Instance, len(sheets)), make(chan struct{})
			hold := func(in *Instance) {
				mu.Lock()
				active++
				most = max(most, active)
				mu.Unlock()
				reached <- in
				<-let
				mu.Lock()
				active--
				mu.Unlock()
			}
			if !logOpening.CompareAndSwap(nil, &hold) {
				t.Fatal("another test holds logs")
			}
			letGo := sync.OnceFunc(func() {
				logOpening.CompareAndSwap(&hold, nil)
				close(let)
			})
			defer letGo()
			for i, e := range sheets {
				in, _, err := Open(e.dir, e.b, e.opts)
				if err != nil {
					t.Fatal(err)
				}
				e.in, e.b.in = in, in
				gateSettlesWith(t, fmt.Sprintf("load %d at its recovery or in line", i), func() bool {
					select {
					case got := <-reached:
						if got != in {
							t.Fatal("another load opened a log")
						}
						return true
					default:
					}
					_, line := gateState()
					return line == 1
				})
				stopsInTime(t, in)
			}
			mu.Lock()
			ran := most
			mu.Unlock()
			if ran > slots {
				t.Fatalf("%d recoveries ran at once with %d slots", ran, slots)
			}
			gateSettles(t, "while the stopped recoveries run", 0, 0)
			short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
			err := AwaitReleased(short, sheets[0].dir)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("the storage of a stopped recovery was released: %v", err)
			}
			letGo()
			gateSettles(t, "after the recoveries", slots, 0)
			for i, e := range sheets {
				wait, cancel := context.WithTimeout(ctx, 30*time.Second)
				err := AwaitReleased(wait, e.dir)
				cancel()
				if err != nil {
					t.Fatalf("the storage of sheet %d: %v", i, err)
				}
				e.open()
				if got := e.text("A40"); got != texts[i] {
					t.Fatalf("A40 of sheet %d holds %.20q, want %.20q", i, got, texts[i])
				}
				e.in.Stop()
			}
			noPrivateCopies(t)
		})
	}
}

// takeFree takes every slot of the gate that is free with passes that
// never look at it, and returns the function that gives them back.
func takeFree(t *testing.T) (release func()) {
	t.Helper()
	free, _ := gateState()
	turns := make([]*calcTurn, free)
	for i := range turns {
		turn, ok := calcSlots.enter(nil, context.Background())
		if !ok {
			t.Fatal("no slot for the test")
		}
		turns[i] = turn
	}
	release = sync.OnceFunc(func() {
		for _, turn := range turns {
			turn.leave()
		}
	})
	t.Cleanup(release)
	return release
}

// An instance opened on a sheet.db whose stopped instance's opener still
// recovers its log opens nothing until that opener closed what it opened.
// The log is short, so that close checkpoints it and removes it, between
// the new open and the copy of the log its load would make. The new load
// then opens sheet.db as it finds it, and holds every commit.
func TestAnOpenWaitsForTheRecoveryAStopLeft(t *testing.T) {
	ownTempDir(t)
	ctx := context.Background()
	e, text := loggedSheet(t, "logged", copyChunk)
	wal := filepath.Join(e.dir, dbName+"-wal")
	if info, err := os.Stat(wal); err != nil || info.Size() > walTruncateBytes {
		t.Fatalf("the log is not short: %v", err)
	}
	reached, let := holdOpeners(t)
	first, _, err := Open(e.dir, e.b, e.opts)
	if err != nil {
		t.Fatal(err)
	}
	heldOpener(t, reached, first)
	stopsInTime(t, first)
	// The next load, should it look at the log before the opener closed,
	// waits for a slot until the log is gone.
	release := takeFree(t)
	next, _, err := Open(e.dir, e.b, e.opts)
	if err != nil {
		t.Fatal(err)
	}
	e.in, e.b.in = next, next
	defer next.Stop()
	if next.StoreID() != "" {
		t.Fatal("the open read sheet.db while the stopped opener held it")
	}
	let()
	// The closing opener removes the log. A load that waits for the
	// opener's release may open sheet.db right after, with a log of its
	// own, before the removal is seen here; one that does not wait is in
	// line for a slot until release.
	gateSettlesWith(t, "removal of the log by the closing opener", func() bool {
		_, err := os.Stat(wal)
		return errors.Is(err, os.ErrNotExist) || next.StoreID() != ""
	})
	release()
	if err := next.await(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if got := e.text("A40"); got != text {
		t.Fatalf("A40 holds %.20q, want %.20q", got, text)
	}
	next.Stop()
	noPrivateCopies(t)
}

// A stop of a load that attached a sheet.db with a long log, here held at
// the integrity check of its preflight, does no checkpoint of the log, so
// sheet.db keeps its bytes and the log its frames. The next open recovers
// every commit from the log.
func TestStopAfterAttachmentLeavesTheLog(t *testing.T) {
	ownTempDir(t)
	e, text := loggedSheet(t, heavySlug, 6*copyChunk)
	w := watchLoads(t)
	w.holdAt("integrity")
	in, _, err := Open(e.dir, e.b, e.opts)
	if err != nil {
		t.Fatal(err)
	}
	e.in, e.b.in = in, in
	w.heldAt(t, 1, "integrity")
	if in.StoreID() == "" {
		t.Fatal("the held load has not attached sheet.db")
	}
	wal := filepath.Join(e.dir, dbName+"-wal")
	file := func() (int64, [sha256.Size]byte) {
		t.Helper()
		info, err := os.Stat(wal)
		if err != nil {
			t.Fatalf("the log: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(e.dir, dbName))
		if err != nil {
			t.Fatal(err)
		}
		return info.Size(), sha256.Sum256(data)
	}
	logged, before := file()
	if logged <= walTruncateBytes {
		t.Fatalf("the log of %d bytes is not long", logged)
	}
	stopsInTime(t, in)
	if size, after := file(); size != logged || after != before {
		t.Fatalf("the stop checkpointed the log: it holds %d bytes of %d, sheet.db changed %v", size, logged, after != before)
	}
	w.holdAt("")
	e.open()
	if got := e.text("A40"); got != text {
		t.Fatalf("A40 holds %.20q, want %.20q", got, text)
	}
	e.in.Stop()
	noPrivateCopies(t)
}

// logState returns the size of the -wal file of the sheet in dir and the
// hash of its sheet.db.
func logState(t *testing.T, dir string) (int64, [sha256.Size]byte) {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, dbName+"-wal"))
	if err != nil {
		t.Fatalf("the log: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, dbName))
	if err != nil {
		t.Fatal(err)
	}
	return info.Size(), sha256.Sum256(data)
}

// openGroups counts the edit groups sheet.db holds open.
func openGroups(t *testing.T, in *Instance) int {
	t.Helper()
	var n int
	if err := in.rdb.QueryRow(`SELECT count(*) FROM groups`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A stop of a sheet that loaded the long log a crash left, with a
// person's edit group open, commits the group's final boundary without a
// checkpoint of the log: sheet.db keeps its bytes, and the log keeps
// every frame it held and gains the boundary's. The next open recovers
// every commit, the person's edit and its revision included.
func TestAStopAfterALoadLeavesTheLog(t *testing.T) {
	ownTempDir(t)
	ctx := context.Background()
	e, text := loggedSheet(t, "logged", 6*copyChunk, func(e *env) {
		e.person("ann").set("B2", "edited")
	})
	e.open()
	if n := openGroups(t, e.in); n != 1 {
		t.Fatalf("the loaded sheet holds %d open groups, want 1", n)
	}
	before, err := e.in.Outbox(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	logged, hash := logState(t, e.dir)
	if logged <= walTruncateBytes {
		t.Fatalf("the log of %d bytes is not long", logged)
	}
	e.in.Stop()
	if size, after := logState(t, e.dir); size <= logged || after != hash {
		t.Fatalf("the stop checkpointed the log: it holds %d bytes of %d, sheet.db changed %v", size, logged, after != hash)
	}
	e.open()
	if got := e.text("A40"); got != text {
		t.Fatalf("A40 holds %.20q, want %.20q", got, text)
	}
	if got := e.text("B2"); got != "edited" {
		t.Fatalf("B2 holds %q", got)
	}
	if n := openGroups(t, e.in); n != 0 {
		t.Fatalf("the reopened sheet holds %d open groups", n)
	}
	after, err := e.in.Outbox(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("the outbox holds %d entries after the stop, want %d", len(after), len(before)+1)
	}
	for i, entry := range before {
		if after[i].Seq != entry.Seq || after[i].Kind != entry.Kind || after[i].StateSeq != entry.StateSeq {
			t.Fatalf("outbox entry %d is %+v after the stop, was %+v", i, after[i], entry)
		}
	}
	if last := after[len(after)-1]; last.Kind != "person" || !slices.Equal(last.Authors, []string{"ann"}) {
		t.Fatalf("the final boundary is %s by %v", last.Kind, last.Authors)
	}
	e.in.Stop()
	noPrivateCopies(t)
}

// With no reader holding the log, each commit leaves it at most
// walTruncateBytes long: the writer truncates a longer log after its
// commits, which SQLite's automatic checkpoint, off for the writing
// connection, does no more. A log a reader held past that size is
// truncated by the next commit of any kind once the reader lets go, here
// the acknowledgement of an import.
func TestCommitsKeepTheLogBounded(t *testing.T) {
	ownTempDir(t)
	e := start(t, unhurried)
	wal := filepath.Join(e.dir, dbName+"-wal")
	size := func() int64 {
		t.Helper()
		info, err := os.Stat(wal)
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	write := func(i int) {
		row := `["` + fmt.Sprintf("%d%s", i, strings.Repeat("a", 32000)) + `"],`
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + strings.TrimSuffix(strings.Repeat(row, 40), ",") + `]}]}`)
	}
	ann := e.person("ann")
	most := int64(0)
	for i := 0; i < 8; i++ {
		write(i)
		ann.set("B1", fmt.Sprint(i))
		n := size()
		if n > walTruncateBytes {
			t.Fatalf("after write %d the log holds %d bytes", i, n)
		}
		most = max(most, n)
	}
	if most == 0 {
		t.Fatal("the writes left no log")
	}
	// A failure while the reader holds the log skips the checked release
	// below. The deferred calls let the log go then.
	reader := rawDB(t, filepath.Join(e.dir, dbName))
	defer reader.Close()
	tx, err := reader.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM meta`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	for i := 8; size() <= walTruncateBytes; i++ {
		write(i)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	e.b.importAll(t)
	if n := size(); n > walTruncateBytes {
		t.Fatalf("after the acknowledgement the log holds %d bytes", n)
	}
}

// stoppedLog is the length of the log whose checkpoint a stop ends. Its
// pages take long enough to copy that the copy is still at work when the
// stop comes after the first of them.
const stoppedLog = 16 << 20

// A stop that comes while the writer checkpoints a long log ends the
// checkpoint between two of the pages it copies. A reader held the log
// while writes made it long and a person's edit left a group open. The
// test holds the next commit where its checkpoint starts, lets the reader
// go, and then lets the checkpoint run. SQLite copies the pages in the
// order of their numbers, so sheet.db grows once the checkpoint has
// written the pages it had. The stop comes then and returns. The log
// keeps every frame and gains the final boundary's, and sheet.db is
// shorter than the whole checkpoint would have left it, so the copy
// ended before its last pages. The sheet opens again with every commit,
// the person's edit and the final boundary of its group.
func TestAStopEndsARunningCheckpoint(t *testing.T) {
	ownTempDir(t)
	ctx := context.Background()
	e := start(t, unhurried)
	var armed atomic.Bool
	entered, let := make(chan struct{}), make(chan struct{})
	e.in.hook = func(step string) {
		if step == "checkpoint" && armed.CompareAndSwap(true, false) {
			close(entered)
			<-let
		}
	}
	// A failure while the commit is held lets it go before the cleanup's
	// stop waits for the writer.
	letGo := sync.OnceFunc(func() { close(let) })
	defer letGo()
	db, wal := filepath.Join(e.dir, dbName), filepath.Join(e.dir, dbName+"-wal")
	// A log that a whole checkpoint and the close took away has no bytes.
	size := func(path string) int64 {
		t.Helper()
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	reader := rawDB(t, db)
	defer reader.Close()
	tx, err := reader.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM meta`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	text := ""
	for i := 0; size(wal) < stoppedLog; i++ {
		text = fmt.Sprintf("%d%s", i, strings.Repeat("a", 32000))
		row := `["` + text + `"],`
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + strings.TrimSuffix(strings.Repeat(row, 40), ",") + `]}]}`)
	}
	e.person("ann").set("B2", "edited")
	before, err := e.in.Outbox(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	// A commit that changes nothing comes to the checkpoint, unless the
	// maintenance tick's commit does first. Either is held there, the
	// reader lets go meanwhile, and the stop may refuse the
	// acknowledgement.
	armed.Store(true)
	committed := make(chan error, 1)
	go func() { committed <- e.in.Acknowledge(ctx, 0, 0) }()
	select {
	case <-entered:
	case err := <-committed:
		t.Fatalf("a commit ended without a checkpoint of the log of %d bytes: %v", size(wal), err)
	case <-time.After(30 * time.Second):
		t.Fatal("no checkpoint began")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	logged, small := size(wal), size(db)
	var pages int64
	if err := e.in.rdb.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	whole := pages * e.in.pageSize
	if logged <= walTruncateBytes || small >= whole/2 {
		t.Fatalf("the log holds %d bytes, and sheet.db %d of the %d the checkpoint leaves", logged, small, whole)
	}
	letGo()
	for deadline := time.Now().Add(30 * time.Second); size(db) == small; runtime.Gosched() {
		select {
		case err := <-committed:
			t.Fatalf("the checkpoint ended with sheet.db at %d bytes and the log at %d: %v", size(db), size(wal), err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the checkpoint copied no page")
		}
	}
	stopsInTime(t, e.in)
	<-committed
	left, grown := size(wal), size(db)
	if left <= logged || grown <= small || grown >= whole {
		t.Fatalf("the stop did not end the copy: the log holds %d bytes, was %d, and sheet.db %d, was %d, of the %d the whole checkpoint leaves",
			left, logged, grown, small, whole)
	}
	t.Logf("the stop ended the copy with sheet.db at %d of %d bytes", grown, whole)

	e.open()
	if got := e.text("A40"); got != text {
		t.Fatalf("A40 holds %.20q, want %.20q", got, text)
	}
	if got := e.text("B2"); got != "edited" {
		t.Fatalf("B2 holds %q", got)
	}
	if n := openGroups(t, e.in); n != 0 {
		t.Fatalf("the reopened sheet holds %d open groups", n)
	}
	after, err := e.in.Outbox(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("the outbox holds %d entries after the stop, want %d", len(after), len(before)+1)
	}
	for i, entry := range before {
		if after[i].Seq != entry.Seq || after[i].Kind != entry.Kind || after[i].StateSeq != entry.StateSeq {
			t.Fatalf("outbox entry %d is %+v after the stop, was %+v", i, after[i], entry)
		}
	}
	if last := after[len(after)-1]; last.Kind != "person" || !slices.Equal(last.Authors, []string{"ann"}) {
		t.Fatalf("the final boundary is %s by %v", last.Kind, last.Authors)
	}
	e.in.Stop()
	noPrivateCopies(t)
}

// A stop that comes after a load's last recalculation, before the load
// publishes the workbook, does not race the publication: the writer
// reads the workbook only once the load has ended, which it does when it
// is let go, and the stop then returns. The sheet opens again with its
// write.
func TestAStopBeforeTheLoadPublishesIsOrdered(t *testing.T) {
	ownTempDir(t)
	e := start(t, unhurried)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["kept"]]}]}`)
	e.in.Stop()
	reached, let := make(chan *Instance, 1), make(chan struct{})
	hold := func(in *Instance) {
		reached <- in
		<-let
	}
	if !loadPublishing.CompareAndSwap(nil, &hold) {
		t.Fatal("another test holds loads")
	}
	letGo := sync.OnceFunc(func() {
		loadPublishing.CompareAndSwap(&hold, nil)
		close(let)
	})
	defer letGo()
	in, _, err := Open(e.dir, e.b, e.opts)
	if err != nil {
		t.Fatal(err)
	}
	e.in, e.b.in = in, in
	select {
	case got := <-reached:
		if got != in {
			t.Fatal("another load was held")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the load did not calculate")
	}
	stopped := make(chan struct{})
	go func() {
		in.Stop()
		close(stopped)
	}()
	gateSettlesWith(t, "stop of the instance", func() bool { return in.ctx.Err() != nil })
	letGo()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("the stop did not return after the load published")
	}
	if err := in.await(context.Background(), 0); err != nil {
		t.Fatalf("the stopped load answered %v", err)
	}
	e.open()
	if got := e.text("A1"); got != "kept" {
		t.Fatalf("A1 holds %q", got)
	}
}

// A wait for the readiness of a sheet whose load succeeds between the
// wait's reads finds the sheet usable, never the load's refusal: when it
// asks without waiting, with a context that has ended, as the host does
// before a history read, when it waits, and when the outbox asks for it.
func TestALoadThatSucceedsDuringAReadinessCheckIsUsable(t *testing.T) {
	ownTempDir(t)
	ctx := context.Background()
	ended, cancel := context.WithCancel(ctx)
	cancel()
	for _, c := range []struct {
		name string
		wait func(in *Instance) error
	}{
		{"ended context", func(in *Instance) error { return in.AwaitChecked(ended) }},
		{"waiting", func(in *Instance) error { return in.AwaitChecked(ctx) }},
		{"outbox", func(in *Instance) error {
			entries, err := in.Outbox(ctx, 0, 0)
			if err == nil && len(entries) == 0 {
				return errors.New("the outbox is empty")
			}
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _ := loggedSheet(t, "logged", copyChunk)
			reached, let := holdOpeners(t)
			in, _, err := Open(e.dir, e.b, e.opts)
			if err != nil {
				t.Fatal(err)
			}
			e.in, e.b.in = in, in
			defer in.Stop()
			heldOpener(t, reached, in)
			at, release, err := HoldReadiness()
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			got := make(chan error, 1)
			go func() { got <- c.wait(in) }()
			select {
			case held := <-at:
				if held != in {
					t.Fatal("another instance was held")
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the wait did not look at the load")
			}
			// The wait has read that the load had not ended. It ends now,
			// with sheet.db attached and checked.
			let()
			if err := in.await(ctx, 0); err != nil {
				t.Fatal(err)
			}
			release()
			select {
			case err := <-got:
				if err != nil {
					t.Fatalf("the wait answered %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the wait did not return")
			}
		})
	}
}
