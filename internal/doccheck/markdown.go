package doccheck

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	nethtml "golang.org/x/net/html"
)

// hashRE matches full Git object names and SHA-256 digests.
var hashRE = regexp.MustCompile(`\b(?:[0-9a-fA-F]{64}|[0-9a-fA-F]{40})\b`)

var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

// rawLink is a link destination as written in a Markdown file.
type rawLink struct {
	line int
	dest string
}

// document is what the checker needs from one Markdown file.
type document struct {
	anchors    map[string]bool
	links      []rawLink
	hashLines  []int
	lineStarts []int

	// Generated heading slugs, kept apart from explicit HTML anchors: an
	// <a name> does not shift the numbering of later headings.
	slugCounts   map[string]int
	headingSlugs map[string]bool

	// An open raw HTML <h1>-<h6>. Inline HTML splits it into several nodes,
	// so its text is collected across them until the closing tag.
	htmlHeading    *strings.Builder
	htmlHeadingTag string
}

// parseDocument parses Markdown with a CommonMark/GFM parser and collects
// heading and HTML anchors, link and image destinations (inline, reference
// and raw HTML), and the lines outside code blocks that hold hash literals.
func parseDocument(src []byte) *document {
	d := &document{
		anchors:      map[string]bool{},
		lineStarts:   lineStarts(src),
		slugCounts:   map[string]int{},
		headingSlugs: map[string]bool{},
	}
	pc := parser.NewContext()
	root := md.Parser().Parse(text.NewReader(src), parser.WithContext(pc))

	seen := map[string]bool{}
	var codeLines [][2]int
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Heading:
			d.addHeading(headingText(n, src))
		case *ast.Text:
			if d.htmlHeading != nil {
				d.htmlHeading.WriteString(decodeText(n.Segment.Value(src)))
			}
		case *ast.CodeSpan:
			if d.htmlHeading != nil {
				d.htmlHeading.WriteString(codeSpanText(n, src))
				return ast.WalkSkipChildren, nil
			}
		case *ast.Link:
			d.addLink(n, decodeText(n.Destination), seen)
		case *ast.Image:
			d.addLink(n, decodeText(n.Destination), seen)
		case *ast.RawHTML:
			var b bytes.Buffer
			for i := 0; i < n.Segments.Len(); i++ {
				seg := n.Segments.At(i)
				b.Write(seg.Value(src))
			}
			d.scanHTML(b.Bytes(), d.line(segmentStart(n.Segments)))
		case *ast.HTMLBlock:
			var b bytes.Buffer
			for i := 0; i < n.Lines().Len(); i++ {
				seg := n.Lines().At(i)
				b.Write(seg.Value(src))
			}
			if n.HasClosure() {
				b.Write(n.ClosureLine.Value(src))
			}
			d.scanHTML(b.Bytes(), d.line(segmentStart(n.Lines())))
		case *ast.FencedCodeBlock, *ast.CodeBlock:
			if lines := n.Lines(); lines.Len() > 0 {
				codeLines = append(codeLines, [2]int{
					d.line(lines.At(0).Start),
					d.line(lines.At(lines.Len() - 1).Start),
				})
			}
		}
		return ast.WalkContinue, nil
	})
	if d.htmlHeading != nil {
		d.addHeading(d.htmlHeading.String())
	}

	// Reference links resolve to ordinary Link nodes above. A definition
	// that no link uses is still checked, at the line that defines it.
	for _, ref := range pc.References() {
		dest := decodeText(ref.Destination())
		if !seen[dest] {
			seen[dest] = true
			d.links = append(d.links, rawLink{line: definitionLine(src, ref.Label()), dest: dest})
		}
	}

	for i, line := range bytes.Split(src, []byte("\n")) {
		n := i + 1
		if !inRanges(n, codeLines) && hashRE.Match(line) {
			d.hashLines = append(d.hashLines, n)
		}
	}
	sort.SliceStable(d.links, func(i, j int) bool { return d.links[i].line < d.links[j].line })
	return d
}

func (d *document) addLink(n ast.Node, dest string, seen map[string]bool) {
	seen[dest] = true
	d.links = append(d.links, rawLink{line: d.line(nodeStart(n)), dest: dest})
}

// addHeading records the anchor GitHub generates for a heading's rendered
// text, numbering repeats as github-slugger does.
func (d *document) addHeading(text string) {
	// An empty slug still counts: a second empty heading becomes "-1".
	slug := uniqueSlug(githubSlug(text), d.slugCounts, d.headingSlugs)
	d.headingSlugs[slug] = true
	if slug != "" {
		d.anchors[slug] = true
	}
}

// escapeOrEntityRE matches a backslash-escaped ASCII punctuation character
// or an entity or numeric character reference, which Markdown requires to
// end with a semicolon.
var escapeOrEntityRE = regexp.MustCompile("\\\\[!-/:-@\\[-`{-~]|&(?:#[0-9]{1,7}|#[xX][0-9a-fA-F]{1,6}|[A-Za-z][A-Za-z0-9]{0,31});")

// decodeText applies the backslash-escape and character reference decoding
// a Markdown renderer applies to text and link destinations, in one pass so
// decoded output is never decoded again. Percent-encoding is left for
// resolve.
func decodeText(b []byte) string {
	return escapeOrEntityRE.ReplaceAllStringFunc(string(b), func(m string) string {
		if m[0] == '\\' {
			return m[1:]
		}
		name := m[1 : len(m)-1]
		if name[0] != '#' {
			if e, ok := util.LookUpHTML5EntityByName(name); ok {
				return string(e.Characters)
			}
			return m
		}
		base, digits := 10, name[1:]
		if digits[0] == 'x' || digits[0] == 'X' {
			base, digits = 16, digits[1:]
		}
		n, err := strconv.ParseUint(digits, base, 32)
		if err != nil || n == 0 || n > unicode.MaxRune || (n >= 0xD800 && n <= 0xDFFF) {
			return "\uFFFD"
		}
		return string(rune(n))
	})
}

