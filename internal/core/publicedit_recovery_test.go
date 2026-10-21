package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/store"
	"github.com/gosuda/flats/internal/wsconn"
)

// joinBoardGuest opens the canvas socket of the Public route at base as a
// guest named name and returns the client with the live elements of its
// snapshot.
func joinBoardGuest(t *testing.T, base, name string) (*boardClient, []map[string]any) {
	t.Helper()
	host := strings.TrimPrefix(base, "http://")
	ws, err := wsconn.Dial(context.Background(), func(ctx context.Context) (net.Conn, error) {
		c, err := net.Dial("tcp", host)
		if err != nil {
			return nil, err
		}
		return &originConn{Conn: c, origin: base}, nil
	}, host, "/_board/ws")
	if err != nil {
		t.Fatal(err)
	}
	c := &boardClient{t: t, ws: ws, msgs: make(chan map[string]any, 256)}
	go func() {
		defer close(c.msgs)
		for {
			raw, err := ws.ReadMessage(64 << 20)
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal([]byte(raw), &m) == nil {
				c.msgs <- m
			}
		}
	}()
	t.Cleanup(func() { ws.Close(1000, "") })
	c.send(map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}, "guest": map[string]any{"name": name}})
	if w := c.expect("welcome"); w["access"] != "guest" {
		t.Fatalf("guest welcome = %v", w)
	}
	var els []map[string]any
	for {
		m := c.expect("snapshot")
		for _, e := range m["elements"].([]any) {
			els = append(els, e.(map[string]any))
		}
		if m["part"] == m["parts"] {
			return c, els
		}
	}
}

// deleteEverything sends one guest batch deleting every element of els.
func deleteEverything(t *testing.T, c *boardClient, n int, els []map[string]any) {
	t.Helper()
	ids := []string{}
	incs := map[string]any{}
	for _, el := range els {
		id := el["id"].(string)
		ids = append(ids, id)
		incs[id] = el["inc"]
	}
	ack := c.batch(n, map[string]any{"op": "delete", "ids": ids, "inc_of": incs})
	if ack["t"] != "ack" {
		t.Fatalf("hostile delete = %v", ack)
	}
}

// historyRequest calls the board's History route on the Private canvas at
// base, as its History panel does.
func historyRequest(t *testing.T, base, method, path string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(method, base+"/_board/api/revisions"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", base)
	req.Header.Set("X-Flats-Board", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s = %d %v %v", method, path, resp.StatusCode, out, err)
	}
	return out
}

// revisionElements is a revision's elements, as the History panel opens it,
// by id in canonical form.
func revisionElements(t *testing.T, base string, n int) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, raw := range historyRequest(t, base, "GET", fmt.Sprintf("/%d/elements", n))["elements"].([]any) {
		el := raw.(map[string]any)
		b, _ := json.Marshal(el)
		out[el["id"].(string)] = string(b)
	}
	return out
}

// currentElements is the running board's elements by id, canonical, in the
// same form as revisionElements: get_board's ver is dropped and its
// text_authors and last_change are written as the stored ta and lk.
func currentElements(t *testing.T, s *Service) map[string]string {
	t.Helper()
	out := map[string]string{}
	for id, el := range boardElements(t, s, "plan", 0) {
		delete(el, "ver")
		if lk, ok := el["last_change"]; ok {
			el["lk"] = lk
			delete(el, "last_change")
		}
		if ta, ok := el["text_authors"].(map[string]any); ok {
			stored := map[string]any{}
			for bundle, raw := range ta {
				a := raw.(map[string]any)
				e := map[string]any{"k": a["kind"]}
				if n, ok := a["name"]; ok {
					e["n"] = n
				}
				stored[bundle] = e
			}
			el["ta"] = stored
			delete(el, "text_authors")
		}
		b, _ := json.Marshal(el)
		out[id] = string(b)
	}
	return out
}

