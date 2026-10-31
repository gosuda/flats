package notebook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

type writerWork struct {
	ctx     context.Context
	fn      func(*sql.Tx) error
	barrier func() error
	action  *actionRequest
	done    chan error
}

type actionRequest struct {
	session string
	n       uint64
	action  Action
	hash    string
	ack     Ack
	err     error
}

func (in *Instance) execute(ctx context.Context, work *writerWork) error {
	work.ctx, work.done = ctx, make(chan error, 1)
	in.admit.RLock()
	if in.ctx.Err() != nil {
		in.admit.RUnlock()
		return unavailable("stopped")
	}
	var err error
	select {
	case in.queue <- work:
	case <-ctx.Done():
		err = ctx.Err()
	case <-in.ctx.Done():
		err = unavailable("stopped")
	default:
		err = unavailable("writer_backpressure")
	}
	in.admit.RUnlock()
	if err != nil {
		return err
	}
	// Once admitted the outcome can be durable even if the caller disconnects.
	// Join it before returning so closures never outlive their captured values.
	return <-work.done
}

func (in *Instance) write(ctx context.Context, fn func(*sql.Tx) error) error {
	return in.execute(ctx, &writerWork{fn: fn})
}

func (in *Instance) runWriter() {
	defer in.workers.Done()
	defer close(in.done)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var next *writerWork
	for {
		if in.ctx.Err() != nil {
			if next != nil {
				next.done <- unavailable("stopped")
			}
			for {
				select {
				case work := <-in.queue:
					work.done <- unavailable("stopped")
				default:
					in.finishGroups()
					return
				}
			}
		}
		work := next
		next = nil
		if work == nil {
			select {
			case work = <-in.queue:
			case <-tick.C:
				if err := in.db.transaction(in.ctx, func(tx *sql.Tx) error { return in.maintenance(in.ctx, tx) }); err != nil && in.ctx.Err() == nil {
					in.log.Warn("notebook maintenance", "err", err)
				}
				in.afterCommit()
				continue
			case <-in.ctx.Done():
				continue
			}
		}
		if err := work.ctx.Err(); err != nil {
			work.done <- err
			continue
		}
		if work.action != nil {
			next = in.groupCommit(work)
			continue
		}
		var err error
		if work.barrier != nil {
			err = work.barrier()
		} else {
			err = in.db.transaction(in.ctx, func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(in.ctx, `DELETE FROM notebook_live_places`); err != nil {
					return err
				}
				return work.fn(tx)
			})
			if err == nil {
				in.afterCommit()
			}
		}
		work.done <- err
	}
}

func (in *Instance) groupCommit(first *writerWork) *writerWork {
	deadline := time.NewTimer(10 * time.Millisecond)
	defer deadline.Stop()
	tx, err := in.db.db.BeginTx(in.ctx, nil)
	if err != nil {
		first.done <- unavailable("storage")
		return nil
	}
	defer tx.Rollback()
	works := []*writerWork{}
	var poisoned error
	stage := func(work *writerWork) {
		works = append(works, work)
		if work.ctx.Err() != nil {
			work.action.err = work.ctx.Err()
			return
		}
		if _, err := tx.ExecContext(in.ctx, `DELETE FROM notebook_live_places`); err != nil {
			poisoned = err
			work.action.err = err
			return
		}
		work.action.ack, work.action.err = in.applyAction(in.ctx, tx, work.action)
		var problem *WriteError
		if errors.As(work.action.err, &problem) && problem.Reason == "rollback" {
			poisoned = work.action.err
		}
	}
	stage(first)
	var next *writerWork
collect:
	for {
		if poisoned != nil {
			break
		}
		select {
		case <-deadline.C:
			break collect
		default:
		}
		select {
		case <-deadline.C:
			break collect
		case <-in.ctx.Done():
			break collect
		case work := <-in.queue:
			if work.action == nil {
				next = work
				break collect
			}
			stage(work)
		}
	}
	if in.hook != nil {
		in.hook("commit", tx)
	}
	if poisoned != nil {
		err = poisoned
	} else {
		err = tx.Commit()
	}
	if err == nil {
		if in.hook != nil {
			in.hook("committed", tx)
		}
		in.afterCommit()
	}
	for _, work := range works {
		if err != nil {
			work.done <- unavailable("commit")
		} else {
			work.done <- work.action.err
		}
	}
	return next
}

func (in *Instance) afterCommit() {
	in.publishLive()
	if err := in.truncateWAL(); err != nil && in.ctx.Err() == nil {
		in.log.Warn("notebook WAL maintenance", "err", err)
	}
	in.b.OutboxReady()
}

