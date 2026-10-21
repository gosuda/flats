package board

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/board/model"
	"github.com/gosuda/flats/internal/wsconn"
)

// noisyPNG is a w×h PNG that does not compress, distinct per seed.
func noisyPNG(t *testing.T, w, h, seed int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	x := uint32(seed*2654435761 + 7)
	for i := range img.Pix {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		img.Pix[i] = byte(x)
	}
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func riffChunk(tag string, body []byte) []byte {
	out := append([]byte(tag), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...)
	out = append(out, body...)
	if len(body)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

func riff(chunks ...[]byte) []byte {
	body := []byte("WEBP")
	for _, c := range chunks {
		body = append(body, c...)
	}
	return append(append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...), body...)
}

func vp8x(flags byte, w, h int) []byte {
	return riffChunk("VP8X", []byte{flags, 0, 0, 0, byte(w - 1), byte((w - 1) >> 8), byte((w - 1) >> 16),
		byte(h - 1), byte((h - 1) >> 8), byte((h - 1) >> 16)})
}

// A WebP whose first VP8X declares 46,340² pixels with alpha and whose
// second declares 1×1 would make the decoder allocate 2 GB for alpha if the
// walk accepted the last header.
func TestWebPConflictingHeadersAreRefusedBeforeDecoding(t *testing.T) {
	bomb := riff(vp8x(0x10, 46340, 46340), riffChunk("ALPH", []byte{0}), vp8x(0, 1, 1))
	if len(bomb) != 58 {
		t.Fatalf("bomb is %d bytes", len(bomb))
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := CheckImage(bomb, 1<<20)
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Fatalf("checking the bomb allocated %d bytes", grew)
	}
	if err == nil || !strings.Contains(err.Error(), "more than one VP8X") {
		t.Fatalf("bomb: %v", err)
	}
	// The image chunk the decoder stops at must match the VP8X canvas.
	vp8l := riffChunk("VP8L", []byte{0x2f, 0xff, 0xff, 0xff, 0x0f})
	if _, err := CheckImage(riff(vp8x(0, 1, 1), vp8l), 1<<20); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("VP8X 1×1 with a large VP8L: %v", err)
	}
	if _, err := CheckImage(stillWebP, 1<<20); err != nil {
		t.Fatalf("still WebP: %v", err)
	}
}

// Replies to a client that sends faster than it reads go through the queue
// cap. Here a large snapshot keeps the socket's in-flight bytes above the
// feed threshold, so the pump waits while rate-limit rejections pile up:
// the socket is closed instead of the queue growing.
func TestRepliesToASlowReaderStayWithinTheQueueCap(t *testing.T) {
	b := newTB(t)
	fillBoard(t, b, 2000, 4000) // about 8 MB of snapshot
	c, err := dial(t, b.priv, origin(b.priv))
	if err != nil {
		t.Fatal(err)
	}
	c.gate.Lock() // the client stops reading after the welcome
	defer c.gate.Unlock()
	c.send(map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}, "editor": "flood"})
	var sc *conn
	waitFor(t, func() bool {
		for _, o := range b.in.hub.list() {
			if o.key == "browser:flood" {
				sc = o
			}
		}
		return sc != nil
	})
	msg, _ := json.Marshal(map[string]any{"t": "ops", "n": 1, "ops": []any{}})
	deadline := time.Now().Add(30 * time.Second)
	done := func() bool {
		select {
		case <-sc.ws.Done():
			return true
		default:
			return false
		}
	}
	maxQueued := int64(0)
	for i := 0; i < 400_000 && time.Now().Before(deadline) && !done(); i++ {
		if c.ws.WriteFrame(wsconn.OpText, msg) != nil {
			break
		}
		if i%64 == 0 {
			sc.mu.Lock()
			maxQueued = max(maxQueued, sc.qBytes)
			sc.mu.Unlock()
		}
	}
	if maxQueued > b.in.privateQueue {
		t.Fatalf("the queue reached %d bytes, over the cap of %d", maxQueued, b.in.privateQueue)
	}
	waitFor(t, done)
	if code, reason := sc.ws.CloseStatus(); code != 1008 {
		t.Fatalf("closed with %d %q", code, reason)
	}
}

