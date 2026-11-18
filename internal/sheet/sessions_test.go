package sheet

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/wsconn"
)

// adopt puts a session that a test read from sheet.db into memory, as the
// writer does when it resumes one.
func (e *env) adopt(s *session) *session {
	e.t.Helper()
	var found *session
	err := e.in.execute(context.Background(), &work{run: func() error {
		e.in.mu.Lock()
		defer e.in.mu.Unlock()
		if err := e.in.sessionRoom(s); err != nil {
			return err
		}
		var err error
		found, err = e.in.adopt(s)
		return err
	}})
	if err != nil {
		e.t.Fatal(err)
	}
	return found
}

// leave lets go of a person's session as a page that closed does: its time
// without activity starts now.
func (p *person) leave() {
	p.e.in.release(p.s)
}

// visit opens a Private connection with the hello, reads until the welcome
// or a fatal frame, and closes it. It keeps nothing of the connection, so
// that a test can measure what the host keeps of many.
func (s *site) visit(hello string) map[string]any {
	s.e.t.Helper()
	host := strings.TrimPrefix(s.server.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, err := wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) { return new(net.Dialer).DialContext(ctx, "tcp", host) }, host, "/private/_sheet/ws")
	if err != nil {
		s.e.t.Fatalf("dial: %v", err)
	}
	defer ws.Close(1000, "")
	if err := ws.Send(hello); err != nil {
		s.e.t.Fatalf("send: %v", err)
	}
	for {
		raw, err := ws.ReadMessage(64 << 20)
		if err != nil {
			s.e.t.Fatalf("no welcome: %v", err)
		}
		var f map[string]any
		if json.Unmarshal([]byte(raw), &f) != nil {
			continue
		}
		if f["type"] == "welcome" || f["type"] == "fatal" {
			return f
		}
	}
}

// gone waits until the connection of a session went away on the host, or
// the session left memory.
func (e *env) gone(id string) {
	e.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		e.in.smu.Lock()
		s := e.in.sessions[id]
		left := s == nil || s.conn == nil
		e.in.smu.Unlock()
		if left {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("session %s is still connected", id)
		}
	}
}

// population returns the sessions in memory and the weight the instance
// counts for them.
func (e *env) population() (count int, weight int64) {
	e.in.smu.Lock()
	defer e.in.smu.Unlock()
	return len(e.in.sessions), e.in.sessionBytes
}

