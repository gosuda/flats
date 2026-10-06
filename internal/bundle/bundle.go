// Package bundle validates uploaded builds and writes them as immutable
// version directories.
//
// An upload is a tar, tar.gz or zip archive (or a list of files) holding the
// build output. An optional flats.json manifest at the root describes it:
//
//	{
//	  "name": "My blog",           // display name (optional)
//	  "kind": "static",            // "static" (default) or "server"
//	  "entry": "index.html",       // static: index document; server: handler script (.js) or module (.wasm)
//	  "spa": false,                // static: serve entry for unknown paths
//	  "not_found": "404.html",     // static: page served with status 404
//	  "health": "/",               // path checked before a deploy goes live
//	  "screenshot": "screenshot.png" // thumbnail shown in the console
//	}
package bundle

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/flats/internal/contenttype"
)

// ManifestName is the optional manifest at the bundle root.
const ManifestName = "flats.json"

// MaxFiles bounds the number of files in one version.
const MaxFiles = 20000

// Manifest describes a build.
type Manifest struct {
	Type       string `json:"type,omitempty"`
	Name       string `json:"name,omitempty"`
	Kind       string `json:"kind"`
	Entry      string `json:"entry"`
	SPA        bool   `json:"spa,omitempty"`
	NotFound   string `json:"not_found,omitempty"`
	Health     string `json:"health"`
	Screenshot string `json:"screenshot,omitempty"`
}

// Problem is one validation failure with a hint on how to fix it.
type Problem struct {
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
	Fix     string `json:"fix"`
}

// ValidationError lists every problem found in an upload.
type ValidationError struct {
	Problems []Problem `json:"problems"`
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	b.WriteString("upload rejected:")
	for _, p := range e.Problems {
		b.WriteString("\n- ")
		if p.Path != "" {
			b.WriteString(p.Path + ": ")
		}
		b.WriteString(p.Message)
		if p.Fix != "" {
			b.WriteString(" (fix: " + p.Fix + ")")
		}
	}
	return b.String()
}

func (e *ValidationError) add(pathname, msg, fix string) {
	e.Problems = append(e.Problems, Problem{Path: pathname, Message: msg, Fix: fix})
}

// File is one in-memory file of an upload.
type File struct {
	Path string
	Data []byte
	Mode fs.FileMode
}

// Result describes a written version.
type Result struct {
	Manifest Manifest
	Hash     string
	Size     int64
	Files    int
}

// Limits bound an upload.
type Limits struct {
	MaxBytes int64 // total uncompressed size
}

// collector accumulates files with limit checks.
type collector struct {
	lim   Limits
	files map[string]File
	total int64
	verr  ValidationError
}

func newCollector(lim Limits) *collector {
	return &collector{lim: lim, files: map[string]File{}}
}

func cleanPath(p string) (string, bool) {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "./")
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsRune(p, '\x00') {
		return "", false
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") || strings.Contains(c, "/../") {
		return "", false
	}
	for _, seg := range strings.Split(c, "/") {
		if seg == ".." || seg == "" {
			return "", false
		}
	}
	return c, true
}

