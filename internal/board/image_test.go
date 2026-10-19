package board

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/jpeg"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/board/model"
)

// TestImageChecksEveryFormat extends TestImageChecks to the forged
// signatures, truncations and pixel bombs of each accepted format
// (board-TECH 10, 14 Images and zip). Every case is refused as invalid
// before any decoder allocation the walk did not bound.
func TestImageChecksEveryFormat(t *testing.T) {
	var jb bytes.Buffer
	_ = jpeg.Encode(&jb, image.NewRGBA(image.Rect(0, 0, 40, 30)), nil)
	jpg := jb.Bytes()
	for name, c := range map[string]struct {
		data []byte
		fail string
	}{
		"forged jpeg signature":  {append([]byte{0xFF, 0xD8, 0xFF}, bytes.Repeat([]byte{7}, 64)...), "JPEG"},
		"truncated jpeg":         {jpg[:len(jpg)/2], "cannot be decoded"},
		"jpeg header only":       {jpg[:20], "JPEG"},
		"jpeg pixel bomb":        {jpegSOF(20000, 20000), "over the limit"},
		"jpeg side bomb":         {jpegSOF(16385, 10), "over the limit"},
		"forged gif signature":   {append([]byte("GIF89a\x01\x00\x01\x00\x00\x00\x00"), 0x99, 0x3B), "GIF is damaged"},
		"truncated gif":          {gifFrames(3)[:30], "truncated"},
		"gif without trailer":    {gifFrames(2)[:len(gifFrames(2))-1], "truncated"},
		"gif all-frames pixels":  {gifSized(9, 5000, 5000), "animation is over the limit"},
		"gif frame side bomb":    {gifSized(1, 20000, 1), "over the limit"},
		"forged webp signature":  {webpChunk("VP8 ", bytes.Repeat([]byte{1}, 12)), "WebP header is damaged"},
		"truncated webp":         {stillWebP[:len(stillWebP)-6], "WebP"},
		"webp pixel bomb":        {webpVP8L(10000, 10000), "over the limit"},
		"webp canvas mismatch":   {append(webpVP8X(100, 100), webpVP8LBody(200, 200)...), "does not match"},
		"riff without webp":      {[]byte("RIFF\x04\x00\x00\x00WAVE"), "only PNG"},
		"empty":                  {nil, "only PNG"},
		"png missing ihdr bytes": {[]byte("\x89PNG\r\n\x1a\n\x00\x00"), "PNG header is damaged"},
	} {
		_, err := CheckImage(c.data, 1<<20)
		var be *Error
		if err == nil || !errors.As(err, &be) || be.Category != CategoryInvalid || !strings.Contains(err.Error(), c.fail) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// jpegSOF is a JPEG whose first frame header declares w×h.
func jpegSOF(w, h int) []byte {
	b := []byte{0xFF, 0xD8, 0xFF, 0xC0, 0x00, 0x11, 0x08}
	b = binary.BigEndian.AppendUint16(b, uint16(h))
	b = binary.BigEndian.AppendUint16(b, uint16(w))
	b = append(b, 3, 1, 0x11, 0, 2, 0x11, 0, 3, 0x11, 0)
	return append(b, 0xFF, 0xD9)
}

// gifSized builds a GIF with n frames of fw×fh and a 1×1 screen.
func gifSized(n, fw, fh int) []byte {
	var b bytes.Buffer
	b.WriteString("GIF89a")
	b.Write([]byte{1, 0, 1, 0, 0x80, 0, 0, 0, 0, 0, 255, 255, 255})
	for i := 0; i < n; i++ {
		b.Write([]byte{0x2C, 0, 0, 0, 0})
		binary.Write(&b, binary.LittleEndian, uint16(fw))
		binary.Write(&b, binary.LittleEndian, uint16(fh))
		b.Write([]byte{0, 2, 2, 0x4C, 0x01, 0})
	}
	b.WriteByte(0x3B)
	return b.Bytes()
}

func webpChunk(tag string, body []byte) []byte {
	var c bytes.Buffer
	c.WriteString("WEBP")
	c.WriteString(tag)
	binary.Write(&c, binary.LittleEndian, uint32(len(body)))
	c.Write(body)
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(c.Len()))
	b.Write(c.Bytes())
	return b.Bytes()
}

// webpVP8LBody is a VP8L chunk header declaring w×h.
func webpVP8LBody(w, h int) []byte {
	var b bytes.Buffer
	b.WriteString("VP8L")
	binary.Write(&b, binary.LittleEndian, uint32(5))
	b.WriteByte(0x2f)
	binary.Write(&b, binary.LittleEndian, uint32(w-1)|uint32(h-1)<<14)
	b.WriteByte(0) // padding to an even size
	return b.Bytes()
}

func webpVP8L(w, h int) []byte {
	body := webpVP8LBody(w, h)
	return webpChunk(string(body[:4]), body[8:13])
}

// webpVP8X is a RIFF/WEBP prefix with a still VP8X canvas of w×h; the
// RIFF size is not checked by the walk.
func webpVP8X(w, h int) []byte {
	c := make([]byte, 10)
	c[4], c[5], c[6] = byte(w-1), byte((w-1)>>8), byte((w-1)>>16)
	c[7], c[8], c[9] = byte(h-1), byte((h-1)>>8), byte((h-1)>>16)
	return webpChunk("VP8X", c)
}

// TestArchiveTruncatedAndForged: a truncated data file and one whose
// image entry is not an image are refused as invalid (board-TECH 9.2, 14).
func TestArchiveTruncatedAndForged(t *testing.T) {
	img := pngBytes(t, 4, 4)
	hash := sha256Hex(img)
	el := model.Element{ID: eid(), Type: model.TypeImage, Geom: model.Geom{W: 4, H: 4}, Z: "V", Asset: hash, Natural: [2]int{4, 4}}
	ok := buildArchive(t, boardFile{Format: 1, Title: "t", Grid: true}, []model.Element{el}, map[string][]byte{"assets/" + hash + ".png": img}, nil)
	forged := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR\x00\x00\x00\x04\x00\x00\x00\x04"), bytes.Repeat([]byte{7}, 64)...)
	cases := map[string][]byte{
		"truncated":         ok[:len(ok)/2],
		"no central dir":    ok[:len(ok)-22],
		"forged image":      buildArchive(t, boardFile{Format: 1}, nil, map[string][]byte{"assets/" + sha256Hex(forged) + ".png": forged}, nil),
		"truncated image":   buildArchive(t, boardFile{Format: 1}, nil, map[string][]byte{"assets/" + sha256Hex(img[:40]) + ".png": img[:40]}, nil),
		"image pixel bomb":  buildArchive(t, boardFile{Format: 1}, nil, map[string][]byte{"assets/" + sha256Hex(pngHeader(20000, 20000)) + ".png": pngHeader(20000, 20000)}, nil),
		"gif frame bomb":    buildArchive(t, boardFile{Format: 1}, nil, map[string][]byte{"assets/" + sha256Hex(gifFrames(501)) + ".gif": gifFrames(501)}, nil),
		"not a zip":         []byte("PK\x03\x04 this is not a zip"),
		"empty":             {},
		"duplicate asset":   duplicateAssetZip(t, img),
		"elements not list": rawElementsZip(t, `{"id":"e_aaaaaaaaaa"}`),
	}
	for name, data := range cases {
		_, err := parseArchive(bytes.NewReader(data), int64(len(data)), 1<<20, 1<<20)
		var be *Error
		if !errors.As(err, &be) || be.Category != CategoryInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// duplicateAssetZip names the same asset path twice.
func duplicateAssetZip(t *testing.T, img []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	name := "assets/" + sha256Hex(img) + ".png"
	for _, e := range []struct {
		name string
		data []byte
	}{{"board.json", []byte(`{"format":1,"title":"","grid":true}`)}, {"elements.json", []byte(`[]`)}, {name, img}, {name, img}} {
		w, _ := zw.Create(e.name)
		w.Write(e.data)
	}
	zw.Close()
	return b.Bytes()
}
