package runtime

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
)

// loadAverage is the host's load average (for timing reports).
func loadAverage() string {
	if goruntime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/loadavg")
		if err != nil {
			return err.Error()
		}
		return strings.TrimSpace(string(b))
	}
	b, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return err.Error()
	}
	return strings.TrimSpace(string(b))
}

// percentiles are nearest-rank p50, p95 and the maximum.
func percentiles(samples []time.Duration) (p50, p95, maximum time.Duration) {
	s := slices.Clone(samples)
	slices.Sort(s)
	rank := func(p int) time.Duration { return s[(p*len(s)+99)/100-1] }
	return rank(50), rank(95), s[len(s)-1]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// smapsRollup returns selected lines of a process's /proc smaps_rollup.
func smapsRollup(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	if err != nil {
		return "", err
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		for _, k := range []string{"Rss:", "Pss:", "Pss_Anon:", "Pss_File:", "Private_Clean:", "Private_Dirty:", "Swap:"} {
			if strings.HasPrefix(l, k) {
				out = append(out, strings.Join(strings.Fields(l), " "))
			}
		}
	}
	return strings.Join(out, "; "), nil
}

// Requirement 1 of docs/spec/worker-cold-start.md with the real binary: a
// tiny JavaScript server flat's Start and first GET take under 50 ms (p95
// over 20 starts) when the pre-started worker is ready.
func TestColdStartBudget(t *testing.T) {
	if os.Getenv("FLATS_COLDSTART_BUDGET") != "1" {
		t.Skip("set FLATS_COLDSTART_BUDGET=1 and FLATS_WORKER_EXE for the 20-start cold start budget")
	}
	exe := os.Getenv("FLATS_WORKER_EXE")
	if exe == "" {
		t.Skip("set FLATS_WORKER_EXE to a built flats binary")
	}
	t.Logf("COLDSTART_LOAD %s %s", goruntime.GOOS, loadAverage())
	// Short: the sockets live in DataDir/run.
	data, err := os.MkdirTemp("/tmp", "flats-budget-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(data) })
	m := &Manager{DataDir: data, Exe: exe, Logf: t.Logf}
	t.Cleanup(m.Close)
	version := t.TempDir()
	writeFiles(t, version, spareTestFiles())
	start := func(name string) core.Instance {
		t.Helper()
		in, err := m.Start(context.Background(), core.RuntimeSpec{Flat: name, Version: 1, Dir: version, Entry: "index.js",
			DataDir: filepath.Join(data, "flats", name), Log: func(level, msg string) { t.Logf("%s: %s: %s", name, level, msg) }})
		if err != nil {
			t.Fatal(err)
		}
		return in
	}
	// A long-lived instance keeps the spare, and its exec start warms the
	// compilation cache.
	keep := start("keep")
	t.Cleanup(keep.Stop)
	var totals, starts []time.Duration
	for i := 0; i < 20; i++ {
		deadline := time.Now().Add(10 * time.Second)
		for !m.SpareReady() {
			if time.Now().After(deadline) {
				t.Fatal("the pre-started worker did not become ready within 10s")
			}
			time.Sleep(5 * time.Millisecond)
		}
		spare := m.SparePID()
		t0 := time.Now()
		in := start(fmt.Sprintf("tiny-%d", i))
		started := time.Since(t0)
		rec := httptest.NewRecorder()
		in.ServeHTTP(rec, httptest.NewRequest("GET", "http://flat/", nil))
		total := time.Since(t0)
		pid := in.(*instance).pid()
		in.Stop()
		if rec.Code != 200 || rec.Body.String() != "ok" {
			t.Fatalf("GET = %d %q", rec.Code, rec.Body)
		}
		if pid != spare {
			t.Fatalf("start %d ran in process %d, not the ready pre-started worker %d", i+1, pid, spare)
		}
		totals, starts = append(totals, total), append(starts, started)
		t.Logf("COLDSTART_FLAT sample=%d total_ms=%.2f start_ms=%.2f get_ms=%.2f", i+1, ms(total), ms(started), ms(total-started))
	}
	p50, p95, maximum := percentiles(totals)
	s50, s95, smax := percentiles(starts)
	t.Logf("COLDSTART_FLAT_TOTAL p50_ms=%.2f p95_ms=%.2f max_ms=%.2f", ms(p50), ms(p95), ms(maximum))
	t.Logf("COLDSTART_FLAT_START p50_ms=%.2f p95_ms=%.2f max_ms=%.2f", ms(s50), ms(s95), ms(smax))
	if p95 >= 50*time.Millisecond {
		t.Errorf("tiny server flat Start and first GET: p95 %v, budget 50ms", p95)
	}
	// The ready spare's memory.
	deadline := time.Now().Add(10 * time.Second)
	for !m.SpareReady() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if pid := m.SparePID(); pid == 0 || !m.SpareReady() {
		t.Error("no ready pre-started worker to measure")
	} else if goruntime.GOOS != "linux" {
		t.Logf("COLDSTART_SPARE pid=%d memory not measured on %s (PSS needs /proc/<pid>/smaps_rollup)", pid, goruntime.GOOS)
	} else if s, err := smapsRollup(pid); err != nil {
		t.Errorf("spare memory: %v", err)
	} else {
		t.Logf("COLDSTART_SPARE pid=%d %s", pid, s)
	}
}
