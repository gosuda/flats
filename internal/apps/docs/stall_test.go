package docs

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/ostest"
)

// On the Windows CI runner the first guest call of several document workers
// has taken the whole request timeout, and nobody found why (the current state
// in docs/spec/windows-support.md). Until someone does, a Windows test worker
// gives its goroutine stacks to a test whose first call is slow, so that the
// next such run shows where the worker waits.

// stallDirEnv names, to a test worker, the directory where it looks for
// requests for its stacks.
const stallDirEnv = "FLATS_DOCS_TEST_STALL_DIR"

// firstCallStall is how long the first guest call of a test host may take
// before the test asks the worker for its stacks: half the request timeout.
const firstCallStall = 5 * time.Second

// stallDir is outside every flat's data, so no test measures its files.
func stallDir() string { return filepath.Join(cacheRoot, "stall") }

// dumpStacksOnRequest runs in a test worker. A request is a file
// <pid>.<name>.want in dir. The worker answers it with all goroutine stacks in
// <pid>.<name>.stacks, twice and 2 s apart, so that a reader sees whether a
// goroutine moved.
func dumpStacksOnRequest(dir string) {
	started := time.Now()
	mine := strconv.Itoa(os.Getpid()) + "."
	for {
		time.Sleep(250 * time.Millisecond)
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			name, ok := strings.CutSuffix(e.Name(), ".want")
			if !ok || !strings.HasPrefix(name, mine) {
				continue
			}
			// Removed first, so that a request gets one answer. A request
			// that its writer still holds open stays for the next round.
			if os.Remove(filepath.Join(dir, e.Name())) != nil {
				continue
			}
			// Per request: a worker that nobody asks keeps no buffer.
			buf := make([]byte, 4<<20)
			var out bytes.Buffer
			for sample := 1; sample <= 2; sample++ {
				if sample > 1 {
					time.Sleep(2 * time.Second)
				}
				start := time.Now()
				stacks := buf[:runtime.Stack(buf, true)]
				fmt.Fprintf(&out, "sample %d at %s, worker %d up for %s, the dump took %s\n%s\n",
					sample, start.Format("15:04:05.000"), os.Getpid(), start.Sub(started).Round(time.Millisecond), time.Since(start), stacks)
			}
			answer := filepath.Join(dir, name+".stacks")
			if os.WriteFile(answer+".tmp", out.Bytes(), 0o600) == nil {
				os.Rename(answer+".tmp", answer)
			}
		}
	}
}

// watchCall asks worker pid for its stacks unless the returned function is
// called within after. That function returns the stacks, or "" when the test
// did not ask or the worker did not answer.
//
// Windows gives the id of an ended process to a new one. Each request has its
// own name, so a test reads only the answer to its own request, and the
// cleanup removes the request and the answer on every way out of the test.
// A caller registers the cleanup that stops the worker after this call, so
// that the worker has ended when this cleanup runs. One case stays: a worker
// that ends with a request open, and a new worker that gets its id within the
// 10 s of the wait. The header of each sample names how long its worker ran.
func watchCall(t *testing.T, pid int, after time.Duration) func() string {
	if !ostest.Windows || pid == 0 {
		return func() string { return "" }
	}
	base := filepath.Join(stallDir(), strconv.Itoa(pid)+"."+strconv.FormatUint(rand.Uint64(), 16))
	var mu sync.Mutex
	asked, done := false, false
	timer := time.AfterFunc(after, func() {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			asked = os.WriteFile(base+".want", nil, 0o600) == nil
		}
	})
	stop := func() bool {
		mu.Lock()
		defer mu.Unlock()
		done = true
		timer.Stop()
		return asked
	}
	t.Cleanup(func() {
		stop()
		os.Remove(base + ".want")
		os.Remove(base + ".stacks")
	})
	return func() string {
		if !stop() {
			return ""
		}
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
	stacks := watchCall(t, a.pid, time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	got := stacks()
	var samples []string
	for line := range strings.Lines(got) {
		if strings.HasPrefix(line, "sample ") {
			samples = append(samples, strings.TrimSpace(line))
		}
	}
	t.Log(strings.Join(samples, "\n"))
	want := fmt.Sprintf(", worker %d up for ", a.pid)
	if len(samples) != 2 || !strings.Contains(samples[0], want) || strings.Count(got, "\ngoroutine ") < 2 {
		t.Fatalf("stacks of worker %d:\n%s", a.pid, got)
	}
	entries, err := os.ReadDir(stallDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), strconv.Itoa(a.pid)+".") {
			t.Fatalf("a file of the request stayed: %s", e.Name())
		}
	}
}
