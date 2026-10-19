package board

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/board/model"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// stillWebP is a 1×1 lossless WebP.
var stillWebP, _ = base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")

func TestImageChecks(t *testing.T) {
	var jb bytes.Buffer
	_ = jpeg.Encode(&jb, image.NewRGBA(image.Rect(0, 0, 3, 2)), nil)
	var gb bytes.Buffer
	_ = gif.EncodeAll(&gb, &gif.GIF{Image: []*image.Paletted{
		image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White}),
		image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White}),
	}, Delay: []int{1, 1}})
	for name, c := range map[string]struct {
		data       []byte
		mime, fail string
	}{
		"png":  {data: pngBytes(t, 4, 3), mime: "image/png"},
		"jpeg": {data: jb.Bytes(), mime: "image/jpeg"},
		"gif":  {data: gb.Bytes(), mime: "image/gif"},
		"webp": {data: stillWebP, mime: "image/webp"},
		"forged signature": {data: append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR\x00\x00\x00\x04\x00\x00\x00\x03"), bytes.Repeat([]byte{7}, 64)...),
			fail: "cannot be decoded"},
		"truncated":         {data: pngBytes(t, 40, 30)[:60], fail: "cannot be decoded"},
		"pixel bomb header": {data: pngHeader(20000, 20000), fail: "over the limit"},
		"gif frame bomb":    {data: gifFrames(501), fail: "animation is over the limit"},
		"animated webp":     {data: animatedWebP(), fail: "animated WebP"},
		"svg":               {data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), fail: "only PNG"},
		"upload limit":      {data: bytes.Repeat([]byte{0}, 2000), fail: "upload limit"},
	} {
		info, err := CheckImage(c.data, 1000+int64(len(c.data))*map[bool]int64{true: 0, false: 1}[name == "upload limit"])
		if c.fail != "" {
			if err == nil || !strings.Contains(err.Error(), c.fail) {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err != nil || info.Mime != c.mime || info.W < 1 {
			t.Errorf("%s: %+v %v", name, info, err)
		}
	}
}

func pngHeader(w, h int) []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr, uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(h))
	ihdr[8], ihdr[9] = 8, 6
	binary.Write(&b, binary.BigEndian, uint32(13))
	b.WriteString("IHDR")
	b.Write(ihdr)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(append([]byte("IHDR"), ihdr...)))
	return b.Bytes()
}

// gifFrames builds a GIF with n tiny frames.
func gifFrames(n int) []byte {
	var b bytes.Buffer
	b.WriteString("GIF89a")
	b.Write([]byte{1, 0, 1, 0, 0x80, 0, 0, 0, 0, 0, 255, 255, 255})
	for i := 0; i < n; i++ {
		b.Write([]byte{0x2C, 0, 0, 0, 0, 1, 0, 1, 0, 0, 2, 2, 0x4C, 0x01, 0})
	}
	b.WriteByte(0x3B)
	return b.Bytes()
}

func animatedWebP() []byte {
	var body bytes.Buffer
	body.WriteString("WEBP")
	body.WriteString("VP8X")
	binary.Write(&body, binary.LittleEndian, uint32(10))
	body.Write([]byte{0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	body.WriteString("ANIM")
	binary.Write(&body, binary.LittleEndian, uint32(6))
	body.Write(make([]byte, 6))
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(body.Len()))
	b.Write(body.Bytes())
	return b.Bytes()
}

// buildArchive writes a data file; extra adds raw entries.
func buildArchive(t *testing.T, board boardFile, els []model.Element, assets map[string][]byte, extra map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	put := func(name string, data []byte) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data)
	}
	put("board.json", model.Canonical(board))
	if els == nil {
		els = []model.Element{}
	}
	raw, _ := json.Marshal(els)
	put("elements.json", raw)
	for name, data := range assets {
		put(name, data)
	}
	for name, data := range extra {
		put(name, data)
	}
	zw.Close()
	return b.Bytes()
}

