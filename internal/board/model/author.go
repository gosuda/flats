package model

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Editor kinds: who wrote something, without saying which person
// (docs/spec/board-public-edit-TECH.md 3.3, 7).
const (
	KindMember = "member"
	KindGuest  = "guest"
	KindAgent  = "agent"
	KindSystem = "system"
)

// GuestKeyPrefix starts every guest's editor key. Only the server issues
// such keys.
const GuestKeyPrefix = "guest:"

// MaxGuestName is the length of a guest name, in characters.
const MaxGuestName = 40

// IsGuestKey reports whether key is a guest's editor key.
func IsGuestKey(key string) bool { return strings.HasPrefix(key, GuestKeyPrefix) }

// EditorKind is the kind of an editor or creator key: a Private editor
// (ts:, browser: or a Private HTTP request) is a member. An empty key, as
// some old or copied elements have, has no kind.
func EditorKind(key string) string {
	switch {
	case key == "":
		return ""
	case IsGuestKey(key):
		return KindGuest
	case strings.HasPrefix(key, "agent:"):
		return KindAgent
	case key == KindSystem:
		return KindSystem
	}
	return KindMember
}

// TextAuthor is the kind of the editor that last wrote one text bundle, and
// the typed name when that editor was a guest.
type TextAuthor struct {
	K string `json:"k"`
	N string `json:"n,omitempty"`
}

// textBundles are the bundles whose last writer the ta field records.
var textBundles = []string{BundleText, BundleAlt}

// IsTextBundle reports whether ta records the last writer of g.
func IsTextBundle(g string) bool { return g == BundleText || g == BundleAlt }

// TextBundles lists the text bundles of typ.
func TextBundles(typ string) []string {
	var out []string
	for _, g := range textBundles {
		if HasBundle(typ, g) {
			out = append(out, g)
		}
	}
	return out
}

// author is the ta entry of an editor of kind k; name counts only for a
// guest.
func author(k, name string) TextAuthor {
	if k != KindGuest {
		name = ""
	}
	return TextAuthor{K: k, N: name}
}

// SetAuthor records an editor of kind k (with name for a guest) as the last
// writer of every text bundle and of the element, as for a new element.
func (e *Element) SetAuthor(k, name string) {
	if e.Type == TypeBoard {
		return
	}
	e.TA = nil
	if k == "" {
		e.LK = ""
		return
	}
	for _, g := range TextBundles(e.Type) {
		if e.TA == nil {
			e.TA = map[string]TextAuthor{}
		}
		e.TA[g] = author(k, name)
	}
	e.LK = k
}

// NoteWrite records a write of bundle g by an editor of kind k: the last
// change, and for a text bundle its writer. The map is replaced, never
// changed in place, since clones share it.
func (e *Element) NoteWrite(g, k, name string) {
	if e.Type == TypeBoard || k == "" {
		return
	}
	e.LK = k
	if !IsTextBundle(g) {
		return
	}
	ta := make(map[string]TextAuthor, len(e.TA)+1)
	for b, a := range e.TA {
		ta[b] = a
	}
	ta[g] = author(k, name)
	e.TA = ta
}

// DecodeNewElement is DecodeElement for an element a client, an agent or a
// file supplies: authorship it claims is dropped, since the server records
// who writes.
func DecodeNewElement(raw []byte) (Element, Present, error) {
	el, present, err := DecodeElement(raw)
	el.TA, el.LK = nil, ""
	return el, present, err
}

// SameAuthorship reports whether two states carry the same ta and lk.
func SameAuthorship(x, y Element) bool {
	if x.LK != y.LK || len(x.TA) != len(y.TA) {
		return false
	}
	for g, a := range x.TA {
		if y.TA[g] != a {
			return false
		}
	}
	return true
}

func validKind(k string) bool {
	switch k {
	case KindMember, KindGuest, KindAgent, KindSystem:
		return true
	}
	return false
}

// validAuthorship checks the form of stored ta and lk.
func (e Element) validAuthorship() error {
	if e.LK != "" && !validKind(e.LK) {
		return formatErr("lk %q is not an editor kind", e.LK)
	}
	for g, a := range e.TA {
		if !IsTextBundle(g) || !HasBundle(e.Type, g) {
			return formatErr("ta names %q, which is no text of a %s", g, e.Type)
		}
		if !validKind(a.K) {
			return formatErr("ta kind %q is not an editor kind", a.K)
		}
		if a.N != "" && (a.K != KindGuest || TextLen(a.N) > MaxGuestName || !utf8.ValidString(a.N)) {
			return formatErr("ta name of %q is not a guest name", g)
		}
	}
	return nil
}

// CleanGuestName trims a typed guest name and removes control and
// invisible format characters (category Cf). ok is false when 1 to 40
// characters do not remain.
func CleanGuestName(s string) (name string, ok bool) {
	if !utf8.ValidString(s) {
		return "", false
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	n := TextLen(s)
	return s, n >= 1 && n <= MaxGuestName
}

// GuestView is the guest projection of an element (3.3): the private form
// with its creator reduced to a kind. ta holds only kinds and guest names,
// so it stays.
func GuestView(p PrivateElement) PrivateElement {
	p.Element.By = EditorKind(p.Element.By)
	return p
}

// GuestDelta is the guest projection of a delta: authors reduced to kinds.
// The changes share their values with d.
func GuestDelta(d Delta) Delta {
	if d.By != nil {
		k := byKind(d.By)
		d.By = &By{Kind: k, Label: kindLabel(k)}
	}
	changes := make([]Change, len(d.Changes))
	for i, c := range d.Changes {
		if c.Add != nil {
			v := GuestView(*c.Add)
			c.Add = &v
		}
		if c.Revive != nil {
			v := GuestView(*c.Revive)
			c.Revive = &v
		}
		changes[i] = c
	}
	d.Changes = changes
	return d
}

// byKind is the editor kind behind a delta's producer. Only a person
// needs Who: an agent and the system are their own kinds.
func byKind(b *By) string {
	switch {
	case b.Who != "":
		return b.Who
	case b.Kind == KindAgent || b.Kind == KindSystem:
		return b.Kind
	}
	return KindMember
}

// kindLabel is the name a guest sees for an editor kind.
func kindLabel(k string) string {
	switch k {
	case KindGuest:
		return "Guest"
	case KindAgent:
		return "Agent"
	case KindSystem:
		return "System"
	}
	return "Member"
}
