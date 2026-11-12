package docs

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/ostest"
)

// On the Windows CI runner the first guest call of several document workers
// has taken the whole request timeout, and nobody found why (the current state
// in docs/spec/windows-support.md). Until someone does, a Windows test worker
// gives its goroutine stacks to a test whose first call is slow, so that the
// next such run shows where the worker waits.

// stallDirEnv names, to a test worker, the directory where it looks for a
// request for its stacks.
const stallDirEnv = "FLATS_DOCS_TEST_STALL_DIR"

// firstCallStall is how long the first guest call of a test host may take
// before the test asks the worker for its stacks: half the request timeout.
const firstCallStall = 5 * time.Second

// stallDir is outside every flat's data, so no test measures its files.
func stallDir() string { return filepath.Join(cacheRoot, "stall") }

// dumpStacksOnRequest runs in a test worker. When the file <pid>.want appears
// in dir, it writes all goroutine stacks to <pid>.stacks, twice and 2 s apart,
// so that a reader sees whether a goroutine moved.
func dumpStacksOnRequest(dir string) {
	base := filepath.Join(dir, strconv.Itoa(os.Getpid()))
	buf := make([]byte, 4<<20)
	for {
		time.Sleep(250 * time.Millisecond)
		if _, err := os.Stat(base + ".want"); err != nil {
			continue
		}
		var out bytes.Buffer
		for sample := 1; sample <= 2; sample++ {
			if sample > 1 {
				time.Sleep(2 * time.Second)
			}
			start := time.Now()
			stacks := buf[:runtime.Stack(buf, true)]
			fmt.Fprintf(&out, "sample %d at %s, the dump took %s\n%s\n", sample, start.Format("15:04:05.000"), time.Since(start), stacks)
		}
		os.Remove(base + ".want")
		if os.WriteFile(base+".tmp", out.Bytes(), 0o600) == nil {
			os.Rename(base+".tmp", base+".stacks")
		}
	}
}

// watchCall asks worker pid for its stacks unless the returned function is
// called within after. That function returns the stacks, or "" when the test
// did not ask or the worker did not answer.
func watchCall(pid int, after time.Duration) func() string {
	if !ostest.Windows || pid == 0 {
		return func() string { return "" }
	}
	base := filepath.Join(stallDir(), strconv.Itoa(pid))
	// Windows gives the id of an ended process to a new one. Nothing of an
	// earlier worker with this id may stay, before the request or after it.
	os.Remove(base + ".stacks")
	asked := make(chan struct{})
	timer := time.AfterFunc(after, func() {
		os.WriteFile(base+".want", nil, 0o600)
		close(asked)
	})
	return func() string {
		if timer.Stop() {
			return ""
		}
		<-asked
		defer os.Remove(base + ".want")
		defer os.Remove(base + ".stacks")
		for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			if b, err := os.ReadFile(base + ".stacks"); err == nil {
				return string(b)
			}
		}
		return ""
	}
}

func TestWorkerGivesItsStacks(t *testing.T) {
	if !ostest.Windows {
		t.Skip("only a Windows test worker answers a request for its stacks")
	}
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	stacks := watchCall(a.pid, time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	got := stacks()
	if !strings.Contains(got, "sample 1 at") || !strings.Contains(got, "sample 2 at") || strings.Count(got, "\ngoroutine ") < 2 {
		t.Fatalf("stacks of worker %d:\n%s", a.pid, got)
	}
}
