package docs

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Anonymous public editing in the document worker
// (docs/spec/docs-public-edit-TECH.md 3).

// --- Yjs version 1 updates, encoded for tests ---

func varuint(b []byte, n uint64) []byte {
	for n >= 0x80 {
		b = append(b, byte(n)|0x80)
		n >>= 7
	}
	return append(b, byte(n))
}

func varstring(b []byte, s string) []byte {
	return append(varuint(b, uint64(len(s))), s...)
}

// yItem is an inserted string: at the start of the text, or right after
// origin (client, clock) when origin is set.
type yItem struct {
	client, clock uint64
	text          string
	origin        *[2]uint64
}

// yDel deletes length characters of client from clock.
type yDel struct{ client, clock, length uint64 }

// yUpdate encodes one update: each item as its own client group, then the
// delete set. Texts are ASCII, so their Yjs length is their byte length.
func yUpdate(items []yItem, dels []yDel) string {
	b := varuint(nil, uint64(len(items)))
	for _, it := range items {
		b = varuint(b, 1)
		b = varuint(b, it.client)
		b = varuint(b, it.clock)
		if it.origin != nil {
			b = append(b, 0x84) // origin, string content
			b = varuint(b, it.origin[0])
			b = varuint(b, it.origin[1])
		} else {
			b = append(b, 4) // string content
			b = varuint(b, 1)
			b = varstring(b, "markdown")
		}
		b = varstring(b, it.text)
	}
	b = varuint(b, uint64(len(dels)))
	for _, d := range dels {
		b = varuint(b, d.client)
		b = varuint(b, 1)
		b = varuint(b, d.clock)
		b = varuint(b, d.length)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// yInsert inserts text at the start of the document as a new client.
func yInsert(client uint64, text string) string {
	return yUpdate([]yItem{{client: client, text: text}}, nil)
}

// yAfter continues client's own text after its character at clock-1.
func yAfter(client, clock uint64, text string) string {
	return yUpdate([]yItem{{client: client, clock: clock, text: text, origin: &[2]uint64{client, clock - 1}}}, nil)
}

// yGC is a content-less clock jump.
func yGC(client, clock, length uint64) string {
	b := varuint(varuint(varuint(varuint(varuint(nil, 1), 1), client), clock), 0)
	b = varuint(b, length)
	return base64.StdEncoding.EncodeToString(varuint(b, 0))
}

// stateVector decodes a base64 Yjs state vector.
func stateVector(t *testing.T, s string) map[uint64]uint64 {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(b)
	read := func() uint64 {
		n, err := binary.ReadUvarint(r)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	sv := map[uint64]uint64{}
	for n := read(); n > 0; n-- {
		client := read()
		sv[client] = read()
	}
	return sv
}

func encodeStateVector(sv map[uint64]uint64) string {
	b := varuint(nil, uint64(len(sv)))
	for client, clock := range sv {
		b = varuint(varuint(b, client), clock)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// presence encodes one awareness state.
func presence(id, clock uint64, state any) string {
	j, _ := json.Marshal(state)
	b := varuint(varuint(varuint(nil, 1), id), clock)
	return base64.StdEncoding.EncodeToString(varstring(b, string(j)))
}

// presenceStates decodes an aw frame's states.
func presenceStates(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(m["u"].(string))
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(b)
	n, _ := binary.ReadUvarint(r)
	var out []map[string]any
	for ; n > 0; n-- {
		id, _ := binary.ReadUvarint(r)
		binary.ReadUvarint(r)
		size, _ := binary.ReadUvarint(r)
		raw := make([]byte, size)
		r.Read(raw)
		var state map[string]any
		json.Unmarshal(raw, &state)
		out = append(out, map[string]any{"id": float64(id), "state": state})
	}
	return out
}

// awaitPresence reads aw frames until one holds the state of id.
func awaitPresence(t *testing.T, s *socket, id uint64) map[string]any {
	t.Helper()
	for {
		for _, st := range presenceStates(t, s.next(t, "aw")) {
			if st["id"] == float64(id) {
				return st["state"].(map[string]any)
			}
		}
	}
}

// --- connections ---

// connectWith opens a WebSocket to a document with request headers.
func connectWith(t *testing.T, url, doc string, headers map[string]string) (*socket, error) {
	t.Helper()
	host := strings.TrimPrefix(url, "http://")
	c, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 16)
	rand.Read(key)
	request := "GET /_docs/ws?doc=" + doc + " HTTP/1.1\r\nHost: " + host + "\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(key) + "\r\n"
	for k, v := range headers {
		request += k + ": " + v + "\r\n"
	}
	if _, err = c.Write([]byte(request + "\r\n")); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(c)
	r, err := http.ReadResponse(reader, nil)
	if err != nil || r.StatusCode != 101 {
		c.Close()
		if err == nil {
			err = fmt.Errorf("upgrade: %s", r.Status)
		}
		return nil, err
	}
	s := &socket{Conn: c, reader: reader}
	t.Cleanup(func() { c.Close() })
	return s, nil
}

func mustConnect(t *testing.T, url, doc string, headers map[string]string) *socket {
	t.Helper()
	s, err := connectWith(t, url, doc, headers)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// publicHeaders are the trusted headers core sets on a Public request.
func publicHeaders(level string, epoch int64) map[string]string {
	return map[string]string{"X-Flats-Access": "public", "X-Flats-Public-Access": level, "X-Flats-Access-Epoch": fmt.Sprint(epoch)}
}

var memberHeaders = map[string]string{"X-Flats-Access": "private"}

func (a *app) setAccess(t *testing.T, level string, epoch int64) {
	t.Helper()
	if status, out := a.hostOp(t, "POST", "access", map[string]any{"level": level, "epoch": epoch}); status != 200 {
		t.Fatal("access", status, out)
	}
}

// guest joins doc as a guest at the flat's edit epoch.
func (a *app) guest(t *testing.T, doc, name, token string, epoch int64) (*socket, map[string]any) {
	t.Helper()
	s := mustConnect(t, a.srv.URL, doc, publicHeaders("edit", epoch))
	g := map[string]any{"name": name}
	if token != "" {
		g["token"] = token
	}
	s.send(t, map[string]any{"t": "hello", "v": 1, "doc": doc, "sv": "AA==", "guest": g})
	return s, s.next(t, "welcome")
}

// member joins doc on a Private route with a page nickname.
func (a *app) member(t *testing.T, doc, nick string, headers map[string]string) (*socket, map[string]any) {
	t.Helper()
	h := map[string]string{"X-Flats-Access": "private"}
	for k, v := range headers {
		h[k] = v
	}
	s := mustConnect(t, a.srv.URL, doc, h)
	s.send(t, map[string]any{"t": "hello", "v": 1, "doc": doc, "sv": "AA==", "name": "Page name", "nick": nick, "editor": "browser-secret-id-123"})
	return s, s.next(t, "welcome")
}

// viewer joins doc on a Public route, with an earlier viewer token.
func (a *app) viewer(t *testing.T, doc, token string, headers map[string]string) (*socket, map[string]any) {
	t.Helper()
	s := mustConnect(t, a.srv.URL, doc, headers)
	s.send(t, map[string]any{"t": "hello", "v": 1, "doc": doc, "sv": "AA==", "viewer_token": token})
	return s, s.next(t, "welcome")
}

func (a *app) db(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(a.data, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func (a *app) markdown(t *testing.T, doc string) string {
	t.Helper()
	status, b, _ := a.getHeaders(t, "/_docs/api/document?doc="+doc, http.Header{"X-Flats-Access": {"private"}})
	if status != 200 {
		t.Fatal(status, b)
	}
	var m map[string]any
	json.Unmarshal([]byte(b), &m)
	return m["markdown"].(string)
}

// update sends an update and returns the answer frame for it: ack, busy or
// error.
func (s *socket) update(t *testing.T, id, u string) map[string]any {
	t.Helper()
	s.send(t, map[string]any{"t": "update", "id": id, "u": u})
	for {
		m, err := s.read()
		if err != nil {
			t.Fatalf("waiting for an answer to %s: %v", id, err)
		}
		if n, ok := m["n"].(float64); ok {
			s.send(t, map[string]any{"t": "received", "n": n})
		}
		switch m["t"] {
		case "ack", "busy":
			if m["id"] == id {
				return m
			}
		case "error":
			return m
		}
		s.inbox = append(s.inbox, m)
	}
}

// refused skips other frames until a fatal error with code and its close.
func (s *socket) refused(t *testing.T, code string) {
	t.Helper()
	for {
		m, err := s.read()
		if err != nil {
			t.Fatalf("waiting for error %s: %v", code, err)
		}
		if m["t"] == "error" {
			expectCode(t, m, code)
			s.closed(t, 1008, code)
			return
		}
	}
}

func expectCode(t *testing.T, m map[string]any, code string) {
	t.Helper()
	if m["t"] != "error" || m["code"] != code {
		t.Fatalf("expected error %s, got %v", code, m)
	}
}

func acked(t *testing.T, m map[string]any) {
	t.Helper()
	if m["t"] != "ack" {
		t.Fatalf("expected ack, got %v", m)
	}
}

// docsModule is a content module of documents with the given texts.
func docsModule(version string, texts map[string]string) string {
	var docs []any
	entry := ""
	for path, text := range texts {
		sum := sha256.Sum256([]byte(text))
		docs = append(docs, map[string]any{"path": path, "hash": hex.EncodeToString(sum[:]), "text": text})
		if entry == "" || path < entry {
			entry = path
		}
	}
	b, _ := json.Marshal(map[string]any{"format": 1, "hash": version, "entry": entry, "title": "Shared", "documents": docs, "assets": []string{}})
	return "export default " + string(b) + ";\n"
}

// --- tests ---

func TestGuestAndViewerAdmission(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	a.setAccess(t, "edit", 3)
	g, w := a.guest(t, "index.md", "  Mina​ ", "", 3)
	me, _ := w["me"].(map[string]any)
	token, _ := w["guest_token"].(string)
	if me["kind"] != "guest" || me["name"] != "Mina" || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(token) ||
		w["access"] != "edit" || w["access_epoch"] != float64(3) || w["readonly"] != false || w["guest_text"] != true ||
		w["you"].(map[string]any)["verified"] != false {
		t.Fatal(w)
	}
	acked(t, g.update(t, "g1", yInsert(5001, "guest text\n")))
	// The token keeps the guest's key: the same guest continues its text.
	g.Close()
	again, w2 := a.guest(t, "index.md", "Mina", token, 3)
	if _, issued := w2["guest_token"]; issued {
		t.Fatal("a known token was replaced", w2)
	}
	acked(t, again.update(t, "g2", yAfter(5001, 11, "more")))

	for _, name := range []string{"", "   ", "​‎", strings.Repeat("x", 41)} {
		s := mustConnect(t, a.srv.URL, "index.md", publicHeaders("edit", 3))
		s.send(t, map[string]any{"t": "hello", "v": 1, "doc": "index.md", "sv": "AA==", "guest": map[string]any{"name": name}})
		s.rejected(t, "name_invalid")
	}
	// Without a guest hello, with another epoch or at view, a Public socket
	// is a viewer: it cannot edit.
	for _, h := range []map[string]string{publicHeaders("edit", 3), publicHeaders("edit", 2), publicHeaders("view", 3)} {
		s := mustConnect(t, a.srv.URL, "index.md", h)
		hello := map[string]any{"t": "hello", "v": 1, "doc": "index.md", "sv": "AA=="}
		if h["X-Flats-Access-Epoch"] != "3" || h["X-Flats-Public-Access"] != "edit" {
			hello["guest"] = map[string]any{"name": "Forger"}
		}
		s.send(t, hello)
		w := s.next(t, "welcome")
		if w["me"].(map[string]any)["kind"] != "viewer" || w["readonly"] != true {
			t.Fatal(h, w)
		}
		s.send(t, map[string]any{"t": "update", "id": "forged", "u": yInsert(5002, "forged")})
		s.rejected(t, "readonly")
	}
	if strings.Contains(a.markdown(t, "index.md"), "forged") {
		t.Fatal("a viewer's update committed")
	}
}

func TestViewerNumbers(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	a.setAccess(t, "view", 1)
	m, _ := a.member(t, "index.md", "Blue Fox", nil)
	h := publicHeaders("view", 1)
	label := func(w map[string]any) string { return w["me"].(map[string]any)["label"].(string) }
	v1, w1 := a.viewer(t, "index.md", "", h)
	v2, w2 := a.viewer(t, "index.md", "", h)
	if label(w1) != "Viewer 1" || label(w2) != "Viewer 2" || w1["access"] != "view" || w1["guest_text"] != false {
		t.Fatal(w1, w2)
	}
	// Members see the room's viewers.
	if f := m.next(t, "viewers"); fmt.Sprint(f["n"]) != "[1]" && fmt.Sprint(f["n"]) != "[1 2]" {
		t.Fatal(f)
	}
	// The number survives a reconnect within a minute, in another document;
	// a released number is held meanwhile.
	v1.Close()
	time.Sleep(100 * time.Millisecond)
	_, w3 := a.viewer(t, "index.md", "", h)
	if label(w3) != "Viewer 3" {
		t.Fatal(w3)
	}
	_, back := a.viewer(t, "nested/other.md", w1["viewer_token"].(string), h)
	if label(back) != "Viewer 1" || back["viewer_token"] != w1["viewer_token"] {
		t.Fatal(back)
	}
	// An unknown token gets a new number.
	_, fresh := a.viewer(t, "index.md", strings.Repeat("ab", 16), h)
	if label(fresh) != "Viewer 4" || fresh["viewer_token"] == strings.Repeat("ab", 16) {
		t.Fatal(fresh)
	}
	v2.send(t, map[string]any{"t": "aw", "u": presence(9, 1, map[string]any{})})
	v2.rejected(t, "readonly")
}

// No frame a guest receives carries a member's verified identity, its
// browser editor id or an agent label; guests see members under their page
// nickname, members see guests with the guest mark (TECH 3.4).
func TestGuestProjectionHidesMemberIdentity(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	a.setAccess(t, "edit", 2)
	tailscale := map[string]string{"Tailscale-User-Login": "alice@example.com", "Tailscale-User-Name": "Alice Example"}
	m, mw := a.member(t, "index.md", "Blue Fox", tailscale)
	if mw["you"].(map[string]any)["name"] != "Alice Example" || mw["access"] != "edit" {
		t.Fatal(mw)
	}
	m.send(t, map[string]any{"t": "aw", "u": presence(301, 1, map[string]any{"user": map[string]any{"name": "alice@example.com"}})})
	m.next(t, "aw")
	plain, _ := a.member(t, "index.md", "", nil)
	plain.send(t, map[string]any{"t": "aw", "u": presence(302, 1, map[string]any{})})
	awaitPresence(t, plain, 302)
	g, w := a.guest(t, "index.md", "Mina", "", 2)
	var frames []map[string]any
	frames = append(frames, w)
	names := map[string]bool{}
	for _, s := range presenceStates(t, g.next(t, "aw")) {
		user := s["state"].(map[string]any)["user"].(map[string]any)
		if user["verified"] != false {
			t.Fatal("verified member shown to a guest", user)
		}
		names[user["name"].(string)] = true
	}
	if !names["Blue Fox"] || !names["Member"] || len(names) != 2 {
		t.Fatal(names)
	}
	// A member edit, an agent edit and presence while the guest watches.
	acked(t, m.update(t, "m1", yInsert(303, "member text\n")))
	if status, res := a.postEdit(t, http.Header{"X-Flats-Access": {"private"}, "X-Flats-Host-Op": {"edit"}},
		`{"label":"Agent Smith","ops":[{"op":"insert","at":"end","text":"agent text\n"}]}`); status != 200 {
		t.Fatal(status, res)
	}
	m.send(t, map[string]any{"t": "aw", "u": presence(301, 2, map[string]any{"cursor": nil})})
	g.send(t, map[string]any{"t": "aw", "u": presence(304, 1, map[string]any{})})
	// Members see the guest with the guest mark.
	user := awaitPresence(t, m, 304)["user"].(map[string]any)
	if user["name"] != "Mina" || user["guest"] != true || user["verified"] != false {
		t.Fatal(user)
	}
	g.send(t, map[string]any{"t": "ping"})
	frames = append(frames, g.next(t, "pong"))
	frames = append(frames, g.inbox...)
	g.inbox = nil
	for _, f := range frames {
		raw, _ := json.Marshal(f)
		text := string(raw)
		if u, ok := f["u"].(string); ok {
			b, _ := base64.StdEncoding.DecodeString(u)
			text += string(b)
		}
		for _, forbidden := range []string{"alice@example.com", "Alice Example", "browser-secret-id-123", "Agent Smith", "ts:"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("a guest received %q in %s", forbidden, text)
			}
		}
	}
}

// Deleted text never reaches a guest or a viewer, whatever state vector it
// joins with, and updates leaving pending data are refused (TECH 3.7).
func TestCurrentTextOnly(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	a.setAccess(t, "edit", 1)
	m, _ := a.member(t, "index.md", "", nil)
	acked(t, m.update(t, "mark", yInsert(400, "SECRETMARKER")))
	acked(t, m.update(t, "unmark", yUpdate(nil, []yDel{{400, 0, 12}})))
	old := encodeStateVector(map[uint64]uint64{400: 0})
	join := func(h map[string]string, hello map[string]any) []map[string]any {
		s := mustConnect(t, a.srv.URL, "index.md", h)
		s.send(t, hello)
		frames := []map[string]any{s.next(t, "welcome")}
		s.send(t, map[string]any{"t": "ping"})
		frames = append(frames, s.next(t, "pong"))
		return append(frames, s.inbox...)
	}
	for _, sv := range []string{"AA==", old} {
		guestHello := map[string]any{"t": "hello", "v": 1, "doc": "index.md", "sv": sv, "guest": map[string]any{"name": "Mina"}}
		viewerHello := map[string]any{"t": "hello", "v": 1, "doc": "index.md", "sv": sv}
		for _, frames := range [][]map[string]any{join(publicHeaders("edit", 1), guestHello), join(publicHeaders("edit", 1), viewerHello)} {
			for _, f := range frames {
				raw, _ := json.Marshal(f)
				data := raw
				if u, ok := f["u"].(string); ok {
					b, _ := base64.StdEncoding.DecodeString(u)
					data = append(data, b...)
				}
				if bytes.Contains(data, []byte("SECRETMARKER")) {
					t.Fatalf("deleted text reached a public socket: %s", raw)
				}
			}
		}
	}
	// Pending structs and deletions are refused, from members and guests.
	g, _ := a.guest(t, "index.md", "Mina", "", 1)
	pending := yUpdate([]yItem{{client: 401, text: "x", origin: &[2]uint64{402, 0}}}, nil)
	expectCode(t, m.update(t, "pending", pending), "invalid_update")
	expectCode(t, g.update(t, "pending", pending), "invalid_update")
	m2, _ := a.member(t, "index.md", "", nil)
	expectCode(t, m2.update(t, "pending-ds", yUpdate(nil, []yDel{{403, 0, 3}})), "invalid_update")
}

// Data stored before pending updates were refused is dropped when the
// document loads: visible text stays, welcomes and checkpoints carry none
// of it, and later edits commit.
func TestStoredPendingDataIsDropped(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	a.document(t)
	db := a.db(t)
	for i, u := range []string{
		yUpdate([]yItem{{client: 410, text: "PENDINGSTRUCT", origin: &[2]uint64{411, 0}}}, nil),
		yUpdate(nil, []yDel{{412, 0, 5}}),
	} {
		var seq int64
		var chain string
		if err := db.QueryRow("SELECT seq,chain FROM flats_docs_documents WHERE doc='index.md'").Scan(&seq, &chain); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprint("stored-", i)
		next := digestOf(chain + "\n" + id)
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{"INSERT INTO flats_docs_updates VALUES('index.md',?,?,?,?,?,0)", []any{seq + 1, id, next, u, len(u)}},
			{"INSERT INTO flats_docs_receipts VALUES('index.md',?,?,?,?)", []any{seq + 1, id, next, digestOf(u)}},
			{"UPDATE flats_docs_documents SET seq=?,chain=? WHERE doc='index.md'", []any{seq + 1, next}},
		} {
			if _, err := db.Exec(q.sql, q.args...); err != nil {
				t.Fatal(err)
			}
		}
	}
	if a.markdown(t, "index.md") != initialText {
		t.Fatal("stored pending data changed the visible text")
	}
	m, w := a.member(t, "index.md", "", nil)
	if b, _ := base64.StdEncoding.DecodeString(w["u"].(string)); bytes.Contains(b, []byte("PENDINGSTRUCT")) {
		t.Fatal("a welcome carried pending data")
	}
	acked(t, m.update(t, "after", yInsert(413, "after\n")))
	var state string
	if err := db.QueryRow("SELECT state FROM flats_docs_checkpoints WHERE doc='index.md'").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if b, _ := base64.StdEncoding.DecodeString(state); bytes.Contains(b, []byte("PENDINGSTRUCT")) {
		t.Fatal("the checkpoint kept pending data")
	}
	if a.markdown(t, "index.md") != "after\n"+initialText {
		t.Fatal(a.markdown(t, "index.md"))
	}
}

// A guest inserts only with client ids new to the document or its own;
// existing text never becomes guest-written (TECH 3.5).
func TestGuestClientOwnership(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	a.setAccess(t, "edit", 1)
	m, mw := a.member(t, "index.md", "", nil)
	seed := uint64(0)
	for client := range stateVector(t, mw["sv"].(string)) {
		seed = client
	}
	acked(t, m.update(t, "m1", yInsert(700, "member\n")))
	before := stateVector(t, a.guestDocument(t)["sv"].(string))
	if status, res := a.postEdit(t, http.Header{"X-Flats-Access": {"private"}, "X-Flats-Host-Op": {"edit"}},
		`{"ops":[{"op":"insert","at":"end","text":"agent\n"}]}`); status != 200 {
		t.Fatal(status, res)
	}
	agent, agentClock := uint64(0), uint64(0)
	for client, clock := range stateVector(t, a.guestDocument(t)["sv"].(string)) {
		if before[client] != clock {
			agent, agentClock = client, clock
		}
	}
	for name, u := range map[string]string{
		"member": yAfter(700, 7, "x"),
		"seed":   yAfter(seed, stateVector(t, mw["sv"].(string))[seed], "x"),
		"agent":  yAfter(agent, agentClock, "x"),
	} {
		g, _ := a.guest(t, "index.md", "Mina", "", 1)
		expectCode(t, g.update(t, "extend-"+name, u), "guest_client")
	}
	g, w := a.guest(t, "index.md", "Mina", "", 1)
	acked(t, g.update(t, "own", yInsert(800, "guest line\n")))
	other, _ := a.guest(t, "index.md", "Bo", "", 1)
	expectCode(t, other.update(t, "steal", yAfter(800, 11, "x")), "guest_client")
	// Reloading keeps authorship: the same token continues the same text.
	g.Close()
	g2, _ := a.guest(t, "index.md", "Mina", w["guest_token"].(string), 1)
	acked(t, g2.update(t, "own2", yAfter(800, 11, "more\n")))
	// A member edit around guest text leaves it marked; a member reusing a
	// guest's id is refused.
	acked(t, m.update(t, "m2", yInsert(701, "top\n")))
	expectCode(t, m.update(t, "reuse", yAfter(800, 16, "x")), "invalid_update")
	doc := a.guestDocument(t)
	first := 0
	for i, line := range strings.Split(doc["markdown"].(string), "\n") {
		if line == "guest line" {
			first = i + 1
		}
	}
	if doc["guest_text"] != true || fmt.Sprint(doc["guest_lines"]) != fmt.Sprintf("[[%d %d]]", first, first+1) {
		t.Fatal(doc["markdown"], doc["guest_text"], doc["guest_lines"])
	}
}

// A member's page announces its presence id before its first edit; a guest
// that saw it cannot insert with it, and the member's first edit commits.
func TestGuestCannotClaimAnnouncedMemberClient(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	a.setAccess(t, "edit", 1)
	g, _ := a.guest(t, "index.md", "Mina", "", 1)
	m, _ := a.member(t, "index.md", "", nil)
	m.send(t, map[string]any{"t": "aw", "u": presence(1901, 1, map[string]any{})})
	m.next(t, "aw")
	g.next(t, "aw")
	expectCode(t, g.update(t, "claim", yInsert(1901, "mine\n")), "guest_client")
	acked(t, m.update(t, "first", yInsert(1901, "member first\n")))
	// After a reconnect the member's id is still kept from guests.
	m.Close()
	g2, _ := a.guest(t, "index.md", "Bo", "", 1)
	expectCode(t, g2.update(t, "claim2", yAfter(1901, 13, "x")), "guest_client")
}

// guestDocument reads the document as host reads do, with its guest marks
// and the server's state vector of a member welcome.
func (a *app) guestDocument(t *testing.T) map[string]any {
	t.Helper()
	d := a.document(t)
	m, w := a.member(t, "index.md", "", nil)
	m.Close()
	d["sv"] = w["sv"]
	return d
}

func TestGuestUpdateLimits(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	a.setAccess(t, "edit", 1)
	g, _ := a.guest(t, "index.md", "Mina", "", 1)
	expectCode(t, g.update(t, "big", yInsert(900, strings.Repeat("x", 65*1024))), "guest_size")
	g, _ = a.guest(t, "index.md", "Mina", "", 1)
	expectCode(t, g.update(t, "gc", yGC(901, 0, 5)), "guest_room")
	// More than 16 updates at once from one guest are slowed, not refused
	// (spec rule 22): the ones over its rate are answered busy, the socket
	// stays open, and a retry later commits.
	g, _ = a.guest(t, "index.md", "Mina", "", 1)
	for i := 0; i < 20; i++ {
		g.send(t, map[string]any{"t": "update", "id": fmt.Sprint("fast-", i), "u": yInsert(uint64(910+i), "f")})
	}
	acks, busy := 0, 0
	for acks+busy < 20 {
		m, err := g.read()
		if err != nil {
			t.Fatal("socket closed", err)
		}
		switch m["t"] {
		case "error":
			t.Fatal("refused", m)
		case "ack":
			acks++
		case "busy":
			busy++
		}
	}
	if busy == 0 || acks == 0 {
		t.Fatal(acks, busy)
	}
	time.Sleep(2 * time.Second)
	g.send(t, map[string]any{"t": "update", "id": "fast-retry", "retry": true, "u": yInsert(940, "r")})
	for {
		m, err := g.read()
		if err != nil {
			t.Fatal("socket closed", err)
		}
		if m["t"] == "ack" && m["id"] == "fast-retry" {
			break
		}
		if m["t"] == "error" || m["t"] == "busy" {
			t.Fatal("retry not applied", m)
		}
	}
	// A member is not limited by guests' limits.
	m, _ := a.member(t, "index.md", "", nil)
	acked(t, m.update(t, "member-big", yInsert(950, strings.Repeat("m", 100*1024))))
}

// When all guests together reach the flat's limit, updates are answered
// busy and resent later in order; nothing is lost or applied twice.
// Serial, in the docs-budget shard: the guest buckets refill over time, so
// the busy answers and the receipts that keep the writers under the output
// limit depend on the worker keeping up with the wall clock.
func TestGuestBusyResendKeepsOrder(t *testing.T) {
	a := startApp(t, t.TempDir(), initialText, "v1")
	a.setAccess(t, "edit", 1)
	const guests, each = 10, 10
	writers := make([]*socket, guests)
	for i := range writers {
		writers[i], _ = a.guest(t, "index.md", fmt.Sprint("Guest ", i), "", 1)
	}
	// Bytes read from guest sockets count against all guests' budget: other
	// guests' large frames keep it empty until every writer was answered.
	var flooders []*socket
	for i := 0; i < 12; i++ {
		f, _ := a.guest(t, "nested/other.md", "Flood", "", 1)
		flooders = append(flooders, f)
	}
	stopFlood := make(chan struct{})
	var flooding, firstRound sync.WaitGroup
	padding := strings.Repeat("x", 450*1024)
	for _, f := range flooders {
		flooding.Add(1)
		go func() {
			defer flooding.Done()
			go func() {
				for {
					if _, err := f.read(); err != nil {
						return
					}
				}
			}()
			for {
				b, _ := json.Marshal(map[string]any{"t": "ping", "padding": padding})
				f.write(b)
				select {
				case <-stopFlood:
					return
				case <-time.After(2 * time.Second):
				}
			}
		}()
	}
	time.Sleep(500 * time.Millisecond)
	firstRound.Add(guests)
	go func() {
		firstRound.Wait()
		close(stopFlood)
	}()
	var wg sync.WaitGroup
	var mu sync.Mutex
	busy := 0
	for i := 0; i < guests; i++ {
		g := writers[i]
		client := uint64(1000 + i)
		var updates [][2]string
		clock := uint64(0)
		for part := 0; part < each; part++ {
			// Every update reaches the other nine writers, and guests share
			// 4 MiB of unconfirmed output (LIMITS.publicSendBytes). Parts of
			// 4,000 characters added up to about 4.9 MB, so a worker slowed
			// by the flood closed every writer as a slow consumer before
			// their receipts arrived. These parts stay near 1.3 MB.
			text := fmt.Sprintf("[%02d-%02d]%s", i, part, strings.Repeat("p", 1000))
			u := yInsert(client, text)
			if part > 0 {
				u = yAfter(client, clock, text)
			}
			clock += uint64(len(text))
			updates = append(updates, [2]string{fmt.Sprint("p", i, "-", part), u})
		}
		// Like a page, a reader takes every frame and confirms it; the
		// writer sees the answers to its updates.
		answers := make(chan map[string]any, 64)
		var readErr error
		go func() {
			defer close(answers)
			for {
				m, err := g.read()
				if err != nil {
					readErr = err
					return
				}
				if n, ok := m["n"].(float64); ok {
					b, _ := json.Marshal(map[string]any{"t": "received", "n": n})
					g.write(b)
				}
				if m["t"] == "ack" || m["t"] == "busy" || m["t"] == "error" {
					answers <- m
				}
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A writer that stops early still ends its first round, so the
			// flood stops and the test reports the failure instead of
			// running until the package timeout.
			firstDone := sync.OnceFunc(firstRound.Done)
			defer firstDone()
			pending, retry := updates, false
			for round := 0; len(pending) > 0; round++ {
				if round > 20 {
					t.Error("never acknowledged")
					return
				}
				// Paced like a page: at most 4 updates a second.
				for j, u := range pending {
					m := map[string]any{"t": "update", "id": u[0], "u": u[1]}
					if retry && j == 0 {
						m["retry"] = true
					}
					b, _ := json.Marshal(m)
					if err := g.write(b); err != nil {
						t.Error(err)
						return
					}
					time.Sleep(250 * time.Millisecond)
				}
				var left [][2]string
				for range pending {
					m, ok := <-answers
					if !ok {
						// close(answers) happens after readErr is set.
						t.Errorf("guest %d: connection closed in round %d: %v", client, round, readErr)
						return
					}
					switch m["t"] {
					case "error":
						t.Errorf("guest %d refused: %v", client, m)
						return
					case "busy":
						mu.Lock()
						busy++
						mu.Unlock()
						for _, u := range pending {
							if u[0] == m["id"] {
								left = append(left, u)
							}
						}
					}
				}
				if round == 0 {
					firstDone()
				}
				// The client waits a second, then resends what was not
				// acknowledged in order, the first marked as a retry.
				pending, retry = left, true
				if len(pending) > 0 {
					time.Sleep(time.Second)
				}
			}
		}()
	}
	wg.Wait()
	flooding.Wait()
	if t.Failed() {
		return
	}
	if busy == 0 {
		t.Fatal("no update was answered busy")
	}
	text := a.markdown(t, "index.md")
	for i := 0; i < guests; i++ {
		at := -1
		for part := 0; part < each; part++ {
			mark := fmt.Sprintf("[%02d-%02d]", i, part)
			next := strings.Index(text, mark)
			if next <= at || strings.Count(text, mark) != 1 {
				t.Fatalf("guest %d part %d at %d after %d", i, part, next, at)
			}
			at = next
		}
	}
	t.Logf("%d busy answers", busy)
}

// Guests may not take a document beyond its reserved share, nor its
// lifetime capacity, while members still can.
func TestGuestReserveKeepsMembersEditing(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("line of seeded text\n", 47*1024)
	a := startApp(t, t.TempDir(), big, "v1")
	a.setAccess(t, "edit", 1)
	g, _ := a.guest(t, "index.md", "Mina", "", 1)
	expectCode(t, g.update(t, "grow", yInsert(1100, strings.Repeat("g", 4096))), "guest_room")
	m, _ := a.member(t, "index.md", "", nil)
	acked(t, m.update(t, "member", yInsert(1101, strings.Repeat("m", 4096))))
	// Lifetime: guests stop at their share of a document's updates.
	if _, err := a.db(t).Exec("UPDATE flats_docs_documents SET seq=80000 WHERE doc='nested/other.md'"); err != nil {
		t.Fatal(err)
	}
	g2, _ := a.guest(t, "nested/other.md", "Mina", "", 1)
	expectCode(t, g2.update(t, "late", yInsert(1102, "g")), "guest_room")
	m2, _ := a.member(t, "nested/other.md", "", nil)
	acked(t, m2.update(t, "late-member", yInsert(1103, "m")))
}

// Guests have their own pool: 30 at once, 20 per document, 30 new a
// minute; a guest beyond it is told the document is full.
func TestGuestPool(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	a.setAccess(t, "edit", 1)
	join := func(doc string) (*socket, bool) {
		t.Helper()
		s := mustConnect(t, a.srv.URL, doc, publicHeaders("edit", 1))
		s.send(t, map[string]any{"t": "hello", "v": 1, "doc": doc, "sv": "AA==", "guest": map[string]any{"name": "Guest"}})
		m, err := s.read()
		for err == nil && m["t"] != "welcome" && m["t"] != "error" {
			m, err = s.read()
		}
		if err != nil {
			t.Fatal(err)
		}
		if m["t"] == "error" {
			if m["code"] != "full" {
				t.Fatal(m)
			}
			return nil, false
		}
		return s, true
	}
	start := time.Now()
	var open []*socket
	for i := 0; i < 20; i++ {
		s, ok := join("index.md")
		if !ok {
			t.Fatal("refused guest", i)
		}
		open = append(open, s)
	}
	if _, ok := join("index.md"); ok {
		t.Fatal("21 guests in one document")
	}
	for i := 0; i < 10; i++ {
		s, ok := join("nested/other.md")
		if !ok {
			t.Fatal("refused guest", 20+i)
		}
		open = append(open, s)
	}
	if _, ok := join("nested/other.md"); ok {
		t.Fatal("31 guests at once")
	}
	// Room in the pool again, but 30 guests just started: at most one more
	// every two seconds.
	for _, s := range open[:10] {
		s.Close()
	}
	time.Sleep(200 * time.Millisecond)
	admitted := 0
	for i := 0; i < 10; i++ {
		if _, ok := join("nested/other.md"); ok {
			admitted++
		}
	}
	if allowed := int(time.Since(start).Seconds()/2) + 1; admitted > allowed {
		t.Fatalf("%d new guests within %v", admitted, time.Since(start))
	}
	// Members and viewers keep their own pools.
	a.member(t, "index.md", "", nil)
	a.viewer(t, "index.md", "", publicHeaders("edit", 1))
}

// Guest rooms are not counted in the members' room memory: guests fill
// four rooms, a member still opens a fifth document, joins a guest room and
// edits; the class stays when the guests leave (TECH 3.3).
// It seeds six documents near 1 MiB each in the WebSocket VM; it runs
// alone, since next to the package's parallel tests one seeding event can
// outlast the VM's event deadline on a loaded machine.
func TestGuestRoomsLeaveMemberRoomMemory(t *testing.T) {
	texts := map[string]string{}
	for i := 0; i < 6; i++ {
		texts[fmt.Sprintf("d%d.md", i)] = fmt.Sprintf("# D%d\n", i) + strings.Repeat("seeded text of a large document\n", 28*1024)
	}
	a := startAppModule(t, t.TempDir(), docsModule("v1", texts), "v1", map[string]string{})
	a.setAccess(t, "edit", 1)
	guests := map[int]*socket{}
	for i := 0; i < 4; i++ {
		g, _ := a.guest(t, fmt.Sprintf("d%d.md", i), "Mina", "", 1)
		acked(t, g.update(t, "g", yInsert(uint64(1200+i), "guest\n")))
		guests[i] = g
	}
	s := mustConnect(t, a.srv.URL, "d4.md", publicHeaders("edit", 1))
	s.send(t, map[string]any{"t": "hello", "v": 1, "doc": "d4.md", "sv": "AA==", "guest": map[string]any{"name": "Fifth"}})
	s.rejected(t, "full")
	// Four 0.9 MiB guest rooms and a fifth member room would pass the
	// members' 4 MiB if guest rooms counted.
	m4, _ := a.member(t, "d4.md", "", nil)
	acked(t, m4.update(t, "m4", yInsert(1300, "member\n")))
	m0, _ := a.member(t, "d0.md", "", nil)
	acked(t, m0.update(t, "m0", yInsert(1301, "member\n")))
	// Guests grow a mixed room.
	acked(t, guests[0].update(t, "g2", yAfter(1200, 6, "more\n")))
	// The last guest leaves one room by disconnecting, and every guest by
	// Stop guest editing; members keep editing.
	guests[0].Close()
	time.Sleep(100 * time.Millisecond)
	acked(t, m0.update(t, "m0b", yInsert(1302, "member\n")))
	a.setAccess(t, "view", 2)
	guests[1].refused(t, "editing_ended")
	m5, _ := a.member(t, "d5.md", "", nil)
	acked(t, m5.update(t, "m5", yInsert(1303, "member\n")))
	acked(t, m0.update(t, "m0c", yInsert(1304, "member\n")))
	acked(t, m4.update(t, "m4b", yInsert(1305, "member\n")))
}

// Guests keep the handshake limits' reserve for members: large client ids
// and growing clocks are refused before the state vector passes 6,144
// bytes, and a member that received everything reconnects and edits.
func TestGuestHandshakeReserve(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	a.setAccess(t, "edit", 1)
	m, _ := a.member(t, "index.md", "", nil)
	var members []yItem
	for i := uint64(0); i < 300; i++ {
		members = append(members, yItem{client: 1<<53 - 1 - i, text: "m"})
	}
	acked(t, m.update(t, "members", yUpdate(members, nil)))
	g, w := a.guest(t, "index.md", "Mina", "", 1)
	token := w["guest_token"].(string)
	next := uint64(1<<53 - 1000)
	var owned []uint64
	refused := false
	for batch := 0; batch < 10 && !refused; batch++ {
		var items []yItem
		for i := 0; i < 40; i++ {
			items = append(items, yItem{client: next, text: "g"})
			next--
		}
		r := g.update(t, fmt.Sprint("ids-", batch), yUpdate(items, nil))
		if r["t"] == "error" {
			expectCode(t, r, "guest_room")
			refused = true
			break
		}
		acked(t, r)
		for _, it := range items {
			owned = append(owned, it.client)
		}
	}
	if !refused || len(owned) < 300 {
		t.Fatal("refused", refused, "after", len(owned), "ids")
	}
	// Owned ids too: growing their clocks past one varint byte is refused.
	g, _ = a.guest(t, "index.md", "Mina", token, 1)
	var grow []yItem
	for _, client := range owned {
		grow = append(grow, yItem{client: client, clock: 1, text: strings.Repeat("c", 130), origin: &[2]uint64{client, 0}})
	}
	expectCode(t, g.update(t, "clocks", yUpdate(grow, nil)), "guest_room")
	_, sw := a.member(t, "index.md", "", nil)
	sv := sw["sv"].(string)
	if raw, _ := base64.StdEncoding.DecodeString(sv); len(raw) > 6144 {
		t.Fatal("state vector", len(raw))
	}
	// The member that received everything reconnects with its full state
	// vector and edits.
	m.Close()
	again := mustConnect(t, a.srv.URL, "index.md", memberHeaders)
	again.send(t, map[string]any{"t": "hello", "v": 1, "doc": "index.md", "sv": sv})
	again.next(t, "welcome")
	acked(t, again.update(t, "after", yInsert(5, "member after\n")))
}

// Stop guest editing: the access operation closes guest sockets and tells
// every page; an update in flight is refused at commit, and a missed signal
// is caught at the next heartbeat (TECH 3.1, 3.2).
func TestStopGuestEditingInTheWorker(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	a.setAccess(t, "edit", 4)
	m, _ := a.member(t, "index.md", "", nil)
	v, _ := a.viewer(t, "index.md", "", publicHeaders("edit", 4))
	g, _ := a.guest(t, "index.md", "Mina", "", 4)
	acked(t, g.update(t, "g", yInsert(1500, "guest\n")))
	before := len(a.outbox(t))
	a.setAccess(t, "view", 5)
	g.refused(t, "editing_ended")
	for _, s := range []*socket{m, v} {
		if l := s.next(t, "level"); l["access"] != "view" || l["access_epoch"] != float64(5) {
			t.Fatal(l)
		}
	}
	e := a.outbox(t)
	if len(e) != before+1 || e[len(e)-1].Kind != "person" {
		t.Fatal("the guests' group was not recorded", e)
	}
	// A lost signal: the level changes in the database only.
	a.setAccess(t, "edit", 6)
	g1, _ := a.guest(t, "index.md", "Mina", "", 6)
	g2, _ := a.guest(t, "index.md", "Bo", "", 6)
	if _, err := a.db(t).Exec(`UPDATE flats_docs_meta SET value='{"level":"view","epoch":7}' WHERE key='access'`); err != nil {
		t.Fatal(err)
	}
	expectCode(t, g1.update(t, "late", yInsert(1501, "late\n")), "editing_ended")
	g2.send(t, map[string]any{"t": "ping"})
	g2.refused(t, "editing_ended")
	m.send(t, map[string]any{"t": "ping"})
	for {
		l := m.next(t, "level")
		if l["access_epoch"] == float64(7) {
			if l["access"] != "view" {
				t.Fatal(l)
			}
			break
		}
	}
	if strings.Contains(a.markdown(t, "index.md"), "late") {
		t.Fatal("a guest update committed after editing ended")
	}
}

func TestPublicAccessHostOperations(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	for _, body := range []map[string]any{{"level": "admin", "epoch": 1}, {"level": "edit", "epoch": -1}, {"level": "edit"}} {
		if status, out := a.hostOp(t, "POST", "access", body); status != 400 {
			t.Fatal(body, status, out)
		}
	}
	a.setAccess(t, "edit", 3)
	if status, out := a.hostOp(t, "POST", "access", map[string]any{"level": "view", "epoch": 2}); status != 409 {
		t.Fatal("an older epoch", status, out)
	}
	status, out := a.hostOp(t, "POST", "safe_point", map[string]any{"prev": nil})
	if status != 200 || out["reuse"] != false || out["seq"].(float64) < 1 {
		t.Fatal(status, out)
	}
	e := a.outbox(t)
	last := e[len(e)-1]
	if last.Kind != "safe_point" || last.Seq != int64(out["seq"].(float64)) {
		t.Fatal(last)
	}
	var docs []map[string]any
	for _, d := range last.Documents {
		docs = append(docs, map[string]any{"path": d.Path, "hash": d.Hash})
	}
	status, out = a.hostOp(t, "POST", "safe_point", map[string]any{"prev": map[string]any{"source": last.Source, "documents": docs}})
	if status != 200 || out["reuse"] != true || out["seq"] != float64(0) {
		t.Fatal(status, out)
	}
	// Host only, and presence comes with sync.
	for _, op := range []string{"access", "safe_point"} {
		req, _ := http.NewRequest("POST", a.srv.URL+"/_docs/host/revisions/"+op, strings.NewReader(`{"level":"edit","epoch":9}`))
		req.Header.Set("X-Flats-Access", "public")
		req.Header.Set("X-Flats-Host-Op", "revisions")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 403 {
			t.Fatal(op, res.StatusCode)
		}
	}
	if status, out := a.hostOp(t, "POST", "sync", nil); status != 200 || out["presence"] == nil {
		t.Fatal(status, out)
	}
}

// The worker writes how many guests and viewers are connected, and zeros
// after the last one leaves (TECH 3.9).
func TestPublicPresenceCounts(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	a.setAccess(t, "edit", 1)
	counts := func() (float64, float64, float64) {
		_, out := a.hostOp(t, "POST", "sync", nil)
		p := out["presence"].(map[string]any)
		return p["guests"].(float64), p["viewers"].(float64), p["at"].(float64)
	}
	g, _ := a.guest(t, "index.md", "Mina", "", 1)
	v, _ := a.viewer(t, "index.md", "", publicHeaders("edit", 1))
	time.Sleep(1100 * time.Millisecond)
	v.send(t, map[string]any{"t": "ping"})
	v.next(t, "pong")
	if guests, viewers, at := counts(); guests != 1 || viewers != 1 || time.Since(time.UnixMilli(int64(at))) > 5*time.Second {
		t.Fatal(guests, viewers, at)
	}
	g.Close()
	v.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if guests, viewers, _ := counts(); guests == 0 && viewers == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("counts stayed after the last public socket closed")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Public requests never reach Private-only reads or writes, at either level.
func TestPublicBoundary(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	for _, level := range []string{"view", "edit"} {
		a.setAccess(t, level, map[string]int64{"view": 1, "edit": 2}[level])
		h := http.Header{"X-Flats-Access": {"public"}, "X-Flats-Public-Access": {level}}
		status, body, _ := a.getHeaders(t, "/_docs/api/document?blocks=1", h)
		if status != 200 || strings.Contains(body, "blocks") || strings.Contains(body, "guest_lines") {
			t.Fatal(level, status, body)
		}
		if status, _ := a.postEdit(t, h, ops(map[string]any{"op": "insert", "at": "end", "text": "x"})); status != 403 {
			t.Fatal(level, "edit", status)
		}
		for _, path := range []string{"/_docs/host/revisions/outbox", "/_docs/api/receipt?id=x"} {
			if status, _, _ := a.getHeaders(t, path, h); status != 403 {
				t.Fatal(level, path, status)
			}
		}
	}
}

// A refused guest update stops that socket: the update and every later one
// stay uncommitted; rejoining with the token loads the server's text and
// edits again.
func TestGuestRefusalAndRejoin(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	a.setAccess(t, "edit", 1)
	g, w := a.guest(t, "index.md", "Mina", "", 1)
	refusedText := strings.Repeat("A", 65*1024)
	g.send(t, map[string]any{"t": "update", "id": "A", "u": yInsert(1600, refusedText)})
	g.send(t, map[string]any{"t": "update", "id": "B", "u": yAfter(1600, uint64(len(refusedText)), "B-depends-on-A")})
	g.rejected(t, "guest_size")
	if text := a.markdown(t, "index.md"); strings.Contains(text, "AAAA") || strings.Contains(text, "B-depends") {
		t.Fatal("a refused or later update committed")
	}
	again, w2 := a.guest(t, "index.md", "Mina", w["guest_token"].(string), 1)
	if w2["me"].(map[string]any)["name"] != "Mina" {
		t.Fatal(w2)
	}
	acked(t, again.update(t, "C", yInsert(1601, "rejoined\n")))
}

// A guest's presence beyond 10 a second is dropped, not refused; its
// awareness id follows the client id rule (TECH 3.5, 3.8).
func TestGuestPresenceLimits(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	a.setAccess(t, "edit", 1)
	m, _ := a.member(t, "index.md", "", nil)
	acked(t, m.update(t, "m", yInsert(1700, "member\n")))
	g, _ := a.guest(t, "index.md", "Mina", "", 1)
	for clock := uint64(1); clock <= 40; clock++ {
		g.send(t, map[string]any{"t": "aw", "u": presence(1701, clock, map[string]any{})})
	}
	g.send(t, map[string]any{"t": "ping"})
	g.next(t, "pong")
	relayed := 0
	m.send(t, map[string]any{"t": "ping"})
	m.next(t, "pong")
	for _, f := range m.inbox {
		if f["t"] == "aw" {
			relayed++
		}
	}
	if relayed == 0 || relayed > 15 {
		t.Fatal("relayed", relayed, "of 40")
	}
	// A member's client id is not a guest's awareness id.
	spoof, _ := a.guest(t, "index.md", "Mina", "", 1)
	spoof.send(t, map[string]any{"t": "aw", "u": presence(1700, 1, map[string]any{})})
	spoof.refused(t, "guest_client")
}

// A heading the live document no longer has never names it on a Public page
// or in the Public document list (spec rule 16a).
func TestPublicTitlesUseLiveText(t *testing.T) {
	t.Parallel()
	text := "# Secret heading\n\nBody\n"
	sum := sha256.Sum256([]byte(text))
	b, _ := json.Marshal(map[string]any{"format": 1, "hash": "v1", "entry": "index.md", "documents": []any{
		map[string]any{"path": "index.md", "hash": hex.EncodeToString(sum[:]), "text": text},
		map[string]any{"path": "notes.md", "hash": "notes", "text": "# Old notes heading\n"},
	}, "assets": []string{}})
	a := startAppModule(t, t.TempDir(), "export default "+string(b)+";\n", "v1", map[string]string{})
	if status, res := a.postEdit(t, http.Header{"X-Flats-Access": {"private"}, "X-Flats-Host-Op": {"edit"}},
		`{"ops":[{"op":"replace","find":"# Secret heading","with":"# Visible"}]}`); status != 200 {
		t.Fatal(status, res)
	}
	for _, path := range []string{"/", "/index.md", "/_docs/api/documents", "/notes.md"} {
		status, body, _ := a.get(t, path)
		if status != 200 || strings.Contains(body, "Secret heading") || (path == "/_docs/api/documents" && strings.Contains(body, "Old notes heading")) {
			t.Fatalf("%s: %d %s", path, status, body)
		}
	}
	if _, body, _ := a.get(t, "/"); !strings.Contains(body, "<h1>Visible</h1>") {
		t.Fatal(body)
	}
}
