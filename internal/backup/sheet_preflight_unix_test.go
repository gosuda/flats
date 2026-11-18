//go:build unix

package backup

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sqliteuri"
	"github.com/gosuda/flats/internal/testutil/peakrss"
)

// The environment of TestVerifierInAProcessOfItsOwn: what it checks, how, and
// the damage it gives the copy of a backup it makes itself.
const (
	verifyMode    = "FLATS_BACKUP_VERIFY_MODE"
	verifyPath    = "FLATS_BACKUP_VERIFY_PATH"
	verifyOut     = "FLATS_BACKUP_VERIFY_OUT"
	verifyDamage  = "FLATS_BACKUP_VERIFY_DAMAGE"
	verifyControl = "FLATS_BACKUP_VERIFY_WITHOUT_PREFLIGHT"
)

// sheetRel is the sheet.db of the populated host, below its data directory.
const sheetRel = "flats/h/sheet/sheet.db"

// damage runs a statement on a sheet.db.
func damage(path, statement string) error {
	db, err := sql.Open("sqlite", sqliteuri.File(path))
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(statement)
	return err
}

// verifierResult is what TestVerifierInAProcessOfItsOwn found.
type verifierResult struct {
	Failures []Failure
	Notes    []string
	Refused  []Skip
	Error    string
}

// TestVerifierInAProcessOfItsOwn checks a copy with Verify, or makes one with
// Run and puts a file in place of the sheet.db of the copy before Run checks
// it, and prints what the check found. TestSheetValuesStayOutOfTheVerifiersMemory
// runs it in a process of its own, whose peak resident memory it reads; it
// does nothing otherwise.
func TestVerifierInAProcessOfItsOwn(t *testing.T) {
	mode := os.Getenv(verifyMode)
	if mode == "" {
		t.Skip("runs in a process that TestSheetValuesStayOutOfTheVerifiersMemory starts")
	}
	defer peakrss.Report(t)
	if os.Getenv(verifyControl) != "" {
		preflightSheet = func(context.Context, string) error { return nil }
	}
	ctx := context.Background()
	var res verifierResult
	switch mode {
	case "verify":
		got, err := Verify(ctx, os.Getenv(verifyPath))
		if err != nil {
			t.Fatal(err)
		}
		res.Failures, res.Notes = got.Failures, got.Notes
	case "backup":
		data := os.Getenv(verifyPath)
		got, err := Run(ctx, Options{DataDir: data, ConfigPath: filepath.Join(data, nameConfig), Out: os.Getenv(verifyOut), FlatsVersion: "vtest"})
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Refused = got.Refused
		}
	case "outbox":
		if _, err := readOutbox(ctx, os.Getenv(verifyPath), typeSheet); err != nil {
			res.Error = err.Error()
		}
	case "min":
		// The read of the outbox's oldest entry before it was bounded.
		db, err := openCopy(os.Getenv(verifyPath))
		if err != nil {
			t.Fatal(err)
		}
		var oldest any
		err = db.QueryRowContext(ctx, `SELECT MIN(seq) FROM outbox`).Scan(&oldest)
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
	case "run":
		data, out := os.Getenv(verifyPath), os.Getenv(verifyOut)
		o := Options{DataDir: data, ConfigPath: filepath.Join(data, nameConfig), Out: out, FlatsVersion: "vtest"}
		o.afterStep = func(step int) error {
			if step != 19 {
				return nil
			}
			partials, err := filepath.Glob(out + ".partial-*")
			if err != nil || len(partials) != 1 {
				return fmt.Errorf("the copy in progress: %v, %v", partials, err)
			}
			return copyFile(os.Getenv(verifyDamage), filepath.Join(partials[0], "data", filepath.FromSlash(sheetRel)))
		}
		if _, err := Run(ctx, o); err != nil {
			res.Error = err.Error()
		}
	}
	line, _ := json.Marshal(res)
	fmt.Printf("verifier result %s\n", line)
}