func TestArchiveChecks(t *testing.T) {
	img := pngBytes(t, 4, 4)
	hash := sha256Hex(img)
	imgEl := model.Element{ID: eid(), Type: model.TypeImage, Geom: model.Geom{W: 4, H: 4}, Z: "V", Asset: hash, Natural: [2]int{4, 4}}
	ok := buildArchive(t, boardFile{Format: 1, Title: "t", Grid: true}, []model.Element{imgEl}, map[string][]byte{"assets/" + hash + ".png": img}, nil)
	if _, err := parseArchive(bytes.NewReader(ok), int64(len(ok)), 1<<20, 1<<20); err != nil {
		t.Fatalf("valid archive: %v", err)
	}
	cases := map[string][]byte{
		"path traversal":   buildArchive(t, boardFile{Format: 1}, nil, nil, map[string][]byte{"../escape.json": []byte("{}")}),
		"nested traversal": buildArchive(t, boardFile{Format: 1}, nil, nil, map[string][]byte{"assets/../../x.png": img}),
		"hash mismatch":    buildArchive(t, boardFile{Format: 1}, nil, map[string][]byte{"assets/" + strings.Repeat("a", 64) + ".png": img}, nil),
		"wrong extension":  buildArchive(t, boardFile{Format: 1}, nil, map[string][]byte{"assets/" + hash + ".gif": img}, nil),
		"missing asset":    buildArchive(t, boardFile{Format: 1}, []model.Element{imgEl}, nil, nil),
		"server field":     buildArchive(t, boardFile{Format: 1}, nil, nil, nil),
		"future format":    buildArchive(t, boardFile{Format: 2}, nil, nil, nil),
		"svg asset":        buildArchive(t, boardFile{Format: 1}, nil, map[string][]byte{"assets/" + sha256Hex([]byte("<svg/>")) + ".png": []byte("<svg/>")}, nil),
		"duplicate path":   duplicateZip(t),
		"zip bomb":         bombZip(t, 4<<20),
		"forged sizes":     forgedSizeZip(t),
	}
	// The server-field case: replace elements.json with an element that
	// carries inc.
	cases["server field"] = rawElementsZip(t, `[{"id":"e_aaaaaaaaaa","type":"sticky","x":0,"y":0,"w":1,"h":1,"rot":0,"z":"V","inc":5}]`)
	for name, data := range cases {
		_, err := parseArchive(bytes.NewReader(data), int64(len(data)), 1<<20, 1<<20)
		var be *Error
		if !errors.As(err, &be) || be.Category != CategoryInvalid {
			t.Errorf("%s: %v", name, err)
		}
		if strings.HasSuffix(name, "traversal") && !strings.Contains(err.Error(), `".."`) {
			t.Errorf("%s: refused for another reason: %v", name, err)
		}
	}
	many := map[string][]byte{}
	for i := 0; i < archiveMaxEntries; i++ {
		many["x"+strings.Repeat("y", i%5)+string(rune('a'+i%26))+string(rune('a'+i/26%26))] = nil
	}
	if _, err := parseArchive(bytes.NewReader(buildArchive(t, boardFile{Format: 1}, nil, nil, many)), 0, 1<<20, 1<<20); err == nil {
		t.Error("too many entries accepted")
	}
}

func rawElementsZip(t *testing.T, elements string) []byte {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, _ := zw.Create("board.json")
	w.Write([]byte(`{"format":1,"title":"","grid":true}`))
	w, _ = zw.Create("elements.json")
	w.Write([]byte(elements))
	zw.Close()
	return b.Bytes()
}

func duplicateZip(t *testing.T) []byte {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, name := range []string{"board.json", "elements.json", "elements.json"} {
		w, _ := zw.Create(name)
		if name == "board.json" {
			w.Write([]byte(`{"format":1,"title":"","grid":true}`))
		} else {
			w.Write([]byte(`[]`))
		}
	}
	zw.Close()
	return b.Bytes()
}

// bombZip compresses n bytes of zeros into one entry.
func bombZip(t *testing.T, n int) []byte {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, _ := zw.Create("board.json")
	w.Write([]byte(`{"format":1,"title":"","grid":true}`))
	w, _ = zw.Create("elements.json")
	w.Write([]byte("["))
	w.Write(bytes.Repeat([]byte(" "), n))
	w.Write([]byte("]"))
	zw.Close()
	return b.Bytes()
}

