package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/sheet"
	"github.com/gosuda/flats/internal/wsconn"
)

// sheetBrowser is one person editing a sheet through its Private route.
type sheetBrowser struct {
	t       *testing.T
	ws      *wsconn.Conn
	tabs    map[string]float64
	seq     any // the sequence of the welcome
	session string
	n       int
}

func dialSheet(t *testing.T, s *Service, slugName string) *sheetBrowser {
	t.Helper()
	return dialSheetAs(t, s, slugName, `{"type":"hello","version":1,"name":"Ada","pending":0}`)
}

// dialSheetAs connects with the hello frame given: a new person, or one
// who comes back with a session and unanswered actions.
func dialSheetAs(t *testing.T, s *Service, slugName, hello string) *sheetBrowser {
	t.Helper()
	server := httptest.NewServer(s.siteHandler(slugName, false))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	ws, err := wsconn.Dial(t.Context(), func(ctx context.Context) (net.Conn, error) {
		c, err := new(net.Dialer).DialContext(ctx, "tcp", host)
		if err != nil {
			return nil, err
		}
		return &originConn{Conn: c, origin: server.URL}, nil
	}, host, "/_sheet/ws")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close(1000, ""); <-ws.Done() })
	b := &sheetBrowser{t: t, ws: ws, tabs: map[string]float64{}}
	b.send(hello)
	welcome := b.next("welcome")
	b.seq = welcome["seq"]
	b.session, _ = welcome["session"].(string)
	for _, tab := range welcome["tabs"].([]any) {
		tab := tab.(map[string]any)
		b.tabs[tab["name"].(string)] = tab["id"].(float64)
	}
	return b
}

func (b *sheetBrowser) send(frame string) {
	b.t.Helper()
	if err := b.ws.Send(frame); err != nil {
		b.t.Fatalf("send: %v", err)
	}
}

// next returns the next frame of the type and passes over the others.
func (b *sheetBrowser) next(kind string) map[string]any {
	b.t.Helper()
	_ = b.ws.NetConn().SetReadDeadline(time.Now().Add(sheetTestWait))
	for {
		raw, err := b.ws.ReadMessage(64 << 20)
		if err != nil {
			b.t.Fatalf("no %s frame: %v", kind, err)
		}
		var f map[string]any
		if err := json.Unmarshal([]byte(raw), &f); err != nil {
			b.t.Fatalf("frame %q: %v", raw, err)
		}
		if f["type"] == kind {
			return f
		}
	}
}

// act sends one action of the operation against the state the sheet has
// sent and returns its outcome: "applied", or "refused:" and the reason.
func (b *sheetBrowser) act(seq any, op string) string {
	b.t.Helper()
	b.n++
	b.send(fmt.Sprintf(`{"type":"action","n":"%d","base":%q,"ops":[%s]}`, b.n, seq, op))
	result := b.next("ack")["results"].([]any)[0].(map[string]any)
	status, _ := result["status"].(string)
	if reason, _ := result["reason"].(string); reason != "" {
		status += ":" + reason
	}
	return status
}

// set types text into the first cell of the tab and returns the outcome of
// the edit: "applied", or "refused:" and the reason.
func (b *sheetBrowser) set(tab, text string) string {
	b.t.Helper()
	id, ok := b.tabs[tab]
	if !ok {
		b.t.Fatalf("no tab %q", tab)
	}
	b.n++
	b.send(fmt.Sprintf(`{"type":"watch","generation":%d,"tab":%.0f}`, b.n, id))
	head := b.next("tab_head")
	row := head["rows"].([]any)[0].([]any)[0].(float64)
	col := head["cols"].([]any)[0].([]any)[0].(float64)
	b.next("tab_done")
	quoted, _ := json.Marshal(text)
	return b.act(head["seq"], fmt.Sprintf(`{"op":"set","tab":%.0f,"row":%.0f,"col":%.0f,"text":%s,"at":[0,0]}`, id, row, col, quoted))
}

