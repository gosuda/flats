package imagecheck

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"slices"
	"sync"

	"golang.org/x/image/webp"
)

// Image decoding limits.
const (
	maxImagePixels = 24_000_000
	maxImageSide   = 16384
	maxGIFFrames   = 500
	maxGIFPixels   = 200_000_000
)

// decodeMu makes the host decode one image at a time.
var decodeMu sync.Mutex

// AssetInfo describes a checked image.
type AssetInfo struct {
	Hash  string `json:"hash"`
	Mime  string `json:"mime"`
	W     int    `json:"w"`
	H     int    `json:"h"`
	Bytes int64  `json:"bytes"`
}

// imageHeader is what the header walk learned before any decoding.
type imageHeader struct {
	mime   string
	w, h   int
	frames [][2]int // GIF: byte ranges of each frame's blocks
	prefix int      // GIF: header, screen descriptor and global table
}

func badImage(format string, args ...any) error {
	return fmt.Errorf("the image cannot be accepted: "+format, args...)
}

// CheckImage accepts PNG, JPEG, GIF and still WebP by signature, checks the
// size bounds by walking the headers before decoding, and then decodes the
// whole image. It never re-encodes.
func CheckImage(data []byte, uploadMax int64) (AssetInfo, error) {
	if uploadMax > 0 && int64(len(data)) > uploadMax {
		return AssetInfo{}, badImage("it is larger than the upload limit of %d bytes", uploadMax)
	}
	h, err := walkImage(data)
	if err != nil {
		return AssetInfo{}, err
	}
	if h.w < 1 || h.h < 1 || h.w > maxImageSide || h.h > maxImageSide || int64(h.w)*int64(h.h) > maxImagePixels {
		return AssetInfo{}, badImage("%d×%d pixels is over the limit (16,384 per side, 24 megapixels)", h.w, h.h)
	}
	decodeMu.Lock()
	defer decodeMu.Unlock()
	if err := decodeImage(data, h); err != nil {
		return AssetInfo{}, err
	}
	hash := sha256.Sum256(data)
	return AssetInfo{Hash: hex.EncodeToString(hash[:]), Mime: h.mime, W: h.w, H: h.h, Bytes: int64(len(data))}, nil
}

func walkImage(data []byte) (*imageHeader, error) {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		if len(data) < 33 || string(data[12:16]) != "IHDR" {
			return nil, badImage("the PNG header is damaged")
		}
		return &imageHeader{mime: "image/png", w: int(binary.BigEndian.Uint32(data[16:20])), h: int(binary.BigEndian.Uint32(data[20:24]))}, nil
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return walkJPEG(data)
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return walkGIF(data)
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return walkWebP(data)
	}
	return nil, badImage("only PNG, JPEG, GIF and still WebP images are accepted")
}

func walkJPEG(data []byte) (*imageHeader, error) {
	i := 2
	for i+4 <= len(data) {
		if data[i] != 0xFF {
			return nil, badImage("the JPEG header is damaged")
		}
		m := data[i+1]
		if m == 0xFF {
			i++
			continue
		}
		if m == 0xD8 || m == 0x01 || (m >= 0xD0 && m <= 0xD7) {
			i += 2
			continue
		}
		if m == 0xDA || m == 0xD9 {
			break
		}
		n := int(binary.BigEndian.Uint16(data[i+2:]))
		if n < 2 || i+2+n > len(data) {
			return nil, badImage("the JPEG header is truncated")
		}
		if m >= 0xC0 && m <= 0xCF && m != 0xC4 && m != 0xC8 && m != 0xCC {
			if n < 7 {
				return nil, badImage("the JPEG header is damaged")
			}
			seg := data[i+4:]
			return &imageHeader{mime: "image/jpeg", h: int(binary.BigEndian.Uint16(seg[1:])), w: int(binary.BigEndian.Uint16(seg[3:]))}, nil
		}
		i += 2 + n
	}
	return nil, badImage("the JPEG has no frame header")
}

