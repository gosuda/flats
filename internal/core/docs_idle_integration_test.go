package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/store"
)

// The fake controls only physical readiness; commit, source ownership, drafts,
// routes, rename and sweep authorization use the real Service and SQLite store.
func TestDocsIdleCommittedFailurePreservesContentAndDormantOperations(t *testing.T) {
	s, _ := newTestService(t)
	s.cfg.DocsApp = testDocsApp()
	var specs []RuntimeSpec
	var workers []*idleTestInstance
	s.cfg.Runtime = lifecycleRuntime(func(spec RuntimeSpec) (Instance, error) {
		specs = append(specs, spec)
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
		if !spec.ManagedDocs {
			return lifecycleInstance{h}, nil
		}
		m := &idleTestInstance{Handler: h}
		m.wake = func(context.Context, func(context.Context, http.Handler) error) (RuntimeLease, error) {
			return nil, errors.New("cannot initialize child")
		}
		workers = append(workers, m)
		return m, nil
	})
	for _, text := range []string{"# first", "# repaired content"} {
		_, err := s.SaveVersion(t.Context(), "notes", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte(text)}}, SaveMeta{}, ViaAPI)
		var de *DeployError
		if !errors.As(err, &de) || !de.Committed || !errors.Is(err, ErrUnavailable) {
			t.Fatalf("expected committed failure: %v", err)
		}
		f, err := s.st.GetFlat(t.Context(), "notes")
		if err != nil || f.LiveVersion < 1 {
			t.Fatalf("live pointer lost: %+v %v", f, err)
		}
		v, err := s.st.GetVersion(t.Context(), "notes", f.LiveVersion)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(s.contentDir("notes", v) + "/index.md")
		if err != nil || string(b) != text {
			t.Fatalf("committed files removed: %q %v", b, err)
		}
		if s.currentDeployment("notes") == nil {
			t.Fatal("logical route removed after commit")
		}
		if workers[len(workers)-1].stopped != 0 {
			t.Fatal("committed child stopped by failed-publish cleanup")
		}
	}
	if workers[0].retired != 1 {
		t.Fatal("old logical instance was not retired on committed replacement")
	}
	if !idleEventContains(t, s, "notes", "save attribution could not be recorded") {
		t.Fatal("missing changed-save attribution warning")
	}
	m := workers[len(workers)-1]
	before := m.wakes
	s.TickDocs(t.Context())
	s.startDocsRevisions(t.Context())
	s.pushAllDocsBudgets(t.Context())
	if m.wakes != before {
		t.Fatal("maintenance woke failed/dormant document")
	}
	if s.prepareDocsSweepLocked(t.Context(), "notes") {
		t.Fatal("unclean dormant source swept")
	}
	m.clean = true
	if !s.prepareDocsSweepLocked(t.Context(), "notes") {
		t.Fatal("clean dormant source cannot be swept")
	}
	m.InvalidateClean()
	if s.prepareDocsSweepLocked(t.Context(), "notes") {
		t.Fatal("invalidated clean certificate still authorizes sweep")
	}
	m.wake = func(context.Context, func(context.Context, http.Handler) error) (RuntimeLease, error) {
		m.lease = &idleTestLease{Handler: m.Handler}
		return m.lease, nil
	}
	rec := httptest.NewRecorder()
	s.docsDemand("notes", s.currentDeployment("notes")).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || m.wakes != before+1 {
		t.Fatalf("committed content did not recover: %d wakes=%d", rec.Code, m.wakes)
	}
	_, err := s.RenameSlug(t.Context(), "notes", "renamed", ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	if s.currentDeployment("notes") != nil || s.currentDeployment("renamed") == nil {
		t.Fatal("rename retained old or lost new route")
	}
	last := specs[len(specs)-1]
	if !last.ManagedDocs || !last.Lazy || workers[len(workers)-1].wakes != 0 {
		t.Fatal("rename eagerly woke unprepared child")
	}
	if _, err := s.applyDelete(t.Context(), "renamed", ViaAPI, ""); err != nil {
		t.Fatal(err)
	}
	if s.currentDeployment("renamed") != nil {
		t.Fatal("delete recreated runtime state")
	}
}

