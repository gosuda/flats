package sheet

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// loggedSheet returns what a crash leaves of a running sheet under slug,
// with a log of at least min bytes: the sheet takes agent writes of long
// texts while a reader holds the log, as the snapshot of a backup does, so
// that no commit truncates it, and sheet.db and its -wal file are copied
// as a killed process leaves them, without the -shm index. edits run on
// the sheet after the writes, while the reader still holds the log. It
// returns the copy and the text of A1.
func loggedSheet(t *testing.T, slug string, min int64, edits ...func(*env)) (*env, string) {
	t.Helper()
	e := start(t, unhurried, func(o *Options) { o.Slug = slug })
	reader := rawDB(t, filepath.Join(e.dir, dbName))
	defer reader.Close()
	tx, err := reader.Begin()
	if err != nil {
		t.Fatal(err)
	}
	// Closing the handle does not end a transaction that is open, which
	// keeps its connection and its hold on the log. A failure before the
	// checked rollback below ends it here.
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM meta`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	text := ""
	for i := 0; ; i++ {
		info, err := os.Stat(filepath.Join(e.dir, dbName+"-wal"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() >= min {
			t.Logf("the log holds %d KiB after %d writes", info.Size()>>10, i)
			break
		}
		text = fmt.Sprintf("%d%s", i, strings.Repeat("a", 32000))
		row := `["` + text + `"],`
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + strings.TrimSuffix(strings.Repeat(row, 40), ",") + `]}]}`)
	}
	for _, edit := range edits {
		edit(e)
	}
	copied := crashCopy(t, e)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	return copied, text
}

// A sheet.db with a log that a crash left is opened by its load: with
// every slot of the gate taken, Open of a sheet whose log is several MiB
// returns without SQLite opening the file, which would build its -shm
// index, and without a check point of the copy of the log. Once a slot is
// free the load copies the log at its check points, recovers it, and the
// sheet holds every commit. It runs with a gate of one slot and with
// several.
func TestLogOfACrashIsOpenedByTheLoad(t *testing.T) {
	ownTempDir(t)
	ctx := context.Background()
	for _, slots := range gateSizes {
		t.Run(fmt.Sprintf("%d slots", slots), func(t *testing.T) {
			gateSlots(t, slots)
			e, text := loggedSheet(t, "logged", 6*copyChunk)
			w := watchLoads(t)
			release := takeSlots(t, slots)
			in, _, err := Open(e.dir, e.b, e.opts)
			if err != nil {
				t.Fatalf("the open read the log: %v", err)
			}
			e.in, e.b.in = in, in
			gateSettles(t, "with the load in line", 0, 1)
			if _, err := os.Stat(filepath.Join(e.dir, dbName+"-shm")); !os.IsNotExist(err) {
				t.Fatalf("the open let SQLite read the log: %v", err)
			}
			if in.StoreID() != "" || w.count(in, "log") != 0 {
				t.Fatal("the open read sheet.db before its load had a slot")
			}
			release()
			if err := in.await(ctx, 0); err != nil {
				t.Fatal(err)
			}
			if got := e.text("A40"); got != text {
				t.Fatalf("A40 holds %.20q, want %.20q", got, text)
			}
			if in.StoreID() == "" {
				t.Fatal("the loaded sheet has no store id")
			}
			if n := w.count(in, "log"); n < 6 {
				t.Fatalf("the copy of the log reached %d check points", n)
			}
			in.Stop()
			noPrivateCopies(t)
		})
	}
}

// The copy of a log takes turns at the gate: loads held while they copy
// their logs hand their slots to two small loads, and a stop ends them
// there, before they open sheet.db. It runs with a gate of one slot and
// with several.
func TestLogCopyTakesTurnsAndStops(t *testing.T) {
	ownTempDir(t)
	for _, slots := range gateSizes {
		t.Run(fmt.Sprintf("%d slots", slots), func(t *testing.T) {
			gateSlots(t, slots)
			held := make([]*env, slots)
			for i := range held {
				held[i], _ = loggedSheet(t, heavySlug, 6*copyChunk)
			}
			w := watchLoads(t)
			w.holdAt("log")
			for _, e := range held {
				in, _, err := Open(e.dir, e.b, e.opts)
				if err != nil {
					t.Fatal(err)
				}
				e.in, e.b.in = in, in
			}
			handOverAndStop(t, w, slots, "log", []string{"preflight", "integrity", "records", "axes", "cells"}, held)
			for _, e := range held {
				if e.in.StoreID() != "" {
					t.Fatal("a load stopped while it copied its log opened sheet.db")
				}
			}
			noPrivateCopies(t)
		})
	}
}

// SQLite cannot interrupt its recovery of a log, so a stop does not wait
// for it: with the opener held where SQLite starts to recover the log for
// the instance's connection, the stop returns, and so does the load. Let
// go, the opener closes what it opened, and the sheet opens again with
// every commit.
func TestStopLeavesTheRecoveryOfALog(t *testing.T) {
	ownTempDir(t)
	ctx := context.Background()
	e, text := loggedSheet(t, "logged", 2*copyChunk)
	reached, let := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hold := func(in *Instance) {
		once.Do(func() { close(reached) })
		<-let
	}
	logOpening.Store(&hold)
	defer logOpening.Store(nil)
	in, _, err := Open(e.dir, e.b, e.opts)
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	stopped := make(chan struct{})
	go func() {
		in.Stop()
		close(stopped)
	}()
	gateSettlesWith(t, "a stop while the log is recovered", func() bool {
		select {
		case <-stopped:
			return true
		default:
			return false
		}
	})
	if err := in.await(ctx, 0); err == nil {
		t.Fatal("the stopped load answered nothing")
	}
	logOpening.Store(nil)
	close(let)
	// The opener's connection closes once it is let go; the next open
	// waits for that before it opens sheet.db.
	e.open()
	if got := e.text("A40"); got != text {
		t.Fatalf("A40 holds %.20q, want %.20q", got, text)
	}
	e.in.Stop()
	noPrivateCopies(t)
}
