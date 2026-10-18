package core

import (
	"bufio"
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"time"
)

// usageWriter counts one request of a flat's live route: its status class,
// response bytes and response time (docs/internal/metrics.md, Request
// accounting). It is used by one handler goroutine, like the ResponseWriter
// it wraps.
type usageWriter struct {
	w     http.ResponseWriter
	u     *flatUsage
	route int
	start time.Time
	// orig is the request's context when it reached Flats, before the public
	// route replaced it with one that its revocation ends.
	orig context.Context

	status    int    // first status sent; 0 while none
	day       string // UTC day the status was sent
	counted   bool
	streaming bool
	// goneAtSend: the visitor had already left (orig cancelled) when the
	// 502 or 503 status was sent.
	goneAtSend bool
}

func newUsageWriter(w http.ResponseWriter, r *http.Request, u *flatUsage, route int) *usageWriter {
	return &usageWriter{w: w, u: u, route: route, start: time.Now(), orig: r.Context()}
}

func (uw *usageWriter) Header() http.Header { return uw.w.Header() }

// Unwrap lets http.ResponseController reach the underlying writer for
// deadlines and the other optional methods.
func (uw *usageWriter) Unwrap() http.ResponseWriter { return uw.w }

func (uw *usageWriter) WriteHeader(code int) {
	// Informational responses other than 101 do not end the header phase.
	if uw.status == 0 && (code < 100 || code > 199 || code == http.StatusSwitchingProtocols) {
		uw.sent(code, false)
	}
	uw.w.WriteHeader(code)
}

func (uw *usageWriter) Write(p []byte) (int, error) {
	if uw.status == 0 {
		uw.sent(http.StatusOK, false)
	}
	n, err := uw.w.Write(p)
	uw.u.responseBytes.Add(uint64(max(n, 0)))
	return n, err
}

// ReadFrom keeps the underlying writer's optimized copy (sendfile for static
// files) while counting the bytes.
func (uw *usageWriter) ReadFrom(r io.Reader) (int64, error) {
	if uw.status == 0 {
		uw.sent(http.StatusOK, false)
	}
	var n int64
	var err error
	if rf, ok := uw.w.(io.ReaderFrom); ok {
		n, err = rf.ReadFrom(r)
	} else {
		n, err = io.Copy(writerOnly{uw.w}, r)
	}
	uw.u.responseBytes.Add(uint64(max(n, 0)))
	return n, err
}

type writerOnly struct{ io.Writer }

// Flush implements http.Flusher, which the design handler asserts for its
// event stream. Every writer Flats serves through supports flushing.
func (uw *usageWriter) Flush() { _ = uw.FlushError() }

func (uw *usageWriter) FlushError() error {
	if uw.status == 0 {
		uw.sent(http.StatusOK, false)
	}
	return http.NewResponseController(uw.w).Flush()
}

// Hijack takes over the connection for an upgrade (WebSocket); a successful
// hijack is the 101 the handler then writes itself.
func (uw *usageWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, brw, err := http.NewResponseController(uw.w).Hijack()
	if err == nil {
		if uw.status == 0 {
			uw.sent(http.StatusSwitchingProtocols, true)
		} else if !uw.streaming {
			uw.streaming = true
			uw.u.openStreams.Add(1)
		}
	}
	return c, brw, err
}

func isEventStream(h http.Header) bool {
	mt, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	return err == nil && mt == "text/event-stream"
}

// sent records the first status. Its class and day are taken now, so a
// WebSocket open for days counts on the day it opened. A 502 or 503 is
// counted when the handler returns: it is closed if the visitor had already
// left when it was sent, which is recorded now, since a visitor who leaves
// later, while it is being sent, does not make it closed.
func (uw *usageWriter) sent(code int, hijacked bool) {
	uw.status = code
	uw.day = uw.u.today()
	if hijacked || isEventStream(uw.w.Header()) {
		uw.streaming = true
		uw.u.openStreams.Add(1)
	}
	if code != http.StatusBadGateway && code != http.StatusServiceUnavailable {
		uw.u.countRequest(uw.route, statusClass(code), uw.day)
		uw.counted = true
		return
	}
	uw.goneAtSend = errors.Is(uw.orig.Err(), context.Canceled)
}

// finish counts the request when its handler returned, or panicked with
// value (nil for a normal return).
func (uw *usageWriter) finish(panicked bool, value any) {
	if uw.streaming {
		uw.u.openStreams.Add(-1)
	}
	if uw.counted {
		if !uw.streaming {
			uw.u.observe(time.Since(uw.start), uw.day)
		}
		return
	}
	abort := panicked && value == http.ErrAbortHandler
	status := uw.status
	day := uw.day
	if day == "" {
		day = uw.u.today()
	}
	// A visitor who left before any answer, or before the 502/503 Flats gives
	// a departed visitor was sent, is closed.
	gone := uw.goneAtSend
	if status == 0 {
		gone = errors.Is(uw.orig.Err(), context.Canceled)
	}
	if (!panicked || abort) && gone {
		uw.u.countRequest(uw.route, classClosed, day)
		return
	}
	switch {
	case status == 0 && panicked:
		status = http.StatusInternalServerError
	case status == 0:
		status = http.StatusOK // returned without writing: an implicit 200
	}
	uw.u.countRequest(uw.route, statusClass(status), day)
	if !uw.streaming {
		uw.u.observe(time.Since(uw.start), day)
	}
}

// serveCounted serves r through h with usage accounting. Panics are counted
// and re-raised for net/http to handle.
func serveCounted(u *flatUsage, route int, w http.ResponseWriter, r *http.Request, h func(http.ResponseWriter, *http.Request)) {
	if u == nil {
		h(w, r)
		return
	}
	uw := newUsageWriter(w, r, u, route)
	panicked := true
	defer func() {
		if !panicked {
			uw.finish(false, nil)
			return
		}
		value := recover()
		if value == nil { // runtime.Goexit: let it continue
			uw.finish(false, nil)
			return
		}
		uw.finish(true, value)
		panic(value)
	}()
	h(uw, r)
	panicked = false
}