// copyFile puts the bytes of the file from in place of the file to.
func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		return errors.Join(err, dst.Close())
	}
	return dst.Close()
}

// runVerifier runs TestVerifierInAProcessOfItsOwn in a process of its own and
// returns what it found and the process's peak resident memory in bytes.
func runVerifier(t *testing.T, env ...string) (verifierResult, int64) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestVerifierInAProcessOfItsOwn$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the verifier's process: %v\n%s", err, out)
	}
	var res verifierResult
	found := false
	for scan := bufio.NewScanner(bytes.NewReader(out)); scan.Scan(); {
		if rest, ok := strings.CutPrefix(scan.Text(), "verifier result "); ok {
			found = true
			if err := json.Unmarshal([]byte(rest), &res); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !found {
		t.Fatalf("the verifier's process printed no result:\n%s", out)
	}
	return res, peakrss.Bytes(t, cmd.ProcessState, out)
}

// A copied sheet.db with a text of 16 MiB in an integer column of meta or of
// a cell, or a blob of 16 MiB in the number column of a cell, fails the check
// of the copy, in a standalone Verify and in the check a backup makes of its
// own copy, and the peak memory of the process stays within a few MiB of the
// check of an undamaged copy: the sheet's preflight refuses the value before
// quick_check, which reads a text in a column of numbers whole inside SQLite.
// Without the preflight, the peak of the same checks of such a text rises by
// more than the value, which shows that the measurement sees it.
func TestSheetValuesStayOutOfTheVerifiersMemory(t *testing.T) {
	if raceEnabled {
		t.Skip("measures the memory of a process")
	}
	const n = 16 << 20
	// read says whether quick_check reads the value whole: a text in a column
	// of numeric affinity. It does not read a blob in num in this SQLite, which
	// the preflight refuses all the same.
	cases := map[string]struct {
		statement string
		read      bool
	}{
		"a text in meta.seq":      {fmt.Sprintf(`UPDATE meta SET seq = CAST(zeroblob(%d) AS TEXT)`, n), true},
		"a text in a cell's kind": {fmt.Sprintf(`INSERT INTO cells(tab, row, col, kind, num, seq) VALUES(1, 1, 1, CAST(zeroblob(%d) AS TEXT), NULL, 1)`, n), true},
		"a text in a cell's num":  {fmt.Sprintf(`INSERT INTO cells(tab, row, col, kind, num, seq) VALUES(1, 1, 1, 1, CAST(zeroblob(%d) AS TEXT), 1)`, n), true},
		"a blob in a cell's num":  {fmt.Sprintf(`INSERT INTO cells(tab, row, col, kind, num, seq) VALUES(1, 1, 1, 1, zeroblob(%d), 1)`, n), false},
	}
	f, out := goodCopy(t)
	copied := filepath.Join(out, "data", filepath.FromSlash(sheetRel))
	pristine := filepath.Join(t.TempDir(), "sheet.db")
	if err := copyFile(copied, pristine); err != nil {
		t.Fatal(err)
	}
	runs := 0
	backup := func(file string, env ...string) (verifierResult, int64) {
		t.Helper()
		runs++
		return runVerifier(t, append(env, verifyMode+"=run", verifyPath+"="+f.data,
			verifyOut+"="+filepath.Join(f.out, fmt.Sprintf("r%d", runs)), verifyDamage+"="+file)...)
	}
	res, baseline := runVerifier(t, verifyMode+"=verify", verifyPath+"="+out)
	if len(res.Failures) > 0 {
		t.Fatalf("the undamaged copy: %v", res.Failures)
	}
	res, runBaseline := backup(pristine)
	if res.Error != "" {
		t.Fatalf("a backup with the copied sheet.db: %s", res.Error)
	}
	t.Logf("undamaged: %d MiB standalone, %d MiB in a backup", baseline>>20, runBaseline>>20)
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if err := copyFile(pristine, copied); err != nil {
				t.Fatal(err)
			}
			if err := damage(copied, c.statement); err != nil {
				t.Fatal(err)
			}
			damaged := filepath.Join(t.TempDir(), "sheet.db")
			if err := copyFile(copied, damaged); err != nil {
				t.Fatal(err)
			}
			rehash(t, out)
			res, peak := runVerifier(t, verifyMode+"=verify", verifyPath+"="+out)
			wantFailure(t, res.Failures, "data/"+sheetRel, "sheet check before quick_check")
			t.Logf("standalone: refused at a peak of %d MiB", peak>>20)
			if peak > baseline+8<<20 {
				t.Errorf("the standalone check took %d MiB, %d MiB for the undamaged copy", peak>>20, baseline>>20)
			}
			_, control := runVerifier(t, verifyMode+"=verify", verifyPath+"="+out, verifyControl+"=1")
			t.Logf("standalone without the preflight: a peak of %d MiB", control>>20)
			if c.read && control < baseline+n {
				t.Errorf("without the preflight the check took %d MiB, %d MiB for the undamaged copy: the measurement does not see the value", control>>20, baseline>>20)
			}
			res, own := backup(damaged)
			if !strings.Contains(res.Error, sheetRel) {
				t.Fatalf("the backup answered %q, not a failure of its sheet.db", res.Error)
			}
			t.Logf("in a backup: refused at a peak of %d MiB: %s", own>>20, res.Error)
			if own > runBaseline+8<<20 {
				t.Errorf("the check of a backup took %d MiB, %d MiB for an undamaged copy", own>>20, runBaseline>>20)
			}
			_, ownControl := backup(damaged, verifyControl+"=1")
			t.Logf("in a backup without the preflight: a peak of %d MiB", ownControl>>20)
			if c.read && ownControl < runBaseline+n {
				t.Errorf("without the preflight a backup took %d MiB, %d MiB for an undamaged copy: the measurement does not see the value", ownControl>>20, runBaseline>>20)
			}
		})
	}
}

