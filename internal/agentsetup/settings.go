package agentsetup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// ErrSettings reports a settings file that this package refuses to edit.
var ErrSettings = errors.New("the settings file cannot be edited")

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrSettings, fmt.Sprintf(format, a...))
}

// SettingsChange is the result of editing a settings file.
type SettingsChange struct {
	Out     []byte
	Action  Action
	Entries []string // entries added by Add, or removed by Remove
	Present []string // entries that Add found already in the file
}

// denyRules are the permissions.deny entries, in the order they are added.
var denyRules = []string{DenySkill, DenyTools}

const (
	keyDenied      = "deniedMcpServers"
	keyPermissions = "permissions"
	keyDeny        = "deny"
	keyServerName  = "serverName"
)

// edit replaces b[from:to] with text.
type edit struct {
	from, to int
	text     string
}

func applyEdits(b []byte, edits []edit) []byte {
	edits = slices.Clone(edits)
	slices.SortFunc(edits, func(x, y edit) int { return y.from - x.from })
	out := slices.Clone(b)
	for _, e := range edits {
		out = slices.Concat(out[:e.from], []byte(e.text), out[e.to:])
	}
	return out
}

// located holds the three containers this package edits, nil when absent.
type located struct {
	root, denied, perms, deny *node
}

func locate(b []byte) (located, error) {
	root, err := parseJSON(b)
	if err != nil {
		return located{}, refuse("%v (comments are not accepted)", err)
	}
	if root.kind != '{' {
		return located{}, refuse("the top-level value is not an object")
	}
	l := located{root: root}
	pick := func(parent *node, key, where string, kind byte, kindName string) (*node, error) {
		ms := parent.find(key)
		switch {
		case len(ms) > 1:
			return nil, refuse("the key %q appears twice%s", key, where)
		case len(ms) == 0:
			return nil, nil
		case ms[0].val.kind != kind:
			return nil, refuse("%q%s must be %s", key, where, kindName)
		}
		return ms[0].val, nil
	}
	if l.denied, err = pick(root, keyDenied, "", '[', "an array"); err != nil {
		return l, err
	}
	if l.perms, err = pick(root, keyPermissions, "", '{', "an object"); err != nil {
		return l, err
	}
	if l.perms != nil {
		l.deny, err = pick(l.perms, keyDeny, " in permissions", '[', "an array")
	}
	return l, err
}

// isDeniedEntry reports whether n is {"serverName": "claude.ai Claude Docs"}
// and nothing else.
func isDeniedEntry(b []byte, n *node) bool {
	if n.kind != '{' || len(n.members) != 1 || n.members[0].key != keyServerName {
		return false
	}
	name, ok := n.members[0].val.stringValue(b)
	return ok && name == ClaudeDocsServer
}

func isDenyRule(b []byte, n *node) bool {
	s, ok := n.stringValue(b)
	return ok && slices.Contains(denyRules, s)
}

func quote(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(buf.String(), "\n")
}

var deniedEntryText = `{"serverName": ` + quote(ClaudeDocsServer) + `}`

// style is the layout of the file being edited.
type style struct {
	b    []byte
	eol  string
	unit string
}

func newStyle(b []byte) style {
	st := style{b: b, eol: lineEnding(b), unit: "  "}
	for _, l := range splitLines(b)[1:] {
		if ws := leadingSpace(b[l.start:l.textEnd]); ws != "" && l.textEnd > l.start+len(ws) {
			st.unit = ws
			break
		}
	}
	return st
}

func leadingSpace(line []byte) string {
	n := 0
	for n < len(line) && (line[n] == ' ' || line[n] == '\t') {
		n++
	}
	return string(line[:n])
}

// indentOf is the indentation of the line that holds position pos.
func (st style) indentOf(pos int) string {
	i := bytes.LastIndexByte(st.b[:pos], '\n') + 1
	return leadingSpace(st.b[i:pos])
}

func (st style) spansLines(from, to int) bool {
	return bytes.IndexByte(st.b[from:to], '\n') >= 0
}

// array renders items as a JSON array whose own line has indentation ind.
func (st style) array(ind string, items []string, multi bool) string {
	if !multi {
		return "[" + strings.Join(items, ", ") + "]"
	}
	var sb strings.Builder
	sb.WriteString("[")
	for i, it := range items {
		sb.WriteString(st.eol + ind + st.unit + it)
		if i < len(items)-1 {
			sb.WriteString(",")
		}
	}
	sb.WriteString(st.eol + ind + "]")
	return sb.String()
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = quote(s)
	}
	return out
}

