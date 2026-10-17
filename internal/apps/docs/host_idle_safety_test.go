package docs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
)

// Faults affect host/worker communication, never the real worker transaction.
type idleHostFault struct {
	budgetUnavailable atomic.Bool
	outboxUnavailable atomic.Bool
	dirtyProof        atomic.Int64
	successfulBudgets atomic.Int64
}

func (f *idleHostFault) handler(next http.Handler, quiescing bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (f.budgetUnavailable.Load() && r.URL.Path == "/_docs/host/revisions/budget") || (f.outboxUnavailable.Load() && r.URL.Path == "/_docs/host/revisions/outbox") {
			http.Error(w, "injected host operation failure", 503)
			return
		}
		if (quiescing && r.URL.Path == "/_docs/host/revisions/sync") || r.URL.Path == "/_docs/host/revisions/budget" {
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			var proof struct {
				Quiescent bool `json:"quiescent"`
			}
			if rec.Code == 200 && r.URL.Path == "/_docs/host/revisions/budget" {
				f.successfulBudgets.Add(1)
			}
			if r.URL.Path == "/_docs/host/revisions/sync" && rec.Code == 200 && json.Unmarshal(rec.Body.Bytes(), &proof) == nil && !proof.Quiescent {
				f.dirtyProof.Add(1)
			}
			for name, values := range rec.Header() {
				w.Header()[name] = values
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())
			return
		}
		next.ServeHTTP(w, r)
	})
}

type idleFaultInstance struct {
	core.ManagedDocsInstance
	fault *idleHostFault
}
type idleFaultLease struct {
	core.RuntimeLease
	handler http.Handler
}

func (l *idleFaultLease) ServeHTTP(w http.ResponseWriter, r *http.Request) { l.handler.ServeHTTP(w, r) }

type idleFaultQuiescence struct {
	core.RuntimeQuiescence
	handler http.Handler
}

func (q *idleFaultQuiescence) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q.handler.ServeHTTP(w, r)
}
func (i *idleFaultInstance) TryAcquire(f bool) (core.RuntimeLease, bool) {
	l, ok := i.ManagedDocsInstance.TryAcquire(f)
	if !ok {
		return nil, false
	}
	return &idleFaultLease{l, i.fault.handler(l, false)}, true
}
func (i *idleFaultInstance) Wake(ctx context.Context, p func(context.Context, http.Handler) error) (core.RuntimeLease, error) {
	l, err := i.ManagedDocsInstance.Wake(ctx, func(ctx context.Context, h http.Handler) error { return p(ctx, i.fault.handler(h, false)) })
	if err != nil {
		return nil, err
	}
	return &idleFaultLease{l, i.fault.handler(l, false)}, nil
}
func (i *idleFaultInstance) TryQuiesce(now time.Time, idle time.Duration) (core.RuntimeQuiescence, bool) {
	q, ok := i.ManagedDocsInstance.TryQuiesce(now, idle)
	if !ok {
		return nil, false
	}
	return &idleFaultQuiescence{q, i.fault.handler(q, true)}, true
}
func installIdleFault(h *idleHost, f *idleHostFault) {
	h.rt.instanceWrap = func(spec core.RuntimeSpec, in core.Instance) core.Instance {
		if spec.ManagedDocs {
			return &idleFaultInstance{in.(core.ManagedDocsInstance), f}
		}
		return in
	}
}