func TestDocsIdleStaleRequestUsesCurrentWrappedHandler(t *testing.T) {
	s, _ := newTestService(t)
	old := &deployed{inst: &idleTestInstance{}}
	next := &deployed{inst: &idleTestInstance{}, handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Wrapper", "asset")
		w.Write([]byte("new asset"))
	})}
	s.state("notes").cur.Store(next)
	rec := httptest.NewRecorder()
	s.docsDemand("notes", old).ServeHTTP(rec, httptest.NewRequest("GET", "/asset.txt", nil))
	if rec.Code != 200 || rec.Header().Get("X-Wrapper") != "asset" || !strings.Contains(rec.Body.String(), "new asset") {
		t.Fatalf("wrapper bypassed: %d %s", rec.Code, rec.Body.String())
	}
	s.state("notes").cur.Store(nil)
	rec = httptest.NewRecorder()
	s.docsDemand("notes", old).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("deleted route response=%d", rec.Code)
	}
}

func TestDocsIdlePreparationRevokesBeforeRecoveryAndBudget(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"fresh budget", "budget unavailable", "import backlog", "revoke fails", "capacity sync", "failed sync"} {
		t.Run(name, func(t *testing.T) {
			s, _ := newTestService(t)
			v := saveDocsDraft(t, s, "notes")
			var ops []string
			var budget map[string]any
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				op := strings.TrimPrefix(r.URL.Path, "/_docs/host/revisions/")
				ops = append(ops, op)
				if (name == "budget unavailable" && op == "budget") || (name == "import backlog" && op == "outbox") || (name == "revoke fails" && op == "budget/revoke") {
					w.WriteHeader(503)
					return
				}
				if op == "sync" && (name == "capacity sync" || name == "failed sync") {
					if name == "capacity sync" {
						w.WriteHeader(422)
					} else {
						w.WriteHeader(503)
					}
					return
				}
				switch op {
				case "sync":
					w.Write([]byte(`{"store":"fake","ready":0,"quiescent":true}`))
				case "outbox":
					w.Write([]byte(`{"store":"fake","entries":[],"more":false}`))
				case "budget":
					if err := json.NewDecoder(r.Body).Decode(&budget); err != nil {
						t.Error(err)
					}
					w.Write([]byte(`{"ok":true}`))
				default:
					w.Write([]byte(`{"ok":true}`))
				}
			})
			m := &idleTestInstance{Handler: h}
			d := &deployed{inst: m, version: v, redact: noRedact}
			// Hold the flat lock, as prepareDocsLocked requires, from before
			// the fake becomes the running document until the assertions are
			// done: the service's own startup and periodic maintenance take
			// it too, and would otherwise acquire this worker.
			unlock := s.lock("notes")
			defer unlock()
			s.state("notes").cur.Store(d)
			err := s.prepareDocsLocked(withDocsDispatch(t.Context(), d, h), "notes", d)
			if name == "revoke fails" {
				if err == nil || len(ops) != 1 {
					t.Fatalf("failed revoke allowed recovery: %v %v", ops, err)
				}
				return
			}
			if name == "failed sync" {
				if err == nil || len(ops) != 3 {
					t.Fatalf("noncapacity sync became ready: %v %v", ops, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if name == "import backlog" && !idleEventContains(t, s, "notes", "document revision import deferred") {
				t.Fatal("import failure missing event")
			}
			if !reflect.DeepEqual(ops[:min(len(ops), 5)], []string{"budget/revoke", "access", "sync", "outbox", "budget"}) {
				t.Fatalf("readiness order: %v", ops)
			}
			if name == "budget unavailable" {
				if len(ops) != 6 || ops[5] != "budget/revoke" {
					t.Fatalf("failed budget left possible stale grant: %v", ops)
				}
			} else if budget["ttl_ms"] != float64(docsBudgetTTL.Milliseconds()) {
				t.Fatalf("fresh budget missing: %#v", budget)
			}
			if m.wakes != 0 || len(m.foreground) != 0 {
				t.Fatal("readiness recursively acquired its own worker")
			}
		})
	}
}