// fillBoard adds n stickies with size characters of text each.
func fillBoard(t *testing.T, b *tb, n, size int) {
	t.Helper()
	for done := 0; done < n; {
		var ops []engine.AgentOp
		for i := done; i < min(n, done+1000); i++ {
			el, _ := json.Marshal(map[string]any{"type": "sticky", "text": noise(size, i), "x": i * 10, "y": 0})
			ops = append(ops, engine.AgentOp{Op: "add", El: el})
		}
		if _, err := b.in.UpdateBoard(context.Background(), UpdateRequest{OpID: "fill-" + randomToken(), Label: "Agent", Ops: ops}, 0); err != nil {
			t.Fatal(err)
		}
		done += len(ops)
	}
}

func heapInUse() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// Slow Public viewers of a large board share one copy of its state and
// hold one encoded part each, not the whole encoded board.
func TestSlowPublicViewersShareTheSnapshot(t *testing.T) {
	b := newTB(t)
	fillBoard(t, b, 2000, 4000) // about 8 MB of encoded elements
	base := heapInUse()
	const viewers = 16
	var clients []*client
	for i := 0; i < viewers; i++ {
		c, err := dial(t, b.pub, origin(b.pub))
		if err != nil {
			t.Fatal(err)
		}
		c.gate.Lock() // the viewer does not read past the welcome
		c.send(map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}})
		clients = append(clients, c)
	}
	defer func() {
		for _, c := range clients {
			c.gate.Unlock()
		}
	}()
	waitFor(t, func() bool {
		n := 0
		for _, c := range b.in.hub.list() {
			if c.public {
				n++
			}
		}
		return n == viewers
	})
	time.Sleep(time.Second) // the pumps fill the sockets and wait
	// A pump still sending or encoding a part holds buffers that are garbage
	// a moment later, so one reading can catch them. What the viewers retain
	// is live in every reading, so one reading within the limit shows they
	// retain no more: read again, every 250 ms and at most 12 times, only
	// while the readings are over it. Check that the viewers were all still
	// connected at that reading.
	grew := int64(heapInUse()) - int64(base)
	for i := 0; i < 12 && grew > 64<<20; i++ {
		t.Logf("reading %d: %d bytes", i, grew)
		time.Sleep(250 * time.Millisecond)
		grew = int64(heapInUse()) - int64(base)
	}
	t.Logf("%d slow viewers retain %d bytes", viewers, grew)
	n := 0
	for _, c := range b.in.hub.list() {
		if c.public {
			n++
		}
	}
	if n != viewers {
		t.Fatalf("%d of %d slow viewers still connected at the reading", n, viewers)
	}
	if grew > 64<<20 {
		t.Fatalf("%d slow Public viewers retain %d bytes", viewers, grew)
	}
	var d *snapData
	b.state(func(*engine.State) { d = b.in.snapCur })
	if d == nil || publicSnaps.Load() != d.cost {
		t.Fatalf("the viewers do not share one charged snapshot state: %v %d", d != nil, publicSnaps.Load())
	}
}

