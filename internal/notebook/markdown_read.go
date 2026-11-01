package notebook

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// Rendering writes small units even when one retained field exceeds the read
// limit. Schema orders and cell values remain in the capture transaction.
type markdownStream struct {
	capture  *markdownReadCapture
	buffer   strings.Builder
	item     *Item
	warnings []Warning
	err      error
}

func (w *markdownStream) flush() {
	if w.err == nil && w.buffer.Len() > 0 {
		w.err = w.capture.emit(w.buffer.String(), w.item, w.warnings)
		w.item, w.warnings = nil, nil
		w.buffer.Reset()
	}
}

// WriteRune boundaries, rather than arbitrary source reads, keep every stored
// Markdown unit valid UTF-8.
func (w *markdownStream) text(s string) {
	for _, r := range s {
		if w.buffer.Len()+4 > 16<<10 {
			w.flush()
		}
		if w.err != nil {
			return
		}
		w.buffer.WriteRune(r)
	}
}

type markdownLines struct {
	out    *markdownStream
	indent string
}

func (w markdownLines) text(s string) {
	for {
		before, after, found := strings.Cut(s, "\n")
		w.out.text(before)
		if !found {
			return
		}
		w.out.text("\n" + w.indent)
		s = after
	}
}

func (c *markdownReadCapture) identity(id ID) (Item, error) {
	var inc, own, aggregate string
	var alive bool
	if err := c.tx.QueryRowContext(c.ctx, `SELECT inc,own_version,aggregate_version,alive FROM items WHERE id=?`, id).Scan(&inc, &own, &aggregate, &alive); err != nil {
		return Item{}, err
	}
	if !alive {
		return Item{}, &WriteError{Code: "not_found", Reason: "gone"}
	}
	i, err := parseDBDecimal(inc)
	if err != nil {
		return Item{}, err
	}
	o, err := parseDBDecimal(own)
	if err != nil {
		return Item{}, err
	}
	a, err := parseDBDecimal(aggregate)
	return Item{Ref: Ref{ID: id, Inc: Incarnation(i)}, Ver: Version(max(o, a))}, err
}

func (c *markdownReadCapture) reference(id ID) error {
	item, err := c.identity(id)
	if err != nil {
		return err
	}
	return c.emit("", &item, nil)
}

func (c *markdownReadCapture) field(id ID, path string, out any) error {
	return c.tx.QueryRowContext(c.ctx, `SELECT json_extract(data,?) FROM items WHERE id=?`, path, id).Scan(out)
}

func (c *markdownReadCapture) itemText(id ID, path string) io.Reader {
	r, err := c.rawField(id, path)
	if err != nil {
		return markdownFailedReader{err}
	}
	return &markdownString{r: r}
}

var markdownReaderPool = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 16<<10) }}

func markdownReader(source io.Reader) *bufio.Reader {
	r := markdownReaderPool.Get().(*bufio.Reader)
	r.Reset(source)
	return r
}

func releaseMarkdownReader(r *bufio.Reader) {
	r.Reset(nil)
	markdownReaderPool.Put(r)
}

func markdownCopy(out markdownLines, source io.Reader, escape, flat, rich bool) error {
	r := markdownReader(source)
	defer releaseMarkdownReader(r)
	for {
		if out.out.err != nil {
			return out.out.err
		}
		ch, _, err := r.ReadRune()
		if err == io.EOF {
			return out.out.err
		}
		if err != nil {
			return err
		}
		if ch == '\n' {
			if flat {
				out.text(" ")
				continue
			}
			if rich {
				out.text("  ")
			}
		}
		if escape && strings.ContainsRune("\\`*_{}[]()#+-.!|<>~&=", ch) {
			out.text("\\")
		}
		out.text(string(ch))
		if out.out.err != nil {
			return out.out.err
		}
	}
}

type markdownTextShape struct {
	first, last rune
	fence       int
	nonspace    bool
	hasText     bool
}

