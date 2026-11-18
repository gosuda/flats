package sheet

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// File identifies one revision file: the manifest, the axes of a tab or a
// row file (technical design, section 16.1).
type File struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

const (
	manifestPath  = "sheet.json"
	chunkRows     = 256
	chunkPartSize = 4 << 20
)

// The most chunks the writer leaves. At a boundary every chunk list is
// one that rebalance made, after which every two neighboring chunks hold
// more than chunkRows filled rows together. Chunks hold filled rows only
// by edits, and an edit makes the chunk it changes dirty, so the counts of
// the chunks it did not touch are those rebalance saw. A tab of n filled
// rows at a boundary therefore has at most 2*floor(n/(chunkRows+1))+1
// chunks, at the most rows a tab holds.
//
// Until the next boundary a tab that was live at the last one keeps its
// list of then, whatever its rows become, and every other tab has one
// chunk: a new tab, a recovered one and one that undo or redo brings back
// (restoreTab), which leaves the list it was saved with behind. So at any
// state the tabs hold at most the chunks the tabs of the last boundary
// held, whose filled rows the cells of a sheet bound, and one for each
// other tab. The chunks beyond one for each tab are therefore at most
// maxExtraChunks at any state, which leaves room for every tab a sheet
// may still add.
//
// A list that does not come from rebalance has no such bound, so every
// list the workbook takes from storage, from sheet.db or from a revision,
// is checked against what rebalance leaves (checkChunks).
const (
	maxTabChunks   = 2*(formula.MaxRows/(chunkRows+1)) + 1
	maxExtraChunks = 2 * (maxCells / (chunkRows + 1))
	maxSheetChunks = maxExtraChunks + maxTabs
)

// maxRevisionFiles is the most files a revision lists: the manifest, the
// axes of each tab, a first part of each chunk, and a part more for each
// chunkPartSize of row files, which every part before the last of a chunk
// holds at least. The row files hold at most maxSheetFileBytes: an entry
// of each cell with its key, number and style, and the format it may add,
// and the typed text of the sheet, each code point of which takes at most
// nine bytes of stored form (formula.MaxStoredBytes) and six bytes in
// JSON for each of those.
const (
	maxSheetFileBytes = maxCells*(128+maxFormatBytes) + 9*6*maxSheetText
	maxRevisionFiles  = 1 + maxTabs + maxSheetChunks + maxSheetFileBytes/chunkPartSize
)

func axesPath(tab TabID) string { return "tabs/" + strconv.FormatUint(uint64(tab), 10) + "/axes.json" }

func chunkPrefix(tab TabID, first RowID) string {
	return "tabs/" + strconv.FormatUint(uint64(tab), 10) + "/rows/" + strconv.FormatUint(uint64(first), 16) + "-"
}

// filePath is a path of a revision file as the writer spells it: the
// manifest, the axes of a tab, or a part of a chunk of a tab.
type filePath struct {
	tab   TabID
	axes  bool
	chunk bool
	first RowID
	part  int
}

// parseFilePath reads a path of a revision file. It takes only the
// spellings the writer makes (axesPath, chunkPrefix): a decimal tab id
// and part number and a lowercase hexadecimal first id, without a sign or
// a leading zero. Another spelling would name the same file twice, and a
// later change of the file would not find it. The first id of a chunk is
// a row id or 0 and lies below maxID, as a load of the chunk list in
// sheet.db requires (decodeRowIDs).
func parseFilePath(path string) (filePath, bool) {
	if path == manifestPath {
		return filePath{}, true
	}
	rest, ok := strings.CutPrefix(path, "tabs/")
	idText, rest, cut := strings.Cut(rest, "/")
	id, err := strconv.ParseUint(idText, 10, 64)
	if !ok || !cut || err != nil || id == 0 || id >= maxID {
		return filePath{}, false
	}
	p := filePath{tab: TabID(id)}
	if rest == "axes.json" {
		p.axes = true
		return p, path == axesPath(p.tab)
	}
	name, ok := strings.CutPrefix(rest, "rows/")
	name, suffix := strings.CutSuffix(name, ".json")
	hexFirst, partText, cut := strings.Cut(name, "-")
	first, ferr := strconv.ParseUint(hexFirst, 16, 64)
	part, perr := strconv.Atoi(partText)
	if !ok || !suffix || !cut || ferr != nil || perr != nil || part < 0 || first >= maxID {
		return filePath{}, false
	}
	p.chunk, p.first, p.part = true, RowID(first), part
	return p, path == chunkPrefix(p.tab, p.first)+strconv.Itoa(part)+".json"
}

func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validHash(hash string) bool {
	if len(hash) != sha256.Size*2 || strings.ToLower(hash) != hash {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil
}

type chunkKey struct {
	tab   TabID
	first RowID
}

// fileState is the revision file list of the newest recorded revision and
// what has changed since: the dirty files, with the cumulative reserve of
// each dirty row file (section 16.5).
type fileState struct {
	list     map[string]File
	manifest bool
	axes     map[TabID]bool
	chunks   map[chunkKey]int64
	changed  map[TabID]bool // tabs that changed since the last boundary
}

func newFileState() fileState {
	return fileState{list: map[string]File{}, axes: map[TabID]bool{}, chunks: map[chunkKey]int64{}, changed: map[TabID]bool{}}
}

func (fs *fileState) dirty() bool { return fs.manifest || len(fs.axes) > 0 || len(fs.chunks) > 0 }

func (fs *fileState) sorted() []File {
	files := make([]File, 0, len(fs.list))
	for _, f := range fs.list {
		files = append(files, f)
	}
	slices.SortFunc(files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	return files
}

// chunkSize returns the bytes of the recorded parts of a chunk.
func (fs *fileState) chunkSize(key chunkKey) int64 {
	prefix := chunkPrefix(key.tab, key.first)
	var size int64
	for part := 0; ; part++ {
		f, ok := fs.list[prefix+strconv.Itoa(part)+".json"]
		if !ok {
			return size
		}
		size += f.Size
	}
}

// appendNumber appends a float as canonical JSON: the shortest text that
// reads back as the same number.
func appendNumber(dst []byte, f float64) []byte {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.AppendInt(dst, int64(f), 10)
	}
	abs := math.Abs(f)
	if abs < 1e-6 || abs >= 1e21 {
		dst = strconv.AppendFloat(dst, f, 'e', -1, 64)
		// Clean up e-09 to e-9, as encoding/json does.
		if n := len(dst); n >= 4 && dst[n-4] == 'e' && dst[n-3] == '-' && dst[n-2] == '0' {
			dst[n-2] = dst[n-1]
			dst = dst[:n-1]
		}
		return dst
	}
	return strconv.AppendFloat(dst, f, 'f', -1, 64)
}

// appendString appends s as a JSON string without escaping HTML characters.
func appendString(dst []byte, s string) []byte {
	const hexDigits = "0123456789abcdef"
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b >= 0x20 && b != '"' && b != '\\' {
			continue
		}
		dst = append(dst, s[start:i]...)
		switch b {
		case '"', '\\':
			dst = append(dst, '\\', b)
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			dst = append(dst, '\\', 'u', '0', '0', hexDigits[b>>4], hexDigits[b&0xf])
		}
		start = i + 1
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

func encodeManifest(wb *Workbook) []byte {
	dst := []byte(`{"format":1,"tabs":[`)
	for i, t := range wb.tabs {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, `{"frozen_cols":`...)
		dst = strconv.AppendInt(dst, int64(t.frozenCols), 10)
		dst = append(dst, `,"frozen_rows":`...)
		dst = strconv.AppendInt(dst, int64(t.frozenRows), 10)
		dst = append(dst, `,"id":`...)
		dst = strconv.AppendUint(dst, uint64(t.id), 10)
		dst = append(dst, `,"name":`...)
		dst = appendString(dst, t.name)
		dst = append(dst, '}')
	}
	return append(dst, ']', '}')
}

func appendRuns(dst []byte, ids []uint64) []byte {
	dst = append(dst, '[')
	first := true
	forRuns(ids, func(start, count uint64) {
		if !first {
			dst = append(dst, ',')
		}
		first = false
		dst = append(dst, '[')
		dst = strconv.AppendUint(dst, start, 10)
		dst = append(dst, ',')
		dst = strconv.AppendUint(dst, count, 10)
		dst = append(dst, ']')
	})
	return append(dst, ']')
}

func encodeAxes(t *Tab) []byte {
	dst := []byte(`{"cols":`)
	dst = appendRuns(dst, t.cols.ids)
	dst = append(dst, `,"rows":`...)
	dst = appendRuns(dst, t.rows.ids)
	dst = append(dst, `,"widths":{`...)
	cols := make([]ColID, 0, len(t.widths))
	for col, w := range t.widths {
		if w.px != defaultWidth {
			cols = append(cols, col)
		}
	}
	slices.Sort(cols)
	for i, col := range cols {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, '"')
		dst = strconv.AppendUint(dst, uint64(col), 10)
		dst = append(dst, '"', ':')
		dst = strconv.AppendInt(dst, int64(t.widths[col].px), 10)
	}
	return append(dst, '}', '}')
}