// membersEdit appends members to an object.
func (st style) membersEdit(obj *node, members func(ind string, multi bool) []string) edit {
	if len(obj.members) == 0 {
		base := st.indentOf(obj.start)
		ind := base + st.unit
		var sb strings.Builder
		ms := members(ind, true)
		for i, m := range ms {
			sb.WriteString(st.eol + ind + m)
			if i < len(ms)-1 {
				sb.WriteString(",")
			}
		}
		if st.spansLines(obj.start, obj.end) {
			return edit{obj.start + 1, obj.start + 1, sb.String()}
		}
		return edit{obj.start + 1, obj.end - 1, sb.String() + st.eol + base}
	}
	last := obj.members[len(obj.members)-1]
	multi := st.spansLines(obj.start, last.keyStart)
	ind := ""
	sep := " "
	if multi {
		ind = st.indentOf(last.keyStart)
		sep = st.eol + ind
	}
	var sb strings.Builder
	for _, m := range members(ind, multi) {
		sb.WriteString("," + sep + m)
	}
	return edit{last.val.end, last.val.end, sb.String()}
}

// elementsEdit appends elements to an array.
func (st style) elementsEdit(arr *node, items []string) edit {
	if len(arr.elems) == 0 {
		if st.spansLines(arr.start, arr.end) {
			ind := st.indentOf(arr.start) + st.unit
			var sb strings.Builder
			for i, it := range items {
				sb.WriteString(st.eol + ind + it)
				if i < len(items)-1 {
					sb.WriteString(",")
				}
			}
			return edit{arr.start + 1, arr.start + 1, sb.String()}
		}
		return edit{arr.start + 1, arr.end - 1, strings.Join(items, ", ")}
	}
	last := arr.elems[len(arr.elems)-1]
	sep := " "
	if st.spansLines(arr.start, last.start) {
		sep = st.eol + st.indentOf(last.start)
	}
	var sb strings.Builder
	for _, it := range items {
		sb.WriteString("," + sep + it)
	}
	return edit{last.end, last.end, sb.String()}
}

// SetSettings adds the Claude Docs entries to a settings file, leaving every
// other byte alone. An empty or whitespace-only file counts as an empty object.
func SetSettings(content []byte) (SettingsChange, error) {
	empty := len(bytes.TrimSpace(content)) == 0
	src := content
	if empty {
		src = []byte("{}")
	}
	l, err := locate(src)
	if err != nil {
		return SettingsChange{}, err
	}
	st := newStyle(src)

	var ch SettingsChange
	var edits []edit
	var rootMembers []func(ind string, multi bool) string
	switch {
	case l.denied == nil:
		ch.Entries = append(ch.Entries, keyDenied+": "+deniedEntryText)
	case !slices.ContainsFunc(l.denied.elems, func(n *node) bool { return isDeniedEntry(src, n) }):
		edits = append(edits, st.elementsEdit(l.denied, []string{deniedEntryText}))
		ch.Entries = append(ch.Entries, keyDenied+": "+deniedEntryText)
	default:
		ch.Present = append(ch.Present, keyDenied+": "+deniedEntryText)
	}
	if l.denied == nil {
		rootMembers = append(rootMembers, func(ind string, multi bool) string {
			return quote(keyDenied) + ": " + st.array(ind, []string{deniedEntryText}, multi)
		})
	}

	var missing []string
	for _, rule := range denyRules {
		if l.deny != nil && slices.ContainsFunc(l.deny.elems, func(n *node) bool { s, _ := n.stringValue(src); return s == rule }) {
			ch.Present = append(ch.Present, keyPermissions+"."+keyDeny+": "+quote(rule))
			continue
		}
		missing = append(missing, rule)
		ch.Entries = append(ch.Entries, keyPermissions+"."+keyDeny+": "+quote(rule))
	}
	if len(missing) > 0 {
		switch {
		case l.perms == nil:
			rootMembers = append(rootMembers, func(ind string, multi bool) string {
				if !multi {
					return quote(keyPermissions) + `: {` + quote(keyDeny) + ": " + st.array(ind, quoteAll(missing), false) + "}"
				}
				inner := ind + st.unit
				return quote(keyPermissions) + ": {" + st.eol + inner + quote(keyDeny) + ": " + st.array(inner, quoteAll(missing), true) + st.eol + ind + "}"
			})
		case l.deny == nil:
			edits = append(edits, st.membersEdit(l.perms, func(ind string, multi bool) []string {
				return []string{quote(keyDeny) + ": " + st.array(ind, quoteAll(missing), multi)}
			}))
		default:
			edits = append(edits, st.elementsEdit(l.deny, quoteAll(missing)))
		}
	}
	if len(rootMembers) > 0 {
		edits = append(edits, st.membersEdit(l.root, func(ind string, multi bool) []string {
			out := make([]string, len(rootMembers))
			for i, m := range rootMembers {
				out[i] = m(ind, multi)
			}
			return out
		}))
	}

	if len(edits) == 0 {
		return SettingsChange{Out: content, Action: Unchanged, Present: ch.Present}, nil
	}
	out := applyEdits(src, edits)
	if empty {
		out = append(out, st.eol...)
	}
	if err := checkSettings(src, out, setEntries); err != nil {
		return SettingsChange{}, err
	}
	ch.Out, ch.Action = out, Add
	return ch, nil
}

