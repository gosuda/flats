// Package benchdata generates the reference board data of the board success
// criteria (docs/spec/board.md, "Success criteria"): "mixed 5k" and "mixed
// 10k". The data comes from a fixed seed, so every run of the same toolchain
// writes the same data file. The file has the data file format of
// docs/spec/board-TECH.md section 9 and imports with
// `flats board import <slug> <file> --replace`.
//
// The board is a grid of tiles. Half of the tiles lie inside a frame and half
// are open canvas. Each tile holds a grid of stickies, two columns of shapes
// joined into flowcharts by connectors, a few connectors between
// neighbouring stickies, pen strokes over the stickies and a strip of images.
package benchdata

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"time"

	"github.com/gosuda/flats/internal/board/model"
)

// Mix is a reference data set: element counts per type and the text, pen
// and image bounds of the success criteria.
type Mix struct {
	Name       string
	Stickies   int
	Shapes     int
	Connectors int
	Pens       int
	Images     int
	Frames     int

	StickyTextAvg  int     // characters on average
	ShapeTextAvg   int     // characters on average
	LabeledShare   float64 // share of connectors with a label
	MaxPenPoints   int     // points per stroke
	MaxImagePixels int     // pixels per image
	MaxImageBytes  int64   // all image files together
}

// Total is the number of elements of m.
func (m Mix) Total() int {
	return m.Stickies + m.Shapes + m.Connectors + m.Pens + m.Images + m.Frames
}

// Mixed5k is "mixed 5k": 2,980 stickies (40 characters on average), 1,000
// shapes (15 characters on average), 700 connectors (30% labeled), 250 pen
// strokes (at most 200 points each), 50 images (each at most 1 MP, at most
// 30 MB together) and 20 frames.
var Mixed5k = Mix{
	Name:     "mixed 5k",
	Stickies: 2980, Shapes: 1000, Connectors: 700, Pens: 250, Images: 50, Frames: 20,
	StickyTextAvg: 40, ShapeTextAvg: 15, LabeledShare: 0.3,
	MaxPenPoints: 200, MaxImagePixels: 1_000_000, MaxImageBytes: 30_000_000,
}

// Mixed10k is "mixed 10k": twice mixed 5k in the same proportions.
var Mixed10k = Mixed5k.Scaled("mixed 10k", 2)

// Scaled returns m with every count and the image byte budget multiplied by k.
func (m Mix) Scaled(name string, k int) Mix {
	m.Name = name
	m.Stickies *= k
	m.Shapes *= k
	m.Connectors *= k
	m.Pens *= k
	m.Images *= k
	m.Frames *= k
	m.MaxImageBytes *= int64(k)
	return m
}

// ByName returns the mix named "5k" or "10k".
func ByName(name string) (Mix, error) {
	switch name {
	case "5k", "mixed5k", "mixed 5k":
		return Mixed5k, nil
	case "10k", "mixed10k", "mixed 10k":
		return Mixed10k, nil
	}
	return Mix{}, fmt.Errorf("unknown mix %q (5k or 10k)", name)
}

// Seed is the fixed seed of the reference data.
const Seed uint64 = 20261019

// Asset is one image file of the board.
type Asset struct {
	Hash string // sha256 hex of Data
	Ext  string // png or jpg
	Data []byte
	W, H int
}

// Board is generated board data.
type Board struct {
	Mix      Mix
	Title    string
	Elements []model.Element // sorted by id
	Assets   []Asset         // sorted by hash
}

// Tile geometry in canvas pixels.
const (
	tileW      = 3600
	tileH      = 2700
	tileGap    = 300
	stickySize = 200
	stickyStep = 220
	stickyCols = 10
	shapeW     = 180
	shapeH     = 100
	shapeStep  = 165
	imageH     = 280
	createdAt  = 1_760_000_000
)

var authors = []string{
	"ts:alice@example.com", "ts:bora@example.com", "ts:chen@example.com",
	"browser:4f1c2a9e0b7d", "agent:benchdata",
}

