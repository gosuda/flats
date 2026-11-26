package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/backup"
	"github.com/gosuda/flats/internal/board"
	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/sheet"
	"github.com/gosuda/flats/internal/sqliteuri"
)

func runBackup(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := BackupCommand(context.Background(), args, &out)
	return out.String(), err
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
}

// writer applies writes to one flat until it is told to stop. It records how many the host
// acknowledged and the sequence number of the last one.
type writer struct {
	acked, lastSeq atomic.Int64
	write          func(n int64) (seq int64, err error)
}

func (w *writer) run(t *testing.T, stop <-chan struct{}, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := int64(1); ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			seq, err := w.write(n)
			if err != nil {
				t.Errorf("write %d: %v", n, err)
				return
			}
			w.lastSeq.Store(seq)
			w.acked.Add(1)
		}
	}()
}

func boardOps(t *testing.T, raw string) []engine.AgentOp {
	t.Helper()
	var ops []engine.AgentOp
	if err := json.Unmarshal([]byte(raw), &ops); err != nil {
		t.Fatal(err)
	}
	return ops
}

// writeBoard adds one sticky note to the board and returns the board's sequence number.
func writeBoard(t *testing.T, h *Host, slug string, n int64) (int64, error) {
	res, err := h.Svc.UpdateBoard(context.Background(), slug, core.BoardUpdate{
		OpID: fmt.Sprintf("board-op-%07d", n),
		Ops:  boardOps(t, fmt.Sprintf(`[{"op":"add","el":{"type":"sticky","text":"note %d"}}]`, n)),
	}, core.ViaMCP)
	return res.Seq, err
}

// writeNotebook adds one page to the notebook and returns its revision number, which is 0 while the revision is pending.
func writeNotebook(h *Host, slug string, n int64) (int64, error) {
	payload := fmt.Sprintf(`{"op_id":"notebook-op-%07d","ops":[{"op":"create_page","ref":"page","parent":null,"before":null,"title":"Page %d"}]}`, n, n)
	res, err := h.Svc.UpdateNotebook(context.Background(), slug, []byte(payload), core.ViaMCP)
	return int64(res.Revision), err
}

// readSheet reads a sheet as get_sheet does. A sheet whose workbook still loads, as after a start of the host,
// refuses the read for that time, and readSheet tries again.
func readSheet(ctx context.Context, h *Host, slug, query string) (map[string]any, error) {
	deadline := time.Now().Add(60 * time.Second)
	for {
		raw, err := h.Svc.GetSheet(ctx, slug, 0, []byte(query))
		var problem *sheet.WriteError
		if errors.As(err, &problem) && problem.Code == "unavailable" && problem.Reason == "calculating" && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if err != nil {
			return nil, err
		}
		var out map[string]any
		return out, json.Unmarshal(raw, &out)
	}
}

// writeSheet applies operations to the current state of a sheet, as an agent's update_sheet does.
func writeSheet(ctx context.Context, h *Host, slug, opID, ops string) (core.SheetWriteResult, error) {
	list, err := readSheet(ctx, h, slug, `{}`)
	if err != nil {
		return core.SheetWriteResult{}, err
	}
	body := fmt.Sprintf(`{"state":%q,"op_id":%q,"ops":[%s]}`, list["state"], opID, ops)
	return h.Svc.UpdateSheet(ctx, slug, []byte(body), core.ViaMCP)
}

// tallyCell is the cell that the n-th write of the sheet writer fills: the tab has 26 columns.
func tallyCell(n int64) string {
	return fmt.Sprintf("%c%d", 'A'+rune((n-1)%26), (n-1)/26+1)
}

// writeTally fills one more cell of the sheet and returns the revision of the write, which is 0 while it is pending.
func writeTally(ctx context.Context, h *Host, slug string, n int64) (int64, error) {
	res, err := writeSheet(ctx, h, slug, fmt.Sprintf("tally-op-%07d", n),
		fmt.Sprintf(`{"op":"write","tab":"Sheet1","at":%q,"rows":[[%d]]}`, tallyCell(n), n))
	return int64(res.Revision), err
}

