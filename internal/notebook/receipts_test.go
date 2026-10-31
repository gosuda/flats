package notebook

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestWriterLargeMappingPersistsAndExpiresWithCommand(t *testing.T) {
	t.Parallel()
	f := newWriterFixture(t)
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		for i := uint64(0); i < 520; i++ {
			id := writerQuotaID(BlockKind, i)
			item := Item{Ref: Ref{ID: id, Inc: 1}, Ver: 1, V: map[GroupKey]Version{}, Data: Block{Page: f.page.ID, Position: fmt.Sprintf("a%04d", i), Content: Content{Kind: Paragraph, Text: RichText{{Text: "small", Marks: []Mark{}}}}}}
			if err := putItem(t.Context(), tx, item); err != nil {
				return err
			}
		}
		return syncContentFiles(t.Context(), tx, 1)
	}); err != nil {
		t.Fatal(err)
	}
	u := Update{Slug: "notes", OpID: "copy-page", Ops: []Operation{DuplicatePageOp{Page: Target{Ref: &f.page}}}}
	result, err := f.in.Update(t.Context(), u, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Mapping.Complete || result.Mapping.Total != 521 || result.Mapping.Cursor == "" || len(result.Refs) != 0 {
		t.Fatalf("not a paged full mapping: complete=%t total=%d cursor=%q", result.Mapping.Complete, result.Mapping.Total, result.Mapping.Cursor)
	}
	body, err := json.Marshal(result)
	if err != nil || len(body) > maxOutcomeBytes {
		t.Fatalf("result size=%d %v", len(body), err)
	}
	if writerCount(t, f.in, "command_mappings") != 521 {
		t.Fatal("mapping lost descendants")
	}
	read := func() []string {
		t.Helper()
		var values []string
		if err := f.in.ReadTx(t.Context(), func(tx *sql.Tx, _ Sequence) error {
			rows, err := tx.QueryContext(t.Context(), `SELECT source||assigned||version FROM command_mappings WHERE op_id=? ORDER BY offset`, u.OpID)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var raw string
				if err := rows.Scan(&raw); err != nil {
					return err
				}
				values = append(values, raw)
			}
			return rows.Err()
		}); err != nil {
			t.Fatal(err)
		}
		return values
	}
	before := read()
	if len(result.Copies) != 1 || len(result.Copies[0].Roots) != 1 {
		t.Fatalf("copy summary=%+v", result.Copies)
	}
	copy := result.Copies[0].Roots[0].Copy
	if _, err := f.in.Update(t.Context(), Update{Slug: "notes", OpID: "edit-copy", Ops: []Operation{SetPageTitleOp{Page: Target{Ref: &copy}, Title: "Changed after mapping"}}}, 0); err != nil {
		t.Fatal(err)
	}
	replayed, err := f.in.Update(t.Context(), u, 0)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Mapping.Cursor != result.Mapping.Cursor || !reflect.DeepEqual(before, read()) {
		t.Fatal("mapping changed with live content/replay")
	}
	f.clock.Add(25 * 3600)
	tick := func() {
		t.Helper()
		if err := f.in.write(t.Context(), func(tx *sql.Tx) error { return f.in.maintenance(t.Context(), tx) }); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	if writerCount(t, f.in, "command_mappings") != 521 {
		t.Fatal("unimported mapping expired")
	}
	entries, err := f.in.Outbox(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.in.Acknowledge(t.Context(), entries[len(entries)-1].Seq, 1000); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(int64(24*time.Hour/time.Second) - 1)
	tick()
	if writerCount(t, f.in, "command_mappings") != 521 {
		t.Fatal("mapping expired before import+24h")
	}
	f.clock.Add(1)
	tick()
	if writerCount(t, f.in, "command_mappings") != 0 || writerCount(t, f.in, "commands") != 0 {
		t.Fatal("mapping and receipt did not expire together")
	}
}

func TestWriterRefusalReserveIncludesSQLGrowth(t *testing.T) {
	f := newWriterFixture(t)
	a := Action{N: 1, ActionID: "reserve", Ops: make([]Operation, 500)}
	for i := range a.Ops {
		a.Ops[i] = DeletePageOp{Page: Target{Ref: &f.page}}
	}
	raw, err := EncodeAction(a)
	if err != nil {
		t.Fatal(err)
	}
	r := &actionRequest{session: contentHash([]byte(f.session.Token)), n: 1, action: a, hash: contentHash(raw)}
	var minimum int64
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		lo, hi := int64(1), int64(4<<20)
		for hi-lo > 1 {
			mid := (lo + hi) / 2
			f.b.quota.Store(mid)
			err := f.in.receiptReserve(t.Context(), tx, 0, r, 2)
			if err == nil {
				hi = mid
			} else {
				writerReason(t, err, "receipt_headroom", true)
				lo = mid
			}
		}
		minimum = hi
		f.b.quota.Store(hi)
		return f.in.receiptReserve(t.Context(), tx, 0, r, 2)
	}); err != nil {
		t.Fatal(err)
	}
	if writerCount(t, f.in, "receipts") != 0 {
		t.Fatal("reservation probe consumed n")
	}
	ack, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, a)
	if err != nil || ack.Summary.Refused != 500 {
		t.Fatalf("minimum admitted reserve %d failed to commit refusal: %+v %v", minimum, ack.Summary, err)
	}
}

