package sheet

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// outboxRows counts the outbox of a running instance.
func outboxRows(t *testing.T, e *env) int {
	t.Helper()
	var n int
	if err := e.in.rdb.QueryRow(`SELECT count(*) FROM outbox`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The writer leaves at most maxOutboxRows entries: with one entry short of
// the admission limit, a person's edit and an agent call, which closes the
// person's group first, reach it. Then an agent call and a rollback are
// refused, a tick appends nothing, and the sheet opens again and loads.
// One row more is refused by the load before it reads the rows, and so
// are 100,000 more, which the load allocates no more for than for the
// sheet at the most.
func TestOutboxAtItsMostLoads(t *testing.T) {
	ctx := context.Background()
	e := start(t)
	rev := closeRevision(e)
	files, _, release, err := e.b.RevisionFiles(ctx, rev)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ann := e.person("Ann")
	for i := range maxOutboxPending - 1 {
		e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"Z9","rows":[[%d]]}]}`, i))
	}
	if got := outboxRows(t, e); got != maxOutboxPending-1 {
		t.Fatalf("the outbox holds %d entries", got)
	}
	ann.set("A1", "ann")
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[[1]]}]}`)
	if got := outboxRows(t, e); got != maxOutboxRows {
		t.Fatalf("the outbox holds %d entries, want %d", got, maxOutboxRows)
	}
	if _, err := e.agent(`{"ops":[{"op":"write","tab":"Sheet1","at":"B2","rows":[[1]]}]}`); refusal(err).Reason != "outbox_backpressure" {
		t.Fatalf("an agent call at the most answered %v", err)
	}
	if err := e.in.Rollback(ctx, files, "Ann", "sheet-rollback:9:x"); refusal(err).Reason != "outbox_backpressure" {
		t.Fatalf("a rollback at the most answered %v", err)
	}
	e.clock.Add(time.Hour)
	e.tick()
	e.restart()
	if got := outboxRows(t, e); got != maxOutboxRows {
		t.Fatalf("after a tick and a restart the outbox holds %d entries", got)
	}
	e.in.Stop()

	copyRow := `INSERT INTO outbox(kind,authors,message,op_id,state_seq,calc_date,tabs,files,created)
		SELECT kind,authors,message,op_id,state_seq,calc_date,tabs,files,created FROM outbox WHERE seq=(SELECT max(seq) FROM outbox)`
	_, atMost := loadAllocs(t, e)
	for name, statement := range map[string]string{
		"one row more": copyRow,
		"100,000 rows more": `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<100000) ` +
			strings.Replace(copyRow, "FROM outbox WHERE", "FROM n, outbox WHERE", 1),
	} {
		t.Run(name, func(t *testing.T) {
			damaged := copied(t, e, statement)
			err, got := loadAllocs(t, damaged)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("more than the %d rows", maxOutboxRows)) {
				t.Fatalf("the load answered %v", err)
			}
			t.Logf("refused after %d KiB, %d KiB for the sheet at the most: %v", got>>10, atMost>>10, err)
			if measures && got > atMost {
				t.Errorf("the refused load allocated %d KiB, %d KiB for the sheet at the most", got>>10, atMost>>10)
			}
			in, _, err := Open(damaged.dir, damaged.b, damaged.opts)
			if err != nil {
				t.Fatal(err)
			}
			defer in.Stop()
			if err := in.await(ctx, 0); err == nil {
				t.Fatal("the sheet loaded")
			}
			if _, err := in.Outbox(ctx, 0, 0); err == nil {
				t.Fatal("the outbox was read")
			}
			db := readOnly(t, damaged)
			defer db.Close()
			if _, err := BlobHashes(ctx, db); err == nil {
				t.Fatal("the backup check read the outbox")
			}
		})
	}
}

// The snapshots of a full outbox, maxSnapshotRows of them, load, and one
// more is refused by the load and by the backup check.
func TestSnapshotsAtTheirMostLoad(t *testing.T) {
	full(t, "fills the snapshots of a full outbox")
	ctx := context.Background()
	e := start(t)
	e.in.Stop()
	var have int
	execDB(t, e, func(db *sql.DB) error {
		if err := db.QueryRow(`SELECT count(*) FROM snapshots`).Scan(&have); err != nil {
			return err
		}
		_, err := db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?)
			INSERT INTO snapshots(hash,data,refs) SELECT printf('%064x', i),x'00',1 FROM n`, maxSnapshotRows-have)
		return err
	})
	e.open()
	e.in.Stop()
	db := readOnly(t, e)
	if _, err := BlobHashes(ctx, db); err != nil {
		t.Fatal(err)
	}
	db.Close()
	execDB(t, e, func(db *sql.DB) error {
		_, err := db.Exec(`INSERT INTO snapshots(hash,data,refs) VALUES(printf('%064x', ?),x'00',1)`, maxSnapshotRows+1)
		return err
	})
	wantLoadRefused(t, e, "a snapshot more")
	db = readOnly(t, e)
	defer db.Close()
	if _, err := BlobHashes(ctx, db); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("the backup check answered %v", err)
	}
}