// sheetCells counts the filled cells of a tab, following every continuation.
func sheetCells(ctx context.Context, h *Host, slug, tab string) (int64, error) {
	var n int64
	query := fmt.Sprintf(`{"tab":%q}`, tab)
	for {
		page, err := readSheet(ctx, h, slug, query)
		if err != nil {
			return n, err
		}
		rows, _ := page["rows"].([]any)
		for _, row := range rows {
			cells, _ := row.([]any)
			for _, c := range cells {
				if c != nil {
					n++
				}
			}
		}
		next, _ := page["next"].(string)
		if next == "" {
			return n, nil
		}
		query = fmt.Sprintf(`{"cursor":%q}`, next)
	}
}

// sheetContent reads everything that a sheet holds for its readers: the tabs in their order with their
// settings, and for each tab the cells with their formulas, the values as they are shown, the formats and the
// column widths. It leaves out the state token and the calculation date, which name a moment and not content.
func sheetContent(t *testing.T, ctx context.Context, h *Host, slug string) string {
	t.Helper()
	list, err := readSheet(ctx, h, slug, `{}`)
	if err != nil {
		t.Fatalf("read the tabs of %s: %v", slug, err)
	}
	content := map[string]any{"tabs": list["tabs"]}
	tabs, _ := list["tabs"].([]any)
	for _, tab := range tabs {
		name, _ := tab.(map[string]any)["name"].(string)
		page, err := readSheet(ctx, h, slug, fmt.Sprintf(`{"tab":%q,"include":["shown","formats","widths"]}`, name))
		if err != nil {
			t.Fatalf("read the tab %s of %s: %v", name, slug, err)
		}
		if page["next"] != nil {
			t.Fatalf("the tab %s of %s does not fit one read", name, slug)
		}
		delete(page, "state")
		delete(page, "date")
		content["tab "+name] = page
	}
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// queryCopy reads one number from a database file that nothing has open, without changing it.
func queryCopy(path, query string) (int64, error) {
	db, err := sql.Open("sqlite", sqliteuri.File(path)+"?mode=ro&immutable=1")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var n int64
	err = db.QueryRow(query).Scan(&n)
	return n, err
}

// restoreFrom moves the data directory away and puts the copy's data directory in its place,
// at the same path, as docs/spec/host-backup.md rule 35 says.
func restoreFrom(t *testing.T, dir, copyDir string) {
	t.Helper()
	if err := os.Rename(dir, dir+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	copyTree(t, filepath.Join(copyDir, "data"), dir)
}

// boardElements counts the elements of a board's current content, following every continuation.
func boardElements(ctx context.Context, h *Host, slug string) (int64, error) {
	var n int64
	cursor := ""
	for {
		page, err := h.Svc.GetBoard(ctx, slug, 0, board.Query{Limit: 2000, Cursor: cursor})
		if err != nil {
			return n, err
		}
		n += int64(len(page.Elements))
		if page.Next == "" {
			return n, nil
		}
		cursor = page.Next
	}
}

// notebookPages counts the pages that writeNotebook made, following every continuation.
func notebookPages(ctx context.Context, h *Host, slug string) (int64, error) {
	var n int64
	query := []byte(`{}`)
	for {
		raw, err := h.Svc.GetNotebook(ctx, slug, 0, query)
		if err != nil {
			return n, err
		}
		var page struct {
			Tree []struct {
				Title string `json:"title"`
			} `json:"tree"`
			Next *string `json:"next"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return n, err
		}
		for _, p := range page.Tree {
			if strings.HasPrefix(p.Title, "Page ") {
				n++
			}
		}
		if page.Next == nil || *page.Next == "" {
			return n, nil
		}
		query, _ = json.Marshal(map[string]string{"cursor": *page.Next})
	}
}

func TestBackupOfARunningHostRestoresToAWorkingHost(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path, key, h := hostWithSecrets(t, dir)
	if _, err := h.Svc.CreateBoard(ctx, "pad", "Pad", core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Svc.CreateNotebook(ctx, "notes", "Notes", core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	// A server flat's own database and files, as its worker leaves them: nothing holds them open.
	serverDB := filepath.Join(dir, "flats", "app", "data", "db.sqlite")
	if err := os.MkdirAll(filepath.Join(dir, "flats", "app", "data", "files"), 0o700); err != nil {
		t.Fatal(err)
	}
	(&fixtureDB{t: t}).exec(serverDB, `CREATE TABLE items(a)`, `INSERT INTO items VALUES(1)`)
	if err := os.WriteFile(filepath.Join(dir, "flats", "app", "data", "files", "notes.txt"), []byte("guest file"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A sheet that is written during the backup, and a sheet that nothing writes then: it has cells, a formula,
	// formats, a column width, a frozen row and a second tab in imported revisions, and one more write whose
	// revision the host could not import, so that its entry waits in the outbox of sheet.db.
	for slug, name := range map[string]string{"tally": "Tally", "ledger": "Ledger"} {
		if _, err := h.Svc.CreateSheet(ctx, slug, name, core.ViaMCP); err != nil {
			t.Fatal(err)
		}
	}
	if res, err := writeSheet(ctx, h, "ledger", "ledger-content", strings.Join([]string{
		`{"op":"write","tab":"Sheet1","at":"A1","rows":[["Item","Amount","Paid"],["Venue",1200,true],["Food",900.5,false],["Total","=SUM(B2:B3)",null]]}`,
		`{"op":"format","tab":"Sheet1","ranges":["B2:B4"],"format":{"number":{"kind":"currency","symbol":"$","decimals":2}}}`,
		`{"op":"widths","tab":"Sheet1","cols":"A:A","width":180}`,
		`{"op":"tab_settings","tab":"Sheet1","frozen_rows":1}`,
		`{"op":"add_tab","name":"Notes"}`,
		`{"op":"write","tab":"Notes","at":"A1","rows":[["Costs of the spring meeting"]]}`,
	}, ",")); err != nil || res.Revision != 2 || res.Pending {
		t.Fatalf("the write of the sheet = %+v, %v; want revision 2", res, err)
	}
	// An import that fails keeps its entry in the outbox, and the host tries it again later. It fails here
	// because the host cannot store a new blob: a file is in the place of the staging directory of the blobs.
	staging := filepath.Join(dir, "flats", "ledger", "revisions", "tmp")
	if err := os.RemoveAll(staging); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staging, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if res, err := writeSheet(ctx, h, "ledger", "ledger-pending", strings.Join([]string{
		`{"op":"write","tab":"Sheet1","at":"A4","rows":[["Drinks",310],["Total","=SUM(B2:B4)"]]}`,
		`{"op":"write","tab":"Notes","at":"A2","rows":[["Drinks were added late"]]}`,
	}, ",")); err != nil || !res.Pending || res.Revision != 0 {
		t.Fatalf("the write whose import fails = %+v, %v; want a pending revision", res, err)
	}
	ledgerBefore := sheetContent(t, ctx, h, "ledger")
	// The commits of the sheet are in sheet.db-wal, and the file sheet.db alone does not hold the waiting entry.
	ledgerDB := filepath.Join(dir, "flats", "ledger", "sheet", "sheet.db")
	if fi, err := os.Stat(ledgerDB + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("the sheet has no commits in a -wal file: %v, %v", fi, err)
	}
	alone := filepath.Join(t.TempDir(), "sheet.db")
	if raw, err := os.ReadFile(ledgerDB); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(alone, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := queryCopy(alone, `SELECT count(*) FROM outbox`); err == nil && n > 0 {
		t.Fatalf("the file sheet.db alone holds %d outbox entries; the test needs them in the -wal file", n)
	}

	boardW := &writer{write: func(n int64) (int64, error) { return writeBoard(t, h, "pad", n) }}
	notebookW := &writer{write: func(n int64) (int64, error) { return writeNotebook(h, "notes", n) }}
	sheetW := &writer{write: func(n int64) (int64, error) { return writeTally(ctx, h, "tally", n) }}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	boardW.run(t, stop, &wg)
	notebookW.run(t, stop, &wg)
	sheetW.run(t, stop, &wg)
	deadline := time.Now().Add(30 * time.Second)
	for boardW.acked.Load() < 5 || notebookW.acked.Load() < 5 || sheetW.acked.Load() < 5 {
		if time.Now().After(deadline) {
			t.Fatal("the writers did not get going")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The writes that the host acknowledged before the backup began must be in the restored host.
	boardSeq, sheetRevision := boardW.lastSeq.Load(), sheetW.lastSeq.Load()
	boardBefore, notebookBefore, sheetBefore := boardW.acked.Load(), notebookW.acked.Load(), sheetW.acked.Load()

	out := filepath.Join(t.TempDir(), "copy")
	text, err := runBackup(t, "--out", out, "--config", path)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("backup: %v\n%s", err, text)
	}
	checkSheetsInTheCopy(t, out)
	canonicalOut, err := filepath.EvalSymlinks(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"backed up ", " files, ", " bytes", "secret.key is in the copy", " retries, ", "attempt(s)", "Keep it as protected", "flats backup --verify " + canonicalOut} {
		if !strings.Contains(text, want) {
			t.Errorf("the output lacks %q:\n%s", want, text)
		}
	}
	if text, err := runBackup(t, "--verify", out); err != nil || !strings.Contains(text, "ok: ") || !strings.Contains(text, "was made by Flats") {
		t.Fatalf("verify: %v\n%s", err, text)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	restoreFrom(t, dir, out)
	h2, err := Start(ctx, configModeOptions(path))
	if err != nil {
		t.Fatalf("the restored host does not start: %v", err)
	}
	t.Cleanup(func() { h2.Close() })
	if check, err := core.CheckKey(ctx, h2.Store, key); err != nil || !check.All() || check.Total != 2 {
		t.Errorf("the key opens %+v, %v; want the flat secret and the operator key", check, err)
	}
	names := map[string]bool{}
	flats, err := h2.Svc.ListFlats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range flats {
		names[f.Slug] = true
	}
	for _, s := range []string{"app", "pad", "notes", "tally", "ledger"} {
		if !names[s] {
			t.Errorf("flat %s is missing after the restore: %v", s, names)
		}
	}
	elements, err := boardElements(ctx, h2, "pad")
	if err != nil || elements < boardBefore {
		t.Errorf("the restored board holds %d elements, %v; the host had acknowledged %d before the backup began", elements, err, boardBefore)
	}
	pages, err := notebookPages(ctx, h2, "notes")
	if err != nil || pages < notebookBefore {
		t.Errorf("the restored notebook holds %d pages, %v; the host had acknowledged %d before the backup began", pages, err, notebookBefore)
	}
	t.Logf("restored: %d board elements (>= %d acknowledged before the backup), %d notebook pages (>= %d)", elements, boardBefore, pages, notebookBefore)
	// The next write must not reuse a number that the copy had already passed.
	seq, err := writeBoard(t, h2, "pad", 1_000_000)
	if err != nil || seq <= boardSeq {
		t.Errorf("a write after the restore has sequence %d, %v; the copy was taken after %d", seq, err, boardSeq)
	}
	if _, err := writeNotebook(h2, "notes", 1_000_000); err != nil {
		t.Errorf("a notebook write after the restore: %v", err)
	}
	cells, err := sheetCells(ctx, h2, "tally", "Sheet1")
	if err != nil || cells < sheetBefore {
		t.Errorf("the restored sheet holds %d cells, %v; the host had acknowledged %d before the backup began", cells, err, sheetBefore)
	}
	t.Logf("restored: %d sheet cells (>= %d acknowledged before the backup)", cells, sheetBefore)
	// The revisions of a sheet take their numbers from flats.db, and the copy held the one of that write.
	if rev, err := writeTally(ctx, h2, "tally", 26_000); err != nil || rev <= sheetRevision {
		t.Errorf("a sheet write after the restore has revision %d, %v; the copy was taken after revision %d", rev, err, sheetRevision)
	}
	// The sheet reads as it did: cells, formulas, shown values, formats, widths and tabs.
	if got := sheetContent(t, ctx, h2, "ledger"); got != ledgerBefore {
		t.Errorf("the restored sheet reads\n%s\nand the host had\n%s", got, ledgerBefore)
	}
	// The entry that waited in the outbox of the copy becomes the revision that the host could not record.
	page, err := h2.Svc.ListRevisions(ctx, "ledger", 0, 10)
	if err != nil || len(page.Revisions) != 3 || page.Revisions[0].Number != 3 || page.Revisions[0].Kind != "agent" {
		t.Errorf("the revisions of the restored sheet = %+v, %v; want the waiting entry as revision 3", page.Revisions, err)
	}
	if res, err := writeSheet(ctx, h2, "ledger", "ledger-after", `{"op":"write","tab":"Notes","at":"A3","rows":[["Checked after the restore"]]}`); err != nil || res.Revision != 4 || res.Pending {
		t.Errorf("a write to the restored sheet = %+v, %v; want revision 4", res, err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "flats", "app", "data", "files", "notes.txt"))
	if err != nil || string(got) != "guest file" {
		t.Errorf("the server flat's file after the restore: %q %v", got, err)
	}
}

// checkSheetsInTheCopy checks how a copy holds the sheets of the host: each sheet.db is a database of the kind
// sheet and has no -wal or -shm file beside it, the scratch files of the undo history are not there, and the
// sheet that nothing wrote has the revisions that the host imported and the entry that waited.
func checkSheetsInTheCopy(t *testing.T, out string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(out, backup.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var m backup.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, d := range m.Databases {
		kinds[d.Path] = d.Kind
	}
	excluded := map[string]bool{}
	for _, p := range m.Excluded {
		excluded[p] = true
	}
	for _, slug := range []string{"tally", "ledger"} {
		db := "data/flats/" + slug + "/sheet/sheet.db"
		if kinds[db] != backup.KindSheet {
			t.Errorf("the manifest lists %s with the kind %q, want %q", db, kinds[db], backup.KindSheet)
		}
		for _, suffix := range []string{"-wal", "-shm"} {
			if _, err := os.Lstat(filepath.Join(out, filepath.FromSlash(db)+suffix)); !os.IsNotExist(err) {
				t.Errorf("the copy holds %s%s: %v", db, suffix, err)
			}
			if !excluded[db+suffix] {
				t.Errorf("the manifest does not list %s%s as excluded", db, suffix)
			}
		}
	}
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, "data/cache/") || strings.HasSuffix(f.Path, "-wal") || strings.HasSuffix(f.Path, "-shm") {
			t.Errorf("the copy holds %s", f.Path)
		}
	}
	if !excluded["data/cache"] {
		t.Errorf("the manifest does not list data/cache, which holds the scratch files of the sheets, as excluded")
	}
	// The sheet that nothing wrote: two revisions are imported, and the third waits in the outbox with the
	// bytes of its new files, which the host could not store as blobs.
	flatsDB := filepath.Join(out, "data", "flats.db")
	ledgerDB := filepath.Join(out, "data", "flats", "ledger", "sheet", "sheet.db")
	for _, q := range []struct {
		path, query string
		want        int64
	}{
		{flatsDB, `SELECT count(*) FROM sheet_imports WHERE flat = 'ledger'`, 2},
		{flatsDB, `SELECT import_seq FROM content_state WHERE flat = 'ledger'`, 2},
		{ledgerDB, `SELECT count(*) FROM outbox`, 1},
		{ledgerDB, `SELECT min(seq) FROM outbox`, 3},
		{ledgerDB, `SELECT seq FROM sqlite_sequence WHERE name = 'outbox'`, 3},
		{ledgerDB, `SELECT count(*) > 0 FROM snapshots WHERE refs > 0`, 1},
		{ledgerDB, `SELECT count(*) FROM tabs`, 2},
		// The snapshot keeps what the sheet set when it made the database: its schema version and incremental auto-vacuum.
		{ledgerDB, `PRAGMA user_version`, 1},
		{ledgerDB, `PRAGMA auto_vacuum`, 2},
	} {
		if got, err := queryCopy(q.path, q.query); err != nil || got != q.want {
			t.Errorf("%s in the copy: %s = %d, %v; want %d", filepath.Base(q.path), q.query, got, err, q.want)
		}
	}
}

func TestBackupWithoutTheSecretKeyRestoresWithTheRecoveryPassword(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path, key, h := hostWithSecrets(t, dir)
	pw := passwordFile(t, "a long recovery password\n")
	if _, err := runSecretKey(t, path, "backup-key", "--password-file", pw); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "copy")
	text, err := runBackup(t, "--out", out, "--exclude-secret-key", "--config", path)
	if err != nil || !strings.Contains(text, "secret.key is not in the copy") {
		t.Fatalf("backup: %v\n%s", err, text)
	}
	if _, err := os.Stat(filepath.Join(out, "data", core.SecretKeyFile)); !os.IsNotExist(err) {
		t.Errorf("secret.key is in the copy: %v", err)
	}
	if text, err := runBackup(t, "--verify", out); err != nil || !strings.Contains(text, "note:") {
		t.Fatalf("verify: %v\n%s", err, text)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	restoreFrom(t, dir, out)
	if _, err := runSecretKey(t, path, "restore-key", "--password-file", pw); err != nil {
		t.Fatal(err)
	}
	h2, err := Start(ctx, configModeOptions(path))
	if err != nil {
		t.Fatalf("the restored host does not start: %v", err)
	}
	t.Cleanup(func() { h2.Close() })
	if check, err := core.CheckKey(ctx, h2.Store, key); err != nil || !check.All() {
		t.Errorf("the key opens %+v, %v", check, err)
	}
}

func TestBackupCommandUsageRefusalsAndFailures(t *testing.T) {
	dir := t.TempDir()
	path := initConfig(t, dir)
	h, err := Start(context.Background(), configModeOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "copy")
	for name, tc := range map[string]struct {
		args []string
		want int
	}{
		"no option":                      {nil, 2},
		"both modes":                     {[]string{"--out", out, "--verify", out}, 2},
		"verify with exclude":            {[]string{"--verify", out, "--exclude-secret-key"}, 2},
		"verify with config":             {[]string{"--verify", out, "--config", path}, 2},
		"exclude without out":            {[]string{"--exclude-secret-key"}, 2},
		"an argument that is not a flag": {[]string{"--out", out, "extra"}, 2},
		"an existing folder":             {[]string{"--out", dir, "--config", path}, 78},
		"a folder that is no folder":     {[]string{"--verify", path}, 78},
	} {
		_, err := runBackup(t, tc.args...)
		if got := exitCode(err); got != tc.want {
			t.Errorf("%s: exit %d (%v), want %d", name, got, err, tc.want)
		}
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("a refused or failed command left %s: %v", out, err)
	}
	// A copy that fails its check exits with status 1 and names each failure.
	if _, err := runBackup(t, "--out", out, "--config", path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "data", "extra.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	text, err := runBackup(t, "--verify", out)
	if got := exitCode(err); got != 1 || !strings.Contains(text, "data/extra.txt: is not listed in the manifest") || !strings.Contains(err.Error(), "1 checks failed") {
		t.Errorf("a damaged copy: exit %d (%v)\n%s", got, err, text)
	}
}

// The report lists what rule 24 of the spec names: the folder, the files and bytes, the state of secret.key, the
// retries, each skipped path with its reason, each unstable path, and the warning about the secrets in the copy.
func TestBackupReportListsWhatTheSpecNames(t *testing.T) {
	res := backup.Result{
		Out: "/copies/c1", Files: 12, Bytes: 3456, Retries: 2, Attempts: 3,
		Skipped:  []backup.Skip{{Path: "data/flats/s/previews/h.example/db.sqlite", Reason: "gone while it was copied"}},
		Unstable: []string{"data/flats/s/data/files/log.txt"},
	}
	for name, tc := range map[string]struct{ key, want string }{
		"included": {backup.SecretKeyIncluded, "secret.key is in the copy\n"},
		"excluded": {backup.SecretKeyExcluded, "secret.key is not in the copy; the recovery copy secret.key.recovery stays"},
		"absent":   {backup.SecretKeyAbsent, "the host has no secret.key\n"},
	} {
		var out bytes.Buffer
		b := &backupCmd{configCmd: configCmd{ctx: context.Background(), out: &out}}
		res.SecretKey = tc.key
		b.report("/data", &res)
		text := out.String()
		for _, want := range []string{
			"backed up /data to /copies/c1\n", "  12 files, 3456 bytes\n", tc.want, "  2 retries, 3 attempt(s) at coherence\n",
			"  skipped data/flats/s/previews/h.example/db.sqlite: gone while it was copied\n",
			"  data/flats/s/data/files/log.txt changed while it was copied; the copy holds its last read\n",
			"Keep it as protected as the data directory", "flats backup --verify /copies/c1",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s: the report lacks %q:\n%s", name, want, text)
			}
		}
	}
}

// fixtureDB runs statements against a database file.
type fixtureDB struct{ t *testing.T }

func (f *fixtureDB) exec(path string, stmts ...string) {
	f.t.Helper()
	db, err := sql.Open("sqlite", sqliteuri.File(path))
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			f.t.Fatalf("%s: %v", s, err)
		}
	}
}
