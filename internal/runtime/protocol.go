// Package runtime runs server flats. The parent (Manager) starts one
// "flats worker" child process per running version instance and proxies HTTP
// to it over a Unix socket; the child (WorkerMain) runs the flat's JavaScript
// in QuickJS on wazero (JS: env.DB, env.FILES, Web Crypto and WebSocket
// callbacks) or runs a fresh WASI preview1 command per request (request JSON
// on stdin, response JSON on stdout, selected environment including secrets,
// clocks and CSPRNG). WASI has no DB/FILES host ABI or WebSocket API. Neither
// engine receives host process environment. JS outbound HTTP(S) is host-mediated
// and limited to explicitly permitted public origins; WASI has no network. JS
// loads read-only bundled modules and persists only through its host ABI;
// WASI receives no filesystem mounts.
package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Limits enforced in the worker.
const (
	DefaultTimeout     = 10 * time.Second
	DefaultMemoryPages = 1024 // 64 KiB wasm pages = 64 MiB
	DefaultPoolSize    = 4
	MaxRequestBody     = 10 << 20
	MaxResponseBody    = 32 << 20
	MaxFileValue       = 10 << 20
	MaxFilesTotal      = 1 << 30
	MaxFilesObjects    = 20_000
	MaxQueryResult     = 16 << 20
	MaxDBBytes         = 1 << 30
	MaxWSMessage       = 1 << 20
	// maxLogLine is the longest log line the worker forwards. The host
	// removes secret values from each complete line and only then truncates
	// it for storage, so the worker never cuts a line: a cut could split a
	// secret and leave a prefix the host cannot recognize. Longer lines are
	// replaced by a note that keeps only their size.
	maxLogLine = 64 << 10
)

func omittedLogLine(n int) string {
	return fmt.Sprintf("log line omitted: %d bytes is more than the %d KiB limit", n, maxLogLine>>10)
}

// workerSpec is sent to the child on stdin as one JSON line. Secrets travel
// only here: never in argv or in the child's environment.
type workerSpec struct {
	Flat           string            `json:"flat"`
	Version        int               `json:"version"`
	Generation     int64             `json:"generation"`
	Dir            string            `json:"dir"`
	Entry          string            `json:"entry"`
	DataDir        string            `json:"data_dir"`
	Env            map[string]string `json:"env"`
	CacheDir       string            `json:"cache_dir,omitempty"`
	TimeoutMS      int64             `json:"timeout_ms,omitempty"`
	MemoryPages    uint32            `json:"memory_pages,omitempty"`
	PoolSize       int               `json:"pool_size,omitempty"`
	NetworkOrigins []string          `json:"network_origins,omitempty"`
	ImagePath      string            `json:"image_path,omitempty"` // document image file; live managed documents only
	// UsageIntervalMS is how often the worker reports its CPU and memory;
	// 0 = usageInterval.
	UsageIntervalMS int64 `json:"usage_interval_ms,omitempty"`
}

// usageInterval is how often a worker reports its usage (after a first
// report right after "ready").
const usageInterval = 15 * time.Second

func (s *workerSpec) usageEvery() time.Duration {
	if s.UsageIntervalMS <= 0 {
		return usageInterval
	}
	return time.Duration(s.UsageIntervalMS) * time.Millisecond
}

// prewarmSpec is the optional first stdin line of a pre-started worker,
// sent before it is assigned a flat: {"prewarm": {...}}.
type prewarmSpec struct {
	CacheDir string `json:"cache_dir"`
	// WarmDir is a path in a private directory of the Manager, never a
	// flat's, where the worker creates a new directory (it must not exist)
	// for a throwaway database that warms up SQLite, and removes it again.
	// The Manager removes it after the worker exited.
	WarmDir string `json:"warm_dir,omitempty"`
}

func (s *workerSpec) timeout() time.Duration {
	if s.TimeoutMS <= 0 {
		return DefaultTimeout
	}
	return time.Duration(s.TimeoutMS) * time.Millisecond
}

