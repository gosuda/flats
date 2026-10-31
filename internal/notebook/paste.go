package notebook

import (
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

// ParseHTMLPaste converts clipboard HTML without executing it or loading URLs.
// Asset admission and block creation use the same rules as Markdown import.
func ParseHTMLPaste(src string, place Placement, resolveAsset func(string) (string, bool)) ([]Operation, []Warning, error) {
	root, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return nil, nil, fmt.Errorf("parse HTML paste: %w", err)
	}
	p := htmlPaste{warnings: []Warning{}}
	md := strings.TrimSpace(p.render(root, 0))
	ops, warnings, err := ParseMarkdown(md, place, resolveAsset)
	return ops, append(p.warnings, warnings...), err
}

type htmlPaste struct {
	warnings []Warning
	inTable  bool
}

func (p *htmlPaste) warn(code, message string) {
	p.warnings = append(p.warnings, Warning{Code: code, Message: message})
}

func htmlPasteDrop(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	switch n.Data {
	case "script", "style", "iframe", "frame", "frameset", "object", "embed", "applet", "template", "svg", "math", "noscript", "link", "meta", "base":
		return true
	}
	return false
}

func (p *htmlPaste) children(n *html.Node, marks uint8) string {
	var out strings.Builder
	children := htmlPasteChildren(n)
	for i := 0; i < len(children); i++ {
		child := children[i]
		bit, marker := htmlPasteMark(child)
		if bit == 0 {
			out.WriteString(p.render(child, marks))
			continue
		}
		// Adjacent equal delimiters would become literal Markdown punctuation.
		var body strings.Builder
		for {
			if bit == 8 {
				body.WriteString(p.plain(child))
			} else {
				body.WriteString(p.children(child, marks|bit))
			}
			if i+1 == len(children) {
				break
			}
			nextBit, _ := htmlPasteMark(children[i+1])
			if nextBit != bit {
				break
			}
			i++
			child = children[i]
		}
		if bit == 8 {
			out.WriteString(p.inlineCode(body.String()))
		} else if marks&bit != 0 {
			out.WriteString(body.String())
		} else {
			out.WriteString(htmlPasteWrap(body.String(), marker))
		}
	}
	return out.String()
}

func htmlPasteChildren(n *html.Node) []*html.Node {
	var children []*html.Node
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode {
			switch child.Data {
			case "span", "font", "u", "small", "big", "label", "mark", "abbr", "time":
				children = append(children, htmlPasteChildren(child)...)
				continue
			}
		}
		children = append(children, child)
	}
	return children
}