// counts splits n items over k slots as evenly as possible.
func counts(n, k int) []int {
	out := make([]int, k)
	for i := range out {
		out[i] = (i+1)*n/k - i*n/k
	}
	return out
}

type gen struct {
	m    Mix
	rng  *rand.Rand
	ids  map[string]bool
	els  []model.Element
	text *textGen
}

func (g *gen) id() string {
	const chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	for {
		b := []byte("e_")
		for range 10 {
			b = append(b, chars[g.rng.IntN(len(chars))])
		}
		if id := string(b); !g.ids[id] {
			g.ids[id] = true
			return id
		}
	}
}

func (g *gen) author(i int) (string, int64) {
	return authors[i%len(authors)], int64(createdAt + i)
}

// Generate builds the board of m from seed.
func Generate(m Mix, seed uint64) (*Board, error) {
	if m.Frames < 1 || m.Total() > model.MaxElements {
		return nil, fmt.Errorf("benchdata: a mix needs at least one frame and at most %d elements", model.MaxElements)
	}
	g := &gen{m: m, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), ids: map[string]bool{}}
	g.text = newTextGen(rand.New(rand.NewPCG(seed+1, seed^0x5851f42d4c957f2d)))
	tiles := 2 * m.Frames
	cols := int(math.Ceil(math.Sqrt(float64(tiles) * 1.6)))
	nSticky := counts(m.Stickies, tiles)
	nShape := counts(m.Shapes, tiles)
	nConn := counts(m.Connectors, tiles)
	nPen := counts(m.Pens, tiles)
	nImage := counts(m.Images, tiles)

	stickyLens := g.text.lengths(m.Stickies, m.StickyTextAvg, 8, model.MaxText)
	shapeLens := g.text.lengths(m.Shapes, m.ShapeTextAvg, 3, model.MaxText)
	labeled := map[int]bool{}
	for _, i := range g.rng.Perm(m.Connectors)[:int(math.Round(float64(m.Connectors)*m.LabeledShare))] {
		labeled[i] = true
	}
	assets, err := images(m, seed)
	if err != nil {
		return nil, err
	}

	// Layers bottom to top: frames, images, stickies, shapes, pens, connectors.
	var frames, imgs, stickies, shapes, pens, conns []int
	add := func(layer *[]int, el model.Element) model.Element {
		el.ID = g.id()
		el.By, el.At = g.author(len(g.els))
		if el.Style == nil {
			el.Style = model.DefaultStyle(el.Type)
		}
		*layer = append(*layer, len(g.els))
		g.els = append(g.els, el)
		return el
	}
	var si, hi, ci, ii int // running indexes per type
	for t := range tiles {
		tx := float64((t % cols) * (tileW + tileGap))
		ty := float64((t / cols) * (tileH + tileGap))
		frame := ""
		if t%2 == 0 {
			f := add(&frames, model.Element{Type: model.TypeFrame,
				Geom:  model.Geom{X: tx, Y: ty, W: tileW, H: tileH},
				Text:  fmt.Sprintf("%d. %s", t/2+1, g.text.exact(18+g.rng.IntN(14))),
				Style: model.Style{"fill": []string{"white", "white", "gray", "none"}[t/2%4]}})
			frame = f.ID
		}
		// Stickies in a grid.
		tileStickies := make([]string, nSticky[t])
		for k := range nSticky[t] {
			col, row := k%stickyCols, k/stickyCols
			rot := 0.0
			if g.rng.IntN(12) == 0 {
				rot = []float64{2, 358, 3, 357}[g.rng.IntN(4)]
			}
			el := add(&stickies, model.Element{Type: model.TypeSticky, Frame: frame,
				Geom: model.Geom{X: tx + 100 + float64(col*stickyStep) + jitter(g.rng, 6),
					Y: ty + 120 + float64(row*stickyStep) + jitter(g.rng, 6), W: stickySize, H: stickySize, Rot: rot},
				Text:  g.text.exact(stickyLens[si]),
				Style: model.Style{"color": model.StickyColors[g.rng.IntN(len(model.StickyColors))], "fontSize": "auto"}})
			tileStickies[k] = el.ID
			si++
		}
		// Shapes in two flowchart columns.
		perCol := (nShape[t] + 1) / 2
		tileShapes := make([][]string, 2)
		for k := range nShape[t] {
			col, row := k/perCol, k%perCol
			st := model.DefaultStyle(model.TypeShape)
			st["shape"] = []string{"rect", "round", "ellipse", "diamond"}[g.rng.IntN(4)]
			st["fill"] = []string{"white", "white", "blue", "green", "none"}[g.rng.IntN(5)]
			if g.rng.IntN(6) == 0 {
				st["dash"] = "dashed"
			}
			el := add(&shapes, model.Element{Type: model.TypeShape, Frame: frame,
				Geom:  model.Geom{X: tx + 2450 + float64(col*500), Y: ty + 120 + float64(row*shapeStep), W: shapeW, H: shapeH},
				Text:  g.text.exact(shapeLens[hi]),
				Style: st})
			tileShapes[col] = append(tileShapes[col], el.ID)
			hi++
		}
		// Connectors: flowchart edges down each shape column, and every
		// fourth one between horizontally neighbouring stickies. Every
		// tenth leaves its second end free.
		var edges [][2]string
		var edgeAt [][2][2]float64
		for _, colIDs := range tileShapes {
			for k := 0; k+1 < len(colIDs); k++ {
				edges = append(edges, [2]string{colIDs[k], colIDs[k+1]})
				edgeAt = append(edgeAt, [2][2]float64{{0.5, 1}, {0.5, 0}})
			}
		}
		var side [][2]string
		for k := 0; k+1 < len(tileStickies); k++ {
			if k%stickyCols != stickyCols-1 {
				side = append(side, [2]string{tileStickies[k], tileStickies[k+1]})
			}
		}
		ei, sj := 0, 0
		for k := range nConn[t] {
			var pair [2]string
			at := [2][2]float64{{1, 0.5}, {0, 0.5}}
			switch {
			case (k%4 == 3 || ei >= len(edges)) && sj < len(side):
				pair = side[(sj*7)%len(side)]
				sj++
			case ei < len(edges):
				pair, at = edges[ei], edgeAt[ei]
				ei++
			default:
				return nil, fmt.Errorf("benchdata: tile %d has too few elements for %d connectors", t, nConn[t])
			}
			end1 := model.End{El: pair[1], At: at[1]}
			if ci%10 == 9 {
				src := g.els[indexOf(g.els, pair[0])].Geom
				end1 = model.End{Pt: [2]float64{src.X + src.W + 160, src.Y - 60}}
			}
			st := model.DefaultStyle(model.TypeConnector)
			if g.rng.IntN(5) < 2 {
				st["route"] = "elbow"
			}
			if g.rng.IntN(8) == 0 {
				st["dash"] = "dashed"
			}
			text := ""
			if labeled[ci] {
				text = g.text.exact(6 + g.rng.IntN(14))
			}
			add(&conns, model.Element{Type: model.TypeConnector, Frame: frame,
				Ends: [2]model.End{{El: pair[0], At: at[0]}, end1}, Text: text, Style: st})
			ci++
		}
		// Pen strokes over the stickies.
		for range nPen[t] {
			w := 150 + float64(g.rng.IntN(450))
			h := 60 + float64(g.rng.IntN(240))
			st := model.DefaultStyle(model.TypePen)
			st["stroke"] = []string{"ink", "red", "blue", "green", "highlight"}[g.rng.IntN(5)]
			if g.rng.IntN(4) == 0 {
				st["width"] = "marker"
			}
			add(&pens, model.Element{Type: model.TypePen, Frame: frame,
				Geom: model.Geom{X: tx + 100 + float64(g.rng.IntN(2000)), Y: ty + 120 + float64(g.rng.IntN(2000)), W: w, H: h},
				Pts:  stroke(g.rng, 20+g.rng.IntN(m.MaxPenPoints-19)), Style: st})
		}
		// Images in a strip along the bottom.
		x := tx + 100
		for range nImage[t] {
			a := assets[ii]
			w := math.Round(float64(a.W) * imageH / float64(a.H))
			add(&imgs, model.Element{Type: model.TypeImage, Frame: frame,
				Geom:  model.Geom{X: x, Y: ty + tileH - imageH - 40, W: w, H: imageH},
				Asset: a.Hash, Alt: g.text.exact(20 + g.rng.IntN(40)), Natural: [2]int{a.W, a.H}})
			x += w + 40
			ii++
		}
	}
	order := append(append(append(append(append(frames, imgs...), stickies...), shapes...), pens...), conns...)
	keys := model.Rekey(len(order))
	for i, idx := range order {
		g.els[idx].Z = keys[i]
	}
	els := g.els
	sort.Slice(els, func(i, j int) bool { return els[i].ID < els[j].ID })
	sort.Slice(assets, func(i, j int) bool { return assets[i].Hash < assets[j].Hash })
	return &Board{Mix: m, Title: "Benchmark " + m.Name, Elements: els, Assets: assets}, nil
}

