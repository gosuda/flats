package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/filelock"
	"github.com/gosuda/flats/internal/ostest"
	"github.com/gosuda/flats/internal/store"
)

// sheetUndoFiles lists the undo scratch files under root with their bytes.
func sheetUndoFiles(t *testing.T, root string) (files []string, bytes int64) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "flats-sheet-undo-*", "*.undo"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		files, bytes = append(files, p), bytes+info.Size()
	}
	return files, bytes
}

// deleteLargeTab adds a tab that holds more text than an undo entry keeps
// in memory and has a person delete it, which spills the entry's payload.
func deleteLargeTab(t *testing.T, s *Service, slugName, tab string) {
	t.Helper()
	var rows strings.Builder
	for i := range 60 {
		if i > 0 {
			rows.WriteByte(',')
		}
		fmt.Fprintf(&rows, "[%q]", fmt.Sprintf("%03d", i)+strings.Repeat("u", 30_000))
	}
	writeSheet(t, s, slugName, "fill-"+tab, fmt.Sprintf(`{"op":"add_tab","name":%q},{"op":"write","tab":%q,"at":"A1","rows":[%s]}`, tab, tab, rows.String()))
	browser := dialSheet(t, s, slugName)
	if got := browser.act(browser.seq, fmt.Sprintf(`{"op":"delete_tab","tab":%.0f}`, browser.tabs[tab])); got != "applied" {
		t.Fatalf("deleting %s: %s", tab, got)
	}
	browser.ws.Close(1000, "")
	<-browser.ws.Done()
}

// TestSheetUndoScratchIsHostStorage covers where the large undo payloads of
// a sheet go and when they leave: under the host's scratch directory, never
// the system's temporary directory or the flat's directory, and gone after
// an idle unload and after the host closes.
func TestSheetUndoScratchIsHostStorage(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s, err := New(t.Context(), Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			s.Close()
		}
	})
	// The instance would use this directory if the host named none. Unix
	// reads TMPDIR for it, and Windows TMP and TEMP.
	system := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, system)
	}
	offset := sheetClock(s)
	in := newSheet(t, s, "budget")

	root := filepath.Join(dir, "cache", "sheet-undo")
	owners, err := filepath.Glob(filepath.Join(root, "owner-*"))
	if err != nil || len(owners) != 1 {
		t.Fatalf("host scratch owners = %v, %v", owners, err)
	}
	owner := owners[0]
	deleteLargeTab(t, s, "budget", "Big")
	files, bytes := sheetUndoFiles(t, owner)
	if len(files) != 1 || bytes < 1<<20 {
		t.Fatalf("%d undo scratch files of %d bytes under the host directory, want the payload of the deleted tab", len(files), bytes)
	}
	if stray, _ := filepath.Glob(filepath.Join(system, "flats-sheet-undo-*")); len(stray) != 0 {
		t.Fatalf("undo scratch files in the system's temporary directory: %v", stray)
	}
	// The directories and files are private to the host's user, as the
	// notebook's are. Windows has no such modes: there the access list of
	// the data directory protects them, and only their presence is checked.
	for path, want := range map[string]os.FileMode{
		root: 0o700, owner: 0o700, filepath.Join(owner, ".owner"): 0o600, filepath.Join(root, ".owners"): 0o600,
		filepath.Dir(files[0]): 0o700, files[0]: 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if ostest.WrongMode(info, want) {
			t.Errorf("%s has mode %o, want %o", path, info.Mode().Perm(), want)
		}
	}

	// Scratch bytes are in no measure of the flat: not in its directory,
	// and a growing scratch file changes neither its disk use nor what the
	// quota of a write counts beside sheet.db.
	if isWithin(s.flatDir("budget"), files[0]) || isWithin(filepath.Join(dir, "flats"), files[0]) {
		t.Fatalf("scratch file %s lies in a flat's directory", files[0])
	}
	measure := func() (int64, int64) {
		t.Helper()
		other, err := s.sheetOther("budget")
		if err != nil {
			t.Fatal(err)
		}
		disk, err := walkSize(s.flatDir("budget"), nil)
		if err != nil {
			t.Fatal(err)
		}
		return other, disk
	}
	other, disk := measure()
	if err := os.WriteFile(filepath.Join(filepath.Dir(files[0]), "more.undo"), make([]byte, 3<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if o, d := measure(); o != other || d != disk {
		t.Fatalf("3 MiB more scratch changed the flat's measured use from %d and %d to %d and %d", other, disk, o, d)
	}

	// An idle unload removes the instance's files and keeps the host's
	// directory for the next instance.
	deadline := time.Now().Add(sheetTestWait)
	for in.Stats().Connections != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the connection never closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The person's edit group closes on the sheet's tick and core imports
	// its revision on its own; the idle time counts from that import.
	unloadSheetNow(t, s, offset, "budget")
	if left, _ := os.ReadDir(owner); len(left) != 1 || left[0].Name() != ".owner" {
		t.Fatalf("the unload left %v in the host scratch directory", left)
	}

	// The sheet loads again, spills again, and closing the host leaves
	// nothing but the lock file of the directory.
	deleteLargeTab(t, s, "budget", "Again")
	if files, _ := sheetUndoFiles(t, owner); len(files) != 1 {
		t.Fatalf("%d undo scratch files after the reload", len(files))
	}
	closed = true
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != ".owners" {
			t.Errorf("sheet scratch remains after the host closed: %s", entry.Name())
		}
	}
	if stray, _ := filepath.Glob(filepath.Join(system, "flats-sheet-undo-*")); len(stray) != 0 {
		t.Fatalf("undo scratch files in the system's temporary directory: %v", stray)
	}
}