// walkGIF walks every block to the trailer, counting frames and their
// pixels, and remembers each frame's byte range for frame-by-frame decoding.
func walkGIF(data []byte) (*imageHeader, error) {
	if len(data) < 13 {
		return nil, badImage("the GIF header is truncated")
	}
	h := &imageHeader{mime: "image/gif", w: int(binary.LittleEndian.Uint16(data[6:])), h: int(binary.LittleEndian.Uint16(data[8:]))}
	i := 13
	if data[10]&0x80 != 0 {
		i += 3 << ((data[10] & 7) + 1)
	}
	h.prefix = i
	truncated := badImage("the GIF is truncated")
	subBlocks := func() bool {
		for {
			if i >= len(data) {
				return false
			}
			n := int(data[i])
			i++
			if n == 0 {
				return true
			}
			i += n
		}
	}
	var pixels int64
	frameStart := -1
	for {
		if i >= len(data) {
			return nil, truncated
		}
		switch data[i] {
		case 0x3B:
			if len(h.frames) == 0 {
				return nil, badImage("the GIF has no image")
			}
			return h, nil
		case 0x21:
			if i+2 > len(data) {
				return nil, truncated
			}
			if data[i+1] == 0xF9 {
				frameStart = i
			}
			i += 2
			if !subBlocks() {
				return nil, truncated
			}
		case 0x2C:
			if i+10 > len(data) {
				return nil, truncated
			}
			fw, fh := int(binary.LittleEndian.Uint16(data[i+5:])), int(binary.LittleEndian.Uint16(data[i+7:]))
			if fw > maxImageSide || fh > maxImageSide {
				return nil, badImage("a GIF frame of %d×%d pixels is over the limit", fw, fh)
			}
			start := i
			if frameStart >= 0 {
				start = frameStart
			}
			packed := data[i+9]
			i += 10
			if packed&0x80 != 0 {
				i += 3 << ((packed & 7) + 1)
			}
			i++ // LZW minimum code size
			if !subBlocks() {
				return nil, truncated
			}
			h.frames = append(h.frames, [2]int{start, i})
			frameStart = -1
			pixels += int64(fw) * int64(fh)
			if len(h.frames) > maxGIFFrames || pixels > maxGIFPixels {
				return nil, badImage("the GIF animation is over the limit (500 frames, 200 megapixels over all frames)")
			}
		default:
			return nil, badImage("the GIF is damaged")
		}
	}
}

// walkWebP reads the dimensions the decoder will use. The pinned decoder
// takes the first VP8X header (a second one is an error), allocates an ALPH
// chunk from it, and stops at the first VP8 or VP8L chunk; the walk follows
// the same order, refuses a duplicate VP8X and a VP8/VP8L size that differs
// from the VP8X canvas, so no decoder allocation exceeds the size checked
// here.
func walkWebP(data []byte) (*imageHeader, error) {
	h := &imageHeader{mime: "image/webp"}
	damaged := badImage("the WebP header is damaged")
	seenVP8X := false
	i := 12
	for i+8 <= len(data) {
		tag := string(data[i : i+4])
		n := int(binary.LittleEndian.Uint32(data[i+4:]))
		body := i + 8
		if n < 0 || body+n > len(data) {
			return nil, badImage("the WebP is truncated")
		}
		c := data[body : body+n]
		var w, ht int
		switch tag {
		case "ANIM", "ANMF":
			return nil, badImage("animated WebP images are not accepted")
		case "VP8X":
			if seenVP8X {
				return nil, badImage("the WebP has more than one VP8X header")
			}
			seenVP8X = true
			if n != 10 {
				return nil, damaged
			}
			if c[0]&0x02 != 0 {
				return nil, badImage("animated WebP images are not accepted")
			}
			h.w = int(uint32(c[4])|uint32(c[5])<<8|uint32(c[6])<<16) + 1
			h.h = int(uint32(c[7])|uint32(c[8])<<8|uint32(c[9])<<16) + 1
			i = body + n + n%2
			continue
		case "VP8 ":
			if n < 10 || c[3] != 0x9d || c[4] != 0x01 || c[5] != 0x2a {
				return nil, damaged
			}
			w, ht = int(binary.LittleEndian.Uint16(c[6:])&0x3fff), int(binary.LittleEndian.Uint16(c[8:])&0x3fff)
		case "VP8L":
			if n < 5 || c[0] != 0x2f {
				return nil, damaged
			}
			bits := binary.LittleEndian.Uint32(c[1:])
			w, ht = int(bits&0x3fff)+1, int((bits>>14)&0x3fff)+1
		default:
			i = body + n + n%2
			continue
		}
		// The decoder stops at the first image chunk.
		if seenVP8X && (w != h.w || ht != h.h) {
			return nil, badImage("the WebP image size does not match its VP8X header")
		}
		h.w, h.h = w, ht
		return h, nil
	}
	return nil, badImage("the WebP has no image")
}

func decodeImage(data []byte, h *imageHeader) error {
	var got image.Rectangle
	switch h.mime {
	case "image/png":
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			return badImage("the PNG cannot be decoded: %v", err)
		}
		got = img.Bounds()
	case "image/jpeg":
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			return badImage("the JPEG cannot be decoded: %v", err)
		}
		got = img.Bounds()
	case "image/webp":
		img, err := webp.Decode(bytes.NewReader(data))
		if err != nil {
			return badImage("the WebP cannot be decoded: %v", err)
		}
		got = img.Bounds()
	case "image/gif":
		// One frame at a time: each frame becomes a GIF of its own with
		// the original screen descriptor and color table.
		for _, f := range h.frames {
			one := slices.Concat(data[:h.prefix], data[f[0]:f[1]], []byte{0x3B})
			if _, err := gif.Decode(bytes.NewReader(one)); err != nil {
				return badImage("a GIF frame cannot be decoded: %v", err)
			}
		}
		return nil
	}
	if got.Dx() != h.w || got.Dy() != h.h {
		return badImage("the image size does not match its header")
	}
	return nil
}