func indexOf(els []model.Element, id string) int {
	for i := len(els) - 1; i >= 0; i-- {
		if els[i].ID == id {
			return i
		}
	}
	panic("benchdata: missing element " + id)
}

func jitter(r *rand.Rand, n int) float64 { return float64(r.IntN(2*n+1) - n) }

// stroke is a smooth random walk of n points, normalized to 0..PenScale.
func stroke(r *rand.Rand, n int) []int {
	xs, ys := make([]float64, n), make([]float64, n)
	x, y, dir := 0.0, 0.0, r.Float64()*2*math.Pi
	for i := range n {
		dir += (r.Float64() - 0.5) * 0.8
		x += math.Cos(dir)
		y += math.Sin(dir)
		xs[i], ys[i] = x, y
	}
	norm := func(v []float64) []int {
		lo, hi := v[0], v[0]
		for _, f := range v {
			lo, hi = math.Min(lo, f), math.Max(hi, f)
		}
		out := make([]int, len(v))
		for i, f := range v {
			if hi > lo {
				out[i] = int(math.Round((f - lo) / (hi - lo) * model.PenScale))
			}
		}
		return out
	}
	nx, ny := norm(xs), norm(ys)
	pts := make([]int, 0, 2*n)
	for i := range n {
		pts = append(pts, nx[i], ny[i])
	}
	return pts
}

