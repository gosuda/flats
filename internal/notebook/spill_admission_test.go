package notebook

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func closeSpillManager(m *SpillManager) {
	close := reflect.ValueOf(m).MethodByName("Close")
	if close.IsValid() {
		close.Call(nil)
		return
	}
	m.CancelScope(func(Scope) bool { return true })
}

func TestSpillRunningOwnerSurvivesStartup(t *testing.T) {
	dir := t.TempDir()
	first := NewSpillManager(dir, 1, nil)
	active := spillLease(t, first, 2, 1)
	t.Cleanup(func() { first.CancelScope(func(Scope) bool { return true }) })
	second := NewSpillManager(dir, 1, nil)
	t.Cleanup(func() { second.CancelScope(func(Scope) bool { return true }) })
	if second.initErr != nil {
		t.Fatal(second.initErr)
	}
	if _, err := os.Stat(active.dir); err != nil {
		t.Fatalf("startup removed a running owner's capture: %v", err)
	}
}

func newSpillInstance(t *testing.T, manager *SpillManager, slug string) *Instance {
	t.Helper()
	opts := Options{Slug: slug, Title: slug}
	field := reflect.ValueOf(&opts).Elem().FieldByName("Spills")
	if !field.IsValid() {
		t.Fatal("Options has no host spill manager")
	}
	field.Set(reflect.ValueOf(manager))
	in, _, err := openNotebookFixture(t, t.TempDir(), &writerBackend{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.Stop)
	return in
}

func TestSpillNotebooksAndHistoryShareAdmission(t *testing.T) {
	m := NewSpillManager(t.TempDir(), 1, nil)
	first := newSpillInstance(t, m, "first")
	second := newSpillInstance(t, m, "second")
	active := spillLease(t, first.spills, 2, 1)
	other := spillLease(t, m, 1, 1)
	for _, in := range []*Instance{first, second} {
		runtime, err := in.historyRuntime()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
		_, err = runtime.manager.acquire(ctx, 2, Scope{Tree: true})
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("history escaped the shared oversized slot: %v", err)
		}
	}
	first.Stop()
	if _, ok := m.Lookup(active.Cursor()); ok {
		t.Fatal("stopped notebook retained its capture")
	}
	if _, ok := m.Lookup(other.Cursor()); !ok {
		t.Fatal("stopping one notebook cancelled another owner's capture")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	next, err := second.spills.acquire(ctx, 2, Scope{Tree: true})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	defer other.Close()
}

func acquireSpillCapture(ctx context.Context, in *Instance, query, ticket string) (*Spill, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(query), &fields); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, agentAdmissionKey{}, agentAdmission{query: contentHash(canonical), ticket: ticket})
	return in.spills.acquire(ctx, 2, Scope{Tree: true})
}

func retrySpillCapture(t *testing.T, in *Instance, query string, retry ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	var err error
	if len(retry) > 0 {
		_, err = acquireSpillCapture(ctx, in, query, retry[0])
	} else {
		_, err = in.Read(ctx, []byte(query), false)
	}
	var refusal *WriteError
	if !errors.As(err, &refusal) || refusal.Code != "unavailable" || !refusal.Retry {
		t.Fatalf("read must refuse promptly with retry guidance: %v", err)
	}
	var hint struct {
		Admission string `json:"admission"`
		Position  int    `json:"position"`
		Expires   int64  `json:"expires_in_ms"`
	}
	if err := json.Unmarshal([]byte(refusal.Detail), &hint); err != nil || hint.Admission == "" || hint.Position < 1 || hint.Expires <= 0 {
		t.Fatalf("missing ordered retry hint: %+v (%v)", refusal, err)
	}
	return hint.Admission, hint.Position
}

func TestSpillAgentRetryOrder(t *testing.T) {
	f := newWriterFixture(t)
	m := f.in.spills
	m.budget = 1
	active := spillLease(t, m, 2, 1)
	queries := []string{`{"limit":1}`, `{"limit":1}`, `{"limit":1}`}
	ids := make([]string, len(queries))
	for i, q := range queries {
		id, position := retrySpillCapture(t, f.in, q)
		if position != i+1 {
			t.Fatalf("position=%d, want %d", position, i+1)
		}
		ids[i] = id
	}
	for i := len(queries) - 1; i >= 0; i-- {
		id, position := retrySpillCapture(t, f.in, queries[i], ids[i])
		if id != ids[i] || position != i+1 {
			t.Fatalf("retry lost its FIFO position: %s/%d", id, position)
		}
	}
	if err := active.Close(); err != nil {
		t.Fatal(err)
	}
	fresh, position := retrySpillCapture(t, f.in, queries[0])
	if position != 4 || fresh == ids[0] {
		t.Fatal("a fresh identical query stole the head retry's reservation")
	}
	retrySpillCapture(t, f.in, queries[2], ids[2])
	retrySpillCapture(t, f.in, queries[1], ids[1])
	if s, err := acquireSpillCapture(t.Context(), f.in, queries[0], ids[0]); err != nil {
		t.Fatalf("head retry did not acquire the released slot: %v", err)
	} else {
		s.Close()
	}
	if s, err := acquireSpillCapture(t.Context(), f.in, queries[1], ids[1]); err != nil {
		t.Fatalf("second retry did not follow the completed head: %v", err)
	} else {
		s.Close()
	}
	if s, err := acquireSpillCapture(t.Context(), f.in, queries[2], ids[2]); err != nil {
		t.Fatalf("third retry did not follow the completed second: %v", err)
	} else {
		s.Close()
	}
}

