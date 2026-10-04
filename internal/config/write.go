package config

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	historyDir  = "config-history"
	historyKeep = 10
	stampLayout = "20060102T150405.000000000Z"
)

// historyName matches config.v<schema>.<UTC stamp>[-n].json.
var historyName = regexp.MustCompile(`^config\.v[0-9]+\.([0-9]{8}T[0-9]{6}\.[0-9]{9}Z)(?:-([0-9]+))?\.json$`)

// ErrConflict is matched by a *ConflictError.
var ErrConflict = errors.New("config: file changed since it was read")

// ConflictError rejects a save whose expected hash no longer matches the
// file on disk.
type ConflictError struct {
	Expected, Actual string
}

func (e *ConflictError) Error() string {
	return "config: file changed since it was read; reload it and retry"
}

func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

// Encode returns the canonical file bytes: keys in schema order, 2-space
// indentation, inline arrays and a trailing newline.
func (d *Document) Encode() []byte {
	var b bytes.Buffer
	b.WriteString("{\n  \"schema_version\": ")
	b.WriteString(strconv.Itoa(d.version))
	for _, sec := range sections {
		first := true
		for _, sp := range specs {
			name, ok := strings.CutPrefix(sp.key, sec+".")
			v, set := d.values[sp.key]
			if !ok || !set {
				continue
			}
			if first {
				b.WriteString(",\n  " + quote(sec) + ": {\n    ")
				first = false
			} else {
				b.WriteString(",\n    ")
			}
			b.WriteString(quote(name) + ": " + encodeValue(v))
		}
		if !first {
			b.WriteString("\n  }")
		}
	}
	b.WriteString("\n}\n")
	return b.Bytes()
}

func encodeValue(v any) string {
	switch v := v.(type) {
	case string:
		return quote(v)
	case bool:
		return strconv.FormatBool(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case []string:
		parts := make([]string, len(v))
		for i, s := range v {
			parts[i] = quote(s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	panic(fmt.Sprintf("config: unexpected value %T", v))
}

func quote(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	return strings.TrimSuffix(b.String(), "\n")
}

// Create writes doc to path only if no file exists there, for config init.
// The file appears complete or not at all. It returns the new hash.
func Create(path string, doc *Document) (string, error) {
	if doc.version != CurrentSchemaVersion {
		return "", fmt.Errorf("config: cannot create schema_version %d", doc.version)
	}
	if err := doc.Validate(); err != nil {
		return "", err
	}
	data := doc.Encode()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := writeTemp(dir, data)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("config: %s already exists: %w", path, fs.ErrExist)
		}
		return "", err
	}
	if err := syncDir(dir); err != nil {
		return "", err
	}
	return Hash(data), nil
}

// Writer saves one config file. Its mutex serializes saves in this process;
// callers hold the data lock to exclude other processes.
type Writer struct {
	path string
	reg  registry
	mu   sync.Mutex
}

// NewWriter returns a writer for the config file at path.
func NewWriter(path string) *Writer {
	return &Writer{path: path, reg: migrations}
}

// Path is the config file path.
func (w *Writer) Path() string { return w.path }

// Save re-reads the file and rejects the save with a *ConflictError unless
// its hash is expectedHash. It then parses the file (migrating an older
// schema_version), applies mutate (which may be nil), validates the result,
// keeps the previous bytes in config-history/ and atomically replaces the
// file. It returns the new hash. If the result encodes to the current bytes,
// nothing is written. Older history copies beyond the newest 10 are removed
// on a best-effort basis. Callers confirm a migration with NeedsConfirm
// before saving that file.
func (w *Writer) Save(expectedHash string, mutate func(*Document) error) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	old, err := readFile(w.path)
	if err != nil {
		return "", err
	}
	if h := Hash(old); h != expectedHash {
		return "", &ConflictError{Expected: expectedHash, Actual: h}
	}
	l, err := w.reg.parse(old)
	if err != nil {
		return "", err
	}
	if mutate != nil {
		if err := mutate(l.Doc); err != nil {
			return "", err
		}
	}
	if err := l.Doc.Validate(); err != nil {
		return "", err
	}
	data := l.Doc.Encode()
	if bytes.Equal(data, old) {
		return l.Hash, nil
	}
	dir := filepath.Dir(w.path)
	if err := keepHistory(dir, old, l.FileVersion); err != nil {
		return "", fmt.Errorf("config: keep previous copy in %s: %w", historyDir, err)
	}
	tmp, err := writeTemp(dir, data)
	if err != nil {
		return "", err
	}
	if err := os.Rename(tmp, w.path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := syncDir(dir); err != nil {
		return "", err
	}
	_ = pruneHistory(filepath.Join(dir, historyDir), historyKeep)
	return Hash(data), nil
}

// writeTemp writes data to a synced 0600 temporary file in dir.
func writeTemp(dir string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, ".config.json.tmp*")
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o600)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// keepHistory copies the previous file bytes into config-history/.
func keepHistory(dir string, data []byte, version int) error {
	hdir := filepath.Join(dir, historyDir)
	if err := os.MkdirAll(hdir, 0o700); err != nil {
		return err
	}
	base := fmt.Sprintf("config.v%d.%s", version, time.Now().UTC().Format(stampLayout))
	for n := 1; n <= 100; n++ {
		name := base + ".json"
		if n > 1 {
			name = fmt.Sprintf("%s-%d.json", base, n)
		}
		path := filepath.Join(hdir, name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(path)
			return err
		}
		return syncDir(hdir)
	}
	return errors.New("too many copies with the same timestamp")
}

// pruneHistory removes all but the newest keep history copies. Other files
// in the directory are left alone.
func pruneHistory(hdir string, keep int) error {
	entries, err := os.ReadDir(hdir)
	if err != nil {
		return err
	}
	type entry struct {
		name, stamp string
		n           int
	}
	var copies []entry
	for _, e := range entries {
		m := historyName.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		n, _ := strconv.Atoi(m[2]) // "" (first copy) sorts as 0
		copies = append(copies, entry{e.Name(), m[1], n})
	}
	slices.SortFunc(copies, func(a, b entry) int {
		return cmp.Or(strings.Compare(b.stamp, a.stamp), cmp.Compare(b.n, a.n))
	})
	var errs []error
	for _, c := range copies[min(keep, len(copies)):] {
		errs = append(errs, os.Remove(filepath.Join(hdir, c.name)))
	}
	return errors.Join(errs...)
}