// DropSettings removes the Claude Docs entries from a settings file. It keeps
// every key and every emptied array.
func DropSettings(content []byte) (SettingsChange, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return SettingsChange{Out: content, Action: Unchanged}, nil
	}
	cur := content
	var ch SettingsChange
	for {
		l, err := locate(cur)
		if err != nil {
			return SettingsChange{}, err
		}
		st := newStyle(cur)
		var edits []edit
		var entry string
		if l.denied != nil {
			for i, n := range l.denied.elems {
				if isDeniedEntry(cur, n) {
					edits, entry = st.removeEdits(l.denied, i), keyDenied+": "+deniedEntryText
					break
				}
			}
		}
		if edits == nil && l.deny != nil {
			for i, n := range l.deny.elems {
				if isDenyRule(cur, n) {
					s, _ := n.stringValue(cur)
					edits, entry = st.removeEdits(l.deny, i), keyPermissions+"."+keyDeny+": "+quote(s)
					break
				}
			}
		}
		if edits == nil {
			break
		}
		cur = applyEdits(cur, edits)
		ch.Entries = append(ch.Entries, entry)
	}
	if len(ch.Entries) == 0 {
		return SettingsChange{Out: content, Action: Unchanged}, nil
	}
	if err := checkSettings(content, cur, dropEntries); err != nil {
		return SettingsChange{}, err
	}
	ch.Out, ch.Action = cur, Remove
	return ch, nil
}

// removeEdits removes element i of arr together with the line or the
// separator that joins it to its neighbours.
func (st style) removeEdits(arr *node, i int) []edit {
	e := arr.elems[i]
	n := len(arr.elems)
	if from, to, ok := st.ownLines(e); ok {
		edits := []edit{{from, to, ""}}
		if i == n-1 && n > 1 {
			prev := arr.elems[i-1]
			c := prev.end
			for st.b[c] != ',' {
				c++
			}
			edits = append(edits, edit{c, c + 1, ""})
		}
		return edits
	}
	switch {
	case n == 1:
		return []edit{{e.start, e.end, ""}}
	case i < n-1:
		return []edit{{e.start, arr.elems[i+1].start, ""}}
	default:
		return []edit{{arr.elems[i-1].end, e.end, ""}}
	}
}

// ownLines reports whether e starts its line and ends its line, apart from a
// comma, and returns the span of those lines including the final line ending.
func (st style) ownLines(e *node) (from, to int, ok bool) {
	from = e.start
	for from > 0 && (st.b[from-1] == ' ' || st.b[from-1] == '\t') {
		from--
	}
	if from > 0 && st.b[from-1] != '\n' {
		return 0, 0, false
	}
	to = e.end
	skip := func(chars string) {
		for to < len(st.b) && strings.IndexByte(chars, st.b[to]) >= 0 {
			to++
		}
	}
	skip(" \t")
	if to < len(st.b) && st.b[to] == ',' {
		to++
	}
	skip(" \t\r")
	if to >= len(st.b) || st.b[to] != '\n' {
		return 0, 0, false
	}
	return from, to + 1, true
}

type settingsOp int

const (
	setEntries settingsOp = iota
	dropEntries
)

// checkSettings verifies that after equals before with the entries added or
// removed, as parsed values. A mismatch is a bug, and nothing is written.
func checkSettings(before, after []byte, op settingsOp) error {
	want, err := decodeObject(before)
	if err != nil {
		return err
	}
	got, err := decodeObject(after)
	if err != nil {
		return refuse("internal check: the edited file is not valid JSON: %v", err)
	}
	if op == setEntries {
		addEntries(want)
	} else {
		removeEntries(want)
	}
	if !reflect.DeepEqual(want, got) {
		return refuse("internal check: the edited file differs from the expected result")
	}
	return nil
}

func decodeObject(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

func entryMap() map[string]any { return map[string]any{keyServerName: ClaudeDocsServer} }

func addEntries(m map[string]any) {
	denied, _ := m[keyDenied].([]any)
	if !slices.ContainsFunc(denied, func(v any) bool { return reflect.DeepEqual(v, entryMap()) }) {
		denied = append(denied, entryMap())
	}
	m[keyDenied] = denied
	perms, _ := m[keyPermissions].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	deny, _ := perms[keyDeny].([]any)
	for _, rule := range denyRules {
		if !slices.Contains(deny, any(rule)) {
			deny = append(deny, rule)
		}
	}
	perms[keyDeny] = deny
	m[keyPermissions] = perms
}

func removeEntries(m map[string]any) {
	if denied, ok := m[keyDenied].([]any); ok {
		m[keyDenied] = slices.DeleteFunc(slices.Clone(denied), func(v any) bool { return reflect.DeepEqual(v, entryMap()) })
	}
	if perms, ok := m[keyPermissions].(map[string]any); ok {
		if deny, ok := perms[keyDeny].([]any); ok {
			perms[keyDeny] = slices.DeleteFunc(slices.Clone(deny), func(v any) bool {
				s, ok := v.(string)
				return ok && slices.Contains(denyRules, s)
			})
		}
	}
}