// appendFileCell appends the entry of one cell in a row file. The keys are
// d, dt, f, s, t, v and x, in that order.
func appendFileCell(dst []byte, c *Cell, style int) []byte {
	dst = append(dst, '{')
	n := len(dst)
	sep := func(key string) {
		if len(dst) > n {
			dst = append(dst, ',')
		}
		dst = append(dst, '"')
		dst = append(dst, key...)
		dst = append(dst, '"', ':')
	}
	styled := func() {
		if style >= 0 {
			sep("s")
			dst = strconv.AppendInt(dst, int64(style), 10)
			style = -1
		}
	}
	k := c.content
	switch k.Kind {
	case formula.ContentDate:
		sep("d")
		dst = appendString(dst, formula.Date(math.Floor(k.Num)).String())
	case formula.ContentDateTime:
		sep("dt")
		dst = append(dst, '"')
		dst = append(dst, formula.Date(math.Floor(k.Num)).String()...)
		dst = append(dst, 'T')
		dst = appendClock(dst, k.Num)
		dst = append(dst, '"')
	case formula.ContentFormula:
		sep("f")
		dst = appendString(dst, k.Text)
	}
	styled()
	switch k.Kind {
	case formula.ContentTime:
		sep("t")
		dst = append(dst, '"')
		dst = appendClock(dst, k.Num)
		dst = append(dst, '"')
	case formula.ContentNumber:
		sep("v")
		dst = appendNumber(dst, k.Num)
	case formula.ContentText:
		sep("v")
		dst = appendString(dst, k.Text)
	case formula.ContentBool:
		sep("v")
		dst = strconv.AppendBool(dst, k.Num != 0)
	case formula.ContentUnreadable:
		sep("x")
		dst = appendString(dst, k.Text)
	}
	return append(dst, '}')
}

// firstDay and lastDay are the days a row file holds: ParseDate reads the
// years 1900 to 9999.
var firstDay, lastDay = float64(formula.DateOf(1900, 1, 1)), float64(formula.DateOf(9999, 12, 31))

// durableNumber reports whether a row file holds the number of a date, a
// time or a date with time as that same number (appendFileCell and
// decodeFileCell): a date is a whole day from 1900 to 9999, a time a whole
// second of a day, and a date with time a whole second of such a day.
// Typed entry and agent writes give no other. Any other number would be
// written as another date or time, or as a date the reader refuses.
func durableNumber(kind formula.ContentKind, n float64) bool {
	var buf [16]byte
	switch kind {
	case formula.ContentDate:
		return n >= firstDay && n <= lastDay && n == math.Trunc(n)
	case formula.ContentTime:
		if !(n >= 0 && n < 1) {
			return false
		}
		frac, ok := parseClock(string(appendClock(buf[:0], n)))
		return ok && frac == n
	case formula.ContentDateTime:
		if !(n >= firstDay && n < lastDay+1) {
			return false
		}
		frac, ok := parseClock(string(appendClock(buf[:0], n)))
		return ok && math.Floor(n)+frac == n
	}
	return true
}

// cellBound is the encoded size of one cell entry in its row file, with
// room for its key, for the cumulative reserve.
func cellBound(c *Cell) int64 {
	n := int64(48)
	if c.content.Kind == formula.ContentText || c.content.IsFormula() {
		n += int64(jsonLen([]byte(c.content.Text)))
	}
	return n
}

// filledRows returns the ids of the rows that hold a cell record with
// content or a format, sorted.
func (wb *Workbook) filledRows(t *Tab) []RowID {
	rows := make([]RowID, 0, len(t.data))
	for id, rd := range t.data {
		for _, s := range rd.cells {
			if wb.counted(s.c) {
				rows = append(rows, id)
				break
			}
		}
	}
	slices.Sort(rows)
	return rows
}

// encodeChunk encodes the filled rows of one chunk as its parts. A part
// ends at a cell boundary once it has reached the part size.
func (wb *Workbook) encodeChunk(t *Tab, rows []RowID) ([][]byte, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	var parts [][]byte
	var body, styles []byte
	styleIndex := map[formula.Format]int{}
	closePart := func() {
		part := make([]byte, 0, len(body)+len(styles)+32)
		part = append(part, `{"rows":[`...)
		part = append(part, body...)
		part = append(part, `],"styles":[`...)
		part = append(part, styles...)
		part = append(part, ']', '}')
		parts = append(parts, part)
		body, styles = body[:0], styles[:0]
		clear(styleIndex)
	}
	for _, row := range rows {
		rd := t.data[row]
		open := false
		for _, s := range rd.cells {
			if !wb.counted(s.c) {
				continue
			}
			if !open {
				if len(body) > 0 {
					body = append(body, ',')
				}
				body = append(body, `{"cells":{`...)
				open = true
			} else {
				body = append(body, ',')
			}
			style := -1
			if f := wb.styles.format(s.c.style); f != (formula.Format{}) {
				idx, ok := styleIndex[f]
				if !ok {
					idx = len(styleIndex)
					styleIndex[f] = idx
					if idx > 0 {
						styles = append(styles, ',')
					}
					var err error
					if styles, err = appendFormat(styles, f); err != nil {
						return nil, err
					}
				}
				style = idx
			}
			body = append(body, '"')
			body = strconv.AppendUint(body, uint64(s.col), 10)
			body = append(body, '"', ':')
			body = appendFileCell(body, s.c, style)
			if len(body)+len(styles) >= chunkPartSize {
				body = append(body, `},"id":`...)
				body = strconv.AppendUint(body, uint64(row), 10)
				body = append(body, '}')
				closePart()
				open = false
			}
		}
		if open {
			body = append(body, `},"id":`...)
			body = strconv.AppendUint(body, uint64(row), 10)
			body = append(body, '}')
		}
	}
	if len(body) > 0 {
		closePart()
	}
	return parts, nil
}

