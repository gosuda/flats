package sheet

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	sessionTTL       = 24 * time.Hour
	undoDisconnected = 10 * time.Minute
	receiptCache     = 64
	receiptsKept     = 1024
	// A session without a row keeps its receipts in memory, at most
	// receiptCache of them and keptSessionBytes. The sheet's sessions keep at
	// most keptSheetBytes together: a disconnected session stays for a day,
	// and every new Private connection over the quota opens one.
	keptSessionBytes = 1 << 20
	keptSheetBytes   = 8 << 20
	// keptOverhead is what a kept receipt holds beside its hash and outcome:
	// the record, its pointers and its place in the session's list.
	keptOverhead = 128
	// A sheet keeps at most maxSessions sessions in memory and
	// sessionsBytes of their identities. The oldest disconnected one leaves
	// first; one that is connected or in use by a request never leaves.
	// Connections are at most maxPrivateConns at a time, so the bound holds
	// every connected session with room to spare.
	maxSessions   = 256
	sessionsBytes = 512 << 10
	// A session's editor is at most maxEditorBytes and its label at most
	// maxLabelChars code points (newSession).
	maxEditorBytes = 1024
	maxLabelChars  = 200
	maxLabelBytes  = 4 * maxLabelChars
	// sessionOverhead is what a session holds beside its strings: the
	// record, its undo state without entries and its place in the map.
	sessionOverhead = 512
	// goneKept is how many sessions without a row that left memory the
	// sheet remembers, so that their actions sent again get
	// receipt_expired. Each is the 32 bytes of its token hash.
	goneKept = 1024
)

// session is one browser tab's editing session: its durable identity and
// action numbers, and its undo history in memory.
type session struct {
	id       string
	ord      uint32 // names the session in cell records and marks, in memory
	hash     []byte
	editor   string
	label    string
	verified bool
	maxN     uint64
	// durable is the highest action number whose receipt is committed.
	// maxN runs ahead of it while a commit group is open.
	durable atomic.Uint64
	// rowless marks a session that has no row in sheet.db: one opened in
	// a flat that was over its quota. It lives in memory, its receipts
	// too, and it gets its row with the first action the quota admits.
	rowless bool
	undo    undoState
	conn    *conn
	left    time.Time // when its last connection or request went away
	// holds counts the callers that found the session and have not let it
	// go: a connection until it attaches, a History request until it ends.
	// A held session does not leave memory.
	holds int
	// recent holds the receipts of a rowless session, oldest first. Memory
	// is their only store until the commit that stores the row moves them
	// to sheet.db.
	recent      []*keptReceipt
	recentBytes int64
	sheet       *keptReceipts // the receipts all sessions of the sheet keep
	// untrimmed counts the receipts this session may store in sheet.db
	// before the next trim. Zero, as after a load or a failed commit, trims
	// with the next one.
	untrimmed int
}

type storedReceipt struct {
	n       uint64
	seq     uint64
	hash    []byte
	outcome json.RawMessage
}

// keptReceipt is a receipt of a session without a row, whose only store is
// memory.
type keptReceipt struct {
	storedReceipt
	s          *session
	prev, next *keptReceipt // the sheet's kept receipts, oldest first
}

// size charges the receipt by the capacity it keeps alive.
func (k *keptReceipt) size() int64 {
	return int64(cap(k.hash)+cap(k.outcome)) + keptOverhead
}

// keptReceipts lists the receipts the sheet's sessions keep in memory,
// oldest first, with their bytes. A session's own receipts come in the
// order of their numbers, so the oldest of the sheet is also the oldest of
// its session. The writer changes the list under the write lock.
type keptReceipts struct {
	head, tail *keptReceipt
	bytes      int64
}

// relink puts a receipt back between the neighbours it had when it was
// unlinked. Unlinks undone in the reverse order restore the list exactly.
func (l *keptReceipts) relink(k *keptReceipt) {
	if k.prev != nil {
		k.prev.next = k
	} else {
		l.head = k
	}
	if k.next != nil {
		k.next.prev = k
	} else {
		l.tail = k
	}
	l.bytes += k.size()
}

