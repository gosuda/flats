package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	// testSecret is the signing secret of the specification's test vector.
	testSecret = "whsec_0123456789abcdef0123456789abcdef"
	// watchdog ends a test that would hang. It is a deadline for a failure and
	// never the way a test waits for something that works.
	watchdog = 30 * time.Second
)

// epoch is the instant of every injected clock. Its nanoseconds are zero, so a
// formatted time shows that the milliseconds are kept as "000".
var epoch = time.Date(2026, 11, 16, 12, 34, 56, 0, time.UTC)

func fixedClock() time.Time { return epoch }

// stepClock is a clock that moves on one second for every reading, so that two
// attempts of one event have two different timestamps.
type stepClock struct{ n atomic.Int64 }

func (c *stepClock) now() time.Time {
	return epoch.Add(time.Duration(c.n.Add(1)-1) * time.Second)
}

// recorder collects the log lines of a sender.
type recorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorder) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	r.mu.Lock()
	r.lines = append(r.lines, line)
	r.mu.Unlock()
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// recv takes one value from ch or fails the test at the watchdog.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(watchdog):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// within runs fn in its own goroutine and fails the test when fn does not
// return. It is how a test shows that a call does not block.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(watchdog):
		t.Fatalf("%s did not return", what)
	}
}

// harness runs a sender in a goroutine and tells the test when the sender has
// finished with an event.
type harness struct {
	t       *testing.T
	sender  *Sender
	log     *recorder
	settled chan string
	cancel  context.CancelFunc
	done    chan struct{}
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	h := &harness{t: t, log: &recorder{}, settled: make(chan string, 1024)}
	if cfg.Logf == nil {
		cfg.Logf = h.log.logf
	}
	cfg.settled = func(id string) { h.settled <- id }
	h.sender = New(cfg)
	return h
}

// start runs the sender until the test ends or ctx is done.
func (h *harness) start(ctx context.Context) {
	h.t.Helper()
	ctx, h.cancel = context.WithCancel(ctx)
	h.done = make(chan struct{})
	go func() {
		defer close(h.done)
		h.sender.Run(ctx)
	}()
	h.t.Cleanup(h.stop)
}

// stop cancels the sender and waits for Run to return.
func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(watchdog):
		h.t.Errorf("Run did not return after its context ended")
	}
}

// wait returns the IDs of the next n events that the sender finished.
func (h *harness) wait(n int) []string {
	h.t.Helper()
	ids := make([]string, 0, n)
	for range n {
		ids = append(ids, recv(h.t, h.settled, "an event to finish"))
	}
	return ids
}

func keysFor(url string) func(context.Context) (Credentials, error) {
	return func(context.Context) (Credentials, error) {
		return Credentials{URL: url, Secret: testSecret}, nil
	}
}

func sampleEvent(id string) Payload {
	return Payload{
		ID:         id,
		Type:       "approval.created",
		At:         "2026-11-16T12:34:56.000Z",
		Flat:       FlatRef{Slug: "notes", ID: "flt_1"},
		Operation:  "publish",
		State:      "pending",
		ApprovalID: "apr_1",
		Via:        "mcp",
		ConsoleURL: "http://127.0.0.1:7777/approvals/apr_1",
	}
}

// refSign is the recipe of the specification written out again, so that a test
// does not use the code under test to compute its own expectation.
func refSign(secret string, unix int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", unix)
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

// seenRequest is what a test receiver saw of one attempt.
type seenRequest struct {
	method string
	header http.Header
	body   []byte
}

// capture is a receiver's record of the attempts it got.
type capture struct {
	mu   sync.Mutex
	reqs []seenRequest
}

// record stores a request and returns its index.
func (c *capture) record(r *http.Request) int {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, seenRequest{r.Method, r.Header.Clone(), body})
	return len(c.reqs) - 1
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

func (c *capture) all() []seenRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]seenRequest(nil), c.reqs...)
}

func (c *capture) signatures() []string {
	var out []string
	for _, r := range c.all() {
		out = append(out, r.header.Get("X-Flats-Signature"))
	}
	return out
}

// countingTransport counts the requests that a client starts, whether or not
// they reach a server.
type countingTransport struct {
	next http.RoundTripper
	n    atomic.Int32
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.next.RoundTrip(r)
}

// newCountingClient returns a client with its own transport and no redirect
// setting, so that a test also shows that the sender sets it.
func newCountingClient(t *testing.T) (*http.Client, *countingTransport) {
	t.Helper()
	tr := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(tr.CloseIdleConnections)
	ct := &countingTransport{next: tr}
	return &http.Client{Transport: ct}, ct
}

func containsAny(line string, needles []string) (string, bool) {
	for _, n := range needles {
		if n != "" && strings.Contains(line, n) {
			return n, true
		}
	}
	return "", false
}

// settableClock is a clock that the test moves by hand between events.
type settableClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *settableClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *settableClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}
