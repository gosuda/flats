package docs

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
	flatsruntime "github.com/gosuda/flats/internal/runtime"
)

// The suite's image mode reaches real document workers: by default and with
// FLATS_FORCE_IMAGES=1 the first worker saves the document image and a
// woken worker restores it; with FLATS_NO_IMAGES=1 no image is created.
func TestHostIdleImageModes(t *testing.T) {
	h := newIdleHost(t)
	var restored atomic.Int32
	h.rt.onLog = func(flat, level, msg string) {
		if flat == "image-doc" && msg == "document runtime restored from image" {
			restored.Add(1)
		}
	}
	h.save(t, "image-doc")
	srv := httptest.NewServer(h.net.handler("image-doc"))
	defer srv.Close()
	idleGET(t, srv.URL+"/")
	images := func() []string {
		t.Helper()
		m, err := filepath.Glob(filepath.Join(h.data, "runtime", "docs", "*.state", "image"))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	disabled := os.Getenv("FLATS_NO_IMAGES") == "1"
	if !disabled {
		// Written right after the worker reported ready.
		deadline := time.Now().Add(10 * time.Second)
		for len(images()) == 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if len(images()) != 1 {
			t.Fatalf("document images after the first worker: %v", images())
		}
	}
	if restored.Load() != 0 {
		t.Fatal("the first worker restored an image")
	}
	h.expire(t, "image-doc")
	idleGET(t, srv.URL+"/")
	if got := idleDocument(t, srv.URL)["markdown"]; got != initialText {
		t.Fatalf("woken document = %q", got)
	}
	if disabled {
		if restored.Load() != 0 || len(images()) != 0 {
			t.Fatalf("images disabled, yet restored=%d images=%v", restored.Load(), images())
		}
		return
	}
	if restored.Load() != 1 {
		t.Fatalf("woken worker restored the image %d times, want 1", restored.Load())
	}
}

// wakePhases are the times of one Wake of a managed document: its call,
// the start and end of core's preparation, and its return, and the host
// calls core's preparation made into the worker.
type wakePhases struct {
	wake, prepare, prepared, woken time.Time
	calls                          []hostCall
}

// hostCall is one request core's wake preparation sent to the worker.
type hostCall struct {
	name string // method and operation, such as "POST budget/revoke"
	took time.Duration
}

// wakeTimer records the phases of the first Wake after arm.
type wakeTimer struct {
	mu    sync.Mutex
	armed bool
	p     wakePhases
}

func (w *wakeTimer) arm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.armed, w.p = true, wakePhases{}
}

func (w *wakeTimer) phases() wakePhases {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.p
}

// call records a host call of the armed wake's preparation.
func (w *wakeTimer) call(c hostCall) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.armed {
		w.p.calls = append(w.p.calls, c)
	}
}

// mark records now as *at unless it was recorded since arm.
func (w *wakeTimer) mark(at *time.Time) {
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.armed && at.IsZero() {
		*at = now
	}
}

type timedDocsInstance struct {
	core.ManagedDocsInstance
	timer *wakeTimer
}

func (i *timedDocsInstance) Wake(ctx context.Context, prepare func(context.Context, http.Handler) error) (core.RuntimeLease, error) {
	w := i.timer
	w.mark(&w.p.wake)
	lease, err := i.ManagedDocsInstance.Wake(ctx, func(ctx context.Context, h http.Handler) error {
		w.mark(&w.p.prepare)
		err := prepare(ctx, timedHandler{h, w})
		w.mark(&w.p.prepared)
		return err
	})
	w.mark(&w.p.woken)
	return lease, err
}

// timedHandler times the host calls of a wake's preparation.
type timedHandler struct {
	http.Handler
	timer *wakeTimer
}

func (h timedHandler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	start := time.Now()
	h.Handler.ServeHTTP(rw, r)
	h.timer.call(hostCall{r.Method + " " + strings.TrimPrefix(r.URL.Path, "/_docs/host/revisions/"), time.Since(start)})
}

// loadAverage is the host's load average (for timing reports).
func loadAverage() string {
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/loadavg")
		if err != nil {
			return err.Error()
		}
		return strings.TrimSpace(string(b))
	}
	b, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return err.Error()
	}
	return strings.TrimSpace(string(b))
}

