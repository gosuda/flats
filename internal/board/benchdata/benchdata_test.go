package benchdata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gosuda/flats/internal/board"
	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/board/model"
)

type generated struct {
	b    *Board
	data []byte
}

var (
	cacheMu sync.Mutex
	cache   = map[string]generated{}
)

// generate returns the reference data of m and its file, generated once per
// test binary.
func generate(t *testing.T, m Mix) (*Board, []byte) {
	t.Helper()
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if g, ok := cache[m.Name]; ok {
		return g.b, g.data
	}
	b, err := Generate(m, Seed)
	if err != nil {
		t.Fatal(err)
	}
	data, err := b.Zip()
	if err != nil {
		t.Fatal(err)
	}
	cache[m.Name] = generated{b, data}
	return b, data
}

// TestMixes checks the counts, proportions and limits of the success
// criteria for both reference sets.
func TestMixes(t *testing.T) {
	for _, m := range []Mix{Mixed5k, Mixed10k} {
		t.Run(m.Name, func(t *testing.T) {
			if m.Name == Mixed10k.Name && testing.Short() {
				t.Skip("mixed 10k in -short")
			}
			b, _ := generate(t, m)
			want := map[string]int{
				model.TypeSticky: m.Stickies, model.TypeShape: m.Shapes, model.TypeConnector: m.Connectors,
				model.TypePen: m.Pens, model.TypeImage: m.Images, model.TypeFrame: m.Frames,
			}
			total := 5000
			if m.Name == Mixed10k.Name {
				total = 10000
			}
			if len(b.Elements) != total || m.Total() != total {
				t.Fatalf("%d elements, want %d", len(b.Elements), total)
			}
			got := map[string]int{}
			chars := map[string]int{}
			labeled := 0
			byID := map[string]model.Element{}
			for _, el := range b.Elements {
				got[el.Type]++
				chars[el.Type] += model.TextLen(el.Text)
				byID[el.ID] = el
				switch el.Type {
				case model.TypeConnector:
					if el.Text != "" {
						labeled++
					}
				case model.TypePen:
					if n := el.PenPoints(); n < 2 || n > m.MaxPenPoints {
						t.Errorf("pen %s has %d points", el.ID, n)
					}
				}
			}
			for typ, n := range want {
				if got[typ] != n {
					t.Errorf("%d %s elements, want %d", got[typ], typ, n)
				}
			}
			if avg := float64(chars[model.TypeSticky]) / float64(m.Stickies); avg < float64(m.StickyTextAvg)-0.5 || avg > float64(m.StickyTextAvg)+0.5 {
				t.Errorf("sticky text averages %.2f characters, want %d", avg, m.StickyTextAvg)
			}
			if avg := float64(chars[model.TypeShape]) / float64(m.Shapes); avg < float64(m.ShapeTextAvg)-0.5 || avg > float64(m.ShapeTextAvg)+0.5 {
				t.Errorf("shape text averages %.2f characters, want %d", avg, m.ShapeTextAvg)
			}
			if want := m.Connectors * 3 / 10; labeled != want {
				t.Errorf("%d labeled connectors, want %d (30%%)", labeled, want)
			}
			// Connectors attach to existing non-connector elements.
			for _, el := range b.Elements {
				for _, end := range el.Ends {
					if end.El != "" && (byID[end.El].ID == "" || byID[end.El].Type == model.TypeConnector) {
						t.Errorf("connector %s ends at %s", el.ID, end.El)
					}
				}
			}
			// Images: at most 1 MP each, the budget in total, every one
			// referenced, natural size equal to the decoded size.
			var bytesTotal int64
			refs := map[string]int{}
			for _, el := range b.Elements {
				if el.Type == model.TypeImage {
					refs[el.Asset]++
				}
			}
			for _, a := range b.Assets {
				bytesTotal += int64(len(a.Data))
				cfg, _, err := image.DecodeConfig(bytes.NewReader(a.Data))
				if err != nil {
					t.Fatalf("asset %s: %v", a.Hash, err)
				}
				if cfg.Width != a.W || cfg.Height != a.H || cfg.Width*cfg.Height > m.MaxImagePixels {
					t.Errorf("asset %s is %dx%d (recorded %dx%d), limit %d pixels", a.Hash, cfg.Width, cfg.Height, a.W, a.H, m.MaxImagePixels)
				}
				if sha256Hex(a.Data) != a.Hash || refs[a.Hash] != 1 {
					t.Errorf("asset %s: hash or reference count %d wrong", a.Hash, refs[a.Hash])
				}
			}
			if len(b.Assets) != m.Images || bytesTotal > m.MaxImageBytes {
				t.Errorf("%d images of %d bytes, want %d of at most %d", len(b.Assets), bytesTotal, m.Images, m.MaxImageBytes)
			}
			if err := engine.CheckContent(b.Elements); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %d elements, %d image bytes", m.Name, len(b.Elements), bytesTotal)
		})
	}
}

