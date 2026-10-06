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
	MaxQueryResult     = 16 << 20
	MaxWSMessage       = 1 << 20
	maxLogLine         = 8 << 10
)

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
type childMsg struct {
	T     string `json:"t"` // "log", "ready", "fatal"
	Level string `json:"level,omitempty"`
	Msg   string `json:"msg,omitempty"`
	MS    int64  `json:"ms,omitempty"`
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
		msg = msg[:maxLogLine] + "… (truncated)"
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
	mu    sync.Mutex
	log   *childLog
	level string
	buf   []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			if len(w.buf) > maxLogLine {
				w.log.Log(w.level, string(w.buf))
				w.buf = w.buf[:0]
			}
			return len(p), nil
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		if line != "" {
			w.log.Log(w.level, line)
		}
	}
}

func (w *lineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.log.Log(w.level, string(w.buf))
		w.buf = nil
	}
}

var stderrLog = newChildLog(os.Stderr)