func (c *collector) addFile(name string, r io.Reader, mode fs.FileMode) error {
	p, ok := cleanPath(name)
	if !ok {
		c.verr.add(name, "path escapes the bundle root or is absolute", "use relative paths inside the build directory")
		return nil
	}
	if skipName(p) {
		return nil
	}
	if _, dup := c.files[p]; dup {
		c.verr.add(p, "file appears twice in the archive", "remove the duplicate entry")
		return nil
	}
	if len(c.files) >= MaxFiles {
		return fmt.Errorf("upload has more than %d files", MaxFiles)
	}
	remaining := c.lim.MaxBytes - c.total
	data, err := io.ReadAll(io.LimitReader(r, remaining+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > remaining {
		return &ValidationError{Problems: []Problem{{Message: fmt.Sprintf("upload is larger than the %d-byte limit", c.lim.MaxBytes), Fix: "remove source maps, large media or node_modules from the build output, or raise the upload limit in system settings"}}}
	}
	c.total += int64(len(data))
	c.files[p] = File{Path: p, Data: data, Mode: mode.Perm() &^ 0o022}
	return nil
}

// skipName drops OS metadata files that are never part of a site.
func skipName(p string) bool {
	base := path.Base(p)
	if base == ".DS_Store" || strings.HasPrefix(base, "._") {
		return true
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".git" || seg == "node_modules" || seg == "__MACOSX" {
			return true
		}
	}
	return false
}

// FromArchive reads a tar, tar.gz or zip archive.
func FromArchive(r io.Reader, lim Limits) ([]File, error) {
	// Bound the compressed body too: a well-formed archive of MaxBytes is never
	// much larger than MaxBytes plus headers.
	raw, err := io.ReadAll(io.LimitReader(r, lim.MaxBytes+lim.MaxBytes/4+1<<20))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > lim.MaxBytes+lim.MaxBytes/4+1<<20 {
		return nil, &ValidationError{Problems: []Problem{{Message: "archive is larger than the upload limit", Fix: "shrink the build output or raise the upload limit in system settings"}}}
	}
	c := newCollector(lim)
	switch {
	case bytes.HasPrefix(raw, []byte("PK\x03\x04")) || bytes.HasPrefix(raw, []byte("PK\x05\x06")):
		err = c.readZip(raw)
	case bytes.HasPrefix(raw, []byte{0x1f, 0x8b}):
		gz, gerr := gzip.NewReader(bytes.NewReader(raw))
		if gerr != nil {
			return nil, &ValidationError{Problems: []Problem{{Message: "invalid gzip data: " + gerr.Error(), Fix: "send a valid .tar.gz, .tar or .zip archive"}}}
		}
		err = c.readTar(gz)
	default:
		err = c.readTar(bytes.NewReader(raw))
	}
	if err != nil {
		return nil, err
	}
	return c.result()
}

func (c *collector) readTar(r io.Reader) error {
	tr := tar.NewReader(r)
	seen := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if !seen {
				return &ValidationError{Problems: []Problem{{Message: "not a tar, tar.gz or zip archive (" + err.Error() + ")", Fix: "pack the build directory with `tar czf site.tgz -C dist .` or use `flats deploy <dir>`"}}}
			}
			return &ValidationError{Problems: []Problem{{Message: "corrupt tar archive: " + err.Error(), Fix: "re-create the archive"}}}
		}
		seen = true
		switch h.Typeflag {
		case tar.TypeDir, tar.TypeXGlobalHeader, tar.TypeXHeader:
			continue
		case tar.TypeReg, tar.TypeRegA:
			if err := c.addFile(h.Name, tr, fs.FileMode(h.Mode)); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			c.verr.add(h.Name, "links are not allowed", "copy the target file into the build instead of linking it")
		default:
			c.verr.add(h.Name, "special files are not allowed", "include only regular files and directories")
		}
	}
	if !seen {
		return &ValidationError{Problems: []Problem{{Message: "archive is empty", Fix: "build the site first and pack its output directory"}}}
	}
	return nil
}

func (c *collector) readZip(raw []byte) error {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return &ValidationError{Problems: []Problem{{Message: "invalid zip archive: " + err.Error(), Fix: "re-create the archive"}}}
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if f.Mode()&fs.ModeSymlink != 0 || !f.Mode().IsRegular() {
			c.verr.add(f.Name, "links and special files are not allowed", "include only regular files")
			continue
		}
		rc, err := f.Open()
		if err != nil {
			c.verr.add(f.Name, "cannot read entry: "+err.Error(), "re-create the archive")
			continue
		}
		err = c.addFile(f.Name, rc, f.Mode())
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// FromDir reads a local directory (used by the CLI and loopback callers).
func FromDir(dir string, lim Limits) ([]File, error) {
	c := newCollector(lim)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && (d.Name() == ".git" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			c.verr.add(rel, "links and special files are not allowed", "copy the target file into the build instead of linking it")
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		return c.addFile(rel, bufio.NewReader(f), info.Mode())
	})
	if err != nil {
		return nil, err
	}
	return c.result()
}

