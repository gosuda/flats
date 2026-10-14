package design

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrStale refuses an operation whose target changed since the person saw
// it. The caller reloads the current state.
var ErrStale = errors.New("this part of the design changed since you loaded it")

// ErrInvalidOp refuses a malformed operation.
var ErrInvalidOp = errors.New("invalid design operation")

// Op is one person operation from the canvas app.
type Op struct {
	Kind string `json:"op"`
	// board.set, board.order, text.set, tweak.set
	Path string `json:"path,omitempty"`
	// note.add, note.set, note.delete
	ID string `json:"id,omitempty"`
	// board.set, note.set, note.add: field values; base: values seen
	Fields map[string]json.RawMessage `json:"fields,omitempty"`
	Base   map[string]json.RawMessage `json:"base,omitempty"`
	// board.order
	To string `json:"to,omitempty"`
	// every op: the generation of the file it targets, as last read
	Gen int64 `json:"gen"`
	// board.order: the board's index in order, as seen
	BaseIndex int `json:"base_index,omitempty"`
	// text.set
	Node      int    `json:"node,omitempty"`
	Tag       string `json:"tag,omitempty"`
	InnerHash string `json:"inner_hash,omitempty"`
	HTML      string `json:"html,omitempty"`
	// tweak.set
	Name      string          `json:"name,omitempty"`
	Value     json.RawMessage `json:"value,omitempty"`
	BaseValue json.RawMessage `json:"base_value,omitempty"`
}

// File is a current file of the design with its generation, which changes
// when an agent write or a rollback replaces the file.
type File struct {
	Data []byte
	Gen  int64
}

// Getter reads a current file of the design.
type Getter func(path string) (File, bool)

func checkGen(path string, f File, gen int64) error {
	if f.Gen != gen {
		return fmt.Errorf("%w: %s was replaced", ErrStale, path)
	}
	return nil
}

// Apply applies op to the design read through get and returns the new
// contents of every file it changes.
func Apply(op Op, get Getter) (map[string][]byte, error) {
	switch op.Kind {
	case "board.set", "board.order", "note.add", "note.set", "note.delete":
		return applyCanvas(op, get)
	case "text.set":
		return applyText(op, get)
	case "tweak.set":
		return applyTweak(op, get)
	}
	return nil, fmt.Errorf("%w: unknown op %q", ErrInvalidOp, op.Kind)
}

func loadCanvas(get Getter, gen int64) (*Canvas, error) {
	f, ok := get(IndexPath)
	if !ok {
		return nil, fmt.Errorf("%w: the design has no canvas.json", ErrInvalidOp)
	}
	if err := checkGen(IndexPath, f, gen); err != nil {
		return nil, err
	}
	return ParseCanvas(f.Data)
}

func applyCanvas(op Op, get Getter) (map[string][]byte, error) {
	c, err := loadCanvas(get, op.Gen)
	if err != nil {
		return nil, err
	}
	// Operate on the order readers see: boards missing from order are
	// appended in path order.
	exists := func(p string) bool { _, ok := get(p); return ok }
	c.Normalize(exists)
	switch op.Kind {
	case "board.set":
		b, ok := c.Boards[op.Path]
		if !ok {
			return nil, fmt.Errorf("%w: board %q no longer exists", ErrStale, op.Path)
		}
		if err := setFields(&b, op.Fields, op.Base, map[string]bool{"x": true, "y": true, "w": true, "h": true, "title": true}); err != nil {
			return nil, err
		}
		c.Boards[op.Path] = b
	case "board.order":
		i := indexOf(c.Order, op.Path)
		if i < 0 {
			return nil, fmt.Errorf("%w: board %q no longer exists", ErrStale, op.Path)
		}
		if i != op.BaseIndex {
			return nil, fmt.Errorf("%w: the board order changed", ErrStale)
		}
		order := append(append([]string{}, c.Order[:i]...), c.Order[i+1:]...)
		switch op.To {
		case "front":
			order = append(order, op.Path)
		case "back":
			order = append([]string{op.Path}, order...)
		default:
			return nil, fmt.Errorf("%w: to must be front or back", ErrInvalidOp)
		}
		c.Order = order
	case "note.add":
		id := op.ID
		if id == "" {
			id = "n" + randomHex(6)
		}
		if _, exists := c.Notes[id]; exists {
			return nil, fmt.Errorf("%w: note %q already exists", ErrStale, id)
		}
		var n Note
		if err := setFields(&n, op.Fields, nil, map[string]bool{"x": true, "y": true, "w": true, "text": true, "color": true}); err != nil {
			return nil, err
		}
		c.Notes[id] = n
	case "note.set":
		n, ok := c.Notes[op.ID]
		if !ok {
			return nil, fmt.Errorf("%w: note %q no longer exists", ErrStale, op.ID)
		}
		if err := setFields(&n, op.Fields, op.Base, map[string]bool{"x": true, "y": true, "w": true, "text": true, "color": true}); err != nil {
			return nil, err
		}
		c.Notes[op.ID] = n
	case "note.delete":
		n, ok := c.Notes[op.ID]
		if !ok {
			return nil, fmt.Errorf("%w: note %q no longer exists", ErrStale, op.ID)
		}
		cur, _ := json.Marshal(n)
		var curMap map[string]json.RawMessage
		_ = json.Unmarshal(cur, &curMap)
		for _, k := range []string{"x", "y", "w", "text", "color"} {
			b, ok := op.Base[k]
			if !ok {
				return nil, fmt.Errorf("%w: note.delete needs the complete note as base", ErrInvalidOp)
			}
			if !sameJSON(curMap[k], b) {
				return nil, fmt.Errorf("%w: note %q was edited", ErrStale, op.ID)
			}
		}
		delete(c.Notes, op.ID)
	}
	if ps := c.Normalize(exists); len(ps) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrInvalidOp, ps[0].Message)
	}
	out, err := c.Marshal()
	if err != nil {
		return nil, err
	}
	if len(out) > MaxIndexBytes {
		return nil, fmt.Errorf("%w: canvas.json would be %d bytes, over the %d-byte limit", ErrInvalidOp, len(out), MaxIndexBytes)
	}
	return map[string][]byte{IndexPath: out}, nil
}

