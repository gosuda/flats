package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
)

// rollbackReplyFault alters only delivery of a successful real restore reply.
// SQLite COMMIT and all preparation/import/ACK work run in the actual worker.
type rollbackReplyFault struct {
	host     *idleHost
	slug     string
	armed    atomic.Bool
	mu       sync.Mutex
	killed   int
	injected bool
	err      error
}

func (f *rollbackReplyFault) handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/_docs/host/revisions/restore" || !f.armed.CompareAndSwap(true, false) {
			next.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			f.mu.Lock()
			f.err = fmt.Errorf("restore did not commit successfully: %d %s", rec.Code, rec.Body.String())
			f.mu.Unlock()
			http.Error(w, "restore injection precondition failed", 503)
			return
		}
		// The response exists only after the worker's restore transaction returned.
		// Kill before exposing any of it to core, reproducing a lost COMMIT reply.
		pid := f.host.rt.pid(f.slug)
		var err error
		if pid <= 0 {
			err = fmt.Errorf("missing exact child PID")
		} else {
			err = syscall.Kill(pid, syscall.SIGKILL)
		}
		deadline := time.Now().Add(10 * time.Second)
		for err == nil && alive(pid) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if err == nil && alive(pid) {
			err = fmt.Errorf("committed worker %d did not exit", pid)
		}
		f.mu.Lock()
		f.killed = pid
		f.injected = err == nil
		f.err = err
		f.mu.Unlock()
		http.Error(w, "injected lost committed restore response", http.StatusServiceUnavailable)
	})
}

type rollbackFaultInstance struct {
	core.ManagedDocsInstance
	fault *rollbackReplyFault
}
type rollbackFaultLease struct {
	core.RuntimeLease
	handler http.Handler
}

func (l *rollbackFaultLease) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.handler.ServeHTTP(w, r)
}
func (i *rollbackFaultInstance) wrap(l core.RuntimeLease) core.RuntimeLease {
	if l == nil {
		return nil
	}
	return &rollbackFaultLease{RuntimeLease: l, handler: i.fault.handler(l)}
}
func (i *rollbackFaultInstance) TryAcquire(f bool) (core.RuntimeLease, bool) {
	l, ok := i.ManagedDocsInstance.TryAcquire(f)
	return i.wrap(l), ok
}
func (i *rollbackFaultInstance) Wake(ctx context.Context, p func(context.Context, http.Handler) error) (core.RuntimeLease, error) {
	l, err := i.ManagedDocsInstance.Wake(ctx, p)
	return i.wrap(l), err
}

func TestHostIdleRealRollbackLostCommitReply(t *testing.T) {
	h := newIdleHost(t)
	const slug = "lost-restore"
	fault := &rollbackReplyFault{host: h, slug: slug}
	h.rt.instanceWrap = func(spec core.RuntimeSpec, in core.Instance) core.Instance {
		if spec.ManagedDocs && spec.Flat == slug {
			return &rollbackFaultInstance{ManagedDocsInstance: in.(core.ManagedDocsInstance), fault: fault}
		}
		return in
	}
	h.save(t, slug)
	target, err := h.st.RevisionFiles(t.Context(), slug, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Preserve document/source shape, so rollback takes the in-place restore path.
	changed := "# Changed after original\n\nThis must be replaced by the rollback.\n"
	_, err = h.s.SaveVersion(t.Context(), slug, []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs"}`)}, {Path: "index.md", Data: []byte(changed)}, {Path: "asset.txt", Data: []byte("persisted asset")}}, core.SaveMeta{Message: "second version"}, core.ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	live, err := h.s.GetDocument(t.Context(), slug, "")
	if err != nil || live.Markdown != changed {
		t.Fatalf("setup live text=%q err=%v", live.Markdown, err)
	}
	before, err := h.s.ListRevisions(t.Context(), slug, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	launches := len(h.rt.launched(slug))
	oldPID := h.rt.pid(slug)
	fault.armed.Store(true)
	started := time.Now()
	n, err := h.s.RollbackRevision(t.Context(), slug, 1, "crash recovery", core.ViaMCP)
	if err != nil {
		t.Fatalf("committed rollback lost: revision=%d error=%v", n, err)
	}
	fault.mu.Lock()
	injected, killed, injectErr := fault.injected, fault.killed, fault.err
	fault.mu.Unlock()
	if !injected || injectErr != nil || killed != oldPID {
		t.Fatalf("injection not exercised: injected=%v killed=%d expected=%d error=%v", injected, killed, oldPID, injectErr)
	}
	if alive(oldPID) || h.rt.pid(slug) == oldPID || len(h.rt.launched(slug)) != launches+1 {
		t.Fatalf("wrong replacement: old=%d launches=%v", oldPID, h.rt.launched(slug))
	}
	if n <= 0 {
		t.Fatal("missing durable rollback receipt")
	}
	revision, err := h.st.Revision(t.Context(), slug, n)
	if err != nil || revision.Kind != "rollback" || revision.RollbackOf != 1 {
		t.Fatalf("receipt=%+v error=%v", revision, err)
	}
	restored, err := h.st.RevisionFiles(t.Context(), slug, n)
	if err != nil || !slices.Equal(target, restored) {
		t.Fatalf("restored hash/size/path mismatch target=%+v restored=%+v error=%v", target, restored, err)
	}
	live, err = h.s.GetDocument(t.Context(), slug, "")
	if err != nil || live.Markdown != initialText {
		t.Fatalf("live rollback text=%q error=%v", live.Markdown, err)
	}
	bytes, _, err := h.s.RevisionFileContent(t.Context(), slug, n, "index.md")
	if err != nil || string(bytes) != initialText {
		t.Fatalf("durable text=%q error=%v", bytes, err)
	}
	holds, err := h.st.Holds(t.Context(), slug)
	if err != nil || len(holds) != 0 {
		t.Fatalf("restore hold leaked: %+v error=%v", holds, err)
	}
	h.s.TickDocs(t.Context())
	after, err := h.s.ListRevisions(t.Context(), slug, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	rollbacks := 0
	for _, r := range after.Revisions {
		if r.Kind == "rollback" {
			rollbacks++
			if r.Number != n {
				t.Errorf("duplicate rollback receipt: %+v", r)
			}
		}
	}
	if rollbacks != 1 || len(after.Revisions) != len(before.Revisions)+1 {
		b, _ := json.Marshal(after)
		t.Fatalf("duplicate or missing history: %s", b)
	}
	t.Logf("real COMMIT reply dropped; child %d -> %d; revision %d; recovery elapsed %s", oldPID, h.rt.pid(slug), n, time.Since(started))
}
