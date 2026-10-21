package engine

import (
	"fmt"
	"maps"
	"slices"

	"github.com/gosuda/flats/internal/board/model"
)

// Content is a whole board's content: a revision's, an import file's or a
// replacing agent call's.
type Content struct {
	Title    string
	Grid     bool
	Elements []model.Element
	// Assets holds the metadata of the images the content references, used
	// for asset rows the board no longer has (4.11 step 2.4).
	Assets map[string]Asset
}

// Reset kinds.
const (
	ResetRollback = "rollback"
	ResetReplace  = "replace"
)

// Rollback sets the board to revision of's content (4.11 step 2): elements
// alive now and in the revision keep their incarnation and change only the
// differing bundles; elements absent now come back with a new incarnation;
// the rest are deleted. Every session's undo history is cleared. Limits are
// not checked.
func (s *State) Rollback(of int64, c Content, now int64) (Outcome, error) {
	return s.reset(ResetRollback, of, c, "system", now, false)
}

// Replace replaces the board's content (4.12 step 2) after checking it as
// a whole: element forms, references, membership invariants and the
// per-item limits.
func (s *State) Replace(c Content, by string, now int64) (Outcome, error) {
	// The replacing content is written by its importer now, whatever
	// authorship its file claims.
	els := make([]model.Element, len(c.Elements))
	for i, el := range c.Elements {
		el.SetAuthor(model.EditorKind(by), "")
		els[i] = el
	}
	c.Elements = els
	if err := checkContent(c.Elements); err != nil {
		return Outcome{}, err
	}
	return s.reset(ResetReplace, 0, c, by, now, true)
}

func (s *State) reset(kind string, of int64, c Content, by string, now int64, limits bool) (Outcome, error) {
	a, m := s.newApplier("", by, now)
	if err := a.resetContent(c); err != nil {
		s.RollbackTo(m)
		return Outcome{}, err
	}
	if limits {
		if err := a.checkLimits(); err != nil {
			s.RollbackTo(m)
			return Outcome{}, err
		}
	}
	return a.commitReset(kind, of, m), nil
}

func (a *applier) commitReset(kind string, of int64, m Mark) Outcome {
	s := a.s
	for _, session := range slices.Sorted(maps.Keys(s.undo)) {
		s.deleteStacks(session)
	}
	d := a.delta()
	if d == nil {
		d = &model.Delta{Seq: a.seq, Changes: []model.Change{}}
	}
	d.Reset = &model.Reset{Kind: kind, Of: of}
	d.By = &model.By{Kind: "system", Label: kind}
	s.seq = a.seq
	s.resets, s.resetAt = append(s.resets, a.seq), append(s.resetAt, a.now)
	out := Outcome{Seq: a.seq, AckSeq: a.seq, Delta: d, Changed: s.changedSince(a.sp)}
	s.Release(m)
	return out
}

