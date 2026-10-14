// Package design implements the design content type: the source model of
// artboards (editable text, tweaks), the canvas index, the serving rewrite
// and the HTTP handler of a design flat. Content storage and revisions live
// in internal/core.
package design

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// maxScanTokens bounds how far one candidate element is scanned for its end
// tag. A longer element is simply not editable.
const maxScanTokens = 20000

// maxTokens bounds the tokens of one artboard. Larger sources are served
// without editable elements.
const maxTokens = 500000

// inlineTags are the tags an editable element may contain.
var inlineTags = map[string]bool{
	"b": true, "strong": true, "i": true, "em": true, "u": true, "s": true,
	"small": true, "sub": true, "sup": true, "code": true, "mark": true,
	"span": true, "br": true,
}

// notEditable are elements that are never editable themselves.
var notEditable = map[string]bool{
	"html": true, "head": true, "body": true, "script": true, "style": true,
	"title": true, "textarea": true, "template": true, "svg": true,
	"math": true, "select": true, "option": true, "iframe": true,
	"object": true, "noscript": true,
}

// opaqueContexts are elements whose content is never scanned for editable
// elements.
var opaqueContexts = map[string]bool{"head": true, "template": true, "svg": true, "math": true, "noscript": true}

type token struct {
	typ        html.TokenType
	tag        string
	start, end int // byte range of the raw token
	tweaks     bool
}

// Element is one editable text element of an artboard source.
type Element struct {
	Node       int    // its id: the k-th editable element in source order
	Tag        string // lower-case tag name
	StartTag   [2]int // byte range of the start tag
	Inner      [2]int // byte range of the content
	startToken int
}

// Source is the scanned structure of one artboard.
type Source struct {
	src      []byte
	tokens   []token
	Elements []Element
	// Tweaks is the byte range of the tweaks block content; HasTweaks says
	// whether the artboard has one.
	Tweaks    [2]int
	HasTweaks bool
	// Insert is where the host inserts its style and bridge script.
	Insert int
}

// ErrTooManyTweakBlocks is returned for an artboard with two tweaks blocks.
var ErrTooManyTweakBlocks = errors.New("an artboard has at most one tweaks block (<script type=\"application/json\" data-flats-tweaks>)")

// Scan tokenizes an artboard source and finds its editable elements, its
// tweaks block and the insertion point for host content.
func Scan(src []byte) (*Source, error) {
	s := &Source{src: src}
	z := html.NewTokenizer(bytes.NewReader(src))
	z.SetMaxBuf(0)
	pos := 0
	headStart, headEnd, bodyStart := -1, -1, -1
	tweakBlocks := 0
	pendingTweaks := false
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if errors.Is(z.Err(), io.EOF) {
				break
			}
			return nil, z.Err()
		}
		raw := z.Raw()
		t := token{typ: tt, start: pos, end: pos + len(raw)}
		pos = t.end
		switch tt {
		case html.StartTagToken, html.EndTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			t.tag = string(name)
			if tt == html.StartTagToken && t.tag == "script" && hasAttr {
				isJSON, isTweaks := false, false
				for {
					k, v, more := z.TagAttr()
					switch string(k) {
					case "type":
						isJSON = strings.EqualFold(strings.TrimSpace(string(v)), "application/json")
					case "data-flats-tweaks":
						isTweaks = true
					}
					if !more {
						break
					}
				}
				if isJSON && isTweaks {
					t.tweaks = true
					tweakBlocks++
					pendingTweaks = true
					s.Tweaks = [2]int{t.end, t.end}
				}
			}
			switch {
			case tt == html.StartTagToken && t.tag == "head" && headStart < 0:
				headStart = t.end
			case tt == html.EndTagToken && t.tag == "head" && headEnd < 0:
				headEnd = t.start
			case tt == html.StartTagToken && t.tag == "body" && bodyStart < 0:
				bodyStart = t.start
			}
		case html.TextToken:
			if pendingTweaks {
				s.Tweaks = [2]int{t.start, t.end}
			}
		}
		if tt != html.TextToken {
			pendingTweaks = pendingTweaks && tt == html.StartTagToken && t.tweaks
		}
		if len(s.tokens) >= maxTokens {
			s.tokens = nil
			break
		}
		s.tokens = append(s.tokens, t)
	}
	if tweakBlocks > 1 {
		return nil, ErrTooManyTweakBlocks
	}
	s.HasTweaks = tweakBlocks == 1
	switch {
	case headEnd >= 0:
		s.Insert = headEnd
	case headStart >= 0:
		s.Insert = headStart
	case bodyStart >= 0:
		s.Insert = bodyStart
	default:
		s.Insert = afterDoctype(src)
	}
	s.findElements()
	return s, nil
}

