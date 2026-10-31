package notebook

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
	"unicode/utf8"
)

// Session contains the resume secret; only ID may appear in source tags.
type Session struct {
	Token    string    `json:"session"`
	ID       string    `json:"session_id"`
	Expires  time.Time `json:"expires"`
	Label    string    `json:"label"`
	Verified bool      `json:"verified"`
}

type sessionRecord struct {
	hash     string
	editor   string
	label    string
	verified bool
	max      uint64
	last     int64
}

func randomNotebookToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func sessionSource(hash string) string { return contentHash([]byte("notebook-session:" + hash))[:32] }

// NewSession accepts identity only from the core route's trusted decision.
// Unverified client labels must never supply verified=true.
func (in *Instance) NewSession(ctx context.Context, editor, label string, verified bool) (Session, error) {
	if editor == "" || len(editor) > 1024 || !utf8.ValidString(editor) || !utf8.ValidString(label) || utf8.RuneCountInString(label) > 200 {
		return Session{}, &WriteError{Code: "invalid", Reason: "identity"}
	}
	token, err := randomNotebookToken()
	if err != nil {
		return Session{}, err
	}
	hash := contentHash([]byte(token))
	expires := in.now().Add(24 * time.Hour)
	err = in.write(ctx, func(tx *sql.Tx) error {
		other, valid := in.measure.get()
		if !valid {
			in.measure.refresh()
			return unavailable("measurement")
		}
		if err := in.checkQuota(ctx, tx, other, 0); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(token_hash,editor,label,verified,max_n,last_at) VALUES(?,?,?,?,'0',?)`, hash, editor, label, verified, in.now().Unix()); err != nil {
			return err
		}
		return in.checkQuota(ctx, tx, other, 0)
	})
	return Session{Token: token, ID: sessionSource(hash), Expires: expires, Label: label, Verified: verified}, err
}

// ResumeSession refreshes a known session without changing its undo identity.
func (in *Instance) ResumeSession(ctx context.Context, token string) (Session, error) {
	var result Session
	err := in.write(ctx, func(tx *sql.Tx) error {
		s, err := readSession(ctx, tx, contentHash([]byte(token)), in.now())
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET last_at=? WHERE token_hash=?`, in.now().Unix(), s.hash); err != nil {
			return err
		}
		result = Session{Token: token, ID: sessionSource(s.hash), Expires: in.now().Add(24 * time.Hour), Label: s.label, Verified: s.verified}
		return nil
	})
	return result, err
}

func readSession(ctx context.Context, tx *sql.Tx, hash string, now time.Time) (sessionRecord, error) {
	s := sessionRecord{hash: hash}
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT editor,label,verified,max_n,last_at FROM sessions WHERE token_hash=?`, hash).Scan(&s.editor, &s.label, &s.verified, &raw, &s.last)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && now.Unix()-s.last >= int64(24*time.Hour/time.Second)) {
		return s, &WriteError{Code: "conflict", Reason: "session_expired", Detail: "session expired; preserve pending edits"}
	}
	if err != nil {
		return s, err
	}
	s.max, err = parseDBDecimal(raw)
	return s, err
}

func touchGroup(ctx context.Context, tx *sql.Tx, session sessionRecord, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO groups(editor,started_at,last_at,dirty,label) VALUES(?,?,?,1,?) ON CONFLICT(editor) DO UPDATE SET last_at=excluded.last_at,dirty=1,label=excluded.label`, session.editor, now.Unix(), now.Unix(), session.label)
	return err
}

func permanentWriteError(err error) error {
	var refusal *WriteError
	if errors.As(err, &refusal) {
		return err
	}
	var problem *editFailure
	if errors.As(err, &problem) {
		code := "invalid"
		if problem.Reason == "gone" || problem.Reason == "guard" {
			code = "conflict"
		}
		if problem.Reason == "item_limit" {
			code = "notebook_capacity"
		}
		return &WriteError{Code: code, Reason: problem.Reason, Detail: problem.Detail}
	}
	return &WriteError{Code: "unavailable", Reason: "storage", Retry: true, cause: err}
}
