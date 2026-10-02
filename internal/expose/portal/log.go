package portal

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

// The Portal SDK logs through zerolog's global logger, which otherwise
// prints JSON lines to stderr at debug level. Flats replaces it once with a
// warn-level logger that forwards to the most recently configured Logf, or
// discards when there is none. No other package in Flats uses zerolog.
var (
	sdkLogOnce sync.Once
	sdkLogf    atomic.Pointer[func(string, ...any)]
)

func installSDKLogger(logf func(string, ...any)) {
	if logf != nil {
		sdkLogf.Store(&logf)
	}
	sdkLogOnce.Do(func() {
		zlog.Logger = zerolog.New(sdkLogWriter{}).Level(zerolog.WarnLevel)
	})
}

type sdkLogWriter struct{}

func (sdkLogWriter) Write(p []byte) (int, error) {
	if f := sdkLogf.Load(); f != nil {
		(*f)("%s", formatSDKLog(p))
	}
	return len(p), nil
}

// formatSDKLog turns one zerolog JSON line into
// "portal sdk <level>: <message> key=value ...".
func formatSDKLog(p []byte) string {
	var m map[string]any
	if err := json.Unmarshal(p, &m); err != nil {
		return "portal sdk: " + strings.TrimSpace(string(p))
	}
	level, _ := m["level"].(string)
	msg, _ := m["message"].(string)
	delete(m, "level")
	delete(m, "message")
	delete(m, "time")
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "portal sdk %s: %s", level, msg)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%v", k, m[k])
	}
	return b.String()
}