// afterDoctype returns the offset after a leading doctype, or 0.
func afterDoctype(src []byte) int {
	trimmed := bytes.TrimLeft(bytes.TrimPrefix(src, []byte("\xef\xbb\xbf")), " \t\r\n")
	if len(trimmed) > 9 && strings.EqualFold(string(trimmed[:9]), "<!doctype") {
		if i := bytes.IndexByte(trimmed, '>'); i >= 0 {
			return len(src) - len(trimmed) + i + 1
		}
	}
	return 0
}

func (s *Source) findElements() {
	opaque := 0 // depth inside opaque contexts
	coveredUntil := -1
	for i := 0; i < len(s.tokens); i++ {
		t := s.tokens[i]
		switch t.typ {
		case html.StartTagToken:
			if opaqueContexts[t.tag] {
				opaque++
				continue
			}
		case html.EndTagToken:
			if opaqueContexts[t.tag] && opaque > 0 {
				opaque--
			}
			continue
		default:
			continue
		}
		if opaque > 0 || i <= coveredUntil || notEditable[t.tag] || isVoid(t.tag) {
			continue
		}
		end, ok := s.editableEnd(i)
		if !ok {
			continue
		}
		s.Elements = append(s.Elements, Element{
			Node: len(s.Elements), Tag: t.tag,
			StartTag:   [2]int{t.start, t.end},
			Inner:      [2]int{t.end, s.tokens[end].start},
			startToken: i,
		})
		coveredUntil = end
	}
}

// editableEnd scans from the start tag at i to its explicit end tag. It
// reports the end tag's index and whether the element is text-editable.
func (s *Source) editableEnd(i int) (int, bool) {
	open := s.tokens[i].tag
	var stack []string
	hasText := false
	for j := i + 1; j < len(s.tokens) && j-i <= maxScanTokens; j++ {
		t := s.tokens[j]
		switch t.typ {
		case html.TextToken:
			if !hasText && len(bytes.TrimSpace(s.src[t.start:t.end])) > 0 {
				hasText = true
			}
		case html.StartTagToken:
			if !inlineTags[t.tag] {
				return 0, false
			}
			if t.tag != "br" {
				if len(stack) >= 256 {
					return 0, false
				}
				stack = append(stack, t.tag)
			}
		case html.SelfClosingTagToken:
			if t.tag != "br" {
				return 0, false
			}
		case html.EndTagToken:
			if len(stack) == 0 {
				if t.tag == open && hasText {
					return j, true
				}
				return 0, false
			}
			if stack[len(stack)-1] != t.tag {
				return 0, false
			}
			stack = stack[:len(stack)-1]
		default: // comments, doctypes
			return 0, false
		}
	}
	return 0, false
}

func isVoid(tag string) bool {
	switch tag {
	case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "source", "track", "wbr":
		return true
	}
	return false
}

// Element returns the editable element with id node.
func (s *Source) Element(node int) (Element, bool) {
	if node < 0 || node >= len(s.Elements) {
		return Element{}, false
	}
	return s.Elements[node], true
}

