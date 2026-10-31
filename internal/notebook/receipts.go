package notebook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	maxOutcomeBytes = 64 << 10
	refusalHeadroom = 16 << 10
)

// SampleInfo makes omission explicit without changing complete outcome counts.
type SampleInfo struct {
	Total    int  `json:"total"`
	Complete bool `json:"complete"`
}
type OutcomeGroup struct {
	Indexes     []int  `json:"indexes"`
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
	FailedIndex *int   `json:"failed_index,omitempty"`
}
type RootMapping struct {
	Source Ref     `json:"source"`
	Copy   Ref     `json:"copy"`
	Ver    Version `json:"ver"`
}
type CopySummary struct {
	Index         int           `json:"index"`
	RootsTotal    int           `json:"roots_total"`
	OtherCounts   [7]int        `json:"other_counts"`
	Roots         []RootMapping `json:"roots"`
	RootsComplete bool          `json:"roots_complete"`
}
type AckSummary struct {
	Applied     int           `json:"applied"`
	Ignored     int           `json:"ignored"`
	Refused     int           `json:"refused"`
	CopiedRoots []CopySummary `json:"copied_roots"`
	Complete    bool          `json:"complete"`
}

// Ack is persisted before it is exposed as durable confirmation.
type Ack struct {
	N             Sequence          `json:"n"`
	Seq           Sequence          `json:"seq"`
	Outcomes      []OutcomeGroup    `json:"outcomes"`
	Summary       AckSummary        `json:"summary"`
	Results       []operationResult `json:"results"`
	ResultsSample SampleInfo        `json:"results_sample"`
	Undo          *UndoSummary      `json:"undo,omitempty"`
}

type AssignedRef struct {
	Ref Ref     `json:"ref"`
	Ver Version `json:"ver"`
}
type MappingEntry struct {
	Index  int     `json:"index"`
	Tmp    string  `json:"tmp,omitempty"`
	Source *Ref    `json:"source,omitempty"`
	Ref    Ref     `json:"ref"`
	Ver    Version `json:"ver"`
}
type ResultMapping struct {
	Complete bool          `json:"complete"`
	Total    int           `json:"total"`
	Copies   []copyMapping `json:"copies,omitempty"`
	Cursor   string        `json:"cursor,omitempty"`
}

func (m ResultMapping) MarshalJSON() ([]byte, error) {
	if m.Complete {
		copies := m.Copies
		if copies == nil {
			copies = []copyMapping{}
		}
		return json.Marshal(struct {
			Complete bool          `json:"complete"`
			Total    int           `json:"total"`
			Copies   []copyMapping `json:"copies"`
		}{true, m.Total, copies})
	}
	return json.Marshal(struct {
		Complete bool   `json:"complete"`
		Total    int    `json:"total"`
		Cursor   string `json:"cursor"`
	}{false, m.Total, m.Cursor})
}

// WriteWarning is the bounded write-side warning projection.
type WriteWarning struct {
	Code    string `json:"code"`
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitempty"`
	Message string `json:"message"`
}
type WriteSamples struct {
	Warnings SampleInfo `json:"warnings"`
}

// WriteResult reports a committed edit independently of host revision import.
type WriteResult struct {
	State           State                  `json:"state"`
	Revision        *int                   `json:"revision"`
	RevisionPending bool                   `json:"revision_pending"`
	Replay          bool                   `json:"replay"`
	Created         bool                   `json:"created"`
	Mapping         ResultMapping          `json:"mapping"`
	Refs            map[string]AssignedRef `json:"refs,omitempty"`
	Applied         int                    `json:"applied"`
	Warnings        []WriteWarning         `json:"warnings"`
	Copies          []CopySummary          `json:"copies,omitempty"`
	Samples         WriteSamples           `json:"samples"`
}

type commandOutcome struct {
	Result *WriteResult `json:"result,omitempty"`
	Error  *WriteError  `json:"error,omitempty"`
}