// zipTime is the fixed modification time of every entry, so the file bytes
// depend on the data alone.
var zipTime = time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)

// WriteZip writes b as a board data file (TECH 9.1): board.json,
// elements.json (content fields sorted by id) and assets/<sha256>.<ext>.
func (b *Board) WriteZip(w io.Writer) error {
	zw := zip.NewWriter(w)
	put := func(name string, data []byte, method uint16) error {
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method, Modified: zipTime})
		if err != nil {
			return err
		}
		_, err = fw.Write(data)
		return err
	}
	if err := put("board.json", model.Canonical(map[string]any{"format": 1, "title": b.Title, "grid": true}), zip.Deflate); err != nil {
		return err
	}
	var els bytes.Buffer
	els.WriteByte('[')
	for i, e := range b.Elements {
		if i > 0 {
			els.WriteByte(',')
		}
		els.Write(e.CanonicalJSON())
	}
	els.WriteByte(']')
	if err := put("elements.json", els.Bytes(), zip.Deflate); err != nil {
		return err
	}
	for _, a := range b.Assets {
		if err := put("assets/"+a.Hash+"."+a.Ext, a.Data, zip.Store); err != nil {
			return err
		}
	}
	return zw.Close()
}

// Zip returns the data file of b.
func (b *Board) Zip() ([]byte, error) {
	var buf bytes.Buffer
	if err := b.WriteZip(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// WriteFile generates m from seed and writes its data file to path.
func WriteFile(path string, m Mix, seed uint64) (*Board, error) {
	b, err := Generate(m, seed)
	if err != nil {
		return nil, err
	}
	data, err := b.Zip()
	if err != nil {
		return nil, err
	}
	return b, os.WriteFile(path, data, 0o644)
}
