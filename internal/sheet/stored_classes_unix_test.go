//go:build unix

package sheet

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/testutil/peakrss"
)

// storedReadersDir names the directory of sheets whose readers
// TestStoredReadersInAProcessOfTheirOwn runs.
const storedReadersDir = "FLATS_SHEET_STORED_READERS_DIR"

// TestStoredReadersInAProcessOfTheirOwn runs every reader of sheet.db
// (runStoredReaders) on each sheet in the directory storedReadersDir
// names, with the classBase in its base.json, and prints what each did.
// TestStoredValuesPastTheirBoundsStayOutOfMemory runs it in a process of
// its own, whose peak resident memory it reads; it does nothing otherwise.
func TestStoredReadersInAProcessOfTheirOwn(t *testing.T) {
	dir := os.Getenv(storedReadersDir)
	if dir == "" {
		t.Skip("runs in a process that TestStoredValuesPastTheirBoundsStayOutOfMemory starts")
	}
	defer peakrss.Report(t)
	raw, err := os.ReadFile(filepath.Join(dir, "base.json"))
	if err != nil {
		t.Fatal(err)
	}
	var b classBase
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		got := runStoredReaders(filepath.Join(dir, entry.Name()), &b)
		line, _ := json.Marshal(got)
		fmt.Printf("stored readers %s %s\n", entry.Name(), line)
		// What one sheet left is returned before the next, so that the
		// peak is that of the sheet whose readers took the most.
		runtime.GC()
		debug.FreeOSMemory()
	}
}

// peakReaders runs the readers of each sheet in dir in a process of its
// own and returns what they did, by sheet, and the process's peak
// resident memory in bytes.
func peakReaders(t *testing.T, dir string) (map[string]readerResult, int64) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStoredReadersInAProcessOfTheirOwn$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), storedReadersDir+"="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the readers' process: %v\n%s", err, out)
	}
	results := map[string]readerResult{}
	for scan := bufio.NewScanner(bytes.NewReader(out)); scan.Scan(); {
		rest, ok := strings.CutPrefix(scan.Text(), "stored readers ")
		if !ok {
			continue
		}
		name, line, _ := strings.Cut(rest, " ")
		var got readerResult
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatal(err)
		}
		results[name] = got
	}
	return results, peakrss.Bytes(t, cmd.ProcessState, out)
}

// readerBatch is the number of sheets one process reads. Their files are
// on disk together.
const readerBatch = 12

// An oversized text and an oversized blob of 16 MiB, in turn in each
// column of each table of sheet.db as SQLite lists them, are refused by
// the open or the load and by every reader of their table, the writer
// runs nothing, the damaged group stays, and the backup check refuses the
// file lists and hashes it reads. The peak memory of a process that runs
// every reader on a batch of such sheets is within a few MiB of one that
// runs them on as many undamaged sheets: no value is read whole, by the
// driver or inside SQLite. A value read whole would take 16 MiB in SQLite
// at least. A column the schema gains is in the list without a change here.
func TestStoredValuesPastTheirBoundsStayOutOfMemory(t *testing.T) {
	if !measures {
		t.Skip("measures the memory of a process")
	}
	const n = 16 << 20
	b := newClassBase(t)
	base := filepath.Join(b.e.dir, dbName)
	batch := func(name string, prepare func(dir string) bool) string {
		dir := filepath.Join(t.TempDir(), name)
		raw, _ := json.Marshal(b)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "base.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		_ = prepare
		return dir
	}
	sheet := func(batchDir string, i int) string {
		dir := filepath.Join(batchDir, fmt.Sprintf("%03d", i))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		copyFile(t, base, filepath.Join(dir, dbName))
		return dir
	}
	undamaged := batch("undamaged", nil)
	for i := range readerBatch {
		sheet(undamaged, i)
	}
	baseResults, baseline := peakReaders(t, undamaged)
	for name, got := range baseResults {
		if got["load"] != "read" || got["write"] != "read" {
			t.Fatalf("the undamaged sheet %s: %v", name, got)
		}
	}
	os.RemoveAll(undamaged)
	t.Logf("a process that reads %d undamaged sheets peaks at %d MiB", readerBatch, baseline>>20)

	subs := substitutions(t, b.e.dir, n)
	checked := 0
	for first := 0; first < len(subs); {
		dir := batch(fmt.Sprintf("batch%03d", first), nil)
		placed := map[string]substitution{}
		for ; first < len(subs) && len(placed) < readerBatch; first++ {
			s := subs[first]
			at := sheet(dir, first)
			if _, ok := s.apply(t, at); !ok {
				// Only an integer primary key, an alias of the row id,
				// cannot hold the value.
				if !isRowIDAlias(t, b.e.dir, s.Table, s.Column) {
					t.Fatalf("%s.%s cannot hold %s", s.Table, s.Column, s.Kind)
				}
				os.RemoveAll(at)
				continue
			}
			placed[filepath.Base(at)] = s
		}
		results, peak := peakReaders(t, dir)
		for name, s := range placed {
			got, ok := results[name]
			if !ok {
				t.Fatalf("no result for %s.%s holding %s", s.Table, s.Column, s.Kind)
			}
			wantRefused(t, s, got)
			checked++
		}
		t.Logf("a batch of %d damaged sheets peaks at %d MiB", len(placed), peak>>20)
		if peak > baseline+8<<20 {
			t.Errorf("the readers of %d sheets each with a value of %d MiB peaked at %d MiB, %d MiB for undamaged sheets: %v",
				len(placed), n>>20, peak>>20, baseline>>20, placed)
		}
		os.RemoveAll(dir)
	}
	t.Logf("%d oversized values in %d columns", checked, len(liveColumns(t, b.e.dir)))
}

