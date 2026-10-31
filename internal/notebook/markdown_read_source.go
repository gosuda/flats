package notebook

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// markdownString decodes one JSON string without retaining the encoded value.
type markdownString struct {
	r             *bufio.Reader
	started, done bool
	pending       []byte
}

func (r *markdownString) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if len(r.pending) > 0 {
			copied := copy(p[n:], r.pending)
			n += copied
			r.pending = r.pending[copied:]
			continue
		}
		if r.done {
			if n > 0 {
				return n, nil
			}
			return 0, io.EOF
		}
		if !r.started {
			b, err := r.r.ReadByte()
			if err != nil {
				return n, err
			}
			if strings.ContainsRune(" \r\n\t", rune(b)) {
				continue
			}
			if b != '"' {
				return n, fmt.Errorf("invalid Markdown text")
			}
			r.started = true
		}
		ch, _, err := r.r.ReadRune()
		if err != nil {
			return n, err
		}
		if ch == '"' {
			r.done = true
			continue
		}
		if ch == '\\' {
			b, err := r.r.ReadByte()
			if err != nil {
				return n, err
			}
			switch b {
			case '"', '\\', '/':
				ch = rune(b)
			case 'b':
				ch = '\b'
			case 'f':
				ch = '\f'
			case 'n':
				ch = '\n'
			case 'r':
				ch = '\r'
			case 't':
				ch = '\t'
			case 'u':
				var hex [4]byte
				if _, err := io.ReadFull(r.r, hex[:]); err != nil {
					return n, err
				}
				v, err := strconv.ParseUint(string(hex[:]), 16, 16)
				if err != nil {
					return n, err
				}
				ch = rune(v)
				if utf16.IsSurrogate(ch) {
					var pair [6]byte
					if _, err := io.ReadFull(r.r, pair[:]); err != nil {
						return n, err
					}
					if pair[0] != '\\' || pair[1] != 'u' {
						return n, fmt.Errorf("invalid Markdown surrogate")
					}
					v, err := strconv.ParseUint(string(pair[2:]), 16, 16)
					if err != nil {
						return n, err
					}
					ch = utf16.DecodeRune(ch, rune(v))
				}
			default:
				return n, fmt.Errorf("invalid Markdown escape")
			}
		}
		var encoded [4]byte
		size := utf8.EncodeRune(encoded[:], ch)
		copied := copy(p[n:], encoded[:size])
		n += copied
		if copied < size {
			r.pending = append(r.pending[:0], encoded[copied:size]...)
		}
	}
	return n, nil
}

func markdownWalk(v revisionRange, object bool, visit func(string, revisionRange) error, c *markdownReadCapture) error {
	source := io.NewSectionReader(v.file, v.start, v.size)
	ranges := readJSONRanges{reader: bufio.NewReaderSize(source, int(min(v.size, 4096))), source: source, start: v.start}
	return ranges.each(object, func(key string, start, end int64) error {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		return visit(key, revisionRange{file: v.file, start: start, size: end - start})
	})
}

func (c *markdownReadCapture) stage(source *bufio.Reader) (revisionRange, func(), error) {
	files := spillCapture{s: c.s}
	file, err := files.queryFile("markdown-source-")
	if err != nil {
		return revisionRange{}, func() {}, err
	}
	writer := captureWriter{s: c.s, out: file}
	buffer := bufio.NewWriterSize(&writer, spillChunk)
	scanner := revisionScanner{ctx: c.ctx, r: source, output: buffer}
	err = scanner.value(0)
	if err == nil {
		err = buffer.Flush()
	}
	close := func() {
		files.closeQueryFile(file)
		_ = c.s.charge(-writer.offset)
	}
	if err != nil {
		close()
		return revisionRange{}, func() {}, err
	}
	return revisionRange{file: file, size: writer.offset}, close, nil
}

