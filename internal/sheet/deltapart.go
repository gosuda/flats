package sheet

import (
	"bytes"
	"errors"
	"strconv"
)

// A delta that passes the server frame limit goes out as delta_part frames
// (section 12, Appendix A). Each part is a complete JSON object that holds
// some of the delta's fields, so a browser parses one part at a time and
// never one text of the delta's size, which the limits allow to pass the
// longest string a JavaScript engine holds. The client joins the parts in
// order and applies the delta once, with its last part.
//
// A list field may go on over several parts: the next part has the field
// again with the next items. Two kinds of item can themselves be longer
// than a part, and go on in the next part:
//
//   - a row of cells, under the same row id, with its next cells, as in a
//     snapshot's rows frames;
//   - a structural edit, which ends with "more":true and goes on in the
//     first edit of the next part's struct, which repeats its op and tab
//     and carries its next fields; a list of the two is continued there.
//
// One cell, one format, one width, one tab and one id run each fit a part
// under the sheet's limits.

var (
	errDeltaItem = errors.New("a delta item does not fit a part")
	errPartStop  = errors.New("the receiver of the parts stopped")
)

// deltaField is one field of a delta frame and its encoded value.
type deltaField struct {
	key   string
	value []byte
}

// splitDelta cuts the fields of a delta, as deltaRec.fields gives them,
// into parts whose fields take at most partDataMax bytes each. It calls
// part with the fields of each part, in order, and stops when part reports
// false. It returns the number of parts it found. A nil part counts them
// without building any.
//
// It reads the fields where they are and keeps nothing of them: what it
// holds is the text of the part it fills, which part may only read until
// it returns, and the lists and objects that text is inside. So the parts
// of one delta can be cut for any number of connections from the one
// entry of the log.
func splitDelta(fields []deltaField, part func(i int, text []byte) bool) (int, error) {
	w := &partWriter{room: partDataMax, emit: part}
	for _, f := range fields {
		if w.stop {
			return w.n, nil
		}
		pre := `,"` + f.key + `":`
		if w.item(pre, f.value) {
			continue
		}
		w.push(pre+"[", "]", "]", true)
		err := eachItem(f.value, func(it []byte) error {
			if w.stop {
				return errPartStop
			}
			if w.item("", it) {
				return nil
			}
			switch f.key {
			case "cells":
				return w.row(it)
			case "struct":
				return w.edit(it)
			}
			return errDeltaItem
		})
		if errors.Is(err, errPartStop) {
			return w.n, nil
		}
		if err != nil {
			return 0, err
		}
		w.pop()
	}
	if !w.stop {
		w.flush()
	}
	return w.n, nil
}

// partHead opens delta_part i of count; the part's fields follow.
func partHead(seq string, i, count int) string {
	return `{"type":"delta_part","seq":"` + seq + `","part":` + strconv.Itoa(i) + `,"parts":` + strconv.Itoa(count)
}

// partWriter fills parts. open holds the lists and objects that the next
// item goes into; when a part ends inside them they are closed and opened
// again at the start of the next. Without emit it only counts.
type partWriter struct {
	room int
	emit func(i int, text []byte) bool
	stop bool // emit asked to stop
	n    int  // parts ended
	cur  []byte
	size int
	base int // the size of the current part's opening of open
	open []partLevel
}

type partLevel struct {
	reopen string // opens the level at the start of a part
	cut    string // closes it at the end of a part that it goes on after
	end    string // closes it when its last item is written
	list   bool
	n      int // items written in the current part
}

func (w *partWriter) add(s string) {
	if w.emit != nil {
		w.cur = append(w.cur, s...)
	}
	w.size += len(s)
}

func (w *partWriter) addBytes(b []byte) {
	if w.emit != nil {
		w.cur = append(w.cur, b...)
	}
	w.size += len(b)
}

func (w *partWriter) fits(n int) bool {
	closing := 1
	for _, l := range w.open {
		closing += len(l.cut)
	}
	return w.size+n+closing <= w.room
}

func (w *partWriter) top() *partLevel {
	if len(w.open) == 0 {
		return nil
	}
	return &w.open[len(w.open)-1]
}

func (w *partWriter) sep() string {
	if l := w.top(); l != nil && l.list && l.n > 0 {
		return ","
	}
	return ""
}

// flush ends the current part and starts the next inside the same levels.
func (w *partWriter) flush() {
	for i := len(w.open) - 1; i >= 0; i-- {
		w.add(w.open[i].cut)
	}
	w.add("}")
	if w.emit != nil && !w.emit(w.n, w.cur) {
		w.stop = true
	}
	w.n++
	w.cur, w.size = w.cur[:0], 0
	for i := range w.open {
		w.add(w.open[i].reopen)
		w.open[i].n = 0
		if i > 0 {
			w.open[i-1].n = 1
		}
	}
	w.base = w.size
}

// item writes pre and text whole, in the current part when they fit and
// else in the next. It reports false when they do not fit a part of their
// own.
func (w *partWriter) item(pre string, text []byte) bool {
	n := len(pre) + len(text)
	if !w.fits(len(w.sep())+n) && w.size > w.base {
		w.flush()
	}
	if !w.fits(len(w.sep()) + n) {
		return false
	}
	w.add(w.sep())
	w.add(pre)
	w.addBytes(text)
	if l := w.top(); l != nil {
		l.n++
	}
	return true
}

