package board

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/wsconn"
)

// clock is an injectable clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeRev is a revision the fake core imported.
type fakeRev struct {
	info  RevisionInfo
	files []File
}

// fakeCore is a small core: blobs in memory, revisions from the outbox
// with the import cursor, the flat lock as a mutex.
type fakeCore struct {
	mu       sync.Mutex // the flat lock
	dir      string     // the flat directory
	data     sync.Mutex
	blobs    map[string][]byte
	limits   Limits
	revs     []fakeRev
	cursor   int64
	store    string
	in       *Instance
	measured int
	failMeas bool
	ready    chan struct{}
	putHook  func(hash string) // runs before PutBlob stores, outside the locks
	leases   map[string]int    // read leases per hash (5.5)
	extra    int64             // bytes MeasureOther adds, as other files would
}

func newFakeCore(dir string) *fakeCore {
	return &fakeCore{dir: dir, blobs: map[string][]byte{}, limits: Limits{UploadMax: 20 << 20}, ready: make(chan struct{}, 1)}
}

func (f *fakeCore) Limits() Limits {
	f.data.Lock()
	defer f.data.Unlock()
	return f.limits
}

func (f *fakeCore) setQuota(q int64) {
	f.data.Lock()
	f.limits.DiskQuota = q
	f.data.Unlock()
}

func (f *fakeCore) MeasureOther(ctx context.Context) (int64, error) {
	f.data.Lock()
	defer f.data.Unlock()
	f.measured++
	if f.failMeas {
		return 0, fmt.Errorf("measurement failed")
	}
	n := f.extra
	for _, b := range f.blobs {
		n += int64(len(b))
	}
	err := filepath.WalkDir(f.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasPrefix(filepath.Base(p), dbName) {
			return nil
		}
		info, err := d.Info()
		if err == nil {
			n += info.Size()
		}
		return err
	})
	return n, err
}

func (f *fakeCore) PutBlob(ctx context.Context, hash string, data []byte) (bool, error) {
	f.data.Lock()
	hook := f.putHook
	f.data.Unlock()
	if hook != nil {
		hook(hash)
	}
	f.data.Lock()
	defer f.data.Unlock()
	if _, ok := f.blobs[hash]; ok {
		return false, nil
	}
	f.blobs[hash] = slices.Clone(data)
	return true, nil
}

func (f *fakeCore) ReadBlob(ctx context.Context, hash string) ([]byte, error) {
	f.data.Lock()
	defer f.data.Unlock()
	b, ok := f.blobs[hash]
	if !ok {
		return nil, errorf(CategoryNotFound, "no blob %s", hash)
	}
	return b, nil
}

func (f *fakeCore) RevisionAsset(ctx context.Context, hash string) (string, bool, error) {
	f.data.Lock()
	defer f.data.Unlock()
	for _, r := range f.revs {
		for _, file := range r.files {
			if strings.HasPrefix(file.Path, "assets/") && file.Hash == hash {
				return file.Path, true, nil
			}
		}
	}
	return "", false, nil
}

func (f *fakeCore) Revisions(ctx context.Context, before, limit int) (RevisionPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.importLocked(ctx); err != nil {
		return RevisionPage{}, err
	}
	f.data.Lock()
	defer f.data.Unlock()
	var p RevisionPage
	for i := len(f.revs) - 1; i >= 0; i-- {
		if before > 0 && f.revs[i].info.Number >= before {
			continue
		}
		p.Revisions = append(p.Revisions, f.revs[i].info)
	}
	p.Count = len(f.revs)
	return p, nil
}

func (f *fakeCore) rev(n int) (fakeRev, bool) {
	f.data.Lock()
	defer f.data.Unlock()
	for _, r := range f.revs {
		if r.info.Number == n {
			return r, true
		}
	}
	return fakeRev{}, false
}

