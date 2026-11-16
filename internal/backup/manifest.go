// Package backup copies a Flats host into a directory that can be checked and
// restored, and checks such a copy. docs/spec/host-backup.md is the contract.
package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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

func (m *Manifest) sort() {
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	sort.Slice(m.Databases, func(i, j int) bool { return m.Databases[i].Path < m.Databases[j].Path })
	sort.Strings(m.Excluded)
	sort.Slice(m.Skipped, func(i, j int) bool { return m.Skipped[i].Path < m.Skipped[j].Path })
	sort.Strings(m.Unstable)
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
