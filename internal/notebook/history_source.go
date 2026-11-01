package notebook

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

// Revision ranges keep source strings and child arrays on disk throughout capture.
type revisionRange struct {
	file        *os.File
	start, size int64
}

func (v revisionRange) reader() io.Reader { return io.NewSectionReader(v.file, v.start, v.size) }

func (v revisionRange) decode(out any) error {
	if v.size > spillChunk {
		return fmt.Errorf("oversized revision metadata")
	}
	raw, err := io.ReadAll(v.reader())
	if err != nil {
		return err
	}
	return strictDecode(raw, out)
}

type revisionScanner struct {
	ctx    context.Context
	r      *bufio.Reader
	file   *os.File
	offset int64
	output *bufio.Writer
}

func (s *revisionScanner) peek() (byte, error) {
	if s.offset%spillChunk == 0 {
		if err := s.ctx.Err(); err != nil {
			return 0, err
		}
	}
	b, err := s.r.Peek(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}
func (s *revisionScanner) take() (byte, error) {
	b, err := s.peek()
	if err == nil {
		_, err = s.r.ReadByte()
		s.offset++
		if err == nil && s.output != nil {
			err = s.output.WriteByte(b)
		}
	}
	return b, err
}
func (s *revisionScanner) space() error {
	for {
		b, err := s.peek()
		if err != nil {
			return err
		}
		if !strings.ContainsRune(" \t\r\n", rune(b)) {
			return nil
		}
		_, _ = s.take()
	}
}
func (s *revisionScanner) expect(want byte) error {
	if err := s.space(); err != nil {
		return err
	}
	b, err := s.take()
	if err != nil {
		return err
	}
	if b != want {
		return fmt.Errorf("invalid revision JSON delimiter")
	}
	return nil
}
func (s *revisionScanner) unicodeEscape() (uint16, error) {
	var value uint16
	for range 4 {
		b, err := s.take()
		if err != nil {
			return 0, err
		}
		value <<= 4
		switch {
		case b >= '0' && b <= '9':
			value |= uint16(b - '0')
		case b >= 'a' && b <= 'f':
			value |= uint16(b - 'a' + 10)
		case b >= 'A' && b <= 'F':
			value |= uint16(b - 'A' + 10)
		default:
			return 0, fmt.Errorf("invalid revision JSON escape")
		}
	}
	return value, nil
}

func (s *revisionScanner) quoted() error {
	if b, err := s.take(); err != nil {
		return err
	} else if b != '"' {
		return fmt.Errorf("invalid revision JSON string")
	}
	for {
		b, err := s.take()
		if err != nil {
			return err
		}
		if b == '"' {
			return nil
		}
		if b < 0x20 {
			return fmt.Errorf("invalid revision JSON string")
		}
		if b >= 0x80 {
			var encoded [4]byte
			encoded[0] = b
			n := 2
			if b >= 0xe0 {
				n = 3
			}
			if b >= 0xf0 {
				n = 4
			}
			for i := 1; i < n; i++ {
				encoded[i], err = s.take()
				if err != nil {
					return err
				}
			}
			if !utf8.Valid(encoded[:n]) {
				return fmt.Errorf("invalid UTF-8 JSON")
			}
		}
		if b != '\\' {
			continue
		}
		b, err = s.take()
		if err != nil {
			return err
		}
		if b != 'u' {
			if !strings.ContainsRune(`"\/bfnrt`, rune(b)) {
				return fmt.Errorf("invalid revision JSON escape")
			}
			continue
		}
		code, err := s.unicodeEscape()
		if err != nil {
			return err
		}
		if code >= 0xdc00 && code <= 0xdfff {
			return fmt.Errorf("unpaired revision JSON surrogate")
		}
		if code < 0xd800 || code > 0xdbff {
			continue
		}
		if b, err = s.take(); err != nil {
			return err
		} else if b != '\\' {
			return fmt.Errorf("unpaired revision JSON surrogate")
		}
		if b, err = s.take(); err != nil {
			return err
		} else if b != 'u' {
			return fmt.Errorf("unpaired revision JSON surrogate")
		}
		low, err := s.unicodeEscape()
		if err != nil {
			return err
		}
		if low < 0xdc00 || low > 0xdfff {
			return fmt.Errorf("unpaired revision JSON surrogate")
		}
	}
}

func (s *revisionScanner) value(depth int) error {
	if depth > 128 {
		return fmt.Errorf("JSON nesting exceeds 128")
	}
	if err := s.space(); err != nil {
		return err
	}
	b, err := s.peek()
	if err != nil {
		return err
	}
	switch b {
	case '"':
		return s.quoted()
	case '{', '[':
		object := b == '{'
		_, _ = s.take()
		close := byte(']')
		if object {
			close = '}'
		}
		if err := s.space(); err != nil {
			return err
		}
		b, _ = s.peek()
		if b == close {
			_, err = s.take()
			return err
		}
		for {
			if object {
				if err := s.quoted(); err != nil {
					return err
				}
				if err := s.expect(':'); err != nil {
					return err
				}
			}
			if err := s.value(depth + 1); err != nil {
				return err
			}
			if err := s.space(); err != nil {
				return err
			}
			b, err = s.take()
			if err != nil {
				return err
			}
			if b == close {
				return nil
			}
			if b != ',' {
				return fmt.Errorf("invalid revision JSON separator")
			}
			if err := s.space(); err != nil {
				return err
			}
		}
	default:
		var token [32]byte
		n := 0
		for {
			b, err = s.peek()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if strings.ContainsRune(" \t\r\n,]}", rune(b)) {
				break
			}
			if n == len(token) {
				return fmt.Errorf("invalid revision JSON literal")
			}
			token[n] = b
			n++
			_, _ = s.take()
		}
		if n == 0 || !json.Valid(token[:n]) {
			return fmt.Errorf("invalid revision JSON literal")
		}
		return nil
	}
}

func (s *revisionScanner) walk(object bool, visit func(string) error) error {
	open, close := byte('['), byte(']')
	if object {
		open, close = '{', '}'
	}
	if err := s.expect(open); err != nil {
		return err
	}
	if err := s.space(); err != nil {
		return err
	}
	b, _ := s.peek()
	if b != close {
		for {
			key := ""
			if object {
				start := s.offset
				if err := s.quoted(); err != nil {
					return err
				}
				if s.offset-start > 128 {
					return fmt.Errorf("invalid revision field")
				}
				if err := (revisionRange{s.file, start, s.offset - start}).decode(&key); err != nil {
					return err
				}
				if err := s.expect(':'); err != nil {
					return err
				}
			}
			if err := s.space(); err != nil {
				return err
			}
			if err := visit(key); err != nil {
				return err
			}
			if err := s.space(); err != nil {
				return err
			}
			b, _ = s.peek()
			if b == close {
				break
			}
			if err := s.expect(','); err != nil {
				return err
			}
			if err := s.space(); err != nil {
				return err
			}
		}
	}
	return s.expect(close)
}

func (s *revisionScanner) end() error {
	for {
		b, err := s.take()
		if errors.Is(err, io.EOF) {
			return s.ctx.Err()
		}
		if err != nil {
			return err
		}
		if !strings.ContainsRune(" 	\r\n", rune(b)) {
			return fmt.Errorf("trailing revision content")
		}
	}
}

func walkRevisionRanges(ctx context.Context, v revisionRange, object bool, visit func(string, revisionRange) error) error {
	s := revisionScanner{ctx: ctx, r: bufio.NewReaderSize(v.reader(), spillChunk), file: v.file, offset: v.start}
	if err := s.walk(object, func(key string) error {
		start := s.offset
		if err := s.value(0); err != nil {
			return err
		}
		return visit(key, revisionRange{v.file, start, s.offset - start})
	}); err != nil {
		return err
	}
	return s.end()
}

func revisionRangeFields(ctx context.Context, v revisionRange, allowed []string) (map[string]revisionRange, error) {
	fields := make(map[string]revisionRange, len(allowed))
	err := walkRevisionRanges(ctx, v, true, func(key string, value revisionRange) error {
		if !slices.Contains(allowed, key) {
			return fmt.Errorf("unknown revision field %q", key)
		}
		if _, ok := fields[key]; ok {
			return fmt.Errorf("duplicate revision field %q", key)
		}
		fields[key] = value
		return nil
	})
	return fields, err
}

type revisionSourceWriter struct {
	out         io.Writer
	charge      func(int64) error
	size, limit int64
}

func (w *revisionSourceWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.limit-w.size {
		return 0, fmt.Errorf("revision content integrity failure")
	}
	if err := w.charge(int64(len(p))); err != nil {
		return 0, err
	}
	n, err := w.out.Write(p)
	w.size += int64(n)
	return n, err
}

func streamRevisionCaptureStore(ctx context.Context, src RevisionSource, selectFile func(revisionPath) bool, c *spillCapture, consume func(*sql.Tx, Item, map[string]revisionRange) error, charge func(string) error) (*revisionStore, error) {
	files, err := checkedRevisionFiles(src)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "flats-notebook-history-")
	if err != nil {
		return nil, err
	}
	store, _, err := openStore(filepath.Join(dir, dbName))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	out := &revisionStore{store: store, dir: dir, files: files}
	ordered := slices.Clone(files)
	ranks := map[string]int{"notebook": 0, "page": 1, "block": 2, "schema": 3, "row": 4, "asset": 5}
	slices.SortStableFunc(ordered, func(a, b File) int {
		pa, _ := parseRevisionPath(a.Path)
		pb, _ := parseRevisionPath(b.Path)
		return ranks[pa.kind] - ranks[pb.kind]
	})
	err = store.transaction(ctx, func(tx *sql.Tx) error {
		for _, query := range []string{
			`CREATE TABLE history_orders(owner TEXT,kind TEXT,ordinal INTEGER,id TEXT,PRIMARY KEY(owner,kind,id))`,
			`CREATE TABLE history_cells(source TEXT,owner INTEGER,property TEXT,start INTEGER,bytes INTEGER,PRIMARY KEY(source,owner,property))`,
		} {
			if _, err := tx.ExecContext(ctx, query); err != nil {
				return err
			}
		}
		for _, f := range ordered {
			p, _ := parseRevisionPath(f.Path)
			if p.kind == "asset" {
				continue
			}
			if p.kind != "notebook" && selectFile != nil && !selectFile(p) {
				continue
			}
			file, err := os.CreateTemp(dir, "source-")
			if err != nil {
				return err
			}
			out.sources = append(out.sources, file)
			if err := func() error {
				input, err := openRevisionFile(ctx, src, f.Hash)
				if err != nil {
					return err
				}
				defer input.Close()
				hash := sha256.New()
				writer := &revisionSourceWriter{out: io.MultiWriter(file, hash), charge: c.s.charge, limit: f.Size}
				scanner := &revisionScanner{ctx: ctx, r: bufio.NewReaderSize(io.TeeReader(input, writer), spillChunk), file: file}
				if p.kind == "notebook" {
					if err := scanner.value(0); err != nil {
						return err
					}
					var marker struct {
						Format int `json:"format"`
					}
					if err := (revisionRange{file, 0, scanner.offset}).decode(&marker); err != nil || marker.Format != 1 {
						return fmt.Errorf("unsupported notebook revision format")
					}
				} else if err := c.streamRevisionSource(ctx, tx, p, scanner, consume); err != nil {
					return fmt.Errorf("revision %s: %w", f.Path, err)
				}
				if err := scanner.end(); err != nil {
					return err
				}
				if writer.size != f.Size || fmt.Sprintf("%x", hash.Sum(nil)) != f.Hash {
					return fmt.Errorf("revision content integrity failure")
				}
				return nil
			}(); err != nil {
				return err
			}
			if err := charge(dir); err != nil {
				return err
			}
		}
		return setSequence(ctx, tx, 1)
	})
	if err != nil {
		return nil, errors.Join(err, out.close())
	}
	return out, nil
}