func markdownShape(source io.Reader, minimum int) (markdownTextShape, error) {
	r := markdownReader(source)
	defer releaseMarkdownReader(r)
	s := markdownTextShape{fence: minimum}
	run := 0
	first := true
	for {
		ch, _, err := r.ReadRune()
		if err == io.EOF {
			return s, nil
		}
		if err != nil {
			return s, err
		}
		if first {
			s.first, first = ch, false
		}
		s.last, s.hasText = ch, true
		s.nonspace = s.nonspace || !unicode.IsSpace(ch)
		if ch == '`' {
			run++
			s.fence = max(s.fence, run+1)
		} else {
			run = 0
		}
	}
}

func markdownFence(out markdownLines, n int) {
	for n > 0 && out.out.err == nil {
		part := min(n, 256)
		out.text(strings.Repeat("`", part))
		n -= part
	}
}

type markdownReadDelimiter struct {
	mark  Mark
	fence int
	link  bool
	url   revisionRange
	page  ID
}

func (c *markdownReadCapture) sameDelimiter(a, b markdownReadDelimiter) (bool, error) {
	if a.mark != b.mark || a.fence != b.fence || a.page != b.page || a.link != b.link {
		return false, nil
	}
	if !a.link || a.page != "" {
		return true, nil
	}
	return markdownEqual(a.url, b.url)
}

func (c *markdownReadCapture) delimiter(out markdownLines, d markdownReadDelimiter, close bool) error {
	if d.link {
		if !close {
			out.text("[")
			return out.out.err
		}
		out.text("](<")
		if d.page != "" {
			out.text("#" + string(d.page))
		} else {
			r := markdownReader(markdownRangeText(d.url))
			defer releaseMarkdownReader(r)
			for {
				if out.out.err != nil {
					return out.out.err
				}
				ch, _, err := r.ReadRune()
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
				switch ch {
				case '&':
					out.text("&amp;")
				case '<':
					out.text("&lt;")
				case '>':
					out.text("&gt;")
				case '\n':
					out.text("&#10;")
				case '\r':
					out.text("&#13;")
				case '	':
					out.text("&#9;")
				case ' ':
					out.text("&#32;")
				case '\\':
					out.text("&#92;")
				default:
					out.text(string(ch))
				}
			}
		}
		out.text(">)")
	} else if d.mark == InlineCode {
		markdownFence(out, d.fence)
	} else {
		switch d.mark {
		case Bold:
			out.text("**")
		case Italic:
			out.text("*")
		case Strike:
			out.text("~~")
		}
	}
	return out.out.err
}

func (c *markdownReadCapture) rich(out markdownLines, id ID, path string) error {
	var size int64
	if err := c.tx.QueryRowContext(c.ctx, `SELECT length(CAST(data AS BLOB)) FROM items WHERE id=?`, id).Scan(&size); err != nil {
		return err
	}
	source, err := c.rawField(id, path)
	if err != nil {
		return err
	}
	// Small fields need no temporary file; both decoding and the exporter's
	// expansion are bounded independently of the retained record size.
	if size <= 4096 {
		var runs RichText
		if err := json.NewDecoder(io.LimitReader(source, 4097)).Decode(&runs); err != nil {
			return err
		}
		out.text(markdownRich(runs))
		return out.out.err
	}
	value, close, err := c.stage(source)
	if err != nil {
		return err
	}
	defer close()
	var active []markdownReadDelimiter
	err = markdownWalk(value, false, func(_ string, run revisionRange) error {
		fields, err := c.fields(run)
		if err != nil {
			return err
		}
		next := []markdownReadDelimiter{}
		if link, ok := fields["link"]; ok {
			values, err := c.fields(link)
			if err != nil {
				return err
			}
			d := markdownReadDelimiter{link: true, url: values["url"]}
			if page, ok := values["page"]; ok {
				if err := readSmallRange(page.file, page.start, page.start+page.size, &d.page); err != nil {
					return err
				}
			}
			next = append(next, d)
		}
		var marks []Mark
		value := fields["marks"]
		if err := readSmallRange(value.file, value.start, value.start+value.size, &marks); err != nil {
			return err
		}
		code := false
		var shape markdownTextShape
		for _, mark := range markOrder {
			present := false
			for _, candidate := range marks {
				present = present || candidate == mark
			}
			if !present {
				continue
			}
			d := markdownReadDelimiter{mark: mark}
			if mark == InlineCode {
				shape, err = markdownShape(markdownRangeText(fields["text"]), 1)
				if err != nil {
					return err
				}
				d.fence, code = shape.fence, true
			}
			next = append(next, d)
		}
		common := 0
		for common < len(active) && common < len(next) {
			same, err := c.sameDelimiter(active[common], next[common])
			if err != nil {
				return err
			}
			if !same {
				break
			}
			common++
		}
		for j := len(active) - 1; j >= common; j-- {
			if err := c.delimiter(out, active[j], true); err != nil {
				return err
			}
		}
		for _, d := range next[common:] {
			if err := c.delimiter(out, d, false); err != nil {
				return err
			}
		}
		padding := code && (shape.first == '`' || shape.last == '`' || shape.first == ' ' && shape.last == ' ' && shape.nonspace)
		if padding {
			out.text(" ")
		}
		if err := markdownCopy(out, markdownRangeText(fields["text"]), !code, code, !code); err != nil {
			return err
		}
		if padding {
			out.text(" ")
		}
		active = next
		return out.out.err
	}, c)
	if err != nil {
		return err
	}
	for j := len(active) - 1; j >= 0; j-- {
		if err := c.delimiter(out, active[j], true); err != nil {
			return err
		}
	}
	return out.out.err
}