// rebalance returns the chunk list of a tab after the touched chunks were
// split at every 256th filled row and merged with neighbors that together
// hold at most 256, and the chunks to encode again.
func rebalance(chunks []RowID, filled []RowID, touched map[RowID]bool) ([]RowID, map[RowID]bool) {
	count := func(first RowID, next RowID, last bool) (int, int) {
		lo := sort.Search(len(filled), func(i int) bool { return filled[i] >= first })
		hi := len(filled)
		if !last {
			hi = sort.Search(len(filled), func(i int) bool { return filled[i] >= next })
		}
		return lo, hi
	}
	type chunk struct {
		first  RowID
		n      int
		encode bool
	}
	var out []chunk
	for i, first := range chunks {
		last := i == len(chunks)-1
		var next RowID
		if !last {
			next = chunks[i+1]
		}
		lo, hi := count(first, next, last)
		if !touched[first] || hi-lo <= chunkRows {
			out = append(out, chunk{first, hi - lo, touched[first]})
			continue
		}
		for at := lo; at < hi; at += chunkRows {
			id := first
			if at > lo {
				id = filled[at]
			}
			out = append(out, chunk{id, min(chunkRows, hi-at), true})
		}
	}
	for i := 0; i+1 < len(out); {
		if (out[i].encode || out[i+1].encode) && out[i].n+out[i+1].n <= chunkRows {
			out[i].n += out[i+1].n
			out[i].encode = true
			out = append(out[:i+1], out[i+2:]...)
			if i > 0 {
				i--
			}
			continue
		}
		i++
	}
	firsts := make([]RowID, len(out))
	encode := map[RowID]bool{}
	for i, c := range out {
		firsts[i] = c.first
		if c.encode {
			encode[c.first] = true
		}
	}
	return firsts, encode
}

// checkChunks refuses a chunk list of t that the writer does not leave.
// dirty holds the chunks an edit marked since the last boundary; a
// revision has none. A chunk without a mark holds the filled rows
// rebalance left in it, so two neighboring chunks without a mark hold
// more than chunkRows together. The next boundary rebalances the marked
// chunks and merges every pair with one of them that holds at most
// chunkRows, which leaves every pair of the tab above chunkRows again:
// a list that passes stays within the bound of maxExtraChunks through
// any later write. It reports false when pace stops it.
func (wb *Workbook) checkChunks(t *Tab, dirty map[chunkKey]int64, pace *loadPace) (bool, error) {
	if len(t.chunks) < 2 {
		return true, nil
	}
	fill := make([]int, len(t.chunks))
	for id, rd := range t.data {
		for _, s := range rd.cells {
			if !pace.step(1) {
				return false, nil
			}
			if wb.counted(s.c) {
				fill[sort.Search(len(t.chunks), func(i int) bool { return t.chunks[i] > id })-1]++
				break
			}
		}
	}
	for i := 0; i+1 < len(t.chunks); i++ {
		_, a := dirty[chunkKey{t.id, t.chunks[i]}]
		_, b := dirty[chunkKey{t.id, t.chunks[i+1]}]
		if !a && !b && fill[i]+fill[i+1] <= chunkRows {
			return true, fmt.Errorf("tab %d holds %d filled rows in the neighboring chunks %x and %x, which rebalancing merges",
				t.id, fill[i]+fill[i+1], uint64(t.chunks[i]), uint64(t.chunks[i+1]))
		}
	}
	return true, nil
}

// filePlan is what closing a revision changes: new files with their bytes,
// removed paths and the new chunk lists.
type filePlan struct {
	put     map[string]File
	data    map[string][]byte // by hash, only bytes the previous list lacks
	remove  []string
	chunks  map[*Tab][]RowID
	changed []TabID
	// encoded is the size of every chunk the plan encoded, whether or not
	// its bytes differ from the recorded file.
	encoded map[chunkKey]int64
}

// planFiles encodes the dirty files of the workbook. It changes nothing.
func (wb *Workbook) planFiles() (*filePlan, error) {
	fs := &wb.files
	p := &filePlan{put: map[string]File{}, data: map[string][]byte{}, chunks: map[*Tab][]RowID{}, encoded: map[chunkKey]int64{}}
	known := make(map[string]bool, len(fs.list))
	for _, f := range fs.list {
		known[f.Hash] = true
	}
	kept := map[string]bool{}
	add := func(path string, data []byte) {
		f := File{Path: path, Hash: contentHash(data), Size: int64(len(data))}
		if old, ok := fs.list[path]; ok && old == f {
			kept[path] = true
			return
		}
		p.put[path] = f
		if !known[f.Hash] {
			p.data[f.Hash] = data
			known[f.Hash] = true
		}
	}
	if fs.manifest {
		add(manifestPath, encodeManifest(wb))
		// Files of tabs that are gone leave the list with the manifest.
		for path := range fs.list {
			if rest, ok := strings.CutPrefix(path, "tabs/"); ok {
				id, _, _ := strings.Cut(rest, "/")
				n, _ := strconv.ParseUint(id, 10, 64)
				if wb.byID[TabID(n)] == nil {
					p.remove = append(p.remove, path)
				}
			}
		}
	}
	for id := range fs.axes {
		if t := wb.byID[id]; t != nil {
			add(axesPath(id), encodeAxes(t))
		}
	}
	touched := map[*Tab]map[RowID]bool{}
	for key := range fs.chunks {
		if t := wb.byID[key.tab]; t != nil {
			if touched[t] == nil {
				touched[t] = map[RowID]bool{}
			}
			touched[t][key.first] = true
		}
	}
	// The chunks the list holds files of, by tab. A tab that came back
	// since the last boundary has another chunk list than the files it
	// had then (restoreTab), so its files are found in the list.
	listed := map[TabID][]RowID{}
	for path := range fs.list {
		if f, ok := parseFilePath(path); ok && f.chunk && f.part == 0 && touched[wb.byID[f.tab]] != nil {
			listed[f.tab] = append(listed[f.tab], f.first)
		}
	}
	for t, firsts := range touched {
		filled := wb.filledRows(t)
		chunks, encode := rebalance(t.chunks, filled, firsts)
		if !slices.Equal(chunks, t.chunks) {
			p.chunks[t] = chunks
		}
		// Parts of chunks that were merged away or re-encoded leave first.
		for _, first := range listed[t.id] {
			if i, ok := slices.BinarySearch(chunks, first); !ok || encode[chunks[i]] {
				p.remove = append(p.remove, wb.chunkPaths(t.id, first)...)
			}
		}
		for i, first := range chunks {
			if !encode[first] {
				continue
			}
			lo := sort.Search(len(filled), func(k int) bool { return filled[k] >= first })
			hi := len(filled)
			if i+1 < len(chunks) {
				hi = sort.Search(len(filled), func(k int) bool { return filled[k] >= chunks[i+1] })
			}
			parts, err := wb.encodeChunk(t, filled[lo:hi])
			if err != nil {
				return nil, err
			}
			p.encoded[chunkKey{t.id, first}] = 0
			for n, part := range parts {
				add(chunkPrefix(t.id, first)+strconv.Itoa(n)+".json", part)
				p.encoded[chunkKey{t.id, first}] += int64(len(part))
			}
		}
	}
	// A path that was written again, with new or with the same bytes, stays.
	p.remove = slices.DeleteFunc(p.remove, func(path string) bool {
		_, again := p.put[path]
		return again || kept[path]
	})
	for id := range fs.changed {
		p.changed = append(p.changed, id)
	}
	slices.Sort(p.changed)
	return p, nil
}