func (p *htmlPaste) render(n *html.Node, marks uint8) string {
	if htmlPasteDrop(n) {
		p.warn("html_unsafe_element", "Dropped inert HTML element <"+n.Data+"> and its contents.")
		return ""
	}
	if n.Type == html.TextNode {
		return htmlPasteEscape(htmlPasteSpace(n.Data))
	}
	if n.Type == html.CommentNode || n.Type == html.DoctypeNode {
		return ""
	}
	if n.Type != html.ElementNode {
		return p.children(n, marks)
	}
	switch n.Data {
	case "title":
		return ""
	case "h1", "h2", "h3", "h4", "h5", "h6":
		return htmlPasteBlock(strings.Repeat("#", int(n.Data[1]-'0')) + " " + strings.TrimSpace(p.children(n, marks)))
	case "p", "div", "section", "article", "header", "footer", "main", "aside", "nav", "address", "figure", "figcaption", "dl", "dt", "dd":
		return htmlPasteBlock(p.children(n, marks))
	case "br":
		return "  \n"
	case "hr":
		return htmlPasteBlock("---")
	case "strong", "b", "em", "i", "del", "s", "strike":
		bit, marker := htmlPasteMark(n)
		body := p.children(n, marks|bit)
		if marks&bit != 0 {
			return body
		}
		return htmlPasteWrap(body, marker)
	case "code", "kbd", "samp":
		return p.inlineCode(p.plain(n))
	case "pre":
		body := p.plain(n)
		language := ""
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			if child.Type != html.ElementNode || child.Data != "code" {
				continue
			}
			for _, class := range strings.Fields(htmlPasteAttr(child, "class")) {
				if strings.HasPrefix(class, "language-") {
					candidate := strings.TrimPrefix(class, "language-")
					if strings.IndexFunc(candidate, func(r rune) bool {
						return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_+.-", r)
					}) == -1 {
						language = candidate
					}
					break
				}
			}
			break
		}
		fence := htmlPasteFence(body, 3)
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		return htmlPasteBlock(fence + language + "\n" + body + fence)
	case "a":
		body := p.children(n, marks)
		href := htmlPasteAttr(n, "href")
		if href == "" {
			return body
		}
		internal := strings.HasPrefix(href, "#p_") && ID(strings.TrimPrefix(href, "#")).ValidateKind(PageKind) == nil
		if !internal && validateURL(href, true) != nil {
			p.warn("html_unsafe_url", "Dropped an unsupported HTML link destination.")
			return body
		}
		return "[" + body + "](" + htmlPasteDestination(href) + ")"
	case "img":
		src := htmlPasteAttr(n, "src")
		alt := htmlPasteEscape(htmlPasteSpace(htmlPasteAttr(n, "alt")))
		if src == "" {
			p.warn("html_image_source", "HTML image without a source became its alternative text.")
			return alt
		}
		return "![" + alt + "](" + htmlPasteDestination(src) + ")"
	case "ul", "ol":
		var out strings.Builder
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			if child.Type != html.ElementNode || child.Data != "li" {
				if child.Type != html.TextNode || strings.TrimSpace(child.Data) != "" {
					out.WriteString(p.render(child, marks))
				}
				continue
			}
			marker := "- "
			if n.Data == "ol" {
				marker = "1. "
			}
			body := strings.TrimSpace(p.children(child, marks))
			indent := strings.Repeat(" ", len(marker))
			if checkbox := htmlPasteCheckbox(child); checkbox != nil {
				task := "[ ] "
				for _, attr := range checkbox.Attr {
					if attr.Namespace == "" && attr.Key == "checked" {
						task = "[x] "
					}
				}
				body = task + body
			}
			out.WriteString(marker + strings.ReplaceAll(body, "\n", "\n"+indent) + "\n")
		}
		return htmlPasteBlock(out.String())
	case "blockquote":
		body := strings.TrimSpace(p.children(n, marks))
		return htmlPasteBlock("> " + strings.ReplaceAll(body, "\n", "\n> "))
	case "table":
		return p.table(n, marks)
	case "input":
		return ""
	default:
		return p.children(n, marks)
	}
}

func htmlPasteMark(n *html.Node) (uint8, string) {
	if n == nil || n.Type != html.ElementNode {
		return 0, ""
	}
	switch n.Data {
	case "b", "strong":
		return 1, "**"
	case "i", "em":
		return 2, "*"
	case "s", "strike", "del":
		return 4, "~~"
	case "code", "kbd", "samp":
		return 8, "`"
	}
	return 0, ""
}

func (p *htmlPaste) inlineCode(body string) string {
	body = strings.ReplaceAll(body, "\n", " ")
	if p.inTable {
		body = strings.ReplaceAll(body, "|", "\\|")
	}
	if body == "" {
		return ""
	}
	fence := htmlPasteFence(body, 1)
	if strings.HasPrefix(body, "`") || strings.HasSuffix(body, "`") || (strings.HasPrefix(body, " ") && strings.HasSuffix(body, " ") && strings.TrimSpace(body) != "") {
		body = " " + body + " "
	}
	return fence + body + fence
}