func (c *spillCapture) streamRevisionSource(ctx context.Context, tx *sql.Tx, p revisionPath, s *revisionScanner, consume func(*sql.Tx, Item, map[string]revisionRange) error) error {
	if p.kind == "page" {
		return c.streamRevisionItem(ctx, tx, p, PageKind, s, consume)
	}
	if p.kind == "schema" {
		fields := map[string]revisionRange{}
		seen := map[string]bool{}
		err := s.walk(true, func(key string) error {
			if seen[key] {
				return fmt.Errorf("duplicate revision field %q", key)
			}
			seen[key] = true
			child := map[string]Kind{"properties": PropertyKind, "options": OptionKind, "views": ViewKind}[key]
			if child != "" {
				return s.walk(false, func(string) error { return c.streamRevisionItem(ctx, tx, p, child, s, consume) })
			}
			if !slices.Contains(append([]string{"id"}, revisionItemFields(DatabaseKind)...), key) {
				return fmt.Errorf("unknown revision field %q", key)
			}
			start := s.offset
			if err := s.value(0); err != nil {
				return err
			}
			fields[key] = revisionRange{s.file, start, s.offset - start}
			return nil
		})
		if err != nil {
			return err
		}
		for _, key := range []string{"properties", "options", "views"} {
			if !seen[key] {
				return fmt.Errorf("missing %s", key)
			}
		}
		return importRevisionRangeItem(ctx, tx, p, DatabaseKind, fields, consume)
	}
	kind := BlockKind
	if p.kind == "row" {
		kind = RowKind
	}
	var previous ID
	return s.walk(false, func(string) error {
		return c.streamRevisionItem(ctx, tx, p, kind, s, func(tx *sql.Tx, item Item, fields map[string]revisionRange) error {
			if previous != "" && item.ID() <= previous {
				return fmt.Errorf("revision bucket not in id order")
			}
			previous = item.ID()
			return consume(tx, item, fields)
		})
	})
}