func (c *markdownReadCapture) database(id ID) error {
	c.releaseOptions()
	options := &markdownReadOptions{capture: spillCapture{s: c.s}}
	if err := options.capture.prepareQueryRanks(c.ctx, c.tx, id); err != nil {
		return err
	}
	file, err := options.capture.queryFile("markdown-options-")
	if err != nil {
		options.capture.closeQueryFile(options.capture.ranks)
		_ = c.s.charge(-options.capture.rankSlots * spillRankSlot)
		return err
	}
	options.ids = file
	c.options = options
	if err := c.reference(id); err != nil {
		return err
	}
	if err := spillOrder(c.ctx, c.tx, id, PropertyOrderGroup, func(pid ID) error {
		if err := c.reference(pid); err != nil {
			return err
		}
		return spillOrder(c.ctx, c.tx, pid, OptionOrderGroup, func(oid ID) error {
			rank := options.count + 1
			if _, err := options.capture.queryRank(oid, &rank); err != nil {
				return err
			}
			var slot [64]byte
			copy(slot[:32], oid)
			copy(slot[32:], pid)
			if err := c.s.charge(64); err != nil {
				return err
			}
			options.count++
			if _, err := options.ids.Write(slot[:]); err != nil {
				return err
			}
			return c.reference(oid)
		})
	}); err != nil {
		return err
	}
	return spillOrder(c.ctx, c.tx, id, ViewOrderGroup, c.reference)
}

