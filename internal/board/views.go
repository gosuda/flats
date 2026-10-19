package board

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
)

// assetView is a copy of the assets table for request handlers, updated
// after each commit, so serving an image never waits for the apply loop.
type assetView struct {
	mu    sync.RWMutex
	m     map[string]engine.Asset
	liveN int
}

func newAssetView() *assetView { return &assetView{m: map[string]engine.Asset{}} }

func (v *assetView) reset(rows []engine.Asset, st *engine.State) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.m = map[string]engine.Asset{}
	v.liveN = 0
	for _, r := range rows {
		if a, ok := st.AssetRow(r.Hash); ok {
			v.m[a.Hash] = a
			if a.Refs > 0 {
				v.liveN++
			}
		}
	}
}

func (v *assetView) apply(ch map[string]*engine.Asset) {
	if len(ch) == 0 {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for h, a := range ch {
		if old, ok := v.m[h]; ok && old.Refs > 0 {
			v.liveN--
		}
		if a == nil {
			delete(v.m, h)
			continue
		}
		v.m[h] = *a
		if a.Refs > 0 {
			v.liveN++
		}
	}
}

func (v *assetView) get(h string) (engine.Asset, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	a, ok := v.m[h]
	return a, ok
}

// all lists the hashes of every asset row.
func (v *assetView) all() []string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return sortedKeys(v.m)
}

func (v *assetView) liveCount() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.liveN
}

// live lists the assets alive elements reference, with a transaction's
// uncommitted changes on top, sorted by hash.
func (v *assetView) live(overlay map[string]*engine.Asset) []engine.Asset {
	v.mu.RLock()
	defer v.mu.RUnlock()
	var out []engine.Asset
	for h, a := range v.m {
		if o, changed := overlay[h]; changed {
			if o == nil {
				continue
			}
			a = *o
		}
		if a.Refs > 0 {
			out = append(out, a)
		}
	}
	for h, o := range overlay {
		if _, seen := v.m[h]; !seen && o != nil && o.Refs > 0 {
			out = append(out, *o)
		}
	}
	slices.SortFunc(out, func(a, b engine.Asset) int { return strings.Compare(a.Hash, b.Hash) })
	return out
}

// measurer caches the measurement of the flat directory without board.db
// (5.4 "Measuring other"). extra counts bytes stored as blobs since the
// running or last measurement started (uploads and imports).
type measurer struct {
	in      *Instance
	mu      sync.Mutex
	other   int64
	at      time.Time
	valid   bool
	running bool
	extra   int64
	waiters []chan struct{}
}

const (
	measureTTL     = 60 * time.Second
	measureRefresh = 45 * time.Second
)

func newMeasurer(in *Instance) *measurer { return &measurer{in: in} }

// get returns other plus the bytes stored since, when a measurement at
// most 60 seconds old exists.
func (m *measurer) get() (int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.valid || m.in.now().Sub(m.at) > measureTTL {
		return 0, false
	}
	return m.other + m.extra, true
}

// add counts bytes stored as blobs after the measurement.
func (m *measurer) add(n int64) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	m.extra += n
	m.mu.Unlock()
}

// install adopts a measurement core took under the flat lock. Bytes stored
// meanwhile stay counted (on the safe side).
func (m *measurer) install(other int64) {
	m.mu.Lock()
	m.other, m.at, m.valid = other, m.in.now(), true
	m.mu.Unlock()
}

// start begins a measurement in a helper goroutine unless one runs. The old
// value stays usable until the new one arrives.
func (m *measurer) start() {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return
	}
	m.running = true
	base := m.extra
	m.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(m.in.ctx, 2*time.Minute)
		defer cancel()
		other, err := m.in.b.MeasureOther(ctx)
		m.mu.Lock()
		m.running = false
		if err != nil {
			m.valid = false
			m.in.log.Warn("board measurement failed", "err", err)
		} else {
			m.other, m.at, m.valid = other, m.in.now(), true
			m.extra -= base
		}
		ws := m.waiters
		m.waiters = nil
		m.mu.Unlock()
		for _, w := range ws {
			close(w)
		}
	}()
}