func (c *spillCapture) streamRevisionItem(ctx context.Context, tx *sql.Tx, p revisionPath, kind Kind, s *revisionScanner, consume func(*sql.Tx, Item, map[string]revisionRange) error) error {
	fields := map[string]revisionRange{}
	include := c.s.scope.Tree && kind == PageKind || c.s.scope.Page != "" && kind != RowKind || c.s.scope.Database != "" && kind != PageKind && kind != BlockKind
	start := c.writer.offset
	if include {
		if _, err := fmt.Fprintf(&c.writer, `{"kind":%q,"ver":"1","v":{}`, kind); err != nil {
			return err
		}
	}
	output := bufio.NewWriterSize(&c.writer, spillChunk)
	err := s.walk(true, func(key string) error {
		if !slices.Contains(append([]string{"id"}, revisionItemFields(kind)...), key) {
			return fmt.Errorf("unknown revision field %q", key)
		}
		if _, ok := fields[key]; ok {
			return fmt.Errorf("duplicate revision field %q", key)
		}
		if include && key != "id" {
			if _, err := fmt.Fprintf(&c.writer, ",%q:", key); err != nil {
				return err
			}
			s.output = output
		}
		valueStart := s.offset
		var err error
		if key == "cells" {
			err = s.walk(true, func(property string) error {
				id := ID(property)
				if err := id.ValidateKind(PropertyKind); err != nil {
					return err
				}
				cellStart := s.offset
				if err := s.value(0); err != nil {
					return err
				}
				cell := revisionRange{s.file, cellStart, s.offset - cellStart}
				if cell.size <= spillChunk {
					var value CellValue
					if err := cell.decode(&value); err != nil {
						return err
					}
				}
				_, err := tx.ExecContext(ctx, `INSERT INTO history_cells(source,owner,property,start,bytes) VALUES(?,?,?,?,?)`, s.file.Name(), valueStart, id, cellStart, s.offset-cellStart)
				return err
			})
		} else {
			err = s.value(0)
		}
		s.output = nil
		if err != nil {
			return err
		}
		if err := output.Flush(); err != nil {
			return err
		}
		fields[key] = revisionRange{s.file, valueStart, s.offset - valueStart}
		return nil
	})
	if err != nil {
		return err
	}
	if include {
		var id ID
		if err := fields["id"].decode(&id); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(&c.writer, `,"ref":{"id":%q,"inc":"1"}}`, id); err != nil {
			return err
		}
		fields["$capture"] = revisionRange{c.s.data, start, c.writer.offset - start}
	}
	return importRevisionRangeItem(ctx, tx, p, kind, fields, consume)
}