func nextSequence(ctx context.Context, tx *sql.Tx) (Sequence, error) {
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT seq FROM meta WHERE singleton=1`).Scan(&raw); err != nil {
		return 0, err
	}
	n, err := parseDBDecimal(raw)
	if err != nil {
		return 0, err
	}
	if n == math.MaxUint64 {
		return 0, unavailable("sequence_exhausted")
	}
	return Sequence(n + 1), nil
}

func setSequence(ctx context.Context, tx *sql.Tx, seq Sequence) error {
	_, err := tx.ExecContext(ctx, `UPDATE meta SET seq=? WHERE singleton=1`, sequenceString(seq))
	return err
}

func saveDelta(ctx context.Context, tx *sql.Tx, seq Sequence, result editResult, source string, now time.Time) error {
	ids := make([]string, 0, len(result.Records))
	for id := range result.Records {
		ids = append(ids, string(id))
	}
	slices.Sort(ids)
	changes := make([]json.RawMessage, 0, len(ids))
	locations := make(map[ID]deltaLocation, len(ids))
	for _, id := range ids {
		record := result.Records[ID(id)]
		location, locationErr := locateDelta(ctx, tx, record.Item)
		if locationErr != nil {
			return locationErr
		}
		if record.Item.Kind() == BlockKind {
			var old ID
			err := tx.QueryRowContext(ctx, `SELECT page FROM notebook_live_places WHERE id=?`, id).Scan(&old)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if old != location.Page {
				location.PreviousPage = old
			}
		}
		locations[ID(id)] = location
		var data []byte
		var err error
		if record.Alive {
			data, err = EncodeItem(record.Item)
		} else {
			data, err = json.Marshal(struct {
				Ref     Ref  `json:"ref"`
				Deleted bool `json:"deleted"`
			}{record.Item.Ref, true})
		}
		if err != nil {
			return err
		}
		changes = append(changes, data)
	}
	var receiptSession string
	var receiptN Sequence
	var tag struct {
		Session string   `json:"session_id"`
		N       Sequence `json:"n"`
	}
	if strings.HasPrefix(source, "{") && json.Unmarshal([]byte(source), &tag) == nil && tag.Session != "" {
		receiptN = tag.N
		if err := tx.QueryRowContext(ctx, `SELECT session FROM receipts WHERE json_extract(outcome,'$.seq')=? AND n=?`, sequenceString(seq), sequenceString(receiptN)).Scan(&receiptSession); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notebook_receipt_units(session,n,kind,scope,item) VALUES(?,?,'receipt','','')`, receiptSession, sequenceString(receiptN)); err != nil {
			return err
		}
		for _, id := range ids {
			record := result.Records[ID(id)]
			loc := locations[ID(id)]
			kind, scope := "page", loc.Page
			if record.Item.Kind() == PageKind {
				kind, scope = "tree", ""
			} else if loc.Database != "" {
				kind, scope = "database", loc.Database
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO notebook_receipt_units(session,n,kind,scope,item) VALUES(?,?,?,?,?)`, receiptSession, sequenceString(receiptN), kind, scope, id); err != nil {
				return err
			}
			if loc.PreviousPage != "" && loc.PreviousPage != loc.Page {
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO notebook_receipt_units(session,n,kind,scope,item) VALUES(?,?,'page',?,?)`, receiptSession, sequenceString(receiptN), loc.PreviousPage, id); err != nil {
					return err
				}
			}
		}
	}
	if receiptSession != "" {
		var bytes int64
		if err := tx.QueryRowContext(ctx, `SELECT length(outcome)+(SELECT coalesce(sum(length(kind)+length(scope)+length(item)+64),0) FROM notebook_receipt_units u WHERE u.session=receipts.session AND u.n=receipts.n) FROM receipts WHERE session=? AND n=?`, receiptSession, sequenceString(receiptN)).Scan(&bytes); err != nil {
			return err
		}
		if bytes > (16<<20)-refusalHeadroom {
			return &WriteError{Code: "notebook_capacity", Reason: "receipt_units"}
		}
		if err := trimRuntimeBytes(ctx, tx, "receipts", receiptRuntimeBytes, (16<<20)-refusalHeadroom); err != nil {
			return err
		}
	}
	type displaced struct {
		Block   Ref             `json:"block"`
		Version Version         `json:"version"`
		Base    Version         `json:"base"`
		Source  json.RawMessage `json:"source"`
	}
	overwrote := make([]displaced, 0, len(result.Overwrote))
	for _, o := range result.Overwrote {
		previous := "null"
		err := tx.QueryRowContext(ctx, `SELECT source FROM notebook_block_sources WHERE block=? AND version=?`, o.Block.ID, sequenceString(Sequence(o.Version))).Scan(&previous)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if o.Version == Version(seq) && receiptSession != "" {
			previous = source
		}
		if previous == "null" {
			continue
		}
		var earlier struct {
			Session string   `json:"session_id"`
			N       Sequence `json:"n"`
		}
		if receiptSession != "" && json.Unmarshal([]byte(previous), &earlier) == nil && earlier.Session == tag.Session && earlier.N < receiptN {
			continue
		}
		overwrote = append(overwrote, displaced{o.Block, o.Version, o.Base, json.RawMessage(previous)})
	}
	for _, id := range ids {
		record := result.Records[ID(id)]
		if record.Item.Kind() != BlockKind || record.Item.V[ContentGroup] != Version(seq) {
			continue
		}
		attribution := "null"
		if receiptSession != "" {
			attribution = source
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notebook_block_sources(block,version,source) VALUES(?,?,?) ON CONFLICT(block) DO UPDATE SET version=excluded.version,source=excluded.source`, id, sequenceString(seq), attribution); err != nil {
			return err
		}
	}
	body, err := json.Marshal(struct {
		Changes   []json.RawMessage    `json:"changes"`
		Overwrote []displaced          `json:"overwrote,omitempty"`
		Locations map[ID]deltaLocation `json:"locations"`
	}{changes, overwrote, locations})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO deltas(seq,effects,source,bytes,at) VALUES(?,?,?,?,?)`, sequenceString(seq), string(body), source, len(body), now.Unix())
	return err
}

