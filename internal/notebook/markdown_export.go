package notebook

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ExportMarkdown emits a page forest and deterministic references without mutating its input.
func ExportMarkdown(page Item, blocks []Item, dbs map[ID]DatabaseSnapshot) (string, []Ref, []Warning) {
	e := markdownExporter{page: page, dbs: dbs, children: map[ID][]Item{}, refs: []Ref{}, warnings: []Warning{}, seen: map[ID]bool{}}
	for _, item := range blocks {
		b, ok := item.Data.(Block)
		if !ok || b.Page != page.ID() {
			continue
		}
		parent := ID("")
		if b.Parent != nil {
			parent = *b.Parent
		}
		e.children[parent] = append(e.children[parent], item)
	}
	for parent := range e.children {
		sort.Slice(e.children[parent], func(i, j int) bool {
			a, b := e.children[parent][i], e.children[parent][j]
			ap, bp := a.Data.(Block).Position, b.Data.(Block).Position
			if ap != bp {
				return ap < bp
			}
			return a.ID() < b.ID()
		})
	}
	md := strings.TrimSpace(e.forest("", ""))
	if md != "" {
		md += "\n"
	}
	return md, e.refs, e.warnings
}

type markdownExporter struct {
	page     Item
	dbs      map[ID]DatabaseSnapshot
	children map[ID][]Item
	refs     []Ref
	warnings []Warning
	seen     map[ID]bool
}

func (e *markdownExporter) reference(item Item) {
	if item.Ref.ID != "" && !e.seen[item.ID()] {
		e.seen[item.ID()] = true
		e.refs = append(e.refs, item.Ref)
	}
}
func (e *markdownExporter) warn(code string, item Item, message string) {
	e.warnings = append(e.warnings, Warning{Code: code, Path: string(item.ID()), Message: message})
}
func (e *markdownExporter) forest(parent ID, indent string) string {
	var out strings.Builder
	for _, item := range e.children[parent] {
		e.reference(item)
		c := item.Data.(Block).Content
		out.WriteString(indent + "<!-- flats:block " + string(item.ID()) + " -->\n")
		body := markdownRich(c.Text)
		switch c.Kind {
		case Heading1:
			body = "# " + body
		case Heading2:
			body = "## " + body
		case Heading3:
			body = "### " + body
		case Bullet:
			body = "- " + body
		case Numbered:
			body = "1. " + body
		case Todo:
			mark := "[ ]"
			if c.Checked {
				mark = "[x]"
			}
			body = "- " + mark + " " + body
		case Quote:
			body = "> " + strings.ReplaceAll(body, "\n", "\n> ")
		case Callout:
			alert := "IMPORTANT"
			for _, a := range markdownAlerts {
				if a.tone == c.Tone {
					alert = a.name
					break
				}
			}
			body = "> [!" + alert + "]\n> " + strings.ReplaceAll(body, "\n", "\n> ")
		case Code:
			fence := htmlPasteFence(c.Code, 3)
			body = fence + c.Language + "\n" + c.Code
			if !strings.HasSuffix(body, "\n") {
				body += "\n"
			}
			body += fence
		case Divider:
			body = "---"
		case Image:
			body = "![" + markdownEscape(c.Alt) + "](<assets/" + c.Asset + ">)"
			if len(c.Caption) > 0 {
				e.warn("image_caption", item, "Image captions become following paragraphs.")
				body += "\n\n" + markdownRich(c.Caption)
			}
		case Toggle:
			e.warn("toggle", item, "Toggle summaries and children become ordinary blocks.")
		case PageLink:
			label := "Missing page"
			if c.Target == e.page.ID() {
				if p, ok := e.page.Data.(Page); ok {
					label = p.Title
				}
			}
			body = "[" + markdownEscape(label) + "](#" + string(c.Target) + ")"
			e.warn("page_link", item, "Page-link blocks become links; unavailable target titles use Missing page.")
		case InlineDatabase:
			body = e.table(item, c.Database)
		}
		continuation := indent
		if c.Kind == Bullet || c.Kind == Todo {
			continuation += "  "
		}
		if c.Kind == Numbered {
			continuation += "   "
		}
		out.WriteString(indent + strings.ReplaceAll(body, "\n", "\n"+continuation) + "\n\n")
		childIndent := indent
		if c.Kind.HoldsChildren() && c.Kind != Toggle {
			childIndent += "  "
			if c.Kind == Numbered {
				childIndent += " "
			}
		}
		out.WriteString(e.forest(item.ID(), childIndent))
	}
	return out.String()
}

func markdownEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("\\`*_{}[]()#+-.!|<>~&=", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

type markdownDelimiter struct{ key, open, close string }

func markdownRich(runs RichText) string {
	var out strings.Builder
	active := []markdownDelimiter{}
	for _, r := range runs {
		next := []markdownDelimiter{}
		if r.Link != nil {
			dest := r.Link.URL
			if r.Link.Page != "" {
				dest = "#" + string(r.Link.Page)
			}
			next = append(next, markdownDelimiter{key: "link:" + dest, open: "[", close: "](" + htmlPasteDestination(dest) + ")"})
		}
		code := false
		for _, m := range markOrder {
			found := false
			for _, mark := range r.Marks {
				if mark == m {
					found = true
				}
			}
			if !found {
				continue
			}
			delim := ""
			switch m {
			case Bold:
				delim = "**"
			case Italic:
				delim = "*"
			case Strike:
				delim = "~~"
			case InlineCode:
				code = true
				delim = htmlPasteFence(r.Text, 1)
			}
			key := string(m)
			if m == InlineCode {
				key += ":" + delim
			}
			next = append(next, markdownDelimiter{key: key, open: delim, close: delim})
		}
		common := 0
		for common < len(active) && common < len(next) && active[common].key == next[common].key {
			common++
		}
		for i := len(active) - 1; i >= common; i-- {
			out.WriteString(active[i].close)
		}
		for _, d := range next[common:] {
			out.WriteString(d.open)
		}
		if code {
			padding := strings.HasPrefix(r.Text, "`") || strings.HasSuffix(r.Text, "`") || (strings.HasPrefix(r.Text, " ") && strings.HasSuffix(r.Text, " ") && strings.TrimSpace(r.Text) != "")
			if padding {
				out.WriteByte(' ')
			}
			out.WriteString(strings.ReplaceAll(r.Text, "\n", " "))
			if padding {
				out.WriteByte(' ')
			}
		} else {
			out.WriteString(strings.ReplaceAll(markdownEscape(r.Text), "\n", "  \n"))
		}
		active = next
	}
	for i := len(active) - 1; i >= 0; i-- {
		out.WriteString(active[i].close)
	}
	return out.String()
}

func (e *markdownExporter) table(owner Item, id ID) string {
	snapshot, ok := e.dbs[id]
	if !ok {
		e.warn("database_missing", owner, "Database contents were not supplied.")
		return "[Database unavailable]"
	}
	e.reference(snapshot.Database)
	for _, v := range snapshot.Views {
		e.reference(v)
	}
	props := snapshot.Schema.Properties
	if len(props) == 0 {
		return ""
	}
	ids := []ID{}
	if d, ok := snapshot.Database.Data.(Database); ok {
		ids = d.PropertyOrder
	}
	var out strings.Builder
	writeRow := func(cells []string) {
		out.WriteString("| ")
		out.WriteString(strings.Join(cells, " | "))
		out.WriteString(" |\n")
	}
	header := []string{}
	separator := []string{}
	for _, prop := range props {
		header = append(header, markdownEscape(prop.Definition.Name))
		separator = append(separator, "---")
	}
	writeRow(header)
	writeRow(separator)
	for _, row := range snapshot.Rows {
		cells := []string{}
		for i, prop := range props {
			value := CellValue{Type: prop.Definition.Type}
			if i < len(ids) {
				value = row.Cells[ids[i]]
			}
			propertyID := ID("")
			if i < len(ids) {
				propertyID = ids[i]
			}
			cells = append(cells, strings.ReplaceAll(markdownEscape(markdownCell(value, prop, snapshot.Schema.Options[propertyID])), "\n", " "))
		}
		writeRow(cells)
	}
	for _, prop := range props {
		if prop.Definition.Type != TitleType && prop.Definition.Type != TextType {
			e.warn("database_display_text", owner, "Non-text database cells become display text in a Markdown table.")
			break
		}
	}
	return strings.TrimSuffix(out.String(), "\n")
}
func markdownCell(v CellValue, prop Property, options []Option) string {
	if v.Value == nil {
		if prop.Definition.Type == CheckboxType {
			return "false"
		}
		return ""
	}
	optionName := func(id ID) string {
		for i, optionID := range prop.OptionOrder {
			if optionID == id && i < len(options) {
				return options[i].Name
			}
		}
		return string(id)
	}
	switch value := v.Value.(type) {
	case string:
		return value
	case Decimal:
		return string(value)
	case CalendarDate:
		return string(value)
	case bool:
		return strconv.FormatBool(value)
	case ID:
		return optionName(value)
	case []ID:
		selected := map[ID]bool{}
		for _, id := range value {
			selected[id] = true
		}
		parts := []string{}
		for _, id := range prop.OptionOrder {
			if selected[id] {
				parts = append(parts, optionName(id))
			}
		}
		return strings.Join(parts, ", ")
	default:
		return fmt.Sprint(value)
	}
}