func (l *keptReceipts) unlink(k *keptReceipt) {
	if k.prev != nil {
		k.prev.next = k.next
	} else {
		l.head = k.next
	}
	if k.next != nil {
		k.next.prev = k.prev
	} else {
		l.tail = k.prev
	}
	l.bytes -= k.size()
}

// keep adds the receipt of a rowless session's action. It drops the
// session's oldest receipts until the session is within receiptCache and
// keptSessionBytes, then the sheet's oldest until the sheet is within
// keptSheetBytes, and records what it dropped for forget.
func (in *Instance) keep(a *actionWork, r storedReceipt) {
	s, l := a.s, &in.kept
	k := &keptReceipt{storedReceipt: r, s: s, prev: l.tail}
	l.relink(k)
	s.recent = append(s.recent, k)
	s.recentBytes += k.size()
	a.kept = k
	for len(s.recent) > 1 && (len(s.recent) > receiptCache || s.recentBytes > keptSessionBytes) {
		a.dropped = append(a.dropped, l.drop(s.recent[0]))
	}
	for l.bytes > keptSheetBytes && l.head != k {
		a.dropped = append(a.dropped, l.drop(l.head))
	}
}

// drop takes the oldest receipt of its session out of memory.
func (l *keptReceipts) drop(k *keptReceipt) *keptReceipt {
	l.unlink(k)
	s := k.s
	copy(s.recent, s.recent[1:])
	s.recent[len(s.recent)-1] = nil
	s.recent = s.recent[:len(s.recent)-1]
	s.recentBytes -= k.size()
	return k
}

// unkeep takes back what keep did for an action whose transaction does not
// commit, in the reverse order.
func (l *keptReceipts) unkeep(a *actionWork) {
	for i := len(a.dropped) - 1; i >= 0; i-- {
		k := a.dropped[i]
		l.relink(k)
		k.s.recent = slices.Insert(k.s.recent, 0, k)
		k.s.recentBytes += k.size()
	}
	k, s := a.kept, a.s
	l.unlink(k)
	s.recent[len(s.recent)-1] = nil
	s.recent = s.recent[:len(s.recent)-1]
	s.recentBytes -= k.size()
	a.kept, a.dropped = nil, nil
}

// release lets go of every receipt a session keeps in memory.
func (l *keptReceipts) release(s *session) {
	for _, k := range s.recent {
		l.unlink(k)
	}
	s.recent, s.recentBytes = nil, 0
}

func findKept(list []*keptReceipt, n uint64) (*keptReceipt, bool) {
	i, ok := slices.BinarySearchFunc(list, n, func(k *keptReceipt, n uint64) int { return cmp.Compare(k.n, n) })
	if !ok {
		return nil, false
	}
	return list[i], true
}

// author is the name a revision credits the session's edits to.
func (s *session) author() string {
	if s.label == "" {
		return "Editor"
	}
	return s.label
}

func randomToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func tokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func sessionID(hash []byte) string {
	sum := sha256.Sum256(append([]byte("sheet-session:"), hash...))
	return hex.EncodeToString(sum[:16])
}

// weight is what the session's identity keeps in memory.
func (s *session) weight() int64 {
	return int64(sessionOverhead + len(s.id) + cap(s.hash) + len(s.editor) + len(s.label))
}

// idle reports whether the session may leave memory: nothing uses it.
func (s *session) idle() bool { return s.conn == nil && s.holds == 0 }

// sessionRoom reports whether the session fits among those in memory once
// every idle one has left. The writer adds sessions, so the answer holds
// until it adopts this one: other goroutines only make sessions idle.
func (in *Instance) sessionRoom(s *session) error {
	in.smu.Lock()
	defer in.smu.Unlock()
	if in.sessions[s.id] != nil {
		return nil
	}
	count, bytes := len(in.sessions)+1, in.sessionBytes+s.weight()
	for _, known := range in.sessions {
		if known.idle() {
			count--
			bytes -= known.weight()
		}
	}
	if count > maxSessions || bytes > sessionsBytes {
		return unavailable("sessions")
	}
	return nil
}