// forgedSizeZip declares a tiny uncompressed size for an entry that
// inflates to megabytes.
func forgedSizeZip(t *testing.T) []byte {
	var c bytes.Buffer
	fw, _ := flate.NewWriter(&c, flate.BestCompression)
	fw.Write(bytes.Repeat([]byte(" "), 3<<20))
	fw.Close()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, _ := zw.Create("board.json")
	w.Write([]byte(`{"format":1,"title":"","grid":true}`))
	w, _ = zw.CreateRaw(&zip.FileHeader{Name: "elements.json", Method: zip.Deflate, CompressedSize64: uint64(c.Len()), UncompressedSize64: 2})
	w.Write(c.Bytes())
	zw.Close()
	return b.Bytes()
}

func TestImportAddRelinksAndRecovery(t *testing.T) {
	b := newTB(t)
	ctx := context.Background()
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	existing := eid()
	a.batch(1, sticky(existing, "existing", 0))

	img := pngBytes(t, 8, 8)
	hash := sha256Hex(img)
	frame := model.Element{ID: eid(), Type: model.TypeFrame, Geom: model.Geom{X: 0, Y: 0, W: 600, H: 400}, Z: "1", Text: "F", Style: model.DefaultStyle(model.TypeFrame)}
	s1 := model.Element{ID: eid(), Type: model.TypeSticky, Geom: model.Geom{X: 10, Y: 10, W: 100, H: 100}, Z: "2", Frame: frame.ID, Text: "s1", Style: model.DefaultStyle(model.TypeSticky), Locked: true}
	s2 := model.Element{ID: eid(), Type: model.TypeSticky, Geom: model.Geom{X: 200, Y: 10, W: 100, H: 100}, Z: "3", Text: "s2", Style: model.DefaultStyle(model.TypeSticky)}
	conn := model.Element{ID: eid(), Type: model.TypeConnector, Z: "4", Ends: [2]model.End{{El: s1.ID, At: [2]float64{1, 0.5}}, {El: s2.ID, At: [2]float64{0, 0.5}}},
		Style: model.DefaultStyle(model.TypeConnector)}
	pic := model.Element{ID: eid(), Type: model.TypeImage, Geom: model.Geom{X: 400, Y: 10, W: 8, H: 8}, Z: "5", Asset: hash, Natural: [2]int{8, 8}, Alt: "a cat"}
	data := buildArchive(t, boardFile{Format: 1, Title: "x", Grid: true}, []model.Element{frame, s1, s2, conn, pic},
		map[string][]byte{"assets/" + hash + ".png": img}, nil)
	resp, m := b.post(b.priv, "/_board/api/import?mode=add", "application/zip", bytes.NewReader(data))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import %d %v", resp.StatusCode, m)
	}
	ids := m["ids"].([]any)
	if len(ids) != 5 {
		t.Fatalf("ids %v", ids)
	}
	got := map[string]model.Element{}
	b.in.do(ctx, func() error {
		for _, e := range b.in.st.Alive() {
			got[e.ID] = e.Element
		}
		return nil
	})
	nf, ns1, ns2, nc := got[ids[0].(string)], got[ids[1].(string)], got[ids[2].(string)], got[ids[3].(string)]
	if nf.Type != model.TypeFrame || ns1.Frame != nf.ID || !ns1.Locked || nc.Ends[0].El != ns1.ID || nc.Ends[1].El != ns2.ID {
		t.Fatalf("relinked %+v %+v %+v", nf, ns1, nc)
	}
	if ns1.Geom.X <= 200 {
		t.Fatalf("imported elements were not placed right of the board: %v", ns1.Geom)
	}
	// The imported image is served on the private route.
	r, err := http.Get(b.priv.URL + "/_board/a/" + hash)
	if err != nil || r.StatusCode != 200 || r.Header.Get("Content-Type") != "image/png" || r.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("asset %v %v", r.StatusCode, err)
	}
	r.Body.Close()

	// A recovery file: changed elements carry recovered_from, a missing
	// image becomes text, loose elements go into a new frame.
	rs := s2
	rs.ID = eid()
	missingHash := strings.Repeat("b", 64)
	lost := model.Element{ID: eid(), Type: model.TypeImage, Geom: model.Geom{X: 0, Y: 300, W: 50, H: 50}, Z: "7", Asset: missingHash, Natural: [2]int{5, 5}, Alt: "lost"}
	elsJSON, _ := json.Marshal([]any{withField(rs, "recovered_from", s2.ID), lost})
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for name, body := range map[string]string{
		"board.json":    `{"format":1,"title":"x","grid":true}`,
		"elements.json": string(elsJSON),
		"recovery.json": `{"format":1,"created_at":"2026-10-19T00:00:00Z","deleted":[],"missing_assets":["` + missingHash + `"]}`,
	} {
		w, _ := zw.Create(name)
		w.Write([]byte(body))
	}
	zw.Close()
	resp, m = b.post(b.priv, "/_board/api/import?mode=add", "application/zip", bytes.NewReader(zb.Bytes()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recovery import %d %v", resp.StatusCode, m)
	}
	rids := m["ids"].([]any)
	got = map[string]model.Element{}
	b.in.do(ctx, func() error {
		for _, e := range b.in.st.Alive() {
			got[e.ID] = e.Element
		}
		return nil
	})
	var texts, frames int
	for _, id := range rids {
		e := got[id.(string)]
		switch e.Type {
		case model.TypeFrame:
			frames++
			if !strings.HasPrefix(e.Text, "Recovered edits") {
				t.Fatalf("frame %q", e.Text)
			}
		case model.TypeText:
			texts++
			if e.Text != "Could not recover image: lost" {
				t.Fatalf("text %q", e.Text)
			}
		}
		if e.Type != model.TypeFrame && e.Frame == "" {
			t.Fatalf("recovered %s is outside the recovery frame", e.ID)
		}
	}
	if texts != 1 || frames != 1 {
		t.Fatalf("recovered %v", rids)
	}
	if resp, _ := b.post(b.priv, "/_board/api/import?mode=replace", "application/zip", bytes.NewReader(zb.Bytes())); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("recovery file with replace: %d", resp.StatusCode)
	}
}

