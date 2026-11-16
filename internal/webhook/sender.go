// Package webhook sends the signed HTTP events of the host to the URL that the
// operator stored. It knows nothing of the store, the approvals or the
// console: the host hands it finished payloads, and it signs, queues, delivers
// and retries them in goroutines of its own, so that nothing an operation does
// waits for a receiver.
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"
)

const (
	defaultQueueSize = 256
	defaultWorkers   = 4
	defaultLimit     = 10 * time.Second
)

// defaultWaits are the waits before attempts 2, 3 and 4. The slice is only
// read, so every sender can share it.
var defaultWaits = []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute}

// ErrOff is what Config.Keys returns when the webhook is off, which is when
// either key has no value. The sender drops the event without a log line.
var ErrOff = errors.New("webhook: off")

// Credentials are the two operator keys that configure the webhook.
type Credentials struct {
	URL, Secret string
}

// Config describes a Sender. Only Keys is required.
type Config struct {
	// Keys reads and opens the two keys. The sender calls it once for each
	// event, at the first attempt, and uses those values for every attempt of
	// that event. It returns ErrOff when either key has no value. Any other
	// error drops the event with one log line that does not quote the error.
	// A nil Keys behaves as a Keys that always returns ErrOff.
	Keys func(ctx context.Context) (Credentials, error)
	// Client sends the requests. Nil builds a client that verifies
	// certificates and has no way to skip that. The sender follows no
	// redirect with either client.
	Client *http.Client
	// Now is the clock for the time of an event and for the timestamp of an
	// attempt. Nil means time.Now.
	Now func() time.Time
	// Waits are the waits before attempt 2, 3 and 4, and len(Waits)+1 is the
	// number of attempts. Nil means 5 seconds, 30 seconds and 2 minutes. An
	// empty, non-nil slice means a single attempt.
	Waits []time.Duration
	// AttemptLimit is the time limit of one whole attempt. Zero means 10
	// seconds.
	AttemptLimit time.Duration
	// QueueSize is the number of events that no delivery has started and
	// that the sender keeps. Zero means 256.
	QueueSize int
	// Workers is the number of deliveries at once. Zero means 4.
	Workers int
	// Logf receives the log lines of the sender. A line holds an event ID, a
	// type and a failure class, and never a URL, a secret, a signature or a
	// response body. Nil discards them. The sender never calls it while it
	// holds its lock, but Notify calls it in the goroutine of its caller.
	Logf func(format string, args ...any)
	// Version is written into the User-Agent header as flats/<Version>.
	Version string

	// settled is called by a worker when it has finished with an event:
	// delivered, dropped or cancelled. Tests use it to know that no further
	// attempt follows.
	settled func(id string)
}

const (
	stateIdle = iota
	stateRunning
	stateStopped
)

// event is a payload that is signed per attempt but marshalled only once, so
// that every attempt sends the same bytes.
type event struct {
	id, typ string
	body    []byte
}

// Sender queues events and delivers them to the webhook URL. Create it with
// New, start it with Run, and hand events to it with Notify.
type Sender struct {
	keys      func(context.Context) (Credentials, error)
	client    *http.Client
	now       func() time.Time
	waits     []time.Duration
	limit     time.Duration
	size      int
	workers   int
	log       func(string, ...any)
	userAgent string
	settled   func(string)

	// wake holds at most one token. A worker that finds the queue empty waits
	// on it, and a worker that leaves events behind passes the token on.
	wake chan struct{}

	mu    sync.Mutex // guards state and q, and is never held across a call out
	state int
	q     fifo
}

// New returns a Sender that has not started. Events handed to it before Run
// wait in its queue.
func New(cfg Config) *Sender {
	s := &Sender{
		keys:      cfg.Keys,
		now:       cfg.Now,
		limit:     cfg.AttemptLimit,
		size:      cfg.QueueSize,
		workers:   cfg.Workers,
		log:       cfg.Logf,
		settled:   cfg.settled,
		wake:      make(chan struct{}, 1),
		waits:     defaultWaits,
		userAgent: "flats/" + versionOrDev(cfg.Version),
	}
	if s.now == nil {
		s.now = time.Now
	}
	if cfg.Waits != nil {
		s.waits = slices.Clone(cfg.Waits)
	}
	if s.limit <= 0 {
		s.limit = defaultLimit
	}
	if s.size <= 0 {
		s.size = defaultQueueSize
	}
	if s.workers <= 0 {
		s.workers = defaultWorkers
	}
	if cfg.Client == nil {
		s.client = newDefaultClient()
	} else {
		// A copy, so that the check of redirects does not change a client that
		// the caller shares with other code.
		c := *cfg.Client
		c.CheckRedirect = noRedirect
		s.client = &c
	}
	return s
}

