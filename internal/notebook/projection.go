package notebook

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

func writePublicItem(w io.Writer, item Item) error {
	if _, ok := item.Data.(Row); ok {
		return writeSpillRow(w, item, true)
	}
	fields := map[string]any{
		"id":   item.ID(),
		"kind": item.Kind(),
	}
	switch data := item.Data.(type) {
	case Page:
		fields["title"] = data.Title
		fields["parent"] = data.Parent
		fields["position"] = data.Position
	case Block:
		fields["page"] = data.Page
		fields["parent"] = data.Parent
		fields["position"] = data.Position
		fields["content"] = data.Content
	case Database:
		fields["block"] = data.Block
		fields["title"] = data.Title
		fields["property_order"] = data.PropertyOrder
		fields["view_order"] = data.ViewOrder
	case Property:
		fields["database"] = data.Database
		fields["definition"] = data.Definition
		fields["option_order"] = data.OptionOrder
	case Option:
		fields["property"] = data.Property
		fields["name"] = data.Name
		fields["color"] = data.Color
	case View:
		fields["database"] = data.Database
		fields["definition"] = data.Definition
	default:
		return fmt.Errorf("unsupported public item data %T", item.Data)
	}
	if item.Kind() != item.Data.itemKind() {
		return fmt.Errorf("public item data does not match id kind")
	}
	return json.NewEncoder(w).Encode(fields)
}

// Stored JSON is already canonical. Read byte slices rather than materializing
// a driver value and re-encoding it, including when a slice ends inside a string.
type spillColumnReader struct {
	ctx    context.Context
	q      dbReader
	query  string
	args   []any
	offset int
	data   []byte
	done   bool
}

func (r *spillColumnReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.data) == 0 {
		if r.done {
			return 0, io.EOF
		}
		args := []any{r.offset + 1, 2 * spillChunk}
		args = append(args, r.args...)
		if err := r.q.QueryRowContext(r.ctx, r.query, args...).Scan(&r.data); err != nil {
			return 0, err
		}
		r.offset += len(r.data)
		r.done = len(r.data) < 2*spillChunk
		if len(r.data) == 0 {
			return 0, io.EOF
		}
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func spillContentFields(kind Kind) []string {
	switch kind {
	case PageKind:
		return []string{"title", "parent", "position"}
	case BlockKind:
		return []string{"page", "parent", "position", "content"}
	case DatabaseKind:
		return []string{"block", "title"}
	case PropertyKind:
		return []string{"database", "definition"}
	case OptionKind:
		return []string{"property", "name", "color"}
	case RowKind:
		return []string{"database", "position"}
	case ViewKind:
		return []string{"database", "definition"}
	}
	return nil
}

// Only the listed content fields can cross the Public boundary. The stored
// object comes from putRecord; nested values are copied without token decoding.
func streamSpillFields(w *bufio.Writer, source io.Reader, kind Kind) error {
	r := bufio.NewReaderSize(source, spillChunk)
	b, err := r.ReadByte()
	if err != nil {
		return err
	}
	if b != '{' {
		return fmt.Errorf("invalid stored content object")
	}
	fields := spillContentFields(kind)
	for n := 0; ; n++ {
		b, err = r.ReadByte()
		if err != nil {
			return err
		}
		if b == '}' {
			if n != len(fields) {
				return fmt.Errorf("missing stored content field")
			}
			return nil
		}
		if b != '"' {
			return fmt.Errorf("invalid stored content key")
		}
		var key [32]byte
		length := 0
		for {
			b, err = r.ReadByte()
			if err != nil {
				return err
			}
			if b == '"' {
				break
			}
			if length == len(key) {
				return fmt.Errorf("invalid stored content key")
			}
			key[length] = b
			length++
		}
		name := string(key[:length])
		found := false
		for _, field := range fields {
			found = found || name == field
		}
		if !found {
			return fmt.Errorf("unexpected stored content field %q", name)
		}
		b, err = r.ReadByte()
		if err != nil {
			return err
		}
		if b != ':' {
			return fmt.Errorf("invalid stored content field")
		}
		if _, err := fmt.Fprintf(w, ",%q:", name); err != nil {
			return err
		}
		depth, quoted, escaped := 0, false, false
		for {
			b, err = r.ReadByte()
			if err != nil {
				return err
			}
			if !quoted && depth == 0 && (b == ',' || b == '}') {
				break
			}
			if err := w.WriteByte(b); err != nil {
				return err
			}
			if quoted {
				if escaped {
					escaped = false
				} else if b == '\\' {
					escaped = true
				} else if b == '"' {
					quoted = false
				}
			} else {
				switch b {
				case '"':
					quoted = true
				case '{', '[':
					depth++
				case '}', ']':
					depth--
				}
			}
		}
		if b == '}' {
			if n+1 != len(fields) {
				return fmt.Errorf("missing stored content field")
			}
			return nil
		}
	}
}