// TestSheetScratchStartupRemovesLeftovers covers a start on a data
// directory where a killed host left undo scratch files and another host
// still runs: the first are removed, the second kept.
func TestSheetScratchStartupRemovesLeftovers(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "cache", "sheet-undo")
	plant := func(owner string) string {
		t.Helper()
		file := filepath.Join(owner, "flats-sheet-undo-1", "1.undo")
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}
		return file
	}
	// A host that still runs holds the lock of its directory.
	live, err := openSheetScratch(root, t.Logf, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.close() })
	kept := plant(live.dir)
	// A killed host left its directory, with its lock file and without.
	dead := filepath.Join(root, "owner-"+strings.Repeat("0a", 16))
	gone := plant(dead)
	if err := os.WriteFile(filepath.Join(dead, ".owner"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bare := plant(filepath.Join(root, "owner-"+strings.Repeat("0b", 16)))
	// Anything that is no owner directory is not the cleanup's to remove.
	other := plant(filepath.Join(root, "owner-notes"))

	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	// The host has no sheet: the cleanup is part of the start.
	s, err := New(t.Context(), Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); st.Close() })
	for _, file := range []string{gone, bare} {
		if _, err := os.Stat(filepath.Dir(filepath.Dir(file))); !os.IsNotExist(err) {
			t.Errorf("the start kept the leftover of a killed host: %s (%v)", file, err)
		}
	}
	for _, file := range []string{kept, other} {
		if _, err := os.Stat(file); err != nil {
			t.Errorf("the start removed %s: %v", file, err)
		}
	}
	owners, err := filepath.Glob(filepath.Join(root, "owner-*"))
	if err != nil || len(owners) != 3 {
		t.Fatalf("owner directories after the start = %v, want the live host's, this host's and the unrelated one", owners)
	}
	if own, err := s.sheetScratchDir(); err != nil || own == live.dir || !isWithin(root, own) {
		t.Fatalf("this host's scratch directory = %s, %v", own, err)
	}
}