func withField(el model.Element, k, v string) map[string]any {
	var m map[string]any
	json.Unmarshal(el.CanonicalJSON(), &m)
	m[k] = v
	return m
}

func TestExportRoundTripAndPublicProjection(t *testing.T) {
	b := newTB(t)
	a := connect(t, b.priv, map[string]any{"editor": "alice", "name": "Alice"}, nil)
	a.snapshot()
	img := pngBytes(t, 4, 4)
	resp, m := b.post(b.priv, "/_board/api/assets", "image/png", bytes.NewReader(img))
	if resp.StatusCode != 200 || m["w"] != 4.0 {
		t.Fatalf("upload %d %v", resp.StatusCode, m)
	}
	hash := m["hash"].(string)
	pic := eid()
	if r := a.batch(1, map[string]any{"op": "add", "refs": map[string]any{hash: 0},
		"el": map[string]any{"id": pic, "type": "image", "x": 0, "y": 0, "w": 4, "h": 4, "rot": 0, "asset": hash, "natural": []int{4, 4}, "alt": "dot"}}); r["t"] != "ack" {
		t.Fatal(r)
	}
	for _, srv := range []*httpServer{{b.priv.URL, false}, {b.pub.URL, true}} {
		r, err := http.Get(srv.url + "/_board/export.zip")
		if err != nil || r.StatusCode != 200 {
			t.Fatal(r.StatusCode, err)
		}
		var buf bytes.Buffer
		buf.ReadFrom(r.Body)
		r.Body.Close()
		zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, f := range zr.File {
			names[f.Name] = true
			if f.Name == "elements.json" {
				rc, _ := f.Open()
				var els []map[string]any
				json.NewDecoder(rc).Decode(&els)
				rc.Close()
				_, hasBy := els[0]["by"]
				if hasBy == srv.public {
					t.Fatalf("public %v export by field present %v", srv.public, hasBy)
				}
			}
		}
		if !names["assets/"+hash+".png"] || !names["board.json"] {
			t.Fatalf("export entries %v", names)
		}
		if !srv.public {
			if _, err := parseArchive(bytes.NewReader(buf.Bytes()), int64(buf.Len()), 1<<20, 1<<20); err != nil {
				t.Fatalf("the export does not import: %v", err)
			}
		}
	}
	// Past-only assets: after the image is deleted, Public gets 404 while
	// Private still reaches it (deletion undo and history).
	a.batch(2, map[string]any{"op": "delete", "ids": []string{pic}, "inc_of": map[string]any{pic: 0}})
	if r, _ := http.Get(b.pub.URL + "/_board/a/" + hash); r.StatusCode != http.StatusNotFound {
		t.Fatalf("public past-only asset %d", r.StatusCode)
	}
	if r, _ := http.Get(b.priv.URL + "/_board/a/" + hash); r.StatusCode != http.StatusOK {
		t.Fatalf("private deleted asset %d", r.StatusCode)
	}
	// SVG and forged uploads are refused.
	if resp, _ := b.post(b.priv, "/_board/api/assets", "image/svg+xml", strings.NewReader("<svg/>")); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("svg upload %d", resp.StatusCode)
	}
}