func TestRetryAgentRetryWire(t *testing.T) {
	f := newWriterFixture(t)
	f.in.spills.budget = 1
	active := spillLease(t, f.in.spills, 2, 1)
	token, _ := retrySpillCapture(t, f.in, `{"limit":1}`)
	if err := active.Close(); err != nil {
		t.Fatal(err)
	}
	req, _ := json.Marshal(map[string]any{"limit": 1, "retry_hint": token})
	if _, err := f.in.Read(t.Context(), req, false); err != nil {
		t.Fatalf("wire retry lost admission: %v", err)
	}
}

func TestSpillCrashOwnerHelper(t *testing.T) {
	root := os.Getenv("NOTEBOOK_SPILL_CRASH_ROOT")
	if root == "" {
		t.Skip("subprocess helper")
	}
	m := NewSpillManager(root, 1, nil)
	s := spillLease(t, m, 2, 1)
	if _, err := os.Stdout.WriteString(s.dir); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestSpillCrashCleanupPreservesLiveOwner(t *testing.T) {
	root := t.TempDir()
	live := NewSpillManager(root, 1, nil)
	defer closeSpillManager(live)
	capture := spillLease(t, live, 2, 1)
	cmd := exec.Command(os.Args[0], "-test.run=^TestSpillCrashOwnerHelper$")
	cmd.Env = append(os.Environ(), "NOTEBOOK_SPILL_CRASH_ROOT="+root)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("crash helper: %s (%v)", out, err)
	}
	abandoned := strings.TrimSpace(string(out))
	if _, err := os.Stat(abandoned); err != nil {
		t.Fatal(err)
	}
	restarted := NewSpillManager(root, 1, nil)
	defer closeSpillManager(restarted)
	if restarted.initErr != nil {
		t.Fatal(restarted.initErr)
	}
	if _, err := os.Stat(abandoned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("crashed owner's files remain: %v", err)
	}
	if _, err := os.Stat(capture.dir); err != nil {
		t.Fatalf("live owner was cleaned: %v", err)
	}
}

func TestSpillCurrentAndHistoricalCaptureContention(t *testing.T) {
	m := NewSpillManager(t.TempDir(), 1, nil)
	defer closeSpillManager(m)
	first := newSpillInstance(t, m, "first")
	second := newSpillInstance(t, m, "second")
	historyEdit(t, first, CreatePageOp{CreateFields: CreateFields{Ref: "extra"}, Title: "Extra"})
	src := historySource(t, first, 1)
	current, err := first.spills.Capture(t.Context(), first.ReadTx, Scope{Tree: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := first.ReadRevision(t.Context(), src, []byte(`{"limit":1}`))
		done <- err
	}()
	spillPending(t, m, 1)
	ticket, _ := retrySpillCapture(t, second, `{}`)
	if err := current.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("history did not acquire the released current slot")
	}
	retrySpillCapture(t, second, `{}`, ticket)
	first.Stop()
	if _, err := acquireSpillCapture(t.Context(), second, `{}`, ticket); err != nil {
		t.Fatalf("history stop retained the oversized slot: %v", err)
	}
}

func TestSpillRetryExpiryReleasesReservation(t *testing.T) {
	clock := newSpillClock()
	m := NewSpillManager(t.TempDir(), 1, clock.now)
	defer closeSpillManager(m)
	in := newSpillInstance(t, m, "notes")
	active := spillLease(t, m, 2, 1)
	retrySpillCapture(t, in, `{}`)
	if err := active.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		s, err := m.acquire(t.Context(), 2, Scope{Tree: true})
		if s != nil {
			s.Close()
		}
		done <- err
	}()
	spillPending(t, m, 2)
	clock.advance(10 * time.Second)
	m.Lookup("expired-ticket")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("expired retry reserved the slot forever")
	}
}