// adopt puts a session in memory, or finds the one there with its id, and
// holds it for the caller until release or attach. A session that is not
// connected starts its time without activity again. To make room the
// oldest idle sessions leave memory. It runs on the writer under the write
// lock, after sessionRoom.
func (in *Instance) adopt(s *session) (*session, error) {
	in.smu.Lock()
	defer in.smu.Unlock()
	if known := in.sessions[s.id]; known != nil {
		known.holds++
		if known.conn == nil {
			known.left = in.now()
		}
		return known, nil
	}
	w := s.weight()
	for len(in.sessions)+1 > maxSessions || in.sessionBytes+w > sessionsBytes {
		var oldest *session
		for _, known := range in.sessions {
			if known.idle() && (oldest == nil || known.left.Before(oldest.left)) {
				oldest = known
			}
		}
		if oldest == nil {
			return nil, unavailable("sessions")
		}
		in.forgetSession(oldest)
		if oldest.rowless {
			in.gone.add(oldest.hash)
		}
	}
	in.nextOrd++
	s.ord = in.nextOrd
	s.sheet = &in.kept
	s.holds = 1
	// A session comes from sheet.db with the numbers it has committed.
	s.durable.Store(s.maxN)
	in.sessions[s.id] = s
	in.sessionBytes += w
	return s, nil
}

// release lets go of a session a request held. Once nothing holds it and
// no connection has it, its time without activity starts.
func (in *Instance) release(s *session) {
	in.smu.Lock()
	defer in.smu.Unlock()
	s.holds--
	if s.idle() {
		s.left = in.now()
	}
}

// attach gives a held session its connection and returns the connection
// it had, if any.
func (in *Instance) attach(s *session, c *conn) (old *conn) {
	in.smu.Lock()
	defer in.smu.Unlock()
	old = s.conn
	s.conn, s.left = c, time.Time{}
	s.holds--
	return old
}

// detach takes the connection from its session, if it still has it.
func (in *Instance) detach(c *conn) {
	in.smu.Lock()
	defer in.smu.Unlock()
	if s := c.s; s.conn == c {
		s.conn = nil
		if s.holds == 0 {
			s.left = in.now()
		}
	}
}

// forgetSession takes a session out of memory with its undo history and
// the receipts it kept there. The caller holds smu and the write lock.
func (in *Instance) forgetSession(s *session) {
	s.undo.drop()
	in.kept.release(s)
	delete(in.sessions, s.id)
	in.sessionBytes -= s.weight()
}

// goneSessions remembers the token hashes of the latest sessions without a
// row that left memory to make room, oldest first, at most goneKept.
type goneSessions struct {
	hashes [][sha256.Size]byte
	next   int
}

func (g *goneSessions) add(hash []byte) {
	var h [sha256.Size]byte
	copy(h[:], hash)
	if len(g.hashes) < goneKept {
		g.hashes = append(g.hashes, h)
		return
	}
	g.hashes[g.next] = h
	g.next = (g.next + 1) % goneKept
}

func (g *goneSessions) has(hash []byte) bool {
	for i := range g.hashes {
		if bytes.Equal(g.hashes[i][:], hash) {
			return true
		}
	}
	return false
}