// Lose the restore response after committing an outbox entry, then force a
// fresh child whose preparation imports/ACKs that entry before receipt lookup.
func TestDocsIdleUncertainRollbackReacquiresAndKeepsImportedReceipt(t *testing.T) {
	t.Parallel()
	s, _ := newTestService(t)
	s.cfg.Runtime = &contentRuntime{}
	s.cfg.DocsApp = testDocsApp()
	saveDocs(t, s, "notes")
	d := s.currentDeployment("notes")
	original := d.inst
	defer original.Stop()
	var files []store.RevisionFile
	for _, path := range []string{"a.md", "index.md"} {
		text := []byte("restored " + path)
		hash, err := putBlob(s.docsStoreDir("notes"), text)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, store.RevisionFile{Path: path, Hash: hash, Size: int64(len(text))})
	}
	if err := s.st.ContentTx(t.Context(), "notes", func(tx *store.ContentTx) error {
		if err := tx.InitReadyState(t.Context(), "docs"); err != nil {
			return err
		}
		return tx.InsertRevision(t.Context(), store.Revision{Number: 1, CreatedAt: time.Now(), Kind: "save", Source: d.version.Hash}, files)
	}); err != nil {
		t.Fatal(err)
	}
	m := &idleTestInstance{}
	d.inst = m
	var pending *docsEntry
	oldDead := false
	newReady := false
	acked := false
	opLookups := 0
	oldAfterCrash := 0
	handler := func(old bool) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if old && oldDead {
				oldAfterCrash++
				w.WriteHeader(503)
				return
			}
			op := strings.TrimPrefix(r.URL.Path, "/_docs/host/revisions/")
			switch op {
			case "sync":
				w.Write([]byte(`{"store":"receipt-store","ready":0,"quiescent":true}`))
			case "outbox":
				out := docsOutbox{Store: "receipt-store"}
				if pending != nil {
					out.Entries = []docsEntry{*pending}
				}
				json.NewEncoder(w).Encode(out)
			case "stage":
				var b struct {
					Text string `json:"text"`
				}
				json.NewDecoder(r.Body).Decode(&b)
				json.NewEncoder(w).Encode(map[string]string{"hash": hashBytes([]byte(b.Text))})
			case "restore":
				var b struct {
					ID string `json:"id"`
				}
				json.NewDecoder(r.Body).Decode(&b)
				pending = &docsEntry{Seq: 1, CreatedAt: time.Now().UnixMilli(), Kind: "rollback", RollbackOf: 1, Op: b.ID, Source: d.version.Hash, Documents: files}
				oldDead = true
				m.lease = nil
				w.WriteHeader(503)
			case "ack":
				pending = nil
				acked = true
				w.Write([]byte(`{"ok":true}`))
			case "op":
				opLookups++
				w.Write([]byte(`{"committed":false}`))
			default:
				w.Write([]byte(`{"ok":true}`))
			}
		})
	}
	oldLease := &idleTestLease{Handler: handler(true)}
	m.lease = oldLease
	m.wake = func(ctx context.Context, prepare func(context.Context, http.Handler) error) (RuntimeLease, error) {
		lease := &idleTestLease{Handler: handler(false)}
		if err := prepare(ctx, lease); err != nil {
			return nil, err
		}
		newReady = true
		m.lease = lease
		return lease, nil
	}
	n, err := s.rollbackDocs(t.Context(), "notes", 1, "test", ViaAPI)
	if err != nil || n != 2 {
		t.Fatalf("lost committed rollback: revision=%d error=%v", n, err)
	}
	if !newReady || !acked || pending != nil || opLookups != 0 || oldAfterCrash != 0 || m.wakes != 1 {
		t.Fatalf("recovery: ready=%v ack=%v pending=%v opQueries=%d staleCalls=%d wakes=%d", newReady, acked, pending, opLookups, oldAfterCrash, m.wakes)
	}
	r, err := s.st.Revision(t.Context(), "notes", n)
	if err != nil || r.RollbackOf != 1 || r.Kind != "rollback" {
		t.Fatalf("persisted receipt=%+v %v", r, err)
	}
	gotFiles, err := s.st.RevisionFiles(t.Context(), "notes", n)
	if err != nil || !reflect.DeepEqual(gotFiles, files) {
		t.Fatalf("restored files differ: %+v want %+v: %v", gotFiles, files, err)
	}
	unlock := s.lock("notes")
	if _, err := s.refreshDocsLocked(t.Context(), "notes"); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	if _, err := s.st.Revision(t.Context(), "notes", n+1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("post-recovery refresh duplicated rollback: %v", err)
	}
	holds, err := s.st.Holds(t.Context(), "notes")
	if err != nil || len(holds) != 0 {
		t.Fatalf("rollback hold leaked: %+v %v", holds, err)
	}
}

