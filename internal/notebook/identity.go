package notebook

import (
	"bytes"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Kind string

const (
	PageKind     Kind = "page"
	BlockKind    Kind = "block"
	DatabaseKind Kind = "database"
	PropertyKind Kind = "property"
	OptionKind   Kind = "option"
	RowKind      Kind = "row"
	ViewKind     Kind = "view"
)

var kindPrefixes = map[Kind]string{PageKind: "p_", BlockKind: "b_", DatabaseKind: "d_", PropertyKind: "pr_", OptionKind: "o_", RowKind: "r_", ViewKind: "v_"}
var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

type ID string

func ParseID(s string) (ID, error) {
	for _, prefix := range kindPrefixes {
		if !strings.HasPrefix(s, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(s, prefix)
		if len(suffix) != 26 {
			return "", fmt.Errorf("invalid notebook id %q", s)
		}
		decoded, err := idEncoding.DecodeString(suffix)
		if err != nil || len(decoded) != 16 || idEncoding.EncodeToString(decoded) != suffix {
			return "", fmt.Errorf("invalid notebook id %q", s)
		}
		return ID(s), nil
	}
	return "", fmt.Errorf("invalid notebook id %q", s)
}

func (id ID) Kind() Kind {
	for kind, prefix := range kindPrefixes {
		if strings.HasPrefix(string(id), prefix) {
			return kind
		}
	}
	return ""
}
func (id ID) Validate() error { _, err := ParseID(string(id)); return err }
func (id ID) ValidateKind(kind Kind) error {
	if err := id.Validate(); err != nil {
		return err
	}
	if id.Kind() != kind {
		return fmt.Errorf("id %q is not %s", id, kind)
	}
	return nil
}
func (id ID) MarshalJSON() ([]byte, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(string(id))
}
func (id *ID) UnmarshalJSON(data []byte) error {
	var s string
	if err := strictDecode(data, &s); err != nil {
		return err
	}
	parsed, err := ParseID(s)
	if err == nil {
		*id = parsed
	}
	return err
}

type Sequence uint64
type Version uint64
type Incarnation uint64

func decimalJSON(n uint64) ([]byte, error) { return json.Marshal(strconv.FormatUint(n, 10)) }
func parseUintJSON(data []byte) (uint64, error) {
	var s string
	if err := strictDecode(data, &s); err != nil {
		return 0, err
	}
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return 0, fmt.Errorf("invalid unsigned decimal %q", s)
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid unsigned decimal %q", s)
		}
	}
	return strconv.ParseUint(s, 10, 64)
}
func (n Sequence) MarshalJSON() ([]byte, error) { return decimalJSON(uint64(n)) }
func (n *Sequence) UnmarshalJSON(b []byte) error {
	v, e := parseUintJSON(b)
	if e == nil {
		*n = Sequence(v)
	}
	return e
}
func (n Version) MarshalJSON() ([]byte, error) { return decimalJSON(uint64(n)) }
func (n *Version) UnmarshalJSON(b []byte) error {
	v, e := parseUintJSON(b)
	if e == nil {
		*n = Version(v)
	}
	return e
}
func (n Incarnation) MarshalJSON() ([]byte, error) { return decimalJSON(uint64(n)) }
func (n *Incarnation) UnmarshalJSON(b []byte) error {
	v, e := parseUintJSON(b)
	if e == nil {
		*n = Incarnation(v)
	}
	return e
}

type Ref struct {
	ID  ID          `json:"id"`
	Inc Incarnation `json:"inc"`
}

func (r Ref) ValidateKind(kind Kind) error { return r.ID.ValidateKind(kind) }
func (r Ref) Validate() error              { return r.ID.Validate() }
func (r Ref) MarshalJSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	type wire Ref
	return json.Marshal(wire(r))
}
func (r *Ref) UnmarshalJSON(data []byte) error {
	type wire Ref
	var v wire
	if err := requiredFields(data, "id", "inc"); err != nil {
		return err
	}
	if err := strictDecode(data, &v); err != nil {
		return err
	}
	if err := Ref(v).Validate(); err != nil {
		return err
	}
	*r = Ref(v)
	return nil
}