func (wb *Workbook) chunkPaths(tab TabID, first RowID) []string {
	prefix := chunkPrefix(tab, first)
	var paths []string
	for part := 0; ; part++ {
		path := prefix + strconv.Itoa(part) + ".json"
		if _, ok := wb.files.list[path]; !ok {
			return paths
		}
		paths = append(paths, path)
	}
}

// applyPlan installs a plan in memory and returns the function that takes
// it out again.
func (wb *Workbook) applyPlan(p *filePlan) func() {
	old := wb.files
	chunks := map[*Tab][]RowID{}
	next := newFileState()
	for path, f := range old.list {
		next.list[path] = f
	}
	for _, path := range p.remove {
		delete(next.list, path)
	}
	for path, f := range p.put {
		next.list[path] = f
	}
	for t, list := range p.chunks {
		chunks[t] = t.chunks
		t.chunks = list
	}
	wb.files = next
	return func() {
		wb.files = old
		for t, list := range chunks {
			t.chunks = list
		}
	}
}

// revisionFile is the decoded form of a row file.
type revisionFile struct {
	Styles []json.RawMessage `json:"styles"`
	Rows   []fileRow         `json:"rows"`
}

type fileRow struct {
	ID    uint64                     `json:"id"`
	Cells map[string]json.RawMessage `json:"cells"`
}

// rawUnits is what a decoded value counts in the pace of a build: one, and
// one for each 64 bytes, so that a few long values reach a check point as
// many short ones do.
func rawUnits(raw json.RawMessage) int { return 1 + len(raw)/64 }

// jsonDelim reads the next token and checks that it is the delimiter d.
func jsonDelim(dec *json.Decoder, d json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != d {
		return fmt.Errorf("JSON %v where %v belongs", tok, d)
	}
	return nil
}

// jsonObject reads an object, calling member for each key with the value
// next in dec.
func jsonObject(dec *json.Decoder, member func(key string) error) error {
	if err := jsonDelim(dec, '{'); err != nil {
		return err
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		if err := member(key); err != nil {
			return err
		}
	}
	return jsonDelim(dec, '}')
}

// jsonArray reads an array, calling elem for each value next in dec.
func jsonArray(dec *json.Decoder, elem func() error) error {
	if err := jsonDelim(dec, '['); err != nil {
		return err
	}
	for dec.More() {
		if err := elem(); err != nil {
			return err
		}
	}
	return jsonDelim(dec, ']')
}

// jsonDone checks that nothing follows the value dec read.
func jsonDone(dec *json.Decoder) error {
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("unexpected data after the JSON value")
	}
	return nil
}

// jsonOnce returns a member function for an object of the named keys,
// each at most once, which calls read with the key's index.
func jsonOnce(keys []string, read func(i int) error) func(key string) error {
	seen := make([]bool, len(keys))
	return func(key string) error {
		i := slices.Index(keys, key)
		switch {
		case i < 0:
			return fmt.Errorf("unknown field %q", key)
		case seen[i]:
			return fmt.Errorf("field %q twice", key)
		}
		seen[i] = true
		return read(i)
	}
}

// decodeRowFile reads a row file value by value, with a step of pace for
// each row and for each cell and style by rawUnits, so that a build reaches
// a check point inside a large file and stops there when pace does. It
// takes what strictJSON takes into a revisionFile, with each key of an
// object once and in the case the engine writes it, and refuses the rest.
//
// What it holds is bounded as the writer bounds a file, and it refuses an
// entry past that bound when it reads it: the rows, each of a cell at
// least, ascend and are at most those of a chunk, each holds at most a
// cell for each column of a tab, and the styles come after the rows, as
// the writer sorts the keys, each a format other than the default and
// none twice, no more of them than the cells that could use them.
func decodeRowFile(r io.Reader, pace *loadPace) (revisionFile, error) {
	var file revisionFile
	dec := json.NewDecoder(r)
	cells := 0
	err := jsonObject(dec, jsonOnce([]string{"rows", "styles"}, func(i int) error {
		if i == 1 {
			if file.Rows == nil {
				return fmt.Errorf("the styles of a row file before its rows")
			}
			file.Styles = []json.RawMessage{}
			formats := map[formula.Format]bool{}
			return jsonArray(dec, func() error {
				if len(file.Styles) == cells {
					return fmt.Errorf("more styles than cells in a row file")
				}
				var raw json.RawMessage
				if err := dec.Decode(&raw); err != nil {
					return err
				}
				p, err := decodeFormatPatch(raw, true)
				if err != nil {
					return err
				}
				if p.f == (formula.Format{}) || formats[p.f] {
					return fmt.Errorf("style %d of the file is no format or one listed before", len(file.Styles))
				}
				formats[p.f] = true
				file.Styles = append(file.Styles, raw)
				if !pace.step(rawUnits(raw)) {
					return errRevisionStopped
				}
				return nil
			})
		}
		file.Rows = []fileRow{}
		return jsonArray(dec, func() error {
			if len(file.Rows) == chunkRows {
				return fmt.Errorf("more rows than a chunk holds in a row file")
			}
			row, err := decodeFileRow(dec, pace)
			if err != nil {
				return err
			}
			// The writer lists only the rows that hold a cell, once each,
			// in id order.
			if len(row.Cells) == 0 {
				return fmt.Errorf("row %d holds no cell", row.ID)
			}
			if n := len(file.Rows); n > 0 && row.ID <= file.Rows[n-1].ID {
				return fmt.Errorf("row %d follows row %d", row.ID, file.Rows[n-1].ID)
			}
			cells += len(row.Cells)
			file.Rows = append(file.Rows, row)
			return nil
		})
	}))
	if err == nil {
		err = jsonDone(dec)
	}
	// The writer writes no file for a chunk without filled rows.
	if err == nil && len(file.Rows) == 0 {
		err = fmt.Errorf("a row file without rows")
	}
	return file, err
}

func decodeFileRow(dec *json.Decoder, pace *loadPace) (fileRow, error) {
	var row fileRow
	err := jsonObject(dec, jsonOnce([]string{"cells", "id"}, func(i int) error {
		if i == 1 {
			return dec.Decode(&row.ID)
		}
		row.Cells = map[string]json.RawMessage{}
		return jsonObject(dec, func(key string) error {
			if _, twice := row.Cells[key]; twice {
				return fmt.Errorf("column %s twice in a row", key)
			}
			if len(row.Cells) == formula.MaxCols {
				return fmt.Errorf("more cells than a tab has columns in a row")
			}
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return err
			}
			row.Cells[key] = raw
			if !pace.step(rawUnits(raw)) {
				return errRevisionStopped
			}
			return nil
		})
	}))
	if err == nil && !pace.step(1) {
		err = errRevisionStopped
	}
	return row, err
}

type revisionManifest struct {
	Format int
	Tabs   []manifestTab
}

type manifestTab struct {
	ID         uint64 `json:"id"`
	Name       string `json:"name"`
	FrozenRows int    `json:"frozen_rows"`
	FrozenCols int    `json:"frozen_cols"`
}

