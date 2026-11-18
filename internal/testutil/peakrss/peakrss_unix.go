//go:build unix

// Package peakrss measures the peak resident memory of test subprocesses.
package peakrss

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

const vmHWMPrefix = "VmHWM:"

// Report prints the high-water mark of the child's address space on Linux,
// where rusage can retain a peak from before exec.
func Report(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		return
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(status), "\n") {
		if strings.HasPrefix(line, vmHWMPrefix) {
			fmt.Println(line)
			return
		}
	}
	t.Fatal("VmHWM is missing from /proc/self/status")
}

// Bytes returns the child's peak resident memory in bytes.
func Bytes(t *testing.T, state *os.ProcessState, output []byte) int64 {
	t.Helper()
	if runtime.GOOS != "linux" {
		peak := int64(state.SysUsage().(*syscall.Rusage).Maxrss)
		if runtime.GOOS != "darwin" {
			peak *= 1024 // kilobytes elsewhere
		}
		return peak
	}
	var peak int64
	for line := range strings.SplitSeq(string(output), "\n") {
		value, ok := strings.CutPrefix(line, vmHWMPrefix)
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		if peak != 0 || len(fields) != 2 || fields[1] != "kB" {
			t.Fatalf("invalid or repeated VmHWM report %q", line)
		}
		kib, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil || kib == 0 || kib > math.MaxInt64/1024 {
			t.Fatalf("invalid VmHWM size %q", fields[0])
		}
		peak = int64(kib) * 1024
	}
	if peak == 0 {
		t.Fatal("the child did not report VmHWM")
	}
	return peak
}