// TestSessionChurnKeepsMemoryBounded keeps one connection open, so that the
// sheet stays loaded, and opens and closes four times as many Private
// connections as the sheet keeps sessions, one after the other, each with
// a label of 200 four-byte characters and without an action. That works
// with and without the quota. The sessions in memory stay within their
// count and bytes, by the live heap too, and so does every walk over them.
// The oldest disconnected session leaves first and the connected one
// stays. One with a row resumes from sheet.db; one without a row is gone:
// a page with pending edits gets receipt_expired, one without gets
// session_expired and starts a new session. A reader over the quota still
// reads.
func TestSessionChurnKeepsMemoryBounded(t *testing.T) {
	slow(t, "opens a thousand connections")
	for _, over := range []bool{false, true} {
		name := "within the quota"
		if over {
			name = "over the quota"
		}
		t.Run(name, func(t *testing.T) {
			var e *env
			if over {
				e = overQuota(t)
			} else {
				e = start(t)
				e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["kept"]]}]}`)
			}
			s := serve(e)
			tab := e.tabID("Sheet1")
			reader := s.dial("private", `{"type":"hello","version":1,"name":"Reader","pending":0}`)
			readerID := reader.next("welcome")["session_id"].(string)
			reader.watch(1, tab)
			label := strings.Repeat("😀", 200)
			hello := fmt.Sprintf(`{"type":"hello","version":1,"name":%q,"pending":0}`, label)
			churn := raced(4*maxSessions, maxSessions+32)
			var first []map[string]any
			before := liveHeap()
			for i := range churn {
				w := s.visit(hello)
				if w["type"] != "welcome" {
					t.Fatalf("connection %d: %v", i, w)
				}
				if len(first) < 4 {
					first = append(first, w)
				}
				e.gone(w["session_id"].(string))
				// Each session leaves after the one before.
				e.clock.Add(time.Millisecond)
				if count, weight := e.population(); count > maxSessions || weight > sessionsBytes {
					t.Fatalf("after %d connections memory keeps %d sessions of %d bytes", i+1, count, weight)
				}
			}
			// The reader got a presence frame for each connection; the test
			// keeps none of them.
			for len(reader.frames) > 0 {
				<-reader.frames
			}
			grew := liveHeap() - before
			count, weight := e.population()
			t.Logf("%d connections: %d sessions of %d bytes in memory, the live heap grew by %d", churn, count, weight, grew)
			if measures && grew > sessionsBytes+256<<10 {
				t.Fatalf("the live heap grew by %d bytes, more than the %d of the sessions", grew, sessionsBytes)
			}
			// Every walk over the sessions, as the undo and the maintenance
			// do, goes over those in memory and no more.
			e.in.mu.Lock()
			walked := 0
			e.in.smu.Lock()
			for range e.in.sessions {
				walked++
			}
			e.in.smu.Unlock()
			held := e.in.holders()
			e.in.mu.Unlock()
			if walked > maxSessions || len(held.entries) != 0 {
				t.Fatalf("a walk goes over %d sessions and finds %d entries", walked, len(held.entries))
			}
			e.in.smu.Lock()
			if r := e.in.sessions[readerID]; r == nil || r.conn == nil {
				t.Fatal("the connected session left memory")
			}
			for _, w := range first {
				if e.in.sessions[w["session_id"].(string)] != nil {
					t.Fatal("the oldest disconnected sessions are still in memory")
				}
			}
			e.in.smu.Unlock()
			if over {
				resume := func(pending int) map[string]any {
					return s.visit(fmt.Sprintf(`{"type":"hello","version":1,"session":%q,"pending":%d}`, first[0]["session"], pending))
				}
				if f := resume(1); f["type"] != "fatal" || f["code"] != "receipt_expired" {
					t.Fatalf("a page with pending edits of a session that left memory: %v", f)
				}
				if f := resume(0); f["type"] != "fatal" || f["code"] != "session_expired" {
					t.Fatalf("a page without pending edits of a session that left memory: %v", f)
				}
			} else {
				w := s.visit(fmt.Sprintf(`{"type":"hello","version":1,"session":%q,"pending":1}`, first[0]["session"]))
				if w["type"] != "welcome" || w["session_id"] != first[0]["session_id"] {
					t.Fatalf("a session with a row that left memory did not resume: %v", w)
				}
			}
			// Reading goes on, for the connection that stayed and a new one.
			reader.watch(2, tab)
			late := s.dial("private", `{"type":"hello","version":1,"name":"Late","pending":0}`)
			late.next("welcome")
			if _, cells := late.watch(1, tab); len(cells) == 0 {
				t.Fatal("a new connection reads nothing")
			}
		})
	}
}

// TestSessionsInUseStayInMemory fills the sheet's sessions with ones that
// are connected or that a request holds. None of them leaves memory for a
// new one, which is refused as unavailable and stores no row; once one is
// let go, the new one comes in and the idle one leaves.
func TestSessionsInUseStayInMemory(t *testing.T) {
	e := start(t)
	var people []*person
	for i := range maxSessions {
		people = append(people, e.person(fmt.Sprint("Held ", i)))
	}
	rows := func() (n int) {
		t.Helper()
		if err := e.in.rdb.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	stored := rows()
	_, _, err := e.in.newSession(context.Background(), "editor:late", "Late", false)
	if werr := refusal(err); err == nil || werr.Code != "unavailable" || werr.Reason != "sessions" {
		t.Fatalf("a new session among held ones: %v", err)
	}
	if rows() != stored {
		t.Fatal("a refused session stored its row")
	}
	people[3].leave()
	late, _, err := e.in.newSession(context.Background(), "editor:late", "Late", false)
	if err != nil {
		t.Fatal(err)
	}
	e.in.smu.Lock()
	defer e.in.smu.Unlock()
	if e.in.sessions[people[3].s.id] != nil || e.in.sessions[late.id] != late || len(e.in.sessions) != maxSessions {
		t.Fatal("the idle session did not make room")
	}
}

// TestHistoryRequestSessionExpires resumes a session after a restart
// through a History recovery alone, while another connection keeps the
// sheet loaded. The session gets the recovery's undo entry, loses its undo
// history after 10 minutes without activity and leaves memory after 24
// hours, without a connection ever coming or going.
func TestHistoryRequestSessionExpires(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Two"},{"op":"write","tab":"Two","at":"A1","rows":[["two"]]}]}`)
	rev := e.b.importAll(t)
	two := e.tabID("Two")
	s := serve(e)
	w := s.visit(`{"type":"hello","version":1,"name":"Alice","pending":0}`)
	id, token := w["session_id"].(string), w["session"].(string)
	e.gone(id)
	e.restart()
	bob := s.dial("private", `{"type":"hello","version":1,"name":"Bob","pending":0}`)
	bob.next("welcome")
	body := fmt.Sprintf(`{"revision":%d,"tab":%d,"session":%q}`, rev, two, token)
	for {
		req, _ := http.NewRequest(http.MethodPost, s.server.URL+"/private/_sheet/api/recover", strings.NewReader(body))
		req.Header.Set("X-Flats-Sheet", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusServiceUnavailable && bytes.Contains(data, []byte(`"calculating"`)) {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("recover: %d %s", resp.StatusCode, data)
		}
		break
	}
	alice := func() (*session, int) {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		e.in.smu.Lock()
		defer e.in.smu.Unlock()
		s := e.in.sessions[id]
		if s == nil {
			return nil, 0
		}
		return s, len(s.undo.undo)
	}
	if s, entries := alice(); s == nil || s.conn != nil || entries != 1 {
		t.Fatalf("after the recovery the session is %v with %d undo entries", s, entries)
	}
	e.clock.Add(undoDisconnected + time.Minute)
	e.tick()
	if s, entries := alice(); s == nil || entries != 0 {
		t.Fatalf("after 11 minutes without activity the session is %v with %d undo entries", s, entries)
	}
	e.clock.Add(sessionTTL)
	e.tick()
	if s, _ := alice(); s != nil {
		t.Fatal("the session is still in memory a day after its request")
	}
}