// A Public viewer whose snapshot state would exceed the host's Public
// budget is closed and admitted once the budget has room.
func TestPublicSnapshotStateCountsTowardTheHostBudget(t *testing.T) {
	b := newTB(t)
	fillBoard(t, b, 3071, 4000) // a 3 MiB state; its 12 MB transfer stalls
	var cost int64
	b.state(func(st *engine.State) { cost = int64(len(st.Alive())+1) * snapElementCost })
	old := hostPublicCap
	// Room for one state, a stalled socket's bytes in flight (under
	// 1.5 MiB) and deltas, not for a second state.
	hostPublicCap = cost + 3<<20
	defer func() { hostPublicCap = old }()

	a, err := dial(t, b.pub, origin(b.pub))
	if err != nil {
		t.Fatal(err)
	}
	a.gate.Lock() // holds its snapshot state while the transfer stalls
	a.send(map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}})
	waitFor(t, func() bool { return publicSnaps.Load() == cost })
	fillBoard(t, b, 1, 10) // a new seq needs a new state
	late, err := dial(t, b.pub, origin(b.pub))
	if err != nil {
		t.Fatal(err)
	}
	late.send(map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}})
	if !late.closed(5 * time.Second) {
		t.Fatal("a viewer beyond the host budget stayed open")
	}
	for _, m := range late.backlog {
		if m["t"] == "snapshot" {
			t.Fatal("a viewer beyond the host budget got a snapshot")
		}
	}
	if publicSnaps.Load() != cost {
		t.Fatalf("charged %d, want the first state's %d", publicSnaps.Load(), cost)
	}
	a.ws.Close(1000, "")
	a.gate.Unlock()
	waitFor(t, func() bool { return publicSnaps.Load() == 0 })
	c := connect(t, b.pub, nil, nil)
	if _, els, _ := c.snapshot(); len(els) != len(b.alive()) {
		t.Fatalf("snapshot of %d elements", len(els))
	}
}

func imageArchive(t *testing.T, img []byte) []byte {
	t.Helper()
	hash := sha256Hex(img)
	pic := model.Element{ID: eid(), Type: model.TypeImage, Geom: model.Geom{X: 0, Y: 0, W: 8, H: 8}, Z: "5", Asset: hash, Natural: [2]int{8, 8}}
	return buildArchive(t, boardFile{Format: 1, Title: "x", Grid: true}, []model.Element{pic}, map[string][]byte{"assets/" + hash + ".png": img}, nil)
}

func (f *fakeCore) blobBytes() (n int, size int64) {
	f.data.Lock()
	defer f.data.Unlock()
	for _, b := range f.blobs {
		size += int64(len(b))
	}
	return len(f.blobs), size
}