func sameElements(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if len(want) == 0 {
		t.Fatalf("%s: the expected board is empty", what)
	}
	for id, w := range want {
		if got[id] != w {
			t.Fatalf("%s: element %s\n got  %s\n want %s", what, id, got[id], w)
		}
	}
	for id := range got {
		if _, ok := want[id]; !ok {
			t.Fatalf("%s: extra element %s %s", what, id, got[id])
		}
	}
}

// TestPublicEditRecoveryAfterHostileGuest is the Recovery criterion of the
// spec (board-public-edit-TECH 10) with the real board under core: a guest
// on the Public route deletes every element; History still lists the safe
// point and the revision from just before the session, and the operator
// restores the board to each of them from History.
func TestPublicEditRecoveryAfterHostileGuest(t *testing.T) {
	ctx := context.Background()
	s, _ := newPublicBoard(t)
	s.publicBoardHook = nil
	priv := httptest.NewServer(s.siteHandler("plan", false))
	defer priv.Close()
	pub := httptest.NewServer(s.siteHandler("plan", true))
	defer pub.Close()

	member := dialBoard(t, priv.URL)
	if ack := member.batch(1, sticky("e_member0001", "member note", 0), sticky("e_member0002", "second note", 300)); ack["t"] != "ack" {
		t.Fatalf("member batch = %v", ack)
	}
	writeBoard(t, s, "plan", "recovery-agent-1", `[{"op":"add","el":{"type":"frame","text":"Agenda","x":0,"y":400,"w":600,"h":400}}]`)
	a := requestAccess(t, s, store.Public, store.AccessEdit, ViaConsole)
	if got, err := s.Decide(ctx, a.ID, true); err != nil || got.Status != "approved" {
		t.Fatalf("enable guest editing = %+v %v", got, err)
	}
	sp := safePointRev(t, s)
	// Work after the safe point: the revision just before the guests'
	// session differs from the safe point.
	if ack := member.batch(2, sticky("e_member0003", "after the safe point", 600)); ack["t"] != "ack" {
		t.Fatalf("member batch = %v", ack)
	}
	res := writeBoard(t, s, "plan", "recovery-agent-2", `[{"op":"add","el":{"type":"sticky","text":"just before the guests","x":900,"y":0}}]`)
	if res.Revision == nil {
		t.Fatalf("agent write recorded no revision: %+v", res)
	}
	before := *res.Revision
	wantBefore := revisionElements(t, priv.URL, before)
	wantSafe := revisionElements(t, priv.URL, sp)
	if len(wantBefore) != len(wantSafe)+2 {
		t.Fatalf("revision %d has %d elements, safe point %d has %d", before, len(wantBefore), sp, len(wantSafe))
	}
	sameElements(t, "board before the session", currentElements(t, s), wantBefore)

	// The hostile session deletes everything.
	guest, els := joinBoardGuest(t, pub.URL, "Mallory")
	if len(els) != len(wantBefore) {
		t.Fatalf("guest snapshot has %d elements, the board %d", len(els), len(wantBefore))
	}
	deleteEverything(t, guest, 1, els)
	if n := len(boardElements(t, s, "plan", 0)); n != 0 {
		t.Fatalf("%d elements left after the hostile delete", n)
	}

	// History still lists both, and the safe point is still pinned.
	listed := map[int]bool{}
	for _, raw := range historyRequest(t, priv.URL, "GET", "?limit=100")["revisions"].([]any) {
		r := raw.(map[string]any)
		listed[int(r["number"].(float64))] = true
		if int(r["number"].(float64)) == sp && r["pinned"] != true {
			t.Fatalf("safe point %d is not pinned: %v", sp, r)
		}
	}
	if !listed[sp] || !listed[before] {
		t.Fatalf("History lists %v, want the safe point %d and revision %d", listed, sp, before)
	}

	// The operator restores the revision from just before the session.
	r := historyRequest(t, priv.URL, "POST", fmt.Sprintf("/%d/rollback", before))
	restored := int(r["revision"].(float64))
	sameElements(t, "board after restoring the revision before the session", currentElements(t, s), wantBefore)
	sameElements(t, "rollback revision", revisionElements(t, priv.URL, restored), wantBefore)

	// Another hostile guest deletes everything again; the operator restores
	// the safe point.
	guest2, els := joinBoardGuest(t, pub.URL, "Mallory again")
	if len(els) != len(wantBefore) {
		t.Fatalf("second guest snapshot has %d elements, want %d", len(els), len(wantBefore))
	}
	deleteEverything(t, guest2, 1, els)
	if n := len(boardElements(t, s, "plan", 0)); n != 0 {
		t.Fatalf("%d elements left after the second hostile delete", n)
	}
	r = historyRequest(t, priv.URL, "POST", fmt.Sprintf("/%d/rollback", sp))
	restored = int(r["revision"].(float64))
	sameElements(t, "board after restoring the safe point", currentElements(t, s), wantSafe)
	sameElements(t, "safe point rollback revision", revisionElements(t, priv.URL, restored), wantSafe)
	if !slices.Contains(pinnedRevisions(t, s), sp) {
		t.Fatalf("safe point %d not pinned after recovery: %v", sp, pinnedRevisions(t, s))
	}
}