func (f *fakeCore) RevisionContent(ctx context.Context, n int) (RevisionInfo, engine.Content, error) {
	r, ok := f.rev(n)
	if !ok {
		return RevisionInfo{}, engine.Content{}, errorf(CategoryNotFound, "no revision %d", n)
	}
	c, err := ContentFromFiles(r.files, func(h string) ([]byte, error) { return f.ReadBlob(ctx, h) })
	return r.info, c, err
}

func (f *fakeCore) HeldRevisionContent(ctx context.Context, n int) (engine.Content, func(), error) {
	r, ok := f.rev(n)
	if !ok {
		return engine.Content{}, func() {}, errorf(CategoryNotFound, "no revision %d", n)
	}
	var hashes []string
	for _, file := range r.files {
		hashes = append(hashes, file.Hash)
	}
	release := f.Lease(hashes)
	c, err := ContentFromFiles(r.files, func(h string) ([]byte, error) { return f.ReadBlob(ctx, h) })
	if err != nil {
		release()
		return c, func() {}, err
	}
	return c, release, nil
}

func (f *fakeCore) Lease(hashes []string) func() {
	f.data.Lock()
	if f.leases == nil {
		f.leases = map[string]int{}
	}
	for _, h := range hashes {
		f.leases[h]++
	}
	f.data.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.data.Lock()
			defer f.data.Unlock()
			for _, h := range hashes {
				if f.leases[h]--; f.leases[h] <= 0 {
					delete(f.leases, h)
				}
			}
		})
	}
}

// sweep removes the blobs of hashes that hold no read lease, as a blob
// sweep does once retention dropped every other reference to them.
func (f *fakeCore) sweep(hashes ...string) {
	f.data.Lock()
	defer f.data.Unlock()
	for _, h := range hashes {
		if f.leases[h] == 0 {
			delete(f.blobs, h)
		}
	}
}

func (f *fakeCore) leased(h string) bool {
	f.data.Lock()
	defer f.data.Unlock()
	return f.leases[h] > 0
}

func (f *fakeCore) Pin(ctx context.Context, n int, pinned bool) error {
	f.data.Lock()
	defer f.data.Unlock()
	for i := range f.revs {
		if f.revs[i].info.Number == n {
			f.revs[i].info.Pinned = pinned
			return nil
		}
	}
	return errorf(CategoryNotFound, "no revision %d", n)
}

func (f *fakeCore) Rollback(ctx context.Context, n int, label string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, c, err := f.RevisionContent(ctx, n)
	if err != nil {
		return 0, err
	}
	if _, err := f.in.Rollback(ctx, "rollback-"+randomToken(), n, c, label); err != nil {
		return 0, err
	}
	if err := f.importLocked(ctx); err != nil {
		return 0, err
	}
	return f.lastRev(), nil
}

func (f *fakeCore) lastRev() int {
	f.data.Lock()
	defer f.data.Unlock()
	if len(f.revs) == 0 {
		return 0
	}
	return f.revs[len(f.revs)-1].info.Number
}

func (f *fakeCore) Exclusive(ctx context.Context, fn func(ctx context.Context, other int64) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	other, err := f.MeasureOther(ctx)
	if err != nil {
		return err
	}
	if err := fn(ctx, other); err != nil {
		return err
	}
	return f.importLocked(ctx)
}

func (f *fakeCore) OutboxReady() {
	select {
	case f.ready <- struct{}{}:
	default:
	}
}

// importOutbox imports the outbox under the flat lock (5.3 Import).
func (f *fakeCore) importOutbox(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.importLocked(ctx)
}