// Imports and agent calls refused for the quota, or as invalid, leave no
// new blob bytes behind.
func TestRefusedImportsAndAgentCallsStoreNoBlobs(t *testing.T) {
	b := newTB(t)
	ctx := context.Background()
	b.core.setQuota(measureUsage(t, b) + 4<<10)
	blobs, size := b.core.blobBytes()
	for i := 0; i < 5; i++ {
		data := imageArchive(t, noisyPNG(t, 64, 64, 100+i)) // about 12 KiB each
		err := b.core.Exclusive(ctx, func(ctx context.Context, other int64) error {
			_, err := b.in.ImportBoard(ctx, ImportRequest{OpID: "refused-import-" + randomToken(), Mode: "add",
				Archive: bytes.NewReader(data), Size: int64(len(data)), Label: "Agent"}, other)
			return err
		})
		var be *Error
		if !errors.As(err, &be) || be.Category != CategoryCapacity {
			t.Fatalf("import %d over quota: %v", i, err)
		}
		if n, s := b.core.blobBytes(); n != blobs || s != size {
			t.Fatalf("refused import %d stored %d blobs (%d bytes)", i, n-blobs, s-size)
		}
	}
	// An invalid agent call with a new image, quota or not.
	for _, quota := range []int64{0, 1 << 30} {
		b.core.setQuota(quota)
		err := b.core.Exclusive(ctx, func(ctx context.Context, other int64) error {
			_, err := b.in.UpdateBoard(ctx, UpdateRequest{OpID: "invalid-" + randomToken(), Label: "Agent",
				Ops: []engine.AgentOp{{Op: "no-such-op"}}, Assets: [][]byte{noisyPNG(t, 16, 16, int(quota%7)+200)}}, other)
			return err
		})
		if err == nil {
			t.Fatal("an invalid agent call was accepted")
		}
		if n, s := b.core.blobBytes(); n != blobs || s != size {
			t.Fatalf("a refused agent call stored %d blobs (%d bytes)", n-blobs, s-size)
		}
	}
	// An admitted one stores its image.
	img := noisyPNG(t, 16, 16, 300)
	err := b.core.Exclusive(ctx, func(ctx context.Context, other int64) error {
		_, err := b.in.UpdateBoard(ctx, UpdateRequest{OpID: "valid-" + randomToken(), Label: "Agent",
			Ops: []engine.AgentOp{{Op: "add", El: json.RawMessage(`{"type":"sticky","text":"ok"}`)}}, Assets: [][]byte{img}}, other)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.core.ReadBlob(ctx, sha256Hex(img)); err != nil {
		t.Fatalf("the admitted image was not stored: %v", err)
	}
}

// Two uploads with room for one: the second is admitted against the first's
// reservation, so only one is stored.
func TestConcurrentUploadsCannotShareHeadroom(t *testing.T) {
	b := newTB(t)
	ctx := context.Background()
	img1, img2 := noisyPNG(t, 64, 64, 1), noisyPNG(t, 64, 64, 2)
	b.core.setQuota(measureUsage(t, b) + int64(len(img1))*3/2)
	entered, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	b.core.data.Lock()
	b.core.putHook = func(hash string) {
		if hash == sha256Hex(img1) {
			once.Do(func() { close(entered); <-proceed })
		}
	}
	b.core.data.Unlock()
	first := make(chan error, 1)
	go func() {
		_, err := b.in.upload(ctx, bytes.NewReader(img1))
		first <- err
	}()
	<-entered // the first upload passed its check and is storing
	_, err := b.in.upload(ctx, bytes.NewReader(img2))
	close(proceed)
	var be *Error
	if !errors.As(err, &be) || be.Category != CategoryCapacity {
		t.Fatalf("the second upload: %v", err)
	}
	if err := <-first; err != nil {
		t.Fatalf("the first upload: %v", err)
	}
	if _, err := b.core.ReadBlob(ctx, sha256Hex(img2)); err == nil {
		t.Fatal("the refused upload was stored")
	}
	b.in.meas.mu.Lock()
	reserved := b.in.meas.reserved
	b.in.meas.mu.Unlock()
	if reserved != 0 {
		t.Fatalf("%d bytes stay reserved", reserved)
	}
}

// A measurement taken under the flat lock already includes what earlier
// imports stored: installing it counts those bytes once, so a write that
// fits next to them is accepted.
func TestFreshMeasurementCountsStoredBytesOnce(t *testing.T) {
	b := newTB(t)
	ctx := context.Background()
	img := noisyPNG(t, 230, 230, 9) // about 200 KiB
	data := imageArchive(t, img)
	err := b.core.Exclusive(ctx, func(ctx context.Context, other int64) error {
		_, err := b.in.ImportBoard(ctx, ImportRequest{OpID: "import-image-1", Mode: "add", Archive: bytes.NewReader(data),
			Size: int64(len(data)), Label: "Agent"}, other)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	b.core.setQuota(measureUsage(t, b) + int64(len(img))/2)
	for i := 0; i < 3; i++ {
		err = b.core.Exclusive(ctx, func(ctx context.Context, other int64) error {
			_, err := b.in.UpdateBoard(ctx, UpdateRequest{OpID: "near-quota-" + randomToken(), Label: "Agent",
				Ops: []engine.AgentOp{{Op: "add", El: json.RawMessage(`{"type":"sticky","text":"fits"}`)}}}, other)
			return err
		})
		if err != nil {
			t.Fatalf("write %d next to the imported image: %v", i, err)
		}
	}

	// An upload stored without the flat lock may or may not be in core's
	// measurement: install measures again itself; a later blob stays
	// counted.
	m := b.in.meas
	m.add(1000, false)
	mark := m.mark()
	b.core.data.Lock()
	measured := b.core.measured
	b.core.data.Unlock()
	m.install(ctx, 123, mark)
	b.core.data.Lock()
	again := b.core.measured
	b.core.data.Unlock()
	fresh, _ := b.core.MeasureOther(ctx)
	if got, _ := m.get(); again != measured+1 || got != fresh {
		t.Fatalf("after an ambiguous install: %d measurements, value %d, want %d", again-measured, got, fresh)
	}
	m.add(7, true)
	if got, _ := m.get(); got != fresh+7 {
		t.Fatalf("a later blob: %d", got)
	}
	m.add(500, true)
	mark = m.mark()
	m.install(ctx, 4000, mark)
	if got, _ := m.get(); got != 4000 {
		t.Fatalf("after a locked install: %d", got)
	}
}

// The reader and the timeout timer decide an assembly's outcome once: the
// host's assembly bytes return to zero, and no batch both times out and
// applies.
func TestAssemblyTimeoutHasOneOutcome(t *testing.T) {
	old := assemblyTimeout
	assemblyTimeout = time.Millisecond
	defer func() { assemblyTimeout = old }()
	b := newTB(t)
	start := hostAssembly.Load()
	c := connect(t, b.priv, map[string]any{"editor": "parts"}, nil)
	c.snapshot()
	n := 1
	answers := 0
	const attempts = 80
	for i := 0; i < attempts; i++ {
		el, _ := json.Marshal(sticky(eid(), "part", float64(i)))
		for k, data := range []string{"[", string(el), "]"} {
			if k > 0 {
				// Around the timeout, so the timer fires between parts.
				time.Sleep(time.Duration(i%5) * 300 * time.Microsecond)
			}
			c.send(map[string]any{"t": "ops_part", "n": n, "part": k + 1, "parts": 3, "data": data})
		}
		m, ok := c.next(func(m map[string]any) bool {
			return (m["t"] == "ack" || m["t"] == "reject" || m["t"] == "fatal") && (m["n"] == nil || int(m["n"].(float64)) == n)
		})
		if !ok || m["t"] == "fatal" {
			t.Fatalf("attempt %d: %v %v", i, ok, m)
		}
		answers++
		if m["t"] == "ack" || m["retry"] == false {
			n++
		}
	}
	extra := 0
	c.quiet(300*time.Millisecond, func(m map[string]any) bool { return false })
	for _, m := range c.backlog {
		if m["t"] == "ack" || m["t"] == "reject" {
			extra++
		}
	}
	if extra > 0 {
		t.Fatalf("%d answers after %d attempts: an assembly had two outcomes", extra, answers)
	}
	c.ws.Close(1000, "")
	waitFor(t, func() bool { return hostAssembly.Load() == start })
}

// A board whose creation stopped after board.db and its schema existed,
// before initialization committed, initializes on the next open with one
// created entry.
func TestInterruptedCreationRecovers(t *testing.T) {
	for name, prepare := range map[string]func(t *testing.T, path string){
		"empty file": func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"schema only": func(t *testing.T, path string) {
			s, fresh, err := openStore(path)
			if err != nil || !fresh {
				t.Fatalf("openStore: %v %v", fresh, err)
			}
			s.close()
		},
		"uncommitted initialization": func(t *testing.T, path string) {
			// The process stops inside the initialization transaction: a
			// copy of the files then is what the next start finds.
			s, _, err := openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			tx, err := s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for _, k := range []string{"store", "seq", noise(9000, 1)} {
				if err := setMeta(context.Background(), tx, k, noise(9000, 2)); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{dbName, dbName + "-wal"} {
				data, err := os.ReadFile(filepath.Join(filepath.Dir(path), name))
				if err != nil {
					continue
				}
				if err := os.WriteFile(path+".crash"+strings.TrimPrefix(name, dbName), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			flat := t.TempDir()
			dir := filepath.Join(flat, "board")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			prepare(t, filepath.Join(dir, dbName))
			for _, suffix := range []string{"", "-wal"} {
				p := filepath.Join(dir, dbName)
				if data, err := os.ReadFile(p + ".crash" + suffix); err == nil {
					os.Remove(p + "-shm")
					if err := os.WriteFile(p+suffix, data, 0o600); err != nil {
						t.Fatal(err)
					}
					os.Remove(p + ".crash" + suffix)
				}
			}
			core := newFakeCore(flat)
			in, created, err := Open(dir, core, Options{Slug: "x", Title: "Recovered"})
			if err != nil || !created {
				t.Fatalf("open: created %v, %v", created, err)
			}
			core.in = in
			defer in.Stop()
			if err := core.importOutbox(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(core.revs) != 1 || core.revs[0].info.Kind != "created" {
				t.Fatalf("revisions %+v", core.revs)
			}
			in.Stop()
			again, created, err := Open(dir, core, Options{Slug: "x"})
			if err != nil || created {
				t.Fatalf("reopen: created %v, %v", created, err)
			}
			again.Stop()
		})
	}
}

// Filtering a revision page by area resolves connectors against the whole
// revision: concatenated pages equal the unpaged result.
func TestRevisionPagesResolveConnectorsAgainstTheWholeRevision(t *testing.T) {
	a := model.Element{ID: "e_a000000001", Type: model.TypeSticky, Geom: model.Geom{X: 10000, Y: 0, W: 100, H: 100}, Z: "1"}
	bb := model.Element{ID: "e_a000000002", Type: model.TypeSticky, Geom: model.Geom{X: 10300, Y: 0, W: 100, H: 100}, Z: "2"}
	cn := model.Element{ID: "e_z000000001", Type: model.TypeConnector, Z: "3",
		Ends: [2]model.End{{El: a.ID, At: [2]float64{1, 0.5}}, {El: bb.ID, At: [2]float64{0, 0.5}}}}
	c := engine.Content{Title: "r", Grid: true, Elements: []model.Element{cn, a, bb}}
	rect := &[4]float64{9900, -100, 600, 300}
	ids := func(p Page) []string {
		var out []string
		for _, e := range p.Elements {
			var m struct{ ID string }
			_ = json.Unmarshal(e, &m)
			out = append(out, m.ID)
		}
		return out
	}
	all, err := ReadContent(c, Query{Rect: rect, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var paged []string
	q := Query{Rect: rect, Limit: 2}
	for {
		p, err := ReadContent(c, q)
		if err != nil {
			t.Fatal(err)
		}
		paged = append(paged, ids(p)...)
		if p.Next == "" {
			break
		}
		q.Cursor = p.Next
	}
	if strings.Join(paged, ",") != strings.Join(ids(all), ",") || len(paged) != 3 {
		t.Fatalf("paged %v, unpaged %v", paged, ids(all))
	}
}

// ContentFromFiles takes an asset's size from the file list and its pixel
// size from what the host learned, reading a blob only the first time.
func TestContentFromFilesReadsKnownAssetsOnce(t *testing.T) {
	board := model.Canonical(boardFile{Format: 1, Title: "t", Grid: true})
	seed := int(time.Now().UnixNano() % 1e9) // the pixel sizes are remembered host-wide
	checked := noisyPNG(t, 7, 5, seed)
	if _, err := CheckImage(checked, 1<<20); err != nil {
		t.Fatal(err)
	}
	unseen := noisyPNG(t, 9, 3, seed+1)
	blobs := map[string][]byte{sha256Hex(board): board, sha256Hex(checked): checked, sha256Hex(unseen): unseen}
	files := []File{{Path: "board.json", Hash: sha256Hex(board), Size: int64(len(board))}}
	for _, img := range [][]byte{checked, unseen} {
		files = append(files, File{Path: "assets/" + sha256Hex(img) + ".png", Hash: sha256Hex(img), Size: int64(len(img))})
	}
	reads := map[string]int{}
	read := func(h string) ([]byte, error) { reads[h]++; return blobs[h], nil }
	for round := 0; round < 2; round++ {
		c, err := ContentFromFiles(files, read)
		if err != nil {
			t.Fatal(err)
		}
		for img, wh := range map[string][2]int{sha256Hex(checked): {7, 5}, sha256Hex(unseen): {9, 3}} {
			a := c.Assets[img]
			if a.W != wh[0] || a.H != wh[1] || a.Mime != "image/png" || a.Bytes != int64(len(blobs[img])) {
				t.Fatalf("round %d asset %+v", round, a)
			}
		}
	}
	if reads[sha256Hex(checked)] != 0 || reads[sha256Hex(unseen)] != 1 {
		t.Fatalf("asset reads %v", reads)
	}
}

// Admission to the host's combined Public budget is atomic across boards:
// two snapshot states (or a state and queued frames) racing for the last
// room never both get it.
func TestPublicHostBudgetAdmissionIsAtomic(t *testing.T) {
	waitFor(t, func() bool { return publicQueued.Load() == 0 && publicSnaps.Load() == 0 && publicBudget.Used() == 0 })
	const cost = 4 << 20
	old := hostPublicCap
	hostPublicCap = cost + cost/2 // room for one charge, not two
	defer func() { hostPublicCap = old }()
	for round := 0; round < 2000; round++ {
		d1, d2 := &snapData{cost: cost}, &snapData{cost: cost} // the states of two boards
		var won [2]bool
		var start, wg sync.WaitGroup
		start.Add(1)
		wg.Add(2)
		go func() {
			defer wg.Done()
			start.Wait()
			won[0] = d1.acquirePublic(false)
		}()
		go func() {
			defer wg.Done()
			start.Wait()
			if round%2 == 0 {
				won[1] = d2.acquirePublic(false)
			} else {
				won[1] = reservePublic(&publicQueued, cost) // a Public connection's frames
			}
		}()
		start.Done()
		wg.Wait()
		total := publicQueued.Load() + publicSnaps.Load()
		if won[0] {
			d1.releasePublic(false)
		}
		if won[1] && round%2 == 0 {
			d2.releasePublic(false)
		} else if won[1] {
			publicQueued.Add(-cost)
		}
		if won[0] == won[1] || total > hostPublicCap {
			t.Fatalf("round %d: admitted %v, %d charged against room for one under %d", round, won, total, hostPublicCap)
		}
	}
}

// Snapshot parts count toward the host's Public budget: a viewer whose
// state fits but whose next part does not is closed without a snapshot,
// and nothing stays charged.
func TestPublicSnapshotPartsCountTowardTheHostBudget(t *testing.T) {
	b := newTB(t)
	fillBoard(t, b, 20, 100)
	var cost int64
	b.state(func(st *engine.State) { cost = int64(len(st.Alive())+1) * snapElementCost })
	waitFor(t, func() bool { return publicQueued.Load() == 0 && publicSnaps.Load() == 0 && publicBudget.Used() == 0 })
	old := hostPublicCap
	hostPublicCap = cost + maxServerFrame/2 // the state fits, a part does not
	defer func() { hostPublicCap = old }()
	v, err := dial(t, b.pub, origin(b.pub))
	if err != nil {
		t.Fatal(err)
	}
	v.send(map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}})
	if !v.closed(5 * time.Second) {
		t.Fatal("a viewer whose snapshot part exceeds the host budget stayed open")
	}
	for _, m := range v.backlog {
		if m["t"] == "snapshot" {
			t.Fatal("a snapshot part went past the host budget")
		}
	}
	waitFor(t, func() bool { return publicQueued.Load() == 0 && publicSnaps.Load() == 0 && publicBudget.Used() == 0 })
	hostPublicCap = old
	c := connect(t, b.pub, nil, nil)
	if _, els, _ := c.snapshot(); len(els) != len(b.alive()) {
		t.Fatalf("snapshot of %d elements", len(els))
	}
}