// resetContent makes the elements and metadata equal to c.
func (a *applier) resetContent(c Content) error {
	s := a.s
	next := make(map[string]model.Element, len(c.Elements))
	for _, el := range c.Elements {
		next[el.ID] = el
	}
	for _, el := range c.Elements {
		if el.Type != model.TypeImage {
			continue
		}
		if cur := s.assets[el.Asset]; cur == nil || cur.Mime == "" {
			meta, ok := c.Assets[el.Asset]
			if !ok {
				return fmt.Errorf("%w: no metadata for image asset %s", model.ErrFormat, el.Asset)
			}
			as := s.editAsset(el.Asset)
			as.Mime, as.W, as.H, as.Bytes = meta.Mime, meta.W, meta.H, meta.Bytes
		}
	}
	// The elements the content omits, and the last coordinate of each end
	// of the connectors among them, are taken from the board before any new
	// row is installed: a target id the content reuses for another type
	// would otherwise give the ends of a connector deleted here the new
	// element's geometry (4.4, 4.12).
	var gone []string
	last := map[string][2][2]float64{}
	for _, id := range slices.Sorted(maps.Keys(s.rows)) {
		r := s.rows[id]
		if _, keep := next[id]; keep || !r.alive() || id == model.BoardID {
			continue
		}
		gone = append(gone, id)
		if r.El.Type == model.TypeConnector {
			last[id] = [2][2]float64{a.endWorld(r.El.Ends[0]), a.endWorld(r.El.Ends[1])}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(next)) {
		a.resetElement(next[id])
	}
	a.deleteSetAt(gone, last)
	for _, id := range slices.Sorted(maps.Keys(next)) {
		r := s.rows[id]
		if r.El.Type != model.TypeConnector {
			continue
		}
		inc := r.EndInc
		for k, end := range r.El.Ends {
			inc[k] = 0
			if end.El != "" {
				inc[k] = s.rows[end.El].Inc
			}
		}
		if inc != r.EndInc {
			nr := r.clone()
			nr.EndInc = inc
			a.put(nr)
		}
	}
	board := s.rows[model.BoardID].clone()
	changed := false
	for g, val := range map[string]any{model.BundleTitle: c.Title, model.BundleGrid: c.Grid} {
		if !model.BundleEqual(g, board.El.Bundle(g), val) {
			a.setBundle(board, g, val)
			changed = true
		}
	}
	if changed {
		a.put(board)
	}
	return nil
}

// resetElement makes element el.ID equal to el. An alive element of the same
// type keeps its incarnation (4.11, 4.12); only a different type starts a new
// one. The fields outside every bundle (the author fields and the immutable
// pen points, image asset and natural size) follow the content too; when one
// of them changes, v.fixed becomes this seq, so the element's ver grows and
// a guard read before the reset no longer matches (8.1), and the element
// goes out whole with its incarnation (diffRow).
func (a *applier) resetElement(el model.Element) {
	cur := a.s.rows[el.ID]
	el.Normalize()
	a.useGroup(el.Group)
	if cur.alive() && cur.El.Type == el.Type {
		r := cur.clone()
		for _, g := range model.Bundles(el.Type) {
			if v := el.Bundle(g); !model.BundleEqual(g, r.El.Bundle(g), v) {
				a.setBundle(r, g, v)
			}
		}
		// A rollback's buckets equal the revision's files (rule 45: no new
		// stored content), so the unversioned fields follow it as well.
		r.El.By, r.El.At = el.By, el.At
		r.El.Pts, r.El.Asset, r.El.Natural = el.Pts, el.Asset, el.Natural
		r.El.TA, r.El.LK = el.TA, el.LK
		// Authorship is outside every bundle too: a change of it alone
		// raises v.fixed, so guards read before it fail and the element
		// goes out whole (8.1).
		if !sameUnversioned(r.El, cur.El) || !model.SameAuthorship(r.El, cur.El) {
			r.V[model.VFixed] = a.seq
		}
		if !maps.Equal(r.V, cur.V) {
			a.put(r)
		}
		return
	}
	r := &Row{El: el, Inc: a.seq, V: map[string]int64{}}
	for _, g := range model.Bundles(el.Type) {
		r.V[g] = a.seq
	}
	a.put(r)
}

// CheckContent checks a whole content set (9.2 element schema): element
// forms, unique ids, references to elements of the right type and the
// membership invariants.
func CheckContent(els []model.Element) error { return checkContent(els) }

func checkContent(els []model.Element) error {
	byID := make(map[string]model.Element, len(els))
	for _, el := range els {
		el.Normalize()
		if err := el.Validate(); err != nil {
			return err
		}
		if !model.ValidKey(el.Z) {
			return fmt.Errorf("%w: element %s needs a stacking key", model.ErrFormat, el.ID)
		}
		if _, dup := byID[el.ID]; dup {
			return fmt.Errorf("%w: element %s appears twice", model.ErrFormat, el.ID)
		}
		byID[el.ID] = el
	}
	groupFrame := map[string]string{}
	for _, el := range els {
		if el.Frame != "" && byID[el.Frame].Type != model.TypeFrame {
			return fmt.Errorf("%w: element %s names %s, which is not a frame", model.ErrFormat, el.ID, el.Frame)
		}
		for _, end := range el.Ends {
			if t, ok := byID[end.El]; end.El != "" && (!ok || t.Type == model.TypeConnector) {
				return fmt.Errorf("%w: connector %s ends at %s, which is not an element", model.ErrFormat, el.ID, end.El)
			}
		}
		if el.Group == "" {
			continue
		}
		if f, seen := groupFrame[el.Group]; seen && f != el.Frame {
			return fmt.Errorf("%w: members of group %s are in different frames", model.ErrFormat, el.Group)
		}
		groupFrame[el.Group] = el.Frame
	}
	return nil
}