func sheetBlobBytes(t *testing.T, s *Service, slugName string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(filepath.Join(s.docsStoreDir(slugName), "blobs"), func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		total += info.Size()
		return err
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return total
}

// TestSheetImportRetryCountsStoredBlobs covers an import that stored its
// blobs and then failed to commit: the retry finds the blobs in the store,
// and the bytes still count toward the quota of people's edits.
func TestSheetImportRetryCountsStoredBlobs(t *testing.T) {
	ctx := context.Background()
	s, dir := newTestService(t)
	in := newSheet(t, s, "budget")
	writeSheet(t, s, "budget", "tabs", `{"op":"add_tab","name":"Edit"}`)
	// Changing the quota measures the directory, so it is set before the
	// failure: afterwards only the import's own accounting moves the usage
	// people's edits are checked against.
	if _, err := s.UpdateSettings(ctx, map[string]string{SetDiskQuotaBytes: fmt.Sprint(24 << 20)}); err != nil {
		t.Fatal(err)
	}
	browser := dialSheet(t, s, "budget")
	if got := browser.set("Edit", "ready"); got != "applied" {
		t.Fatalf("edit below the quota: %s", got)
	}

	cell := func(i int) string { return fmt.Sprintf("%06d", i) + strings.Repeat("q", 10_000) }
	rows := func(first, count int) string {
		var b strings.Builder
		for i := range count {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "[%q]", cell(first+i))
		}
		return b.String()
	}
	before := sheetBlobBytes(t, s, "budget")
	flats := filepath.Join(dir, "flats.db")
	boardExecDB(t, flats, `CREATE TRIGGER fail_revision BEFORE INSERT ON revisions BEGIN SELECT RAISE(ABORT, 'injected commit failure'); END`)
	const big = 120
	body := fmt.Sprintf(`{"state":%q,"op_id":"big","ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[%s]}]}`, sheetState(t, s, "budget"), rows(0, big))
	out, err := s.UpdateSheet(ctx, "budget", []byte(body), ViaMCP)
	if err != nil || !out.Pending {
		t.Fatalf("write with a failed import = %+v, %v", out, err)
	}
	stored := sheetBlobBytes(t, s, "budget") - before
	if stored < big*10_000 {
		t.Fatalf("the failed import stored %d bytes of blobs, want the write's content", stored)
	}
	boardExecDB(t, flats, `DROP TRIGGER fail_revision`)
	s.sheetRetry.Delete("budget")
	s.TickSheets(ctx)
	if entries, err := in.Outbox(ctx, 0, 10); err != nil || len(entries) != 0 {
		t.Fatalf("entries after the retry = %+v, %v", entries, err)
	}

	// Agent writes measure the directory themselves. They fill the sheet,
	// each in a tab of its own, until the quota refuses one, and then add
	// to the rows a person will edit until it refuses a small one.
	refused := func(body string) bool {
		t.Helper()
		_, err := s.UpdateSheet(ctx, "budget", []byte(fmt.Sprintf(`{"state":%q,"ops":[%s]}`, sheetState(t, s, "budget"), body)), ViaMCP)
		if err == nil {
			return false
		}
		var problem *sheet.WriteError
		if !errors.As(err, &problem) || problem.Reason != "disk_quota" {
			t.Fatalf("fill write: %v", err)
		}
		return true
	}
	tabs := 0
	for _, count := range []int{72, 18} {
		for {
			tabs++
			if tabs > 40 {
				t.Fatal("the quota never refused a write")
			}
			if refused(fmt.Sprintf(`{"op":"add_tab","name":"Fill%d"},{"op":"write","tab":"Fill%d","at":"A1","rows":[%s]}`, tabs, tabs, rows(tabs*100, count))) {
				break
			}
		}
	}
	for row := 2; ; row++ {
		if row > 200 {
			t.Fatal("the quota never refused a small write")
		}
		if refused(fmt.Sprintf(`{"op":"write","tab":"Edit","at":"A%d","rows":[%s]}`, row, rows(row, 1))) {
			break
		}
	}
	// The flat is within one small write of its quota. A person's larger
	// edit of the same rows is over it, with the blobs of the failed attempt
	// counted.
	if got := browser.set("Edit", strings.Repeat("e", 32_000)); got != "refused:disk_quota" {
		t.Fatalf("edit at the quota after an import retry: %s, want refused:disk_quota (the first attempt stored %d bytes)", got, stored)
	}
}

