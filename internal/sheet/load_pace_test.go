package sheet

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// countingReader counts the bytes a decoder took from its input.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// readGaps follows the input a decoder read between its check points.
type readGaps struct {
	in           *countingReader
	points, last int
	most         int
	stopAt       int // the check point that reports false, 0 for none
}

func (g *readGaps) pause() bool {
	g.points++
	g.most = max(g.most, g.in.n-g.last)
	g.last = g.in.n
	return g.points != g.stopAt
}

// readGapBound is the most input a decode may read between two check
// points: the 4,096 units of a step at 64 bytes and a little for keys and
// punctuation each, and what the decoder reads ahead, which is one large
// value at most and a buffer beside it.
const readGapBound = loadCheckCells*80 + 128<<10

// denseRowFile returns a row file of rows full rows of cols small numbers,
// as the engine writes it.
func denseRowFile(rows, cols int) []byte {
	var b strings.Builder
	b.WriteString(`{"rows":[`)
	for r := range rows {
		if r > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"cells":{`)
		for c := range cols {
			if c > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"%d":{"v":%d}`, c+1, (r*cols+c)%1000)
		}
		fmt.Fprintf(&b, `},"id":%d}`, r+1)
	}
	b.WriteString(`],"styles":[]}`)
	return []byte(b.String())
}

// longRowFile returns a row file of rows cells of long text, one to a row,
// and of styles formats.
func longRowFile(rows, styles int) []byte {
	text := strings.Repeat("x", 32767)
	var b strings.Builder
	b.WriteString(`{"rows":[`)
	for r := range rows {
		if r > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"cells":{"1":{"s":%d,"v":%q}},"id":%d}`, r%styles, text, r+1)
	}
	b.WriteString(`],"styles":[`)
	for s := range styles {
		if s > 0 {
			b.WriteByte(',')
		}
		b.WriteString(symbolFormat(s))
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// styledRowFile returns a row file of rows rows of cols numbers, each cell
// of a format of its own.
func styledRowFile(rows, cols int) []byte {
	var b strings.Builder
	b.WriteString(`{"rows":[`)
	for r := range rows {
		if r > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"cells":{`)
		for c := range cols {
			if c > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"%d":{"s":%d,"v":1}`, c+1, r*cols+c)
		}
		fmt.Fprintf(&b, `},"id":%d}`, r+1)
	}
	b.WriteString(`],"styles":[`)
	for s := range rows * cols {
		if s > 0 {
			b.WriteByte(',')
		}
		b.WriteString(symbolFormat(s))
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// A row file is decoded value by value: between two check points the
// decode reads no more of the file than a step of rows, cells and styles,
// whether the file holds 256 full rows of 702 small numbers or cells of
// long text. It decodes what a whole decode of the file takes, and a check
// point that reports false ends it there, with nothing more read.
func TestRowFileDecodeTakesTurns(t *testing.T) {
	for name, data := range map[string][]byte{
		"256 rows of 702 numbers": denseRowFile(chunkRows, 702),
		"cells of long text":      longRowFile(100, 100),
		"a style for each cell":   styledRowFile(chunkRows, 20),
	} {
		t.Run(name, func(t *testing.T) {
			var want revisionFile
			if err := strictJSON(data, &want); err != nil {
				t.Fatal(err)
			}
			in := &countingReader{r: bytes.NewReader(data)}
			g := &readGaps{in: in}
			got, err := decodeRowFile(in, &loadPace{pause: g.pause})
			if err != nil {
				t.Fatal(err)
			}
			g.most = max(g.most, in.n-g.last)
			if !reflect.DeepEqual(got, want) {
				t.Fatal("the decode differs from a whole decode of the file")
			}
			if g.most > readGapBound || in.n != len(data) {
				t.Fatalf("the decode of %d bytes read %d between two of its %d check points, at most %d", len(data), g.most, g.points, readGapBound)
			}
			in = &countingReader{r: bytes.NewReader(data)}
			g = &readGaps{in: in, stopAt: 2}
			if _, err := decodeRowFile(in, &loadPace{pause: g.pause}); !errors.Is(err, errRevisionStopped) {
				t.Fatalf("a stopped decode answered %v", err)
			}
			if g.points != 2 || in.n != g.last {
				t.Fatalf("the decode went on after its stop: %d check points, %d bytes read, %d at the stop", g.points, in.n, g.last)
			}
		})
	}
}

// The decode of a row file and of an axes file refuses what the engine
// does not write: a key twice, a key it does not know, a column twice in
// a row, and data after the object.
func TestRevisionDecodeRefusesRepeatedKeys(t *testing.T) {
	for _, data := range []string{
		`{"rows":[],"rows":[],"styles":[]}`,
		`{"rows":[],"styles":[],"extra":1}`,
		`{"rows":[{"cells":{"1":{"v":1},"1":{"v":2}},"id":1}],"styles":[]}`,
		`{"rows":[{"cells":{},"cells":{},"id":1}],"styles":[]}`,
		`{"rows":[{"cells":{},"id":1,"id":2}],"styles":[]}`,
		`{"rows":[],"styles":[]} {}`,
		`null`,
	} {
		if _, err := decodeRowFile(strings.NewReader(data), &loadPace{}); err == nil {
			t.Errorf("the row file %s was taken", data)
		}
	}
	for _, data := range []string{
		`{"cols":[[1,1]],"cols":[[1,1]],"rows":[[1,1]],"widths":{}}`,
		`{"cols":[[1,1]],"rows":[[1,1]],"widths":{"1":40,"1":50}}`,
		`{"cols":[[1,1]],"rows":[[1,1]],"widths":{},"other":[]}`,
	} {
		if _, err := decodeAxes(strings.NewReader(data), &loadPace{}); err == nil {
			t.Errorf("the axes file %s was taken", data)
		}
	}
}

// fragmentedAxes returns an axes file of rows runs of one row each and
// as many runs of columns as a tab can hold, each with a width.
func fragmentedAxes(rows int) []byte {
	var b strings.Builder
	runs := func(n int) {
		b.WriteByte('[')
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "[%d,1]", 2*i+1)
		}
		b.WriteByte(']')
	}
	b.WriteString(`{"cols":`)
	runs(702)
	b.WriteString(`,"rows":`)
	runs(rows)
	b.WriteString(`,"widths":{`)
	for i := range 702 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"%d":%d`, 2*i+1, 40+i)
	}
	b.WriteString(`}}`)
	return []byte(b.String())
}

