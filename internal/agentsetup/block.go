package agentsetup

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Action is what a change does to a file.
type Action int

const (
	Unchanged Action = iota
	Create
	Add
	Replace
	Remove
	Delete
)

func (a Action) String() string {
	return [...]string{"unchanged", "create", "add", "replace", "remove", "delete"}[a]
}

// ErrBlock reports a block shape that is refused: a begin marker without an
// end marker, an end before a begin, or more than one block.
var ErrBlock = errors.New("the Flats block is malformed")

type line struct {
	start, textEnd, end int // text is [start, textEnd), the line ending is [textEnd, end)
}

func splitLines(b []byte) []line {
	var out []line
	for start := 0; start < len(b); {
		end := len(b)
		if i := bytes.IndexByte(b[start:], '\n'); i >= 0 {
			end = start + i + 1
		}
		textEnd := end
		if textEnd > start && b[textEnd-1] == '\n' {
			textEnd--
			if textEnd > start && b[textEnd-1] == '\r' {
				textEnd--
			}
		}
		out = append(out, line{start, textEnd, end})
		start = end
	}
	return out
}

func lineEnding(b []byte) string {
	if bytes.Contains(b, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

// fence tracks fenced code blocks (backticks or tildes) line by line.
type fence struct {
	char  byte
	count int
}

// step reports whether text lies inside a fenced code block, including the
// fence lines themselves.
func (f *fence) step(text string) bool {
	t := strings.TrimLeft(text, " ")
	if len(text)-len(t) > 3 {
		return f.char != 0
	}
	if len(t) >= 3 && (t[0] == '`' || t[0] == '~') {
		n := 0
		for n < len(t) && t[n] == t[0] {
			n++
		}
		if n >= 3 {
			if f.char == 0 {
				f.char, f.count = t[0], n
				return true
			}
			if t[0] == f.char && n >= f.count && strings.Trim(t[n:], " \t") == "" {
				f.char, f.count = 0, 0
			}
			return true
		}
	}
	return f.char != 0
}

// findBlock returns the line indexes of the begin and end markers.
func findBlock(content []byte, lines []line) (begin, end int, found bool, err error) {
	var f fence
	begin, end = -1, -1
	for i, l := range lines {
		text := string(content[l.start:l.textEnd])
		if f.step(text) {
			continue
		}
		switch text {
		case BeginMarker:
			if begin >= 0 {
				return 0, 0, false, fmt.Errorf("%w: a second begin marker, so more than one block", ErrBlock)
			}
			begin = i
		case EndMarker:
			if begin < 0 {
				return 0, 0, false, fmt.Errorf("%w: an end marker comes before a begin marker", ErrBlock)
			}
			if end >= 0 {
				return 0, 0, false, fmt.Errorf("%w: a second end marker, so more than one block", ErrBlock)
			}
			end = i
		}
	}
	switch {
	case begin < 0:
		return 0, 0, false, nil
	case end < 0:
		return 0, 0, false, fmt.Errorf("%w: a begin marker has no end marker", ErrBlock)
	}
	return begin, end, true, nil
}

func joinLines(lines []string, eol string) []byte {
	return []byte(strings.Join(lines, eol) + eol)
}

// HasBlock reports whether content holds a Flats block. A malformed block is
// an error.
func HasBlock(content []byte) (bool, error) {
	_, _, found, err := findBlock(content, splitLines(content))
	return found, err
}

// SetBlock returns content with the Flats block present and current.
func SetBlock(content []byte) ([]byte, Action, error) {
	lines := splitLines(content)
	begin, end, found, err := findBlock(content, lines)
	if err != nil {
		return nil, Unchanged, err
	}
	want := blockLines()
	eol := lineEnding(content)
	if found {
		var have []string
		for _, l := range lines[begin : end+1] {
			have = append(have, string(content[l.start:l.textEnd]))
		}
		if slices.Equal(have, want) {
			return content, Unchanged, nil
		}
		replacement := strings.Join(want, eol)
		if last := lines[end]; last.end > last.textEnd {
			replacement += eol
		}
		out := slices.Concat(content[:lines[begin].start], []byte(replacement), content[lines[end].end:])
		return out, Replace, nil
	}
	if len(content) == 0 {
		return joinLines(want, eol), Add, nil
	}
	out := slices.Clone(content)
	if content[len(content)-1] != '\n' {
		out = append(out, eol...)
	}
	out = append(out, eol...)
	return append(out, joinLines(want, eol)...), Add, nil
}

// DropBlock returns content without the Flats block and the blank line
// before it.
func DropBlock(content []byte) ([]byte, Action, error) {
	lines := splitLines(content)
	begin, end, found, err := findBlock(content, lines)
	if err != nil {
		return nil, Unchanged, err
	}
	if !found {
		return content, Unchanged, nil
	}
	from := lines[begin].start
	if begin > 0 && lines[begin-1].start == lines[begin-1].textEnd {
		from = lines[begin-1].start
	}
	return slices.Concat(content[:from], content[lines[end].end:]), Remove, nil
}

// OnlyFrontMatter reports whether content holds nothing but a front matter
// block (a first line "---" through the next line "---") and whitespace.
// Content with no front matter and only whitespace also qualifies.
func OnlyFrontMatter(content []byte) bool {
	if len(bytes.TrimSpace(content)) == 0 {
		return true
	}
	lines := splitLines(content)
	if string(content[lines[0].start:lines[0].textEnd]) != "---" {
		return false
	}
	for i := 1; i < len(lines); i++ {
		if string(content[lines[i].start:lines[i].textEnd]) == "---" {
			return len(bytes.TrimSpace(content[lines[i].end:])) == 0
		}
	}
	return false
}