// sheetBlobs lists the hashes of the blobs a sheet has in its store.
func sheetBlobs(t *testing.T, s *Service, slugName string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(filepath.Join(s.docsStoreDir(slugName), "blobs"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out[d.Name()] = p
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSheetSweepsBlobsWhileUnloaded covers a sheet nobody uses: its pruned
// revisions leave the blob store on a later background sweep, while the
// head, the retained revisions and leased blobs stay.
func TestSheetSweepsBlobsWhileUnloaded(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	offset := sheetClock(s)
	if _, err := s.UpdateSettings(ctx, map[string]string{SetRevisionQuota: "2"}); err != nil {
		t.Fatal(err)
	}
	newSheet(t, s, "budget")
	writeSheet(t, s, "budget", "rows", sheetBudgetRows)
	if err := s.PinRevision(ctx, "budget", 2, true); err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		writeSheet(t, s, "budget", fmt.Sprint("change-", i), fmt.Sprintf(`{"op":"write","tab":"Sheet1","at":"B2","rows":[[%d]]}`, 1300+i))
	}
	if got := storedRevisions(t, s, "budget"); !slices.Equal(got, []int{8, 7, 2}) {
		t.Fatalf("retained revisions = %v", got)
	}
	referenced, err := s.st.ReferencedHashes(ctx, "budget")
	if err != nil {
		t.Fatal(err)
	}
	var pruned []string
	for hash := range sheetBlobs(t, s, "budget") {
		if !referenced[hash] {
			pruned = append(pruned, hash)
		}
	}
	slices.Sort(pruned)
	if len(pruned) < 2 {
		t.Fatalf("retention left %d unreferenced blobs, want the files of the pruned revisions", len(pruned))
	}

	unloadSheetNow(t, s, offset, "budget")
	// SweepDesignBlobs passes over a sheet whose lock is taken, here also
	// by the sweeps the retention of the writes above started in the
	// background. The test runs the same sweep under the lock, so that each
	// check below follows a sweep that ran.
	sweep := func() {
		unlock := s.lock("budget")
		defer unlock()
		s.sweepBlobsLocked(ctx, "budget", contenttype.Sheet)
	}
	// The sweep that follows the unload finds the blobs younger than the
	// grace period and keeps them.
	sweep()
	if blobs := sheetBlobs(t, s, "budget"); len(blobs) != len(referenced)+len(pruned) {
		t.Fatalf("a sweep removed blobs inside the grace period: %d left of %d", len(blobs), len(referenced)+len(pruned))
	}
	release := s.lease("budget", pruned[:1])
	old := time.Now().Add(-2 * blobGrace)
	for _, p := range sheetBlobs(t, s, "budget") {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	s.SweepDesignBlobs(ctx)
	sweep()
	if s.liveSheet("budget") != nil {
		t.Fatal("the sweep loaded a sheet nobody uses")
	}
	blobs := sheetBlobs(t, s, "budget")
	for hash := range referenced {
		if blobs[hash] == "" {
			t.Fatalf("the sweep removed referenced blob %.12s", hash)
		}
	}
	if blobs[pruned[0]] == "" {
		t.Fatal("the sweep removed a leased blob")
	}
	for _, hash := range pruned[1:] {
		if blobs[hash] != "" {
			t.Fatalf("pruned blob %.12s stays in the store of an unloaded sheet", hash)
		}
	}
	release()
	sweep()
	if blobs := sheetBlobs(t, s, "budget"); len(blobs) != len(referenced) {
		t.Fatalf("%d blobs after the lease ended, want the %d referenced ones", len(blobs), len(referenced))
	}

	// The sheet loads again with its content and its retained history.
	if got := sheetRead(t, s, "budget", 0, `{"tab":"Sheet1","range":"B2"}`)["rows"].([]any)[0].([]any)[0]; got != float64(1305) {
		t.Fatalf("B2 after the reload = %v", got)
	}
	for rev, want := range map[int]float64{2: 1200, 7: 1304, 8: 1305} {
		if got := sheetRead(t, s, "budget", rev, `{"tab":"Sheet1","range":"B2"}`)["rows"].([]any)[0].([]any)[0]; got != want {
			t.Fatalf("B2 of revision %d after the sweep = %v, want %v", rev, got, want)
		}
	}
	if res, err := s.RollbackRevisionResult(ctx, "budget", 2, "agent", ViaMCP); err != nil || res.Revision != 9 {
		t.Fatalf("rollback after the sweep = %+v, %v", res, err)
	}
	out, _ := writeSheet(t, s, "budget", "after", `{"op":"write","tab":"Sheet1","at":"B3","rows":[[950]]}`)
	if out.Revision != 10 {
		t.Fatalf("write after the sweep = %+v", out)
	}
}