// FromFiles validates an in-memory file list (used by MCP).
func FromFiles(in []File, lim Limits) ([]File, error) {
	c := newCollector(lim)
	for _, f := range in {
		if err := c.addFile(f.Path, bytes.NewReader(f.Data), 0o644); err != nil {
			return nil, err
		}
	}
	return c.result()
}

func (c *collector) result() ([]File, error) {
	if len(c.verr.Problems) > 0 {
		v := c.verr
		return nil, &v
	}
	files := make([]File, 0, len(c.files))
	for _, f := range c.files {
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return stripCommonRoot(files), nil
}

// stripCommonRoot removes a single top-level directory wrapping every file
// (e.g. an archive of "dist/") when there is no manifest or index at the root.
func stripCommonRoot(files []File) []File {
	if len(files) == 0 {
		return files
	}
	first := strings.SplitN(files[0].Path, "/", 2)
	if len(first) < 2 {
		return files
	}
	prefix := first[0] + "/"
	for _, f := range files {
		if !strings.HasPrefix(f.Path, prefix) {
			return files
		}
	}
	out := make([]File, len(files))
	for i, f := range files {
		f.Path = strings.TrimPrefix(f.Path, prefix)
		out[i] = f
	}
	return out
}

// ParseManifest reads and validates flats.json (or derives defaults). It
// reports every problem it finds, not just the first one.
func ParseManifest(files []File) (Manifest, error) {
	index := map[string]bool{}
	for _, f := range files {
		index[f.Path] = true
	}
	var m Manifest
	fields := map[string]json.RawMessage{}
	var verr ValidationError
	// bad holds the fields (or "*" for the whole manifest) that could not be
	// decoded, so their dependent checks don't report follow-on noise.
	bad := map[string]bool{}
	for _, f := range files {
		if f.Path == ManifestName {
			decodeManifest(f.Data, &m, &verr, bad)
			_ = json.Unmarshal(f.Data, &fields)
		}
	}
	_, hasKind := fields["kind"]
	if _, explicit := fields["type"]; !explicit && !hasKind && m.Kind == "" && !bad["*"] && !bad["kind"] && !index["index.html"] {
		hasServer := false
		for _, candidate := range serverEntries {
			hasServer = hasServer || index[candidate]
		}
		// A custom static entry that exists was already a valid upload.
		entryPath, entryOK := cleanPath(m.Entry)
		customStatic := m.Entry != "" && entryOK && index[entryPath]
		if !hasServer && !customStatic && markdownEntry(files) != "" {
			m.Type = contenttype.Docs
		}
	}
	if m.Type == "" {
		if _, explicit := fields["type"]; explicit && !bad["type"] {
			verr.add(ManifestName, "type must be flat or docs", `use "flat" or "docs", or omit type`)
		}
		m.Type = contenttype.Flat
	}
	if m.Type != contenttype.Flat && m.Type != contenttype.Docs {
		verr.add(ManifestName, fmt.Sprintf("unknown type %q", m.Type), `use "flat" or "docs"`)
	}
	if m.Type == contenttype.Docs {
		return parseDocs(files, index, fields, m, &verr)
	}
	if (bad["*"] || bad["kind"]) && m.Kind == "" && !index["index.html"] {
		// Guess the kind so the entry checks below stay useful.
		for _, cand := range serverEntries {
			if index[cand] {
				m.Kind = "server"
				break
			}
		}
	}
	if m.Kind == "" {
		m.Kind = "static"
	}
	switch m.Kind {
	case "static":
		if m.Entry == "" {
			m.Entry = "index.html"
		}
	case "server":
		if m.Entry == "" {
			for _, cand := range serverEntries {
				if index[cand] {
					m.Entry = cand
					break
				}
			}
		}
		if m.Entry == "" {
			if !bad["entry"] {
				verr.add(ManifestName, "server flat has no entry", `set "entry" to the handler script (e.g. "server.js") or Wasm module`)
			}
		} else if !strings.HasSuffix(m.Entry, ".js") && !strings.HasSuffix(m.Entry, ".mjs") && !strings.HasSuffix(m.Entry, ".wasm") {
			verr.add(m.Entry, "server entry must be a .js, .mjs or .wasm file", "point entry at the JavaScript handler or the compiled Wasm module")
		}
	default:
		verr.add(ManifestName, fmt.Sprintf("unknown kind %q", m.Kind), `use "static" or "server"`)
	}
	if m.Health == "" {
		m.Health = "/"
	}
	if msg, fix := checkHealth(m.Health); msg != "" {
		verr.add(ManifestName, msg, fix)
	}
	check := func(field, p string) {
		if p == "" {
			return
		}
		cp, ok := cleanPath(p)
		if !ok {
			verr.add(ManifestName, field+" is not a relative path", "use a path inside the bundle")
			return
		}
		if !index[cp] {
			verr.add(cp, field+" file is missing", fmt.Sprintf("add %s to the build or change %q in %s", cp, field, ManifestName))
		}
	}
	if m.Kind == "static" && m.Entry == "index.html" && !index["index.html"] {
		if !bad["entry"] {
			verr.add("index.html", "static flat has no index.html at the root",
				`make sure the archive root is the build output directory (it must contain index.html), or set "entry" in flats.json`)
		}
	} else if m.Entry != "" {
		check("entry", m.Entry)
	}
	check("not_found", m.NotFound)
	check("screenshot", m.Screenshot)
	if len(verr.Problems) > 0 {
		return m, &verr
	}
	return m, nil
}

// serverEntries are the default server entries, in order of preference.
var serverEntries = []string{"server.js", "index.js", "main.wasm", "server.wasm"}

// manifestFields lists the manifest fields in documentation order.
var manifestFields = []string{"type", "name", "kind", "entry", "spa", "not_found", "health", "screenshot"}

const manifestExample = `{"kind":"static","entry":"index.html"}`

// decodeManifest decodes flats.json field by field, so that one bad field
// does not hide the others. Every problem gets a fix specific to its cause.
func decodeManifest(data []byte, m *Manifest, verr *ValidationError, bad map[string]bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		bad["*"] = true
		var se *json.SyntaxError
		var te *json.UnmarshalTypeError
		switch {
		case errors.As(err, &se):
			line, col := lineCol(data, se.Offset)
			verr.add(ManifestName, fmt.Sprintf("invalid JSON at byte %d (line %d, column %d): %v", se.Offset, line, col, se),
				"fix the JSON syntax at that position (check for missing quotes, commas or braces and trailing commas), e.g. "+manifestExample)
		case errors.As(err, &te):
			verr.add(ManifestName, "manifest must be a JSON object, not a JSON "+te.Value, "wrap the settings in braces, e.g. "+manifestExample)
		default:
			verr.add(ManifestName, "invalid manifest: "+err.Error(), "write the manifest as a JSON object, e.g. "+manifestExample)
		}
		return
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var dst any
		switch k {
		case "type":
			dst = &m.Type
		case "name":
			dst = &m.Name
		case "kind":
			dst = &m.Kind
		case "entry":
			dst = &m.Entry
		case "spa":
			dst = &m.SPA
		case "not_found":
			dst = &m.NotFound
		case "health":
			dst = &m.Health
		case "screenshot":
			dst = &m.Screenshot
		default:
			fix := "remove it; the allowed fields are " + strings.Join(manifestFields, ", ")
			for _, f := range manifestFields {
				if strings.EqualFold(strings.ReplaceAll(k, "-", "_"), f) {
					fix = fmt.Sprintf("rename it to %q (field names are lowercase); the allowed fields are %s", f, strings.Join(manifestFields, ", "))
				}
			}
			verr.add(ManifestName, fmt.Sprintf("unknown field %q", k), fix)
			continue
		}
		if err := json.Unmarshal(raw[k], dst); err != nil {
			bad[k] = true
			if k == "spa" {
				verr.add(ManifestName, fmt.Sprintf("field \"spa\" must be a boolean, not %s", jsonKind(raw[k])), `use true or false without quotes, e.g. "spa": true`)
			} else {
				verr.add(ManifestName, fmt.Sprintf("field %q must be a string, not %s", k, jsonKind(raw[k])), fmt.Sprintf("put the value in double quotes, e.g. %q: %q", k, fieldExample(k)))
			}
		}
	}
}