// SubmitAction accepts the next action or replays its original bounded ack.
func (in *Instance) SubmitAction(ctx context.Context, session string, n uint64, a Action) (ack Ack, err error) {
	defer func() {
		if err == nil && ack.Summary.Applied == 0 && ack.Summary.Refused > 0 {
			notebookRejected.Add(1)
		} else {
			recordBatch(err)
		}
	}()
	if a.N != 0 && uint64(a.N) != n {
		return Ack{}, &WriteError{Code: "invalid", Reason: "action_number"}
	}
	a.N = Sequence(n)
	raw, err := EncodeAction(a)
	if err != nil {
		return Ack{}, &WriteError{Code: "invalid", Reason: "action", Detail: err.Error()}
	}
	a, err = DecodeAction(raw)
	if err != nil {
		return Ack{}, err
	}
	r := &actionRequest{session: contentHash([]byte(session)), n: n, action: a, hash: contentHash(raw)}
	err = in.execute(ctx, &writerWork{action: r})
	return r.ack, err
}

func (in *Instance) applyAction(ctx context.Context, tx *sql.Tx, r *actionRequest) (Ack, error) {
	s, err := readSession(ctx, tx, r.session, in.now())
	if err != nil {
		return Ack{}, err
	}
	if r.n <= s.max {
		var hash, raw string
		err := tx.QueryRowContext(ctx, `SELECT request_hash,outcome FROM receipts WHERE session=? AND n=?`, r.session, decimalN(r.n)).Scan(&hash, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return Ack{}, &WriteError{Code: "conflict", Reason: "receipt_expired"}
		}
		if err != nil {
			return Ack{}, err
		}
		if hash != r.hash {
			return Ack{}, &WriteError{Code: "conflict", Reason: "action_conflict"}
		}
		var ack Ack
		if err := json.Unmarshal([]byte(raw), &ack); err != nil {
			return ack, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET last_at=? WHERE token_hash=?`, in.now().Unix(), s.hash)
		return ack, err
	}
	if r.n != s.max+1 || r.n == 0 {
		return Ack{}, &WriteError{Code: "invalid", Reason: "action_gap"}
	}
	other, valid := in.measure.get()
	if !valid {
		in.measure.refresh()
		return Ack{}, unavailable("measurement")
	}
	seq, err := nextSequence(ctx, tx)
	if err != nil {
		return Ack{}, err
	}
	if err := in.receiptReserve(ctx, tx, other, r, seq); err != nil {
		return Ack{}, err
	}
	if err := in.outboxAdmission(ctx, tx); err != nil {
		return Ack{}, err
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT writer_action`); err != nil {
		return Ack{}, err
	}
	var undo *UndoSummary
	stage := func() (editResult, error) {
		if err := in.checkQuota(ctx, tx, other, 0); err != nil {
			return editResult{}, err
		}
		if err := in.closeGroups(ctx, tx, hasPropertyConversion(r.action.Ops)); err != nil {
			return editResult{}, err
		}
		opts := editOptions{Browser: true, Session: s.hash, Seq: seq, ActionID: r.action.ActionID,
			TypingBlock: r.action.TypingBlock, Close: r.action.Close != nil && *r.action.Close, Now: in.now(), UndoBudget: undoBudget(in.b.Limits().DiskQuota)}
		var result editResult
		var err error
		if len(r.action.Ops) == 1 && isUndoControl(r.action.Ops[0]) {
			result, undo, err = applyBrowserUndo(ctx, tx, opts, r.action.Ops[0].OperationName() == "redo")
		} else {
			result, err = applyEdit(ctx, tx, opts, r.action.Ops)
		}
		if err != nil {
			return result, err
		}
		if err := syncContentFiles(ctx, tx, seq); err != nil {
			return result, err
		}
		if len(result.Records) > 0 {
			if err := touchGroup(ctx, tx, s, in.now()); err != nil {
				return result, err
			}
		}
		return result, nil
	}
	result, err := stage()
	if err == nil {
		var ack Ack
		ack, err = buildAck(ctx, tx, r.n, seq, result, r.action.Ops)
		ack.Undo = undo
		if err == nil {
			err = persistAction(ctx, tx, r, seq, ack, result, in.now())
		}
		if err == nil {
			err = in.checkQuota(ctx, tx, other, 0)
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `RELEASE writer_action`)
			return ack, err
		}
	}
	err = rollbackSavepoint(ctx, tx, "writer_action", err)
	var permanent *WriteError
	converted := permanentWriteError(err)
	if !errors.As(converted, &permanent) || permanent.Retry {
		return Ack{}, converted
	}
	refused := editResult{Results: make([]operationResult, len(r.action.Ops))}
	for i := range refused.Results {
		refused.Results[i] = operationResult{Index: i, Status: "refused", Reason: permanent.Reason}
	}
	ack, err := buildAckLimit(ctx, tx, r.n, seq, refused, nil, refusalHeadroom/2)
	if err != nil {
		return Ack{}, err
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT refusal_receipt`); err != nil {
		return Ack{}, err
	}
	if err = persistAction(ctx, tx, r, seq, ack, editResult{}, in.now()); err == nil {
		err = in.receiptFootprint(ctx, tx, other, 0)
	}
	if err != nil {
		return Ack{}, rollbackSavepoint(ctx, tx, "refusal_receipt", unavailable("receipt_headroom"))
	}
	_, err = tx.ExecContext(ctx, `RELEASE refusal_receipt`)
	return ack, err
}

func persistAction(ctx context.Context, tx *sql.Tx, r *actionRequest, seq Sequence, ack Ack, result editResult, now time.Time) error {
	raw, err := json.Marshal(ack)
	if err != nil {
		return err
	}
	if len(raw) > maxOutcomeBytes {
		return fmt.Errorf("notebook ack exceeds bound")
	}
	if err := setSequence(ctx, tx, seq); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO receipts(session,n,request_hash,outcome) VALUES(?,?,?,?)`, r.session, decimalN(r.n), r.hash, string(raw)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET max_n=?,last_at=? WHERE token_hash=?`, decimalN(r.n), now.Unix(), r.session); err != nil {
		return err
	}
	source, err := json.Marshal(struct {
		SessionID string   `json:"session_id"`
		N         Sequence `json:"n"`
	}{sessionSource(r.session), Sequence(r.n)})
	if err != nil {
		return err
	}
	return saveDelta(ctx, tx, seq, result, string(source), now)
}

func (in *Instance) receiptReserve(ctx context.Context, tx *sql.Tx, other int64, r *actionRequest, seq Sequence) error {
	var used int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(length(outcome)),0) FROM receipts`).Scan(&used); err != nil {
		return err
	}
	if used > (16<<20)-maxOutcomeBytes {
		return unavailable("receipt_backpressure")
	}
	if in.b.Limits().DiskQuota <= 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT receipt_reserve`); err != nil {
		return err
	}
	// Encoded bytes alone miss overflow pages, primary-key splits, session
	// updates and their WAL frames. Probe the real refusal transaction.
	refused := editResult{Results: make([]operationResult, len(r.action.Ops))}
	for i := range refused.Results {
		refused.Results[i] = operationResult{Index: i, Status: "refused", Reason: "disk_quota"}
	}
	ack, err := buildAckLimit(ctx, tx, r.n, seq, refused, nil, refusalHeadroom/2)
	if err == nil {
		err = persistAction(ctx, tx, r, seq, ack, editResult{}, in.now())
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE receipts SET outcome=outcome||? WHERE session=? AND n=?`, strings.Repeat(" ", refusalHeadroom/2), r.session, decimalN(r.n))
	}
	if err == nil {
		err = in.receiptFootprint(ctx, tx, other, 0)
	}
	rolled := rollbackSavepoint(ctx, tx, "receipt_reserve", err)
	if rolled == nil {
		return nil
	}
	var problem *WriteError
	if errors.As(rolled, &problem) && problem.Reason == "rollback" {
		return rolled
	}
	return unavailable("receipt_headroom")
}

