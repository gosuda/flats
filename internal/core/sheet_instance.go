package core

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/sheet"
)

// sheetIdleUnload is how long a sheet stays loaded while nobody uses it
// (technical design, section 3.2).
const sheetIdleUnload = 10 * time.Minute

// sheetInstance is a running sheet in the route table. It loads the
// workbook on demand and unloads it when the sheet is idle, which bounds
// host memory to the sheets in use.
type sheetInstance struct {
	s    *Service
	slug string

	mu      sync.Mutex
	in      *sheet.Instance // nil while unloaded
	users   int             // requests that use in outside the flat lock
	stopped bool
	// stopping is the instance Shutdown stopped. A later caller stops it
	// again, which returns once the first stop has ended and filed the
	// release of its storage (sheet.AwaitReleased), so that no caller goes
	// on to move or remove the sheet's files before.
	stopping *sheet.Instance
	// stopHold lets a test hold Shutdown after it took the instance and
	// before it stops it.
	stopHold func()
	// drained is set while the workbook is unloaded after an idle time: the
	// outbox was empty then, so flats.db names every blob the sheet needs.
	drained bool
}

// loaded returns the instance when the workbook is loaded, and nil otherwise.
func (h *sheetInstance) loaded() *sheet.Instance {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.in
}

// sweepState returns the loaded instance, or nil and whether the unloaded
// sheet can be swept by the references of flats.db alone.
func (h *sheetInstance) sweepState() (in *sheet.Instance, drained bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.in, h.in == nil && h.drained && !h.stopped
}

// open returns the instance, loading the workbook again when it was
// unloaded. The caller holds the flat lock, or uses acquire.
func (h *sheetInstance) open() (*sheet.Instance, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.openLocked()
}

func (h *sheetInstance) openLocked() (*sheet.Instance, error) {
	if h.stopped {
		return nil, fmt.Errorf("%w: sheet %s is not running", ErrUnavailable, h.slug)
	}
	if h.in != nil {
		return h.in, nil
	}
	cursor, err := h.s.importCursor(h.slug)
	if err != nil {
		return nil, err
	}
	in, err := h.s.openSheet(h.slug, cursor)
	if err != nil {
		return nil, fmt.Errorf("%w: open the sheet: %w", ErrUnavailable, err)
	}
	h.in, h.drained = in, false
	// The sheet may hold entries that an earlier import left behind.
	h.s.sheetPending.Store(h.slug, true)
	return in, nil
}

// acquire returns the instance for a request that does not hold the flat
// lock. The sheet stays loaded until release is called.
func (h *sheetInstance) acquire() (*sheet.Instance, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	in, err := h.openLocked()
	if err != nil {
		return nil, nil, err
	}
	h.users++
	return in, func() {
		h.mu.Lock()
		h.users--
		h.mu.Unlock()
	}, nil
}

func (h *sheetInstance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	in, release, err := h.acquire()
	if err != nil {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "the sheet is not available now", http.StatusServiceUnavailable)
		return
	}
	defer release()
	in.ServeHTTP(w, r)
}

func (h *sheetInstance) Stop() { h.Shutdown("") }

// Shutdown stops the sheet for good. code tells open browsers why.
func (h *sheetInstance) Shutdown(code string) {
	h.mu.Lock()
	in := h.in
	if in == nil {
		in = h.stopping
	}
	h.in, h.stopped, h.stopping = nil, true, in
	hold := h.stopHold
	h.mu.Unlock()
	if hold != nil {
		hold()
	}
	if in != nil {
		h.s.closeSheet(in, code)
	}
}

// unloadIdle unloads the workbook when nothing has used the sheet for
// sheetIdleUnload. The caller holds the flat lock, and no request holds the
// instance. The instance checks and stops in one step, so a read that just
// got a cursor keeps it loaded, and it stops only with an empty outbox,
// which is what lets the blob sweep treat the unloaded sheet as drained.
func (h *sheetInstance) unloadIdle() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped || h.in == nil || h.users > 0 {
		return false
	}
	if !h.in.StopIfIdle(sheetIdleUnload) {
		return false
	}
	h.s.sheetStats.retire(h.in)
	h.in, h.drained = nil, true
	return true
}

// discard drops in after it refused a request as stopped, so that the next
// open loads the workbook again. Requests hold the handle while they use
// the instance, so an idle unload never stops it under them; this covers
// an instance that stopped for another reason.
func (h *sheetInstance) discard(in *sheet.Instance) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.in != in {
		return
	}
	h.s.closeSheet(in, "")
	h.in, h.drained = nil, false
}

// unloadIdleSheets unloads the sheets nobody has used for sheetIdleUnload,
// skipping sheets whose lock is taken until the next tick.
func (s *Service) unloadIdleSheets() {
	now := s.now()
	for _, h := range s.sheetHandles() {
		in := h.loaded()
		if in == nil {
			continue
		}
		// The idle time the instance reports is up to one of its ticks
		// old. It only selects candidates: unloadIdle asks the instance.
		if idle := in.Stats().IdleSince; idle.IsZero() || now.Sub(idle) < sheetIdleUnload {
			continue
		}
		s.unloadSheet(h)
	}
}

// unloadSheet unloads the workbook of h if the sheet is idle and its flat
// lock is free, and reports whether it did.
func (s *Service) unloadSheet(h *sheetInstance) bool {
	m, _ := s.locks.LoadOrStore(h.slug, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	if !mu.TryLock() {
		return false
	}
	defer mu.Unlock()
	if s.checkStorageJournal(h.slug) != nil || s.sheetHandle(h.slug) != h {
		return false
	}
	return h.unloadIdle()
}

// awaitSheetStorage waits until no stopped instance of the sheet of
// slugName holds a handle of its sheet.db, or until ctx ends. A rename and
// a delete wait for it before they move the flat's files. An instance that
// stopped while SQLite recovered a log keeps a handle until SQLite
// returns, and a handle can keep a file from moving.
func (s *Service) awaitSheetStorage(ctx context.Context, slugName string) error {
	return sheet.AwaitReleased(ctx, s.sheetDir(slugName))
}
