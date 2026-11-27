// Package backup copies a Flats host into a directory that can be checked and
// restored, and checks such a copy. docs/spec/host-backup.md is the contract.
package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// FormatVersion is the manifest format this binary writes and reads.
const FormatVersion = 1

// ManifestFile is the name of the manifest at the top of a copy.
const ManifestFile = "manifest.json"

// Values of Manifest.SecretKey.
const (
	SecretKeyIncluded = "included"
	SecretKeyExcluded = "excluded"
	SecretKeyAbsent   = "absent"
	// SecretKeyExternal: host.secret_key_path keeps the key outside the data directory, so the copy never holds it.
	SecretKeyExternal = "external"
)

// Values of Manifest.Config.
const (
	ConfigIncluded = "included" // PATH/config.json, the file lay outside the data directory
	ConfigInData   = "in_data"  // data/config.json, the file lay inside the data directory
	ConfigAbsent   = "absent"
)

// Kinds of a database in Manifest.Databases.
const (
	KindFlats    = "flats"
	KindData     = "data"
	KindPreview  = "preview"
	KindBoard    = "board"
	KindNotebook = "notebook"
	KindSheet    = "sheet"
)

// Manifest describes one copy (rule 4, 26 and 36 of the spec).
type Manifest struct {
	Format       int        `json:"format"`
	FlatsVersion string     `json:"flats_version"`
	Created      string     `json:"created"`
	SecretKey    string     `json:"secret_key"`
	Config       string     `json:"config"`
	Attempts     int        `json:"attempts"`
	Files        []File     `json:"files"`
	Databases    []Database `json:"databases"`
	Excluded     []string   `json:"excluded"`
	Skipped      []Skip     `json:"skipped"`
	Unstable     []string   `json:"unstable"`
	// Refused lists the sheet.db files of databases that the sheet check
	// refuses, with its reason. The copy holds their bytes as they are and
	// no check reads their rows (rule 4).
	Refused []Skip `json:"refused,omitempty"`
}

// File is one file of a copy, by its path below the copy's folder.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Database names a SQLite database of a copy and its kind.
type Database struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// Skip names an entry that the copy did not take and why.
type Skip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// checkManifestPath returns why p is not a path that a copy lists, or nil.
// A copy lists relative, clean, slash-separated paths of valid UTF-8 with no
// backslash, no volume and no "." or ".." element, below data/ or for the
// configuration file outside the data directory and its history. The check
// is the same on every system: Windows reads a backslash as a separator, so
// a path one system reads as another file than its spelling says is refused
// everywhere (rule 26).
func checkManifestPath(p string) error {
	switch {
	case p == "":
		return errors.New("is empty")
	case !utf8.ValidString(p):
		return errors.New("is not valid UTF-8")
	case strings.Contains(p, `\`):
		return errors.New("holds a backslash")
	case strings.HasPrefix(p, "/"):
		return errors.New("starts with a slash")
	case len(p) >= 2 && p[1] == ':' && (p[0]|0x20 >= 'a' && p[0]|0x20 <= 'z'):
		return errors.New("starts with a volume")
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "..":
			return errors.New(`has a ".." element`)
		case ".":
			return errors.New("is not clean")
		}
	}
	if path.Clean(p) != p {
		return errors.New("is not clean")
	}
	top, rest, _ := strings.Cut(p, "/")
	switch {
	case top == "data" && rest != "":
	case p == nameConfig:
	case top == nameConfigHist && rest != "" && !strings.Contains(rest, "/"):
	default:
		return errors.New("lies outside the layout of a copy")
	}
	return nil
}

// checkDatabasePath returns why the manifest's database d is not one that a
// copy lists, or nil: its path is canonical and is the place of a database of
// its kind in the layout, exactly as written.
func checkDatabasePath(d Database) error {
	if err := checkManifestPath(d.Path); err != nil {
		return fmt.Errorf("is not a path a copy holds: it %w", err)
	}
	rel, ok := strings.CutPrefix(d.Path, "data/")
	c := classify(rel)
	switch {
	case !ok || c.class != classDatabase:
		return fmt.Errorf("is listed as a %q database, and the layout of a copy holds no database at this path", d.Kind)
	case c.kind != d.Kind:
		return fmt.Errorf("is listed as a %q database, and the layout of a copy makes it a %s database", d.Kind, c.kind)
	}
	return nil
}

func (m *Manifest) sort() {
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	sort.Slice(m.Databases, func(i, j int) bool { return m.Databases[i].Path < m.Databases[j].Path })
	sort.Strings(m.Excluded)
	sort.Slice(m.Skipped, func(i, j int) bool { return m.Skipped[i].Path < m.Skipped[j].Path })
	sort.Strings(m.Unstable)
	sort.Slice(m.Refused, func(i, j int) bool { return m.Refused[i].Path < m.Refused[j].Path })
}

// write puts the manifest at root/manifest.json through a new file.
func (m *Manifest) write(root string) error {
	m.sort()
	if m.Files == nil {
		m.Files = []File{}
	}
	if m.Databases == nil {
		m.Databases = []Database{}
	}
	if m.Excluded == nil {
		m.Excluded = []string{}
	}
	if m.Skipped == nil {
		m.Skipped = []Skip{}
	}
	if m.Unstable == nil {
		m.Unstable = []string{}
	}
	f, err := os.OpenFile(filepath.Join(root, ManifestFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// errNoManifest reports a folder without manifest.json.
var errNoManifest = errors.New("manifest.json is missing")

// readManifest reads root/manifest.json. A format that this version does not
// know is a *RefusedError.
func readManifest(root string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(root, ManifestFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNoManifest
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", ManifestFile, err)
	}
	if m.Format != FormatVersion {
		return nil, &RefusedError{fmt.Sprintf("%s has format %d, which this Flats does not know (it reads format %d); check the copy with the Flats release that made it", ManifestFile, m.Format, FormatVersion)}
	}
	return &m, nil
}

// RefusedError is a refusal: the command writes nothing and exits with status
// 78 (rule 30 of the spec).
type RefusedError struct{ Msg string }

func (e *RefusedError) Error() string { return e.Msg }

// RerunError is a failure that a second run can clear. Its message ends with
// the advice to run the backup again.
type RerunError struct{ Err error }

func (e *RerunError) Error() string { return e.Err.Error() + "; run the backup again" }
func (e *RerunError) Unwrap() error { return e.Err }