func (s *workerSpec) pages() uint32 {
	if s.MemoryPages == 0 {
		return DefaultMemoryPages
	}
	return s.MemoryPages
}

func (s *workerSpec) poolSize() int {
	if s.PoolSize <= 0 {
		return DefaultPoolSize
	}
	return s.PoolSize
}

// childMsg is one line the child writes on stderr. Lines that are not JSON
// (for example a Go runtime crash) are forwarded as error logs.
//
// A "usage" line carries the worker's CPU time (user+system, ns since it
// started) and, on Linux, its resident private anonymous memory (RssAnon).
// Flat code cannot write such a line: its console and stderr output reaches
// the parent only inside the msg of a "log" line.
type childMsg struct {
	T     string `json:"t"` // "log", "ready", "fatal", "usage"; "warm" and "spec" (pre-started workers)
	Level string `json:"level,omitempty"`
	Msg   string `json:"msg,omitempty"`
	MS    int64  `json:"ms,omitempty"`
	CPU   *int64 `json:"cpu_ns,omitempty"`
	Mem   *int64 `json:"mem,omitempty"`
}

// childLog writes rate-limited JSON lines to the parent.
type childLog struct {
	mu      sync.Mutex
	w       io.Writer
	tokens  float64
	last    time.Time
	dropped int
}

func newChildLog(w io.Writer) *childLog {
	return &childLog{w: w, tokens: logBurst, last: time.Now()}
}

const (
	logBurst = 100
	logRate  = 20 // lines per second
)

func (l *childLog) send(m childMsg) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, _ := json.Marshal(m)
	l.w.Write(append(b, '\n'))
}

// Logf logs one line (rate limited; control messages are never dropped).
func (l *childLog) Log(level, msg string) {
	if len(msg) > maxLogLine {
		msg = omittedLogLine(len(msg))
	}
	l.mu.Lock()
	now := time.Now()
	l.tokens += now.Sub(l.last).Seconds() * logRate
	if l.tokens > logBurst {
		l.tokens = logBurst
	}
	l.last = now
	if l.tokens < 1 {
		l.dropped++
		l.mu.Unlock()
		return
	}
	l.tokens--
	dropped := l.dropped
	l.dropped = 0
	l.mu.Unlock()
	if dropped > 0 {
		l.send(childMsg{T: "log", Level: "warn", Msg: fmt.Sprintf("%d log lines dropped (rate limit %d/s)", dropped, logRate)})
	}
	l.send(childMsg{T: "log", Level: level, Msg: msg})
}

// lineWriter turns guest stdout/stderr bytes into log lines.
type lineWriter struct {
	mu      sync.Mutex
	log     *childLog
	level   string
	buf     []byte
	omitted int // length of the current line once it exceeds maxLogLine
}

// appendFragment buffers a line only while it fits maxLogLine, plus one byte
// for a CR that emit drops as part of a CRLF delimiter (Log omits the line if
// it is still too long). Once it is longer, keep no part of it (see
// maxLogLine), only count its length.
func (w *lineWriter) appendFragment(p []byte) {
	if w.omitted > 0 {
		w.omitted += len(p)
		return
	}
	if len(p) > maxLogLine+1-len(w.buf) {
		w.omitted = len(w.buf) + len(p)
		w.buf = w.buf[:0]
		return
	}
	w.buf = append(w.buf, p...)
}

func (w *lineWriter) emit() {
	if w.omitted > 0 {
		w.log.Log(w.level, omittedLogLine(w.omitted))
	} else if line := strings.TrimRight(string(w.buf), "\r"); line != "" {
		w.log.Log(w.level, line)
	}
	w.buf = w.buf[:0]
	w.omitted = 0
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.appendFragment(p)
			break
		}
		w.appendFragment(p[:i])
		w.emit()
		p = p[i+1:]
	}
	return n, nil
}

func (w *lineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.emit()
	w.buf = nil
}

var stderrLog = newChildLog(os.Stderr)