func (f *fakeCore) importLocked(ctx context.Context) error {
	in := f.in
	if f.store != in.StoreID() {
		f.store, f.cursor = in.StoreID(), 0
	}
	// Entries the cursor passed were imported before a stop between the
	// flats.db commit and the acknowledgement: acknowledge them again.
	if old, err := in.Outbox(ctx, 0, 1); err != nil {
		return err
	} else if len(old) > 0 && old[0].Seq <= f.cursor {
		if err := in.Acknowledge(ctx, f.cursor, 0); err != nil {
			return err
		}
	}
	entries, err := in.Outbox(ctx, f.cursor, 100)
	if err != nil || len(entries) == 0 {
		return err
	}
	var stored int64
	for _, e := range entries {
		for _, h := range e.Snapshots {
			data, err := in.SnapshotData(ctx, h)
			if err != nil {
				return err
			}
			if isNew, _ := f.PutBlob(ctx, h, data); isNew {
				stored += int64(len(data))
			}
		}
		for _, file := range e.Files {
			if _, err := f.ReadBlob(ctx, file.Hash); err != nil {
				return fmt.Errorf("entry %d names a missing blob %s (%s)", e.Seq, file.Hash, file.Path)
			}
		}
		f.data.Lock()
		f.revs = append(f.revs, fakeRev{info: RevisionInfo{Number: len(f.revs) + 1, Kind: e.Kind, Authors: e.Authors,
			Message: e.Message, At: e.CreatedAt, RollbackOf: e.RollbackOf, GuestAuthors: e.Guests, GuestsMore: e.GuestsMore}, files: e.Files})
		f.cursor = e.Seq
		f.data.Unlock()
	}
	return in.Acknowledge(ctx, entries[len(entries)-1].Seq, stored)
}

// tb is a board under test with a private and a public route.
type tb struct {
	t     *testing.T
	dir   string
	core  *fakeCore
	clock *clock
	in    *Instance
	priv  *httptest.Server
	pub   *httptest.Server
	// level is the flat's stored public access level: core sends it on
	// Public requests (PublicAccessHeader) and opens the instance with it.
	level struct {
		mu sync.Mutex
		v  string
	}
	cancel struct {
		mu  sync.Mutex
		fns []context.CancelFunc
	}
}