func revisionItemFields(kind Kind) []string {
	fields := append([]string{}, spillContentFields(kind)...)
	switch kind {
	case DatabaseKind:
		fields = append(fields, "property_order", "view_order")
	case PropertyKind:
		fields = append(fields, "option_order")
	case RowKind:
		fields = append(fields, "cells")
	}
	return fields
}

// Canonical revision names use fixed-size keys in the metadata-only name index.
func revisionNameKey(ctx context.Context, field revisionRange) (string, error) {
	if field.size <= spillChunk {
		var name string
		if err := field.decode(&name); err != nil {
			return "", err
		}
		raw, err := json.Marshal(name)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
	}
	var first [1]byte
	if _, err := field.file.ReadAt(first[:], field.start); err != nil {
		return "", err
	}
	if first[0] != '"' {
		return "", fmt.Errorf("invalid revision name")
	}
	hash := sha256.New()
	reader := field.reader()
	buffer := make([]byte, spillChunk)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := reader.Read(buffer)
		_, _ = hash.Write(buffer[:n])
		if errors.Is(err, io.EOF) {
			return fmt.Sprintf("%x", hash.Sum(nil)), nil
		}
		if err != nil {
			return "", err
		}
	}
}

func importRevisionRangeItem(ctx context.Context, tx *sql.Tx, p revisionPath, kind Kind, fields map[string]revisionRange, consume func(*sql.Tx, Item, map[string]revisionRange) error) error {
	var err error
	var id ID
	if err := fields["id"].decode(&id); err != nil {
		return err
	}
	if err := id.ValidateKind(kind); err != nil {
		return err
	}
	// Only small structural metadata enters the temporary relational store. The
	// original field ranges, not these query placeholders, supply capture output.
	compact := map[string]json.RawMessage{}
	for _, key := range revisionItemFields(kind) {
		field, ok := fields[key]
		if !ok {
			return fmt.Errorf("missing %s", key)
		}
		switch key {
		case "cells":
			compact[key] = json.RawMessage(`{}`)
		case "property_order", "view_order", "option_order":
			compact[key] = json.RawMessage(`[]`)
			ordinal := 0
			err := walkRevisionRanges(ctx, field, false, func(_ string, child revisionRange) error {
				var childID ID
				if err := child.decode(&childID); err != nil {
					return err
				}
				expected := PropertyKind
				if key == "view_order" {
					expected = ViewKind
				}
				if key == "option_order" {
					expected = OptionKind
				}
				if err := childID.ValidateKind(expected); err != nil {
					return err
				}
				_, err := tx.ExecContext(ctx, `INSERT INTO history_orders(owner,kind,ordinal,id) VALUES(?,?,?,?)`, id, key, ordinal, childID)
				ordinal++
				return err
			})
			if err != nil {
				return err
			}
		case "name":
			name, err := revisionNameKey(ctx, field)
			if err != nil {
				return err
			}
			compact[key], _ = json.Marshal(name)
		case "title":
			if field.size <= spillChunk {
				var text string
				if err := field.decode(&text); err != nil {
					return err
				}
			} else {
				var first [1]byte
				if _, err := field.file.ReadAt(first[:], field.start); err != nil {
					return err
				}
				if first[0] != '"' {
					return fmt.Errorf("invalid revision text")
				}
			}
			compact[key], _ = json.Marshal(string(id))
		case "definition":
			if kind == PropertyKind {
				definition, err := revisionRangeFields(ctx, field, []string{"name", "type", "format"})
				if err != nil {
					return err
				}
				var typ PropertyType
				if err := definition["type"].decode(&typ); err != nil {
					return err
				}
				name, err := revisionNameKey(ctx, definition["name"])
				if err != nil {
					return err
				}
				var format NumberFormat
				if field, ok := definition["format"]; ok {
					if err := field.decode(&format); err != nil {
						return err
					}
				}
				compact[key], err = json.Marshal(PropertyDef{Name: name, Type: typ, Format: format})
				if err != nil {
					return err
				}
			} else {
				if field.size <= spillChunk {
					var definition StoredView
					if err := field.decode(&definition); err != nil {
						return err
					}
					compact[key], err = json.Marshal(definition)
				} else {
					definition, decodeErr := revisionRangeFields(ctx, field, []string{"name", "display", "sort", "filter"})
					if decodeErr != nil {
						return decodeErr
					}
					for _, key := range []string{"name", "display", "sort", "filter"} {
						if _, ok := definition[key]; !ok {
							return fmt.Errorf("missing %s", key)
						}
					}
					query := StoredView{Name: "History", Display: StoredDisplay{Layout: "table", Visible: []ID{}, Widths: []StoredWidth{}}}
					if err := definition["sort"].decode(&query.Sort); err != nil {
						return err
					}
					if err := definition["filter"].decode(&query.Filter); err != nil {
						return err
					}
					compact[key], err = json.Marshal(query)
				}
				if err != nil {
					return err
				}
			}
		case "content":
			if field.size <= spillChunk {
				var content Content
				if err := field.decode(&content); err != nil {
					return err
				}
				compact[key], err = json.Marshal(content)
				if err != nil {
					return err
				}
			} else {
				compact[key] = json.RawMessage(`{"kind":"paragraph","text":[]}`)
			}
		default:
			raw, err := io.ReadAll(io.LimitReader(field.reader(), spillChunk+1))
			if err != nil {
				return err
			}
			if len(raw) > spillChunk {
				return fmt.Errorf("oversized revision structural metadata")
			}
			compact[key] = raw
		}
	}
	compact["id"], _ = json.Marshal(id)
	raw, err := json.Marshal(compact)
	if err != nil {
		return err
	}
	// The normal importer enforces identities, ownership, subtype and duplicate
	// constraints without ever seeing retained payload strings or collections.
	return importRevisionItem(ctx, tx, p, kind, raw, func(tx *sql.Tx, item Item) error { return consume(tx, item, fields) })
}

