//go:build unix

package sheet

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/gosuda/flats/internal/sqliteuri"
)

const vmHWMPrefix = "VmHWM:"

// Linux rusage can retain a peak from before exec. The child reports the
// high-water mark of the address space it used for the test.
func reportPeakRSS(t *testing.T) {
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

func childPeakRSS(t *testing.T, state *os.ProcessState, output []byte) int64 {
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

// peakLoadDir names the sheet whose sheet.db TestLoadInAProcessOfItsOwn
// loads.
const peakLoadDir = "FLATS_SHEET_PEAK_LOAD_DIR"

// TestLoadInAProcessOfItsOwn loads the sheet.db that peakLoadDir names and
// exits. TestOversizedFieldsStayOutOfSQLite runs it in a process of its
// own, whose peak resident memory it reads; it does nothing otherwise.
func TestLoadInAProcessOfItsOwn(t *testing.T) {
	dir := os.Getenv(peakLoadDir)
	if dir == "" {
		t.Skip("runs in a process that TestOversizedFieldsStayOutOfSQLite starts")
	}
	defer reportPeakRSS(t)
	db, err := sql.Open("sqlite", sqliteuri.File(filepath.Join(dir, dbName))+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = loadWorkbook(context.Background(), db)
	fmt.Println("load:", err)
}

// peakLoad loads the sheet.db of the stopped sheet in a process of its own
// and returns that process's peak resident memory in bytes.
func peakLoad(t *testing.T, e *env) int64 {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLoadInAProcessOfItsOwn$", "-test.count=1")
	cmd.Env = append(os.Environ(), peakLoadDir+"="+e.dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the load process: %v\n%s", err, out)
	}
	t.Logf("%s", out)
	return childPeakRSS(t, cmd.ProcessState, out)
}

// A field of 32 MiB is refused without SQLite reading it either: the peak
// memory of a process that loads a sheet.db with such a field, in a tab's
// axis or in a cell's text, is within a few MiB of one that loads the
// sheet without it. A field read whole would take 32 MiB in SQLite and as
// much again in the copy the driver hands over.
func TestOversizedFieldsStayOutOfSQLite(t *testing.T) {
	if !measures {
		t.Skip("measures the memory of a process")
	}
	const n = 32 << 20
	base := fieldSheet(t)
	baseline := peakLoad(t, base)
	for _, name := range []string{"the row ids", "a text"} {
		e := copied(t, base, fmt.Sprintf(oversizedFields[name], n))
		peak := peakLoad(t, e)
		t.Logf("%s: a peak of %d MiB, %d MiB without the field", name, peak>>20, baseline>>20)
		if peak > baseline+8<<20 {
			t.Errorf("%s of %d MiB: the load process peaked at %d MiB, %d MiB without the field", name, n>>20, peak>>20, baseline>>20)
		}
	}
}
