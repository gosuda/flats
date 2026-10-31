package notebook

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestWriterSessionCreationRespectsQuota(t *testing.T) {
	f := newWriterFixture(t)
	f.b.quota.Store(1)
	for range 3 {
		_, err := f.in.NewSession(t.Context(), "another", "Another", false)
		writerReason(t, err, "disk_quota", false)
	}
	if writerCount(t, f.in, "sessions") != 1 {
		t.Fatal("session creation bypassed quota")
	}
}

func TestWriterSessionsHashSecretsResumeAndExpire(t *testing.T) {
	f := newWriterFixture(t)
	var token string
	if err := f.in.readers.QueryRowContext(t.Context(), `SELECT token_hash FROM sessions`).Scan(&token); err != nil {
		t.Fatal(err)
	}
	if token == f.session.Token || strings.Contains(f.session.ID, f.session.Token) || token != contentHash([]byte(f.session.Token)) {
		t.Fatal("resume secret was persisted or exposed")
	}
	f.clock.Add(23 * 3600)
	resumed, err := f.in.ResumeSession(t.Context(), f.session.Token)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Token != f.session.Token || resumed.ID != f.session.ID || resumed.Verified != true {
		t.Fatal("reconnect changed identity")
	}
	f.clock.Add(24 * 3600)
	_, err = f.in.ResumeSession(t.Context(), f.session.Token)
	writerReason(t, err, "session_expired", false)
}

func TestWriterStaleMeasurementDoesNotConsumeNumber(t *testing.T) {
	f := newWriterFixture(t)
	f.b.measureErr.Store(true)
	f.clock.Add(int64(measureTTL/time.Second) + 1)
	a := writerAction(f.page, "Retry same number")
	_, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, a)
	writerReason(t, err, "measurement", true)
	if writerCount(t, f.in, "receipts") != 0 {
		t.Fatal("retryable admission consumed n")
	}
	f.b.measureErr.Store(false)
	if err := f.in.Remeasure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, a); err != nil {
		t.Fatal(err)
	}
}

func TestWriterRuntimeRetentionDoesNotReexecuteOldNumbers(t *testing.T) {
	f := newWriterFixture(t)
	a := writerAction(f.page, "Retained")
	if _, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, a); err != nil {
		t.Fatal(err)
	}
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		return trimRuntimeBytes(t.Context(), tx, "receipts", "length(outcome)", 0)
	}); err != nil {
		t.Fatal(err)
	}
	_, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, a)
	writerReason(t, err, "receipt_expired", false)
	if n := writerCount(t, f.in, "deltas"); n != 1 {
		t.Fatalf("old number reexecuted: %d deltas", n)
	}
	f.clock.Add(int64(time.Hour / time.Second))
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error { return f.in.maintenance(t.Context(), tx) }); err != nil {
		t.Fatal(err)
	}
	if writerCount(t, f.in, "deltas") != 0 {
		t.Fatal("one-hour delta retention was not enforced")
	}
}

func TestWriterPersonIdleBoundaryBeforeNewEdit(t *testing.T) {
	f := newWriterFixture(t)
	if _, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, writerAction(f.page, "Before idle")); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(31)
	a := writerAction(f.page, "After idle")
	a.N = 2
	if _, err := f.in.SubmitAction(t.Context(), f.session.Token, 2, a); err != nil {
		t.Fatal(err)
	}
	if n := writerCount(t, f.in, "outbox"); n != 2 {
		t.Fatalf("idle period was merged across new edit: %d", n)
	}
	var started int64
	if err := f.in.readers.QueryRowContext(t.Context(), `SELECT started_at FROM groups`).Scan(&started); err != nil || started != f.clock.Load() {
		t.Fatalf("new group start=%d %v", started, err)
	}
}