// setFields copies allowed fields into dst (a *Board or *Note), checking
// each changed field's base value against the current one.
func setFields(dst any, fields, base map[string]json.RawMessage, allowed map[string]bool) error {
	if len(fields) == 0 {
		return fmt.Errorf("%w: no fields to set", ErrInvalidOp)
	}
	cur, err := json.Marshal(dst)
	if err != nil {
		return err
	}
	var curMap map[string]json.RawMessage
	_ = json.Unmarshal(cur, &curMap)
	for k := range fields {
		if !allowed[k] {
			return fmt.Errorf("%w: field %q cannot be set", ErrInvalidOp, k)
		}
	}
	for k, b := range base {
		if !allowed[k] {
			continue
		}
		if !sameJSON(curMap[k], b) {
			return fmt.Errorf("%w: %s changed", ErrStale, k)
		}
	}
	for k, v := range fields {
		curMap[k] = v
	}
	merged, _ := json.Marshal(curMap)
	if err := json.Unmarshal(merged, dst); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidOp, err)
	}
	return nil
}

// sameJSON compares a current field (absent means its zero value) with a
// base value.
func sameJSON(cur, base json.RawMessage) bool {
	var a, b any
	if len(cur) > 0 {
		_ = json.Unmarshal(cur, &a)
	}
	_ = json.Unmarshal(base, &b)
	if a == nil {
		switch b.(type) {
		case string:
			return b == ""
		case float64:
			return b == 0.0
		case bool:
			return b == false
		}
		return b == nil
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func indexOf(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func artboard(op Op, get Getter) ([]byte, *Source, error) {
	if !strings.HasSuffix(strings.ToLower(op.Path), ".html") {
		return nil, nil, fmt.Errorf("%w: %q is not an artboard", ErrInvalidOp, op.Path)
	}
	f, ok := get(op.Path)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %q no longer exists", ErrStale, op.Path)
	}
	if err := checkGen(op.Path, f, op.Gen); err != nil {
		return nil, nil, err
	}
	src := f.Data
	s, err := Scan(src)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidOp, err)
	}
	return src, s, nil
}

func applyText(op Op, get Getter) (map[string][]byte, error) {
	src, s, err := artboard(op, get)
	if err != nil {
		return nil, err
	}
	e, ok := s.Element(op.Node)
	if !ok || e.Tag != strings.ToLower(op.Tag) || InnerHash(src[e.Inner[0]:e.Inner[1]]) != op.InnerHash {
		return nil, fmt.Errorf("%w: the text was changed", ErrStale)
	}
	clean, err := SanitizeInline(e.Tag, op.HTML)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidOp, err)
	}
	if strings.TrimSpace(TextOf([]byte(clean))) == "" {
		return nil, fmt.Errorf("%w: text cannot be empty; delete the element in the source instead", ErrInvalidOp)
	}
	return map[string][]byte{op.Path: Splice(src, e.Inner, []byte(clean))}, nil
}

func applyTweak(op Op, get Getter) (map[string][]byte, error) {
	src, s, err := artboard(op, get)
	if err != nil {
		return nil, err
	}
	if !s.HasTweaks {
		return nil, fmt.Errorf("%w: the artboard has no tweaks", ErrStale)
	}
	m, err := ParseTweaks(src[s.Tweaks[0]:s.Tweaks[1]])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidOp, err)
	}
	t, ok := m[op.Name]
	if !ok {
		return nil, fmt.Errorf("%w: tweak %q no longer exists", ErrStale, op.Name)
	}
	if len(op.BaseValue) > 0 {
		var base any
		if json.Unmarshal(op.BaseValue, &base) != nil || fmt.Sprint(base) != fmt.Sprint(t.Value) {
			return nil, fmt.Errorf("%w: tweak %q changed", ErrStale, op.Name)
		}
	}
	var v any
	if err := json.Unmarshal(op.Value, &v); err != nil {
		return nil, fmt.Errorf("%w: value: %v", ErrInvalidOp, err)
	}
	m, err = SetTweak(m, op.Name, v)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidOp, err)
	}
	body, err := MarshalTweaks(m)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{op.Path: Splice(src, s.Tweaks, body)}, nil
}

// InnerHash is the guard of a text edit: the SHA-256 of an element's exact
// inner source bytes.
func InnerHash(inner []byte) string {
	sum := sha256.Sum256(inner)
	return hex.EncodeToString(sum[:])
}

// Node describes an editable element for the canvas app's text editor.
type Node struct {
	Tag       string `json:"tag"`
	Inner     string `json:"inner"`
	InnerHash string `json:"inner_hash"`
	Text      string `json:"text"`
}

// NodeOf returns editable element node of an artboard source.
func NodeOf(src []byte, node int) (Node, error) {
	s, err := Scan(src)
	if err != nil {
		return Node{}, err
	}
	e, ok := s.Element(node)
	if !ok {
		return Node{}, fmt.Errorf("%w: no editable element %d", ErrStale, node)
	}
	inner := src[e.Inner[0]:e.Inner[1]]
	return Node{Tag: e.Tag, Inner: string(inner), InnerHash: InnerHash(inner), Text: s.InnerText(e)}, nil
}