func versionOrDev(v string) string {
	if v == "" {
		return "dev"
	}
	return v
}

func (s *Sender) logf(format string, args ...any) {
	if s.log != nil {
		s.log(format, args...)
	}
}

// Notify hands an event to the sender. It marshals the payload, which is CPU
// work only, appends the bytes to the queue under a short lock and wakes a
// worker. It does no I/O and never waits for a delivery, so any goroutine can
// call it at any time: before Run, while Run delivers, and after Run returned,
// when it drops the event without a log line. When the queue is full it drops
// the oldest event that no delivery has started and logs its ID. A nil Sender
// drops the event too.
func (s *Sender) Notify(p Payload) {
	if s == nil {
		return
	}
	if p.ID == "" {
		p.ID = newID()
	}
	if p.At == "" {
		p.At = formatAt(s.now())
	}
	body, err := json.Marshal(&p)
	if err != nil {
		s.logf("webhook: event %s (%s) dropped: it could not be encoded", p.ID, p.Type)
		return
	}
	if len(body) > maxBody {
		s.logf("webhook: event %s (%s) dropped: the body has %d bytes, over the limit of %d", p.ID, p.Type, len(body), maxBody)
		return
	}
	ev := &event{id: p.ID, typ: p.Type, body: body}

	s.mu.Lock()
	if s.state == stateStopped {
		s.mu.Unlock()
		return
	}
	var dropped *event
	if s.q.len() >= s.size {
		dropped = s.q.pop()
	}
	s.q.push(ev)
	s.mu.Unlock()

	s.signal()
	if dropped != nil {
		s.logf("webhook: event %s (%s) dropped: the queue is full", dropped.id, dropped.typ)
	}
}

// signal wakes one idle worker. It never blocks: when a token is waiting
// already, a worker is about to look at the queue.
func (s *Sender) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// next takes the oldest waiting event, which is what makes deliveries start in
// the order in which the events were made.
func (s *Sender) next() (ev *event, more bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev = s.q.pop()
	return ev, s.q.len() > 0
}

// Run starts the delivery goroutines and blocks until ctx is done. It then
// cancels the attempts in flight, ends the retry waits, drops the events that
// still wait, waits for every goroutine it started and marks the sender
// stopped. Run can be called once: a second call returns at once.
func (s *Sender) Run(ctx context.Context) {
	s.mu.Lock()
	if s.state != stateIdle {
		s.mu.Unlock()
		return
	}
	s.state = stateRunning
	s.mu.Unlock()

	var wg sync.WaitGroup
	for range s.workers {
		wg.Go(func() { s.worker(ctx) })
	}
	wg.Wait()

	s.mu.Lock()
	s.state = stateStopped
	s.q.reset()
	s.mu.Unlock()
	// Idle connections hold goroutines of the transport. The sender is done
	// with them, and a later start of the host builds its own.
	s.client.CloseIdleConnections()
}

// worker delivers one event at a time and keeps its slot during the retry
// waits of that event.
func (s *Sender) worker(ctx context.Context) {
	for ctx.Err() == nil {
		ev, more := s.next()
		if ev == nil {
			select {
			case <-s.wake:
			case <-ctx.Done():
			}
			continue
		}
		if more {
			// The token that woke this worker covered several events.
			s.signal()
		}
		s.deliver(ctx, ev)
		if s.settled != nil {
			s.settled(ev.id)
		}
	}
}

// fifo is a queue of events that grows with its content and never holds more
// than the queue size, so it needs no up-front allocation. Popped slots are
// cleared, and the live part moves back to the start when the array is full.
type fifo struct {
	items []*event
	head  int
}

func (f *fifo) len() int { return len(f.items) - f.head }

func (f *fifo) push(e *event) {
	if f.head > 0 && len(f.items) == cap(f.items) {
		n := copy(f.items, f.items[f.head:])
		clear(f.items[n:])
		f.items = f.items[:n]
		f.head = 0
	}
	f.items = append(f.items, e)
}

func (f *fifo) pop() *event {
	if f.len() == 0 {
		return nil
	}
	e := f.items[f.head]
	f.items[f.head] = nil
	f.head++
	if f.head == len(f.items) {
		f.items = f.items[:0]
		f.head = 0
	}
	return e
}

func (f *fifo) reset() {
	clear(f.items)
	f.items = nil
	f.head = 0
}
