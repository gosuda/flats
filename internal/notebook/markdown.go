package notebook

import (
	"fmt"
	"html"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	ext "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

// Warning describes a deterministic conversion loss at a source line or item path.
type Warning struct {
	Code    string `json:"code"`
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitempty"`
	Message string `json:"message"`
}

// DatabaseSnapshot supplies the database contents for a Markdown page read.
type DatabaseSnapshot struct {
	Database Item
	Schema   Schema
	Views    []Item
	Rows     []RowValues
}

var markdownMarker = regexp.MustCompile(`^<!-- flats:block (b_[a-z2-7]{26}) -->$`)
var markdownFootnote = regexp.MustCompile(`\[\^[^]\n]+\]`)

// ParseMarkdown creates a deterministic, source-ordered fragment; metadata never reuses identities.
func ParseMarkdown(src string, place Placement, resolveAsset func(path string) (hash string, ok bool)) ([]Operation, []Warning, error) {
	return parseMarkdownAtDepth(src, place, resolveAsset, 0)
}

// parentDepth comes from the current destination tree, not the pasted fragment.
func parseMarkdownAtDepth(src string, place Placement, resolveAsset func(path string) (hash string, ok bool), parentDepth int) ([]Operation, []Warning, error) {
	if parentDepth < 0 || parentDepth >= MaxBlockDepth {
		return nil, nil, fmt.Errorf("Markdown destination exceeds block depth")
	}
	if !utf8.ValidString(src) {
		return nil, nil, fmt.Errorf("Markdown must be UTF-8")
	}
	p := markdownParser{place: place, resolve: resolveAsset, maxDepth: MaxBlockDepth - parentDepth, ops: []Operation{}, warnings: []Warning{}, last: map[string]Target{}, assets: map[*ast.Image]string{}}
	parser := goldmark.New(goldmark.WithExtensions(extension.Table, extension.TaskList, extension.Strikethrough)).Parser()
	original := []byte(src)
	protected := [][2]int{}
	ast.Walk(parser.Parse(text.NewReader(original)), func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		if !enter {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.HTMLBlock:
			if markdownMarker.MatchString(strings.TrimSpace(string(n.Text(original)))) {
				break
			}
			for i := 0; i < n.Lines().Len(); i++ {
				s := n.Lines().At(i)
				protected = append(protected, [2]int{s.Start, s.Stop})
			}
			if v.HasClosure() {
				protected = append(protected, [2]int{v.ClosureLine.Start, v.ClosureLine.Stop})
			}
		case *ast.CodeBlock, *ast.FencedCodeBlock:
			for i := 0; i < n.Lines().Len(); i++ {
				s := n.Lines().At(i)
				protected = append(protected, [2]int{s.Start, s.Stop})
			}
		case *ast.CodeSpan:
			for c := n.FirstChild(); c != nil; c = c.NextSibling() {
				if text, ok := c.(*ast.Text); ok {
					protected = append(protected, [2]int{text.Segment.Start, text.Segment.Stop})
				}
			}
		}
		return ast.WalkContinue, nil
	})
	isProtected := func(offset int) bool {
		for _, span := range protected {
			if offset >= span[0] && offset < span[1] {
				return true
			}
		}
		return false
	}
	lines := strings.Split(src, "\n")
	offset := 0
	for i, line := range lines {
		start := offset
		offset += len(line) + 1
		at := len(line) - len(strings.TrimLeft(line, " 	"))
		if m := markdownMarker.FindStringSubmatch(strings.TrimLeft(line, " 	")); m != nil && ID(m[1]).ValidateKind(BlockKind) == nil && !isProtected(start+at) {
			lines[i] = ""
			continue
		}
		// CommonMark otherwise consumes footnote definitions as invisible link definitions.
		matches := markdownFootnote.FindAllStringIndex(line, -1)
		for j := len(matches) - 1; j >= 0; j-- {
			match := matches[j]
			backslashes := 0
			for at := match[0] - 1; at >= 0 && line[at] == '\\'; at-- {
				backslashes++
			}
			if isProtected(start+match[0]) || backslashes%2 == 1 {
				continue
			}
			p.warnings = append(p.warnings, Warning{Code: "footnote", Line: i + 1, Message: "Footnotes remain literal paragraph text."})
			line = line[:match[0]] + "\\" + line[match[0]:]
		}
		lines[i] = line
	}
	p.src = []byte(strings.Join(lines, "\n"))
	doc := parser.Parse(text.NewReader(p.src))
	err := ast.Walk(doc, func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		image, ok := n.(*ast.Image)
		if !enter || !ok {
			return ast.WalkContinue, nil
		}
		path := markdownDecode(string(image.Destination))
		u, urlErr := url.Parse(path)
		if urlErr != nil || u.IsAbs() || u.Host != "" || strings.HasPrefix(path, "//") {
			return ast.WalkStop, fmt.Errorf("unknown Markdown image path %q", path)
		}
		hash, known := "", false
		if p.resolve != nil {
			hash, known = p.resolve(path)
			if !known && strings.HasPrefix(path, "assets/") {
				alias := strings.TrimPrefix(path, "assets/")
				if (Content{Kind: Image, Asset: alias, Caption: RichText{}}).Validate() == nil {
					hash, known = p.resolve(alias)
				}
			}
		}
		if !known {
			return ast.WalkStop, fmt.Errorf("unknown Markdown image path %q", path)
		}
		if err := (Content{Kind: Image, Asset: hash, Caption: RichText{}}).Validate(); err != nil {
			return ast.WalkStop, fmt.Errorf("Markdown image path %q: %w", path, err)
		}
		p.assets[image] = hash
		return ast.WalkContinue, nil
	})
	if err != nil {
		return nil, nil, err
	}
	for n := doc.FirstChild(); n != nil; n = n.NextSibling() {
		if err := p.block(n, nil); err != nil {
			return nil, nil, err
		}
	}
	sort.SliceStable(p.warnings, func(i, j int) bool { return p.warnings[i].Line < p.warnings[j].Line })
	return p.ops, p.warnings, nil
}