// The axes of a revision whose ids lie in runs of one are decoded run by
// run and built id by id, with a check point at least every step of
// either: the decode reads no more than a step between two, and a build
// that a check point stops holds no more than a step of ids.
func TestRevisionAxesTakeTurns(t *testing.T) {
	data := fragmentedAxes(100_000)
	in := &countingReader{r: bytes.NewReader(data)}
	g := &readGaps{in: in}
	axes, err := decodeAxes(in, &loadPace{pause: g.pause})
	if err != nil {
		t.Fatal(err)
	}
	g.most = max(g.most, in.n-g.last)
	if len(axes.Rows) != 100_000 || len(axes.Cols) != 702 || len(axes.Widths) != 702 {
		t.Fatalf("the decode found %d row runs, %d column runs and %d widths", len(axes.Rows), len(axes.Cols), len(axes.Widths))
	}
	if g.most > readGapBound {
		t.Fatalf("the decode of %d bytes read %d between two of its %d check points, at most %d", len(data), g.most, g.points, readGapBound)
	}
	points := 0
	a, next, err := axisFromRuns(axes.Rows, 100_000, &loadPace{pause: func() bool { points++; return true }})
	if err != nil || len(a.ids) != 100_000 || next != 2*100_000 {
		t.Fatalf("the build: %v, %d ids, next %d", err, len(a.ids), next)
	}
	if points < 100_000/loadCheckCells {
		t.Fatalf("the build of 100,000 ids reached %d check points", points)
	}
	a, _, err = axisFromRuns(axes.Rows, 100_000, &loadPace{pause: func() bool { return false }})
	if !errors.Is(err, errRevisionStopped) || len(a.ids) > loadCheckCells {
		t.Fatalf("a stopped build answered %v with %d ids built", err, len(a.ids))
	}
}

// heapAllocs returns the bytes the process allocated so far. It reads
// the memory statistics, which count every allocation up to the call.
func heapAllocs() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.TotalAlloc
}

