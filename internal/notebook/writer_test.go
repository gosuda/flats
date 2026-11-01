package notebook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type writerBackend struct {
	quota      atomic.Int64
	other      atomic.Int64
	ready      atomic.Int64
	measureErr atomic.Bool
}

func (b *writerBackend) Limits() Limits { return Limits{DiskQuota: b.quota.Load()} }
func (b *writerBackend) MeasureOther(context.Context) (int64, error) {
	if b.measureErr.Load() {
		return 0, fmt.Errorf("measurement unavailable")
	}
	return b.other.Load(), nil
}
func (*writerBackend) PutBlob(context.Context, string, []byte) (bool, error) {
	return false, fmt.Errorf("unexpected blob write")
}
func (*writerBackend) ReadBlob(context.Context, string) ([]byte, error) {
	return nil, fmt.Errorf("unexpected blob read")
}
func (*writerBackend) Lease([]string) func() { return func() {} }
func (b *writerBackend) Exclusive(ctx context.Context, fn func(context.Context, int64) error) error {
	return fn(ctx, b.other.Load())
}
func (b *writerBackend) OutboxReady() { b.ready.Add(1) }

type writerFixture struct {
	in      *Instance
	b       *writerBackend
	clock   *atomic.Int64
	session Session
	page    Ref
}