func (c *markdownReadCapture) table(out markdownLines, owner Item, id ID, indent string) error {
	defer c.releaseOptions()
	var count int
	if err := c.tx.QueryRowContext(c.ctx, `SELECT json_array_length(ids) FROM orders WHERE owner_id=? AND kind=?`, id, PropertyOrderGroup).Scan(&count); err != nil {
		return err
	}
	warn := false
	if count > 0 {
		out.text("| ")
		first := true
		if err := spillOrder(c.ctx, c.tx, id, PropertyOrderGroup, func(pid ID) error {
			if !first {
				out.text(" | ")
			}
			first = false
			var typ PropertyType
			if err := c.field(pid, "$.definition.type", &typ); err != nil {
				return err
			}
			warn = warn || typ != TitleType && typ != TextType
			return markdownCopy(out, c.itemText(pid, "$.definition.name"), true, false, false)
		}); err != nil {
			return err
		}
		out.text(" |\n| ")
		for i := 0; i < count; i++ {
			if i > 0 {
				out.text(" | ")
			}
			out.text("---")
		}
		out.text(" |")
	}
	if warn {
		out.out.warnings = append(out.out.warnings, Warning{Code: "database_display_text", Path: string(owner.ID()), Message: "Non-text database cells become display text in a Markdown table."})
	}
	out.out.text("\n")
	out.out.flush()
	rows, err := c.tx.QueryContext(c.ctx, `SELECT r.id FROM rows r JOIN items i ON i.id=r.id WHERE r.database_id=? AND i.alive=1 ORDER BY r.position,r.id`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rid ID
		if err := rows.Scan(&rid); err != nil {
			return err
		}
		if err := c.reference(rid); err != nil {
			return err
		}
		if count > 0 {
			out.out.text(indent + "| ")
			first := true
			if err := spillOrder(c.ctx, c.tx, id, PropertyOrderGroup, func(pid ID) error {
				if !first {
					out.out.text(" | ")
				}
				first = false
				return c.cell(markdownLines{out: out.out}, rid, pid)
			}); err != nil {
				return err
			}
			out.out.text(" |\n")
			out.out.flush()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	out.out.text("\n")
	out.out.flush()
	return out.out.err
}

func (c *markdownReadCapture) cell(out markdownLines, row, property ID) error {
	var typ PropertyType
	if err := c.field(property, "$.definition.type", &typ); err != nil {
		return err
	}
	var exists bool
	if err := c.tx.QueryRowContext(c.ctx, `SELECT EXISTS(SELECT 1 FROM cells WHERE row_id=? AND property_id=? AND json_type(value,'$.value')<>'null')`, row, property).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if typ == CheckboxType {
			out.text("false")
		}
		return out.out.err
	}
	option := func(id ID) error {
		rank, err := c.options.capture.queryRank(id, nil)
		if err != nil {
			return err
		}
		ordered := rank != 0
		if ordered {
			var slot [64]byte
			if _, err := c.options.ids.ReadAt(slot[:], int64((rank-1)*64)); err != nil {
				return err
			}
			ordered = ID(strings.TrimRight(string(slot[32:]), "\x00")) == property
		}
		if !ordered {
			out.text(markdownEscape(string(id)))
			return out.out.err
		}
		return markdownCopy(out, c.itemText(id, "$.name"), true, true, false)
	}
	switch typ {
	case CheckboxType:
		var value bool
		if err := c.tx.QueryRowContext(c.ctx, `SELECT json_extract(value,'$.value') FROM cells WHERE row_id=? AND property_id=?`, row, property).Scan(&value); err != nil {
			return err
		}
		out.text(strconv.FormatBool(value))
	case SelectType:
		var value ID
		if err := c.tx.QueryRowContext(c.ctx, `SELECT json_extract(value,'$.value') FROM cells WHERE row_id=? AND property_id=?`, row, property).Scan(&value); err != nil {
			return err
		}
		return option(value)
	case MultiSelectType:
		return c.selected(out, row, property)
	default:
		source := &spillColumnReader{ctx: c.ctx, q: c.tx, query: `SELECT substr(CAST(value AS BLOB),?,?) FROM cells WHERE row_id=? AND property_id=?`, args: []any{row, property}}
		value, err := spillFieldReader(source, "value")
		if err != nil {
			return err
		}
		return markdownCopy(out, &markdownString{r: value}, true, true, false)
	}
	return out.out.err
}

func (c *markdownReadCapture) forest(page Item, parent ID, indent string) error {
	rows, err := c.tx.QueryContext(c.ctx, `SELECT b.id FROM blocks b JOIN items i ON i.id=b.id WHERE b.page=? AND b.parent IS ? AND i.alive=1 ORDER BY b.position,b.id`, page.ID(), nullableID(parent))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id ID
		if err := rows.Scan(&id); err != nil {
			return err
		}
		item, err := c.identity(id)
		if err != nil {
			return err
		}
		var kind ContentKind
		if err := c.field(id, "$.content.kind", &kind); err != nil {
			return err
		}
		var database ID
		if kind == InlineDatabase {
			if err := c.field(id, "$.content.database", &database); err != nil {
				return err
			}
			if err := c.database(database); err != nil {
				return err
			}
		}
		var warnings []Warning
		switch kind {
		case Toggle:
			warnings = []Warning{{Code: "toggle", Path: string(id), Message: "Toggle summaries and children become ordinary blocks."}}
		case PageLink:
			warnings = []Warning{{Code: "page_link", Path: string(id), Message: "Page-link blocks become links; unavailable target titles use Missing page."}}
		case Image:
			var caption bool
			if err := c.tx.QueryRowContext(c.ctx, `SELECT COALESCE(json_array_length(data,'$.content.caption'),0)>0 FROM items WHERE id=?`, id).Scan(&caption); err != nil {
				return err
			}
			if caption {
				warnings = []Warning{{Code: "image_caption", Path: string(id), Message: "Image captions become following paragraphs."}}
			}
		}
		w := &markdownStream{capture: c, item: &item, warnings: warnings}
		w.text(indent + "<!-- flats:block " + string(id) + " -->\n" + indent)
		continuation := indent
		if kind == Bullet || kind == Todo {
			continuation += "  "
		}
		if kind == Numbered {
			continuation += "   "
		}
		out := markdownLines{out: w, indent: continuation}
		switch kind {
		case Heading1:
			out.text("# ")
		case Heading2:
			out.text("## ")
		case Heading3:
			out.text("### ")
		case Bullet:
			out.text("- ")
		case Numbered:
			out.text("1. ")
		case Todo:
			var checked bool
			if err := c.field(id, "$.content.checked", &checked); err != nil {
				return err
			}
			if checked {
				out.text("- [x] ")
			} else {
				out.text("- [ ] ")
			}
		case Quote:
			out.text("> ")
			out.indent += "> "
		case Callout:
			var tone Tone
			if err := c.field(id, "$.content.tone", &tone); err != nil {
				return err
			}
			alert := "IMPORTANT"
			for _, a := range markdownAlerts {
				if a.tone == tone {
					alert = a.name
					break
				}
			}
			out.text("> [!" + alert + "]\n> ")
			out.indent += "> "
		}
		switch kind {
		case Code:
			shape, err := markdownShape(c.itemText(id, "$.content.text"), 3)
			if err != nil {
				return err
			}
			markdownFence(out, shape.fence)
			if err := markdownCopy(out, c.itemText(id, "$.content.language"), false, false, false); err != nil {
				return err
			}
			out.text("\n")
			if err := markdownCopy(out, c.itemText(id, "$.content.text"), false, false, false); err != nil {
				return err
			}
			if shape.hasText && shape.last != '\n' {
				out.text("\n")
			}
			markdownFence(out, shape.fence)
		case Divider:
			out.text("---")
		case Image:
			out.text("![")
			if err := markdownCopy(out, c.itemText(id, "$.content.alt"), true, false, false); err != nil {
				return err
			}
			out.text("](<assets/")
			if err := markdownCopy(out, c.itemText(id, "$.content.asset"), false, false, false); err != nil {
				return err
			}
			out.text(">)")
			if len(warnings) > 0 {
				out.text("\n\n")
				if err := c.rich(out, id, "$.content.caption"); err != nil {
					return err
				}
			}
		case PageLink:
			var target ID
			if err := c.field(id, "$.content.target", &target); err != nil {
				return err
			}
			var alive bool
			err := c.tx.QueryRowContext(c.ctx, `SELECT alive FROM items WHERE id=?`, target).Scan(&alive)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			out.text("[")
			if alive {
				if err := markdownCopy(out, c.itemText(target, "$.title"), true, false, false); err != nil {
					return err
				}
			} else {
				out.text("Missing page")
			}
			out.text("](#" + string(target) + ")")
		case InlineDatabase:
			if err := c.table(out, item, database, indent); err != nil {
				return err
			}
		default:
			if err := c.rich(out, id, "$.content.text"); err != nil {
				return err
			}
		}
		if kind != InlineDatabase {
			w.text("\n\n")
		}
		w.flush()
		if w.err != nil {
			return w.err
		}
		childIndent := indent
		if kind.HoldsChildren() && kind != Toggle {
			childIndent += "  "
			if kind == Numbered {
				childIndent += " "
			}
		}
		if kind.HoldsChildren() {
			if err := c.forest(page, id, childIndent); err != nil {
				return err
			}
		}
	}
	return rows.Err()
}