// TestDeterministic checks that the fixed seed gives the same file.
func TestDeterministic(t *testing.T) {
	_, a := generate(t, Mixed5k)
	again, err := Generate(Mixed5k, Seed)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := again.Zip()
	if !bytes.Equal(a, b) {
		t.Fatal("two runs of mixed 5k differ")
	}
	other, err := Generate(Mixed5k, Seed+1)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := other.Zip()
	if bytes.Equal(a, c) {
		t.Fatal("another seed gives the same file")
	}
}

// TestImport imports both data files into a board through the data file
// import path (TECH 9.2) and reads the board back.
func TestImport(t *testing.T) {
	for _, m := range []Mix{Mixed5k, Mixed10k} {
		t.Run(m.Name, func(t *testing.T) {
			if m.Name == Mixed10k.Name && testing.Short() {
				t.Skip("mixed 10k in -short")
			}
			b, data := generate(t, m)
			be := &backend{blobs: map[string][]byte{}}
			in, created, err := board.Open(filepath.Join(t.TempDir(), "board"), be, board.Options{Slug: "bench", Title: "Bench"})
			if err != nil || !created {
				t.Fatalf("open: created %v, %v", created, err)
			}
			defer in.Stop()
			ctx := context.Background()
			res, err := in.ImportBoard(ctx, board.ImportRequest{OpID: "benchdata-import", Mode: "replace",
				Archive: bytes.NewReader(data), Size: int64(len(data)), Label: "benchdata"}, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.IDs) != m.Total() || len(res.Assets) != m.Images {
				t.Fatalf("import reports %d ids and %d assets", len(res.IDs), len(res.Assets))
			}
			want := map[string]model.Element{}
			for _, el := range b.Elements {
				want[el.ID] = el
			}
			seen := 0
			q := board.Query{Limit: 2000}
			for {
				p, err := in.GetBoard(ctx, q)
				if err != nil {
					t.Fatal(err)
				}
				for _, raw := range p.Elements {
					var f struct {
						ID   string `json:"id"`
						Type string `json:"type"`
						Text string `json:"text"`
					}
					if err := json.Unmarshal(raw, &f); err != nil {
						t.Fatal(err)
					}
					w, ok := want[f.ID]
					if !ok || w.Type != f.Type || w.Text != f.Text {
						t.Fatalf("board holds %s %s, not in the data file as such", f.Type, f.ID)
					}
					seen++
				}
				if p.Next == "" {
					break
				}
				q = board.Query{Cursor: p.Next}
			}
			if seen != m.Total() {
				t.Fatalf("board holds %d elements, want %d", seen, m.Total())
			}
			if len(be.blobs) != m.Images {
				t.Fatalf("%d stored blobs, want %d", len(be.blobs), m.Images)
			}
		})
	}
}

// backend is the smallest board.Backend that imports a data file: blobs in
// memory, no history.
type backend struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

var errNoHistory = errors.New("benchdata test backend keeps no history")

func (b *backend) Limits() board.Limits { return board.Limits{UploadMax: 20 << 20} }

func (b *backend) MeasureOther(context.Context) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var n int64
	for _, d := range b.blobs {
		n += int64(len(d))
	}
	return n, nil
}

func (b *backend) PutBlob(_ context.Context, hash string, data []byte) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.blobs[hash]; ok {
		return false, nil
	}
	b.blobs[hash] = bytes.Clone(data)
	return true, nil
}

func (b *backend) ReadBlob(_ context.Context, hash string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if d, ok := b.blobs[hash]; ok {
		return d, nil
	}
	return nil, errors.New("no blob " + hash)
}

func (b *backend) RevisionAsset(context.Context, string) (string, bool, error) { return "", false, nil }

func (b *backend) Revisions(context.Context, int, int) (board.RevisionPage, error) {
	return board.RevisionPage{}, nil
}

func (b *backend) RevisionContent(context.Context, int) (board.RevisionInfo, engine.Content, error) {
	return board.RevisionInfo{}, engine.Content{}, errNoHistory
}

func (b *backend) HeldRevisionContent(context.Context, int) (engine.Content, func(), error) {
	return engine.Content{}, func() {}, errNoHistory
}

func (b *backend) Lease([]string) func()                              { return func() {} }
func (b *backend) Pin(context.Context, int, bool) error               { return errNoHistory }
func (b *backend) Rollback(context.Context, int, string) (int, error) { return 0, errNoHistory }

func (b *backend) Exclusive(ctx context.Context, fn func(context.Context, int64) error) error {
	other, err := b.MeasureOther(ctx)
	if err != nil {
		return err
	}
	return fn(ctx, other)
}

func (b *backend) OutboxReady() {}