func newWriterFixture(t *testing.T) writerFixture {
	t.Helper()
	clock := new(atomic.Int64)
	clock.Store(10000)
	b := &writerBackend{}
	in, created, err := openNotebookFixture(t, t.TempDir(), b, Options{Slug: "notes", Title: "First", Now: func() time.Time { return time.Unix(clock.Load(), 0) }})
	if err != nil || !created {
		t.Fatalf("Open: %t %v", created, err)
	}
	t.Cleanup(in.Stop)
	session, err := in.NewSession(t.Context(), "person", "Person", true)
	if err != nil {
		t.Fatal(err)
	}
	var page Ref
	if err := in.ReadTx(t.Context(), func(tx *sql.Tx, seq Sequence) error {
		var id ID
		if err := tx.QueryRowContext(t.Context(), `SELECT id FROM items WHERE kind='page' AND alive=1`).Scan(&id); err != nil {
			return err
		}
		record, err := loadRecord(t.Context(), tx, id)
		page = record.Item.Ref
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return writerFixture{in, b, clock, session, page}
}
func writerAction(page Ref, title string) Action {
	return Action{N: 1, ActionID: "title", Ops: []Operation{SetPageTitleOp{Page: Target{Ref: &page}, Title: title}}}
}
func writerTitle(t *testing.T, in *Instance, page Ref) string {
	t.Helper()
	var title string
	if err := in.ReadTx(t.Context(), func(tx *sql.Tx, _ Sequence) error {
		r, err := loadRecord(t.Context(), tx, page.ID)
		if err == nil {
			title = r.Item.Data.(Page).Title
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return title
}
func writerCount(t *testing.T, in *Instance, table string) int {
	t.Helper()
	var n int
	if err := in.readers.QueryRowContext(t.Context(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func writerReason(t *testing.T, err error, reason string, retry bool) {
	t.Helper()
	var e *WriteError
	if !errors.As(err, &e) || e.Reason != reason || e.Retry != retry {
		t.Fatalf("error=%v, want %s retry=%t", err, reason, retry)
	}
}

func TestWriterSavedOnlyAfterCommit(t *testing.T) {
	f := newWriterFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.in.hook = func(stage string, _ *sql.Tx) {
		if stage == "commit" {
			close(entered)
			<-release
		}
	}
	done := make(chan error, 1)
	go func() {
		ack, err := f.in.SubmitAction(context.Background(), f.session.Token, 1, writerAction(f.page, "Durable"))
		if err == nil && ack.Summary.Applied != 1 {
			err = fmt.Errorf("ack=%+v", ack)
		}
		done <- err
	}()
	<-entered
	select {
	case err := <-done:
		t.Fatalf("ack before commit: %v", err)
	default:
	}
	if n := writerCount(t, f.in, "receipts"); n != 0 {
		t.Fatalf("uncommitted receipt visible: %d", n)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := writerTitle(t, f.in, f.page); got != "Durable" {
		t.Fatalf("title=%q", got)
	}
	var full int
	if err := f.in.db.db.QueryRowContext(t.Context(), `PRAGMA synchronous`).Scan(&full); err != nil || full != 2 {
		t.Fatalf("synchronous=%d %v", full, err)
	}
}

func TestWriterCommitFailureDoesNotSave(t *testing.T) {
	f := newWriterFixture(t)
	f.in.hook = func(stage string, tx *sql.Tx) {
		if stage == "commit" {
			if err := tx.Rollback(); err != nil {
				t.Error(err)
			}
		}
	}
	_, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, writerAction(f.page, "Not durable"))
	writerReason(t, err, "commit", true)
	if got := writerTitle(t, f.in, f.page); got != "First" {
		t.Fatalf("failed commit changed title: %q", got)
	}
	if n := writerCount(t, f.in, "receipts"); n != 0 {
		t.Fatalf("receipt survived failed commit: %d", n)
	}
	f.in.hook = nil
	if _, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, writerAction(f.page, "Retry")); err != nil {
		t.Fatal(err)
	}
}

func TestWriterDuplicateAndExactReplay(t *testing.T) {
	f := newWriterFixture(t)
	a := writerAction(f.page, "Once")
	var acks [2]Ack
	var errs [2]error
	var wg sync.WaitGroup
	for i := range acks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			acks[i], errs[i] = f.in.SubmitAction(context.Background(), f.session.Token, 1, a)
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || !reflect.DeepEqual(acks[0], acks[1]) {
		t.Fatalf("replay: %+v %v", acks, errs)
	}
	if n := writerCount(t, f.in, "receipts"); n != 1 {
		t.Fatalf("receipts=%d", n)
	}
	if n := writerCount(t, f.in, "deltas"); n != 1 {
		t.Fatalf("deltas=%d", n)
	}
	_, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, writerAction(f.page, "Different"))
	writerReason(t, err, "action_conflict", false)
	gap := writerAction(f.page, "Gap")
	gap.N = 3
	_, err = f.in.SubmitAction(t.Context(), f.session.Token, 3, gap)
	writerReason(t, err, "action_gap", false)
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error { _, err := tx.ExecContext(t.Context(), `DELETE FROM receipts`); return err }); err != nil {
		t.Fatal(err)
	}
	_, err = f.in.SubmitAction(t.Context(), f.session.Token, 1, a)
	writerReason(t, err, "receipt_expired", false)
	if got := writerTitle(t, f.in, f.page); got != "Once" {
		t.Fatal(got)
	}
}

func TestWriterAgentAtomicityGuardsAndReplay(t *testing.T) {
	f := newWriterFixture(t)
	person, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, writerAction(f.page, "Person draft"))
	if err != nil {
		t.Fatal(err)
	}
	u := Update{Slug: "notes", OpID: "atomic", Ops: []Operation{SetPageTitleOp{Page: Target{Ref: &f.page}, Title: "Must roll back"}, DeletePageOp{Page: Target{Ref: &f.page}}}}
	before := writerCount(t, f.in, "outbox")
	if _, err := f.in.Update(t.Context(), u, 0); err == nil {
		t.Fatal("partially accepted failing agent call")
	}
	if got := writerTitle(t, f.in, f.page); got != "Person draft" {
		t.Fatal(got)
	}
	if writerCount(t, f.in, "outbox") != before || writerCount(t, f.in, "groups") != 1 || writerCount(t, f.in, "commands") != 0 {
		t.Fatal("failed call changed boundaries or receipt")
	}
	u.Ops = u.Ops[:1]
	u.Guards = &Guards{State: &State{f.in.StoreID(), 1}}
	_, err = f.in.Update(t.Context(), u, 0)
	writerReason(t, err, "guard", false)
	if writerCount(t, f.in, "outbox") != before {
		t.Fatal("guard failure closed person group")
	}
	u.Guards.State.Seq = person.Seq
	result, err := f.in.Update(t.Context(), u, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied != 1 || !result.RevisionPending || result.Revision != nil {
		t.Fatalf("result=%+v", result)
	}
	if writerCount(t, f.in, "groups") != 0 || writerCount(t, f.in, "outbox") != before+2 {
		t.Fatal("missing person/agent boundaries")
	}
	replay, err := f.in.Update(t.Context(), u, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replay {
		t.Fatal("not marked replay")
	}
	replay.Replay = false
	if !reflect.DeepEqual(result, replay) {
		t.Fatalf("different result on replay: %+v %+v", result, replay)
	}
	u.Ops = []Operation{SetPageTitleOp{Page: Target{Ref: &f.page}, Title: "Other"}}
	_, err = f.in.Update(t.Context(), u, 0)
	writerReason(t, err, "op_id_conflict", false)
}

func TestWriterQuotaRefusalReserveAndOverQuotaDeletion(t *testing.T) {
	f := newWriterFixture(t)
	// Leave admission's reserved refusal space, but not the future-revision
	// reservation. Even deleting otherwise legal content must be refused.
	var low int64
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		low = writerQuotaEstimate(t, f.in, tx, 0, 0) - 8*(2*f.in.db.pageSize+24) + 2*refusalHeadroom + 8192
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.b.quota.Store(low)
	id, err := NewID(PageKind)
	if err != nil {
		t.Fatal(err)
	}
	a := Action{N: 1, ActionID: "quota", Ops: []Operation{CreatePageOp{CreateFields: CreateFields{Ref: "extra", ProposedID: &id}, Title: "Other"}}}
	ack, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, a)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Summary.Refused != 1 || ack.Results[0].Reason != "disk_quota" {
		t.Fatalf("ack=%+v", ack)
	}
	if writerCount(t, f.in, "deltas") != 1 || writerCount(t, f.in, "items") != 1 {
		t.Fatal("quota refusal must record only its empty sequence delta")
	}
	var effects string
	if err := f.in.readers.QueryRowContext(t.Context(), `SELECT effects FROM deltas`).Scan(&effects); err != nil {
		t.Fatal(err)
	}
	var empty struct {
		Changes []json.RawMessage `json:"changes"`
	}
	if err := json.Unmarshal([]byte(effects), &empty); err != nil || len(empty.Changes) != 0 {
		t.Fatalf("refusal leaked canonical changes: %s (%v)", effects, err)
	}
	var bytes int
	if err := f.in.readers.QueryRowContext(t.Context(), `SELECT length(outcome) FROM receipts`).Scan(&bytes); err != nil || bytes > refusalHeadroom {
		t.Fatalf("receipt bytes=%d %v", bytes, err)
	}
	replay, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, a)
	if err != nil || !reflect.DeepEqual(ack, replay) {
		t.Fatalf("quota replay: %+v %v", replay, err)
	}
	_, err = f.in.Update(t.Context(), Update{Slug: "notes", OpID: "delete-over-quota", Ops: []Operation{DeletePageOp{Page: Target{Ref: &f.page}}}}, 0)
	writerReason(t, err, "disk_quota", false)
	if got := writerTitle(t, f.in, f.page); got != "First" {
		t.Fatal(got)
	}
}

func TestWriterGroupCloseClocksAndCredits(t *testing.T) {
	f := newWriterFixture(t)
	other, err := f.in.NewSession(t.Context(), "other", "Other", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, writerAction(f.page, "A")); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(10)
	if _, err := f.in.SubmitAction(t.Context(), other.Token, 1, writerAction(f.page, "B")); err != nil {
		t.Fatal(err)
	}
	tick := func() {
		t.Helper()
		if err := f.in.write(t.Context(), func(tx *sql.Tx) error { return f.in.maintenance(t.Context(), tx) }); err != nil {
			t.Fatal(err)
		}
	}
	f.clock.Store(10029)
	tick()
	if n := writerCount(t, f.in, "outbox"); n != 1 {
		t.Fatalf("early boundary: %d", n)
	}
	f.clock.Store(10030)
	tick()
	entries, err := f.in.Outbox(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || !reflect.DeepEqual(entries[1].Authors, []string{"Other", "Person"}) {
		t.Fatalf("credits=%+v", entries)
	}
	f.clock.Store(10040)
	tick()
	if n := writerCount(t, f.in, "outbox"); n != 2 {
		t.Fatalf("duplicate clean boundary: %d", n)
	}
	f.clock.Store(10100)
	for n := uint64(2); n <= 12; n++ {
		if err := f.in.Remeasure(t.Context()); err != nil {
			t.Fatal(err)
		}
		a := writerAction(f.page, fmt.Sprint(n))
		a.N = Sequence(n)
		if _, err := f.in.SubmitAction(t.Context(), other.Token, n, a); err != nil {
			t.Fatal(err)
		}
		if n < 12 {
			f.clock.Add(29)
		}
	}
	// This editor's group began at 10100; continued edits do not reset it.
	f.clock.Store(10400)
	tick()
	if n := writerCount(t, f.in, "groups"); n != 0 {
		t.Fatalf("maximum age did not close: %d", n)
	}
}

func TestWriterCrashProcess(t *testing.T) {
	if os.Getenv("NOTEBOOK_WRITER_CRASH") != "1" {
		return
	}
	in, _, err := openNotebookFixture(t, os.Getenv("NOTEBOOK_WRITER_DIR"), &writerBackend{}, Options{Slug: "notes", Now: func() time.Time { return time.Unix(10000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	var page Ref
	if err := json.Unmarshal([]byte(os.Getenv("NOTEBOOK_WRITER_PAGE")), &page); err != nil {
		t.Fatal(err)
	}
	in.hook = func(stage string, _ *sql.Tx) {
		if stage == "committed" {
			os.Exit(23)
		}
	}
	_, _ = in.SubmitAction(t.Context(), os.Getenv("NOTEBOOK_WRITER_SESSION"), 1, writerAction(page, "Survives crash"))
	t.Fatal("did not crash between commit and reply")
}

func TestWriterCrashBetweenCommitAndReplyReplays(t *testing.T) {
	f := newWriterFixture(t)
	dir := filepath.Dir(f.in.db.path)
	pageJSON := fmt.Sprintf(`{"id":%q,"inc":%q}`, f.page.ID, decimalN(uint64(f.page.Inc)))
	f.in.Stop()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestWriterCrashProcess$")
	cmd.Env = append(os.Environ(), "NOTEBOOK_WRITER_CRASH=1", "NOTEBOOK_WRITER_DIR="+dir, "NOTEBOOK_WRITER_PAGE="+pageJSON, "NOTEBOOK_WRITER_SESSION="+f.session.Token)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("crash helper: %v %s", err, output)
	}
	in, created, err := openNotebookFixture(t, dir, f.b, Options{Slug: "notes", Now: func() time.Time { return time.Unix(10000, 0) }})
	if err != nil || created {
		t.Fatalf("reopen: %t %v", created, err)
	}
	defer in.Stop()
	ack, err := in.SubmitAction(t.Context(), f.session.Token, 1, writerAction(f.page, "Survives crash"))
	if err != nil {
		t.Fatal(err)
	}
	if ack.Summary.Applied != 1 || writerTitle(t, in, f.page) != "Survives crash" || writerCount(t, in, "receipts") != 1 || writerCount(t, in, "deltas") != 1 {
		t.Fatalf("crash replay: %+v", ack)
	}
	var stored string
	if err := in.readers.QueryRowContext(t.Context(), `SELECT outcome FROM receipts`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored, `"applied":1`) {
		t.Fatal(stored)
	}
}