func TestWriterRefusalsPreserveSavedRevisionReservation(t *testing.T) {
	f := newWriterFixture(t)
	if _, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, writerAction(f.page, "Saved draft")); err != nil {
		t.Fatal(err)
	}
	var limit int64
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error { limit = writerQuotaEstimate(t, f.in, tx, 0, 0); return nil }); err != nil {
		t.Fatal(err)
	}
	f.b.quota.Store(limit)
	refused := 0
	for n := uint64(2); n < 12; n++ {
		ops := make([]Operation, 500)
		for i := range ops {
			ops[i] = DeletePageOp{Page: Target{Ref: &f.page}}
		}
		ack, err := f.in.SubmitAction(t.Context(), f.session.Token, n, Action{N: Sequence(n), ActionID: "refusal", Ops: ops})
		if err != nil {
			writerReason(t, err, "receipt_headroom", true)
			break
		}
		refused++
		body, err := json.Marshal(ack)
		if err != nil || len(body) > refusalHeadroom/2 || ack.Summary.Refused != 500 {
			t.Fatalf("refusal reserve: bytes=%d summary=%+v err=%v", len(body), ack.Summary, err)
		}
	}
	if refused == 0 {
		t.Fatal("reserved refusal headroom was not usable")
	}
	f.clock.Add(31)
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error { return f.in.maintenance(t.Context(), tx) }); err != nil {
		t.Fatal(err)
	}
	if writerCount(t, f.in, "outbox") != 2 || writerTitle(t, f.in, f.page) != "Saved draft" {
		t.Fatal("saved person boundary was lost")
	}
	tx, err := f.in.readers.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	m, err := measureContent(t.Context(), tx)
	if err != nil {
		t.Fatal(err)
	}
	used, err := quotaUsedPages(t.Context(), tx)
	if err != nil {
		t.Fatal(err)
	}
	wal, err := f.in.quotaWALBytes()
	if err != nil {
		t.Fatal(err)
	}
	actual, err := quotaUsage(m, f.in.db.pageSize, used, 0, 0, 0, wal)
	if err != nil || actual > limit {
		t.Fatalf("refusals spent saved revision space: usage=%d quota=%d err=%v", actual, limit, err)
	}
}

func TestWriterAckCompactionKeepsEveryOutcome(t *testing.T) {
	result := editResult{}
	unit := string(make([]byte, 128))
	for i := range 500 {
		result.Results = append(result.Results, operationResult{Index: i, UnitID: &unit, Status: "applied"})
	}
	ack, err := buildAck(context.Background(), nil, 1, 2, result, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(ack)
	if err != nil || len(body) > maxOutcomeBytes {
		t.Fatalf("ack size=%d %v", len(body), err)
	}
	if ack.Summary.Applied != 500 || ack.ResultsSample.Total != 500 || ack.Summary.Complete || ack.ResultsSample.Complete || len(ack.Outcomes) != 1 || len(ack.Outcomes[0].Indexes) != 500 {
		t.Fatalf("lost complete outcomes: %+v", ack.Summary)
	}
}

func TestWriterAgentInlineTemporaryReferences(t *testing.T) {
	f := newWriterFixture(t)
	u := Update{Slug: "notes", OpID: "create", Ops: []Operation{CreatePageOp{CreateFields: CreateFields{Ref: "new"}, Title: "New"}, SetPageTitleOp{Page: Target{Tmp: "new"}, Title: "Final"}}}
	result, err := f.in.Update(t.Context(), u, 0)
	if err != nil {
		t.Fatal(err)
	}
	assigned, ok := result.Refs["new"]
	if !ok || !result.Mapping.Complete || result.Mapping.Total != 1 || assigned.Ref.Inc == 0 || assigned.Ver != Version(result.State.Seq) {
		t.Fatalf("mapping=%+v %+v", result.Mapping, result.Refs)
	}
	if got := writerTitle(t, f.in, assigned.Ref); got != "Final" {
		t.Fatal(got)
	}
	replay, err := f.in.Update(t.Context(), u, 0)
	if err != nil || !reflect.DeepEqual(result.Refs, replay.Refs) {
		t.Fatalf("replayed refs: %+v %v", replay, err)
	}
}

func TestWriterMappingQuotaFailureLeavesNothing(t *testing.T) {
	f := newWriterFixture(t)
	f.b.quota.Store(1)
	_, err := f.in.Update(t.Context(), Update{Slug: "notes", OpID: "quota-create", Ops: []Operation{CreatePageOp{CreateFields: CreateFields{Ref: "new"}, Title: "No"}}}, 0)
	writerReason(t, err, "disk_quota", false)
	if writerCount(t, f.in, "commands") != 0 || writerCount(t, f.in, "command_mappings") != 0 || writerCount(t, f.in, "pages") != 1 {
		t.Fatal("refused write retained mappings/content")
	}
}