func newTB(t *testing.T) *tb {
	t.Helper()
	dir := t.TempDir()
	b := &tb{t: t, dir: dir, core: newFakeCore(dir), clock: &clock{t: time.Unix(1_760_000_000, 0)}}
	b.open()
	route := func(access string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Del("X-Flats-Access")
			r.Header.Del(PublicAccessHeader)
			if access == "public" {
				r.Header.Del("Tailscale-User-Login")
				r.Header.Del("Tailscale-User-Name")
				if l := b.publicLevel(); l != "" {
					r.Header.Set(PublicAccessHeader, l)
				}
			} else {
				r.Header.Set("X-Flats-Access", access)
			}
			ctx, cancel := context.WithCancel(r.Context())
			b.cancel.mu.Lock()
			b.cancel.fns = append(b.cancel.fns, cancel)
			b.cancel.mu.Unlock()
			b.in.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	b.priv = httptest.NewServer(route("private"))
	b.pub = httptest.NewServer(route("public"))
	t.Cleanup(func() {
		b.in.Stop()
		b.priv.Close()
		b.pub.Close()
	})
	return b
}

func (b *tb) publicLevel() string {
	b.level.mu.Lock()
	defer b.level.mu.Unlock()
	return b.level.v
}

// setLevel changes the stored public access level and, when the instance
// runs, pushes it as core does (TECH public edit 2.6).
func (b *tb) setLevel(level string) {
	b.t.Helper()
	b.level.mu.Lock()
	b.level.v = level
	b.level.mu.Unlock()
	if err := b.in.SetPublicAccess(context.Background(), level); err != nil {
		b.t.Fatal(err)
	}
}

func (b *tb) open() {
	b.t.Helper()
	in, _, err := Open(filepath.Join(b.dir, "board"), b.core, Options{Slug: "test", Title: "Test", Now: b.clock.Now, tickEvery: time.Hour,
		PublicAccess: b.publicLevel()})
	if err != nil {
		b.t.Fatal(err)
	}
	b.in, b.core.in = in, in
}

// restart stops the instance and opens it again from board.db.
func (b *tb) restart() {
	b.in.Stop()
	b.open()
}

// withdraw cancels every request context, as core's route epoch does.
func (b *tb) withdraw() {
	b.cancel.mu.Lock()
	defer b.cancel.mu.Unlock()
	for _, f := range b.cancel.fns {
		f()
	}
	b.cancel.fns = nil
}

// client is a WebSocket test client.
type client struct {
	gate    sync.Mutex // held while the client stops reading
	t       *testing.T
	ws      *wsconn.Conn
	msgs    chan map[string]any
	backlog []map[string]any
	session string
	seq     int64
}

// headerConn adds headers to the handshake request written through it.
type headerConn struct {
	net.Conn
	extra string
	done  bool
}

func (h *headerConn) Write(p []byte) (int, error) {
	if !h.done && bytes.HasSuffix(p, []byte("\r\n\r\n")) {
		h.done = true
		q := append(slices.Clone(p[:len(p)-2]), []byte(h.extra+"\r\n")...)
		if _, err := h.Conn.Write(q); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	return h.Conn.Write(p)
}

func dial(t *testing.T, srv *httptest.Server, headers map[string]string) (*client, error) {
	t.Helper()
	host := strings.TrimPrefix(srv.URL, "http://")
	extra := ""
	for k, v := range headers {
		extra += k + ": " + v + "\r\n"
	}
	ws, err := wsconn.Dial(context.Background(), func(ctx context.Context) (net.Conn, error) {
		c, err := net.Dial("tcp", host)
		if err != nil {
			return nil, err
		}
		return &headerConn{Conn: c, extra: extra}, nil
	}, host, "/_board/ws")
	if err != nil {
		return nil, err
	}
	c := &client{t: t, ws: ws, msgs: make(chan map[string]any, 1024)}
	go func() {
		defer close(c.msgs)
		for {
			c.gate.Lock()
			c.gate.Unlock()
			raw, err := ws.ReadMessage(64 << 20)
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal([]byte(raw), &m) == nil {
				m["_raw"] = raw
				c.msgs <- m
			}
		}
	}()
	t.Cleanup(func() { ws.Close(1000, "") })
	return c, nil
}

func origin(srv *httptest.Server) map[string]string { return map[string]string{"Origin": srv.URL} }

// connect dials and says hello, returning after the welcome.
func connect(t *testing.T, srv *httptest.Server, hello map[string]any, headers map[string]string) *client {
	t.Helper()
	h := origin(srv)
	for k, v := range headers {
		h[k] = v
	}
	c, err := dial(t, srv, h)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}, "editor": "ed1"}
	for k, v := range hello {
		m[k] = v
	}
	c.send(m)
	w := c.expect("welcome")
	if s, _ := w["session"].(string); s != "" {
		c.session = s
	}
	return c
}

func (c *client) send(v any) {
	c.t.Helper()
	b, _ := json.Marshal(v)
	if err := c.ws.WriteFrame(wsconn.OpText, b); err != nil {
		c.t.Fatal(err)
	}
}

// next returns the next message matching pred within 5 seconds, keeping
// other messages for later.
func (c *client) next(pred func(map[string]any) bool) (map[string]any, bool) {
	for i, m := range c.backlog {
		if pred(m) {
			c.backlog = slices.Delete(c.backlog, i, i+1)
			return m, true
		}
	}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-c.msgs:
			if !ok {
				return nil, false
			}
			if pred(m) {
				return m, true
			}
			c.backlog = append(c.backlog, m)
		case <-timeout:
			return nil, false
		}
	}
}

func (c *client) expect(t string) map[string]any {
	c.t.Helper()
	m, ok := c.next(func(m map[string]any) bool { return m["t"] == t })
	if !ok {
		c.t.Fatalf("no %s message; backlog %v", t, c.backlog)
	}
	return m
}

// quiet reports whether no message matching pred arrives within d.
func (c *client) quiet(d time.Duration, pred func(map[string]any) bool) bool {
	for _, m := range c.backlog {
		if pred(m) {
			return false
		}
	}
	timeout := time.After(d)
	for {
		select {
		case m, ok := <-c.msgs:
			if !ok {
				return true
			}
			if pred(m) {
				return false
			}
			c.backlog = append(c.backlog, m)
		case <-timeout:
			return true
		}
	}
}

