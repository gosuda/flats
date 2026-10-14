package design

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// IndexPath is the canvas index of a design.
const IndexPath = "canvas.json"

// MaxIndexBytes bounds canvas.json.
const MaxIndexBytes = 256 << 10

const (
	maxNotes     = 200
	maxNoteText  = 4000
	maxBoardText = 200
	minFrame     = 40
	maxFrame     = 8000
)

var noteID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

var noteColors = map[string]bool{"": true, "gray": true, "yellow": true, "blue": true, "green": true, "pink": true, "purple": true}

// Board is one artboard's frame on the canvas.
type Board struct {
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	W     float64 `json:"w"`
	H     float64 `json:"h"`
	Title string  `json:"title,omitempty"`
	Fill  bool    `json:"fill,omitempty"`
}

// Note is a free note on the canvas.
type Note struct {
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	W     float64 `json:"w,omitempty"`
	Text  string  `json:"text"`
	Color string  `json:"color,omitempty"`
}

// Launch selects how the canvas opens.
type Launch struct {
	View  string `json:"view"`
	Board string `json:"board,omitempty"`
}

// Canvas is the parsed canvas.json. Unknown top-level keys are kept.
type Canvas struct {
	Title  string           `json:"title"`
	Boards map[string]Board `json:"boards"`
	Order  []string         `json:"order"`
	Notes  map[string]Note  `json:"notes"`
	Launch *Launch          `json:"launch,omitempty"`
	extra  map[string]json.RawMessage
}

var knownCanvasKeys = []string{"title", "boards", "order", "notes", "launch"}

// ParseCanvas decodes canvas.json without validating it against files.
func ParseCanvas(raw []byte) (*Canvas, error) {
	if len(raw) > MaxIndexBytes {
		return nil, fmt.Errorf("canvas.json is %d bytes, over the %d-byte limit", len(raw), MaxIndexBytes)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("canvas.json is not a JSON object: %w", err)
	}
	if all == nil {
		return nil, errors.New("canvas.json is not a JSON object")
	}
	c := &Canvas{extra: map[string]json.RawMessage{}}
	dec := func(key string, into any) error {
		v, ok := all[key]
		if !ok || string(v) == "null" {
			return nil
		}
		d := json.NewDecoder(bytes.NewReader(v))
		d.DisallowUnknownFields()
		if err := d.Decode(into); err != nil {
			return fmt.Errorf("canvas.json %s: %w", key, err)
		}
		return nil
	}
	for _, f := range []struct {
		k string
		v any
	}{{"title", &c.Title}, {"boards", &c.Boards}, {"order", &c.Order}, {"notes", &c.Notes}, {"launch", &c.Launch}} {
		if err := dec(f.k, f.v); err != nil {
			return nil, err
		}
	}
	for k, v := range all {
		if !slices.Contains(knownCanvasKeys, k) {
			c.extra[k] = v
		}
	}
	if c.Boards == nil {
		c.Boards = map[string]Board{}
	}
	if c.Notes == nil {
		c.Notes = map[string]Note{}
	}
	return c, nil
}

// Problem is one validation problem with a fix.
type Problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

