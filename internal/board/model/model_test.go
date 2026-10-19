package model

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeElement(t *testing.T) {
	el, present, err := DecodeElement([]byte(`{"id":"e_7Hk2pQ9xLm","type":"sticky","x":120.04,"y":80,"w":200,"h":200,"z":"a0V","text":"Onboarding is too long","style":{"color":"blue"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !present.HasGeom() || present["rot"] {
		t.Fatalf("present = %v", present)
	}
	el.Normalize()
	if err := el.Validate(); err != nil {
		t.Fatal(err)
	}
	if el.Geom.X != 120 || el.Style["color"] != "blue" || el.Style["fontSize"] != "auto" {
		t.Fatalf("element = %+v", el)
	}
	for name, raw := range map[string]string{
		"unknown field":      `{"id":"e_7Hk2pQ9xLm","type":"sticky","color":"red"}`,
		"server field":       `{"id":"e_7Hk2pQ9xLm","type":"sticky","inc":4}`,
		"unknown type":       `{"id":"e_7Hk2pQ9xLm","type":"video"}`,
		"unknown style key":  `{"id":"e_7Hk2pQ9xLm","type":"sticky","style":{"stroke":"ink"}}`,
		"bad style value":    `{"id":"e_7Hk2pQ9xLm","type":"sticky","style":{"color":"ink"}}`,
		"fractional font":    `{"id":"e_7Hk2pQ9xLm","type":"text","style":{"fontSize":12.5}}`,
		"pen text":           `{"id":"e_7Hk2pQ9xLm","type":"pen","text":"x"}`,
		"connector geometry": `{"id":"e_7Hk2pQ9xLm","type":"connector","x":1}`,
		"end with at and pt": `{"id":"e_7Hk2pQ9xLm","type":"connector","ends":[{"el":"e_aaaaaaaaaa","pt":[1,2]},{"pt":[0,0]}]}`,
	} {
		if _, _, err := DecodeElement([]byte(raw)); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	for name, el := range map[string]Element{
		"bad id":         {ID: "e_short", Type: TypeSticky, Geom: Geom{W: 1, H: 1}, Style: DefaultStyle(TypeSticky)},
		"too far":        {ID: "e_aaaaaaaaaa", Type: TypeSticky, Geom: Geom{X: 2e7, W: 1, H: 1}, Style: DefaultStyle(TypeSticky)},
		"too small":      {ID: "e_aaaaaaaaaa", Type: TypeSticky, Geom: Geom{W: 0.5, H: 1}, Style: DefaultStyle(TypeSticky)},
		"rotation":       {ID: "e_aaaaaaaaaa", Type: TypeSticky, Geom: Geom{W: 1, H: 1, Rot: 361}, Style: DefaultStyle(TypeSticky)},
		"nested frame":   {ID: "e_aaaaaaaaaa", Type: TypeFrame, Frame: "e_bbbbbbbbbb", Geom: Geom{W: 1, H: 1}, Style: DefaultStyle(TypeFrame)},
		"pen odd points": {ID: "e_aaaaaaaaaa", Type: TypePen, Pts: []int{1, 2, 3}, Geom: Geom{W: 1, H: 1}, Style: DefaultStyle(TypePen)},
		"pen range":      {ID: "e_aaaaaaaaaa", Type: TypePen, Pts: []int{1, 20000}, Geom: Geom{W: 1, H: 1}, Style: DefaultStyle(TypePen)},
		"end range":      {ID: "e_aaaaaaaaaa", Type: TypeConnector, Ends: [2]End{{El: "e_bbbbbbbbbb", At: [2]float64{1.5, 0}}, {}}, Style: DefaultStyle(TypeConnector)},
		"self connector": {ID: "e_aaaaaaaaaa", Type: TypeConnector, Ends: [2]End{{El: "e_aaaaaaaaaa"}, {}}, Style: DefaultStyle(TypeConnector)},
		"image asset":    {ID: "e_aaaaaaaaaa", Type: TypeImage, Asset: "x", Natural: [2]int{1, 1}, Geom: Geom{W: 1, H: 1}, Style: Style{}},
		"z with zero":    {ID: "e_aaaaaaaaaa", Type: TypeSticky, Z: "a0", Geom: Geom{W: 1, H: 1}, Style: DefaultStyle(TypeSticky)},
		"missing style":  {ID: "e_aaaaaaaaaa", Type: TypeSticky, Geom: Geom{W: 1, H: 1}},
	} {
		if err := el.Validate(); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestDecodeBundle(t *testing.T) {
	v, err := DecodeBundle(TypeSticky, BundleGeom, json.RawMessage(`{"x":1.26,"y":-0.04,"w":10,"h":10,"rot":0}`))
	if err != nil || v.(Geom) != (Geom{X: 1.3, Y: 0, W: 10, H: 10}) {
		t.Fatalf("geom = %v %v", v, err)
	}
	for name, c := range map[string]struct{ typ, g, raw string }{
		"partial geom":   {TypeSticky, BundleGeom, `{"x":1}`},
		"connector geom": {TypeConnector, BundleGeom, `{"x":1,"y":1,"w":1,"h":1,"rot":0}`},
		"bad end":        {TypeConnector, BundleEnd0, `{"el":"nope","at":[0,0]}`},
		"style key":      {TypeShape, "style.color", `"red"`},
		"z":              {TypeShape, BundleZ, `"a-b"`},
		"long title":     {TypeBoard, BundleTitle, `"` + strings.Repeat("x", MaxTitle+1) + `"`},
	} {
		if _, err := DecodeBundle(c.typ, c.g, json.RawMessage(c.raw)); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if Settable(TypeSticky, BundleFrame) || Settable(TypeSticky, BundleGroup) || !Settable(TypeSticky, "style.color") || Settable(TypePen, BundleText) {
		t.Fatal("settable bundles are wrong")
	}
	if got := BundleJSON(BundleFrame, ""); string(got) != "null" {
		t.Fatalf("empty frame = %s", got)
	}
}

func TestCanonicalJSON(t *testing.T) {
	a, _, err := DecodeElement([]byte(`{"type":"shape","id":"e_aaaaaaaaaa","x":1,"y":2,"w":3,"h":4,"text":"<b>","style":{"fill":"none","shape":"round"}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := DecodeElement([]byte(`{"style":{"shape":"round","fill":"none"},"h":4,"w":3,"y":2,"x":1,"text":"<b>","id":"e_aaaaaaaaaa","type":"shape"}`))
	if string(a.CanonicalJSON()) != string(b.CanonicalJSON()) {
		t.Fatalf("canonical differs:\n%s\n%s", a.CanonicalJSON(), b.CanonicalJSON())
	}
	want := `{"at":0,"by":"","h":4,"id":"e_aaaaaaaaaa","locked":false,"rot":0,"style":{"dash":"solid","fill":"none","fontSize":14,"shape":"round","stroke":"ink","strokeWidth":2},"text":"<b>","type":"shape","w":3,"x":1,"y":2,"z":""}`
	if got := string(a.CanonicalJSON()); got != want {
		t.Fatalf("canonical =\n%s\nwant\n%s", got, want)
	}
	var back Element
	if err := json.Unmarshal(a.CanonicalJSON(), &back); err != nil || !Equal(a, back) {
		t.Fatalf("round trip: %v", err)
	}
	bucket := BucketJSON([]Element{b, a})
	if !strings.HasPrefix(string(bucket), `{"e_aaaaaaaaaa":{"at":0`) {
		t.Fatalf("bucket = %s", bucket)
	}
}

func TestBucketOf(t *testing.T) {
	for _, id := range []string{"e_7Hk2pQ9xLm", "e_aaaaaaaaaa", "e_0000000000"} {
		h := sha256.Sum256([]byte(id))
		if got := BucketOf(id); got != int(h[0]) {
			t.Fatalf("BucketOf(%s) = %d, want %d", id, got, h[0])
		}
	}
	if BucketPath(0x0f) != "elements/0f.json" || BucketPath(BoardBucket) != "board.json" || BucketOf(BoardID) != BoardBucket {
		t.Fatal("bucket paths are wrong")
	}
}

func TestPublicProjection(t *testing.T) {
	el := Element{ID: "e_aaaaaaaaaa", Type: TypeSticky, Geom: Geom{W: 1, H: 1}, Z: "V", Text: "hi", Style: DefaultStyle(TypeSticky), By: "ts:alice@example.com", At: 1760846400}
	priv := PrivateElement{Element: el, Inc: 3, V: map[string]int64{"geom": 3}}
	d := Delta{
		Seq: 9, From: "s1:4", By: &By{Kind: "person", Label: "Alice"}, Reset: &Reset{Kind: "rollback", Of: 2},
		Changes: []Change{
			{ID: el.ID, Inc: 3, Add: &priv},
			{ID: "e_bbbbbbbbbb", Inc: 2, Set: map[string]SetVal{"text": {Val: json.RawMessage(`"x"`), V: 9}}, Overwrote: 4},
			{ID: "e_cccccccccc", Del: 9},
			{ID: "e_dddddddddd", Inc: 9, Revive: &priv},
		},
	}
	raw, _ := json.Marshal(PublicDelta(d))
	for _, leak := range []string{"alice", "Alice", "s1:4", `"by"`, `"at"`, `"inc"`, `"v"`, "overwrote", "reset", "rollback"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("public delta leaks %q: %s", leak, raw)
		}
	}
	want := `{"changes":[{"add":{"h":1,"id":"e_aaaaaaaaaa","locked":false,"rot":0,"style":{"color":"yellow","fontSize":"auto"},"text":"hi","type":"sticky","w":1,"x":0,"y":0,"z":"V"},"id":"e_aaaaaaaaaa"},{"id":"e_bbbbbbbbbb","set":{"text":"x"}},{"del":true,"id":"e_cccccccccc"},{"id":"e_dddddddddd","revive":{"h":1,"id":"e_aaaaaaaaaa","locked":false,"rot":0,"style":{"color":"yellow","fontSize":"auto"},"text":"hi","type":"sticky","w":1,"x":0,"y":0,"z":"V"}}],"seq":9}`
	if string(raw) != want {
		t.Fatalf("public delta =\n%s\nwant\n%s", raw, want)
	}
	private, _ := json.Marshal(d)
	for _, field := range []string{`"from":"s1:4"`, `"overwrote":4`, `"set":{"text":["x",9]}`, `"del":9`, `"inc":3`, `"by":"ts:alice@example.com"`} {
		if !strings.Contains(string(private), field) {
			t.Fatalf("private delta lacks %s: %s", field, private)
		}
	}
	board := PublicElement(Element{ID: BoardID, Type: TypeBoard, Text: "Plan", Grid: true})
	if board["title"] != "Plan" || board["id"] != BoardID || board["by"] != nil {
		t.Fatalf("public board = %v", board)
	}
}

func TestKeys(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	keys := []string{""}
	for range 2000 {
		i := rng.IntN(len(keys))
		lo := keys[i]
		hi := ""
		if i+1 < len(keys) {
			hi = keys[i+1]
		}
		k, err := KeyBetween(lo, hi)
		if err != nil {
			t.Fatal(err)
		}
		if k <= lo || (hi != "" && k >= hi) || !wellFormedKey(k) {
			t.Fatalf("KeyBetween(%q, %q) = %q", lo, hi, k)
		}
		keys = append(keys[:i+1], append([]string{k}, keys[i+1:]...)...)
	}
	if _, err := KeyBetween("b", "a"); !errors.Is(err, ErrKeyOrder) {
		t.Fatalf("out of order: %v", err)
	}
	for _, n := range []int{0, 1, 61, 62, 10000} {
		ks := Rekey(n)
		for i, k := range ks {
			if !ValidKey(k) || (i > 0 && ks[i-1] >= k) {
				t.Fatalf("Rekey(%d)[%d] = %q", n, i, k)
			}
		}
	}
	if k, _ := KeyBetween("", ""); k != "V" {
		t.Fatalf("first key = %q", k)
	}
}

var update = flag.Bool("update", false, "rewrite testdata/geometry.json")

type geomVector struct {
	El   Geom       `json:"el"`
	At   [2]float64 `json:"at"`
	Want [2]float64 `json:"want"`
}

// vectorInputs are the shared EndpointWorld cases. The JS client reads the
// same file.
var vectorInputs = []struct {
	el Geom
	at [2]float64
}{
	{Geom{0, 0, 100, 50, 0}, [2]float64{1, 0.5}},
	{Geom{0, 0, 100, 50, 0}, [2]float64{0, 0}},
	{Geom{0, 0, 100, 50, 0}, [2]float64{0.5, 0.5}},
	{Geom{10, 20, 100, 50, 90}, [2]float64{1, 0.5}},
	{Geom{10, 20, 100, 50, 90}, [2]float64{0, 0}},
	{Geom{10, 20, 100, 50, 180}, [2]float64{1, 0.5}},
	{Geom{10, 20, 100, 50, 180}, [2]float64{0.25, 0.75}},
	{Geom{10, 20, 100, 50, 270}, [2]float64{0.5, 0}},
	{Geom{-300, -120.5, 240, 160, 33.5}, [2]float64{1, 0.5}},
	{Geom{-300, -120.5, 240, 160, 33.5}, [2]float64{0, 1}},
	{Geom{-300, -120.5, 240, 160, 33.5}, [2]float64{0.3, 0.6}},
	{Geom{500, 400, 200, 200, 45}, [2]float64{1, 1}},
	{Geom{500, 400, 200, 200, 45}, [2]float64{0.5, 0}},
	{Geom{1234.5, -987.6, 321.7, 88.8, 12.3}, [2]float64{0.1, 0.9}},
	{Geom{0, 0, 1, 1, 359.9}, [2]float64{1, 1}},
	{Geom{-10000000, 10000000, 100000, 100000, 0}, [2]float64{1, 0}},
	{Geom{40, 60, 160, 100, 120}, [2]float64{0.333, 0.667}},
	{Geom{40, 60, 160, 100, 210}, [2]float64{0.75, 0.2}},
	{Geom{40, 60, 160, 100, 300}, [2]float64{0.05, 0.95}},
	{Geom{7.7, 8.8, 9.9, 11.1, 77.7}, [2]float64{0.6, 0.4}},
	{Geom{100, 100, 300, 100, 360}, [2]float64{1, 0.5}},
	{Geom{0, 0, 200, 200, 135}, [2]float64{0.5, 1}},
}

type sideVector struct {
	El     Geom       `json:"el"`
	Toward [2]float64 `json:"toward"`
	Want   [2]float64 `json:"want"`
}

// sideInputs are the shared DefaultAt cases: each side, rotations that
// turn the sides, a box much wider than tall and the zero direction.
var sideInputs = []struct {
	el     Geom
	toward [2]float64
}{
	{Geom{0, 0, 200, 200, 0}, [2]float64{500, 120}},
	{Geom{0, 0, 200, 200, 0}, [2]float64{-300, 40}},
	{Geom{0, 0, 200, 200, 0}, [2]float64{90, 700}},
	{Geom{0, 0, 200, 200, 0}, [2]float64{130, -400}},
	{Geom{0, 0, 200, 200, 0}, [2]float64{100, 100}},
	{Geom{0, 0, 400, 50, 0}, [2]float64{150, 300}},
	{Geom{0, 0, 400, 50, 0}, [2]float64{700, 300}},
	{Geom{10, 20, 100, 50, 90}, [2]float64{300, 60}},
	{Geom{10, 20, 100, 50, 90}, [2]float64{60, 400}},
	{Geom{10, 20, 100, 50, 180}, [2]float64{300, 40}},
	{Geom{10, 20, 100, 50, 270}, [2]float64{60, -300}},
	{Geom{500, 400, 200, 200, 45}, [2]float64{1000, 520}},
	{Geom{500, 400, 200, 200, 45}, [2]float64{450, 1000}},
	{Geom{-300, -120.5, 240, 160, 33.5}, [2]float64{-500, 300}},
	{Geom{-300, -120.5, 240, 160, 33.5}, [2]float64{200, -900}},
	{Geom{1234.5, -987.6, 321.7, 88.8, 12.3}, [2]float64{1300, -2000}},
	{Geom{40, 60, 160, 100, 300}, [2]float64{-400, 100}},
}

func TestGeometryVectors(t *testing.T) {
	path := filepath.Join("testdata", "geometry.json")
	if *update {
		lines := make([]string, len(vectorInputs))
		for i, in := range vectorInputs {
			raw, _ := json.Marshal(geomVector{in.el, in.at, EndpointWorld(in.el, in.at)})
			lines[i] = "  " + string(raw)
		}
		sides := make([]string, len(sideInputs))
		for i, in := range sideInputs {
			raw, _ := json.Marshal(sideVector{in.el, in.toward, DefaultAt(in.el, in.toward)})
			sides[i] = "  " + string(raw)
		}
		out := "{\"endpoints\":[\n" + strings.Join(lines, ",\n") + "\n],\n\"default_at\":[\n" + strings.Join(sides, ",\n") + "\n]}\n"
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Endpoints []geomVector `json:"endpoints"`
		DefaultAt []sideVector `json:"default_at"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Endpoints) < 20 {
		t.Fatalf("only %d vectors", len(file.Endpoints))
	}
	for i, v := range file.Endpoints {
		if got := EndpointWorld(v.El, v.At); got != v.Want {
			t.Errorf("vector %d: EndpointWorld(%v, %v) = %v, want %v", i, v.El, v.At, got, v.Want)
		}
		// A vector whose unrounded value sits on a rounding boundary would
		// let float differences between Go and JS flip the result.
		cx, cy := v.El.X+v.El.W/2, v.El.Y+v.El.H/2
		dx, dy := v.El.X+v.At[0]*v.El.W-cx, v.El.Y+v.At[1]*v.El.H-cy
		sin, cos := sinCos(v.El.Rot)
		for _, raw := range []float64{cx + dx*cos - dy*sin, cy + dx*sin + dy*cos} {
			if frac := math.Abs(math.Mod(raw*10, 1)); math.Abs(frac-0.5) < 1e-6 {
				t.Errorf("vector %d lies on a rounding boundary: %v", i, raw)
			}
		}
	}
	if got := EndpointWorld(Geom{0, 0, 100, 50, 0}, [2]float64{1, 0.5}); got != [2]float64{100, 25} {
		t.Fatalf("basic vector = %v", got)
	}
	if len(file.DefaultAt) < len(sideInputs) {
		t.Fatalf("only %d default_at vectors", len(file.DefaultAt))
	}
	for i, v := range file.DefaultAt {
		if got := DefaultAt(v.El, v.Toward); got != v.Want {
			t.Errorf("default_at vector %d: DefaultAt(%v, %v) = %v, want %v", i, v.El, v.Toward, got, v.Want)
		}
		// A direction on a box diagonal would let float differences between
		// Go and JS pick different sides.
		dx, dy := v.Toward[0]-(v.El.X+v.El.W/2), v.Toward[1]-(v.El.Y+v.El.H/2)
		sin, cos := sinCos(v.El.Rot)
		lx, ly := dx*cos+dy*sin, dy*cos-dx*sin
		if d := math.Abs(lx)*v.El.H - math.Abs(ly)*v.El.W; math.Abs(d) < 1e-6*(v.El.W*v.El.H) && (lx != 0 || ly != 0) {
			t.Errorf("default_at vector %d lies on a diagonal", i)
		}
	}
	if got := DefaultAt(Geom{0, 0, 200, 100, 0}, [2]float64{1000, 60}); got != [2]float64{1, 0.5} {
		t.Fatalf("right side = %v", got)
	}
}

func TestConnectorBounds(t *testing.T) {
	if got := ConnectorBounds([2]float64{100, 25}, [2]float64{-20, 70}); got != (Geom{X: -20, Y: 25, W: 120, H: 45}) {
		t.Fatalf("bounds = %+v", got)
	}
}

func TestRound1(t *testing.T) {
	for in, want := range map[float64]float64{1.25: 1.3, -1.25: -1.3, -0.04: 0, 0.05: 0.1, 2.449: 2.4} {
		if got := Round1(in); got != want || math.Signbit(got) != math.Signbit(want) {
			t.Errorf("Round1(%v) = %v, want %v", in, got, want)
		}
	}
}

// TestSchemaInvalidValues checks that malformed values are refused rather
// than turned into valid edits: short or long coordinate pairs, and null
// where a value is required.
func TestSchemaInvalidValues(t *testing.T) {
	for name, c := range map[string]struct{ typ, g, raw string }{
		"short pt":      {TypeConnector, BundleEnd0, `{"pt":[10]}`},
		"long pt":       {TypeConnector, BundleEnd1, `{"pt":[10,20,30]}`},
		"short at":      {TypeConnector, BundleEnd0, `{"el":"e_aaaaaaaaaa","at":[0.5]}`},
		"long at":       {TypeConnector, BundleEnd0, `{"el":"e_aaaaaaaaaa","at":[0.5,0.5,0.5]}`},
		"null end":      {TypeConnector, BundleEnd0, `null`},
		"null text":     {TypeSticky, BundleText, `null`},
		"null alt":      {TypeImage, BundleAlt, `null`},
		"null locked":   {TypeSticky, BundleLocked, `null`},
		"null z":        {TypeSticky, BundleZ, `null`},
		"null geom":     {TypeSticky, BundleGeom, `null`},
		"null style":    {TypeSticky, "style.color", `null`},
		"null title":    {TypeBoard, BundleTitle, `null`},
		"null grid":     {TypeBoard, BundleGrid, `null`},
		"null pt":       {TypeConnector, BundleEnd0, `{"pt":null}`},
		"null in pt":    {TypeConnector, BundleEnd0, `{"pt":[10,null]}`},
		"nulls in pt":   {TypeConnector, BundleEnd1, `{"pt":[null,null]}`},
		"null in at":    {TypeConnector, BundleEnd0, `{"el":"e_aaaaaaaaaa","at":[null,0.5]}`},
		"text in at":    {TypeConnector, BundleEnd0, `{"el":"e_aaaaaaaaaa","at":["0.5",0.5]}`},
		"empty el":      {TypeConnector, BundleEnd0, `{"el":"","at":[1,0.5]}`},
		"empty el only": {TypeConnector, BundleEnd1, `{"el":""}`},
	} {
		if v, err := DecodeBundle(c.typ, c.g, json.RawMessage(c.raw)); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: %v, err = %v", name, v, err)
		}
	}
	for name, raw := range map[string]string{
		"short pt":        `{"id":"e_aaaaaaaaaa","type":"connector","ends":[{"pt":[10]},{"pt":[0,0]}]}`,
		"long at":         `{"id":"e_aaaaaaaaaa","type":"connector","ends":[{"el":"e_bbbbbbbbbb","at":[0,0,1]},{"pt":[0,0]}]}`,
		"null in pt":      `{"id":"e_aaaaaaaaaa","type":"connector","ends":[{"pt":[10,null]},{"pt":[0,0]}]}`,
		"null in at":      `{"id":"e_aaaaaaaaaa","type":"connector","ends":[{"el":"e_bbbbbbbbbb","at":[null,0.5]},{"pt":[0,0]}]}`,
		"null in pts":     `{"id":"e_aaaaaaaaaa","type":"pen","x":0,"y":0,"w":10,"h":10,"pts":[0,0,1,null]}`,
		"null in natural": `{"id":"e_aaaaaaaaaa","type":"image","natural":[null,3]}`,
		"empty el":        `{"id":"e_aaaaaaaaaa","type":"connector","ends":[{"el":"","at":[1,0.5]},{"pt":[0,0]}]}`,
	} {
		if _, _, err := DecodeElement([]byte(raw)); !errors.Is(err, ErrFormat) {
			t.Errorf("element %s: err = %v", name, err)
		}
	}
	// a null "at" is a missing one: the center of the target
	if v, err := DecodeBundle(TypeConnector, BundleEnd0, json.RawMessage(`{"el":"e_aaaaaaaaaa","at":null}`)); err != nil || v != (End{El: "e_aaaaaaaaaa", At: [2]float64{0.5, 0.5}}) {
		t.Fatalf("null at = %v, %v", v, err)
	}
	// membership bundles keep null as "none"
	if v, err := DecodeBundle(TypeSticky, BundleFrame, json.RawMessage(`null`)); err != nil || v != "" {
		t.Fatalf("null frame = %v, %v", v, err)
	}
}

func TestClampPoint(t *testing.T) {
	for _, c := range []struct{ in, want [2]float64 }{
		{[2]float64{0, 0}, [2]float64{0, 0}},
		{[2]float64{MaxCoord, -MaxCoord}, [2]float64{MaxCoord, -MaxCoord}},
		{[2]float64{MaxCoord + 100, 50}, [2]float64{MaxCoord, 50}},
		{[2]float64{-3, -MaxCoord - 0.1}, [2]float64{-3, -MaxCoord}},
	} {
		got := ClampPoint(c.in)
		if got != c.want {
			t.Errorf("ClampPoint(%v) = %v, want %v", c.in, got, c.want)
		}
		if err := (End{Pt: got}).validate(); err != nil {
			t.Errorf("ClampPoint(%v) gives an invalid end: %v", c.in, err)
		}
	}
}
