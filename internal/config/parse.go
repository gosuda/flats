package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"unicode/utf8"
)

// MaxFileSize bounds config.json.
const MaxFileSize = 1 << 20

// ErrTooNew is matched by a *TooNewError.
var ErrTooNew = errors.New("config: schema_version is newer than this Flats supports")

// TooNewError rejects a file written by a newer Flats. The file is never
// changed.
type TooNewError struct {
	Version, Supported int
}

func (e *TooNewError) Error() string {
	return fmt.Sprintf("config: schema_version %d is newer than this Flats supports (%d); "+
		"upgrade Flats or restore an older copy from config-history/", e.Version, e.Supported)
}

func (e *TooNewError) Is(target error) bool { return target == ErrTooNew }

// Migration converts the raw document (decoded with json.Number numbers)
// from schema_version From to To. Apply must keep JSON value types (string,
// bool, json.Number, []any, map[string]any); the runner sets schema_version.
// NeedsConfirm marks a step that can widen network or credentials meaning;
// the caller must confirm it before persisting.
type Migration struct {
	From, To     int
	NeedsConfirm bool
	Apply        func(doc map[string]any) error
}

type registry struct {
	current int
	steps   []Migration
}

// migrations is the production registry. Version 1 has no steps.
var migrations = registry{current: CurrentSchemaVersion}

// Loaded is a parsed config file.
type Loaded struct {
	Doc          *Document // at CurrentSchemaVersion
	Hash         string    // SHA-256 of the file bytes (the ETag)
	FileVersion  int       // schema_version in the file
	Migrated     bool      // Doc differs in version from the file; nothing was written
	NeedsConfirm bool      // a migration step needs operator confirmation
}

// Hash returns the hex SHA-256 of data, used as the config ETag.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Load reads and parses path. An older schema_version is migrated in memory
// only; persist it with Writer.Save.
func Load(path string) (*Loaded, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	return migrations.parse(data)
}

// Parse parses config.json bytes like Load.
func Parse(data []byte) (*Loaded, error) { return migrations.parse(data) }

func readFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (r registry) parse(data []byte) (*Loaded, error) {
	if len(data) > MaxFileSize {
		return nil, fmt.Errorf("config: file is larger than %d bytes", MaxFileSize)
	}
	if !utf8.Valid(data) {
		return nil, errors.New("config: file is not valid UTF-8 (save it as UTF-8)")
	}
	v, problem, err := decode(data, "")
	if err != nil {
		return nil, err
	}
	raw, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New(`config: the file must be a JSON object such as {"schema_version": 1, ...}`)
	}
	version, verr := readVersion(raw)
	if verr == nil && version > r.current {
		return nil, &TooNewError{Version: version, Supported: r.current}
	}
	if problem != nil {
		return nil, problem
	}
	if verr != nil {
		return nil, verr
	}
	needsConfirm, err := r.migrate(raw, version)
	if err != nil {
		return nil, err
	}
	doc, err := documentFromRaw(raw, r.current)
	if err != nil {
		return nil, err
	}
	return &Loaded{Doc: doc, Hash: Hash(data), FileVersion: version,
		Migrated: version < r.current, NeedsConfirm: needsConfirm}, nil
}

func readVersion(raw map[string]any) (int, error) {
	const hint = `use "schema_version": 1 for a new file`
	v, ok := raw["schema_version"]
	if !ok {
		return 0, fieldErr("schema_version", hint, "is required")
	}
	n, ok := v.(json.Number)
	if !ok || !isIntSyntax(string(n)) {
		return 0, fieldErr("schema_version", hint, "must be an integer, not %s", describe(v))
	}
	i, err := strconv.ParseInt(string(n), 10, 32)
	if err != nil || i < 1 {
		return 0, fieldErr("schema_version", hint, "%s is not a schema version", n)
	}
	return int(i), nil
}

// migrate applies the registered steps from version up to r.current.
func (r registry) migrate(raw map[string]any, version int) (needsConfirm bool, err error) {
	for version < r.current {
		i := slices.IndexFunc(r.steps, func(m Migration) bool { return m.From == version })
		if i < 0 {
			return false, fmt.Errorf("config: no migration from schema_version %d", version)
		}
		m := r.steps[i]
		if m.To <= m.From || m.To > r.current {
			return false, fmt.Errorf("config: invalid migration %d->%d", m.From, m.To)
		}
		if err := m.Apply(raw); err != nil {
			return false, fmt.Errorf("config: migrate schema_version %d to %d: %w", m.From, m.To, err)
		}
		raw["schema_version"] = json.Number(strconv.Itoa(m.To))
		needsConfirm = needsConfirm || m.NeedsConfirm
		version = m.To
	}
	return needsConfirm, nil
}