func TestHostIdlePendingHumanGroupRefusesCleanSleep(t *testing.T) {
	h := newIdleHost(t)
	fault := &idleHostFault{}
	installIdleFault(h, fault)
	h.save(t, "pending-doc")
	server := httptest.NewServer(h.net.handler("pending-doc"))
	defer server.Close()
	ws := connect(t, server.URL, "private")
	ws.hello(t, nil)
	edit(t, ws, "pending-human", fixture(t).Independent)
	ws.Close()
	started := time.Now()
	pid := h.rt.pid("pending-doc")
	deadline := time.Now().Add(5 * time.Second)
	for fault.dirtyProof.Load() == 0 && time.Now().Before(deadline) {
		h.offset.Add(int64(3 * time.Minute))
		h.s.TickDocs(t.Context())
		time.Sleep(10 * time.Millisecond)
	}
	if fault.dirtyProof.Load() == 0 {
		t.Fatal("did not observe real non-quiescent sync during idle drain")
	}
	// Confirm refusal beyond asynchronous four-second termination backstop.
	time.Sleep(4500 * time.Millisecond)
	if !alive(pid) || h.rt.controller("pending-doc").CleanDormant() {
		t.Fatal("unsealed real human group acquired clean dormant certificate")
	}
	// A separate unavailable import must prevent blob sweeping, even while live.
	hash := strings.Repeat("a", 64)
	orphan := filepath.Join(h.data, "flats", "pending-doc", "revisions", "blobs", hash[:2], hash)
	if err := os.MkdirAll(filepath.Dir(orphan), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("unreferenced synthetic blob"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	fault.outboxUnavailable.Store(true)
	h.s.SweepDesignBlobs(t.Context())
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("failed import did not protect sweep: %v", err)
	}
	fault.outboxUnavailable.Store(false)
	// Host fake time cannot mature JavaScript groups; wait actual worker time.
	if remaining := 31*time.Second - time.Since(started); remaining > 0 {
		time.Sleep(remaining)
	}
	h.waitClean(t, "pending-doc")
	waitIdleExit(t, pid)
	history, err := h.s.ListRevisions(t.Context(), "pending-doc", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	person := false
	for _, revision := range history.Revisions {
		person = person || revision.Kind == "person"
	}
	if !person {
		t.Fatal("clean drain did not import person revision")
	}
	if !h.rt.controller("pending-doc").CleanDormant() {
		t.Fatal("successful real drain lacks certificate")
	}
	launches := h.rt.spawnCount("pending-doc")
	h.s.SweepDesignBlobs(t.Context())
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clean dormant sweep did not reclaim orphan: %v", err)
	}
	if h.rt.spawnCount("pending-doc") != launches {
		t.Fatal("clean sweep woke document")
	}
	if !strings.Contains(idleDocument(t, server.URL)["markdown"].(string), "Person 한국어 🙂") {
		t.Fatal("pending edit lost")
	}
}

func TestHostIdleWakeFreshMutationsAndQuota(t *testing.T) {
	t.Run("fresh-ws-and-api", func(t *testing.T) {
		h := newIdleHost(t)
		h.save(t, "fresh-doc")
		server := httptest.NewServer(h.net.handler("fresh-doc"))
		defer server.Close()
		h.expire(t, "fresh-doc")
		ws := connect(t, server.URL, "private")
		ws.hello(t, nil)
		edit(t, ws, "new-after-sleep", fixture(t).Independent)
		ws.Close()
		if !strings.Contains(idleDocument(t, server.URL)["markdown"].(string), "Person 한국어 🙂") {
			t.Fatal("fresh WS edit absent")
		}
		_, err := h.s.UpdateDocument(t.Context(), "fresh-doc", "", []core.DocumentEditOp{{Op: "insert", At: "end", Text: "Fresh API edit after sleep."}}, nil, core.ViaMCP)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(idleDocument(t, server.URL)["markdown"].(string), "Fresh API edit after sleep.") {
			t.Fatal("fresh API edit absent")
		}
		events, err := h.st.ListEvents(t.Context(), "fresh-doc", "quota", 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if strings.Contains(e.Message, "budget unavailable") {
				t.Fatal(e.Message)
			}
		}
	})
	for _, first := range []string{"websocket", "api"} {
		t.Run("sleeping-quota-first-"+first, func(t *testing.T) {
			h := newIdleHost(t)
			h.save(t, "quota-doc")
			server := httptest.NewServer(h.net.handler("quota-doc"))
			defer server.Close()
			h.expire(t, "quota-doc")
			launches := h.rt.spawnCount("quota-doc")
			if _, err := h.s.UpdateSettings(t.Context(), map[string]string{core.SetDiskQuotaBytes: "1"}); err != nil {
				t.Fatal(err)
			}
			if h.rt.spawnCount("quota-doc") != launches {
				t.Fatal("quota change woke sleeper")
			}
			if first == "websocket" {
				ws := connect(t, server.URL, "private")
				ws.hello(t, nil)
				ws.send(t, map[string]any{"t": "update", "id": "first-over-quota", "u": fixture(t).Independent})
				ws.rejected(t, "disk_quota")
				ws.Close()
			} else {
				_, err := h.s.UpdateDocument(t.Context(), "quota-doc", "", []core.DocumentEditOp{{Op: "insert", At: "end", Text: "Must not commit."}}, nil, core.ViaMCP)
				if !errors.Is(err, core.ErrDiskQuota) {
					t.Fatalf("first edit did not enforce changed quota: %v", err)
				}
			}
			if idleDocument(t, server.URL)["markdown"] != initialText {
				t.Fatal("quota-refused first mutation changed content")
			}
		})
	}
}

func TestHostIdleUnavailableBudgetIsReadOnly(t *testing.T) {
	h := newIdleHost(t)
	fault := &idleHostFault{}
	installIdleFault(h, fault)
	h.save(t, "budget-doc")
	server := httptest.NewServer(h.net.handler("budget-doc"))
	defer server.Close()
	h.expire(t, "budget-doc")
	fault.budgetUnavailable.Store(true)
	if idleDocument(t, server.URL)["markdown"] != initialText {
		t.Fatal("read unavailable after failed budget install")
	}
	ws := connect(t, server.URL, "private")
	ws.hello(t, nil)
	ws.send(t, map[string]any{"t": "update", "id": "no-budget", "u": fixture(t).Independent})
	ws.closed(t, 1012, "resync")
	ws.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err := h.s.UpdateDocument(ctx, "budget-doc", "", []core.DocumentEditOp{{Op: "insert", At: "end", Text: "Must not commit."}}, nil, core.ViaMCP)
	if err == nil {
		t.Fatal("API edit accepted unavailable budget")
	}
	if idleDocument(t, server.URL)["markdown"] != initialText {
		t.Fatal("budget-unavailable edit mutated text")
	}
	events, err := h.st.ListEvents(t.Context(), "budget-doc", "quota", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		found = found || strings.Contains(event.Message, "budget unavailable")
	}
	if !found {
		t.Fatal("missing budget-unavailable diagnostic")
	}
	beforeBudget := fault.successfulBudgets.Load()
	fault.budgetUnavailable.Store(false)
	recoveryDeadline := time.Now().Add(5 * time.Second)
	for fault.successfulBudgets.Load() == beforeBudget && time.Now().Before(recoveryDeadline) {
		h.s.TickDocs(t.Context())
		time.Sleep(10 * time.Millisecond)
	}
	if fault.successfulBudgets.Load() == beforeBudget {
		t.Fatal("no successful budget refresh during recovery")
	}
	ws = connect(t, server.URL, "private")
	ws.hello(t, nil)
	edit(t, ws, "new-after-budget-recovery", fixture(t).Independent)
	ws.Close()
	if !strings.Contains(idleDocument(t, server.URL)["markdown"].(string), "Person 한국어 🙂") {
		t.Fatal("fresh edit after budget recovery absent")
	}
}

func TestHostIdleWebSocketBeyondBudgetTTL(t *testing.T) {
	if os.Getenv("FLATS_IDLE_REAL_CLOCK") != "1" {
		t.Skip("set FLATS_IDLE_REAL_CLOCK=1 for real websocket beyond budget TTL")
	}
	h := newIdleHost(t)
	h.save(t, "long-ws")
	server := httptest.NewServer(h.net.handler("long-ws"))
	defer server.Close()
	ws := connect(t, server.URL, "private")
	ws.hello(t, nil)
	defer ws.Close()
	pid := h.rt.pid("long-ws")
	launches := h.rt.spawnCount("long-ws")
	// Cross both the two-minute idle interval and initial budget expiry.
	// Maintenance refreshes are real host operations, not a worker clock seam.
	deadline := time.Now().Add(125 * time.Second)
	nextPing := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		h.s.TickDocs(t.Context())
		if !time.Now().Before(nextPing) {
			ws.send(t, map[string]any{"t": "ping"})
			ws.next(t, "pong")
			nextPing = time.Now().Add(20 * time.Second)
		}
	}
	if !alive(pid) || h.rt.pid("long-ws") != pid || h.rt.spawnCount("long-ws") != launches {
		t.Fatal("active long-lived websocket worker changed")
	}
	edit(t, ws, "new-after-initial-budget-ttl", fixture(t).Independent)
	if !strings.Contains(idleDocument(t, server.URL)["markdown"].(string), "Person 한국어 🙂") {
		t.Fatal("budget refresh did not preserve edit availability")
	}
}
