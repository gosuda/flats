package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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

	"github.com/gosuda/flats/internal/board"
	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/core"
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

	boardW := &writer{write: func(n int64) (int64, error) { return writeBoard(t, h, "pad", n) }}
	notebookW := &writer{write: func(n int64) (int64, error) { return writeNotebook(h, "notes", n) }}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	boardW.run(t, stop, &wg)
	notebookW.run(t, stop, &wg)
	deadline := time.Now().Add(30 * time.Second)
	for boardW.acked.Load() < 5 || notebookW.acked.Load() < 5 {
		if time.Now().After(deadline) {
			t.Fatal("the writers did not get going")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The writes that the host acknowledged before the backup began must be in the restored host.
	boardSeq := boardW.lastSeq.Load()
	boardBefore, notebookBefore := boardW.acked.Load(), notebookW.acked.Load()

	out := filepath.Join(t.TempDir(), "copy")
	text, err := runBackup(t, "--out", out, "--config", path)
	if err != nil {
		t.Fatalf("backup: %v\n%s", err, text)
	}
	close(stop)
	wg.Wait()
	if !strings.Contains(text, "backed up") || !strings.Contains(text, "secret.key is in the copy") {
		t.Errorf("output = %q", text)
	}
	if text, err := runBackup(t, "--verify", out); err != nil || !strings.Contains(text, "ok:") {
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
	for _, s := range []string{"app", "pad", "notes"} {
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
	got, err := os.ReadFile(filepath.Join(dir, "flats", "app", "data", "files", "notes.txt"))
	if err != nil || string(got) != "guest file" {
		t.Errorf("the server flat's file after the restore: %q %v", got, err)
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
	if got := exitCode(err); got != 1 || !strings.Contains(text, "data/extra.txt: is not listed in the manifest") {
		t.Errorf("a damaged copy: exit %d (%v)\n%s", got, err, text)
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