type Target struct {
	Ref *Ref   `json:"-"`
	Tmp string `json:"-"`
}

func (t Target) Validate() error {
	if (t.Ref == nil) == (t.Tmp == "") {
		return fmt.Errorf("target needs exactly one reference or temporary name")
	}
	if t.Ref != nil {
		return t.Ref.Validate()
	}
	if !utf8.ValidString(t.Tmp) || len(t.Tmp) > 128 {
		return fmt.Errorf("invalid temporary name")
	}
	for _, c := range t.Tmp {
		if c < 0x21 || c > 0x7e {
			return fmt.Errorf("invalid temporary name")
		}
	}
	return nil
}
func (t Target) ValidateKind(kind Kind) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if t.Ref != nil {
		return t.Ref.ValidateKind(kind)
	}
	return nil
}
func (t Target) MarshalJSON() ([]byte, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	if t.Ref != nil {
		return json.Marshal(t.Ref)
	}
	return json.Marshal(struct {
		Tmp string `json:"tmp"`
	}{t.Tmp})
}
func (t *Target) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := strictDecode(data, &fields); err != nil {
		return err
	}
	var v Target
	if _, ok := fields["tmp"]; ok {
		var w struct {
			Tmp string `json:"tmp"`
		}
		if err := strictDecode(data, &w); err != nil {
			return err
		}
		v.Tmp = w.Tmp
	} else {
		var ref Ref
		if err := strictDecode(data, &ref); err != nil {
			return err
		}
		v.Ref = &ref
	}
	if err := v.Validate(); err != nil {
		return err
	}
	*t = v
	return nil
}

func strictDecode(data []byte, out any) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("invalid UTF-8 JSON")
	}
	if err := validateSurrogates(data); err != nil {
		return err
	}
	scan := json.NewDecoder(bytes.NewReader(data))
	scan.UseNumber()
	if err := scanJSON(scan, 0); err != nil {
		return err
	}
	if _, err := scan.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	if err := checkJSONShape(data, reflect.TypeOf(out).Elem()); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return err
	}
	return nil
}
func validateSurrogates(data []byte) error {
	inString := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) || data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return fmt.Errorf("incomplete unicode escape")
		}
		n, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil {
			return err
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return fmt.Errorf("unpaired low surrogate")
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return fmt.Errorf("unpaired high surrogate")
			}
			low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return fmt.Errorf("unpaired high surrogate")
			}
			i += 6
		}
	}
	return nil
}

var unmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()

func checkJSONShape(data []byte, t reflect.Type) error {
	if t.Kind() == reflect.Interface {
		return nil
	}
	if t.Kind() == reflect.Pointer {
		if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
			return nil
		}
		return checkJSONShape(data, t.Elem())
	}
	if reflect.PointerTo(t).Implements(unmarshalerType) {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("null is not valid for %s", t)
	}
	switch t.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		known := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if !field.IsExported() {
				continue
			}
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			known[name] = field.Type
		}
		for key, raw := range fields {
			field, ok := known[key]
			if !ok {
				return fmt.Errorf("unknown field %q", key)
			}
			if err := checkJSONShape(raw, field); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return err
		}
		for _, raw := range values {
			if err := checkJSONShape(raw, t.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		for _, raw := range fields {
			if err := checkJSONShape(raw, t.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func scanJSON(dec *json.Decoder, depth int) error {
	if depth > 128 {
		return fmt.Errorf("JSON nesting exceeds 128")
	}
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			token, err = dec.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok {
				return fmt.Errorf("invalid object key")
			}
			if seen[key] {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = true
			if err := scanJSON(dec, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := scanJSON(dec, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected delimiter")
	}
	_, err = dec.Token()
	return err
}
func requiredFields(data []byte, names ...string) error {
	var fields map[string]json.RawMessage
	if err := strictDecode(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return fmt.Errorf("expected object")
	}
	for _, name := range names {
		v, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return fmt.Errorf("missing %s", name)
		}
	}
	return nil
}

func CanonicalJSON(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var normalized any
	if err := strictDecode(data, &normalized); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(normalized); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