// A copied sheet.db whose outbox is a table of another schema, a sequence
// column of blobs holding one of 16 MiB, fails the sheet check of the copy,
// in a standalone Verify and in the check a backup makes of its own copy,
// and no later check reads it: the peak memory of the process stays within a
// few MiB of the check of an undamaged copy. readOutbox, which the copy also
// runs on the sheet.db of the live host, refuses the outbox without reading
// the blob. The read of its oldest sequence as it was takes more than the
// value, which shows that the measurement sees it.
func TestARejectedSheetStaysOutOfTheVerifiersMemory(t *testing.T) {
	if raceEnabled {
		t.Skip("measures the memory of a process")
	}
	const n = 16 << 20
	f, out := goodCopy(t)
	copied := filepath.Join(out, "data", filepath.FromSlash(sheetRel))
	pristine := filepath.Join(t.TempDir(), "sheet.db")
	if err := copyFile(copied, pristine); err != nil {
		t.Fatal(err)
	}
	res, baseline := runVerifier(t, verifyMode+"=verify", verifyPath+"="+out)
	if len(res.Failures) > 0 {
		t.Fatalf("the undamaged copy: %v", res.Failures)
	}
	res, runBaseline := runVerifier(t, verifyMode+"=run", verifyPath+"="+f.data, verifyOut+"="+filepath.Join(f.out, "r0"), verifyDamage+"="+pristine)
	if res.Error != "" {
		t.Fatalf("a backup with the copied sheet.db: %s", res.Error)
	}
	res, outboxBaseline := runVerifier(t, verifyMode+"=outbox", verifyPath+"="+pristine)
	if res.Error != "" {
		t.Fatalf("the outbox of the undamaged copy: %s", res.Error)
	}
	_, minBaseline := runVerifier(t, verifyMode+"=min", verifyPath+"="+pristine)
	for _, statement := range []string{`DROP TABLE outbox`, `CREATE TABLE outbox (seq BLOB)`, fmt.Sprintf(`INSERT INTO outbox VALUES (zeroblob(%d))`, n)} {
		if err := damage(copied, statement); err != nil {
			t.Fatal(err)
		}
	}
	damaged := filepath.Join(t.TempDir(), "sheet.db")
	if err := copyFile(copied, damaged); err != nil {
		t.Fatal(err)
	}
	rehash(t, out)

	res, peak := runVerifier(t, verifyMode+"=verify", verifyPath+"="+out)
	wantFailure(t, res.Failures, "data/"+sheetRel, "sheet check before quick_check")
	t.Logf("standalone: refused at a peak of %d MiB, %d MiB for the undamaged copy", peak>>20, baseline>>20)
	if peak > baseline+8<<20 {
		t.Errorf("the standalone check took %d MiB, %d MiB for the undamaged copy", peak>>20, baseline>>20)
	}
	res, own := runVerifier(t, verifyMode+"=run", verifyPath+"="+f.data, verifyOut+"="+filepath.Join(f.out, "r1"), verifyDamage+"="+damaged)
	if !strings.Contains(res.Error, sheetRel) {
		t.Fatalf("the backup answered %q, not a failure of its sheet.db", res.Error)
	}
	t.Logf("in a backup: refused at a peak of %d MiB, %d MiB for an undamaged copy: %s", own>>20, runBaseline>>20, res.Error)
	if own > runBaseline+8<<20 {
		t.Errorf("the check of a backup took %d MiB, %d MiB for an undamaged copy", own>>20, runBaseline>>20)
	}
	res, read := runVerifier(t, verifyMode+"=outbox", verifyPath+"="+damaged)
	if !strings.Contains(res.Error, "not a number") {
		t.Errorf("readOutbox answered %q for an outbox of blobs", res.Error)
	}
	t.Logf("readOutbox: refused at a peak of %d MiB, %d MiB for the undamaged copy", read>>20, outboxBaseline>>20)
	if read > outboxBaseline+8<<20 {
		t.Errorf("readOutbox took %d MiB, %d MiB for the undamaged copy", read>>20, outboxBaseline>>20)
	}
	_, control := runVerifier(t, verifyMode+"=min", verifyPath+"="+damaged)
	t.Logf("the oldest sequence read unbounded: a peak of %d MiB, %d MiB for the undamaged copy", control>>20, minBaseline>>20)
	if control < minBaseline+n {
		t.Errorf("the unbounded read took %d MiB, %d MiB for the undamaged copy: the measurement does not see the value", control>>20, minBaseline>>20)
	}
}