// closed waits until the socket is closed by the server.
func (c *client) closed(d time.Duration) bool {
	timeout := time.After(d)
	for {
		select {
		case _, ok := <-c.msgs:
			if !ok {
				return true
			}
		case <-timeout:
			return false
		}
	}
}

// snapshot collects every part of the next snapshot.
func (c *client) snapshot() (seq int64, elements []map[string]any, meta map[string]any) {
	c.t.Helper()
	for {
		m := c.expect("snapshot")
		seq = int64(m["seq"].(float64))
		if mm, ok := m["meta"].(map[string]any); ok {
			meta = mm
		}
		for _, e := range m["elements"].([]any) {
			elements = append(elements, e.(map[string]any))
		}
		if m["part"] == m["parts"] {
			return seq, elements, meta
		}
	}
}

// batch sends ops as batch n and returns the ack or reject.
func (c *client) batch(n int, ops ...map[string]any) map[string]any {
	c.t.Helper()
	c.send(map[string]any{"t": "ops", "n": n, "ops": ops})
	m, ok := c.next(func(m map[string]any) bool {
		return (m["t"] == "ack" || m["t"] == "reject") && int(m["n"].(float64)) == n
	})
	if !ok {
		c.t.Fatalf("no answer to batch %d; backlog %v", n, c.backlog)
	}
	return m
}

// settle sends batch n until it is not refused as transient, as the
// client does with backoff.
func (c *client) settle(n int, ops ...map[string]any) map[string]any {
	c.t.Helper()
	for i := 0; ; i++ {
		m := c.batch(n, ops...)
		if m["retry"] != true || i == 50 {
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// noise is text that does not compress well.
func noise(n int, seed int) string {
	var b strings.Builder
	x := uint32(seed*2654435761 + 1)
	for b.Len() < n {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b.WriteByte(byte('a' + x%26))
	}
	return b.String()
}

var idSeq struct {
	mu sync.Mutex
	n  int
}

// eid returns a fresh valid element id.
func eid() string {
	idSeq.mu.Lock()
	defer idSeq.mu.Unlock()
	idSeq.n++
	return fmt.Sprintf("e_t%09d", idSeq.n)
}

func sticky(id, text string, x float64) map[string]any {
	return map[string]any{"op": "add", "el": map[string]any{"id": id, "type": "sticky", "x": x, "y": 0, "w": 200, "h": 200, "rot": 0, "text": text}}
}

// state reads the engine state in the apply loop.
func (b *tb) state(fn func(st *engine.State)) {
	b.t.Helper()
	if err := b.in.do(context.Background(), func() error { fn(b.in.st); return nil }); err != nil {
		b.t.Fatal(err)
	}
}

// apply runs a retention-style change in the apply loop and commits it.
func (b *tb) apply(fn func(st *engine.State) engine.Changed) {
	b.t.Helper()
	err := b.in.do(context.Background(), func() error {
		t, err := b.in.begin()
		if err != nil {
			return err
		}
		if err := t.persist(fn(b.in.st), b.in.st.Seq()); err != nil {
			t.rollback()
			return err
		}
		return t.commit()
	})
	if err != nil {
		b.t.Fatal(err)
	}
}

func (b *tb) alive() map[string]string {
	out := map[string]string{}
	b.state(func(st *engine.State) {
		for _, e := range st.Alive() {
			out[e.ID] = e.Text
		}
	})
	return out
}

// post sends a same-origin board POST.
func (b *tb) post(srv *httptest.Server, path, contentType string, body io.Reader) (*http.Response, map[string]any) {
	b.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, body)
	req.Header.Set("X-Flats-Board", "1")
	req.Header.Set("Origin", srv.URL)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &m)
	return resp, m
}

func readAll(r *bufio.Reader) string {
	b, _ := io.ReadAll(r)
	return string(b)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