// dbFiles returns the bytes of sheet.db and its write-ahead log.
func (e *env) dbFiles() int64 {
	e.t.Helper()
	var total int64
	for _, suffix := range []string{"", "-wal"} {
		info, err := os.Stat(e.in.path + suffix)
		if err == nil {
			total += info.Size()
		} else if !os.IsNotExist(err) {
			e.t.Fatal(err)
		}
	}
	return total
}

// ignoredOps builds an action of maxActionOps operations on a tab that does
// not exist: each is ignored, the action changes nothing, and its receipt
// is about 36 KB.
func ignoredOps() string {
	op := `{"op":"set","tab":999,"row":1,"col":1,"text":"x","at":[0,0]}`
	return "[" + strings.Repeat(op+",", maxActionOps-1) + op + "]"
}

// TestReceiptsCountAgainstTheQuota sends a session that has its row 1,024
// actions of 1,000 operations after the quota was lowered below the sheet,
// and 64 that change nothing. Each is refused for the quota, and sheet.db
// does not grow: the room of a receipt is reserved before an action
// applies, and an action without it is refused and stored nowhere. Its
// number stays free, and the receipt kept from before answers as it did.
func TestReceiptsCountAgainstTheQuota(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["kept"]]}]}`)
	e.b.importAll(t)
	alice := e.person("Alice")
	keptBase, keptOp := e.seq(), e.setOp("C3", "before")
	kept := alice.act(keptBase, keptOp)
	e.b.setQuota(1)
	if err := e.in.Remeasure(context.Background()); err != nil {
		t.Fatal(err)
	}
	var receipts int
	count := func() (n int) {
		t.Helper()
		if err := e.in.rdb.QueryRow(`SELECT count(*) FROM receipts`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	receipts = count()
	files := e.dbFiles()
	refused := e.refusals()
	sends := raced(1024, 64)
	for n := uint64(2); n < uint64(sends)+2; n++ {
		ack, err := submitRaw(e, alice.s, n, e.seq(), refused)
		if err != nil || bytes.Count(ack, []byte(`"reason":"disk_quota"`)) != maxActionOps {
			t.Fatalf("action %d over the quota: %.120s, %v", n, ack, err)
		}
	}
	stayed := func(what string) {
		t.Helper()
		grew := e.dbFiles() - files
		t.Logf("%s: sheet.db and its log grew by %d bytes", what, grew)
		if got := count(); got != receipts {
			t.Fatalf("%s: %d receipts were stored over the quota", what, got-receipts)
		}
		if grew > 64<<10 {
			t.Fatalf("%s: sheet.db and its log grew by %d bytes over the quota", what, grew)
		}
	}
	stayed(fmt.Sprint(sends, " refused actions"))
	ignored := ignoredOps()
	for n := uint64(sends) + 2; n < uint64(sends)+66; n++ {
		ack, err := submitRaw(e, alice.s, n, e.seq(), ignored)
		if err != nil || bytes.Count(ack, []byte(`"reason":"disk_quota"`)) != maxActionOps {
			t.Fatalf("action %d that changes nothing over the quota: %.120s, %v", n, ack, err)
		}
	}
	stayed("64 actions that change nothing")
	// The receipt from before answers as it did; a refused number was not
	// used and is decided again.
	if ack, err := alice.send(1, keptBase, keptOp); err != nil || ack.Seq != kept.Seq || status(ack) != "applied" {
		t.Fatalf("the action from before the quota sent again: %+v, %v", ack, err)
	}
	if ack, err := submitRaw(e, alice.s, 2, e.seq(), refused); err != nil || !bytes.Contains(ack, []byte(`"reason":"disk_quota"`)) {
		t.Fatalf("a refused action sent again: %.120s, %v", ack, err)
	}
	e.b.setQuota(0)
	ack, err := submitRaw(e, alice.s, 2, e.seq(), refused)
	if err != nil || !bytes.Contains(ack, []byte(`"status":"applied"`)) {
		t.Fatalf("the refused action once the quota is raised: %.120s, %v", ack, err)
	}
	if got := count(); got != receipts+1 {
		t.Fatalf("the applied action stored %d receipts", got-receipts)
	}
}

// TestReceiptsNearTheQuota gives a sheet 512 KiB more than its usage and
// sends a session with its row edits that succeed and actions of 1,000
// operations that change nothing, until both are refused. Every receipt
// is counted before its action is answered, so sheet.db and its log never
// take more than the quota beyond the pages that were free at the start.
func TestReceiptsNearTheQuota(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["kept"]]}]}`)
	e.b.importAll(t)
	alice := e.person("Alice")
	alice.set("A2", "first")
	total, _ := e.measured()
	quota := total + 512<<10
	e.b.setQuota(quota)
	if err := e.in.Remeasure(context.Background()); err != nil {
		t.Fatal(err)
	}
	var free, pageSize int64
	if err := e.in.rdb.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
		t.Fatal(err)
	}
	if err := e.in.rdb.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	ignored := ignoredOps()
	text := strings.Repeat("w", 2<<10)
	var applied, unchanged, refused int
	for i := 0; i < 400 && refused < 8; i++ {
		var ack json.RawMessage
		var err error
		if i%2 == 0 {
			alice.n++
			ack, err = submitRaw(e, alice.s, alice.n, e.seq(), "["+e.setOp(fmt.Sprintf("B%d", i+1), text)+"]")
		} else {
			alice.n++
			ack, err = submitRaw(e, alice.s, alice.n, e.seq(), ignored)
		}
		switch {
		case err != nil:
			t.Fatalf("action %d: %v", alice.n, err)
		case bytes.Contains(ack, []byte(`"reason":"disk_quota"`)):
			refused++
		case bytes.Contains(ack, []byte(`"status":"applied"`)):
			applied++
		default:
			unchanged++
		}
		if got := e.dbFiles(); got > quota+free*pageSize {
			t.Fatalf("after %d applied, %d unchanged and %d refused actions sheet.db and its log take %d bytes, the quota is %d", applied, unchanged, refused, got, quota)
		}
	}
	t.Logf("%d applied, %d unchanged, %d refused: sheet.db and its log take %d bytes of the quota's %d", applied, unchanged, refused, e.dbFiles(), quota)
	if applied == 0 || unchanged == 0 || refused < 8 {
		t.Fatalf("%d applied, %d unchanged and %d refused actions", applied, unchanged, refused)
	}
}

// TestSessionExpiryReadsOnlyExpiredRows checks that maintenance finds the
// sessions and receipts that expired by the index of last_at.
func TestSessionExpiryReadsOnlyExpiredRows(t *testing.T) {
	wantIndexedExpiry(t, start(t).in.rdb)
}

// wantIndexedExpiry asks SQLite how maintenance finds the sessions and
// receipts that expired: by the index of last_at, so the work does not
// grow with the sessions that are still kept.
func wantIndexedExpiry(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, query := range []string{
		`DELETE FROM receipts WHERE session IN (SELECT id FROM sessions WHERE last_at<=?1)`,
		`DELETE FROM sessions WHERE last_at<=?1`,
	} {
		rows, err := db.Query(`EXPLAIN QUERY PLAN `+query, 0)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(plan, "; ")
		if !strings.Contains(joined, "sessions_last") || strings.Contains(joined, "SCAN sessions") || strings.Contains(joined, "SCAN receipts") {
			t.Fatalf("%s: %s", query, joined)
		}
	}
}