// wait returns a current measurement, starting one and waiting for it
// when none is fresh.
func (m *measurer) wait(ctx context.Context) (int64, error) {
	for i := 0; i < 2; i++ {
		if v, ok := m.get(); ok {
			return v, nil
		}
		ch := make(chan struct{})
		m.mu.Lock()
		m.waiters = append(m.waiters, ch)
		m.mu.Unlock()
		m.start()
		select {
		case <-ch:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if v, ok := m.get(); ok {
		return v, nil
	}
	return 0, errorf(CategoryUnavailable, "the board's disk usage cannot be measured right now; try again")
}

// refresher keeps the measurement fresh while Private connections exist,
// so an editor's batch rarely misses the cache.
func (m *measurer) refresher() {
	t := time.NewTicker(measureRefresh)
	defer t.Stop()
	for {
		select {
		case <-m.in.ctx.Done():
			return
		case <-t.C:
			if m.in.hub.privateCount() > 0 && m.in.b.Limits().DiskQuota > 0 {
				m.start()
			}
		}
	}
}

// shareList holds the selections people sent to agents (6.2): the most
// recent 50, for 24 hours, in memory only.
type shareList struct {
	mu   sync.Mutex
	list []Share
}

// Share is a selection a person sent to an agent.
type Share struct {
	N        int64    `json:"n"`
	IDs      []string `json:"ids"`
	Key      string   `json:"-"`
	Label    string   `json:"label"`
	Verified bool     `json:"verified"`
	At       int64    `json:"at"`
}

func (l *shareList) add(s Share) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = append(l.list, s)
	if len(l.list) > 50 {
		l.list = slices.Clone(l.list[len(l.list)-50:])
	}
}

func (l *shareList) expire(now int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = slices.DeleteFunc(l.list, func(s Share) bool { return now-s.At >= 24*3600 })
}

func (l *shareList) snapshot() []Share {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.list)
}

// startLimiter limits Public snapshot starts per board (6): a token bucket
// with a rate and burst of perSecond.
type startLimiter struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
}

func newStartLimiter(perSecond float64) *startLimiter {
	return &startLimiter{rate: perSecond, tokens: perSecond, last: time.Now()}
}

// wait takes a token, waiting at most max.
func (l *startLimiter) wait(ctx context.Context, max time.Duration) bool {
	deadline := time.Now().Add(max)
	for {
		l.mu.Lock()
		now := time.Now()
		l.tokens = min(l.rate, l.tokens+now.Sub(l.last).Seconds()*l.rate)
		l.last = now
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return true
		}
		need := time.Duration((1 - l.tokens) / l.rate * float64(time.Second))
		l.mu.Unlock()
		if now.Add(need).After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(need):
		}
	}
}

// pageCache keeps the consistent snapshots of multi-page get_board reads
// (8.1): at most 4 per board, 60 seconds and 32 MiB each, 256 MiB per host.
type pageCache struct {
	mu sync.Mutex
	m  map[string]*pageSnap
}

type pageSnap struct {
	pages   [][]byte // the JSON elements of each page after the first
	seq     int64
	pending bool
	bytes   int64
	expires time.Time
}

const (
	pageSnapsPerBoard = 4
	pageSnapTTL       = 60 * time.Second
	pageSnapMax       = 32 << 20
)

var pageHostBytes atomic.Int64

const pageHostMax = 256 << 20

func newPageCache() *pageCache { return &pageCache{m: map[string]*pageSnap{}} }

// put stores a snapshot and returns its cursor, evicting the oldest of the
// board beyond four. It refuses when the host is full.
func (c *pageCache) put(s *pageSnap, now time.Time) (string, error) {
	if s.bytes > pageSnapMax {
		return "", errorf(CategoryInvalid, "the read is larger than 32 MiB; narrow it by frame, area or ids, or read a revision")
	}
	if pageHostBytes.Add(s.bytes) > pageHostMax {
		pageHostBytes.Add(-s.bytes)
		return "", errorf(CategoryUnavailable, "too many paged reads are open on this host; try again shortly")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked(now)
	for len(c.m) >= pageSnapsPerBoard {
		var oldest string
		for k, v := range c.m {
			if oldest == "" || v.expires.Before(c.m[oldest].expires) {
				oldest = k
			}
		}
		c.dropLocked(oldest)
	}
	tok := randomToken()
	s.expires = now.Add(pageSnapTTL)
	c.m[tok] = s
	return tok, nil
}

// next takes the next page of cursor tok.
func (c *pageCache) next(tok string, now time.Time) (*pageSnap, []byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked(now)
	s := c.m[tok]
	if s == nil || len(s.pages) == 0 {
		return nil, nil, false
	}
	page := s.pages[0]
	s.pages = s.pages[1:]
	pageHostBytes.Add(-int64(len(page)))
	s.bytes -= int64(len(page))
	if len(s.pages) == 0 {
		c.dropLocked(tok)
	}
	return s, page, true
}

func (c *pageCache) dropLocked(tok string) {
	if s := c.m[tok]; s != nil {
		pageHostBytes.Add(-s.bytes)
		delete(c.m, tok)
	}
}

func (c *pageCache) expireLocked(now time.Time) {
	for k, v := range c.m {
		if now.After(v.expires) {
			c.dropLocked(k)
		}
	}
}

func (c *pageCache) expire(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked(now)
}

func (c *pageCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.m {
		c.dropLocked(k)
	}
}