// TestSheetScratchRecoversAfterRepair covers a host that could not make
// its scratch directory when it started. A sheet that was open all the
// while refuses the action that needs a scratch file, as unavailable and
// without applying anything, for as long as the storage is broken. Once
// the storage is repaired the same action, sent again as a browser sends
// it, applies once, its undo works, and a sheet that was open beside it
// gets its scratch files too. No payload goes to the system's temporary
// directory in between.
func TestSheetScratchRecoversAfterRepair(t *testing.T) {
	dir := t.TempDir()
	// A file stands where the scratch directory belongs. Missing
	// permissions would do the same, and do not stop every user on every
	// system.
	root := filepath.Join(dir, "cache", "sheet-undo")
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(t.Context(), Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); st.Close() })
	system := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, system)
	}
	newSheet(t, s, "budget")
	newSheet(t, s, "plans")
	var rows strings.Builder
	for i := range 60 {
		if i > 0 {
			rows.WriteByte(',')
		}
		fmt.Fprintf(&rows, "[%q]", fmt.Sprintf("%03d", i)+strings.Repeat("u", 30_000))
	}
	writeSheet(t, s, "budget", "fill", fmt.Sprintf(`{"op":"add_tab","name":"Big"},{"op":"write","tab":"Big","at":"A1","rows":[%s]}`, rows.String()))
	tabs := func() []string {
		t.Helper()
		var names []string
		for _, tab := range sheetRead(t, s, "budget", 0, "")["tabs"].([]any) {
			names = append(names, tab.(map[string]any)["name"].(string))
		}
		return names
	}
	state := func() string {
		t.Helper()
		return sheetRead(t, s, "budget", 0, "")["state"].(string)
	}

	browser := dialSheet(t, s, "budget")
	before := state()
	action := fmt.Sprintf(`{"type":"action","n":"1","base":%q,"ops":[{"op":"delete_tab","tab":%.0f}]}`, browser.seq, browser.tabs["Big"])
	// send sends the action on a connection of the browser's session and
	// returns what the host answers: the status of its ack, or the code
	// that ends the connection.
	send := func(b *sheetBrowser) string {
		t.Helper()
		b.send(action)
		_ = b.ws.NetConn().SetReadDeadline(time.Now().Add(sheetTestWait))
		for {
			raw, err := b.ws.ReadMessage(64 << 20)
			if err != nil {
				t.Fatalf("no answer to the action: %v", err)
			}
			var f map[string]any
			if err := json.Unmarshal([]byte(raw), &f); err != nil {
				t.Fatalf("frame %q: %v", raw, err)
			}
			switch f["type"] {
			case "fatal":
				return f["code"].(string)
			case "ack":
				return f["results"].([]any)[0].(map[string]any)["status"].(string)
			}
		}
	}
	again := func() *sheetBrowser {
		t.Helper()
		return dialSheetAs(t, s, "budget", fmt.Sprintf(`{"type":"hello","version":1,"session":%q,"pending":1}`, browser.session))
	}
	for attempt := range 2 {
		b := browser
		if attempt > 0 {
			b = again()
		}
		if got := send(b); got != "unavailable" {
			t.Fatalf("attempt %d while the scratch storage is broken = %s, want unavailable", attempt+1, got)
		}
		if got := tabs(); !slices.Contains(got, "Big") || state() != before {
			t.Fatalf("a refused action changed the sheet: tabs %v, state %s after %s", got, state(), before)
		}
	}
	if stray, _ := filepath.Glob(filepath.Join(system, "flats-sheet-undo-*")); len(stray) != 0 {
		t.Fatalf("undo scratch files in the system's temporary directory: %v", stray)
	}

	// The operator repairs the storage. Nothing restarts and no sheet is
	// opened: the sheet that needs the storage asks for it again.
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if got := send(again()); got != "applied" {
		t.Fatalf("the action after the repair = %s, want applied", got)
	}
	after := state()
	if got := tabs(); slices.Contains(got, "Big") || after == before {
		t.Fatalf("after the repair: tabs %v, state %s after %s", got, after, before)
	}
	owners, err := filepath.Glob(filepath.Join(root, "owner-*"))
	if err != nil || len(owners) != 1 {
		t.Fatalf("host scratch owners after the repair = %v, %v", owners, err)
	}
	if files, bytes := sheetUndoFiles(t, owners[0]); len(files) != 1 || bytes < 1<<20 {
		t.Fatalf("%d undo scratch files of %d bytes, want the payload of the deleted tab", len(files), bytes)
	}
	// Sent once more with its number, the action is answered from its
	// receipt and changes nothing.
	resumed := again()
	if got := send(resumed); got != "applied" || state() != after {
		t.Fatalf("the action sent again = %s, state %s after %s", got, state(), after)
	}
	// The undo reads the payload back from the scratch file.
	resumed.n = 1
	if got := resumed.act(resumed.seq, `{"op":"undo"}`); got != "applied" {
		t.Fatalf("undo of the deleted tab: %s", got)
	}
	if got := tabs(); !slices.Contains(got, "Big") {
		t.Fatalf("tabs after the undo = %v", got)
	}
	cell := sheetRead(t, s, "budget", 0, `{"tab":"Big","range":"A60"}`)["rows"].([]any)[0].([]any)[0].(string)
	if len(cell) != 30_003 || !strings.HasPrefix(cell, "059u") {
		t.Fatalf("the restored tab holds %d characters starting %.8q in A60", len(cell), cell)
	}
	// The other sheet was opened while the storage was broken as well.
	held, _ := sheetUndoFiles(t, owners[0])
	deleteLargeTab(t, s, "plans", "Big")
	if files, _ := sheetUndoFiles(t, owners[0]); len(files) != len(held)+1 {
		t.Fatalf("%d undo scratch files after the second sheet spilled, want %d", len(files), len(held)+1)
	}
	if stray, _ := filepath.Glob(filepath.Join(system, "flats-sheet-undo-*")); len(stray) != 0 {
		t.Fatalf("undo scratch files in the system's temporary directory: %v", stray)
	}
}

