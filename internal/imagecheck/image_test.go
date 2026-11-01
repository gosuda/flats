package imagecheck

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
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
		t.Run(name, func(t *testing.T) {
			limit := int64(len(c.data)) + 1000
			if name == "upload limit" {
				limit = 1000
			}
			info, err := CheckImage(c.data, limit)
			if c.fail != "" {
				if err == nil || !strings.Contains(err.Error(), c.fail) {
					t.Errorf("%s: %v", name, err)
				}
				return
			}
			if err != nil || info.Mime != c.mime || info.W < 1 {
				t.Errorf("%s: %+v %v", name, info, err)
			}
		})
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
		t.Run(name, func(t *testing.T) {
			_, err := CheckImage(c.data, 1<<20)
			if err == nil || !strings.Contains(err.Error(), c.fail) {
				t.Errorf("%s: %v", name, err)
			}
		})
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

func TestImageMetadataAndUploadBoundary(t *testing.T) {
	data := pngBytes(t, 4, 3)
	original := bytes.Clone(data)
	hash := sha256.Sum256(data)
	want := AssetInfo{Hash: hex.EncodeToString(hash[:]), Mime: "image/png", W: 4, H: 3, Bytes: int64(len(data))}
	for name, limit := range map[string]int64{"exact": int64(len(data)), "unlimited": 0, "negative": -1} {
		t.Run(name, func(t *testing.T) {
			got, err := CheckImage(data, limit)
			if err != nil || got != want {
				t.Fatalf("image = %+v, %v; want %+v", got, err, want)
			}
			if !bytes.Equal(data, original) {
				t.Fatal("image bytes changed")
			}
		})
	}
	got, err := CheckImage(data, int64(len(data)-1))
	if err == nil || !strings.Contains(err.Error(), "upload limit") || got != (AssetInfo{}) {
		t.Fatalf("over upload limit = %+v, %v", got, err)
	}
	for i := 0; i < 8; i++ {
		t.Run("concurrent", func(t *testing.T) {
			t.Parallel()
			got, err := CheckImage(data, 0)
			if err != nil || got != want {
				t.Errorf("image = %+v, %v; want %+v", got, err, want)
			}
		})
	}
}

func TestImageDimensionBoundaries(t *testing.T) {
	for name, c := range map[string]struct {
		w, h int
		fail string
	}{
		"zero width":          {0, 1, "over the limit"},
		"zero height":         {1, 0, "over the limit"},
		"maximum side":        {16384, 1, "cannot be decoded"},
		"over maximum side":   {16385, 1, "over the limit"},
		"maximum pixels":      {6000, 4000, "cannot be decoded"},
		"over maximum pixels": {6001, 4000, "over the limit"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := CheckImage(pngHeader(c.w, c.h), 0)
			if err == nil || !strings.Contains(err.Error(), c.fail) {
				t.Fatalf("%dx%d: %v", c.w, c.h, err)
			}
		})
	}
}

func TestWebPDuplicateCanvas(t *testing.T) {
	data := append(webpVP8X(46340, 46340), webpVP8X(1, 1)[12:]...)
	_, err := CheckImage(data, 0)
	if err == nil || !strings.Contains(err.Error(), "more than one VP8X") {
		t.Fatalf("duplicate canvas: %v", err)
	}
}
