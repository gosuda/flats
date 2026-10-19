package board

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/board/model"
)

// richContent is one board with every element type and non-default values
// of every field: styles, a group, a lock, frame membership, connector
// labels with attached and free ends, pen points and an image with
// alternative text.
func richContent(t *testing.T) ([]model.Element, map[string][]byte) {
	t.Helper()
	img := pngBytes(t, 7, 5)
	hash := sha256Hex(img)
	keys := model.Rekey(8)
	frame, sticky, text, shape, conn, pen, pic, free := eid(), eid(), eid(), eid(), eid(), eid(), eid(), eid()
	group := "g_AbCdEfGh12"
	els := []model.Element{
		{ID: frame, Type: model.TypeFrame, Geom: model.Geom{X: -100, Y: -100, W: 3000, H: 2000}, Z: keys[0], Text: "Frame title", Style: model.Style{"fill": "gray"}},
		{ID: sticky, Type: model.TypeSticky, Geom: model.Geom{X: 10, Y: 20, W: 220, H: 180, Rot: 15}, Z: keys[1], Frame: frame, Group: group, Locked: true,
			Text: "Sticky 한글 <b>&", Style: model.Style{"color": "pink", "fontSize": 24.0}},
		{ID: text, Type: model.TypeText, Geom: model.Geom{X: 400, Y: 20, W: 300, H: 60}, Z: keys[2], Frame: frame, Group: group,
			Text: "Line one\nLine two", Style: model.Style{"color": "purple", "fontSize": 36.0, "align": "right"}},
		{ID: shape, Type: model.TypeShape, Geom: model.Geom{X: 800, Y: 300, W: 200, H: 120, Rot: 329.5}, Z: keys[3], Frame: frame,
			Text: "Decision?", Style: model.Style{"shape": "diamond", "fill": "none", "stroke": "red", "strokeWidth": 4.0, "dash": "dotted", "fontSize": 18.0}},
		{ID: conn, Type: model.TypeConnector, Z: keys[4], Text: "yes",
			Ends:  [2]model.End{{El: sticky, At: [2]float64{1, 0.5}}, {El: shape, At: [2]float64{0, 0.3}}},
			Style: model.Style{"stroke": "blue", "strokeWidth": 1.0, "dash": "dashed", "route": "elbow", "head0": "arrow", "head1": "none", "fontSize": 16.0}},
		{ID: free, Type: model.TypeConnector, Z: keys[5],
			Ends:  [2]model.End{{El: text, At: [2]float64{0.5, 1}}, {Pt: [2]float64{5000.5, -42}}},
			Style: model.DefaultStyle(model.TypeConnector)},
		{ID: pen, Type: model.TypePen, Geom: model.Geom{X: 50, Y: 600, W: 400, H: 100}, Z: keys[6], Frame: frame,
			Pts: []int{0, 0, 2500, 10000, 5000, 0, 7500, 10000, 10000, 5000}, Style: model.Style{"stroke": "highlight", "width": "marker"}},
		{ID: pic, Type: model.TypeImage, Geom: model.Geom{X: 1200, Y: 600, W: 70, H: 50}, Z: keys[7],
			Asset: hash, Natural: [2]int{7, 5}, Alt: "a small red dot"},
	}
	for i := range els {
		if els[i].Style == nil {
			els[i].Style = model.DefaultStyle(els[i].Type)
		}
	}
	return els, map[string][]byte{"assets/" + hash + ".png": img}
}

// contentOf maps id to the canonical content JSON of elements without the
// author fields, which a copy rewrites.
func contentOf(els []model.Element, keepAuthor bool) map[string]string {
	out := map[string]string{}
	for _, e := range els {
		c := e.Content()
		if !keepAuthor {
			delete(c, "by")
			delete(c, "at")
		}
		out[e.ID] = string(model.Canonical(c))
	}
	return out
}

func aliveElements(b *tb) []model.Element {
	var out []model.Element
	b.state(func(st *engine.State) {
		for _, e := range st.Alive() {
			out = append(out, e.Element)
		}
	})
	return out
}