func waitSpareReady(t *testing.T, m *flatsruntime.Manager) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !m.SpareReady() {
		if time.Now().After(deadline) {
			t.Fatal("the pre-started worker did not become ready within 10s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// percentiles are nearest-rank p50, p95 and the maximum.
func percentiles(samples []time.Duration) (p50, p95, maximum time.Duration) {
	s := slices.Clone(samples)
	slices.Sort(s)
	rank := func(p int) time.Duration { return s[(p*len(s)+99)/100-1] }
	return rank(50), rank(95), s[len(s)-1]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// Requirement 2 of docs/spec/worker-cold-start.md: with the pre-started
// worker ready, a sleeping document's first page and a collaboration hello
// right after it each take under 50 ms (p95 over 20 wakes).
func TestHostIdleWakeBudget(t *testing.T) {
	if os.Getenv("FLATS_COLDSTART_BUDGET") != "1" {
		t.Skip("set FLATS_COLDSTART_BUDGET=1 for the 20-wake cold start budget")
	}
	t.Logf("COLDSTART_LOAD %s %s", runtime.GOOS, loadAverage())
	h := newIdleHost(t)
	timer := &wakeTimer{}
	h.rt.instanceWrap = func(spec core.RuntimeSpec, in core.Instance) core.Instance {
		if spec.ManagedDocs {
			return &timedDocsInstance{in.(core.ManagedDocsInstance), timer}
		}
		return in
	}
	const slug = "budget-doc"
	var restored atomic.Int32
	h.rt.onLog = func(flat, level, msg string) {
		if flat == slug && msg == "document runtime restored from image" {
			restored.Add(1)
		}
	}
	h.save(t, slug)
	srv := httptest.NewServer(h.net.handler(slug))
	defer srv.Close()
	idleGET(t, srv.URL+"/") // the first worker also saves the document image
	var pages, hellos []time.Duration
	calls := map[string][]time.Duration{} // per host call: every wake's time
	var callOrder []string
	for i := 0; i < 20; i++ {
		h.expire(t, slug)
		waitSpareReady(t, h.rt.manager)
		spare := h.rt.manager.SparePID()
		timer.arm()
		before := restored.Load()
		pageStart := time.Now()
		idleGET(t, srv.URL+"/")
		pageEnd := time.Now()
		helloStart := time.Now()
		ws := connect(t, srv.URL, "private")
		ws.hello(t, nil)
		hello := time.Since(helloStart)
		ws.Close()
		page := pageEnd.Sub(pageStart)
		if got := h.rt.pid(slug); got != spare {
			t.Fatalf("wake %d ran in process %d, not the ready pre-started worker %d", i+1, got, spare)
		}
		w := timer.phases()
		if w.wake.IsZero() || w.prepare.IsZero() || w.prepared.IsZero() || w.woken.IsZero() {
			t.Fatalf("wake %d phases not observed: %+v", i+1, w)
		}
		h.rt.mu.Lock()
		generation := h.rt.generation[slug]
		h.rt.mu.Unlock()
		pages, hellos = append(pages, page), append(hellos, hello)
		var perCall []string
		for j, c := range w.calls {
			// The n-th call of a name in this wake, so a repeated call
			// (a second revoke) is reported apart from the first.
			name := c.name
			for _, prev := range w.calls[:j] {
				if prev.name == c.name {
					name += "'"
				}
			}
			if calls[name] == nil {
				callOrder = append(callOrder, name)
			}
			calls[name] = append(calls[name], c.took)
			perCall = append(perCall, fmt.Sprintf("%s=%.2f", strings.ReplaceAll(name, " ", "_"), ms(c.took)))
		}
		// core_before_wake: request arrival to Wake (routing, storage lock);
		// spawn: Wake to preparation minus NextGeneration (adoption, worker
		// init, ready report); core_prepare: core's preparation requests;
		// serve: Wake's return to the complete page.
		t.Logf("COLDSTART_WAKE sample=%d image_restored=%v page_ms=%.2f hello_ms=%.2f core_before_wake_ms=%.2f next_generation_ms=%.2f spawn_ms=%.2f core_prepare_ms=%.2f serve_ms=%.2f",
			i+1, restored.Load() > before, ms(page), ms(hello), ms(w.wake.Sub(pageStart)), ms(generation),
			ms(w.prepare.Sub(w.wake)-generation), ms(w.prepared.Sub(w.prepare)), ms(pageEnd.Sub(w.woken)))
		t.Logf("COLDSTART_CALLS sample=%d %s", i+1, strings.Join(perCall, " "))
	}
	for _, name := range callOrder {
		c50, c95, cmax := percentiles(calls[name])
		t.Logf("COLDSTART_CALL %q n=%d p50_ms=%.2f p95_ms=%.2f max_ms=%.2f", name, len(calls[name]), ms(c50), ms(c95), ms(cmax))
	}
	p50, p95, maximum := percentiles(pages)
	t.Logf("COLDSTART_PAGE p50_ms=%.2f p95_ms=%.2f max_ms=%.2f", ms(p50), ms(p95), ms(maximum))
	h50, h95, hmax := percentiles(hellos)
	t.Logf("COLDSTART_HELLO p50_ms=%.2f p95_ms=%.2f max_ms=%.2f", ms(h50), ms(h95), ms(hmax))
	if p95 >= 50*time.Millisecond {
		t.Errorf("first page after a wake: p95 %v, budget 50ms", p95)
	}
	if h95 >= 50*time.Millisecond {
		t.Errorf("collaboration hello after a wake: p95 %v, budget 50ms", h95)
	}
}