func (p *htmlPaste) plain(n *html.Node) string {
	if htmlPasteDrop(n) {
		p.warn("html_unsafe_element", "Dropped inert HTML element <"+n.Data+"> and its contents.")
		return ""
	}
	if n.Type == html.TextNode {
		return n.Data
	}
	if n.Type == html.CommentNode {
		return ""
	}
	if n.Type == html.ElementNode && n.Data == "br" {
		return "\n"
	}
	var out strings.Builder
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		out.WriteString(p.plain(child))
	}
	return out.String()
}

func (p *htmlPaste) table(n *html.Node, marks uint8) string {
	if p.inTable {
		p.warn("html_nested_table", "Dropped a nested HTML table.")
		return ""
	}
	p.inTable = true
	defer func() { p.inTable = false }()
	var rows [][]string
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node != n && node.Type == html.ElementNode && node.Data == "table" {
			p.warn("html_nested_table", "Dropped a nested HTML table.")
			return
		}
		if htmlPasteDrop(node) {
			p.render(node, marks)
			return
		}
		if node.Type == html.ElementNode && node.Data == "tr" {
			var cells []string
			for cell := node.FirstChild; cell != nil; cell = cell.NextSibling {
				if cell.Type == html.ElementNode && (cell.Data == "td" || cell.Data == "th") {
					value := strings.TrimSpace(htmlPasteSpace(p.children(cell, marks)))
					cells = append(cells, value)
				}
			}
			rows = append(rows, cells)
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	width := 0
	for _, row := range rows {
		if len(row) > width {
			width = len(row)
		}
	}
	if width == 0 {
		return ""
	}
	var out strings.Builder
	for i, row := range rows {
		out.WriteString("| ")
		for col := range width {
			if col < len(row) {
				out.WriteString(row[col])
			}
			out.WriteString(" | ")
		}
		out.WriteByte('\n')
		if i == 0 {
			out.WriteString("|" + strings.Repeat(" --- |", width) + "\n")
		}
	}
	return htmlPasteBlock(out.String())
}

func htmlPasteCheckbox(n *html.Node) *html.Node {
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode {
			if child.Data == "ul" || child.Data == "ol" || htmlPasteDrop(child) {
				continue
			}
			if child.Data == "input" && strings.EqualFold(htmlPasteAttr(child, "type"), "checkbox") {
				return child
			}
		}
		if found := htmlPasteCheckbox(child); found != nil {
			return found
		}
	}
	return nil
}

func htmlPasteAttr(n *html.Node, key string) string {
	for _, attr := range n.Attr {
		if attr.Namespace == "" && attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func htmlPasteSpace(s string) string {
	var out strings.Builder
	space := false
	for _, r := range s {
		if strings.ContainsRune(" \t\n\r\f", r) {
			if !space {
				out.WriteByte(' ')
			}
			space = true
		} else {
			out.WriteRune(r)
			space = false
		}
	}
	return out.String()
}

func htmlPasteEscape(s string) string {
	var out strings.Builder
	for _, r := range s {
		if strings.ContainsRune("\\`*_{}[]()#+-.!|<>~&=", r) {
			out.WriteByte('\\')
		}
		out.WriteRune(r)
	}
	return out.String()
}

func htmlPasteDestination(s string) string {
	// Entity-escape ampersands so Markdown cannot decode the URL a second time.
	s = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\n", "&#10;", "\r", "&#13;", "\t", "&#9;", " ", "&#32;", "\\", "&#92;").Replace(s)
	return "<" + s + ">"
}

func htmlPasteBlock(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return "\n\n" + strings.TrimSpace(s) + "\n\n"
}

func htmlPasteWrap(s, marker string) string {
	body := strings.TrimFunc(s, unicode.IsSpace)
	if body == "" {
		return s
	}
	start := strings.Index(s, body)
	return s[:start] + marker + body + marker + s[start+len(body):]
}

func htmlPasteFence(s string, min int) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(min, longest+1))
}