// decodeManifest reads the manifest of a revision, a tab at a time, and
// refuses one of more tabs than a sheet may hold before it reads them. Each
// key of an object comes once and in the case the engine writes it.
func decodeManifest(data []byte) (revisionManifest, error) {
	var m revisionManifest
	dec := json.NewDecoder(bytes.NewReader(data))
	err := jsonObject(dec, jsonOnce([]string{"format", "tabs"}, func(i int) error {
		if i == 0 {
			return dec.Decode(&m.Format)
		}
		return jsonArray(dec, func() error {
			if len(m.Tabs) == maxTabs {
				return fmt.Errorf("more than %d tabs", maxTabs)
			}
			var tab manifestTab
			err := jsonObject(dec, jsonOnce([]string{"frozen_cols", "frozen_rows", "id", "name"}, func(i int) error {
				switch i {
				case 0:
					return dec.Decode(&tab.FrozenCols)
				case 1:
					return dec.Decode(&tab.FrozenRows)
				case 2:
					return dec.Decode(&tab.ID)
				}
				return dec.Decode(&tab.Name)
			}))
			if err != nil {
				return err
			}
			m.Tabs = append(m.Tabs, tab)
			return nil
		})
	}))
	if err == nil {
		err = jsonDone(dec)
	}
	return m, err
}

// revisionAxes is the decoded form of the axes file of a tab.
type revisionAxes struct {
	Rows, Cols [][2]uint64
	Widths     []fileWidth
}

type fileWidth struct {
	key string
	px  int
}

// decodeAxes reads an axes file value by value, with a step of pace for
// each run and width. It takes what strictJSON takes, with each key once
// and in the engine's case, each run of exactly two numbers, and no more
// runs or widths than the ids an axis can hold.
func decodeAxes(r io.Reader, pace *loadPace) (revisionAxes, error) {
	var axes revisionAxes
	dec := json.NewDecoder(r)
	runs := func(list *[][2]uint64, limit int) error {
		return jsonArray(dec, func() error {
			if len(*list) == limit {
				return fmt.Errorf("more runs than ids")
			}
			// A run is exactly a first id and a count. A longer array
			// would otherwise be read as its first two numbers.
			var run [2]uint64
			n := 0
			if err := jsonArray(dec, func() error {
				if n == len(run) {
					return fmt.Errorf("a run of more than two numbers")
				}
				n++
				return dec.Decode(&run[n-1])
			}); err != nil {
				return err
			}
			if n != len(run) {
				return fmt.Errorf("a run of %d numbers", n)
			}
			*list = append(*list, run)
			if !pace.step(1) {
				return errRevisionStopped
			}
			return nil
		})
	}
	err := jsonObject(dec, jsonOnce([]string{"cols", "rows", "widths"}, func(i int) error {
		switch i {
		case 0:
			return runs(&axes.Cols, formula.MaxCols)
		case 1:
			return runs(&axes.Rows, formula.MaxRows)
		}
		seen := map[string]bool{}
		return jsonObject(dec, func(key string) error {
			if seen[key] || len(seen) == formula.MaxCols {
				return fmt.Errorf("width %q twice or one too many", key)
			}
			seen[key] = true
			w := fileWidth{key: key}
			if err := dec.Decode(&w.px); err != nil {
				return err
			}
			axes.Widths = append(axes.Widths, w)
			if !pace.step(1) {
				return errRevisionStopped
			}
			return nil
		})
	}))
	if err == nil {
		err = jsonDone(dec)
	}
	return axes, err
}

type fileCell struct {
	D  *string         `json:"d"`
	DT *string         `json:"dt"`
	F  *string         `json:"f"`
	S  *int            `json:"s"`
	T  *string         `json:"t"`
	V  json.RawMessage `json:"v"`
	X  *string         `json:"x"`
}

// The largest revision files the writer writes, in bytes. A row file part
// ends at the first cell boundary at or past chunkPartSize (encodeChunk),
// so it holds less than that, the row and the cell that pass it, the
// format of the cell, and the brackets. A string of the files takes at
// most six bytes for each byte or code point it escapes (appendString).
const (
	maxManifestBytes  = len(`{"format":1,"tabs":[]}`) + maxTabs*(len(`{"frozen_cols":10,"frozen_rows":20,"id":,"name":""},`)+16+6*maxTabName)
	maxAxesBytes      = len(`{"cols":[],"rows":[],"widths":{}}`) + (formula.MaxRows+formula.MaxCols)*len(`[9007199254740991,100000],`) + formula.MaxCols*len(`"9007199254740991":1000,`)
	maxCellEntryBytes = len(`{"s":4294967295,"x":""}`) + 6*max(formula.MaxStoredBytes, formula.MaxTextLen, formula.MaxFormulaChars)
	maxPartBytes      = chunkPartSize + len(`,{"cells":{"9007199254740991":},"id":9007199254740991}`) + maxCellEntryBytes + 1 + maxFormatBytes + len(`{"rows":[],"styles":[]}`)
)

// maxFileBytes is the largest file the writer writes at a path that
// parseFilePath read.
func maxFileBytes(p filePath) int64 {
	switch {
	case p.chunk:
		return int64(maxPartBytes)
	case p.axes:
		return int64(maxAxesBytes)
	}
	return int64(maxManifestBytes)
}

// The most a sheet revision's file list holds, which a host reader admits
// before it holds the list: its files, and the bytes of a path and of a
// hash of one.
const (
	MaxRevisionFiles = maxRevisionFiles
	MaxFilePathBytes = maxPathBytes
	MaxFileHashBytes = maxHashBytes
)

// CheckFile checks an entry of a revision file list as the writer lists
// one: a path in its spelling, a lowercase SHA-256 and a size within what
// it writes at the path.
func CheckFile(f File) error {
	p, ok := parseFilePath(f.Path)
	switch {
	case !ok:
		return fmt.Errorf("sheet revision path %q", f.Path)
	case !validHash(f.Hash):
		return fmt.Errorf("sheet revision file %s has an invalid hash", f.Path)
	case f.Size < 0 || f.Size > maxFileBytes(p):
		return fmt.Errorf("sheet revision file %s is listed at %d bytes, more than the writer writes", f.Path, f.Size)
	}
	return nil
}

// ReadRevisionFile reads the revision file f from the reader open returns
// for its hash: as many bytes as its entry gives, at most what the writer
// writes at its path, and to the end, where a reader that checks the
// content against its hash does so. A path the writer does not write is
// refused.
func ReadRevisionFile(f File, open func(hash string) (io.ReadCloser, error)) ([]byte, error) {
	p, ok := parseFilePath(f.Path)
	if !ok {
		return nil, fmt.Errorf("sheet revision path %q", f.Path)
	}
	data, err := readFile(f, maxFileBytes(p), open, &loadPace{})
	if err != nil {
		return nil, fmt.Errorf("sheet revision file %s: %w", f.Path, err)
	}
	return data, nil
}

// filePiece is how much of a revision file readFile reads at a time.
const filePiece = 64 << 10