// documentFromRaw type-checks a raw document at the current version.
func documentFromRaw(raw map[string]any, version int) (*Document, error) {
	d := &Document{version: version, values: map[string]any{}}
	var errs []error
	for _, top := range sortedKeys(raw) {
		if top == "schema_version" {
			continue
		}
		if !slices.Contains(sections, top) {
			errs = append(errs, unknownFileKey(top))
			continue
		}
		obj, ok := raw[top].(map[string]any)
		if !ok {
			errs = append(errs, fieldErr(top, "use a JSON object", "must be an object, not %s", describe(raw[top])))
			continue
		}
		for _, name := range sortedKeys(obj) {
			key := top + "." + name
			sp, ok := lookupSpec(key)
			if !ok {
				errs = append(errs, unknownFileKey(key))
				continue
			}
			v, err := sp.fromRaw(obj[name], key)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			d.values[key] = v
		}
	}
	if len(errs) == 0 {
		errs = append(errs, d.Validate())
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return d, nil
}

func unknownFileKey(path string) *FieldError {
	return fieldErr(path, "remove it; it may be a key added by a newer Flats", "unknown key")
}

// fromRaw converts one decoded JSON value to the key's type and validates it.
func (sp *spec) fromRaw(raw any, path string) (any, error) {
	var v any
	switch sp.kind {
	case kindString:
		s, ok := raw.(string)
		if !ok {
			return nil, fieldErr(path, sp.hint, "must be a string, not %s", describe(raw))
		}
		v = s
	case kindBool:
		b, ok := raw.(bool)
		if !ok {
			return nil, fieldErr(path, sp.hint, "must be true or false, not %s", describe(raw))
		}
		v = b
	case kindInt:
		n, ok := raw.(json.Number)
		if !ok || !isIntSyntax(string(n)) {
			return nil, fieldErr(path, sp.hint, "must be an integer, not %s", describe(raw))
		}
		i, err := strconv.ParseInt(string(n), 10, 64)
		if err != nil {
			return nil, fieldErr(path, sp.hint, "%s is out of range", n)
		}
		v = i
	case kindList:
		items, ok := raw.([]any)
		if !ok {
			return nil, fieldErr(path, sp.hint, "must be an array of strings, not %s", describe(raw))
		}
		list := make([]string, len(items))
		for i, item := range items {
			s, ok := item.(string)
			if !ok {
				return nil, fieldErr(fmt.Sprintf("%s[%d]", path, i), sp.hint, "must be a string, not %s", describe(item))
			}
			list[i] = s
		}
		v = list
	}
	return sp.validate(v, path)
}

// isIntSyntax accepts JSON integer syntax only: no sign other than a
// leading minus, no leading zeros, fraction or exponent.
func isIntSyntax(s string) bool {
	if len(s) > 0 && s[0] == '-' {
		s = s[1:]
	}
	if s == "" || (s[0] == '0' && len(s) > 1) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func describe(v any) string {
	switch v := v.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case json.Number:
		if isIntSyntax(string(v)) {
			return "an integer"
		}
		return "the number " + string(v)
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", v)
}

// decodeValue decodes one JSON value, rejecting duplicate keys and null.
func decodeValue(data []byte, path string) (any, error) {
	v, problem, err := decode(data, path)
	if err == nil {
		err = problem
	}
	return v, err
}

// decode decodes exactly one JSON value with json.Number numbers. err is a
// syntax error. problem is the first duplicate key or null, which the caller
// reports after checking schema_version.
func decode(data []byte, path string) (v any, problem, err error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	w := &walker{dec: dec}
	v, err = w.value(path)
	if err == nil {
		if _, terr := dec.Token(); terr != io.EOF {
			err = errors.New("unexpected data after the top-level value")
		}
	}
	if err != nil {
		return nil, nil, fmt.Errorf("config: invalid JSON near byte %d: %v (fix the syntax)", dec.InputOffset(), err)
	}
	return v, w.problem, nil
}

type walker struct {
	dec     *json.Decoder
	problem error
}

func (w *walker) note(err error) {
	if w.problem == nil {
		w.problem = err
	}
}

func (w *walker) value(path string) (any, error) {
	tok, err := w.dec.Token()
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	switch tok := tok.(type) {
	case json.Delim:
		if tok == '[' {
			list := []any{}
			for i := 0; w.dec.More(); i++ {
				v, err := w.value(fmt.Sprintf("%s[%d]", path, i))
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			}
			_, err := w.dec.Token()
			return list, err
		}
		obj := map[string]any{}
		for w.dec.More() {
			kt, err := w.dec.Token()
			if err != nil {
				return nil, err
			}
			k := kt.(string)
			p := k
			if path != "" {
				p = path + "." + k
			}
			if _, dup := obj[k]; dup {
				w.note(fieldErr(p, "keep one of them", "duplicate key"))
			}
			v, err := w.value(p)
			if err != nil {
				return nil, err
			}
			obj[k] = v
		}
		_, err := w.dec.Token()
		return obj, err
	case nil:
		where := path
		if where == "" {
			where = "(top level)"
		}
		w.note(fieldErr(where, "remove the key to use its default", "null is not allowed"))
		return nil, nil
	}
	return tok, nil // string, bool or json.Number
}