// isRowIDAlias reports whether a column is an integer primary key of its
// table, which SQLite keeps as the row id.
func isRowIDAlias(t *testing.T, dir, table, column string) bool {
	t.Helper()
	keys := 0
	alias := false
	for _, c := range liveColumns(t, dir) {
		if c.table == table && c.pk {
			keys++
			alias = c.name == column && strings.EqualFold(c.typ, "INTEGER")
		}
	}
	return keys == 1 && alias
}

// groupsCloseDir names the sheet whose groups TestGroupsCloseInAProcessOfTheirOwn
// closes, and groupsCloseQuery the query it runs instead when set.
const groupsCloseDir, groupsCloseQuery = "FLATS_SHEET_GROUPS_CLOSE_DIR", "FLATS_SHEET_GROUPS_CLOSE_QUERY"

// TestGroupsCloseInAProcessOfTheirOwn closes the due groups of the
// sheet.db groupsCloseDir names as a tick does (closeGroups), or runs the
// query groupsCloseQuery names over its groups, and exits.
// TestGroupOrderStaysOutOfMemory runs it in a process of its own, whose
// peak resident memory it reads; it does nothing otherwise.
func TestGroupsCloseInAProcessOfTheirOwn(t *testing.T) {
	dir := os.Getenv(groupsCloseDir)
	if dir == "" {
		t.Skip("runs in a process that TestGroupOrderStaysOutOfMemory starts")
	}
	defer peakrss.Report(t)
	db, _, _, err := openDB(filepath.Join(dir, dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if query := os.Getenv(groupsCloseQuery); query != "" {
		var n int
		err = tx.QueryRowContext(ctx, `SELECT count(*) FROM (`+query+`)`).Scan(&n)
	} else {
		in := &Instance{now: time.Now}
		_, _, err = in.closeGroups(ctx, tx, false)
	}
	fmt.Println("close:", err)
}

// peakClose closes the groups of the sheet in dir in a process of its own,
// or runs the query, and returns what it printed and its peak resident
// memory in bytes.
func peakClose(t *testing.T, dir, query string) (string, int64) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestGroupsCloseInAProcessOfTheirOwn$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), groupsCloseDir+"="+dir, groupsCloseQuery+"="+query)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the process: %v\n%s", err, out)
	}
	_, line, _ := strings.Cut(string(out), "close: ")
	line, _, _ = strings.Cut(line, "\n")
	return line, peakrss.Bytes(t, cmd.ProcessState, out)
}

// A group whose editor is a text of 32 MiB is refused where the tick reads
// the groups, in the order the revision names their editors, and the
// process that closes them peaks within a few MiB of one that closes the
// groups without it: the sort of the groups holds no such editor. A sort
// by the editor as stored would hold all of it, which the same measurement
// sees.
func TestGroupOrderStaysOutOfMemory(t *testing.T) {
	if !measures {
		t.Skip("measures the memory of a process")
	}
	const n = 32 << 20
	b := newClassBase(t)
	base := copied(t, b.e, `UPDATE groups SET dirty=0`)
	_, baseline := peakClose(t, base.dir, "")
	damaged := copied(t, base, fmt.Sprintf(`INSERT INTO groups(editor,label,verified,started,last,dirty) VALUES(CAST(zeroblob(%d) AS TEXT),'Ann',0,0,0,0)`, n))
	got, peak := peakClose(t, damaged.dir, "")
	t.Logf("closing the groups: %s, a peak of %d MiB, %d MiB without the group", got, peak>>20, baseline>>20)
	if !strings.Contains(got, "groups.editor") {
		t.Errorf("closing the groups answered %q", got)
	}
	if peak > baseline+8<<20 {
		t.Errorf("closing the groups peaked at %d MiB, %d MiB without an editor of %d MiB", peak>>20, baseline>>20, n>>20)
	}
	_, raw := peakClose(t, damaged.dir, `SELECT label FROM groups ORDER BY started,editor`)
	t.Logf("a sort by the stored editor peaks at %d MiB", raw>>20)
	if raw < baseline+n/2 {
		t.Errorf("a sort by the stored editor peaked at %d MiB, %d MiB without it: the measurement does not see it", raw>>20, baseline>>20)
	}
}
