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
	MaxQueryResult     = 16 << 20
	MaxWSMessage       = 1 << 20
	maxLogLine         = 8 << 10
	omittedLogLine     = "log line omitted (exceeds 8 KiB)"
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
	// The parent redacts exact secret values, including secrets larger than
	// this limit. A truncated prefix would bypass that redaction.
	if len(msg) > maxLogLine {
		msg = omittedLogLine
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
	omitted bool
}

// appendFragment buffers a line only while its complete contents can be logged.
// Once it exceeds the limit, keep no prefix that could contain part of a secret.
func (w *lineWriter) appendFragment(p []byte) {
	if w.omitted {
		return
	}
	if len(p) > maxLogLine-len(w.buf) {
		w.buf = w.buf[:0]
		w.omitted = true
		return
	}
	w.buf = append(w.buf, p...)
}

func (w *lineWriter) emit() {
	if w.omitted {
		w.log.Log(w.level, omittedLogLine)
	} else if line := strings.TrimRight(string(w.buf), "\r"); line != "" {
		w.log.Log(w.level, line)
	}
	w.buf = w.buf[:0]
	w.omitted = false
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