type markdownParser struct {
	src      []byte
	place    Placement
	resolve  func(string) (string, bool)
	ops      []Operation
	warnings []Warning
	last     map[string]Target
	assets   map[*ast.Image]string
	next     int
	maxDepth int
}

func (p *markdownParser) name() string { p.next++; return "$md" + strconv.Itoa(p.next) }
func (p *markdownParser) placement(parents []Target) Placement {
	place := p.place
	key := ""
	if len(parents) > 0 {
		parent := parents[len(parents)-1]
		place.Parent = &parent
		place.Before, place.After = nil, nil
		key = parent.Tmp
	}
	if last, ok := p.last[key]; ok {
		place.Before = nil
		place.After = &last
	}
	return place
}
func (p *markdownParser) remember(parents []Target, ref string) {
	key := ""
	if len(parents) > 0 {
		key = parents[len(parents)-1].Tmp
	}
	p.last[key] = Target{Tmp: ref}
}
func (p *markdownParser) add(c Content, parents []Target) (Target, error) {
	if c.Kind.IsText() {
		var err error
		c.Text, err = CanonicalizeRichText(c.Text)
		if err != nil {
			return Target{}, err
		}
	}
	if err := c.Validate(); err != nil {
		return Target{}, fmt.Errorf("Markdown %s: %w", c.Kind, err)
	}
	ref := p.name()
	p.ops = append(p.ops, CreateBlockOp{CreateFields: CreateFields{Ref: ref}, Place: p.placement(parents), Content: c})
	p.remember(parents, ref)
	return Target{Tmp: ref}, nil
}
func (p *markdownParser) line(n ast.Node) int {
	start := 0
	if raw, ok := n.(*ast.RawHTML); ok && raw.Segments.Len() > 0 {
		start = raw.Segments.At(0).Start
	} else if n.Type() == ast.TypeBlock && n.Lines().Len() > 0 {
		start = n.Lines().At(0).Start
	} else {
		ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
			if entering {
				if t, ok := c.(*ast.Text); ok {
					start = t.Segment.Start
					return ast.WalkStop, nil
				}
			}
			return ast.WalkContinue, nil
		})
	}
	return 1 + strings.Count(string(p.src[:start]), "\n")
}
func (p *markdownParser) warn(code string, n ast.Node, message string) {
	p.warnings = append(p.warnings, Warning{Code: code, Line: p.line(n), Message: message})
}
func (p *markdownParser) raw(n ast.Node) string {
	var b strings.Builder
	for i := 0; i < n.Lines().Len(); i++ {
		s := n.Lines().At(i)
		b.Write(s.Value(p.src))
	}
	return strings.TrimSuffix(b.String(), "\n")
}
func (p *markdownParser) block(n ast.Node, parents []Target) error {
	if len(parents) >= p.maxDepth && n.Kind() != ast.KindList {
		parents = parents[:p.maxDepth-1]
		p.warn("nesting_flattened", n, "Excess list nesting becomes siblings at depth six.")
	}
	switch v := n.(type) {
	case *ast.Heading:
		level := v.Level
		if level > 3 {
			level = 3
			p.warn("heading_clamped", n, "Headings deeper than three become heading three.")
		}
		return p.textBlocks(n, Content{Kind: []ContentKind{Heading1, Heading2, Heading3}[level-1]}, parents)
	case *ast.Paragraph, *ast.TextBlock:
		return p.textBlocks(n, Content{Kind: Paragraph}, parents)
	case *ast.FencedCodeBlock:
		_, err := p.add(Content{Kind: Code, Code: p.raw(n), Language: string(v.Language(p.src))}, parents)
		return err
	case *ast.CodeBlock:
		_, err := p.add(Content{Kind: Code, Code: p.raw(n)}, parents)
		return err
	case *ast.ThematicBreak:
		_, err := p.add(Content{Kind: Divider}, parents)
		return err
	case *ast.List:
		for item := n.FirstChild(); item != nil; item = item.NextSibling() {
			kind := Bullet
			if v.IsOrdered() {
				kind = Numbered
			}
			body := item.FirstChild()
			c := Content{Kind: kind, Text: RichText{}}
			if body != nil && (body.Kind() == ast.KindTextBlock || body.Kind() == ast.KindParagraph) {
				if check, ok := body.FirstChild().(*ext.TaskCheckBox); ok {
					c.Kind = Todo
					c.Checked = check.IsChecked
				}
			}
			legal := parents
			if len(legal) >= p.maxDepth {
				legal = legal[:p.maxDepth-1]
				p.warn("nesting_flattened", item, "Excess list nesting becomes siblings at depth six.")
			}
			index := len(p.ops)
			if body != nil && (body.Kind() == ast.KindTextBlock || body.Kind() == ast.KindParagraph) {
				if err := p.textBlocks(body, c, legal); err != nil {
					return err
				}
			} else {
				if _, err := p.add(c, legal); err != nil {
					return err
				}
			}
			ref := Target{Tmp: p.ops[index].(CreateBlockOp).Ref}
			children := append(append([]Target{}, legal...), ref)
			for child := item.FirstChild(); child != nil; child = child.NextSibling() {
				if child == body && (child.Kind() == ast.KindTextBlock || child.Kind() == ast.KindParagraph) {
					continue
				}
				if err := p.block(child, children); err != nil {
					return err
				}
			}
		}
		return nil
	case *ast.Blockquote:
		first := n.FirstChild()
		if first == nil {
			_, err := p.add(Content{Kind: Quote, Text: RichText{}}, parents)
			return err
		}
		if first.Kind() == ast.KindParagraph {
			if err := p.textBlocks(first, Content{Kind: Quote}, parents); err != nil {
				return err
			}
			first = first.NextSibling()
		}
		for child := first; child != nil; child = child.NextSibling() {
			p.warn("quote_flattened", child, "Nested quote content becomes sibling blocks.")
			if err := p.block(child, parents); err != nil {
				return err
			}
		}
		return nil
	case *ext.Table:
		return p.table(v, parents)
	case *ast.HTMLBlock:
		raw := p.raw(n)
		if v.HasClosure() {
			raw += "\n" + string(v.ClosureLine.Value(p.src))
			raw = strings.TrimSuffix(raw, "\n")
		}
		p.warn("raw_html", n, "Raw HTML remains literal paragraph text.")
		_, err := p.add(Content{Kind: Paragraph, Text: RichText{{Text: raw, Marks: []Mark{}}}}, parents)
		return err
	default:
		p.warn("unsupported", n, "Unsupported Markdown remains literal paragraph text.")
		_, err := p.add(Content{Kind: Paragraph, Text: RichText{{Text: p.raw(n), Marks: []Mark{}}}}, parents)
		return err
	}
}

