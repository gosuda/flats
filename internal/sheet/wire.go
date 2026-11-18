package sheet

import (
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// The wire schema in code (technical design, Appendix A). Everything a
// client or an agent sends is read once, by walk, against a shape: the
// bytes are checked and the typed value is built in the same pass, from
// the same reading of every name and value. No second decoder looks at the
// bytes, so no two readings of them can differ.
//
// A shape says what JSON may stand at one place: its type, whether null is
// a value there, the fields of an object and which of them it needs, the
// items of a list. walk refuses everything else: a value of another JSON
// type, a null where the schema names none, a field the object does not
// have, a field twice, a field in another case, a required field that is
// missing. A field name is compared after its JSON escapes are read, so
// "r\u006fw" is row. The tests read the same shapes to build the cases of
// web/testdata/frames.json.

// jkind is the JSON type of a place.
type jkind uint8

const (
	jText   jkind = iota + 1 // a string
	jInt                     // an integer written in digits
	jNumber                  // a number
	jFlag                    // true or false
	jList                    // an array of elem
	jObject                  // an object of members
	jOneOf                   // one of alts, told apart by JSON type
)

// shape describes the value of one place. slot is what the sink is told
// with the value, so that it knows which place it is.
type shape struct {
	kind jkind
	slot int
	null bool // null is a value here: Appendix A writes "| null"

	elem  *shape // jList: every item
	count int    // jList: the exact number of items, or 0 for any number

	members []member // jObject, at most 32
	// An object whose fields depend on its kind names the kind in member
	// tag-1, and kinds lists per kind the fields it needs and may carry.
	// With open set, a kind the list does not have is left to the sink.
	tag   int
	kinds map[string]kindFields
	open  bool

	alts []*shape // jOneOf, at most one per JSON type
}

type member struct {
	name     string
	val      *shape
	required bool // for an object without kinds
}

// kindFields are the members of one kind as bits of their place in the
// object's member list, with the name of the kind.
type kindFields struct {
	name              string
	required, allowed uint32
}

// sink receives the parts of a value in the order they stand in the
// bytes. It gets only parts that passed the shape. A text is valid until
// the call returns.
type sink interface {
	begin(slot int) // an object or a list starts
	end(slot int)   // and ends
	text(slot int, s []byte)
	integer(slot int, n int64)
	number(slot int, f float64)
	flag(slot int, on bool)
	null(slot int)
}

// memberSink is a sink that also learns how many bytes each member of the
// outermost object takes: its name, its value, the white space between
// them and the comma that parts it from a neighbour. Without those bytes
// the object is the same object without that member.
type memberSink interface {
	member(slot, size int)
}

// noSink is a sink that keeps nothing. A type embeds it and takes the
// parts it reads.
type noSink struct{}

func (noSink) begin(int)           {}
func (noSink) end(int)             {}
func (noSink) text(int, []byte)    {}
func (noSink) integer(int, int64)  {}
func (noSink) number(int, float64) {}
func (noSink) flag(int, bool)      {}
func (noSink) null(int)            {}

func anObject(slot int, members ...member) *shape {
	if len(members) > 32 {
		panic("sheet: an object of the wire schema has at most 32 fields")
	}
	return &shape{kind: jObject, slot: slot, members: members}
}

// aVariant is an object whose kind is named by its member tag.
func aVariant(slot int, tag string, open bool, members []member, kinds map[string][2]string) *shape {
	s := anObject(slot, members...)
	s.open = open
	s.tag = s.index(tag) + 1
	s.kinds = make(map[string]kindFields, len(kinds))
	for name, lists := range kinds {
		need := s.mask(lists[0])
		s.kinds[name] = kindFields{name, need, need | s.mask(lists[1]) | 1<<(s.tag-1)}
	}
	return s
}

func need(name string, val *shape) member { return member{name, val, true} }
func may(name string, val *shape) member  { return member{name, val, false} }

func aText(slot int) *shape   { return &shape{kind: jText, slot: slot} }
func anInt(slot int) *shape   { return &shape{kind: jInt, slot: slot} }
func aNumber(slot int) *shape { return &shape{kind: jNumber, slot: slot} }
func aFlag(slot int) *shape   { return &shape{kind: jFlag, slot: slot} }

func aList(slot int, elem *shape) *shape { return &shape{kind: jList, slot: slot, elem: elem} }

// aTuple is a list of exactly count items.
func aTuple(slot, count int, elem *shape) *shape {
	return &shape{kind: jList, slot: slot, elem: elem, count: count}
}

// oneOf is one of alts. A null, where the place takes one, is told with
// the slot of the first.
func oneOf(alts ...*shape) *shape { return &shape{kind: jOneOf, slot: alts[0].slot, alts: alts} }

// orNull returns the shape with null as one of its values.
func orNull(s *shape) *shape {
	c := *s
	c.null = true
	return &c
}

func (s *shape) index(name string) int {
	for i := range s.members {
		if s.members[i].name == name {
			return i
		}
	}
	panic("sheet: the wire schema has no field " + name)
}

// mask returns the bits of space-separated member names.
func (s *shape) mask(names string) uint32 {
	var m uint32
	for _, name := range strings.Fields(names) {
		m |= 1 << s.index(name)
	}
	return m
}

// starts reports whether a value of the shape can begin with c.
func (s *shape) starts(c byte) bool {
	switch s.kind {
	case jText:
		return c == '"'
	case jInt, jNumber:
		return c == '-' || c >= '0' && c <= '9'
	case jFlag:
		return c == 't' || c == 'f'
	case jList:
		return c == '['
	case jObject:
		return c == '{'
	}
	return false
}

// The rules a value can break. They are the rule classes of the frame
// table.
const (
	faultSyntax  = "syntax"  // no JSON, or bytes after the value
	faultType    = "type"    // a value of another JSON type than the place takes
	faultNull    = "null"    // a null where the schema names none
	faultUnknown = "unknown" // a field the object does not have
	faultTwice   = "twice"   // a field twice
	faultMissing = "missing" // a required field is not there
	faultForeign = "foreign" // a field of another kind
	faultKind    = "kind"    // a kind the schema does not have
	faultCount   = "count"   // a list of a fixed length with another number of items
)

// wireFault is why walk refused a value: the rule it breaks, the place by
// its path from the top, and the field the rule names. A value of the
// wrong type can say what its place takes instead.
type wireFault struct {
	rule   string
	path   []wireStep
	field  string
	takes  string
	offset int // the byte the walk stopped at
}

// wireStep is one step of a path: a field of an object, or the index of
// an item when name is empty.
type wireStep struct {
	name  string
	index int
}

func (f *wireFault) Error() string {
	at := f.place()
	if at == "" {
		at = "the value"
	}
	switch f.rule {
	case faultSyntax:
		return "the bytes are no JSON value at " + at
	case faultType:
		if f.takes != "" {
			return at + " takes " + f.takes
		}
		return at + " has a value of the wrong JSON type"
	case faultNull:
		return at + " is null"
	case faultUnknown:
		return at + " has the unknown field " + strconv.Quote(f.field)
	case faultTwice:
		return at + " has the field " + f.field + " twice"
	case faultMissing:
		return at + " needs " + f.field
	case faultForeign:
		return "field " + strconv.Quote(f.field) + " does not belong to " + at
	case faultKind:
		return at + " is of an unknown kind"
	case faultCount:
		return at + " has the wrong number of items"
	}
	return at + " is refused"
}

// place writes the path as an agent would: ops[0].rows[1][2].
func (f *wireFault) place() string {
	var b strings.Builder
	for _, step := range f.path {
		if step.name == "" {
			b.WriteByte('[')
			b.WriteString(strconv.Itoa(step.index))
			b.WriteByte(']')
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(step.name)
	}
	return b.String()
}

// under returns the name of the field at depth of the path, or "" when
// the path is shorter or holds an item there.
func (f *wireFault) under(depth int) string {
	if depth < len(f.path) {
		return f.path[depth].name
	}
	return ""
}

// wireDepth is the deepest nesting a shape of the schema has. A value
// nested deeper never passes its shape, so walk never goes deeper either.
const wireDepth = 8

// maxIntDigits bounds an integer on the wire to what an int64 holds.
const maxIntDigits = 18

type reader struct {
	data    []byte
	i       int
	sink    sink
	check   bool // nothing reads the values: a text with escapes is not built
	scratch []byte
	path    [wireDepth]wireStep
	depth   int
	fault   *wireFault
	kind    string // the kind of the value itself, when its shape has kinds
	members memberSink
}

// walk reads data as one value of shape s and tells the sink its parts.
// The fault is nil when the whole of data is that value; kind is then the
// kind of the value, when its shape has kinds and lists this one. With a
// nil sink it only checks.
func walk(data []byte, s *shape, to sink) (kind string, fault *wireFault) {
	r := reader{data: data, sink: to}
	if to == nil {
		r.sink, r.check = noSink{}, true
	}
	r.members, _ = to.(memberSink)
	if !utf8.Valid(data) {
		return "", &wireFault{rule: faultSyntax}
	}
	if r.value(s, true) {
		if r.space(); r.i < len(r.data) {
			r.fail(faultSyntax, "")
		}
	}
	return r.kind, r.fault
}

func (r *reader) fail(rule, field string) bool {
	if r.fault == nil {
		r.fault = &wireFault{rule: rule, field: field, offset: r.i, path: append([]wireStep(nil), r.path[:min(r.depth, wireDepth)]...)}
	}
	return false
}

// failTakes refuses a value of the wrong type and says what its place
// takes.
func (r *reader) failTakes(takes string) bool {
	if r.fault == nil {
		r.fail(faultType, "")
		r.fault.takes = takes
	}
	return false
}

func (r *reader) space() {
	for r.i < len(r.data) {
		switch r.data[r.i] {
		case ' ', '\t', '\n', '\r':
			r.i++
		default:
			return
		}
	}
}

func (r *reader) literal(word string) bool {
	if len(r.data)-r.i < len(word) || string(r.data[r.i:r.i+len(word)]) != word {
		return r.fail(faultSyntax, "")
	}
	r.i += len(word)
	return true
}

// push enters a field or an item. No shape is deeper than wireDepth
// (TestWireSchemaDepth), and a deeper path would only lose its last steps.
func (r *reader) push(name string, index int) {
	if r.depth < wireDepth {
		r.path[r.depth] = wireStep{name, index}
	}
	r.depth++
}

func (r *reader) value(s *shape, top bool) bool {
	r.space()
	if r.i >= len(r.data) {
		return r.fail(faultSyntax, "")
	}
	c := r.data[r.i]
	if c == 'n' {
		if !r.literal("null") {
			return false
		}
		if !s.null {
			return r.fail(faultNull, "")
		}
		r.sink.null(s.slot)
		return true
	}
	if s.kind == jOneOf {
		for _, alt := range s.alts {
			if alt.starts(c) {
				s = alt
				break
			}
		}
	}
	if !s.starts(c) {
		return r.fail(faultType, "")
	}
	switch s.kind {
	case jText:
		b, ok := r.string()
		if ok {
			r.sink.text(s.slot, b)
		}
		return ok
	case jInt, jNumber:
		return r.num(s)
	case jFlag:
		if c == 't' {
			if !r.literal("true") {
				return false
			}
		} else if !r.literal("false") {
			return false
		}
		r.sink.flag(s.slot, c == 't')
		return true
	case jList:
		return r.list(s)
	}
	return r.object(s, top)
}

func (r *reader) list(s *shape) bool {
	r.i++ // [
	r.sink.begin(s.slot)
	n := 0
	if r.space(); r.i < len(r.data) && r.data[r.i] == ']' {
		r.i++
	} else {
		for {
			r.push("", n)
			if !r.value(s.elem, false) {
				return false
			}
			r.depth--
			n++
			if r.space(); r.i >= len(r.data) {
				return r.fail(faultSyntax, "")
			}
			if r.data[r.i] == ']' {
				r.i++
				break
			}
			if r.data[r.i] != ',' {
				return r.fail(faultSyntax, "")
			}
			r.i++
		}
	}
	if s.count != 0 && n != s.count {
		return r.fail(faultCount, "")
	}
	r.sink.end(s.slot)
	return true
}

func (r *reader) object(s *shape, top bool) bool {
	r.i++ // {
	r.sink.begin(s.slot)
	var present uint32
	var of kindFields
	known := false
	if r.space(); r.i < len(r.data) && r.data[r.i] == '}' {
		r.i++
	} else {
		comma := -1 // the comma before the member
		for {
			if r.space(); r.i >= len(r.data) || r.data[r.i] != '"' {
				return r.fail(faultSyntax, "")
			}
			start := r.i
			at, ok := r.key(s)
			if !ok {
				return false
			}
			m := &s.members[at]
			if present&(1<<at) != 0 {
				return r.fail(faultTwice, m.name)
			}
			present |= 1 << at
			if r.space(); r.i >= len(r.data) || r.data[r.i] != ':' {
				return r.fail(faultSyntax, "")
			}
			r.i++
			r.push(m.name, 0)
			if r.space(); at == s.tag-1 && r.i < len(r.data) && r.data[r.i] == '"' {
				// The kind is a text. Its name is read like the name of a
				// field and looked up at once.
				var buf [nameMax]byte
				if n, _, fits, _ := r.name(r.i, &buf); fits {
					of, known = s.kinds[string(buf[:n])]
				}
			}
			if !r.value(m.val, false) {
				return false
			}
			end := r.i
			r.depth--
			if r.space(); r.i >= len(r.data) {
				return r.fail(faultSyntax, "")
			}
			last := r.data[r.i] == '}'
			if !last && r.data[r.i] != ',' {
				return r.fail(faultSyntax, "")
			}
			if top && r.members != nil {
				switch {
				case !last:
					r.members.member(m.val.slot, r.i+1-start)
				case comma >= 0:
					r.members.member(m.val.slot, end-comma)
				default:
					r.members.member(m.val.slot, end-start)
				}
			}
			if last {
				r.i++
				break
			}
			comma = r.i
			r.i++
		}
	}
	if s.tag == 0 {
		for at := range s.members {
			if s.members[at].required && present&(1<<at) == 0 {
				return r.fail(faultMissing, s.members[at].name)
			}
		}
	} else {
		if present&(1<<(s.tag-1)) == 0 {
			return r.fail(faultMissing, s.members[s.tag-1].name)
		}
		if top {
			r.kind = of.name
		}
		switch {
		case known && present&^of.allowed != 0:
			return r.fail(faultForeign, s.firstOf(present&^of.allowed))
		case known && of.required&^present != 0:
			return r.fail(faultMissing, s.firstOf(of.required&^present))
		case !known && !s.open:
			return r.fail(faultKind, s.members[s.tag-1].name)
		}
	}
	r.sink.end(s.slot)
	return true
}

func (s *shape) firstOf(mask uint32) string {
	for at := range s.members {
		if mask&(1<<at) != 0 {
			return s.members[at].name
		}
	}
	return ""
}

// key reads a field name and returns the member it names.
func (r *reader) key(s *shape) (int, bool) {
	var buf [nameMax]byte
	n, end, fits, ok := r.name(r.i, &buf)
	switch {
	case !ok:
		return 0, r.fail(faultSyntax, "")
	case !fits:
		return 0, r.fail(faultUnknown, string(buf[:n])+"...")
	}
	r.i = end
	for at := range s.members {
		if s.members[at].name == string(buf[:n]) {
			return at, true
		}
	}
	return 0, r.fail(faultUnknown, string(buf[:n]))
}

// nameMax is more than the longest name of the schema has bytes: the name
// of a field, or of a kind.
const nameMax = 24

// name reads the JSON string that starts at byte i into buf, with its
// escapes, and returns its length and the byte after it. fits is false
// for a text that is longer than buf or holds a character outside ASCII:
// no name of the schema is such a text, so it is not read to its end. ok
// is false for bytes that are no JSON string as far as they were read.
func (r *reader) name(i int, buf *[nameMax]byte) (n, end int, fits, ok bool) {
	for i++; ; {
		if i >= len(r.data) {
			return n, i, false, false
		}
		c := r.data[i]
		if c == '"' {
			return n, i + 1, true, true
		}
		if c < 0x20 {
			return n, i, false, false
		}
		if n == len(buf) {
			return n, i, false, true
		}
		if c != '\\' {
			buf[n] = c
			n, i = n+1, i+1
			continue
		}
		ch, size := unescape(r.data[i:])
		if size == 0 {
			return n, i, false, false
		}
		if ch >= utf8.RuneSelf {
			return n, i, false, true
		}
		buf[n] = byte(ch)
		n, i = n+1, i+size
	}
}

// string reads a JSON string. Without escapes the text is the bytes
// between the quotes; with escapes it is a copy, which a walk that only
// checks does not make. An escape that names half of a surrogate pair
// reads as U+FFFD, as it does for a browser that sends the text of a cell.
func (r *reader) string() ([]byte, bool) {
	start := r.i + 1
	i := start
	for {
		if i >= len(r.data) {
			return nil, r.fail(faultSyntax, "")
		}
		c := r.data[i]
		if c == '"' {
			r.i = i + 1
			return r.data[start:i], true
		}
		if c == '\\' {
			break
		}
		if c < 0x20 {
			return nil, r.fail(faultSyntax, "")
		}
		i++
	}
	// The copy is made in a buffer the reader keeps for the next text: a
	// sink takes what it keeps before it returns.
	out := r.scratch[:0]
	if !r.check {
		out = append(out, r.data[start:i]...)
	}
	for {
		if i >= len(r.data) {
			return nil, r.fail(faultSyntax, "")
		}
		c := r.data[i]
		switch {
		case c == '"':
			r.i, r.scratch = i+1, out
			return out, true
		case c < 0x20:
			return nil, r.fail(faultSyntax, "")
		case c != '\\':
			if !r.check {
				out = append(out, c)
			}
			i++
		default:
			ch, size := unescape(r.data[i:])
			if size == 0 {
				return nil, r.fail(faultSyntax, "")
			}
			if !r.check {
				out = utf8.AppendRune(out, ch)
			}
			i += size
		}
	}
}

// unescape reads the escape at the start of b, which begins with a
// backslash, and returns its character and its length in bytes, or 0 for
// what is no escape. Two \u escapes that are a surrogate pair are one
// character.
func unescape(b []byte) (rune, int) {
	if len(b) < 2 {
		return 0, 0
	}
	switch b[1] {
	case '"', '\\', '/':
		return rune(b[1]), 2
	case 'b':
		return '\b', 2
	case 'f':
		return '\f', 2
	case 'n':
		return '\n', 2
	case 'r':
		return '\r', 2
	case 't':
		return '\t', 2
	case 'u':
		hi, ok := hex4(b[2:])
		if !ok {
			return 0, 0
		}
		if utf16.IsSurrogate(hi) {
			if len(b) >= 12 && b[6] == '\\' && b[7] == 'u' {
				if lo, ok := hex4(b[8:]); ok {
					if ch := utf16.DecodeRune(hi, lo); ch != utf8.RuneError {
						return ch, 12
					}
				}
			}
			return utf8.RuneError, 6
		}
		return hi, 6
	}
	return 0, 0
}

func hex4(b []byte) (rune, bool) {
	if len(b) < 4 {
		return 0, false
	}
	var v rune
	for _, c := range b[:4] {
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v<<4 | rune(c-'A'+10)
		default:
			return 0, false
		}
	}
	return v, true
}

// num reads a JSON number. An integer place takes digits only, with a
// minus sign in front of a number that is not zero: a fraction, an
// exponent or more than maxIntDigits digits make the value one of the
// wrong type. A number place takes every number a float64 holds.
func (r *reader) num(s *shape) bool {
	start := r.i
	i := start
	if r.data[i] == '-' {
		i++
	}
	digits := i
	for i < len(r.data) && r.data[i] >= '0' && r.data[i] <= '9' {
		i++
	}
	whole := i - digits
	if whole == 0 || whole > 1 && r.data[digits] == '0' {
		return r.fail(faultSyntax, "")
	}
	plain := true
	if i < len(r.data) && r.data[i] == '.' {
		plain = false
		frac := i + 1
		for i = frac; i < len(r.data) && r.data[i] >= '0' && r.data[i] <= '9'; i++ {
		}
		if i == frac {
			return r.fail(faultSyntax, "")
		}
	}
	if i < len(r.data) && (r.data[i] == 'e' || r.data[i] == 'E') {
		plain = false
		i++
		if i < len(r.data) && (r.data[i] == '+' || r.data[i] == '-') {
			i++
		}
		exp := i
		for ; i < len(r.data) && r.data[i] >= '0' && r.data[i] <= '9'; i++ {
		}
		if i == exp {
			return r.fail(faultSyntax, "")
		}
	}
	r.i = i
	if s.kind == jNumber {
		f, err := strconv.ParseFloat(string(r.data[start:i]), 64)
		if err != nil {
			return r.fail(faultType, "") // outside what a number holds
		}
		r.sink.number(s.slot, f)
		return true
	}
	switch {
	case !plain:
		return r.failTakes("a whole number written in plain digits, such as 2, not 2.0 or 2e0")
	case whole > maxIntDigits:
		return r.failTakes("a whole number of at most " + strconv.Itoa(maxIntDigits) + " digits")
	case r.data[start] == '-' && r.data[digits] == '0':
		return r.failTakes("a whole number written in plain digits, such as 0, not -0")
	}
	var n int64
	for _, c := range r.data[digits:i] {
		n = n*10 + int64(c-'0')
	}
	if r.data[start] == '-' {
		n = -n
	}
	r.sink.integer(s.slot, n)
	return true
}