func idleEventContains(t *testing.T, s *Service, slug, text string) bool {
	t.Helper()
	events, err := s.Events(t.Context(), slug, "", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if strings.Contains(e.Message, text) {
			return true
		}
	}
	return false
}

type idleTestQuiescence struct {
	http.Handler
	suspended []bool
	resumed   int
	resumeErr error
	onResume  func()
}

func (q *idleTestQuiescence) Suspend(clean bool) { q.suspended = append(q.suspended, clean) }
func (q *idleTestQuiescence) Resume() error {
	q.resumed++
	if q.onResume != nil {
		q.onResume()
	}
	return q.resumeErr
}

func TestDocsIdleDrainRefusesUnresolvedState(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"clean", "sync error", "import error", "import more", "not quiescent", "ready entries", "holds", "reopen fails", "resume fails"} {
		t.Run(name, func(t *testing.T) {
			s, _ := newTestService(t)
			v := saveDocsDraft(t, s, "notes")
			var ops []string
			syncs := 0
			q := &idleTestQuiescence{}
			q.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				op := strings.TrimPrefix(r.URL.Path, "/_docs/host/revisions/")
				ops = append(ops, op)
				switch op {
				case "sync":
					syncs++
					if name == "sync error" && syncs == 1 {
						w.WriteHeader(503)
						return
					}
					quiescent := true
					ready := 0
					if name == "not quiescent" || name == "reopen fails" || name == "resume fails" {
						quiescent = false
					}
					if name == "ready entries" {
						ready = 1
					}
					json.NewEncoder(w).Encode(docsSyncResult{Store: "drain", Quiescent: quiescent, Ready: ready})
				case "outbox":
					if name == "import error" {
						w.WriteHeader(503)
						return
					}
					if name == "import more" {
						// A referenced source is absent: import returns more rather than
						// inventing a revision or authorizing source/blob collection.
						json.NewEncoder(w).Encode(docsOutbox{Store: "drain", Entries: []docsEntry{{Seq: 1, Source: "missing"}}})
						return
					}
					json.NewEncoder(w).Encode(docsOutbox{Store: "drain"})
				case "budget/revoke":
					if name == "reopen fails" {
						w.WriteHeader(503)
						return
					}
					w.Write([]byte(`{"ok":true}`))
				default:
					w.Write([]byte(`{"ok":true}`))
				}
			})
			if name == "holds" {
				if err := s.st.ContentTx(t.Context(), "notes", func(tx *store.ContentTx) error {
					if err := tx.InitReadyState(t.Context(), "docs"); err != nil {
						return err
					}
					if err := tx.InsertRevision(t.Context(), store.Revision{Number: 1, CreatedAt: time.Now(), Kind: "save"}, nil); err != nil {
						return err
					}
					return tx.AddHold(t.Context(), "in-progress", 1)
				}); err != nil {
					t.Fatal(err)
				}
				s.docsRollbacks.Store("in-progress", true)
			}
			if name == "resume fails" {
				q.resumeErr = errors.New("child exited")
			}
			q.onResume = func() {
				start := -1
				for i, op := range ops {
					if op == "budget/revoke" {
						start = i
						break
					}
				}
				if start < 0 || len(ops) < start+5 || !reflect.DeepEqual(ops[start:start+5], []string{"budget/revoke", "access", "sync", "outbox", "budget"}) {
					t.Fatalf("resumed without full readiness: %v", ops)
				}
			}
			m := &idleTestInstance{q: q}
			d := &deployed{inst: m, version: v, redact: noRedact}
			s.state("notes").cur.Store(d)
			unlock := s.lock("notes")
			attempted := s.idleDocsLocked(t.Context(), "notes", d)
			unlock()
			if !attempted {
				t.Fatal("drain not attempted")
			}
			switch name {
			case "clean":
				if !reflect.DeepEqual(q.suspended, []bool{true}) || q.resumed != 0 {
					t.Fatalf("clean result: %+v", q)
				}
			case "reopen fails":
				if !reflect.DeepEqual(q.suspended, []bool{false}) || q.resumed != 0 {
					t.Fatalf("failed reopen result: %+v", q)
				}
			case "resume fails":
				if !reflect.DeepEqual(q.suspended, []bool{false}) || q.resumed != 1 {
					t.Fatalf("failed resume result: %+v", q)
				}
			default:
				if len(q.suspended) != 0 || q.resumed != 1 {
					t.Fatalf("unresolved state certified clean: %+v", q)
				}
			}
			if name == "import error" {
				if !idleEventContains(t, s, "notes", "document revision import deferred") {
					t.Fatal("missing import failure event")
				}
				m.lease = &idleTestLease{Handler: q.Handler}
				before := len(ops)
				if s.prepareDocsSweepLocked(t.Context(), "notes") {
					t.Fatal("backlog allowed sweep")
				}
				if len(ops) <= before || ops[before] != "outbox" {
					t.Fatalf("sweep refused without attempting live import: %v", ops[before:])
				}
			}
		})
	}
}

