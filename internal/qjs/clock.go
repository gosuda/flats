package qjs

import (
	"sync"
	"sync/atomic"
	"time"
)

// FORK (flats): per-runtime guest clocks.
//
// Upstream used wazero's system clocks. Every runtime now has its own guest
// clocks so a runtime restored from an Image can continue the monotonic
// clock of the runtime it was captured from (QuickJS keeps
// performance.timeOrigin and std timer deadlines in that clock). A fresh
// runtime's offset is exactly 0, so it reads the same values wazero's system
// clocks return. Wall time is always the real host clock.
//
// While New_QJS runs, the readings handed to the guest are recorded: they
// identify the JSContext's random_state slot (see findRandomState).

type guestClock struct {
	offset    atomic.Int64 // added to the host monotonic clock
	recording atomic.Bool
	mu        sync.Mutex
	walls     []int64 // wall readings (ns since the epoch) while recording
	monos     []int64 // monotonic readings while recording
	census    *Census
}

// walltime is wazero's sys.Walltime: time.Now, as WithSysWalltime.
func (c *guestClock) walltime() (int64, int32) {
	t := time.Now()
	sec, nsec := t.Unix(), int32(t.Nanosecond())
	if c.census != nil {
		c.census.WallReads.Add(1)
	}
	if c.recording.Load() {
		c.mu.Lock()
		c.walls = append(c.walls, sec*1e9+int64(nsec))
		c.mu.Unlock()
	}
	return sec, nsec
}

// nanotime is wazero's sys.Nanotime: the host monotonic clock (the value
// WithSysNanotime returns) plus the runtime's offset.
func (c *guestClock) nanotime() int64 {
	v := c.now()
	if c.census != nil {
		c.census.MonoReads.Add(1)
	}
	if c.recording.Load() {
		c.mu.Lock()
		c.monos = append(c.monos, v)
		c.mu.Unlock()
	}
	return v
}

// now is the guest monotonic time without counting it as a guest read.
func (c *guestClock) now() int64 { return hostNanotime() + c.offset.Load() }

// stopRecording ends recording and returns the readings.
func (c *guestClock) stopRecording() (walls, monos []int64) {
	c.recording.Store(false)
	c.mu.Lock()
	defer c.mu.Unlock()
	walls, monos = c.walls, c.monos
	c.walls, c.monos = nil, nil
	return walls, monos
}

// Census counts guest events of one runtime: clock reads, bytes written to
// stdout/stderr, and files opened and closed in the ReadOnlyDir mount.
// FORK (flats): for tests that pin what a module does while it loads.
type Census struct {
	WallReads   atomic.Int64
	MonoReads   atomic.Int64
	OutputBytes atomic.Int64
	FileOpens   atomic.Int64
	FileCloses  atomic.Int64
}