func writeRevisionRangeItem(w io.Writer, item Item, fields map[string]revisionRange) error {
	if _, err := fmt.Fprintf(w, `{"kind":%q,"ref":{"id":%q,"inc":"1"},"ver":"1","v":{}`, item.Kind(), item.ID()); err != nil {
		return err
	}
	for _, key := range revisionItemFields(item.Kind()) {
		if _, err := fmt.Fprintf(w, ",%q:", key); err != nil {
			return err
		}
		if _, err := io.Copy(w, fields[key].reader()); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "}")
	return err
}

// The payload was encoded while traversing the source. Register its immutable
// range without re-encoding it or charging those bytes as progress twice.
func (c *spillCapture) addRevisionCaptured(item Item, row bool, value revisionRange) error {
	end := c.writer.offset
	c.writer.offset = value.start
	defer func() { c.writer.offset = end }()
	return c.addEncoded(item, row, func(io.Writer) error { c.writer.offset = value.start + value.size; return nil })
}

func (c *spillCapture) revisionQueryItem(ctx context.Context, tx *sql.Tx, item Item, fields map[string]revisionRange) (Item, error) {
	row := item.Data.(Row)
	wanted := map[ID]bool{}
	for _, key := range c.view.Sort {
		if key.Property.Ref != nil {
			wanted[key.Property.Ref.ID] = true
		}
	}
	for _, key := range c.view.Filter {
		if key.Property.Ref != nil {
			wanted[key.Property.Ref.ID] = true
		}
	}
	cells := fields["cells"]
	for property := range wanted {
		value := revisionRange{file: cells.file}
		err := tx.QueryRowContext(ctx, `SELECT start,bytes FROM history_cells WHERE source=? AND owner=? AND property=?`, cells.file.Name(), cells.start, property).Scan(&value.start, &value.size)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return item, err
		}
		var typ PropertyType
		if err := readRecordField(value.reader(), "type", &typ); err != nil {
			return item, err
		}
		if typ == MultiSelectType {
			row.Cells[property] = CellValue{Type: typ, Value: &spillQueryCollection{raw: value.reader, capture: c}}
			continue
		}
		if value.size > spillChunk && (typ == TextType || typ == TitleType || typ == URLType) {
			row.Cells[property] = CellValue{Type: typ, Value: &spillQueryText{raw: value.reader, err: &c.queryError}}
			continue
		}
		var cell CellValue
		if err := value.decode(&cell); err != nil {
			return item, err
		}
		row.Cells[property] = cell
	}
	item.Data = row
	return item, nil
}