func TestSpillCaptureCleanupLeavesEmptyScratch(t *testing.T) {
	m := NewSpillManager(t.TempDir(), 1, nil)
	defer closeSpillManager(m)
	in := newSpillInstance(t, m, "notes")
	s, err := in.spills.Capture(t.Context(), in.ReadTx, Scope{Tree: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(m.dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("capture cleanup left owner metadata in scratch: %v, %v", files, err)
	}
}

func TestSpillLegacyCrashRootCleanup(t *testing.T) {
	dir := t.TempDir()
	dead := filepath.Join(dir, "notebook-spill-0123456789abcdef0123456789abcdef")
	if err := os.Mkdir(dead, 0700); err != nil {
		t.Fatal(err)
	}
	m := NewSpillManager(dir, 1, nil)
	t.Cleanup(func() { m.CancelScope(func(Scope) bool { return true }) })
	if _, err := os.Stat(dead); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned legacy capture survived: %v", err)
	}
}

func TestRetryReadContention(t *testing.T) {
	var seconds atomic.Int64
	now := func() time.Time { return time.Unix(1_000_000+seconds.Load(), 0) }
	m := NewSpillManager(t.TempDir(), 1, now)
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	first := newSpillInstance(t, m, "first")
	second := newSpillInstance(t, m, "second")
	read := func(in *Instance, ticket string, limit int) ([]byte, error) {
		t.Helper()
		fields := map[string]any{"limit": limit}
		if ticket != "" {
			fields["retry_hint"] = ticket
		}
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		return in.Read(ctx, raw, false)
	}
	hint := func(in *Instance, ticket string, position int) string {
		t.Helper()
		_, err := read(in, ticket, 1)
		var refusal *WriteError
		if !errors.As(err, &refusal) || refusal.Code != "unavailable" || refusal.Reason != "spill_admission" || !refusal.Retry {
			t.Fatalf("expected admission refusal: %v", err)
		}
		var got struct {
			Admission string `json:"admission"`
			Position  int    `json:"position"`
			Expires   int64  `json:"expires_in_ms"`
		}
		if err := json.Unmarshal([]byte(refusal.Detail), &got); err != nil || got.Admission == "" || got.Position != position || got.Expires <= 0 {
			t.Fatalf("hint=%+v (%v), want position %d", got, err, position)
		}
		if ticket != "" && ticket != got.Admission {
			t.Fatal("retry changed its ticket")
		}
		return got.Admission
	}
	success := func(in *Instance, ticket string) {
		t.Helper()
		raw, err := read(in, ticket, 1)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Complete bool `json:"complete"`
			Tree     []struct {
				Title string `json:"title"`
			} `json:"tree"`
		}
		if err := json.Unmarshal(raw, &got); err != nil || !got.Complete || len(got.Tree) != 1 || got.Tree[0].Title != in.slug {
			t.Fatalf("wrong notebook content: %s (%v)", raw, err)
		}
	}
	expired := func(in *Instance, ticket string, limit int) {
		t.Helper()
		_, err := read(in, ticket, limit)
		var refusal *WriteError
		if !errors.As(err, &refusal) || refusal.Code != "conflict" || refusal.Reason != "read_expired" || refusal.Retry {
			t.Fatalf("expected expired ticket refusal: %v", err)
		}
	}
	active := spillLease(t, m, 2, 1)
	instances := []*Instance{first, second, first}
	tickets := make([]string, len(instances))
	for i, in := range instances {
		tickets[i] = hint(in, "", i+1)
	}
	for i := len(instances) - 1; i >= 0; i-- {
		hint(instances[i], tickets[i], i+1)
	}
	expired(second, tickets[0], 1)
	expired(first, tickets[0], 2)
	if err := active.Close(); err != nil {
		t.Fatal(err)
	}
	fresh := hint(first, "", 4)
	for _, ticket := range tickets {
		if ticket == fresh {
			t.Fatal("fresh request reused ticket")
		}
	}
	hint(first, tickets[2], 3)
	hint(second, tickets[1], 2)
	success(first, tickets[0])
	hint(first, tickets[2], 2)
	success(second, tickets[1])
	success(first, tickets[2])
	success(first, fresh)
	expired(first, tickets[0], 1)
	expired(first, "unknown-ticket", 1)
	active = spillLease(t, m, 2, 1)
	ticket := hint(second, "", 1)
	seconds.Add(11)
	expired(second, ticket, 1)
	if err := active.Close(); err != nil {
		t.Fatal(err)
	}
	success(second, "")
}

func TestRetryReadStrictQuery(t *testing.T) {
	f := newWriterFixture(t)
	for _, tt := range []struct{ name, query string }{
		{"empty", `{"retry_hint":""}`},
		{"null", `{"retry_hint":null}`},
		{"number", `{"retry_hint":1}`},
		{"duplicate", `{"retry_hint":"one","retry_hint":"two"}`},
		{"cursor", `{"cursor":"cursor","retry_hint":"ticket"}`},
		{"mapping", `{"cursor":"cursor","op_id":"write","retry_hint":"ticket"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.in.Read(t.Context(), []byte(tt.query), false)
			var refusal *WriteError
			if !errors.As(err, &refusal) || refusal.Code != "invalid" || refusal.Retry {
				t.Fatalf("expected invalid query: %v", err)
			}
		})
	}
}