// readFile reads a revision file whose entry is f, at most limit bytes
// long, from the reader open returns for its hash. It reads a piece at a
// time, a step of pace for each 64 bytes as a decode counts them, so that
// a large file reaches check points while it is read and a check point
// that reports false ends the read there. A file of more bytes than its
// entry says is refused after one byte more; a reader that checks the
// content against its hash does so at the end it then reaches.
func readFile(f File, limit int64, open func(hash string) (io.ReadCloser, error), pace *loadPace) ([]byte, error) {
	if f.Size < 0 || f.Size > limit {
		return nil, fmt.Errorf("listed at %d bytes, more than the %d the writer writes", f.Size, limit)
	}
	r, err := open(f.Hash)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data := make([]byte, f.Size)
	for n := 0; n < len(data); {
		m, err := io.ReadFull(r, data[n:min(n+filePiece, len(data))])
		n += m
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("holds %d bytes, not %d", n, f.Size)
		}
		if err != nil {
			return nil, err
		}
		if !pace.step(1 + m/64) {
			return nil, errRevisionStopped
		}
	}
	var more [1]byte
	switch _, err := io.ReadFull(r, more[:]); {
	case err == nil:
		return nil, fmt.Errorf("holds more than %d bytes", f.Size)
	case !errors.Is(err, io.EOF):
		return nil, err
	}
	return data, nil
}

// loadRevision builds a workbook from revision files. open returns a
// reader of a file by its hash, which loadRevision closes. Stamps are zero
// and the id counters cover the ids of the files; the caller sets what its
// use needs.
//
// pause, when it is not nil, is the check point of the build in a phase,
// every loadCheckCells units of it: the files it lists, the bytes it reads
// (readFile), the runs, widths and ids of the axes, the rows, cells and
// styles it decodes in a row file (rawUnits), the styles and cells it
// builds and the formats it indexes. It stops the build when it reports
// false.
func loadRevision(files []File, open func(hash string) (io.ReadCloser, error), pause func(phase string) bool) (*Workbook, error) {
	if pause == nil {
		pause = func(string) bool { return true }
	}
	phase := func(name string) loadPace { return loadPace{pause: func() bool { return pause(name) }} }
	listed, readPace, axesPace, rowsPace := phase("files"), phase("read"), phase("axes"), phase("rows")
	// A list of more files than a revision holds is refused before
	// anything in proportion to it is made.
	if len(files) > maxRevisionFiles {
		return nil, fmt.Errorf("sheet revision lists %d files, more than the %d of a revision", len(files), maxRevisionFiles)
	}
	byPath := make(map[string]File, len(files))
	// The row files of each tab, by the id in their path.
	rowFiles := map[TabID][]chunkFile{}
	// Every part of a chunk but its last holds chunkPartSize bytes at
	// least (chunkFiles), so the bytes of the row files bound the parts a
	// chunk can list beside the chunks.
	var rowBytes int64
	for _, f := range files {
		if !listed.step(1) {
			return nil, errRevisionStopped
		}
		p, ok := parseFilePath(f.Path)
		if _, twice := byPath[f.Path]; !ok || twice {
			return nil, fmt.Errorf("sheet revision path %s", f.Path)
		}
		if limit := maxFileBytes(p); f.Size < 0 || f.Size > limit {
			return nil, fmt.Errorf("sheet revision file %s is listed at %d bytes, more than the %d the writer writes", f.Path, f.Size, limit)
		}
		byPath[f.Path] = f
		if p.chunk {
			if rowBytes += f.Size; rowBytes > maxSheetFileBytes {
				return nil, fmt.Errorf("sheet revision row files are listed at more than the %d bytes of a sheet", int64(maxSheetFileBytes))
			}
			rowFiles[p.tab] = append(rowFiles[p.tab], chunkFile{f, p.first, p.part})
		}
	}
	// used counts the files the build reads. Every file of the list is one
	// of them, so that a rollback stores no file that its workbook does
	// not stand for.
	used := 0
	// load reads a file and checks that it is as large as the list says,
	// since the list goes into the outbox as it is, and no larger than the
	// writer writes a file at its path.
	load := func(f File) ([]byte, error) {
		p, _ := parseFilePath(f.Path)
		data, err := readFile(f, maxFileBytes(p), open, &readPace)
		if errors.Is(err, errRevisionStopped) {
			return nil, err
		}
		if err != nil {
			return nil, fmt.Errorf("sheet revision file %s: %w", f.Path, err)
		}
		used++
		return data, nil
	}
	get := func(path string) ([]byte, error) {
		f, ok := byPath[path]
		if !ok {
			return nil, fmt.Errorf("sheet revision lacks %s", path)
		}
		return load(f)
	}
	data, err := get(manifestPath)
	if err != nil {
		return nil, err
	}
	manifest, err := decodeManifest(data)
	if err != nil || manifest.Format != 1 || len(manifest.Tabs) == 0 {
		return nil, fmt.Errorf("sheet revision manifest cannot be read: %v", err)
	}
	wb := newWorkbook("")
	parsed := map[string]*formula.Formula{}
	extraChunks := 0
	for _, mt := range manifest.Tabs {
		if mt.ID == 0 || mt.ID >= maxID || wb.byID[TabID(mt.ID)] != nil || !validTabName(mt.Name) || wb.tab(mt.Name) != nil ||
			mt.FrozenRows < 0 || mt.FrozenRows > maxFrozenRows || mt.FrozenCols < 0 || mt.FrozenCols > maxFrozenCols {
			return nil, fmt.Errorf("sheet revision manifest holds a bad tab")
		}
		t := &Tab{id: TabID(mt.ID), name: mt.Name, frozenRows: mt.FrozenRows, frozenCols: mt.FrozenCols,
			widths: map[ColID]width{}, data: map[RowID]*rowData{}}
		data, err := get(axesPath(t.id))
		if err != nil {
			return nil, err
		}
		axes, err := decodeAxes(bytes.NewReader(data), &axesPace)
		if err != nil {
			if errors.Is(err, errRevisionStopped) {
				return nil, err
			}
			return nil, fmt.Errorf("sheet revision axes of tab %d: %w", t.id, err)
		}
		var nextRow, nextCol uint64
		if t.rows, nextRow, err = axisFromRuns(axes.Rows, formula.MaxRows, &axesPace); err != nil {
			return nil, err
		}
		if t.cols, nextCol, err = axisFromRuns(axes.Cols, formula.MaxCols, &axesPace); err != nil {
			return nil, err
		}
		for _, w := range axes.Widths {
			col, err := strconv.ParseUint(w.key, 10, 64)
			_, live := t.cols.pos[col]
			_, twice := t.widths[ColID(col)]
			if err != nil || !live || twice || strconv.FormatUint(col, 10) != w.key || w.px < minWidth || w.px > maxWidth {
				return nil, fmt.Errorf("sheet revision holds a bad width")
			}
			t.widths[ColID(col)] = width{px: w.px}
		}
		wb.nextRow, wb.nextCol = max(wb.nextRow, nextRow), max(wb.nextCol, nextCol)
		wb.nextTab = max(wb.nextTab, mt.ID+1)
		wb.placeTab(t, len(wb.tabs))
		chunks, err := chunkFiles(mt.ID, rowFiles[t.id], &listed)
		if err != nil {
			return nil, err
		}
		if extraChunks += len(chunks.firsts) - 1; extraChunks > maxExtraChunks {
			return nil, fmt.Errorf("sheet revision tabs hold more than the %d chunks beyond one for each tab that a sheet holds", maxExtraChunks)
		}
		for _, c := range chunks.files {
			data, err := load(c.f)
			if err != nil {
				return nil, err
			}
			if err := wb.loadRows(t, data, chunks.span(c.first), parsed, &rowsPace, pause); err != nil {
				if errors.Is(err, errRevisionStopped) {
					return nil, err
				}
				return nil, fmt.Errorf("sheet revision file %s: %w", c.f.Path, err)
			}
		}
		t.chunks = chunks.firsts
		if done, err := wb.checkChunks(t, nil, &rowsPace); !done {
			return nil, errRevisionStopped
		} else if err != nil {
			return nil, fmt.Errorf("sheet revision %w", err)
		}
	}
	if used != len(files) {
		return nil, fmt.Errorf("sheet revision lists files of no tab of its manifest")
	}
	wb.loaded()
	for _, f := range files {
		if !listed.step(1) {
			return nil, errRevisionStopped
		}
		wb.files.list[f.Path] = f
	}
	// The table gave out its ids from 1 on, so they come in order without
	// a sort.
	ids := make([]uint32, 0, len(wb.styles.byID))
	for id := uint64(1); id < wb.styles.next; id++ {
		if id%loadCheckCells == 0 && !pause("formats") {
			return nil, errRevisionStopped
		}
		if wb.styles.byID[uint32(id)] != nil {
			ids = append(ids, uint32(id))
		}
	}
	if !wb.indexStyles(ids, func() bool { return pause("formats") }) {
		return nil, errRevisionStopped
	}
	return wb, nil
}