// joinAs says hello on the Public route at base with a guest name and
// returns the welcome's access: "guest" when editing is open, "public" for
// a viewer.
func joinAs(t *testing.T, base, name string) string {
	t.Helper()
	host := strings.TrimPrefix(base, "http://")
	ws, err := wsconn.Dial(context.Background(), func(ctx context.Context) (net.Conn, error) {
		c, err := net.Dial("tcp", host)
		if err != nil {
			return nil, err
		}
		return &originConn{Conn: c, origin: base}, nil
	}, host, "/_board/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close(1000, "")
	b, _ := json.Marshal(map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}, "guest": map[string]any{"name": name}})
	if err := ws.WriteFrame(wsconn.OpText, b); err != nil {
		t.Fatal(err)
	}
	for {
		raw, err := ws.ReadMessage(64 << 20)
		if err != nil {
			t.Fatalf("no welcome: %v", err)
		}
		var m map[string]any
		if json.Unmarshal([]byte(raw), &m) == nil && m["t"] == "welcome" {
			access, _ := m["access"].(string)
			return access
		}
	}
}

// TestPublicEditStopWhileBoardStopped is the Kill switch criterion's last
// sentence: Stop guest editing on a board whose instance is not running
// changes the stored level alone, and the board applies it when it starts
// again, so a guest hello is then a viewer.
func TestPublicEditStopWhileBoardStopped(t *testing.T) {
	ctx := context.Background()
	s, _ := newPublicBoard(t)
	s.publicBoardHook = nil
	pub := httptest.NewServer(s.siteHandler("plan", true))
	defer pub.Close()
	setAccess(t, s, store.Public, store.AccessEdit)
	if got := joinAs(t, pub.URL, "Before"); got != "guest" {
		t.Fatalf("at edit a guest hello got %q", got)
	}
	lf := s.state("plan")
	old := lf.cur.Swap(nil)
	if old == nil {
		t.Fatal("no running board")
	}
	stopInstance(old.inst, "")
	if s.liveBoard("plan") != nil {
		t.Fatal("the board still runs")
	}
	setAccess(t, s, store.Public, store.AccessView)
	if vis, access := flatAccess(t, s); vis != store.Public || access != store.AccessView {
		t.Fatalf("stored level after Stop = %s %q", vis, access)
	}
	if err := s.restart(ctx, "plan", old); err != nil {
		t.Fatal(err)
	}
	if s.liveBoard("plan") == nil {
		t.Fatal("the board did not start again")
	}
	if got := joinAs(t, pub.URL, "After"); got != "public" {
		t.Fatalf("after Stop on a stopped board, a guest hello got %q", got)
	}
}