const receiptRuntimeBytes = `length(outcome)+(SELECT coalesce(sum(length(kind)+length(scope)+length(item)+64),0) FROM notebook_receipt_units u WHERE u.session=receipts.session AND u.n=receipts.n)`

func undoBudget(quota int64) int64 {
	if quota > 0 {
		return min(int64(MaxUndoBytes), quota/16)
	}
	return int64(MaxUndoBytes)
}

func (in *Instance) closeGroups(ctx context.Context, tx *sql.Tx, all bool) error {
	now := in.now()
	rows, err := tx.QueryContext(ctx, `SELECT editor,started_at,last_at,dirty,label FROM groups ORDER BY editor`)
	if err != nil {
		return err
	}
	var due, authors []string
	dirtyDue := false
	for rows.Next() {
		var editor, label string
		var start, last int64
		var dirty bool
		if err := rows.Scan(&editor, &start, &last, &dirty, &label); err != nil {
			rows.Close()
			return err
		}
		if dirty {
			authors = append(authors, label)
		}
		if all || now.Unix()-last >= 30 || now.Unix()-start >= 300 {
			due = append(due, editor)
			dirtyDue = dirtyDue || dirty
		}
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if dirtyDue {
		if _, err := recordOutbox(ctx, tx, "person", "", "", authors, now, false); err != nil {
			return err
		}
	}
	for _, editor := range due {
		if _, err := tx.ExecContext(ctx, `DELETE FROM groups WHERE editor=?`, editor); err != nil {
			return err
		}
	}
	return nil
}

func (in *Instance) finishGroups() {
	ctx := context.Background()
	if err := in.db.transaction(ctx, func(tx *sql.Tx) error { return in.closeGroups(ctx, tx, true) }); err != nil {
		in.log.Warn("notebook final boundary", "err", err)
	} else {
		in.b.OutboxReady()
	}
}

func (in *Instance) maintenance(ctx context.Context, tx *sql.Tx) error {
	if err := in.closeGroups(ctx, tx, false); err != nil {
		return err
	}
	now := in.now()
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE last_at<=?`, now.Add(-24*time.Hour).Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM commands WHERE retain_until IS NOT NULL AND retain_until<=? AND import_status='imported'`, now.Unix()); err != nil {
		return err
	}
	rows, err := readUndoRows(ctx, tx, "")
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.Entry.Open && now.UnixNano()-row.Entry.At >= int64(time.Second) {
			if err := closeUndoAction(ctx, tx, row.Session, row.Entry.ActionID); err != nil {
				return err
			}
		}
	}
	if err := pruneUndo(ctx, tx, undoBudget(in.b.Limits().DiskQuota)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM deltas WHERE at<=? OR seq IN (SELECT seq FROM deltas ORDER BY seq DESC LIMIT -1 OFFSET 20000)`, now.Add(-time.Hour).Unix()); err != nil {
		return err
	}
	if err := trimRuntimeBytes(ctx, tx, "deltas", "bytes", 16<<20); err != nil {
		return err
	}
	if err := trimRuntimeBytes(ctx, tx, "receipts", receiptRuntimeBytes, (16<<20)-refusalHeadroom); err != nil {
		return err
	}
	return in.collectDeleted(ctx, tx)
}

func trimRuntimeBytes(ctx context.Context, tx *sql.Tx, table, expression string, budget int64) error {
	// Both identifiers are package-owned constants; no caller input enters SQL.
	rows, err := tx.QueryContext(ctx, `SELECT rowid,`+expression+` FROM `+table+` ORDER BY rowid DESC`)
	if err != nil {
		return err
	}
	var total int64
	var remove []int64
	for rows.Next() {
		var id, size int64
		if err := rows.Scan(&id, &size); err != nil {
			rows.Close()
			return err
		}
		if err := addContentBytes(&total, size); err != nil {
			rows.Close()
			return err
		}
		if total > budget {
			remove = append(remove, id)
		}
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	for _, id := range remove {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE rowid=?`, id); err != nil {
			return err
		}
	}
	return nil
}

func rollbackSavepoint(ctx context.Context, tx *sql.Tx, name string, cause error) error {
	_, rollback := tx.ExecContext(ctx, `ROLLBACK TO `+name)
	_, release := tx.ExecContext(ctx, `RELEASE `+name)
	if rollback != nil || release != nil {
		return &WriteError{Code: "unavailable", Reason: "rollback", Retry: true, cause: fmt.Errorf("notebook savepoint rollback: %w", errors.Join(cause, rollback, release))}
	}
	return cause
}

func decimalN(n uint64) string { return strconv.FormatUint(n, 10) }