// revisionChunks are the row files of one tab of a revision: each file
// with the first id of its chunk, and the first ids in order, 0 among
// them.
type revisionChunks struct {
	files  []chunkFile
	firsts []RowID
}

type chunkFile struct {
	f     File
	first RowID
	part  int
}

// span returns the ids the chunk that starts at first holds: from first
// up to the next chunk's first id, or to the end when next is 0.
func (c *revisionChunks) span(first RowID) [2]RowID {
	i, _ := slices.BinarySearch(c.firsts, first)
	if i+1 < len(c.firsts) {
		return [2]RowID{first, c.firsts[i+1]}
	}
	return [2]RowID{first, 0}
}

// chunkFiles orders the row files of a tab, whose paths parseFilePath
// read, of at most maxTabChunks chunks, before it reads any. The
// parts of a chunk are numbered from 0 on without a gap, so that a later
// change of the chunk replaces every part of it (chunkPaths), and each part
// but the last holds chunkPartSize bytes at least, as the writer ends a
// part only there. Each part takes the place its number names, without a
// sort, and a part is a step of pace each time it is looked at; the sort
// of the first ids, at most maxTabChunks, counts a step for each
// comparison it can make.
func chunkFiles(tab uint64, files []chunkFile, pace *loadPace) (revisionChunks, error) {
	out := revisionChunks{files: files}
	counts := map[RowID]int{0: 0}
	for _, c := range files {
		if !pace.step(1) {
			return out, errRevisionStopped
		}
		if _, ok := counts[c.first]; !ok && len(counts) == maxTabChunks {
			return out, fmt.Errorf("sheet revision tab %d has more than %d chunks", tab, maxTabChunks)
		}
		counts[c.first]++
	}
	parts := make(map[RowID][]*chunkFile, len(counts))
	for i := range files {
		if !pace.step(1) {
			return out, errRevisionStopped
		}
		c := &files[i]
		list := parts[c.first]
		if list == nil {
			list = make([]*chunkFile, counts[c.first])
			parts[c.first] = list
		}
		if c.part >= len(list) || list[c.part] != nil {
			return out, fmt.Errorf("sheet revision tab %d numbers the parts of the chunk %x with a gap or twice at %d", tab, uint64(c.first), c.part)
		}
		list[c.part] = c
	}
	for first := range counts {
		for i, c := range parts[first] {
			if !pace.step(1) {
				return out, errRevisionStopped
			}
			if i+1 < len(parts[first]) && c.f.Size < chunkPartSize {
				return out, fmt.Errorf("sheet revision tab %d has the part %d of %d bytes of the chunk %x", tab, c.part, c.f.Size, uint64(first))
			}
		}
		out.firsts = append(out.firsts, first)
	}
	if !pace.step(len(out.firsts) * bits.Len(uint(len(out.firsts)))) {
		return out, errRevisionStopped
	}
	slices.Sort(out.firsts)
	return out, nil
}

// errRevisionStopped ends a revision build whose pause reported false.
var errRevisionStopped = fmt.Errorf("sheet revision build stopped")

// axisFromRuns builds an axis from its runs, with a step of pace for each
// id, and returns the id after its highest.
func axisFromRuns(runs [][2]uint64, limit int, pace *loadPace) (axis, uint64, error) {
	a := axis{pos: map[uint64]int32{}}
	next := uint64(0)
	for _, run := range runs {
		// The ids of a run lie from 1 to maxID-1. The count is compared
		// with what is left below maxID, so that no sum can wrap.
		if run[0] == 0 || run[0] >= maxID || run[1] == 0 || run[1] > uint64(limit-len(a.ids)) || run[1] > maxID-run[0] {
			return a, 0, fmt.Errorf("sheet revision axes hold a bad run")
		}
		for i := uint64(0); i < run[1]; i++ {
			if !pace.step(1) {
				return a, 0, errRevisionStopped
			}
			id := run[0] + i
			if _, dup := a.pos[id]; dup {
				return a, 0, fmt.Errorf("sheet revision axes hold id %d twice", id)
			}
			a.pos[id] = int32(len(a.ids))
			a.ids = append(a.ids, id)
		}
		next = max(next, run[0]+run[1])
	}
	if len(a.ids) == 0 {
		return a, 0, fmt.Errorf("sheet revision axes are empty")
	}
	return a, next, nil
}

// loadRows loads one row file of a tab, whose rows lie in span, the ids
// of its chunk.
func (wb *Workbook) loadRows(t *Tab, data []byte, span [2]RowID, parsed map[string]*formula.Formula, rows *loadPace, pause func(phase string) bool) error {
	file, err := decodeRowFile(bytes.NewReader(data), rows)
	if err != nil {
		return err
	}
	// The writer lists each format its cells use once, and no other, so a
	// style of the file is a format and a cell uses it. The decode refused
	// a style that is no format or one listed before.
	styles := make([]uint32, len(file.Styles))
	used := make([]bool, len(file.Styles))
	for i, raw := range file.Styles {
		if i%loadCheckCells == loadCheckCells-1 && !pause("styles") {
			return errRevisionStopped
		}
		p, err := decodeFormatPatch(raw, true)
		if err != nil {
			return err
		}
		styles[i] = wb.styles.intern(styleState{f: p.f})
	}
	// A cell counts as its entry does in the decode (rawUnits): reading its
	// formula, measuring its text and counting its characters go through
	// at most a few times the bytes of the entry.
	cells := loadPace{pause: func() bool { return pause("cells") }}
	for _, row := range file.Rows {
		if _, live := t.rows.pos[row.ID]; !live {
			return fmt.Errorf("row %d is not on the axis", row.ID)
		}
		// A row in another chunk's file would stay there when its own
		// chunk is written next, and then be in two files.
		if id := RowID(row.ID); id < span[0] || (span[1] != 0 && id >= span[1]) {
			return fmt.Errorf("row %d is outside its chunk", row.ID)
		}
		for key, raw := range row.Cells {
			if !cells.step(rawUnits(raw)) {
				return errRevisionStopped
			}
			col, err := strconv.ParseUint(key, 10, 64)
			if _, live := t.cols.pos[col]; err != nil || !live || strconv.FormatUint(col, 10) != key {
				return fmt.Errorf("column %s is not on the axis", key)
			}
			fc, err := decodeCellEntry(raw)
			if err != nil {
				return err
			}
			c := &Cell{}
			pos := formula.Pos{Row: t.rows.pos[row.ID], Col: t.cols.pos[col]}
			if err := wb.decodeFileCell(c, &fc, pos, parsed); err != nil {
				return err
			}
			if fc.S != nil {
				if *fc.S < 0 || *fc.S >= len(styles) {
					return fmt.Errorf("style %d is not in the file", *fc.S)
				}
				c.style, used[*fc.S] = styles[*fc.S], true
			}
			// A file holds the cells with content or a format, and no
			// blank record (filledRows): such an entry would be kept
			// without a bound, since no tick of a revision collects it.
			if !wb.counted(c) {
				return fmt.Errorf("cell %d/%s holds no content and no format", row.ID, key)
			}
			// A cell comes once, so that it counts once.
			if t.cell(RowID(row.ID), ColID(col)) != nil {
				return fmt.Errorf("cell %d/%s twice", row.ID, key)
			}
			if err := wb.loadCell(t, RowID(row.ID), ColID(col), pos, c); err != nil {
				return err
			}
		}
	}
	if i := slices.Index(used, false); i >= 0 {
		return fmt.Errorf("style %d of the file is used by no cell", i)
	}
	return nil
}

