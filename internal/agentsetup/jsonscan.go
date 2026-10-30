package agentsetup

import (
	"encoding/json"
	"errors"
)

// node is a JSON value with its byte span in the source text. Only valid JSON
// reaches the scanner, so it does no error recovery.
type node struct {
	kind       byte // '{', '[', '"' or 'v' (number, true, false, null)
	start, end int  // [start, end) of the value text
	members    []member
	elems      []*node
}

type member struct {
	key      string
	keyStart int
	val      *node
}

var errNotJSON = errors.New("not valid JSON")

// parseJSON scans b, which must be a single valid JSON value.
func parseJSON(b []byte) (*node, error) {
	if !json.Valid(b) {
		return nil, errNotJSON
	}
	s := scanner{b: b}
	s.skipSpace()
	return s.value(), nil
}

type scanner struct {
	b   []byte
	pos int
}

func (s *scanner) skipSpace() {
	for s.pos < len(s.b) {
		switch s.b[s.pos] {
		case ' ', '\t', '\r', '\n':
			s.pos++
		default:
			return
		}
	}
}

func (s *scanner) value() *node {
	n := &node{start: s.pos}
	switch s.b[s.pos] {
	case '{':
		n.kind = '{'
		s.pos++
		s.skipSpace()
		for s.b[s.pos] != '}' {
			keyStart := s.pos
			s.str()
			var key string
			_ = json.Unmarshal(s.b[keyStart:s.pos], &key)
			s.skipSpace()
			s.pos++ // ':'
			s.skipSpace()
			n.members = append(n.members, member{key, keyStart, s.value()})
			s.skipSpace()
			if s.b[s.pos] == ',' {
				s.pos++
				s.skipSpace()
			}
		}
		s.pos++
	case '[':
		n.kind = '['
		s.pos++
		s.skipSpace()
		for s.b[s.pos] != ']' {
			n.elems = append(n.elems, s.value())
			s.skipSpace()
			if s.b[s.pos] == ',' {
				s.pos++
				s.skipSpace()
			}
		}
		s.pos++
	case '"':
		n.kind = '"'
		s.str()
	default:
		n.kind = 'v'
		for s.pos < len(s.b) && !isValueEnd(s.b[s.pos]) {
			s.pos++
		}
	}
	n.end = s.pos
	return n
}

func isValueEnd(c byte) bool {
	switch c {
	case ',', '}', ']', ' ', '\t', '\r', '\n':
		return true
	}
	return false
}

// str advances over a string literal.
func (s *scanner) str() {
	s.pos++
	for s.b[s.pos] != '"' {
		if s.b[s.pos] == '\\' {
			s.pos++
		}
		s.pos++
	}
	s.pos++
}

// text returns the source text of n.
func (n *node) text(b []byte) []byte { return b[n.start:n.end] }

// stringValue decodes a string node.
func (n *node) stringValue(b []byte) (string, bool) {
	if n.kind != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(n.text(b), &s); err != nil {
		return "", false
	}
	return s, true
}

// find returns the members of an object with the given key.
func (n *node) find(key string) []member {
	var out []member
	for _, m := range n.members {
		if m.key == key {
			out = append(out, m)
		}
	}
	return out
}
