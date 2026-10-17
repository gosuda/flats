package metrics

import (
	"bufio"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
)

// Management server surfaces, by path prefix.
const (
	SurfaceAPI     = "api"
	SurfaceConsole = "console"
	SurfaceMCP     = "mcp"
	SurfaceMetrics = "metrics"
	SurfaceOther   = "other"
)

var surfaces = []string{SurfaceAPI, SurfaceConsole, SurfaceMCP, SurfaceMetrics, SurfaceOther}

// managementClasses are the status classes of management requests. A
// management request has no "closed" class.
var managementClasses = []string{"1xx", "2xx", "3xx", "4xx", "5xx"}

type managementCounts [5][5]atomic.Uint64 // [surface][class]

func surfaceIndex(path string) int {
	switch {
	case strings.HasPrefix(path, "/api/"):
		return 0
	case strings.HasPrefix(path, "/console/api/"):
		return 1
	case path == "/mcp" || strings.HasPrefix(path, "/mcp/"):
		return 2
	case path == "/metrics":
		return 3
	}
	return 4
}

// classIndex maps a status code to 1xx…5xx; codes net/http would refuse
// count as 5xx.
func classIndex(code int) int {
	if c := code / 100; c >= 1 && c <= 5 {
		return c - 1
	}
	return 4
}

// ManagementCount returns the number of management requests of surface with
// status class class ("2xx", …).
func (m *Metrics) ManagementCount(surface, class string) uint64 {
	for i, s := range surfaces {
		if s != surface {
			continue
		}
		for j, c := range managementClasses {
			if c == class {
				return m.mgmt[i][j].Load()
			}
		}
	}
	return 0
}

// Management counts every request next answers by surface and status class.
// Wrap it outside the host guards so refused requests are counted. A nil
// Metrics returns next unchanged.
func (m *Metrics) Management(next http.Handler) http.Handler {
	if m == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		surface := surfaceIndex(r.URL.Path)
		panicked := true
		defer func() {
			code := sw.status()
			if panicked && code == 0 {
				code = http.StatusInternalServerError
			}
			if code == 0 {
				code = http.StatusOK // net/http sends 200 for a handler that wrote nothing
			}
			m.mgmt[surface][classIndex(code)].Add(1)
		}()
		next.ServeHTTP(sw, r)
		panicked = false
	})
}

// statusWriter records the status a response sent. Unwrap lets
// http.ResponseController reach the underlying writer for deadlines; Flush
// and Hijack go through a ResponseController on it, so streams (MCP, event
// streams) and upgrades keep working.
type statusWriter struct {
	http.ResponseWriter
	code atomic.Int64
}

func (w *statusWriter) status() int { return int(w.code.Load()) }

func (w *statusWriter) sent(code int) { w.code.CompareAndSwap(0, int64(code)) }

func (w *statusWriter) WriteHeader(code int) {
	// Informational answers other than 101 precede the real status.
	if code >= 200 || code == http.StatusSwitchingProtocols {
		w.sent(code)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.sent(http.StatusOK)
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush implements http.Flusher; flushing sends the header, 200 by default.
func (w *statusWriter) Flush() {
	if http.NewResponseController(w.ResponseWriter).Flush() == nil {
		w.sent(http.StatusOK)
	}
}

// FlushError lets http.ResponseController report a failed flush.
func (w *statusWriter) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err == nil {
		w.sent(http.StatusOK)
	}
	return err
}

// Hijack implements http.Hijacker; a hijacked connection counts as 101.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.sent(http.StatusSwitchingProtocols)
	}
	return c, rw, err
}