// decodeCellEntry reads the entry of a cell in a row file, with each key
// once and in the case the engine writes it. The writer gives no key the
// value null, which would read as the key's absence, or as the number 0
// for v.
func decodeCellEntry(raw []byte) (fileCell, error) {
	var fc fileCell
	dec := json.NewDecoder(bytes.NewReader(raw))
	err := jsonObject(dec, jsonOnce([]string{"d", "dt", "f", "s", "t", "v", "x"}, func(i int) error {
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return err
		}
		if bytes.Equal(value, []byte("null")) {
			return fmt.Errorf("a cell entry with a null value")
		}
		switch i {
		case 0:
			return json.Unmarshal(value, &fc.D)
		case 1:
			return json.Unmarshal(value, &fc.DT)
		case 2:
			return json.Unmarshal(value, &fc.F)
		case 3:
			return json.Unmarshal(value, &fc.S)
		case 4:
			return json.Unmarshal(value, &fc.T)
		case 5:
			fc.V = value
			return nil
		}
		return json.Unmarshal(value, &fc.X)
	}))
	if err == nil {
		err = jsonDone(dec)
	}
	return fc, err
}

// decodeFileCell reads the entry of the cell at pos. A formula and a text
// meet the limits a typed one does (storedFormula, storedUnreadable,
// storedText). The writer gives an entry at most one content key, so an
// entry with more has two readings and is refused.
func (wb *Workbook) decodeFileCell(c *Cell, fc *fileCell, pos formula.Pos, parsed map[string]*formula.Formula) error {
	bad := fmt.Errorf("a cell entry cannot be read")
	keys := 0
	for _, set := range []bool{fc.D != nil, fc.DT != nil, fc.F != nil, fc.T != nil, fc.V != nil, fc.X != nil} {
		if set {
			keys++
		}
	}
	if keys > 1 {
		return bad
	}
	switch {
	case fc.F != nil:
		f, err := wb.storedFormula(*fc.F, pos, parsed)
		if err != nil {
			return err
		}
		c.f, c.content = f, formulaContent(f)
	case fc.X != nil:
		f, err := wb.storedUnreadable(*fc.X, pos)
		if err != nil {
			return err
		}
		c.f, c.content = f, formulaContent(f)
	case fc.D != nil:
		d, ok := formula.ParseDate(*fc.D)
		if !ok {
			return bad
		}
		c.content = formula.Content{Kind: formula.ContentDate, Num: float64(d)}
	case fc.T != nil:
		frac, ok := parseClock(*fc.T)
		if !ok {
			return bad
		}
		c.content = formula.Content{Kind: formula.ContentTime, Num: frac}
	case fc.DT != nil:
		day, clock, _ := strings.Cut(*fc.DT, "T")
		d, ok := formula.ParseDate(day)
		frac, okClock := parseClock(clock)
		if !ok || !okClock {
			return bad
		}
		c.content = formula.Content{Kind: formula.ContentDateTime, Num: float64(d) + frac}
	case fc.V != nil:
		v := bytes.TrimSpace(fc.V)
		switch {
		case bytes.Equal(v, []byte("true")):
			c.content = formula.Content{Kind: formula.ContentBool, Num: 1}
		case bytes.Equal(v, []byte("false")):
			c.content = formula.Content{Kind: formula.ContentBool}
		case len(v) > 0 && v[0] == '"':
			var s string
			if json.Unmarshal(v, &s) != nil {
				return bad
			}
			if err := storedText(s); err != nil {
				return err
			}
			c.content = formula.Content{Kind: formula.ContentText, Text: s}
		default:
			var n float64
			if json.Unmarshal(v, &n) != nil {
				return bad
			}
			c.content = formula.Content{Kind: formula.ContentNumber, Num: n}
		}
	}
	if c.f == nil {
		c.val = c.content.Value()
	}
	return nil
}

// storedFormula reads the stored formula of the cell at pos, once for
// each text through parsed, and holds it to the limits of a typed one
// (spec rules 4 and 9). ParseStored refuses a stored form longer than
// formula.MaxStoredBytes, which bounds the read, or nested deeper than 64
// levels, and LimitAt one whose text at the cell is longer than 8,192
// characters. The writer stores no other formula, since it refuses a write
// that would make a formula's text longer.
func (wb *Workbook) storedFormula(stored string, pos formula.Pos, parsed map[string]*formula.Formula) (*formula.Formula, error) {
	f, ok := parsed[stored]
	if !ok {
		var err error
		if f, err = formula.ParseStored(stored); err != nil {
			return nil, err
		}
		f = wb.intern(f)
		parsed[stored] = f
	}
	if err := f.LimitAt(pos); err != nil {
		return nil, err
	}
	return f, nil
}

// storedUnreadable reads the typed text of an unreadable formula stored
// for the cell at pos. It is the text of a formula as ParseTyped takes
// one, "=" and more, that the lexer cannot split. Text the lexer can split
// is a formula with references and no unreadable one: read as one it would
// refer to cells from another cell than its own, its text at its cell
// could be longer than a formula may be, and it would be stored again as a
// readable formula. An unreadable formula reads as it was typed at any
// cell, so the limits Parse holds it to are those of its text at the cell.
func (wb *Workbook) storedUnreadable(typed string, pos formula.Pos) (*formula.Formula, error) {
	if len(typed) < 2 || typed[0] != '=' {
		return nil, fmt.Errorf("an unreadable formula without its \"=\"")
	}
	f, err := formula.Parse(typed, pos, formula.Limits{})
	if err != nil {
		return nil, err
	}
	if !f.Unreadable() {
		return nil, fmt.Errorf("an unreadable formula that can be read")
	}
	return wb.intern(f), nil
}

// storedText refuses a stored text value longer than a typed one may be.
func storedText(s string) error {
	if len(s) <= formula.MaxTextLen {
		return nil
	}
	if n := utf8.RuneCountInString(s); n > formula.MaxTextLen {
		return fmt.Errorf("a text of %d characters", n)
	}
	return nil
}
