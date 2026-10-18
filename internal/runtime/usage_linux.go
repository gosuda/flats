//go:build linux

package runtime

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// privateMemory is RssAnon from /proc/self/status: the resident private
// anonymous memory, which covers the Go heap and stacks, wazero's linear
// memory and compiled code and SQLite's allocations, and excludes the shared
// program text.
func privateMemory() (int64, bool) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "RssAnon:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			return 0, false
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || kb < 0 {
			return 0, false
		}
		return kb << 10, true
	}
	return 0, false
}
