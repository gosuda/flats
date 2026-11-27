package sheet

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/wsconn"
)

// escapedTab writes cells cells of an apostrophe, an equals sign and
// 30,000 U+0001 into column A of Sheet1. A frame carries each as its value
// and as the text to type, six bytes a character as JSON: 360 KB a cell.
func escapedTab(e *env, cells int) (frames int) {
	const chars = 30000
	text := strings.Repeat(`\u0001`, chars)
	for row := 1; row <= cells; row += 25 {
		list := make([]string, min(25, cells-row+1))
		for i := range list {
			list[i] = `["'=` + text + `"]`
		}
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, row, strings.Join(list, ",")))
	}
	return 2 * 6 * cells * chars
}

var partHeader = regexp.MustCompile(`^\{"type":"delta_part","seq":"(\d+)","part":(\d+),"parts":(\d+)`)

// fanReader is a real connection that watches a tab. Once its snapshot is
// in it reads the deltas, and its reader can stop it after given numbers
// of bytes of delta_part frames.
type fanReader struct {
	route string
	ws    *wsconn.Conn
	kinds []string // the frames after the snapshot, by type
	parts []string // the delta_part frames, whole for the first reader
	bytes int      // of delta_part frames
}

func (s *site) fanReader(t *testing.T, route string, tab TabID) *fanReader {
	t.Helper()
	host := strings.TrimPrefix(s.server.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, err := wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) { return new(net.Dialer).DialContext(ctx, "tcp", host) }, host, "/"+route+"/_sheet/ws")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close(1000, "") })
	hello := helloV1
	if route == "private" {
		hello = `{"type":"hello","version":1,"name":"Ann","pending":0}`
	}
	for _, frame := range []string{hello, fmt.Sprintf(`{"type":"watch","generation":1,"tab":%d}`, tab)} {
		if err := ws.Send(frame); err != nil {
			t.Fatal(err)
		}
	}
	for {
		raw, err := ws.ReadMessage(64 << 20)
		if err != nil {
			t.Fatalf("a %s connection before its snapshot: %v", route, err)
		}
		if strings.HasPrefix(raw, `{"type":"tab_done"`) {
			return &fanReader{route: route, ws: ws}
		}
	}
}

// read takes frames until the last part of the delta seq, or, when it
// resyncs, until the snapshot after the resync is in. read is called with
// the bytes of parts read so far.
func (r *fanReader) read(seq uint64, keep bool, read func(int)) error {
	want := strconv.FormatUint(seq, 10)
	resynced := false
	next := 0
	for {
		raw, err := r.ws.ReadMessage(64 << 20)
		if err != nil {
			return fmt.Errorf("after %d bytes of parts: %w", r.bytes, err)
		}
		if len(raw) > serverFrameMax {
			return fmt.Errorf("a frame of %d bytes", len(raw))
		}
		var kind struct{ Type string }
		head := raw[:min(len(raw), 64)]
		if m := partHeader.FindStringSubmatch(head); m != nil {
			kind.Type = "delta_part"
		} else if err := json.Unmarshal([]byte(raw), &kind); err != nil {
			return err
		}
		r.kinds = append(r.kinds, kind.Type)
		switch kind.Type {
		case "delta_part":
			m := partHeader.FindStringSubmatch(head)
			part, _ := strconv.Atoi(m[2])
			parts, _ := strconv.Atoi(m[3])
			if resynced || m[1] != want || part != next {
				return fmt.Errorf("part %d of %d for %s, want part %d of %s", part, parts, m[1], next, want)
			}
			next++
			r.bytes += len(raw)
			if keep {
				r.parts = append(r.parts, raw)
			}
			if next == parts {
				return nil
			}
			read(r.bytes)
		case "delta":
			return fmt.Errorf("a whole delta: %.80s", raw)
		case "resync":
			resynced = true
		case "tab_done":
			if resynced {
				return nil
			}
		}
	}
}

// deltaEntry returns the entry of seq in the delta log of h.
func deltaEntry(t *testing.T, h *hub, seq uint64) *deltaRec {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, rec := range h.log {
		if rec.seq == seq {
			return rec
		}
	}
	t.Fatalf("the log has no entry %d", seq)
	return nil
}