// TestSheetScratchMadeLateKeepsTheStartRules covers the directory of a host
// that could not make it at start and makes it for a sheet that needs a
// scratch file. That sheet holds its workbook lock: the host waits for no
// other host and removes nothing then. The directory of a host that still
// runs is never removed, and what a killed host left goes when the next
// sheet opens, as it would have gone at the start.
func TestSheetScratchMadeLateKeepsTheStartRules(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "cache", "sheet-undo")
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(t.Context(), Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); st.Close() })
	if got, err := s.sheetScratchNow(); err == nil {
		t.Fatalf("a scratch directory %s while a file stands in its place", got)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	// Another host runs on the data directory, and a killed one left its
	// directory.
	live, err := openSheetScratch(root, t.Logf, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.close() })
	dead := filepath.Join(root, "owner-"+strings.Repeat("0a", 16))
	left := filepath.Join(dead, "flats-sheet-undo-1", "1.undo")
	if err := os.MkdirAll(filepath.Dir(left), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(left, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	// While another host makes its directory, a sheet is told to come
	// back. It is not kept waiting with its workbook locked.
	unlock, err := lockSheetScratch(root, true)
	if err != nil {
		t.Fatal(err)
	}
	answered := make(chan error, 1)
	go func() {
		_, err := s.sheetScratchNow()
		answered <- err
	}()
	select {
	case err := <-answered:
		if !errors.Is(err, errSheetScratchBusy) {
			t.Fatalf("a sheet that asks while another host holds the lock = %v, want %v", err, errSheetScratchBusy)
		}
	case <-time.After(sheetTestWait):
		t.Fatal("a sheet that asks for the scratch directory waits for another host")
	}
	unlock()

	own, err := s.sheetScratchNow()
	if err != nil || own == live.dir || !isWithin(root, own) {
		t.Fatalf("this host's scratch directory = %s, %v", own, err)
	}
	for _, path := range []string{left, live.dir, own} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("making the directory for a sheet removed %s: %v", path, err)
		}
	}
	// The next sheet that opens removes the leftover and keeps the rest.
	newSheet(t, s, "budget")
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatalf("the leftover of a killed host stays after a sheet opened: %v", err)
	}
	for _, path := range []string{live.dir, own} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("opening a sheet removed %s: %v", path, err)
		}
	}
	if again, err := s.sheetScratchNow(); err != nil || again != own {
		t.Fatalf("the scratch directory changed from %s to %s, %v", own, again, err)
	}
}

// TestSheetScratchRemovedWhileRunning covers an operator who removes the
// host's scratch directory while sheets are open: undo history that was in
// it is lost, and the next action that needs a scratch file gets a new
// directory of the host.
func TestSheetScratchRemovedWhileRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not remove a directory whose lock file the host holds open")
	}
	s, dir := newTestService(t)
	newSheet(t, s, "budget")
	root := filepath.Join(dir, "cache", "sheet-undo")
	first, err := s.sheetScratchNow()
	if err != nil {
		t.Fatal(err)
	}
	deleteLargeTab(t, s, "budget", "Big")
	if files, _ := sheetUndoFiles(t, first); len(files) != 1 {
		t.Fatalf("%d undo scratch files, want the payload of the deleted tab", len(files))
	}
	if err := os.RemoveAll(first); err != nil {
		t.Fatal(err)
	}
	deleteLargeTab(t, s, "budget", "Again")
	second, err := s.sheetScratchNow()
	if err != nil || second == first || !isWithin(root, second) {
		t.Fatalf("the scratch directory after the removal = %s, %v (before: %s)", second, err, first)
	}
	if files, _ := sheetUndoFiles(t, second); len(files) != 1 {
		t.Fatalf("%d undo scratch files in the new directory, want the payload of the second tab", len(files))
	}
	if err := filelock.TryLock(mustOpen(t, filepath.Join(second, ".owner"))); !errors.Is(err, filelock.ErrLocked) {
		t.Fatalf("the new directory is not held by this host: %v", err)
	}
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