func (in *Instance) receiptFootprint(ctx context.Context, tx *sql.Tx, other, pending int64) error {
	if in.b.Limits().DiskQuota <= 0 {
		return nil
	}
	m, err := measureContent(ctx, tx)
	if err != nil {
		return err
	}
	used, err := quotaUsedPages(ctx, tx)
	if err != nil {
		return err
	}
	future, err := quotaFuturePages(m, in.db.pageSize)
	if err != nil {
		return err
	}
	// Only the eight fixed headroom pages may pay for refusals; dirty
	// content and its eventual revision must keep their full reservation.
	future -= 8
	if len(m.DirtySizes) > 0 {
		growth, err := in.quotaRecordedGrowth(ctx, tx, used)
		if err != nil {
			return err
		}
		future = max(future, growth)
	}
	wal, err := in.quotaPendingWAL(ctx, tx, future)
	if err != nil {
		return err
	}
	other, err = in.measure.agentOther(other)
	if err != nil {
		return err
	}
	total, err := quotaUsage(m, in.db.pageSize, used, future, other, pending, wal)
	if err != nil {
		return err
	}
	if total > in.b.Limits().DiskQuota {
		return unavailable("receipt_headroom")
	}
	return nil
}

func (in *Instance) outboxAdmission(ctx context.Context, tx *sql.Tx) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM outbox`).Scan(&count); err != nil {
		return err
	}
	if count >= 1024 {
		return unavailable("outbox_backpressure")
	}
	return nil
}

func hasPropertyConversion(ops []Operation) bool {
	for _, op := range ops {
		if op.OperationName() == "set_property" {
			return true
		}
	}
	return false
}

// Update is atomic with its replay row, result mapping, and forced outbox entry.
func (in *Instance) Update(ctx context.Context, u Update, other int64) (result WriteResult, err error) {
	defer func() { recordBatch(err) }()
	if other < 0 {
		return result, &WriteError{Code: "invalid", Reason: "measurement"}
	}
	raw, err := EncodeUpdate(u)
	if err != nil {
		return result, &WriteError{Code: "invalid", Reason: "update", Detail: err.Error()}
	}
	u, err = DecodeUpdate(raw)
	if err != nil {
		return result, err
	}
	if u.Slug != in.slug {
		return result, &WriteError{Code: "invalid", Reason: "slug"}
	}
	hash := contentHash(raw)
	if replay, found, err := in.replayUpdate(ctx, u.OpID, hash); err != nil || found {
		return replay, err
	}
	u, err = in.prepareUpdateAssets(ctx, u, other)
	if err != nil {
		return result, err
	}
	var outcomeErr error
	err = in.write(ctx, func(tx *sql.Tx) error {
		var storedHash, storedOutcome string
		err := tx.QueryRowContext(in.ctx, `SELECT request_hash,outcome FROM commands WHERE op_id=?`, u.OpID).Scan(&storedHash, &storedOutcome)
		if err == nil {
			if hash != storedHash {
				return &WriteError{Code: "conflict", Reason: "op_id_conflict"}
			}
			var out commandOutcome
			if err := json.Unmarshal([]byte(storedOutcome), &out); err != nil {
				return err
			}
			if out.Result == nil && out.Error == nil {
				return unavailable("receipt_corrupt")
			}
			if out.Result != nil {
				result = *out.Result
				result.Replay = true
			}
			if out.Error != nil {
				outcomeErr = out.Error
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := in.outboxAdmission(in.ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(in.ctx, `SAVEPOINT agent_call`); err != nil {
			return err
		}
		result, err = in.applyUpdate(in.ctx, tx, u, hash, other)
		if err == nil {
			_, err = tx.ExecContext(in.ctx, `RELEASE agent_call`)
			return err
		}
		err = rollbackSavepoint(in.ctx, tx, "agent_call", err)
		outcomeErr = permanentWriteError(err)
		var problem *WriteError
		if errors.As(outcomeErr, &problem) && problem.Retry {
			return outcomeErr
		}
		// Rejected agent calls do not adopt an op id: the entire call, including
		// its revision boundaries and mappings, is rolled back.
		return nil
	})
	if err != nil {
		return WriteResult{}, err
	}
	return result, outcomeErr
}

func (in *Instance) applyUpdate(ctx context.Context, tx *sql.Tx, u Update, hash string, other int64) (WriteResult, error) {
	result := WriteResult{}
	guard := &edit{ctx: ctx, tx: tx, browser: false}
	if err := guard.checkGuards(u.Guards); err != nil {
		return result, err
	}
	if err := in.checkQuota(ctx, tx, other, 0); err != nil {
		return result, err
	}
	if err := in.closeGroups(ctx, tx, true); err != nil {
		return result, err
	}
	seq, err := nextSequence(ctx, tx)
	if err != nil {
		return result, err
	}
	edited, err := applyEdit(ctx, tx, editOptions{Seq: seq, Now: in.now(), UndoBudget: undoBudget(in.b.Limits().DiskQuota)}, u.Ops)
	if err != nil {
		return result, err
	}
	for _, r := range edited.Results {
		if r.Status != "applied" {
			return result, &WriteError{Code: "invalid", Reason: r.Reason}
		}
	}
	if err := setSequence(ctx, tx, seq); err != nil {
		return result, err
	}
	if err := syncContentFiles(ctx, tx, seq); err != nil {
		return result, err
	}
	message := ""
	if u.Message != nil {
		message = *u.Message
	}
	outbox, err := recordOutbox(ctx, tx, "agent", message, u.OpID, nil, in.now(), true)
	if err != nil {
		return result, err
	}
	result, entries, err := buildWriteResult(ctx, tx, State{in.storeID, seq}, edited, u.Ops)
	if err != nil {
		return result, err
	}
	out, err := json.Marshal(commandOutcome{Result: &result})
	if err != nil {
		return result, err
	}
	state, err := json.Marshal(result.State)
	if err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO commands(op_id,request_hash,outcome,mapping_cursor,state,outbox_seq,import_status) VALUES(?,?,?,?,?,?,'pending')`, u.OpID, hash, string(out), result.Mapping.Cursor, string(state), strconv.FormatInt(outbox, 10)); err != nil {
		return result, err
	}
	for offset, entry := range entries {
		assigned, err := json.Marshal(entry.Ref)
		if err != nil {
			return result, err
		}
		var tmp, source any
		if entry.Tmp != "" {
			tmp = entry.Tmp
		} else {
			encoded, err := json.Marshal(entry.Source)
			if err != nil {
				return result, err
			}
			source = string(encoded)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO command_mappings(op_id,offset,operation_index,tmp,source,assigned,version) VALUES(?,?,?,?,?,?,?)`, u.OpID, offset, entry.Index, tmp, source, string(assigned), decimalN(uint64(entry.Ver))); err != nil {
			return result, err
		}
	}
	if err := saveDelta(ctx, tx, seq, edited, "", in.now()); err != nil {
		return result, err
	}
	if err := in.checkQuota(ctx, tx, other, 0); err != nil {
		return result, err
	}
	return result, nil
}

func buildAck(ctx context.Context, tx *sql.Tx, n uint64, seq Sequence, result editResult, ops []Operation) (Ack, error) {
	return buildAckLimit(ctx, tx, n, seq, result, ops, maxOutcomeBytes)
}

func buildAckLimit(ctx context.Context, tx *sql.Tx, n uint64, seq Sequence, result editResult, ops []Operation, limit int) (Ack, error) {
	ack := Ack{N: Sequence(n), Seq: seq, Outcomes: []OutcomeGroup{}, Summary: AckSummary{CopiedRoots: []CopySummary{}, Complete: true}, Results: slices.Clone(result.Results), ResultsSample: SampleInfo{len(result.Results), true}}
	if ack.Results == nil {
		ack.Results = []operationResult{}
	}
	for _, r := range result.Results {
		switch r.Status {
		case "applied", "adjusted":
			ack.Summary.Applied++
		case "ignored":
			ack.Summary.Ignored++
		case "refused":
			ack.Summary.Refused++
		}
		found := false
		for i := range ack.Outcomes {
			g := &ack.Outcomes[i]
			if g.Status == r.Status && g.Reason == r.Reason && sameFailedIndex(g.FailedIndex, r.FailedIndex) {
				g.Indexes = append(g.Indexes, r.Index)
				found = true
				break
			}
		}
		if !found {
			ack.Outcomes = append(ack.Outcomes, OutcomeGroup{[]int{r.Index}, r.Status, r.Reason, r.FailedIndex})
		}
	}
	var err error
	ack.Summary.CopiedRoots, err = copySummaries(ctx, tx, result, ops)
	if err != nil {
		return ack, err
	}
	for {
		raw, err := json.Marshal(ack)
		if err != nil {
			return ack, err
		}
		if len(raw) <= limit {
			return ack, nil
		}
		ack.Summary.Complete = false
		if len(ack.Results) > 0 {
			ack.Results = ack.Results[:len(ack.Results)/2]
			ack.ResultsSample.Complete = false
			continue
		}
		if !shrinkCopyRoots(ack.Summary.CopiedRoots) {
			return ack, fmt.Errorf("notebook outcome summary exceeds bound")
		}
	}
}

func sameFailedIndex(a, b *int) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

func shrinkCopyRoots(copies []CopySummary) bool {
	changed := false
	for i := range copies {
		if len(copies[i].Roots) > 0 {
			copies[i].Roots = copies[i].Roots[:len(copies[i].Roots)/2]
			copies[i].RootsComplete = false
			changed = true
		}
	}
	return changed
}

func copySummaries(ctx context.Context, tx *sql.Tx, result editResult, ops []Operation) ([]CopySummary, error) {
	summaries := []CopySummary{}
	for i, op := range ops {
		if op.OperationName() != "duplicate_page" && op.OperationName() != "duplicate_blocks" {
			continue
		}
		roots := map[ID]bool{}
		s := CopySummary{Index: i, Roots: []RootMapping{}, RootsComplete: true}
		for _, root := range result.CopyRoots {
			if root.Index == i {
				roots[root.Ref.ID] = true
				s.Roots = append(s.Roots, RootMapping{root.Source, root.Ref, root.Ver})
				s.RootsTotal++
			}
		}
		if len(roots) == 0 {
			continue
		}
		for _, c := range result.Copies {
			if c.Index != i {
				continue
			}
			if !roots[c.Ref.ID] {
				kinds := []Kind{PageKind, BlockKind, DatabaseKind, PropertyKind, OptionKind, RowKind, ViewKind}
				for k, kind := range kinds {
					if c.Source.ID.Kind() == kind {
						s.OtherCounts[k]++
					}
				}
			}
		}
		summaries = append(summaries, s)
	}
	return summaries, nil
}

func buildWriteResult(ctx context.Context, tx *sql.Tx, state State, edit editResult, ops []Operation) (WriteResult, []MappingEntry, error) {
	result := WriteResult{State: state, RevisionPending: true, Applied: len(edit.Results), Mapping: ResultMapping{Complete: true}, Refs: map[string]AssignedRef{}, Warnings: []WriteWarning{{Code: "revision_pending", Message: "edit committed; revision import is pending"}}, Samples: WriteSamples{SampleInfo{1, true}}}
	for _, warning := range edit.Warnings {
		result.Warnings = append(result.Warnings, WriteWarning{Code: warning.Code, Path: warning.Path, Line: warning.Line, Message: warning.Message})
	}
	result.Samples.Warnings.Total = len(result.Warnings)
	entries := []MappingEntry{}
	seen := map[string]bool{}
	add := func(index int, name string) error {
		ref, ok := edit.Refs[name]
		if !ok || seen[name] {
			return nil
		}
		record, err := loadRecord(ctx, tx, ref.ID)
		if err != nil {
			return err
		}
		result.Refs[name] = AssignedRef{ref, record.Item.Ver}
		entries = append(entries, MappingEntry{Index: index, Tmp: name, Ref: ref, Ver: record.Item.Ver})
		seen[name] = true
		return nil
	}
	for i, op := range ops {
		creates, err := operationCreates(op)
		if err != nil {
			return result, nil, err
		}
		for _, create := range creates {
			if err := add(i, create.Name); err != nil {
				return result, nil, err
			}
		}
		for _, c := range edit.Copies {
			if c.Index == i {
				source := c.Source
				entries = append(entries, MappingEntry{Index: i, Source: &source, Ref: c.Ref, Ver: c.Ver})
			}
		}
	}
	for _, create := range edit.MarkdownCreates {
		if err := add(create.Index, create.Name); err != nil {
			return result, nil, err
		}
	}
	keys := make([]string, 0, len(edit.Refs))
	for name := range edit.Refs {
		if !seen[name] {
			keys = append(keys, name)
		}
	}
	slices.Sort(keys)
	for _, name := range keys {
		if err := add(0, name); err != nil {
			return result, nil, err
		}
	}
	result.Mapping.Total = len(entries)
	result.Mapping.Copies = slices.Clone(edit.Copies)
	if result.Mapping.Copies == nil {
		result.Mapping.Copies = []copyMapping{}
	}
	if len(result.Refs) == 0 {
		result.Refs = nil
	}
	var err error
	result.Copies, err = copySummaries(ctx, tx, edit, ops)
	if err != nil {
		return result, nil, err
	}
	if len(result.Copies) == 0 {
		result.Copies = nil
	}
	for {
		raw, err := json.Marshal(commandOutcome{Result: &result})
		if err != nil {
			return result, nil, err
		}
		if len(raw) <= maxOutcomeBytes-128 {
			if result.Mapping.Complete {
				return result, nil, nil
			}
			return result, entries, nil
		}
		if result.Mapping.Complete {
			cursor, err := randomNotebookToken()
			if err != nil {
				return result, nil, err
			}
			result.Mapping = ResultMapping{Complete: false, Total: len(entries), Cursor: cursor}
			result.Refs = nil
			continue
		}
		if shrinkCopyRoots(result.Copies) {
			continue
		}
		if len(result.Warnings) > 1 {
			result.Warnings = result.Warnings[:max(1, len(result.Warnings)/2)]
			result.Samples.Warnings.Complete = false
			continue
		}
		return result, nil, fmt.Errorf("notebook write summary exceeds bound")
	}
}