// A live sheet.db with a text of 16 MiB in the number column of a cell, which
// the sheet check refuses and a host therefore keeps closed, is copied by a
// backup as it is and listed as refused: the backup succeeds, and no later
// step reads that sheet.db's rows. The peak memory of the backup does not grow
// with the value: it is below a backup of the undamaged host and the value,
// and a text of 64 MiB takes no more than one of 16 MiB, within a few MiB, as
// VACUUM INTO moves a row's cells without reading the value. In the order of before, with the snapshot taken
// for a good one, the check of the copy runs quick_check on it and takes more
// than the value, which shows that the measurement sees it. The copy holds
// the live file's rows, and its own check notes the refusal. A backup of the
// undamaged host lists nothing as refused, and its copy passes the check.
func TestARefusedLiveSheetIsCopiedAsItIs(t *testing.T) {
	if raceEnabled {
		t.Skip("measures the memory of a process")
	}
	const n = 16 << 20
	f := populated(t)
	live := filepath.Join(f.data, filepath.FromSlash(sheetRel))
	backup := func(name string, env ...string) (verifierResult, int64, string) {
		t.Helper()
		out := filepath.Join(f.out, name)
		res, peak := runVerifier(t, append(env, verifyMode+"=backup", verifyPath+"="+f.data, verifyOut+"="+out)...)
		if res.Error != "" {
			t.Fatalf("the backup %s: %s", name, res.Error)
		}
		return res, peak, out
	}
	res, baseline, out := backup("undamaged")
	if len(res.Refused) > 0 {
		t.Fatalf("the backup of the undamaged host refused %v", res.Refused)
	}
	if raw, err := os.ReadFile(filepath.Join(out, ManifestFile)); err != nil || bytes.Contains(raw, []byte(`"refused"`)) {
		t.Fatalf("the manifest of the undamaged host names refused files, or cannot be read: %v", err)
	}
	if got, err := Verify(context.Background(), out); err != nil || len(got.Failures) > 0 || len(got.Notes) > 0 && strings.Contains(strings.Join(got.Notes, " "), "sheet") {
		t.Fatalf("the copy of the undamaged host: %+v, %v", got, err)
	}
	if err := damage(live, fmt.Sprintf(`INSERT INTO cells(tab, row, col, kind, num, seq) VALUES(1, 1, 1, 1, CAST(zeroblob(%d) AS TEXT), 1)`, n)); err != nil {
		t.Fatal(err)
	}
	res, peak, out := backup("damaged")
	if len(res.Refused) != 1 || res.Refused[0].Path != "data/"+sheetRel {
		t.Fatalf("the backup of the damaged host refused %v", res.Refused)
	}
	t.Logf("a backup of the damaged host: a peak of %d MiB, %d MiB for the undamaged host; refused %v", peak>>20, baseline>>20, res.Refused)
	if peak >= baseline+n {
		t.Errorf("the backup of the damaged host took %d MiB, %d MiB for the undamaged host", peak>>20, baseline>>20)
	}
	if err := damage(live, fmt.Sprintf(`UPDATE cells SET num = CAST(zeroblob(%d) AS TEXT) WHERE typeof(num) = 'text'`, 4*n)); err != nil {
		t.Fatal(err)
	}
	res, larger, _ := backup("larger")
	t.Logf("with a text of %d MiB: a peak of %d MiB", 4*n>>20, larger>>20)
	if len(res.Refused) != 1 || larger > peak+8<<20 {
		t.Errorf("with a text of %d MiB the backup took %d MiB and refused %v, %d MiB with one of %d MiB", 4*n>>20, larger>>20, res.Refused, peak>>20, n>>20)
	}
	if err := damage(live, fmt.Sprintf(`UPDATE cells SET num = CAST(zeroblob(%d) AS TEXT) WHERE typeof(num) = 'text'`, n)); err != nil {
		t.Fatal(err)
	}
	_, control, _ := backup("unchecked", verifyControl+"=1")
	t.Logf("without the sheet check: a peak of %d MiB", control>>20)
	if control < baseline+n {
		t.Errorf("without the sheet check the backup took %d MiB, %d MiB for the undamaged host: the measurement does not see the value", control>>20, baseline>>20)
	}
	check, checkPeak := runVerifier(t, verifyMode+"=verify", verifyPath+"="+out)
	if len(check.Failures) > 0 || !strings.Contains(strings.Join(check.Notes, " "), sheetRel) {
		t.Fatalf("the check of the copy: %v, notes %v", check.Failures, check.Notes)
	}
	t.Logf("its check: a peak of %d MiB: %v", checkPeak>>20, check.Notes)
	if checkPeak >= baseline+n {
		t.Errorf("the check of the copy took %d MiB, %d MiB for a backup of the undamaged host", checkPeak>>20, baseline>>20)
	}
	if !sameRows(t, live, filepath.Join(out, "data", filepath.FromSlash(sheetRel))) {
		t.Error("the copy does not hold the rows of the live sheet.db")
	}
}

// sameRows reports whether two sheet.db files hold the same cells.
func sameRows(t *testing.T, a, b string) bool {
	t.Helper()
	digest := func(path string) [32]byte {
		db, err := openCopy(path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rows, err := db.Query(`SELECT tab, row, col, kind, quote(num) FROM cells ORDER BY tab, row, col`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		h := sha256.New()
		for rows.Next() {
			var tab, row, col int64
			var kind, num any
			if err := rows.Scan(&tab, &row, &col, &kind, &num); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(h, "%d,%d,%d,%v,%v;", tab, row, col, kind, num)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return [32]byte(h.Sum(nil))
	}
	return digest(a) == digest(b)
}