// scanHTML records id/name anchors, generated anchors for <h1>-<h6>
// headings, and a/img/link/script/source destinations in raw HTML that
// starts at line. Comments, including PROTECTED blocks, are skipped by the
// tokenizer.
func (d *document) scanHTML(b []byte, line int) {
	z := nethtml.NewTokenizer(bytes.NewReader(b))
	offset := 0
	for {
		tt := z.Next()
		tokenLine := line + bytes.Count(b[:offset], []byte("\n"))
		offset += len(z.Raw())
		switch tt {
		case nethtml.ErrorToken:
			return
		case nethtml.TextToken:
			if d.htmlHeading != nil {
				d.htmlHeading.Write(z.Text())
			}
		case nethtml.EndTagToken:
			if name, _ := z.TagName(); d.htmlHeading != nil && string(name) == d.htmlHeadingTag {
				d.addHeading(d.htmlHeading.String())
				d.htmlHeading = nil
			}
		case nethtml.StartTagToken, nethtml.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			tag := string(name)
			if tt == nethtml.StartTagToken && d.htmlHeading == nil && len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6' {
				d.htmlHeading, d.htmlHeadingTag = &strings.Builder{}, tag
			}
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				key, val := string(k), strings.TrimSpace(string(v))
				if val == "" {
					continue
				}
				if key == "id" || (key == "name" && tag == "a") {
					d.anchors[val] = true
				}
				if (key == "href" || key == "src") && (tag == "a" || tag == "img" || tag == "link" || tag == "script" || tag == "source") {
					d.links = append(d.links, rawLink{line: tokenLine, dest: val})
				}
			}
		}
	}
}

func (d *document) line(offset int) int {
	if offset < 0 {
		return 0
	}
	return sort.SearchInts(d.lineStarts, offset+1)
}

func lineStarts(src []byte) []int {
	starts := []int{0}
	for i, c := range src {
		if c == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

func segmentStart(s *text.Segments) int {
	if s == nil || s.Len() == 0 {
		return -1
	}
	return s.At(0).Start
}

// nodeStart returns the source offset of the first text inside an inline
// node, falling back to the start of its enclosing block.
func nodeStart(n ast.Node) int {
	start := -1
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch c := c.(type) {
		case *ast.Text:
			start = c.Segment.Start
			return ast.WalkStop, nil
		case *ast.RawHTML:
			start = segmentStart(c.Segments)
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	if start >= 0 {
		return start
	}
	for p := n.Parent(); p != nil; p = p.Parent() {
		if p.Type() == ast.TypeBlock && p.Lines().Len() > 0 {
			return p.Lines().At(0).Start
		}
	}
	return -1
}

// definitionLine finds the line of a reference definition by its label.
func definitionLine(src, label []byte) int {
	re, err := regexp.Compile(`(?im)^[ \t]{0,3}\[` + regexp.QuoteMeta(string(label)) + `\]:`)
	if err != nil {
		return 0
	}
	loc := re.FindIndex(src)
	if loc == nil {
		return 0
	}
	return bytes.Count(src[:loc[0]], []byte("\n")) + 1
}

func inRanges(n int, ranges [][2]int) bool {
	for _, r := range ranges {
		if n >= r[0] && n <= r[1] {
			return true
		}
	}
	return false
}

// headingText returns the text GitHub renders for a heading: code span
// contents and link labels are kept, markup and raw HTML tags are not.
func headingText(h *ast.Heading, src []byte) string {
	var b strings.Builder
	var walk func(n ast.Node)
	walk = func(n ast.Node) {
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			switch c := c.(type) {
			case *ast.Text:
				b.WriteString(decodeText(c.Segment.Value(src)))
				if c.SoftLineBreak() || c.HardLineBreak() {
					b.WriteByte(' ')
				}
			case *ast.String:
				b.Write(c.Value)
			case *ast.CodeSpan:
				b.WriteString(codeSpanText(c, src))
			case *ast.AutoLink:
				b.Write(c.Label(src))
			case *ast.RawHTML, *ast.Image:
			default:
				walk(c)
			}
		}
	}
	walk(h)
	return b.String()
}

// codeSpanText returns a code span's literal text as rendered: no
// escape or entity decoding, and line endings turned into spaces.
func codeSpanText(c *ast.CodeSpan, src []byte) string {
	var b strings.Builder
	for t := c.FirstChild(); t != nil; t = t.NextSibling() {
		if t, ok := t.(*ast.Text); ok {
			b.WriteString(strings.ReplaceAll(string(t.Segment.Value(src)), "\n", " "))
		}
	}
	return b.String()
}

// githubSlug converts heading text to the anchor GitHub generates: lower
// case, letters, marks, numbers, connector punctuation and hyphens kept,
// spaces turned into hyphens, everything else dropped.
func githubSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-', unicode.IsLetter(r), unicode.IsNumber(r), unicode.Is(unicode.M, r), unicode.Is(unicode.Pc, r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// uniqueSlug appends -1, -2, ... to repeated slugs as GitHub does.
func uniqueSlug(base string, counts map[string]int, used map[string]bool) string {
	for {
		n := counts[base]
		counts[base] = n + 1
		slug := base
		if n > 0 {
			slug = fmt.Sprintf("%s-%d", base, n)
		}
		if !used[slug] {
			return slug
		}
	}
}
