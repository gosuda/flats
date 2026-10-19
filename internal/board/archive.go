package board

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // DecodeConfig of revision assets
	_ "image/jpeg" // DecodeConfig of revision assets
	_ "image/png"  // DecodeConfig of revision assets
	"io"
	"math"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/board/model"
	_ "golang.org/x/image/webp" // DecodeConfig of revision assets
)

// Data file limits (9.2). The ratio applies to image entries only: a
// legal elements.json of repetitive text or pen points compresses far past
// any fixed ratio, so the JSON entries are bounded by the decompressed
// total alone.
const (
	archiveMaxEntries = 1024
	archiveMaxRatio   = 100
	// archiveRatioFloor lets tiny entries compress further than the ratio.
	archiveRatioFloor = 4096
	formatVersion     = 1
)

var assetPathRE = regexp.MustCompile(`^assets/([0-9a-f]{64})\.(png|jpg|gif|webp)$`)

// boardFile is board.json.
type boardFile struct {
	Format int    `json:"format"`
	Title  string `json:"title"`
	Grid   bool   `json:"grid"`
}

// recoveryFile is recovery.json (9.3).
type recoveryFile struct {
	Format        int      `json:"format"`
	CreatedAt     string   `json:"created_at"`
	Deleted       []any    `json:"deleted"`
	MissingAssets []string `json:"missing_assets"`
}

// archive is a checked data file.
type archive struct {
	board    boardFile
	elements []model.Element
	assets   map[string]AssetInfo
	data     map[string][]byte
	recovery *recoveryFile
	missing  map[string]bool
}

// parseArchive reads and checks a data file (9.2): at most 1,024 entries,
// the actual decompressed total at most maxTotal and a ratio of at most 100
// per image entry, only the known paths once each, asset names equal to their
// sha256, every image checked (10), the element schema and invariants, and
// every referenced asset present unless a recovery file lists it missing.
func parseArchive(r io.ReaderAt, size, maxTotal, uploadMax int64) (*archive, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, errorf(CategoryInvalid, "the file is not a board data file (zip): %v", err)
	}
	if len(zr.File) > archiveMaxEntries {
		return nil, errorf(CategoryInvalid, "a data file holds at most %d entries", archiveMaxEntries)
	}
	a := &archive{assets: map[string]AssetInfo{}, data: map[string][]byte{}, missing: map[string]bool{}}
	seen := map[string]bool{}
	var total int64
	var boardRaw, elementsRaw, recoveryRaw []byte
	for _, f := range zr.File {
		name := f.Name
		if strings.Contains(name, "..") {
			return nil, errorf(CategoryInvalid, "the data file holds a path containing \"..\"")
		}
		if seen[name] {
			return nil, errorf(CategoryInvalid, "the data file holds %s twice", name)
		}
		seen[name] = true
		m := assetPathRE.FindStringSubmatch(name)
		if name != "board.json" && name != "elements.json" && name != "recovery.json" && m == nil {
			return nil, errorf(CategoryInvalid, "the data file holds an unexpected path %q", name)
		}
		limit := maxTotal - total
		if m != nil && f.CompressedSize64 <= (math.MaxInt64-archiveRatioFloor)/archiveMaxRatio {
			limit = min(limit, int64(f.CompressedSize64)*archiveMaxRatio+archiveRatioFloor)
		}
		data, err := readEntry(f, limit)
		if err != nil {
			return nil, err
		}
		total += int64(len(data))
		switch {
		case name == "board.json":
			boardRaw = data
		case name == "elements.json":
			elementsRaw = data
		case name == "recovery.json":
			recoveryRaw = data
		default:
			info, err := CheckImage(data, uploadMax)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			if info.Hash != m[1] {
				return nil, errorf(CategoryInvalid, "%s does not match its content hash", name)
			}
			if assetExt(info.Mime) != m[2] {
				return nil, errorf(CategoryInvalid, "%s is not a %s image", name, m[2])
			}
			a.assets[info.Hash], a.data[info.Hash] = info, data
		}
	}
	if boardRaw == nil || elementsRaw == nil {
		return nil, errorf(CategoryInvalid, "a data file needs board.json and elements.json")
	}
	if err := strictJSON(boardRaw, &a.board); err != nil {
		return nil, errorf(CategoryInvalid, "board.json: %v", err)
	}
	if a.board.Format < 1 || a.board.Format > formatVersion {
		return nil, errorf(CategoryInvalid, "data file format %d is not supported", a.board.Format)
	}
	if model.TextLen(a.board.Title) > model.MaxTitle {
		return nil, errorf(CategoryInvalid, "the title is longer than %d characters", model.MaxTitle)
	}
	if recoveryRaw != nil {
		a.recovery = &recoveryFile{}
		if err := strictJSON(recoveryRaw, a.recovery); err != nil {
			return nil, errorf(CategoryInvalid, "recovery.json: %v", err)
		}
		for _, h := range a.recovery.MissingAssets {
			if !model.ValidAsset(h) {
				return nil, errorf(CategoryInvalid, "recovery.json names an invalid asset %q", h)
			}
			a.missing[h] = true
		}
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(elementsRaw, &raws); err != nil {
		return nil, errorf(CategoryInvalid, "elements.json is not an array of elements: %v", err)
	}
	for i, raw := range raws {
		if a.recovery != nil {
			raw = withoutField(raw, "recovered_from")
		}
		el, _, err := model.DecodeElement(raw)
		if err != nil {
			return nil, errorf(CategoryInvalid, "element %d: %v", i, err)
		}
		a.elements = append(a.elements, el)
	}
	if err := engine.CheckContent(a.elements); err != nil {
		return nil, errorf(CategoryInvalid, "%v", err)
	}
	for _, el := range a.elements {
		if el.Type != model.TypeImage {
			continue
		}
		if _, ok := a.assets[el.Asset]; !ok && !a.missing[el.Asset] {
			return nil, errorf(CategoryInvalid, "image %s references asset %s, which the file does not hold", el.ID, el.Asset)
		}
	}
	return a, nil
}