// senders returns how many connections still send rec. A connection holds
// it from the moment it starts to send it until it has queued its last
// part.
func senders(h *hub, rec *deltaRec) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return rec.holders
}

// TestLargeDeltaIsSharedByItsSenders formats a tab whose cells take six
// times their size as JSON, so its delta goes out in many parts, while
// eight connections watch it, Public and Private. The readers stop
// together at three points of the delta while the host is measured. Every
// sender still holds the delta's entry when a measurement ends, so a sender
// that kept a copy of the delta of its own would still hold it then. The
// host holds the entry once, the send queues and a part for each
// connection. Each reader gets every part once it reads again.
func TestLargeDeltaIsSharedByItsSenders(t *testing.T) {
	cells := 100
	if raceEnabled {
		cells = 20
	}
	e := start(t)
	frames := escapedTab(e, cells)
	s := serve(e)
	tab := e.tabID("Sheet1")
	routes := []string{"public", "private", "public", "private", "public", "private", "public", "private"}
	readers := make([]*fanReader, len(routes))
	queues := 0
	for i, route := range routes {
		readers[i] = s.fanReader(t, route, tab)
		queues += publicQueue
		if route == "private" {
			queues += privateQueue - publicQueue
		}
	}
	// A small edit drops the snapshot cache, so the heap's start holds
	// none of it.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[["x"]]}]}`)
	for _, r := range readers {
		for {
			raw, err := r.ws.ReadMessage(64 << 20)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(raw, `{"type":"delta"`) {
				break
			}
		}
	}
	// measure formats the tab, bold or not, and measures the host while
	// the readers are stopped in its delta. It reports false when the
	// attempt could not be measured.
	measure := func(bold bool) bool {
		for _, r := range readers {
			r.kinds, r.parts, r.bytes = nil, nil, 0
		}
		heldBefore := deltaHeld.Load()
		var base int64
		if measures {
			base = quietHeap(t, 1<<20)
		}
		// The last point leaves half of the delta unread, more than a send
		// queue and the sockets between a sender and its reader take, so
		// every sender still has parts to queue.
		stops := newPauses(len(readers), serverFrameMax, frames/4, frames/2)
		defer stops.giveUp()
		seq := e.seq() + 1
		var wg sync.WaitGroup
		for i, r := range readers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				read, done := stops.reader()
				defer done()
				if err := r.read(seq, i == 0, read); err != nil {
					t.Errorf("a %s reader: %v", r.route, err)
				}
			}()
		}
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"format","tab":"Sheet1","ranges":["A1:A%d"],"format":{"bold":%t}}]}`, cells, bold))
		rec := deltaEntry(t, e.in.hub, seq)
		source, _ := logRetained(e.in.hub)
		// What may be held: the entry, a send queue and two parts for every
		// connection (the one it fills and the one its reader holds), and
		// some slack for the test itself. The first reader also keeps the
		// parts it has read, to join them at the end.
		allowed := int64(source + queues + len(readers)*2*serverFrameMax + 16<<20)
		if int64(len(readers)*frames) < 2*allowed && measures {
			t.Fatalf("the test allows %d bytes, too close to the %d of a copy of the delta for every connection", allowed, len(readers)*frames)
		}
		measured := true
		for i, at := range stops.at {
			if !stops.wait(i) {
				t.Logf("the readers did not all stop after %d bytes in time; the attempt is given up", at)
				measured = false
				break
			}
			if measures {
				reading, why := stops.heapWhileStopped(i, func() (bool, string) {
					if n := senders(e.in.hub, rec); n != len(readers) {
						return false, fmt.Sprintf("only %d of %d connections still sent the delta when the heap was read", n, len(readers))
					}
					return true, ""
				})
				if why != "" {
					t.Logf("after %d bytes %s; the attempt is given up", at, why)
					measured = false
					break
				}
				held := reading - base - int64(readers[0].bytes)
				t.Logf("with every reader stopped after %d bytes and every sender holding the delta the heap holds %d bytes over its start and the kept parts; the entry has %d", at, held, source)
				if held > allowed {
					t.Errorf("with every reader stopped after %d bytes and every sender holding the delta the heap holds %d bytes over its start and the kept parts, more than the %d allowed; the entry has %d, %d connections get %d bytes of parts each", at, held, allowed, source, len(readers), frames)
				}
			}
			stops.release(i)
		}
		wg.Wait()
		for _, r := range readers {
			if r.bytes < frames {
				t.Errorf("a %s reader got %d bytes of parts, want more than %d", r.route, r.bytes, frames)
			}
		}
		if got := deltaHeld.Load() - heldBefore; got != 0 {
			t.Errorf("entries still in the log are charged %d bytes as held", got)
		}
		return measured
	}
	for attempt := 1; !measure(attempt%2 == 1); attempt++ {
		if attempt == pauseAttempts {
			t.Fatalf("none of %d attempts could be measured", attempt)
		}
	}
	var parts []map[string]any
	for _, raw := range readers[0].parts {
		var p map[string]any
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, p)
	}
	d, err := joinDeltaParts(parts)
	if err != nil {
		t.Fatal(err)
	}
	value := "=" + strings.Repeat("\x01", 30000)
	got := 0
	for _, row := range asList(d["cells"]) {
		for _, c := range row.([]any)[1].([]any) {
			if c.([]any)[2] != value || c.([]any)[4] != "'"+value {
				t.Fatalf("a cell of the delta holds %.40q", c)
			}
			got++
		}
	}
	if got != cells {
		t.Fatalf("the delta holds %d cells, want %d", got, cells)
	}
}

// TestHeldDeltaIsChargedAfterTheLog stops four readers inside a large
// delta, Public and Private, and makes the next edit push that delta out
// of the log. Its senders still hold it, so it is charged to the host-wide
// count of held entries until the last of them lets go. When the charge
// fits, the readers get the rest of the delta and the next one; when it
// does not, the senders let go of the entry while their readers are still
// stopped, and each reader gets a resync and a new snapshot.
func TestHeldDeltaIsChargedAfterTheLog(t *testing.T) {
	cells := 150
	if raceEnabled {
		cells = 50
	}
	for _, fits := range []bool{true, false} {
		name := "the charge fits"
		if !fits {
			name = "the charge does not fit"
		}
		t.Run(name, func(t *testing.T) {
			e := start(t)
			frames := escapedTab(e, cells)
			s := serve(e)
			tab := e.tabID("Sheet1")
			// A connection is cut off once for lagging and closed the second
			// time, so an attempt after one that was given up has readers of
			// its own.
			var readers []*fanReader
			dial := func() {
				for _, r := range readers {
					r.ws.Close(1000, "")
				}
				readers = nil
				for _, route := range []string{"public", "private", "public", "private"} {
					readers = append(readers, s.fanReader(t, route, tab))
				}
			}
			dial()
			if !fits {
				old := deltaHeldBudget.Load()
				deltaHeldBudget.Store(1 << 20)
				t.Cleanup(func() { deltaHeldBudget.Store(old) })
			}
			// measure formats the tab, bold or not, and pushes its delta
			// out of the log while the readers are stopped in it. It
			// reports false when the attempt could not be measured.
			idle := deltaHeld.Load()
			measure := func(attempt int) bool {
				if attempt > 1 {
					// The closed connections let go of what they held.
					dial()
					for start := time.Now(); deltaHeld.Load() != idle; time.Sleep(time.Millisecond) {
						if time.Since(start) > time.Minute {
							t.Fatalf("closed connections still hold %d bytes of entries", deltaHeld.Load()-idle)
						}
					}
				}
				before := deltaHeld.Load()
				var base int64
				if measures {
					base = quietHeap(t, 1<<20)
				}
				stops := newPauses(len(readers), serverFrameMax)
				defer stops.giveUp()
				seq := e.seq() + 1
				var wg sync.WaitGroup
				for _, r := range readers {
					wg.Add(1)
					go func() {
						defer wg.Done()
						read, done := stops.reader()
						defer done()
						if err := r.read(seq, false, read); err != nil {
							t.Errorf("a %s reader: %v", r.route, err)
						}
					}()
				}
				e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"format","tab":"Sheet1","ranges":["A1:A%d"],"format":{"bold":%t}}]}`, cells, attempt%2 == 1))
				rec := deltaEntry(t, e.in.hub, seq)
				source, _ := logRetained(e.in.hub)
				if source <= deltaLogBytes {
					t.Fatalf("the delta has %d bytes; the test needs more than the log keeps", source)
				}
				if !stops.wait(0) {
					t.Log("the readers did not all stop in time; the attempt is given up")
					wg.Wait()
					return false
				}
				// What is read while the readers are stopped counts only
				// when the stall has not ended since, because its end
				// resumes them.
				expired := func(what string) bool {
					if !stops.expired.Load() {
						return false
					}
					t.Logf("the readers' stall ended before %s; the attempt is given up", what)
					wg.Wait()
					return true
				}
				// The readers are stopped with most of the delta unread, so
				// each sender holds the entry until it is resumed or the
				// entry is revoked.
				if n := senders(e.in.hub, rec); expired("the senders were counted") {
					return false
				} else if n != len(readers) {
					t.Fatalf("only %d of %d connections still send the delta while their readers are stopped", n, len(readers))
				}
				e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[["next %d"]]}]}`, attempt))
				if source, _ := logRetained(e.in.hub); source > 4096 {
					t.Fatalf("after the next edit the log holds %d bytes", source)
				}
				if fits {
					charged := deltaHeld.Load() - before
					if n := senders(e.in.hub, rec); expired("the charge was read") {
						return false
					} else if n != len(readers) {
						t.Fatalf("only %d of %d connections still sent the delta when its charge was read", n, len(readers))
					}
					if charged < int64(frames) {
						t.Fatalf("the held entry is charged %d bytes while its senders hold it, want its %d", charged, frames)
					}
				} else {
					// The senders stop while their readers do not read.
					for deltaHeld.Load() != before {
						if expired("the revoked entry was let go") {
							return false
						}
						time.Sleep(time.Millisecond)
					}
					if n := senders(e.in.hub, rec); expired("the senders were counted") {
						return false
					} else if n != 0 {
						t.Fatalf("the revoked entry is charged to nobody while %d connections still send it", n)
					}
					if measures {
						// The entry has left the heap while the readers are
						// still stopped: what is left is the send queues and
						// the parts in the hands of the connections.
						reading, why := stops.heapWhileStopped(0, nil)
						if why != "" {
							t.Logf("%s; the attempt is given up", why)
							wg.Wait()
							return false
						}
						held := reading - base
						allowed := int64(2*publicQueue + 2*privateQueue + len(readers)*2*serverFrameMax + 16<<20)
						if allowed*3/2 > int64(source) {
							t.Fatalf("the test allows %d bytes, too close to the entry's %d", allowed, source)
						}
						if held > allowed {
							t.Errorf("after the senders let go of the entry the heap holds %d bytes over its start, more than the %d allowed; the entry has %d", held, allowed, source)
						}
					}
				}
				stops.release(0)
				wg.Wait()
				if got := deltaHeld.Load() - before; got != 0 {
					t.Fatalf("after the readers took the delta %d bytes are still charged as held", got)
				}
				for _, r := range readers {
					if fits {
						for {
							raw, err := r.ws.ReadMessage(64 << 20)
							if err != nil {
								t.Fatalf("a %s reader after the delta: %v", r.route, err)
							}
							if strings.HasPrefix(raw, `{"type":"delta","seq":"`+strconv.FormatUint(seq+1, 10)+`"`) {
								break
							}
						}
					}
					resyncs := strings.Count(strings.Join(r.kinds, ","), "resync")
					if fits && (resyncs != 0 || r.bytes < frames) {
						t.Errorf("a %s reader resynced %d times and got %d bytes of parts, want all %d", r.route, resyncs, r.bytes, frames)
					}
					if !fits && (resyncs != 1 || r.bytes >= frames) {
						t.Errorf("a %s reader resynced %d times and got %d bytes of parts of %d", r.route, resyncs, r.bytes, frames)
					}
				}
				return true
			}
			for attempt := 1; !measure(attempt); attempt++ {
				if attempt == pauseAttempts {
					t.Fatalf("none of %d attempts could be measured", attempt)
				}
			}
		})
	}
}