func fieldExample(field string) string {
	switch field {
	case "type":
		return "flat"
	case "name":
		return "My blog"
	case "kind":
		return "static"
	case "entry":
		return "index.html"
	case "not_found":
		return "404.html"
	case "health":
		return "/"
	case "screenshot":
		return "screenshot.png"
	}
	return ""
}

// jsonKind names the JSON type of a raw value for error messages.
func jsonKind(v json.RawMessage) string {
	t := bytes.TrimSpace(v)
	if len(t) == 0 {
		return "an empty value"
	}
	switch t[0] {
	case '"':
		return "a string"
	case '{':
		return "an object"
	case '[':
		return "an array"
	case 't', 'f':
		return "a boolean"
	case 'n':
		return "null"
	}
	return "a number"
}

// lineCol converts a byte offset into 1-based line and column numbers.
func lineCol(data []byte, off int64) (line, col int) {
	// json.SyntaxError.Offset counts the offending byte too.
	if off > 0 {
		off--
	}
	if off > int64(len(data)) {
		off = int64(len(data))
	}
	before := data[:off]
	line = bytes.Count(before, []byte("\n")) + 1
	col = int(off) - bytes.LastIndexByte(before, '\n')
	return line, col
}

// checkHealth validates the health-check path. It must be an origin-form
// request target ("/path?query"): no scheme, host, fragment, spaces or
// control characters, so the deploy can always build the request.
func checkHealth(p string) (msg, fix string) {
	const fixPath = `use an absolute URL path such as "/" or "/healthz"`
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return fmt.Sprintf("health %q must be a URL path starting with a single /, without a scheme or host", p), fixPath
	}
	for _, r := range p {
		if r <= ' ' || r >= 0x7f {
			return fmt.Sprintf("health %q contains a space, control or non-ASCII character", p), fixPath + `; percent-encode other characters (a space is "%20")`
		}
	}
	if strings.Contains(p, "#") {
		return fmt.Sprintf("health %q contains a fragment (#)", p), fixPath + `; remove the "#..." part`
	}
	u, err := url.ParseRequestURI(p)
	if err != nil {
		return fmt.Sprintf("health %q is not a valid URL path: %v", p, errors.Unwrap(err)), fixPath + `; write a literal % as "%25"`
	}
	if u.Scheme != "" || u.Host != "" {
		return fmt.Sprintf("health %q must not include a scheme or host", p), fixPath
	}
	return "", ""
}