// TestEveryValueOfARevisionIsRecoverable: every element, value and asset of
// a retained revision can be viewed, rolled back to and copied back, after
// the board deleted all of it (board.md Success criteria, Recovery).
func TestEveryValueOfARevisionIsRecoverable(t *testing.T) {
	ctx := context.Background()
	b := newTB(t)
	els, assets := richContent(t)
	data := buildArchive(t, boardFile{Format: 1, Title: "Rich", Grid: false}, els, assets, nil)
	err := b.core.Exclusive(ctx, func(ctx context.Context, other int64) error {
		_, err := b.in.ImportBoard(ctx, ImportRequest{OpID: "rich-import-1", Mode: "replace", Archive: strings.NewReader(string(data)), Size: int64(len(data)), Label: "Agent"}, other)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := aliveElements(b)
	if len(ref) != len(els) {
		t.Fatalf("%d elements imported of %d", len(ref), len(els))
	}
	// The import kept every value of the data file but the author fields.
	if got, want := contentOf(ref, false), contentOf(els, false); !mapsEqualStr(got, want) {
		t.Fatalf("imported content differs:\n%v\n%v", got, want)
	}
	rev := b.core.lastRev()
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()

	// Delete everything (unlock first), and let the revision record it.
	var incs = map[string]any{}
	var ids []string
	b.state(func(st *engine.State) {
		for _, e := range st.Alive() {
			incs[e.ID] = e.Inc
			ids = append(ids, e.ID)
		}
	})
	var lockedID string
	for _, e := range ref {
		if e.Locked {
			lockedID = e.ID
		}
	}
	if m := a.batch(1, map[string]any{"op": "set", "id": lockedID, "inc": incs[lockedID], "g": "locked", "val": false}); m["t"] != "ack" {
		t.Fatal(m)
	}
	if m := a.batch(2, map[string]any{"op": "delete", "ids": ids, "inc_of": incs}); m["t"] != "ack" {
		t.Fatal(m)
	}
	if n := len(b.alive()); n != 0 {
		t.Fatalf("%d elements left after deleting everything", n)
	}
	b.clock.Add(31 * time.Second)
	if err := b.in.do(ctx, b.in.tick); err != nil {
		t.Fatal(err)
	}
	if err := b.core.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	// Purge the deleted rows and their asset rows, so only the revision
	// holds the content.
	b.clock.Add(2 * time.Hour)
	b.apply(func(st *engine.State) engine.Changed { return st.TrimDeleted(0) })
	b.apply(func(st *engine.State) engine.Changed { return st.SweepAssets(b.clock.Now().Unix()) })

	// View: the revision's elements, every value with its author fields.
	r, err := http.Get(b.priv.URL + "/_board/api/revisions/" + itoa(rev) + "/elements")
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("revision elements %v %v", r.StatusCode, err)
	}
	var page struct {
		Elements []json.RawMessage `json:"elements"`
		Meta     map[string]any    `json:"meta"`
	}
	json.NewDecoder(r.Body).Decode(&page)
	r.Body.Close()
	viewed := map[string]string{}
	for _, raw := range page.Elements {
		var m map[string]any
		json.Unmarshal(raw, &m)
		viewed[m["id"].(string)] = string(model.Canonical(m))
	}
	if want := contentOf(ref, true); !mapsEqualStr(viewed, want) {
		t.Fatalf("revision view differs:\n%v\n%v", viewed, want)
	}
	if page.Meta["title"] != "Rich" || page.Meta["grid"] != false {
		t.Fatalf("revision meta %v", page.Meta)
	}
	// The asset is viewable from the revision alone.
	for name, blob := range assets {
		hash := strings.TrimSuffix(strings.TrimPrefix(name, "assets/"), ".png")
		resp, err := http.Get(b.priv.URL + "/_board/a/" + hash)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("revision-only asset %v %v", resp.StatusCode, err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !bytes.Equal(got, blob) {
			t.Fatal("the revision-only asset bytes differ")
		}
	}

	// Recover: copies of every element with new ids and the same values;
	// relations among the copied elements follow the new ids.
	body, _ := json.Marshal(map[string]any{"revision": rev, "ids": ids})
	resp, m := b.post(b.priv, "/_board/api/recover", "application/json", bytes.NewReader(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recover %d %v", resp.StatusCode, m)
	}
	newIDs := map[string]string{}
	for i, id := range m["ids"].([]any) {
		newIDs[ids[i]] = id.(string)
	}
	copied := map[string]model.Element{}
	for _, e := range aliveElements(b) {
		copied[e.ID] = e
	}
	if len(copied) != len(ref) {
		t.Fatalf("%d elements recovered of %d", len(copied), len(ref))
	}
	for _, e := range ref {
		c, ok := copied[newIDs[e.ID]]
		if !ok {
			t.Fatalf("%s was not recovered", e.ID)
		}
		want := e.Clone()
		want.ID = c.ID
		if want.Frame != "" {
			want.Frame = newIDs[want.Frame]
		}
		for k := range want.Ends {
			if want.Ends[k].El != "" {
				want.Ends[k].El = newIDs[want.Ends[k].El]
			}
		}
		// A recovered copy is a new group and a fresh stacking key, which
		// checkRecoveredRelations checks below.
		want.Group, want.Z = c.Group, c.Z
		w, g := contentOf([]model.Element{want}, false), contentOf([]model.Element{c}, false)
		if w[c.ID] != g[c.ID] {
			t.Errorf("recovered %s (%s):\n got %s\nwant %s", e.ID, e.Type, g[c.ID], w[c.ID])
		}
	}
	// The copies have fresh ids, the same grouping under fresh group ids
	// and the same relative stacking, members above their frame.
	if err := checkRecoveredRelations(ref, newIDs, copied); err != nil {
		t.Fatal(err)
	}
	// The check rejects a split group, a reversed stacking order and
	// reused ids.
	var grouped []string
	for _, e := range ref {
		if e.Group != "" {
			grouped = append(grouped, e.ID)
		}
	}
	if len(grouped) != 2 {
		t.Fatalf("the rich content has %d grouped elements, want 2", len(grouped))
	}
	split := maps.Clone(copied)
	c := split[newIDs[grouped[1]]]
	c.Group = "g_SplitOff00"
	split[c.ID] = c
	if checkRecoveredRelations(ref, newIDs, split) == nil {
		t.Fatal("checkRecoveredRelations accepted a split group")
	}
	reversed := maps.Clone(copied)
	var zs []string
	for _, c := range copied {
		zs = append(zs, c.Z)
	}
	slices.Sort(zs)
	slices.Reverse(zs)
	order := slices.Clone(ref)
	slices.SortFunc(order, func(x, y model.Element) int { return strings.Compare(x.Z, y.Z) })
	for i, e := range order {
		c := reversed[newIDs[e.ID]]
		c.Z = zs[i]
		reversed[c.ID] = c
	}
	if checkRecoveredRelations(ref, newIDs, reversed) == nil {
		t.Fatal("checkRecoveredRelations accepted a reversed stacking order")
	}
	reused, sameIDs := map[string]model.Element{}, map[string]string{}
	for _, e := range ref {
		c := copied[newIDs[e.ID]]
		c.ID = e.ID
		reused[e.ID], sameIDs[e.ID] = c, e.ID
	}
	if checkRecoveredRelations(ref, sameIDs, reused) == nil {
		t.Fatal("checkRecoveredRelations accepted reused ids")
	}
	sameGroup := maps.Clone(copied)
	for _, e := range ref {
		if e.Group != "" {
			c := sameGroup[newIDs[e.ID]]
			c.Group = e.Group
			sameGroup[c.ID] = c
		}
	}
	if checkRecoveredRelations(ref, newIDs, sameGroup) == nil {
		t.Fatal("checkRecoveredRelations accepted a reused group id")
	}

	// Rollback: the board equals the revision again, every value with its
	// author fields; the recovered copies are gone.
	if _, err := b.core.Rollback(ctx, rev, "Alice"); err != nil {
		t.Fatal(err)
	}
	if got, want := contentOf(aliveElements(b), true), contentOf(ref, true); !mapsEqualStr(got, want) {
		t.Fatalf("rolled back content differs:\n%v\n%v", got, want)
	}
	var meta model.PrivateElement
	b.state(func(st *engine.State) { meta = st.Board() })
	if meta.Text != "Rich" || meta.Grid {
		t.Fatalf("rolled back board meta %+v", meta.Element)
	}
}

// checkRecoveredRelations checks the copies recovered from ref (newIDs maps
// an original id to its copy's): every copy has a fresh id, two copies share
// a group exactly when their originals did, under a group id that is not an
// original one, and the copies stack in the order of their originals, so
// every member stays above its frame.
func checkRecoveredRelations(ref []model.Element, newIDs map[string]string, copies map[string]model.Element) error {
	orig, origGroups, fresh := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, e := range ref {
		orig[e.ID] = true
		if e.Group != "" {
			origGroups[e.Group] = true
		}
	}
	for _, e := range ref {
		id := newIDs[e.ID]
		if orig[id] || fresh[id] {
			return fmt.Errorf("the copy of %s has id %q, not a fresh one", e.ID, id)
		}
		fresh[id] = true
		c, ok := copies[id]
		if !ok || c.ID != id {
			return fmt.Errorf("no copy %q of %s", id, e.ID)
		}
		if origGroups[c.Group] {
			return fmt.Errorf("the copy of %s reuses group %q", e.ID, c.Group)
		}
		if (e.Group == "") != (c.Group == "") {
			return fmt.Errorf("%s group %q -> %q", e.ID, e.Group, c.Group)
		}
	}
	for _, x := range ref {
		cx := copies[newIDs[x.ID]]
		for _, y := range ref {
			if x.ID == y.ID {
				continue
			}
			cy := copies[newIDs[y.ID]]
			if same, csame := x.Group != "" && x.Group == y.Group, cx.Group != "" && cx.Group == cy.Group; same != csame {
				return fmt.Errorf("%s and %s share a group %v, their copies %v", x.ID, y.ID, same, csame)
			}
			if (x.Z < y.Z) != (cx.Z < cy.Z) {
				return fmt.Errorf("%s (z %s) and %s (z %s) stack as copies %s and %s", x.ID, x.Z, y.ID, y.Z, cx.Z, cy.Z)
			}
		}
		if x.Frame != "" {
			if f := copies[newIDs[x.Frame]]; cx.Z <= f.Z {
				return fmt.Errorf("the copy of member %s (z %s) is not above its frame's copy (z %s)", x.ID, cx.Z, f.Z)
			}
		}
	}
	return nil
}

func mapsEqualStr(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return slices.Equal(sortedKeys(a), sortedKeys(b))
}
