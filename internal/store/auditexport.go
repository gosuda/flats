package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// The reads of `flats audit export` (docs/spec/event-webhook.md, rules 22 to 25). The export merges three streams:
// the audit events, the requests of the approvals and the decisions of the approvals. Each stream rises by (at, id)
// and is read a page at a time. A page is one query that is closed before it returns, so the command holds no read
// transaction between two pages, however slowly its output is read.

// ExportRange is the span of time that an export covers. Both ends belong to it, and a zero end is open.
type ExportRange struct {
	Since  time.Time
	Cutoff time.Time
}

// ExportApproval is an approval as the export reads it. It has the fields of the two approval lines (rule 23) and no
// other column of the row. At is requested_at in the stream of the requests. In the stream of the decisions it is
// decided_at, or requested_at for a row that has no decided_at.
type ExportApproval struct {
	At        time.Time
	ID        string
	Flat      string // the slug that the row has now: a rename rewrites it
	FlatID    string
	Action    string
	Status    string
	Via       string
	Reason    string
	DecidedBy string
	Result    string
}

// exportDecidedAt is the time of a decision. Some writes of a final status set no decided_at, and such a row counts
// at the time of its request.
const exportDecidedAt = `COALESCE(NULLIF(decided_at,0),requested_at)`

// exportPage builds the query of one page of a stream: the rows of table that match cond, whose time is in rng, and
// that come after the row with the key after (its time and its id) in the order (at, id). at is the SQL of the time
// of a row. A nil after starts the stream.
func exportPage(cols, table, at, cond string, rng ExportRange, after []any, limit int) (string, []any) {
	var conds []string
	var args []any
	if cond != "" {
		conds = append(conds, cond)
	}
	if !rng.Since.IsZero() {
		conds = append(conds, at+`>=?`)
		args = append(args, unix(rng.Since))
	}
	if !rng.Cutoff.IsZero() {
		conds = append(conds, at+`<=?`)
		args = append(args, unix(rng.Cutoff))
	}
	if after != nil {
		// The pair is compared as a whole, so the rows that share the time of the last row and have a larger id are
		// not skipped.
		conds = append(conds, `(`+at+`,id)>(?,?)`)
		args = append(args, after...)
	}
	q := `SELECT ` + cols + ` FROM ` + table
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, ` AND `)
	}
	return q + ` ORDER BY ` + at + `,id LIMIT ?`, append(args, limit)
}

// ExportAudit returns up to limit audit events of rng that come after the event after by (at, id), oldest first. A
// nil after starts with the oldest.
func (r *AuditReader) ExportAudit(ctx context.Context, rng ExportRange, after *AuditEvent, limit int) ([]AuditEvent, error) {
	var key []any
	if after != nil {
		key = []any{unix(after.Time), after.ID}
	}
	q, args := exportPage(`id,at,kind,flat,flat_id,name,via,actor,data`, `audit_events`, `at`, ``, rng, key, limit)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var e AuditEvent
		var at int64
		var data sql.NullString
		if err := rows.Scan(&e.ID, &at, &e.Kind, &e.Flat, &e.FlatID, &e.Name, &e.Via, &e.Actor, &data); err != nil {
			return nil, err
		}
		e.Time = fromUnix(at)
		if data.Valid {
			e.Data = json.RawMessage(data.String)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ExportCreated returns up to limit approvals of rng by the time of their request, oldest first, that come after the
// approval after by (requested_at, id). Every approval has a request, whatever its status.
func (r *AuditReader) ExportCreated(ctx context.Context, rng ExportRange, after *ExportApproval, limit int) ([]ExportApproval, error) {
	return r.exportApprovals(ctx, `requested_at`, ``, rng, after, limit)
}

// ExportDecided returns up to limit approvals with a final status (approved, rejected or failed) of rng by the time
// of their decision, oldest first, that come after the approval after by that time and the id.
func (r *AuditReader) ExportDecided(ctx context.Context, rng ExportRange, after *ExportApproval, limit int) ([]ExportApproval, error) {
	return r.exportApprovals(ctx, exportDecidedAt, `status IN ('approved','rejected','failed')`, rng, after, limit)
}

func (r *AuditReader) exportApprovals(ctx context.Context, at, cond string, rng ExportRange, after *ExportApproval, limit int) ([]ExportApproval, error) {
	var key []any
	if after != nil {
		key = []any{unix(after.At), after.ID}
	}
	// reason, result and decided_by can be NULL. params, result_data, authorized_at and idempotency_key are not read,
	// so no line can hold them.
	q, args := exportPage(at+`,id,flat,flat_id,action,status,via,COALESCE(reason,''),COALESCE(decided_by,''),COALESCE(result,'')`,
		`approvals`, at, cond, rng, key, limit)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExportApproval
	for rows.Next() {
		var a ExportApproval
		var ms int64
		if err := rows.Scan(&ms, &a.ID, &a.Flat, &a.FlatID, &a.Action, &a.Status, &a.Via, &a.Reason, &a.DecidedBy, &a.Result); err != nil {
			return nil, err
		}
		a.At = fromUnix(ms)
		out = append(out, a)
	}
	return out, rows.Err()
}