// readEntry decompresses one entry, refusing more than limit bytes; the
// declared sizes are not trusted.
func readEntry(f *zip.File, limit int64) ([]byte, error) {
	if f.Mode().IsDir() || strings.HasSuffix(f.Name, "/") {
		return nil, errorf(CategoryInvalid, "the data file holds a directory %q", f.Name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, errorf(CategoryInvalid, "%s cannot be read: %v", f.Name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil && !errors.Is(err, zip.ErrChecksum) {
		return nil, errorf(CategoryInvalid, "%s cannot be read: %v", f.Name, err)
	}
	if int64(len(data)) > limit {
		return nil, errorf(CategoryInvalid, "%s is too large or too highly compressed", f.Name)
	}
	if err != nil {
		return nil, errorf(CategoryInvalid, "%s is damaged: %v", f.Name, err)
	}
	return data, nil
}

func strictJSON(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

// withoutField removes one top-level key of a JSON object.
func withoutField(raw json.RawMessage, key string) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	if _, ok := m[key]; !ok {
		return raw
	}
	delete(m, key)
	out, _ := json.Marshal(m)
	return out
}

// convertRecovery applies the recovery file rules (9.3): images whose bytes
// are missing become text elements, elements that are not frames and have
// no frame go into a new "Recovered edits" frame, and a group stays only
// when all its members are in the same place.
func convertRecovery(a *archive, now time.Time) {
	if a.recovery == nil {
		return
	}
	els := a.elements
	for i, el := range els {
		if el.Type == model.TypeImage && a.missing[el.Asset] {
			text := "Could not recover image: " + el.Alt
			els[i] = model.Element{ID: el.ID, Type: model.TypeText, Geom: el.Geom, Z: el.Z, Frame: el.Frame, Group: el.Group,
				Locked: el.Locked, Text: text, Style: model.DefaultStyle(model.TypeText)}
		}
	}
	byID := map[string]model.Element{}
	minZ := ""
	for _, el := range els {
		byID[el.ID] = el
		if minZ == "" || el.Z < minZ {
			minZ = el.Z
		}
	}
	var loose []int
	box := model.Geom{}
	first := true
	for i, el := range els {
		if el.Type == model.TypeFrame || el.Frame != "" {
			continue
		}
		loose = append(loose, i)
		b := bounds(el, byID)
		if first {
			box, first = b, false
			continue
		}
		x0, y0 := math.Min(box.X, b.X), math.Min(box.Y, b.Y)
		x1, y1 := math.Max(box.X+box.W, b.X+b.W), math.Max(box.Y+box.H, b.Y+b.H)
		box = model.Geom{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
	}
	if len(loose) > 0 {
		z, _ := model.KeyBetween("", minZ)
		frame := model.Element{ID: newID("e_"), Type: model.TypeFrame, Z: z,
			Geom: model.Geom{X: box.X - 32, Y: box.Y - 32, W: math.Max(box.W+64, 1), H: math.Max(box.H+64, 1)},
			Text: "Recovered edits (" + now.UTC().Format("2006-01-02 15:04") + " UTC)", Style: model.DefaultStyle(model.TypeFrame)}
		frame.Geom.W, frame.Geom.H = math.Min(frame.Geom.W, model.MaxSize), math.Min(frame.Geom.H, model.MaxSize)
		frame.Normalize()
		for _, i := range loose {
			els[i].Frame = frame.ID
		}
		els = append(els, frame)
	}
	place := map[string]string{}
	split := map[string]bool{}
	for _, el := range els {
		if el.Group == "" {
			continue
		}
		if f, ok := place[el.Group]; ok && f != el.Frame {
			split[el.Group] = true
		}
		place[el.Group] = el.Frame
	}
	for i := range els {
		if split[els[i].Group] {
			els[i].Group = ""
		}
	}
	a.elements = els
}

// copyOps builds the agent operations that copy els with new ids (4.4
// relinking, shared by data file import with mode=add and element
// recovery): references inside the set point to the copies, connector ends
// attached outside it become free ends at the target's coordinate at that
// moment (outside looks targets up), membership outside it ends and groups
// get new ids. The copies keep their stacking order above top and move by
// dx, dy. It returns the operations and the new ids in input order.
func copyOps(els []model.Element, outside func(id string) (model.Element, bool), dx, dy float64, top string) ([]engine.AgentOp, []string) {
	set := map[string]model.Element{}
	newIDs := map[string]string{}
	for _, el := range els {
		set[el.ID] = el
		newIDs[el.ID] = newID("e_")
	}
	order := slices.Clone(els)
	slices.SortFunc(order, func(a, b model.Element) int {
		if c := strings.Compare(a.Z, b.Z); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	keys := model.Rekey(len(order))
	shift := func(v, d float64) float64 {
		return model.Round1(math.Max(-model.MaxCoord, math.Min(model.MaxCoord, v+d)))
	}
	copies := make([]model.Element, len(order))
	groups := map[string][]string{}
	groupFrame := map[string]map[string]bool{}
	for i, el := range order {
		c := el.Clone()
		c.ID, c.Z, c.Locked, c.By, c.At = newIDs[el.ID], top+keys[i], false, "", 0
		c.Frame, c.Group = newIDs[el.Frame], ""
		if c.Type == model.TypeConnector {
			for k, end := range c.Ends {
				switch {
				case end.El == "":
					c.Ends[k].Pt = [2]float64{shift(end.Pt[0], dx), shift(end.Pt[1], dy)}
				case set[end.El].ID != "":
					c.Ends[k].El = newIDs[end.El]
				default:
					p := end.Pt
					if t, ok := outside(end.El); ok {
						p = model.EndpointWorld(t.Geom, end.At)
					}
					c.Ends[k] = model.End{Pt: [2]float64{shift(p[0], dx), shift(p[1], dy)}}
				}
			}
		} else {
			c.Geom.X, c.Geom.Y = shift(c.Geom.X, dx), shift(c.Geom.Y, dy)
		}
		if el.Group != "" && el.Type != model.TypeFrame {
			groups[el.Group] = append(groups[el.Group], c.ID)
			if groupFrame[el.Group] == nil {
				groupFrame[el.Group] = map[string]bool{}
			}
			groupFrame[el.Group][c.Frame] = true
		}
		copies[i] = c
	}
	var ops []engine.AgentOp
	add := func(pick func(model.Element) bool) {
		for _, c := range copies {
			if pick(c) {
				ops = append(ops, engine.AgentOp{Op: engine.OpAdd, El: c.CanonicalJSON()})
			}
		}
	}
	add(func(c model.Element) bool { return c.Type == model.TypeFrame })
	add(func(c model.Element) bool { return c.Type != model.TypeFrame && c.Type != model.TypeConnector })
	add(func(c model.Element) bool { return c.Type == model.TypeConnector })
	for _, g := range sortedKeys(groups) {
		if members := groups[g]; len(members) >= 2 && len(groupFrame[g]) == 1 {
			ops = append(ops, engine.AgentOp{Op: engine.OpGroup, IDs: members, Group: newID("g_")})
		}
	}
	for _, el := range order {
		if el.Locked {
			ops = append(ops, engine.AgentOp{Op: engine.OpSet, ID: newIDs[el.ID], Fields: json.RawMessage(`{"locked":true}`)})
		}
	}
	ids := make([]string, len(els))
	for i, el := range els {
		ids[i] = newIDs[el.ID]
	}
	return ops, ids
}

// topKey is the highest stacking key of the board's alive elements.
func topKey(st *engine.State) string {
	top := ""
	for _, e := range st.Alive() {
		top = max(top, e.Z)
	}
	return top
}

// boardBox is the bounding box of the board's alive elements.
func boardBox(els []model.Element) (model.Geom, bool) {
	byID := map[string]model.Element{}
	for _, e := range els {
		byID[e.ID] = e
	}
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, e := range els {
		b := bounds(e, byID)
		minX, minY = math.Min(minX, b.X), math.Min(minY, b.Y)
		maxX, maxY = math.Max(maxX, b.X+b.W), math.Max(maxY, b.Y+b.H)
	}
	if math.IsInf(minX, 1) {
		return model.Geom{}, false
	}
	return model.Geom{X: minX, Y: minY, W: maxX - minX, H: maxY - minY}, true
}

// ImportRequest is an import_board call or a POST /_board/api/import.
type ImportRequest struct {
	OpID    string
	Mode    string // add or replace
	Archive io.ReaderAt
	Size    int64
	Label   string
	// Person marks an import a person started in the browser; its delta
	// and new elements are credited to Key instead of an agent.
	Person bool
	Key    string
}

// ImportResult is a committed import.
type ImportResult struct {
	Seq       int64    `json:"seq"`
	IDs       []string `json:"ids"`
	Assets    []string `json:"assets,omitempty"`
	OutboxSeq int64    `json:"outbox_seq"`
	Replayed  bool     `json:"-"`
}

// ImportBoard imports a data file (9.2) as one apply-loop item: mode=add
// copies its elements with new ids to the right of the board, mode=replace
// replaces the board (4.12). Core holds the flat lock and passes its fresh
// measurement of "other".
func (in *Instance) ImportBoard(ctx context.Context, req ImportRequest, other int64) (ImportResult, error) {
	prep, err := in.prepareImport(ctx, req)
	if err != nil {
		return ImportResult{}, err
	}
	return in.applyImport(ctx, prep, other)
}

// preparedImport is a checked data file. Its images are stored only when
// applying admits it.
type preparedImport struct {
	req    ImportRequest
	a      *archive
	assets []AssetInfo
	hash   string
}

// prepareImport runs every check of 9.2 before the flat lock is taken.
func (in *Instance) prepareImport(ctx context.Context, req ImportRequest) (*preparedImport, error) {
	if err := checkOpID(req.OpID); err != nil {
		return nil, err
	}
	if req.Mode != "add" && req.Mode != "replace" {
		return nil, errorf(CategoryInvalid, "mode is add or replace")
	}
	lim := in.b.Limits()
	a, err := parseArchive(req.Archive, req.Size, max(lim.UploadMax, 1)*4, lim.UploadMax)
	if err != nil {
		return nil, err
	}
	if a.recovery != nil && req.Mode != "add" {
		return nil, errorf(CategoryInvalid, "a recovery file is imported with mode=add")
	}
	convertRecovery(a, in.now())
	if err := engine.CheckContent(a.elements); err != nil {
		return nil, errorf(CategoryInvalid, "%v", err)
	}
	p := &preparedImport{req: req, a: a}
	for _, h := range sortedKeys(a.assets) {
		p.assets = append(p.assets, a.assets[h])
	}
	p.hash = sha256Hex(model.Canonical(map[string]any{"mode": req.Mode, "archive": archiveHash(req.Archive, req.Size)}))
	return p, nil
}

// applyImport stores the images and applies the import under the flat
// lock. A dry run admits it first, so a refused import stores no blob
// (5.4).
func (in *Instance) applyImport(ctx context.Context, p *preparedImport, other int64) (ImportResult, error) {
	mark := in.meas.mark()
	in.meas.install(ctx, other, mark)
	blobs := make([]newBlob, len(p.assets))
	for i, info := range p.assets {
		blobs[i] = newBlob{info, p.a.data[info.Hash]}
	}
	store, release, err := in.admitBlobs(ctx, blobs, func(t *txn) error {
		var res ImportResult
		if done, err := in.replay(p.req.OpID, p.hash, &res); done || err != nil {
			return cmp.Or(err, errReplay)
		}
		return asError(in.importItem(t, p.req, p.hash, p.a, p.assets, &res))
	})
	defer release()
	if err != nil {
		return ImportResult{}, err
	}
	if store {
		if err := in.putBlobs(ctx, blobs, true); err != nil {
			return ImportResult{}, err
		}
	}
	release() // the stored blobs count from here on
	var res ImportResult
	err = in.do(ctx, func() error {
		if done, err := in.replay(p.req.OpID, p.hash, &res); done || err != nil {
			return err
		}
		t, err := in.begin()
		if err != nil {
			return err
		}
		if err := in.importItem(t, p.req, p.hash, p.a, p.assets, &res); err != nil {
			t.rollback()
			return asError(err)
		}
		return t.commit()
	})
	return res, err
}

func archiveHash(r io.ReaderAt, size int64) string {
	h := sha256.New()
	_, _ = io.Copy(h, io.NewSectionReader(r, 0, size))
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (in *Instance) importItem(t *txn, req ImportRequest, reqHash string, a *archive, assets []AssetInfo, res *ImportResult) error {
	st := in.st
	now := in.now().Unix()
	if err := in.holdAssets(t, assets, now); err != nil {
		return err
	}
	if err := in.agentBoundary(t); err != nil {
		return err
	}
	for _, info := range assets {
		res.Assets = append(res.Assets, info.Hash)
	}
	by := &model.By{Kind: "agent", Label: req.Label}
	if req.Person {
		by.Kind = "person"
	}
	var changed engine.Changed
	var delta *model.Delta
	if req.Mode == "replace" {
		meta := map[string]engine.Asset{}
		for h, info := range a.assets {
			meta[h] = engine.Asset{Hash: h, Mime: info.Mime, W: info.W, H: info.H, Bytes: info.Bytes}
		}
		creator := req.Key
		if creator == "" {
			creator = "agent:" + req.Label
		}
		out, err := st.Replace(engine.Content{Title: a.board.Title, Grid: a.board.Grid, Elements: a.elements, Assets: meta}, creator, now)
		if err != nil {
			return err
		}
		changed, delta = out.Changed, out.Delta
		for _, el := range a.elements {
			res.IDs = append(res.IDs, el.ID)
		}
		res.Seq = out.Seq
	} else {
		var dx, dy float64
		alive := st.Alive()
		cur := make([]model.Element, len(alive))
		for i, e := range alive {
			cur[i] = e.Element
		}
		if box, ok := boardBox(cur); ok {
			if src, ok := boardBox(a.elements); ok {
				dx, dy = box.X+box.W+64-src.X, box.Y-src.Y
			}
		}
		ops, ids := copyOps(a.elements, func(string) (model.Element, bool) { return model.Element{}, false }, dx, dy, topKey(st))
		out, err := st.ApplyAgent(engine.AgentCall{Ops: ops, Label: req.Label, By: req.Key, Unlimited: true, Now: now, NewID: newID})
		if err != nil {
			return err
		}
		changed, delta, res.IDs, res.Seq = out.Changed, out.Delta, ids, out.Seq
	}
	if delta != nil {
		delta.By = by
	}
	if res.Seq == 0 {
		res.Seq = st.Seq()
	}
	if err := in.commitAgentChange(t, changed, delta, recordSpec{kind: "import", forced: true, all: req.Mode == "replace",
		authors: []string{req.Label}, op: req.OpID}, &res.OutboxSeq); err != nil {
		return err
	}
	if err := t.putOp(req.OpID, "import", reqHash, res.OutboxSeq, res); err != nil {
		return err
	}
	return in.checkQuota(t)
}

// ContentFromFiles reads a revision's content from its file list (5.2):
// board.json, the element buckets and the asset metadata, read from the
// blobs' headers, that a rollback needs for asset rows the board no longer
// has (4.11 step 2.4). An asset blob is read only when the host has not
// learned its pixel size yet (dimCache); the byte size is the file list's.
func ContentFromFiles(files []File, read func(hash string) ([]byte, error)) (engine.Content, error) {
	c := engine.Content{Grid: true, Assets: map[string]engine.Asset{}}
	for _, f := range files {
		switch {
		case f.Path == "board.json":
			data, err := read(f.Hash)
			if err != nil {
				return c, err
			}
			var b boardFile
			if err := json.Unmarshal(data, &b); err != nil {
				return c, fmt.Errorf("board.json: %w", err)
			}
			c.Title, c.Grid = b.Title, b.Grid
		case strings.HasPrefix(f.Path, "elements/"):
			data, err := read(f.Hash)
			if err != nil {
				return c, err
			}
			var bucket map[string]json.RawMessage
			if err := json.Unmarshal(data, &bucket); err != nil {
				return c, fmt.Errorf("%s: %w", f.Path, err)
			}
			for _, id := range sortedKeys(bucket) {
				el, _, err := model.DecodeElement(bucket[id])
				if err != nil {
					return c, fmt.Errorf("%s: %w", f.Path, err)
				}
				c.Elements = append(c.Elements, el)
			}
		case strings.HasPrefix(f.Path, "assets/"):
			m := assetPathRE.FindStringSubmatch(f.Path)
			if m == nil {
				continue
			}
			// A blob never changes: its stored size and a pixel size learned
			// earlier (a check, an asset row, an earlier read) need no read.
			if d, ok := knownDims(m[1]); ok && f.Size > 0 {
				c.Assets[m[1]] = engine.Asset{Hash: m[1], Mime: extMime(m[2]), W: d[0], H: d[1], Bytes: f.Size}
				continue
			}
			data, err := read(f.Hash)
			if err != nil {
				return c, err
			}
			cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil {
				return c, fmt.Errorf("%s: %w", f.Path, err)
			}
			rememberDims(m[1], cfg.Width, cfg.Height)
			c.Assets[m[1]] = engine.Asset{Hash: m[1], Mime: extMime(m[2]), W: cfg.Width, H: cfg.Height, Bytes: int64(len(data))}
		}
	}
	slices.SortFunc(c.Elements, func(a, b model.Element) int { return strings.Compare(a.ID, b.ID) })
	return c, nil
}

// exportData is the current content for a data file: board.json, the
// elements (private: content fields; public: the 7.3 projection) and the
// assets alive elements reference. The assets are leased in the apply loop
// where they are chosen, before any later edit can drop their rows, and
// stay leased until the caller calls release (5.5).
func (in *Instance) exportData(ctx context.Context, public bool) (boardJSON []byte, elements []json.RawMessage, assets []engine.Asset, release func(), err error) {
	// The loop can still run fn after do gave up on a stopping instance, so
	// fn publishes its results under mu and releases a late lease itself.
	var mu sync.Mutex
	abandoned := false
	release = func() {}
	err = in.do(ctx, func() error {
		board := in.st.Board().Element
		bj := model.Canonical(boardFile{Format: formatVersion, Title: board.Text, Grid: board.Grid})
		var els []json.RawMessage
		for _, e := range in.st.Alive() {
			if public {
				els = append(els, model.Canonical(model.PublicElement(e.Element)))
			} else {
				els = append(els, e.CanonicalJSON())
			}
		}
		as := in.assets.live(nil)
		hashes := make([]string, len(as))
		for i, a := range as {
			hashes[i] = a.Hash
		}
		rel := in.b.Lease(hashes)
		mu.Lock()
		defer mu.Unlock()
		if abandoned {
			rel()
			return nil
		}
		boardJSON, elements, assets, release = bj, els, as, rel
		return nil
	})
	mu.Lock()
	defer mu.Unlock()
	if err != nil {
		abandoned = true
		release()
		return nil, nil, nil, func() {}, err
	}
	return boardJSON, elements, assets, release, nil
}

// writeArchive writes a data file (9.1).
func writeArchive(ctx context.Context, w io.Writer, boardJSON []byte, elements []json.RawMessage, assets []engine.Asset, read func(hash string) ([]byte, error)) error {
	zw := zip.NewWriter(w)
	put := func(name string, data []byte, method uint16) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method, Modified: time.Now().UTC()})
		if err != nil {
			return err
		}
		_, err = fw.Write(data)
		return err
	}
	if err := put("board.json", boardJSON, zip.Deflate); err != nil {
		return err
	}
	var els bytes.Buffer
	els.WriteByte('[')
	for i, e := range elements {
		if i > 0 {
			els.WriteByte(',')
		}
		els.Write(e)
	}
	els.WriteByte(']')
	if err := put("elements.json", els.Bytes(), zip.Deflate); err != nil {
		return err
	}
	for _, a := range assets {
		data, err := read(a.Hash)
		if err != nil {
			return err
		}
		if err := put(path.Join("assets", a.Hash+"."+assetExt(a.Mime)), data, zip.Store); err != nil {
			return err
		}
	}
	return zw.Close()
}