type concurrentIdleInstance struct {
	*idleTestInstance
	mu               sync.Mutex
	ready            RuntimeLease
	count            int
	started, unblock chan struct{}
}

func (m *concurrentIdleInstance) TryAcquire(bool) (RuntimeLease, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ready, m.ready != nil
}
func (m *concurrentIdleInstance) Wake(context.Context, func(context.Context, http.Handler) error) (RuntimeLease, error) {
	m.mu.Lock()
	m.count++
	if m.count == 1 {
		close(m.started)
	}
	m.mu.Unlock()
	<-m.unblock
	// A lease without mutable bookkeeping is sufficient for this acquisition
	// test; exact physical lease ownership is tested by runtime separately.
	l := concurrentIdleLease{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })}
	m.mu.Lock()
	m.ready = l
	m.mu.Unlock()
	return l, nil
}

type concurrentIdleLease struct{ http.Handler }

func (concurrentIdleLease) Release() {}
func TestDocsIdleConcurrentDemandStartsOnce(t *testing.T) {
	s, _ := newTestService(t)
	m := &concurrentIdleInstance{idleTestInstance: &idleTestInstance{}, started: make(chan struct{}), unblock: make(chan struct{})}
	d := &deployed{inst: m, redact: noRedact}
	s.state("notes").cur.Store(d)
	handler := s.docsDemand("notes", d)
	results := make(chan int, 8)
	entered := make(chan struct{}, 8)
	for range 8 {
		go func() {
			entered <- struct{}{}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
			results <- rec.Code
		}()
	}
	for range 8 {
		<-entered
	}
	select {
	case <-m.started:
	case <-time.After(time.Second):
		t.Fatal("first demand never entered wake")
	}
	select {
	case code := <-results:
		t.Fatalf("request escaped blocked readiness: %d", code)
	case <-time.After(30 * time.Millisecond):
	}
	close(m.unblock)
	for range 8 {
		select {
		case code := <-results:
			if code != 204 {
				t.Fatalf("response %d", code)
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent request stranded")
		}
	}
	m.mu.Lock()
	count := m.count
	m.mu.Unlock()
	if count != 1 {
		t.Fatalf("shared demand started %d times", count)
	}
}

func TestDocsIdleBackoffRefusalsDoNotFloodEvents(t *testing.T) {
	s, _ := newTestService(t)
	if _, err := s.CreateFlat(t.Context(), "notes", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	m := &idleTestInstance{wake: func(context.Context, func(context.Context, http.Handler) error) (RuntimeLease, error) {
		return nil, errors.New("document worker retry in 4s")
	}}
	d := &deployed{inst: m, redact: noRedact}
	before, err := s.Events(t.Context(), "notes", "", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		_, release, err := s.acquireDocsLocked(t.Context(), "notes", d)
		release()
		if !errors.Is(err, ErrUnavailable) {
			t.Fatal(err)
		}
	}
	after, err := s.Events(t.Context(), "notes", "", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("backoff requests flooded event log: %d -> %d", len(before), len(after))
	}
}
