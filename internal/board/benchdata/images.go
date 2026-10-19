package benchdata

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"math/rand/v2"
)

// imageSizes are the pixel sizes of the generated images, each at most
// 1 MP.
var imageSizes = [][2]int{
	{1000, 1000}, {1152, 864}, {1280, 780}, {1024, 768}, {800, 1200},
	{1200, 800}, {960, 720}, {1100, 900}, {640, 480}, {1240, 800},
}

// images generates m.Images distinct images: four in five are photo-like
// JPEGs (gradients, waves and grain, which compress like photographs), one
// in five a screenshot-like PNG of flat panels and text lines.
func images(m Mix, seed uint64) ([]Asset, error) {
	out := make([]Asset, 0, m.Images)
	seen := map[string]bool{}
	var total int64
	for i := range m.Images {
		r := rand.New(rand.NewPCG(seed+uint64(i)*7919, 0xa5a5a5a5^uint64(i)))
		size := imageSizes[i%len(imageSizes)]
		w, h := size[0], size[1]
		if w*h > m.MaxImagePixels {
			return nil, fmt.Errorf("benchdata: image size %dx%d is over %d pixels", w, h, m.MaxImagePixels)
		}
		var buf bytes.Buffer
		ext := "jpg"
		if i%5 == 4 {
			ext = "png"
			if err := png.Encode(&buf, screenshot(r, w, h)); err != nil {
				return nil, err
			}
		} else if err := jpeg.Encode(&buf, photo(r, w, h), &jpeg.Options{Quality: 95}); err != nil {
			return nil, err
		}
		a := Asset{Hash: sha256Hex(buf.Bytes()), Ext: ext, Data: buf.Bytes(), W: w, H: h}
		if seen[a.Hash] {
			return nil, fmt.Errorf("benchdata: image %d repeats another image", i)
		}
		seen[a.Hash] = true
		total += int64(len(a.Data))
		out = append(out, a)
	}
	if total > m.MaxImageBytes {
		return nil, fmt.Errorf("benchdata: images take %d bytes, over %d", total, m.MaxImageBytes)
	}
	return out, nil
}

func photo(r *rand.Rand, w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var base, fx, fy [3]float64
	for c := range 3 {
		base[c] = 60 + r.Float64()*140
		fx[c] = 2 + r.Float64()*10
		fy[c] = 2 + r.Float64()*10
	}
	grain := 70 + r.Float64()*20
	for y := range h {
		for x := range w {
			u, v := float64(x)/float64(w), float64(y)/float64(h)
			var px [3]uint8
			for c := range 3 {
				val := base[c] + 50*math.Sin(u*fx[c]+v*3) + 40*math.Cos(v*fy[c]-u*2) + (r.Float64()-0.5)*grain
				px[c] = uint8(max(0, min(255, val)))
			}
			o := img.PixOffset(x, y)
			img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = px[0], px[1], px[2], 255
		}
	}
	return img
}

func screenshot(r *rand.Rand, w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	fill := func(x0, y0, x1, y1 int, c color.RGBA) {
		for y := max(0, y0); y < min(h, y1); y++ {
			for x := max(0, x0); x < min(w, x1); x++ {
				img.SetRGBA(x, y, c)
			}
		}
	}
	fill(0, 0, w, h, color.RGBA{248, 248, 246, 255})
	fill(0, 0, w, 56, color.RGBA{uint8(30 + r.IntN(60)), uint8(60 + r.IntN(80)), uint8(120 + r.IntN(100)), 255})
	fill(0, 56, 220, h, color.RGBA{236, 236, 232, 255})
	for y := 90; y+14 < h; y += 30 + r.IntN(10) {
		fill(250, y, 250+120+r.IntN(w-420), y+12, color.RGBA{90, 90, 96, 255})
		if r.IntN(6) == 0 {
			fill(250, y+20, 250+80+r.IntN(300), y+120, color.RGBA{uint8(r.IntN(255)), uint8(r.IntN(255)), uint8(r.IntN(255)), 255})
			y += 110
		}
	}
	return img
}