func markdownSeekField(r *bufio.Reader, field string) error {
	nonspace := func() (byte, error) {
		for {
			b, err := r.ReadByte()
			if err != nil || !strings.ContainsRune(" \r\n\t", rune(b)) {
				return b, err
			}
		}
	}
	if b, err := nonspace(); err != nil {
		return err
	} else if b != '{' {
		return fmt.Errorf("invalid Markdown object")
	}
	for {
		b, err := nonspace()
		if err != nil {
			return err
		}
		if b == '}' {
			return fmt.Errorf("missing Markdown field %s", field)
		}
		if err := r.UnreadByte(); err != nil {
			return err
		}
		var key bytes.Buffer
		if err := scanReadValue(r, &key); err != nil {
			return err
		}
		var name string
		if err := json.Unmarshal(key.Bytes(), &name); err != nil {
			return err
		}
		if b, err := nonspace(); err != nil {
			return err
		} else if b != ':' {
			return fmt.Errorf("invalid Markdown field")
		}
		if name == field {
			return nil
		}
		if err := scanReadValue(r, nil); err != nil {
			return err
		}
		if b, err := nonspace(); err != nil {
			return err
		} else if b != ',' {
			return fmt.Errorf("missing Markdown field %s", field)
		}
	}
}

func (c *markdownReadCapture) rawField(id ID, path string) (*bufio.Reader, error) {
	var source io.Reader = &spillColumnReader{ctx: c.ctx, q: c.tx, query: `SELECT substr(CAST(data AS BLOB),?,?) FROM items WHERE id=?`, args: []any{id}}
	r := bufio.NewReaderSize(source, 1024)
	for _, field := range strings.Split(strings.TrimPrefix(path, "$."), ".") {
		if err := markdownSeekField(r, field); err != nil {
			return nil, err
		}
	}
	return r, nil
}

type markdownFailedReader struct{ err error }

func (r markdownFailedReader) Read([]byte) (int, error) { return 0, r.err }

func markdownRangeText(v revisionRange) io.Reader {
	return &markdownString{r: bufio.NewReaderSize(v.reader(), int(min(v.size, 16<<10)))}
}

func (c *markdownReadCapture) fields(v revisionRange) (map[string]revisionRange, error) {
	out := map[string]revisionRange{}
	err := markdownWalk(v, true, func(key string, value revisionRange) error { out[key] = value; return nil }, c)
	return out, err
}

func markdownEqual(a, b revisionRange) (bool, error) {
	x, y := markdownReader(markdownRangeText(a)), markdownReader(markdownRangeText(b))
	defer releaseMarkdownReader(x)
	defer releaseMarkdownReader(y)
	for {
		left, le := x.ReadByte()
		right, re := y.ReadByte()
		if le != nil && le != io.EOF {
			return false, le
		}
		if re != nil && re != io.EOF {
			return false, re
		}
		if le == io.EOF || re == io.EOF {
			return le == re, nil
		}
		if left != right {
			return false, nil
		}
	}
}

// The rank index is built once per database, not once per row. Selection
// bitsets and the rank-to-id vector stay on disk with the capture's quota.
type markdownReadOptions struct {
	capture spillCapture
	ids     *os.File
	count   int
}

func (c *markdownReadCapture) releaseOptions() {
	if c.options == nil {
		return
	}
	v := c.options
	v.capture.closeQueryFile(v.ids)
	v.capture.closeQueryFile(v.capture.ranks)
	_ = c.s.charge(-int64(v.count*64) - v.capture.rankSlots*spillRankSlot)
	c.options = nil
}

func (c *markdownReadCapture) selected(out markdownLines, row, property ID) error {
	options := c.options
	collection := spillQueryCollection{capture: &options.capture, raw: func() io.Reader {
		return &spillColumnReader{ctx: c.ctx, q: c.tx, query: `SELECT substr(CAST(value AS BLOB),?,?) FROM cells WHERE row_id=? AND property_id=?`, args: []any{row, property}}
	}}
	next, close := collection.ranks(dbQueryColumn{rank: func(id ID) int {
		rank, err := options.capture.queryRank(id, nil)
		if err != nil {
			collection.fail(err)
		}
		return rank
	}})
	defer close()
	first := true
	for {
		rank, more := next()
		if !more {
			break
		}
		if rank == 0 {
			continue
		}
		var slot [64]byte
		if _, err := options.ids.ReadAt(slot[:], int64((rank-1)*64)); err != nil {
			return err
		}
		if ID(strings.TrimRight(string(slot[32:]), "\x00")) != property {
			continue
		}
		id := ID(strings.TrimRight(string(slot[:32]), "\x00"))
		if !first {
			out.text(", ")
		}
		first = false
		if err := markdownCopy(out, c.itemText(id, "$.name"), true, true, false); err != nil {
			return err
		}
	}
	if err := options.capture.queryError; err != nil {
		return err
	}
	return out.out.err
}