func (w *partWriter) push(open, cut, end string, list bool) {
	w.add(w.sep())
	w.add(open)
	if l := w.top(); l != nil {
		l.n++
	}
	w.open = append(w.open, partLevel{reopen: open, cut: cut, end: end, list: list})
}

func (w *partWriter) pop() {
	w.add(w.top().end)
	w.open = w.open[:len(w.open)-1]
}

// row writes a row of cells, [row id, [cell, ...]], that is longer than a
// part: its cells as far as they fit, and the rest under the same row id.
func (w *partWriter) row(text []byte) error {
	comma := bytes.IndexByte(text, ',')
	if len(text) < 4 || text[0] != '[' || comma < 0 || comma+1 >= len(text) || text[comma+1] != '[' || text[len(text)-1] != ']' {
		return errDeltaItem
	}
	w.push(string(text[:comma+2]), "]]", "]]", true)
	err := eachItem(text[comma+1:len(text)-1], func(c []byte) error {
		if w.stop {
			return errPartStop
		}
		if !w.item("", c) {
			return errDeltaItem
		}
		return nil
	})
	if err != nil {
		return err
	}
	w.pop()
	return nil
}

// edit writes a structural edit that is longer than a part: its op and tab
// in every part it takes, its other fields as far as they fit, and a list
// field that does not fit a part item by item.
func (w *partWriter) edit(text []byte) error {
	members, ok := jsonMembers(text, 0)
	if !ok || len(members) < 2 || members[0].key != "op" || members[1].key != "tab" {
		return errDeltaItem
	}
	w.push(`{"op":`+string(members[0].value)+`,"tab":`+string(members[1].value), `,"more":true}`, "}", false)
	for _, m := range members[2:] {
		if w.stop {
			return errPartStop
		}
		pre := `,"` + m.key + `":`
		if w.item(pre, m.value) {
			continue
		}
		w.push(pre+"[", "]", "]", true)
		err := eachItem(m.value, func(it []byte) error {
			if w.stop {
				return errPartStop
			}
			if !w.item("", it) {
				return errDeltaItem
			}
			return nil
		})
		if err != nil {
			return err
		}
		w.pop()
	}
	w.pop()
	return nil
}

type jsonMember struct {
	key   string
	value []byte
}

// jsonMembers reads the members of the JSON object at the start of text.
// The text is one the sheet wrote: without white space, and with names
// that hold no escape.
func jsonMembers(text []byte, i int) ([]jsonMember, bool) {
	if i >= len(text) || text[i] != '{' {
		return nil, false
	}
	var out []jsonMember
	i++
	if i < len(text) && text[i] == '}' {
		return out, true
	}
	for i < len(text) {
		end := jsonEnd(text, i)
		if end < 0 || end >= len(text) || text[i] != '"' || text[end] != ':' {
			return nil, false
		}
		key := string(text[i+1 : end-1])
		start := end + 1
		if end = jsonEnd(text, start); end < 0 || end >= len(text) {
			return nil, false
		}
		out = append(out, jsonMember{key, text[start:end]})
		switch text[end] {
		case ',':
			i = end + 1
		case '}':
			return out, true
		default:
			return nil, false
		}
	}
	return nil, false
}

// eachItem calls fn with each item of the JSON list that is text, in
// order, and stops at the first error. A text that is no list is
// errDeltaItem; the items before the fault have been seen by then.
func eachItem(text []byte, fn func(item []byte) error) error {
	if len(text) < 2 || text[0] != '[' || text[len(text)-1] != ']' {
		return errDeltaItem
	}
	i := 1
	if text[i] == ']' {
		if i != len(text)-1 {
			return errDeltaItem
		}
		return nil
	}
	for i < len(text) {
		end := jsonEnd(text, i)
		if end < 0 || end >= len(text) {
			return errDeltaItem
		}
		if err := fn(text[i:end]); err != nil {
			return err
		}
		switch text[end] {
		case ',':
			i = end + 1
		case ']':
			if end != len(text)-1 {
				return errDeltaItem
			}
			return nil
		default:
			return errDeltaItem
		}
	}
	return errDeltaItem
}

// jsonEnd returns where the JSON value that starts at i ends, or -1.
func jsonEnd(text []byte, i int) int {
	if i >= len(text) {
		return -1
	}
	switch text[i] {
	case '"':
		for j := i + 1; j < len(text); j++ {
			switch text[j] {
			case '\\':
				j++
			case '"':
				return j + 1
			}
		}
		return -1
	case '{', '[':
		depth := 0
		for j := i; j < len(text); j++ {
			switch text[j] {
			case '"':
				end := jsonEnd(text, j)
				if end < 0 {
					return -1
				}
				j = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return j + 1
				}
			}
		}
		return -1
	default:
		j := i
		for j < len(text) && text[j] != ',' && text[j] != ']' && text[j] != '}' {
			j++
		}
		return j
	}
}
