package model

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// Buckets of a revision snapshot (5.2): elements/00.json … ff.json, plus
// board.json for the metadata, numbered BoardBucket.
const (
	BucketCount = 256
	BoardBucket = BucketCount
)

// BucketOf is the bucket of element id: the first byte of sha256(id).
func BucketOf(id string) int {
	if id == BoardID {
		return BoardBucket
	}
	h := sha256.Sum256([]byte(id))
	return int(h[0])
}

// BucketPath is the snapshot file path of bucket b.
func BucketPath(b int) string {
	if b == BoardBucket {
		return "board.json"
	}
	return fmt.Sprintf("elements/%02x.json", b)
}

// BucketJSON is the canonical bucket file {"<id>": {content fields}} of els,
// or board.json when els is the metadata pseudo-element alone.
func BucketJSON(els []Element) []byte {
	if len(els) == 1 && els[0].Type == TypeBoard {
		return els[0].CanonicalJSON()
	}
	m := make(map[string]any, len(els))
	for _, e := range els {
		m[e.ID] = e.Content()
	}
	return Canonical(m)
}

// PrivateElement is an element as private clients receive it: content
// fields plus incarnation, bundle versions and, for connectors, the
// incarnation of each attached end's target.
type PrivateElement struct {
	Element
	Inc    int64
	V      map[string]int64
	EndInc [2]int64
}

// MarshalJSON writes the content fields with inc, v and end_inc.
func (p PrivateElement) MarshalJSON() ([]byte, error) {
	m := p.Element.Content()
	if p.Type == TypeBoard {
		m["id"] = BoardID
	}
	m["inc"], m["v"] = p.Inc, p.V
	if p.Type == TypeConnector {
		m["end_inc"] = []int64{p.EndInc[0], p.EndInc[1]}
	}
	return Canonical(m), nil
}

// PublicElement is the public projection of an element (7.3): the content
// fields without the author fields. It is an allow-list: a field reaches
// Public only when this function names it.
func PublicElement(e Element) map[string]any {
	c := e.Content()
	allowed := []string{"id", "type", "x", "y", "w", "h", "rot", "z", "frame", "group", "locked",
		"text", "style", "ends", "pts", "asset", "alt", "natural", "format", "title", "grid"}
	out := make(map[string]any, len(allowed))
	for _, k := range allowed {
		if v, ok := c[k]; ok {
			out[k] = v
		}
	}
	if e.Type == TypeBoard {
		out["id"] = BoardID
	}
	return out
}

// SetVal is one bundle of a canonical delta: the resulting value and its
// version, written as [val, v].
type SetVal struct {
	Val json.RawMessage
	V   int64
}

// MarshalJSON writes [val, v].
func (s SetVal) MarshalJSON() ([]byte, error) {
	return Canonical([]any{s.Val, s.V}), nil
}

// Change is one element entry of a canonical delta (4.6). Exactly one of
// Set, Add, Revive or Del is set.
type Change struct {
	ID        string
	Inc       int64
	Set       map[string]SetVal
	Overwrote int64
	// Author is the element's new authorship when a set changed it (TECH
	// public edit 7), written as "ta" and "lk" beside "set".
	Author *Authorship
	Add    *PrivateElement
	Revive *PrivateElement
	Del    int64
}

// Authorship is an element's ta and lk.
type Authorship struct {
	TA map[string]TextAuthor
	LK string
}

// MarshalJSON writes the 4.6 form of the change.
func (c Change) MarshalJSON() ([]byte, error) {
	m := map[string]any{"id": c.ID}
	switch {
	case c.Add != nil:
		m["inc"], m["add"] = c.Inc, c.Add
	case c.Revive != nil:
		m["inc"], m["revive"] = c.Inc, c.Revive
	case c.Del != 0:
		m["del"] = c.Del
	default:
		m["inc"], m["set"] = c.Inc, c.Set
		if c.Overwrote != 0 {
			m["overwrote"] = c.Overwrote
		}
		if c.Author != nil {
			ta := c.Author.TA
			if ta == nil {
				ta = map[string]TextAuthor{}
			}
			m["ta"], m["lk"] = ta, c.Author.LK
		}
	}
	return json.Marshal(m)
}

// By names who produced a delta.
type By struct {
	Kind  string `json:"kind"` // person, agent or system
	Label string `json:"label"`
	// Guest marks a guest's change for members, whose canvases draw the
	// guest badge next to Label (spec rule 13).
	Guest bool `json:"guest,omitempty"`
	// Who is the editor kind of a person (KindMember or KindGuest); it
	// stays on the server and lets the guest projection name the kind.
	Who string `json:"-"`
}

// Reset marks a delta of a whole-board rollback or replacement.
type Reset struct {
	Kind string `json:"kind"` // rollback or replace
	Of   int64  `json:"of,omitempty"`
}

// Delta is what one batch produced (4.6). From names the sender's batch,
// "session:n"; the transport sends it only to that session.
type Delta struct {
	Seq     int64    `json:"seq"`
	From    string   `json:"from,omitempty"`
	By      *By      `json:"by,omitempty"`
	Reset   *Reset   `json:"reset,omitempty"`
	Changes []Change `json:"changes"`
}

// PublicDelta is the public projection of d (7.3): the seq and the changes
// without versions, incarnations, authors or reset. A delta whose changes have
// no public part still keeps its seq, so Public clients see consecutive seqs.
func PublicDelta(d Delta) map[string]any {
	changes := make([]map[string]any, 0, len(d.Changes))
	for _, c := range d.Changes {
		m := map[string]any{"id": c.ID}
		switch {
		case c.Add != nil:
			m["add"] = PublicElement(c.Add.Element)
		case c.Revive != nil:
			m["revive"] = PublicElement(c.Revive.Element)
		case c.Del != 0:
			m["del"] = true
		default:
			set := make(map[string]json.RawMessage, len(c.Set))
			for g, sv := range c.Set {
				set[g] = sv.Val
			}
			m["set"] = set
		}
		changes = append(changes, m)
	}
	return map[string]any{"seq": d.Seq, "changes": changes}
}