// wantPacedAllocs runs work, which goes through units units of size bytes
// each, with a pause that sees what the process allocated so far. A size
// of 0 leaves the allocations unchecked. Up to
// each check point the work allocates no more than eight times what the
// units before it take, which covers a slice that grows by a quarter each
// time and the copies on the way, and a little beside, so it
// cannot have built more than that before its first check point either.
// It reaches a check point for every step of units, and a check point that
// reports false stops it there.
func wantPacedAllocs(t *testing.T, units, size int, work func(pause func() bool) error) {
	t.Helper()
	const slack = 256 << 10
	start, points, most := heapAllocs(), 0, int64(0)
	err := work(func() bool {
		points++
		allowed := int64(8*size*points*loadCheckCells + slack)
		most = max(most, int64(heapAllocs()-start)-allowed)
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if points < units/loadCheckCells {
		t.Fatalf("the work of %d units reached %d check points", units, points)
	}
	if measures && size > 0 && most > 0 {
		t.Fatalf("the work allocated %d bytes more by a check point than the units before it take", most)
	}
	stops := 0
	start = heapAllocs()
	err = work(func() bool { stops++; return false })
	if err == nil || stops != 1 {
		t.Fatalf("a stopped work answered %v after %d check points", err, stops)
	}
	if got := heapAllocs() - start; measures && size > 0 && got > uint64(8*size*loadCheckCells+slack) {
		t.Fatalf("the work allocated %d bytes before it stopped at its first check point", got)
	}
}

// A load reads the id runs, the born stamps and the chunk list of an axis
// with a check point at least every step of ids or runs, also when every
// run holds one id, and the free style ids the same way: each allocates up
// to a check point only what the units before it need, and stops there.
func TestLoadDecodingTakesTurns(t *testing.T) {
	const n = 100_000
	ids := make([]uint64, n)
	var born runStamps
	for i := range ids {
		ids[i] = uint64(2*i + 1)
		born = append(born, stampRun{ids[i], 1, uint64(i + 1)})
	}
	idBlob, bornBlob := encodeIDs(ids), encodeStamps(born)
	var chunkBlob []byte
	for _, id := range ids {
		chunkBlob = binary.AppendUvarint(chunkBlob, id)
	}
	t.Run("ids", func(t *testing.T) {
		wantPacedAllocs(t, n, 8, func(pause func() bool) error {
			got, err := decodeIDs(idBlob, n, &loadPace{pause: pause})
			if err == nil && len(got) != n {
				err = fmt.Errorf("%d ids", len(got))
			}
			return err
		})
	})
	t.Run("born stamps", func(t *testing.T) {
		wantPacedAllocs(t, n, 24, func(pause func() bool) error {
			got, err := decodeStamps(bornBlob, n, &loadPace{pause: pause})
			if err == nil && len(got) != n {
				err = fmt.Errorf("%d runs", len(got))
			}
			return err
		})
	})
	t.Run("chunk list", func(t *testing.T) {
		wantPacedAllocs(t, n, 8, func(pause func() bool) error {
			got, err := decodeRowIDs(chunkBlob, n, &loadPace{pause: pause})
			if err == nil && len(got) != n {
				err = fmt.Errorf("%d chunks", len(got))
			}
			return err
		})
	})
	// The axis also places its ids in a map made at once; its decoding is
	// checked above.
	t.Run("axis", func(t *testing.T) {
		wantPacedAllocs(t, 3*n, 0, func(pause func() bool) error {
			_, err := loadAxis(idBlob, bornBlob, n, 2*n+1, &loadPace{pause: pause})
			return err
		})
	})
	t.Run("free style ids", func(t *testing.T) {
		styles := make([]uint32, n)
		for i := range styles {
			styles[i] = uint32(2*i + 2)
		}
		table := newStyleTable()
		wantPacedAllocs(t, n, 8, func(pause func() bool) error {
			was := table
			if !table.refill(styles, pause) {
				if !reflect.DeepEqual(was, table) {
					return fmt.Errorf("a stopped refill changed the table")
				}
				return errLoadStopped
			}
			if table.freeIDs != n || table.next != 2*n+1 {
				return fmt.Errorf("the refill found %d free ids below %d", table.freeIDs, table.next)
			}
			return nil
		})
	})
}