var markdownAlerts = []struct {
	name string
	tone Tone
}{
	{name: "NOTE", tone: InfoTone}, {name: "TIP", tone: SuccessTone}, {name: "IMPORTANT", tone: NeutralTone}, {name: "WARNING", tone: WarningTone}, {name: "CAUTION", tone: DangerTone},
}

func markdownPlain(runs RichText) string {
	var b strings.Builder
	for _, r := range runs {
		b.WriteString(r.Text)
	}
	return b.String()
}
func markdownTrimPrefix(runs RichText, n int) RichText {
	out := RichText{}
	for _, r := range runs {
		if n >= len(r.Text) {
			n -= len(r.Text)
			continue
		}
		r.Text = r.Text[n:]
		n = 0
		out = append(out, r)
	}
	return out
}

func (p *markdownParser) inline(n ast.Node) (RichText, error) {
	return p.inlineImages(n, nil)
}

func (p *markdownParser) inlineImages(n ast.Node, image func(*ast.Image, RichText) error) (RichText, error) {
	out := RichText{}
	var walk func(ast.Node, []Mark, *Link) error
	walk = func(n ast.Node, marks []Mark, link *Link) error {
		appendText := func(s string) { out = append(out, Run{Text: s, Marks: append([]Mark{}, marks...), Link: link}) }
		switch v := n.(type) {
		case *ast.Text:
			s := string(v.Segment.Value(p.src))
			if !v.IsRaw() {
				s = markdownDecode(s)
			}
			appendText(s)
			if v.HardLineBreak() {
				appendText("\n")
			} else if v.SoftLineBreak() {
				// Rich-text newlines are visible breaks; soft wraps are ordinary prose spaces.
				appendText(" ")
			}
			return nil
		case *ast.String:
			appendText(string(v.Value))
			return nil
		case *ast.CodeSpan:
			var b strings.Builder
			for c := n.FirstChild(); c != nil; c = c.NextSibling() {
				b.Write(c.Text(p.src))
			}
			out = append(out, Run{Text: b.String(), Marks: append(append([]Mark{}, marks...), InlineCode), Link: link})
			return nil
		case *ast.Emphasis:
			mark := Italic
			if v.Level == 2 {
				mark = Bold
			}
			marks = append(append([]Mark{}, marks...), mark)
		case *ext.Strikethrough:
			marks = append(append([]Mark{}, marks...), Strike)
		case *ast.Link:
			dest := markdownDecode(string(v.Destination))
			l := Link{URL: dest}
			if strings.HasPrefix(dest, "#p_") {
				l = Link{Page: ID(strings.TrimPrefix(dest, "#"))}
			}
			if err := l.Validate(); err != nil {
				p.warn("unsupported_link", n, "Unsupported link destinations remain literal paragraph text.")
				appendText("[" + markdownDecode(string(v.Text(p.src))) + "](" + dest + ")")
				return nil
			}
			link = &l
		case *ast.AutoLink:
			l := Link{URL: string(v.URL(p.src))}
			if err := l.Validate(); err != nil {
				p.warn("unsupported_link", n, "Unsupported link destinations remain literal paragraph text.")
				appendText("<" + string(v.Label(p.src)) + ">")
				return nil
			}
			out = append(out, Run{Text: string(v.Label(p.src)), Marks: append([]Mark{}, marks...), Link: &l})
			return nil
		case *ast.RawHTML:
			p.warn("raw_html", n, "Raw HTML remains literal paragraph text.")
			appendText(string(v.Text(p.src)))
			return nil
		case *ext.TaskCheckBox:
			return nil
		case *ast.Image:
			if image != nil {
				if err := image(v, out); err != nil {
					return err
				}
				out = RichText{}
				return nil
			}
			appendText(markdownDecode(string(v.Text(p.src))))
			return nil
		}
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			if err := walk(c, marks, link); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(n, []Mark{}, nil); err != nil {
		return nil, err
	}
	return CanonicalizeRichText(out)
}

func (p *markdownParser) textBlocks(n ast.Node, c Content, parents []Target) error {
	first := true
	emit := func(runs RichText) error {
		if len(runs) == 0 && !(first && c.Kind.HoldsChildren()) {
			return nil
		}
		next := c
		next.Text = runs
		if first && c.Kind == Quote {
			plain := markdownPlain(runs)
			header := ""
			if n.Lines().Len() > 0 {
				line := n.Lines().At(0)
				header = strings.TrimSpace(string(line.Value(p.src)))
			}
			for _, alert := range markdownAlerts {
				prefix := "[!" + alert.name + "]"
				if plain == prefix || strings.HasPrefix(plain, prefix+"\n") || header == prefix && strings.HasPrefix(plain, prefix+" ") {
					next.Kind, next.Tone = Callout, alert.tone
					next.Text = markdownTrimPrefix(runs, len(prefix))
					if body := markdownPlain(next.Text); strings.HasPrefix(body, "\n") || strings.HasPrefix(body, " ") {
						next.Text = markdownTrimPrefix(next.Text, 1)
					}
					break
				}
			}
		}
		first = false
		_, err := p.add(next, parents)
		return err
	}
	split := false
	runs, err := p.inlineImages(n, func(image *ast.Image, before RichText) error {
		split = true
		if err := emit(markdownTrimBoundary(before)); err != nil {
			return err
		}
		first = false
		_, err := p.add(Content{Kind: Image, Asset: p.assets[image], Alt: markdownDecode(string(image.Text(p.src))), Caption: RichText{}}, parents)
		return err
	})
	if err != nil {
		return err
	}
	if split {
		runs = markdownTrimBoundary(runs)
	}
	return emit(runs)
}

func markdownTrimBoundary(runs RichText) RichText {
	runs = append(RichText{}, runs...)
	for len(runs) > 0 && !slices.Contains(runs[0].Marks, InlineCode) {
		runs[0].Text = strings.TrimLeftFunc(runs[0].Text, unicode.IsSpace)
		if runs[0].Text != "" {
			break
		}
		runs = runs[1:]
	}
	for len(runs) > 0 && !slices.Contains(runs[len(runs)-1].Marks, InlineCode) {
		last := len(runs) - 1
		runs[last].Text = strings.TrimRightFunc(runs[last].Text, unicode.IsSpace)
		if runs[last].Text != "" {
			break
		}
		runs = runs[:last]
	}
	return runs
}

var markdownDecodeToken = regexp.MustCompile(`\\[[:punct:]]|&(?:#[xX][0-9a-fA-F]+|#[0-9]+|[A-Za-z][A-Za-z0-9]+);`)

func markdownDecode(s string) string {
	return markdownDecodeToken.ReplaceAllStringFunc(s, func(token string) string {
		if token[0] == '\\' {
			return token[1:]
		}
		return html.UnescapeString(token)
	})
}

func (p *markdownParser) table(table *ext.Table, parents []Target) error {
	rows := [][]string{}
	for row := table.FirstChild(); row != nil; row = row.NextSibling() {
		cells := []string{}
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			runs, err := p.inline(cell)
			if err != nil {
				return err
			}
			lossy := false
			for _, r := range runs {
				if len(r.Marks) > 0 || r.Link != nil {
					lossy = true
				}
			}
			ast.Walk(cell, func(n ast.Node, enter bool) (ast.WalkStatus, error) {
				if enter && n.Kind() == ast.KindImage {
					lossy = true
				}
				return ast.WalkContinue, nil
			})
			if lossy {
				p.warn("table_cell_plain", cell, "Table cell formatting becomes visible plain text.")
			}
			cells = append(cells, markdownPlain(runs))
		}
		rows = append(rows, cells)
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return nil
	}
	ref, block := p.name(), p.name()
	title := rows[0][0]
	p.ops = append(p.ops, CreateDatabaseOp{CreateFields: CreateFields{Ref: ref}, BlockRef: block, Place: p.placement(parents), Title: "", TitlePropertyName: &title})
	p.remember(parents, block)
	props := []Target{{Tmp: ref + ".title"}}
	seen := map[string]bool{title: true}
	for _, name := range rows[0][1:] {
		original := name
		for suffix := 2; seen[name]; suffix++ {
			name = original + " (" + strconv.Itoa(suffix) + ")"
		}
		if name != original {
			p.warn("table_header_renamed", table, "Duplicate table property names receive a numeric suffix.")
		}
		seen[name] = true
		prop := p.name()
		props = append(props, Target{Tmp: prop})
		p.ops = append(p.ops, CreatePropertyOp{CreateFields: CreateFields{Ref: prop}, Database: Target{Tmp: ref}, Definition: PropertyDef{Name: name, Type: TextType}})
	}
	for _, row := range rows[1:] {
		cells := []CellInput{}
		for i, prop := range props {
			value := ""
			if i < len(row) {
				value = row[i]
			}
			cells = append(cells, CellInput{Property: prop, Value: CellInputValue{Text: &value}})
		}
		p.ops = append(p.ops, CreateRowOp{CreateFields: CreateFields{Ref: p.name()}, Database: Target{Tmp: ref}, Cells: cells})
	}
	return nil
}