// Hash returns the content hash of a file list.
func Hash(files []File) string {
	h := sha256.New()
	for _, f := range files {
		sum := sha256.Sum256(f.Data)
		fmt.Fprintf(h, "%s\x00%x\n", f.Path, sum)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Write validates files, then writes them into dst atomically (dst must not
// exist). It returns the manifest and content hash.
func Write(files []File, dst string) (Result, error) {
	files, err := canonicalWriteFiles(files)
	if err != nil {
		return Result{}, err
	}
	m, err := ParseManifest(files)
	if err != nil {
		return Result{}, err
	}
	if _, err := os.Stat(dst); err == nil {
		return Result{}, fmt.Errorf("version directory %s already exists", dst)
	}
	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return Result{}, err
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(dst)+".tmp-")
	if err != nil {
		return Result{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmp)
		}
	}()
	root, err := os.OpenRoot(tmp)
	if err != nil {
		return Result{}, err
	}
	if err := writeBundleFiles(root, files); err != nil {
		_ = root.Close()
		return Result{}, err
	}
	if err := root.Close(); err != nil {
		return Result{}, err
	}
	var size int64
	for _, f := range files {
		size += int64(len(f.Data))
	}
	if err := os.Rename(tmp, dst); err != nil {
		return Result{}, err
	}
	cleanup = false
	return Result{Manifest: m, Hash: Hash(files), Size: size, Files: len(files)}, nil
}

func canonicalWriteFiles(files []File) ([]File, error) {
	out := make([]File, len(files))
	seen := make(map[string]struct{}, len(files))
	var verr ValidationError
	for i, f := range files {
		p, ok := cleanPath(f.Path)
		rel := filepath.FromSlash(p)
		if !ok || p != f.Path || !filepath.IsLocal(rel) {
			verr.add(f.Path, "path escapes the bundle root or is absolute", "use relative paths inside the build directory")
			continue
		}
		if _, ok := seen[p]; ok {
			verr.add(p, "file appears twice in the bundle", "remove the duplicate entry")
			continue
		}
		seen[p] = struct{}{}
		f.Path = p
		out[i] = f
	}
	if len(verr.Problems) > 0 {
		return nil, &verr
	}
	return out, nil
}

// writeBundleFiles writes only through an os.Root opened on the fresh staging
// directory. Even if a caller supplies a hostile path or a local race replaces
// a directory with a symlink, the kernel-backed root cannot escape tmp.
func writeBundleFiles(root *os.Root, files []File) error {
	for _, f := range files {
		name := filepath.FromSlash(f.Path)
		if dir := filepath.Dir(name); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		if err := root.WriteFile(name, f.Data, 0o444); err != nil {
			return err
		}
	}
	return nil
}

// IsValidation reports whether err is a ValidationError.
func IsValidation(err error) (*ValidationError, bool) {
	var v *ValidationError
	ok := errors.As(err, &v)
	return v, ok
}

// IsMarkdown reports which uploaded files are documents.
func IsMarkdown(p string) bool {
	return strings.HasSuffix(p, ".md") || strings.HasSuffix(p, ".markdown")
}

func markdownEntry(files []File) string {
	for _, candidate := range []string{"index.md", "README.md"} {
		for _, f := range files {
			if f.Path == candidate {
				return candidate
			}
		}
	}
	entry := ""
	for _, f := range files {
		if IsMarkdown(f.Path) {
			if entry != "" {
				return ""
			}
			entry = f.Path
		}
	}
	return entry
}

func parseDocs(files []File, index map[string]bool, fields map[string]json.RawMessage, m Manifest, verr *ValidationError) (Manifest, error) {
	for _, field := range []string{"kind", "spa", "not_found"} {
		if _, ok := fields[field]; ok {
			verr.add(ManifestName, field+" is not allowed for docs", "remove "+field+"; the host selects the docs runtime")
		}
	}
	m.Kind = "server"
	if m.Entry == "" {
		m.Entry = markdownEntry(files)
	}
	if m.Entry == "" || !IsMarkdown(m.Entry) || !index[m.Entry] {
		verr.add(ManifestName, "docs entry must name an existing Markdown file", `add index.md or README.md, or set "entry" to a .md or .markdown file`)
	}
	if m.Health == "" {
		m.Health = "/_docs/healthz"
	}
	if m.Health != "/_docs/healthz" {
		verr.add(ManifestName, "docs health must be /_docs/healthz", `remove health or use "/_docs/healthz"`)
	}
	total, documents := 0, 0
	for _, f := range files {
		if f.Path == "_docs" || strings.HasPrefix(f.Path, "_docs/") {
			verr.add(f.Path, "path is reserved for the docs app", "move the file outside _docs/")
		}
		if !IsMarkdown(f.Path) {
			continue
		}
		documents++
		total += len(f.Data)
		if !utf8.Valid(f.Data) {
			verr.add(f.Path, "Markdown must be valid UTF-8", "encode the document as UTF-8")
		}
		if len(f.Data) > 1<<20 {
			verr.add(f.Path, "Markdown exceeds 1 MiB", "split or shorten the document to at most 1 MiB")
		}
	}
	if documents > 128 {
		verr.add(ManifestName, "docs bundle exceeds 128 documents", "reduce the bundle to at most 128 Markdown files")
	}
	if total > 4<<20 {
		verr.add(ManifestName, "Markdown total exceeds 4 MiB", "reduce the documents to at most 4 MiB combined")
	}
	if m.Screenshot != "" {
		cp, ok := cleanPath(m.Screenshot)
		if !ok {
			verr.add(ManifestName, "screenshot is not a relative path", "use a screenshot path inside the bundle")
		} else if !index[cp] {
			verr.add(cp, "screenshot file is missing", "add the screenshot or remove screenshot from flats.json")
		}
	}
	if len(verr.Problems) > 0 {
		return m, verr
	}
	return m, nil
}