// Normalize completes the order (appending boards missing from it in path
// order) and validates the canvas against the set of files. It returns
// every problem at once.
func (c *Canvas) Normalize(exists func(path string) bool) []Problem {
	var ps []Problem
	add := func(msg, fix string) { ps = append(ps, Problem{Path: IndexPath, Message: msg, Fix: fix}) }
	if len([]rune(c.Title)) > maxBoardText {
		add("title is longer than 200 characters", "shorten the title")
	}
	keys := make([]string, 0, len(c.Boards))
	for k := range c.Boards {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b := c.Boards[k]
		if !strings.HasSuffix(strings.ToLower(k), ".html") {
			add(fmt.Sprintf("board %q is not an .html file", k), "list only .html artboard files in boards")
		} else if !exists(k) {
			add(fmt.Sprintf("board %q names a file the design does not have", k), "save the artboard file, or remove it from boards")
		}
		if !finite(b.X) || !finite(b.Y) || math.Abs(b.X) > 1e7 || math.Abs(b.Y) > 1e7 {
			add(fmt.Sprintf("board %q has an invalid x or y", k), "use finite numbers within ±10,000,000")
		}
		if !finite(b.W) || !finite(b.H) || b.W < minFrame || b.W > maxFrame || b.H < minFrame || b.H > maxFrame {
			add(fmt.Sprintf("board %q is %vx%v; w and h must be 40 to 8000", k, b.W, b.H), "set a frame size from 40 to 8000 px")
		}
		if len([]rune(b.Title)) > maxBoardText {
			add(fmt.Sprintf("board %q has a title longer than 200 characters", k), "shorten the title")
		}
	}
	seen := map[string]bool{}
	order := make([]string, 0, len(keys))
	for _, k := range c.Order {
		if _, ok := c.Boards[k]; !ok {
			add(fmt.Sprintf("order names %q, which is not in boards", k), "list only board keys in order")
			continue
		}
		if seen[k] {
			add(fmt.Sprintf("order lists %q twice", k), "list each board once")
			continue
		}
		seen[k] = true
		order = append(order, k)
	}
	for _, k := range keys {
		if !seen[k] {
			order = append(order, k)
		}
	}
	c.Order = order
	if len(c.Notes) > maxNotes {
		add(fmt.Sprintf("canvas has %d notes, over the limit of %d", len(c.Notes), maxNotes), "remove notes")
	}
	for id, n := range c.Notes {
		if !noteID.MatchString(id) {
			add(fmt.Sprintf("note id %q must match %s", id, noteID), "rename the note")
		}
		if !finite(n.X) || !finite(n.Y) || !finite(n.W) || n.W < 0 || n.W > maxFrame {
			add(fmt.Sprintf("note %q has an invalid position or width", id), "use finite numbers; w from 0 to 8000")
		}
		if len([]rune(n.Text)) > maxNoteText {
			add(fmt.Sprintf("note %q is longer than %d characters", id, maxNoteText), "shorten the note")
		}
		if !noteColors[n.Color] {
			add(fmt.Sprintf("note %q has color %q", id, n.Color), "use gray, yellow, blue, green, pink or purple")
		}
	}
	if c.Launch != nil {
		switch c.Launch.View {
		case "canvas":
		case "focused":
			if _, ok := c.Boards[c.Launch.Board]; !ok {
				add(fmt.Sprintf("launch board %q is not in boards", c.Launch.Board), "name a board key in launch.board")
			}
		default:
			add(fmt.Sprintf("launch view %q", c.Launch.View), `use {"view":"canvas"} or {"view":"focused","board":"<key>"}`)
		}
	}
	return ps
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// Marshal renders canvas.json with 2-space indentation, known keys first,
// then kept unknown keys.
func (c *Canvas) Marshal() ([]byte, error) {
	m := map[string]any{"title": c.Title, "boards": c.Boards, "order": c.Order, "notes": c.Notes}
	if c.Launch != nil {
		m["launch"] = c.Launch
	}
	for k, v := range c.extra {
		m[k] = v
	}
	if c.Order == nil {
		m["order"] = []string{}
	}
	return json.MarshalIndent(m, "", "  ")
}

// Clone returns a deep copy.
func (c *Canvas) Clone() *Canvas {
	out := &Canvas{Title: c.Title, Boards: map[string]Board{}, Notes: map[string]Note{}, Order: slices.Clone(c.Order), extra: map[string]json.RawMessage{}}
	for k, v := range c.Boards {
		out.Boards[k] = v
	}
	for k, v := range c.Notes {
		out.Notes[k] = v
	}
	if c.Launch != nil {
		l := *c.Launch
		out.Launch = &l
	}
	for k, v := range c.extra {
		out.extra[k] = v
	}
	return out
}