// newSession creates a session and holds it for the caller. Identity comes
// only from the core route's trusted decision: a client label never earns
// verified.
func (in *Instance) newSession(ctx context.Context, editor, label string, verified bool) (*session, string, error) {
	if editor == "" || len(editor) > maxEditorBytes || !utf8.ValidString(editor) || !utf8.ValidString(label) || utf8.RuneCountInString(label) > maxLabelChars {
		return nil, "", &WriteError{Code: "invalid", Reason: "identity"}
	}
	token, err := randomToken()
	if err != nil {
		return nil, "", err
	}
	// The session owns its strings, which its weight counts.
	s := &session{hash: tokenHash(token), editor: strings.Clone(editor), label: strings.Clone(label), verified: verified}
	s.id = sessionID(s.hash)
	var adopted *session
	err = in.execute(ctx, &work{run: func() error {
		in.mu.Lock()
		defer in.mu.Unlock()
		// A session that would not fit in memory stores no row.
		if err := in.sessionRoom(s); err != nil {
			return err
		}
		// A check close to the quota measures by recording the boundary and
		// leaves exact reserves, which belong to a transaction that commits.
		reserves := maps.Clone(in.wb.files.chunks)
		err := in.transaction(ctx, func(tx *sql.Tx) error {
			// A session is a row of sheet.db. It is not stored in a flat
			// that is over its quota, nor when storing it would take the
			// flat over.
			room := func() error { return nil }
			if in.b.Limits().DiskQuota > 0 {
				other, ok := in.measure.get()
				if !ok {
					in.measure.refresh()
					return unavailable("measurement")
				}
				room = func() error { return in.checkQuota(ctx, tx, in.wb, other) }
			}
			if err := room(); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(token_hash,id,editor,label,verified,max_n,last_at) VALUES(?,?,?,?,?,0,?)`,
				s.hash, s.id, editor, label, verified, in.now().Unix()); err != nil {
				return err
			}
			return room()
		})
		if err != nil {
			in.wb.files.chunks = reserves
		}
		// Over the quota a person still opens the sheet and reads it (spec
		// rule 57 refuses edits, not reading): the session goes on without
		// its row.
		if werr := asWriteError(err); err != nil && werr.Code == "sheet_capacity" && werr.Reason == "disk_quota" {
			s.rowless, err = true, nil
		}
		if err == nil {
			adopted, err = in.adopt(s)
		}
		return err
	}})
	if err != nil {
		return nil, "", err
	}
	return adopted, token, nil
}

// storeSession gives a rowless session its row, in the transaction of the
// first action of it that changes the sheet, before the quota check of that
// action. The receipts the session kept in memory go to sheet.db with the
// row, so that sheet.db alone answers every number up to the session's
// newest, as it does for a session that always had its row. The session
// counts as stored from here on; forget takes that back when the action or
// its commit fails, and the rollback takes the rows.
func (in *Instance) storeSession(ctx context.Context, tx *sql.Tx, a *actionWork) error {
	s := a.s
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(token_hash,id,editor,label,verified,max_n,last_at) VALUES(?,?,?,?,?,?,?)`,
		s.hash, s.id, s.editor, s.label, s.verified, s.maxN, in.now().Unix()); err != nil {
		return err
	}
	for _, r := range s.recent {
		if _, err := tx.ExecContext(ctx, `INSERT INTO receipts(session,n,hash,seq,outcome) VALUES(?,?,?,?,?)`, s.id, r.n, r.hash, r.seq, []byte(r.outcome)); err != nil {
			return err
		}
	}
	s.rowless, a.rowed = false, true
	return nil
}

// resumeSession finds the session of a token and holds it for the caller.
// After a restart or a reload the session comes back from sheet.db with an
// empty undo history, and so does one that left memory to make room.
func (in *Instance) resumeSession(ctx context.Context, token string) (*session, error) {
	return in.findSession(ctx, token, false)
}

// findSession is resumeSession for a caller that says whether it waits
// through a recalculation that no edit started. A connection does, and its
// page is told. A request does not (soon) and is refused as calculating.
// A session without a row that left memory is known by its token as one
// whose receipts are gone.
func (in *Instance) findSession(ctx context.Context, token string, soon bool) (*session, error) {
	hash := tokenHash(token)
	s := &session{hash: hash, id: sessionID(hash)}
	expired := &WriteError{Code: "conflict", Reason: "session_expired", Detail: "the session expired; keep the unconfirmed edits for copying"}
	var found *session
	err := in.execute(ctx, &work{soon: soon, run: func() error {
		// A session without a row is known in memory only, for as long as
		// the instance runs.
		in.smu.Lock()
		known := in.sessions[s.id]
		gone := known == nil && in.gone.has(hash)
		in.smu.Unlock()
		if gone {
			return errReceiptExpired()
		}
		if known == nil || !known.rowless {
			if err := in.sessionRoom(s); err != nil {
				return err
			}
			err := in.transaction(ctx, func(tx *sql.Tx) error {
				var last int64
				var editor, label sql.NullString
				var editorLen, labelLen sql.NullInt64
				err := tx.QueryRowContext(ctx, `SELECT `+boundedField("editor", maxEditorBytes)+`,`+boundedField("label", maxLabelBytes)+
					`,verified,max_n,last_at FROM sessions WHERE token_hash=?`, hash).
					Scan(&editor, &editorLen, &label, &labelLen, &s.verified, &s.maxN, &last)
				if errors.Is(err, sql.ErrNoRows) || (err == nil && in.now().Unix()-last >= int64(sessionTTL/time.Second)) {
					return expired
				}
				if err == nil {
					err = errors.Join(fieldFits("session editor", editorLen, maxEditorBytes), fieldFits("session label", labelLen, maxLabelBytes))
				}
				if err != nil {
					return err
				}
				s.editor, s.label = editor.String, label.String
				// The new time is a write of sheet.db. Without room for it
				// the session resumes as it is stored.
				if fits, err := in.refreshRoom(ctx, tx, s); err != nil || !fits {
					return err
				}
				_, err = tx.ExecContext(ctx, `UPDATE sessions SET last_at=? WHERE token_hash=?`, in.now().Unix(), hash)
				return err
			})
			if err != nil {
				return err
			}
		}
		in.mu.Lock()
		defer in.mu.Unlock()
		var err error
		found, err = in.adopt(s)
		return err
	}})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// refreshRoom reports whether the disk quota admits storing the time of a
// session's resume. It asks for the room the receipt of an action needs
// (receiptRoom), which counts the write-ahead log as it is. A reader that
// holds the log keeps every frame in it, so resumes stored without that
// room would grow the log past the quota for as long as the reader
// stays. Over the quota, and without a measurement of the directory,
// nothing is stored, and the row keeps the time of its last stored resume
// and expires by it.
func (in *Instance) refreshRoom(ctx context.Context, tx *sql.Tx, s *session) (bool, error) {
	if in.b.Limits().DiskQuota <= 0 {
		return true, nil
	}
	other, ok := in.measure.get()
	if !ok {
		in.measure.refresh()
		return false, nil
	}
	// The usage is measured on the workbook, which this reads outside an
	// action, so under the read lock.
	in.mu.RLock()
	err := in.receiptRoom(ctx, tx, in.wb, s, other, 0)
	in.mu.RUnlock()
	if werr := asWriteError(err); err != nil && werr.Code == "sheet_capacity" && werr.Reason == "disk_quota" {
		return false, nil
	}
	return err == nil, err
}

// receipt returns the stored outcome of an action the session already
// sent, and the hash of that request, from the one store that holds the
// session's receipts: memory while it has no row, sheet.db from the action
// that stores the row on, whose transaction this is.
func (in *Instance) receipt(ctx context.Context, tx *sql.Tx, s *session, n uint64) (json.RawMessage, []byte, error) {
	if s.rowless {
		r, ok := findKept(s.recent, n)
		if !ok {
			return nil, nil, errReceiptExpired()
		}
		return r.outcome, r.hash, nil
	}
	r, err := readReceipt(ctx, tx, s, n)
	return r.outcome, r.hash, err
}

// storeReceipt records the outcome of an action with its number in the
// action's transaction, so that a resent action never applies twice.
func (in *Instance) storeReceipt(ctx context.Context, tx *sql.Tx, a *actionWork, seq uint64) error {
	s := a.s
	if len(a.ack) > maxOutcomeBytes {
		return &WriteError{Code: "invalid", Reason: "outcome_size"}
	}
	if s.rowless {
		// The action changed nothing, or the session would have its row:
		// the outcome is kept in memory, within the bounds of keep.
		a.prevMax, a.stored, a.memory = s.maxN, true, true
		s.maxN = a.n
		in.keep(a, storedReceipt{n: a.n, seq: seq, hash: a.hash, outcome: a.ack})
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO receipts(session,n,hash,seq,outcome) VALUES(?,?,?,?,?)`, s.id, a.n, a.hash, seq, []byte(a.ack)); err != nil {
		return err
	}
	// The expiry of sessions without activity can take the row of one that
	// stayed connected and only read. Its action is activity and stores the
	// row again, so that its receipts belong to a session sheet.db resumes.
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(token_hash,id,editor,label,verified,max_n,last_at) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(token_hash) DO UPDATE SET max_n=excluded.max_n,last_at=excluded.last_at`,
		s.hash, s.id, s.editor, s.label, s.verified, a.n, in.now().Unix()); err != nil {
		return err
	}
	// sheet.db keeps the newest receiptsKept receipts of the session, by
	// count. Finding the oldest of them walks all of them, so that runs once
	// every receiptCache receipts and the table holds fewer than receiptsKept
	// + receiptCache. Fewer numbers than receiptsKept hold no more receipts.
	if s.untrimmed == 0 && a.n > receiptsKept {
		if _, err := tx.ExecContext(ctx, `DELETE FROM receipts WHERE session=?1 AND n<=(SELECT n FROM receipts WHERE session=?1 ORDER BY n DESC LIMIT 1 OFFSET ?2)`, s.id, receiptsKept); err != nil {
			return err
		}
		s.untrimmed = receiptCache
	}
	s.untrimmed = max(s.untrimmed-1, 0)
	a.prevMax, a.stored = s.maxN, true
	s.maxN = a.n
	return nil
}

// unstored answers an action refused at admission because sheet.db has no
// room for its receipt. Every operation is refused with the reason, and
// nothing is stored: the number stays free and the receipts kept before
// stay as they are.
func (in *Instance) unstored(a *actionWork, werr *WriteError) error {
	results := make([]opResult, len(a.ops))
	for i := range results {
		results[i] = opResult{Status: "refused", Reason: werr.reason()}
	}
	data, err := json.Marshal(ackBody{Type: "ack", N: decimal(a.n), Seq: decimal(in.wb.seq), Results: results})
	if err != nil {
		return err
	}
	a.ack = data
	in.rejected.Add(1)
	return nil
}

// committed releases what memory kept of a session's receipts once the
// commit that stored its row put them in sheet.db.
func (a *actionWork) committed() {
	if a.rowed {
		a.s.sheet.release(a.s)
	}
	a.kept, a.dropped = nil, nil
}

// forget takes back the memory part of a receipt this run stored, when its
// transaction does not commit. A replay stored nothing: the receipt it read
// was committed before and stays.
func (a *actionWork) forget() {
	// The rollback may have taken back a trim.
	a.s.untrimmed = 0
	if a.rowed {
		a.s.rowless, a.rowed = true, false
	}
	if !a.stored {
		return
	}
	a.stored = false
	if a.s.maxN == a.n {
		a.s.maxN = a.prevMax
	}
	if a.memory {
		a.s.sheet.unkeep(a)
		a.memory = false
	}
}

// dropIdleUndo clears the undo history of sessions that were disconnected
// for more than 10 minutes, and forgets sessions that expired (spec rule
// 23).
// A session that a request holds stays until the request lets it go.
func (in *Instance) dropIdleUndo(now time.Time) {
	in.smu.Lock()
	defer in.smu.Unlock()
	for _, s := range in.sessions {
		if s.conn != nil || s.left.IsZero() {
			continue
		}
		if now.Sub(s.left) > undoDisconnected {
			s.undo.drop()
		}
		if now.Sub(s.left) > sessionTTL && s.holds == 0 {
			in.forgetSession(s)
		}
	}
}

// clearUndo drops every undo entry of the sheet, after a rollback, and at
// once what the entries hold outside themselves: their share of the
// sheet's undo memory, their scratch files, the edits that spilled
// payloads waited for and the collected blank records they could read. The
// next action is then admitted against what is left, without waiting for a
// tick to count again.
func (in *Instance) clearUndo() {
	p := &in.scratch
	in.smu.Lock()
	for _, s := range in.sessions {
		for _, stack := range [2][]*undoEntry{s.undo.undo, s.undo.redo} {
			for _, e := range stack {
				e.release()
			}
		}
		s.undo = undoState{}
	}
	p.trimEdits(nil)
	in.smu.Unlock()
	// What actions that did not commit admitted goes as between any two
	// changes.
	p.forgetUnplaced()
	// The store lets go of the workbook it collected from also when its
	// file was given up after a failure.
	p.blanks.clear()
	// No note is left, and no sweep of one.
	p.notes = noteBudget{}
}

// submit sends a person's action through the writer and returns its ack.
// These are the ways it ends, what the client gets, and whether that tells
// the client what became of the action:
//
//	the action                              answer                          known
//	applied, ignored or refused, in parts   ack with one result each        yes
//	  or whole, and its receipt committed
//	sent again after its receipt was        the ack of the receipt, from    yes
//	  committed                               sheet.db alone
//	sent again while its first send is in   the fate of that commit: its    yes
//	  the same open commit                    ack, or unavailable
//	sent again with other content           invalid action_number           yes: not applied
//	sent again after the receipt was        conflict receipt_expired        it was answered
//	  dropped                                                                 before; not how
//	malformed, or its base is ahead         invalid action                  yes: not applied
//	its receipt would not fit sheet.db      ack refusing every operation,   yes: not applied,
//	  within the quota                        with disk_quota or the           the number is free;
//	                                          refusal's reason, stored         sent again, it is
//	                                          nowhere                          decided again
//	not taken: stopped, the writer's        unavailable                     yes: not applied,
//	  queue or the outbox is full, no                                          the number is free
//	  measurement, scratch storage failed
//	its commit failed                       unavailable commit              yes: not applied
//	sent again, and the receipt cannot      unavailable storage             no: the outcome
//	  be read now                                                             stands, ask again
//
// An action whose receipt is committed is answered from the receipt before
// it reaches the writer's queue, and again by the writer when it committed
// while this send waited there. Such an answer stores nothing, so it is no
// part of a commit and does not depend on one.
func (in *Instance) submit(ctx context.Context, s *session, n, base uint64, ops []personOp, hash []byte) (json.RawMessage, error) {
	// sheet.db stores action numbers as signed integers. A larger one could
	// never get a receipt, and a session that kept it in memory could never
	// store its row.
	if n > math.MaxInt64 {
		return nil, invalidf("action", "an action number is at most %d", int64(math.MaxInt64))
	}
	if n <= s.durable.Load() {
		return in.committedAck(ctx, s, n, hash)
	}
	a := &actionWork{s: s, n: n, base: base, ops: ops, hash: hash}
	if err := in.execute(ctx, &work{action: a}); err != nil {
		// An earlier send of the action may have committed while this one
		// was refused a place in the queue.
		if n <= s.durable.Load() {
			return in.committedAck(ctx, s, n, hash)
		}
		return nil, err
	}
	return a.ack, nil
}

// committedAck returns the ack of an action whose receipt is committed. It
// reads sheet.db outside the writer.
func (in *Instance) committedAck(ctx context.Context, s *session, n uint64, hash []byte) (json.RawMessage, error) {
	if err := in.checkTables(ctx, "receipts"); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, unavailable("storage")
	}
	r, err := readReceipt(ctx, in.rdb, s, n)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(r.hash, hash) {
		return nil, errActionNumber()
	}
	return r.outcome, nil
}

func errReceiptExpired() *WriteError {
	return &WriteError{Code: "conflict", Reason: "receipt_expired", Detail: "the outcome of this action is no longer kept"}
}

func errActionNumber() *WriteError {
	return &WriteError{Code: "invalid", Reason: "action_number", Detail: "another action used this number"}
}

func readReceipt(ctx context.Context, q querier, s *session, n uint64) (storedReceipt, error) {
	var r storedReceipt
	var outcome []byte
	var hashLen, outcomeLen sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT `+boundedField("hash", sha256.Size)+`,`+boundedField("outcome", maxOutcomeBytes)+
		` FROM receipts WHERE session=? AND n=?`, s.id, n).Scan(&r.hash, &hashLen, &outcome, &outcomeLen)
	r.outcome = outcome
	if errors.Is(err, sql.ErrNoRows) {
		return r, errReceiptExpired()
	}
	if err == nil {
		err = errors.Join(fieldFits("receipt hash", hashLen, sha256.Size), fieldFits("receipt outcome", outcomeLen, maxOutcomeBytes))
	}
	if err != nil {
		return storedReceipt{}, unavailable("storage")
	}
	return r, nil
}