type httpServer struct {
	url    string
	public bool
}

// A legal board of highly repetitive content compresses far past the image
// ratio; Flats must still import its own export, while a bomb stays refused.
func TestRepetitiveExportRoundTrip(t *testing.T) {
	var els []model.Element
	n := 0
	zkey := func() string { // fixed-length keys that never end with "0"
		n++
		return "V" + fmt.Sprintf("%04d", n) + "1"
	}
	for i := 0; i < 1000; i++ {
		z := zkey()
		els = append(els, model.Element{ID: eid(), Type: model.TypeSticky, Geom: model.Geom{X: float64(i), Y: 0, W: 100, H: 100}, Z: z,
			Text: strings.Repeat("a", model.MaxText), Style: model.DefaultStyle(model.TypeSticky)})
	}
	pts := make([]int, 2*model.MaxPenPoints)
	for i := range pts {
		pts[i] = 7
	}
	for i := 0; i < 50; i++ {
		z := zkey()
		els = append(els, model.Element{ID: eid(), Type: model.TypePen, Geom: model.Geom{X: 0, Y: float64(i), W: 10, H: 10}, Z: z,
			Pts: pts, Style: model.DefaultStyle(model.TypePen)})
	}
	// The source file stores elements.json, as the client writes it.
	var src bytes.Buffer
	zw := zip.NewWriter(&src)
	w, _ := zw.CreateHeader(&zip.FileHeader{Name: "board.json", Method: zip.Store})
	w.Write(model.Canonical(boardFile{Format: 1, Title: "big", Grid: true}))
	w, _ = zw.CreateHeader(&zip.FileHeader{Name: "elements.json", Method: zip.Store})
	raw, _ := json.Marshal(els)
	w.Write(raw)
	zw.Close()

	b := newTB(t)
	if resp, m := b.post(b.priv, "/_board/api/import?mode=replace", "application/zip", bytes.NewReader(src.Bytes())); resp.StatusCode != http.StatusOK {
		t.Fatalf("import %d %v", resp.StatusCode, m)
	}
	r, err := http.Get(b.priv.URL + "/_board/export.zip")
	if err != nil || r.StatusCode != 200 {
		t.Fatal(r.StatusCode, err)
	}
	var exp bytes.Buffer
	exp.ReadFrom(r.Body)
	r.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(exp.Bytes()), int64(exp.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name == "elements.json" && f.UncompressedSize64 < archiveMaxRatio*f.CompressedSize64 {
			t.Fatalf("the probe does not exceed the ratio: %d of %d", f.CompressedSize64, f.UncompressedSize64)
		}
	}
	resp, m := b.post(b.priv, "/_board/api/import?mode=replace", "application/zip", bytes.NewReader(exp.Bytes()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Flats refuses its own export: %d %v", resp.StatusCode, m)
	}
	if n := len(m["ids"].([]any)); n != len(els) {
		t.Fatalf("round trip kept %d of %d elements", n, len(els))
	}

	// Bombs: a JSON entry past the decompressed total, and an image entry
	// past the ratio although within the total.
	if _, err := parseArchive(bytes.NewReader(exp.Bytes()), int64(exp.Len()), 1<<20, 1<<20); err == nil {
		t.Fatal("an elements.json past the decompressed total was accepted")
	}
	var c bytes.Buffer
	zw = zip.NewWriter(&c)
	w, _ = zw.Create("board.json")
	w.Write([]byte(`{"format":1,"title":"","grid":true}`))
	w, _ = zw.Create("elements.json")
	w.Write([]byte("[]"))
	w, _ = zw.Create("assets/" + strings.Repeat("a", 64) + ".png")
	w.Write(make([]byte, 4<<20))
	zw.Close()
	_, err = parseArchive(bytes.NewReader(c.Bytes()), int64(c.Len()), 64<<20, 64<<20)
	if err == nil || !strings.Contains(err.Error(), "too highly compressed") {
		t.Fatalf("an image bomb within the total: %v", err)
	}
}