// TextOf returns the text content of an HTML fragment as a browser's
// textContent would read it, with whitespace runs collapsed and trimmed.
func TextOf(fragment []byte) string {
	z := html.NewTokenizer(bytes.NewReader(fragment))
	var b strings.Builder
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		if tt == html.TextToken {
			b.Write(z.Text())
		}
	}
	return CollapseSpace(b.String())
}

// CollapseSpace collapses whitespace runs to one space and trims the ends.
func CollapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// InnerText returns the collapsed text of an editable element.
func (s *Source) InnerText(e Element) string {
	return TextOf(s.src[e.Inner[0]:e.Inner[1]])
}

// Rewrite returns the source as served: editable elements carry
// data-flats-node attributes and head holds the host's insert. The stored
// source is not changed.
func (s *Source) Rewrite(insert string) []byte {
	var b bytes.Buffer
	b.Grow(len(s.src) + len(insert) + 24*len(s.Elements))
	last := 0
	inserted := false
	emitInsert := func(at int) {
		if !inserted && at >= s.Insert {
			b.Write(s.src[last:s.Insert])
			last = s.Insert
			b.WriteString(insert)
			inserted = true
		}
	}
	for _, e := range s.Elements {
		emitInsert(e.StartTag[0])
		tagEnd := e.StartTag[1] - 1 // the '>'
		if tagEnd > e.StartTag[0] && s.src[tagEnd-1] == '/' {
			tagEnd--
		}
		b.Write(s.src[last:tagEnd])
		b.WriteString(` data-flats-node="`)
		b.WriteString(strconv.Itoa(e.Node))
		b.WriteByte('"')
		last = tagEnd
	}
	emitInsert(len(s.src))
	b.Write(s.src[last:])
	return b.Bytes()
}

// Splice returns src with the byte range r replaced by repl.
func Splice(src []byte, r [2]int, repl []byte) []byte {
	var out []byte
	out = append(out, src[:r[0]]...)
	out = append(out, repl...)
	return append(out, src[r[1]:]...)
}

// maxEditHTML bounds the content of one text edit.
const maxEditHTML = 64 << 10

// SanitizeInline parses fragment as the content of an element with tag
// parent and keeps only the inline allow list (with class and style
// attributes). Other elements are unwrapped and comments are dropped. Text
// keeps HTML whitespace semantics: a line break is an explicit <br>, never
// a newline in text, which HTML renders as a space.
func SanitizeInline(parent, fragment string) (string, error) {
	if len(fragment) > maxEditHTML {
		return "", fmt.Errorf("edited text is %d bytes, over the %d-byte limit", len(fragment), maxEditHTML)
	}
	ctx := &html.Node{Type: html.ElementNode, Data: parent, DataAtom: atom.Lookup([]byte(parent))}
	nodes, err := html.ParseFragment(strings.NewReader(fragment), ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			b.WriteString(html.EscapeString(n.Data))
		case html.ElementNode:
			tag := strings.ToLower(n.Data)
			if !inlineTags[tag] {
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c)
				}
				return
			}
			b.WriteByte('<')
			b.WriteString(tag)
			for _, a := range n.Attr {
				if a.Namespace == "" && (a.Key == "class" || a.Key == "style") {
					b.WriteByte(' ')
					b.WriteString(a.Key)
					b.WriteString(`="`)
					b.WriteString(html.EscapeString(a.Val))
					b.WriteByte('"')
				}
			}
			b.WriteByte('>')
			if tag == "br" {
				return
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			b.WriteString("</")
			b.WriteString(tag)
			b.WriteByte('>')
		}
	}
	for _, n := range nodes {
		walk(n)
	}
	out := b.String()
	if len(out) > maxEditHTML {
		return "", fmt.Errorf("edited text is %d bytes, over the %d-byte limit", len(out), maxEditHTML)
	}
	return out, nil
}
