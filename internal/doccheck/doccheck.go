// Package doccheck audits the repository's Markdown documentation: relative
// links and #anchors must resolve to files Git ships, with exact case, and
// docs/agent/ topics must not use relative links because MCP and llms.txt
// serve them outside the repository. It also reports, for review only,
// Markdown files nothing reaches and lines that hold hash literals.
//
// Run it with `make docs-check` (errors only) or `make docs-audit` (the full
// report). CI runs `make docs-check`; TestRepositoryDocs runs the same
// error check under go test.
package doccheck

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// skillDirs hold agent skills, whose Markdown is not repository docs.
var skillDirs = map[string]bool{".cursor": true, ".claude": true, ".agents": true}

// colocatedDirs hold package, command and example notes that sit next to
// their code instead of in the docs index.
var colocatedDirs = map[string]bool{"internal": true, "examples": true, "cmd": true}

var (
	schemeRE   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
	embedRE    = regexp.MustCompile(`(?m)^[ \t]*//go:embed[ \t]+(.+)$`)
	quotedMDRE = regexp.MustCompile("[\"'`]([A-Za-z0-9_./-]+\\.(?:md|markdown))[\"'`]")
	testFileRE = regexp.MustCompile(`(?i)(?:_test\.go$|(?:^|[._-])tests?\.[A-Za-z0-9]+$|(?:^|[._-])spec\.(?:mjs|js|ts|py|sh)$|^test_)`)
	codeSuffix = map[string]bool{".go": true, ".mjs": true, ".js": true, ".ts": true, ".sh": true, ".py": true, ".yml": true, ".yaml": true, ".json": true}
)

// Finding is one problem at a file and line (0 when the line is unknown).
type Finding struct {
	File    string
	Line    int
	Message string
}

func (f Finding) String() string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d: %s", f.File, f.Line, f.Message)
	}
	return fmt.Sprintf("%s: %s", f.File, f.Message)
}

// Report is the result of one audit. Errors fail the check; the other
// fields are review material.
type Report struct {
	Markdown  int
	Errors    []Finding
	Orphans   []string    // docs nothing links to, from README.md or code
	Colocated []Colocated // unlinked package, command and example notes
	Hashes    []Finding   // lines with 40/64-character hex literals
}

// Colocated is an unlinked Markdown file kept next to its code.
type Colocated struct {
	Path     string
	TestOnly bool // referenced only from tests
}

// GitFiles lists the files Git would ship from root: tracked files plus
// untracked files that are not ignored, as slash-separated relative paths.
func GitFiles(root string) ([]string, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f == "" {
			continue
		}
		// Deleted but still tracked files are listed by --cached.
		if st, err := os.Stat(filepath.Join(root, filepath.FromSlash(f))); err == nil && st.Mode().IsRegular() {
			files = append(files, f)
		}
	}
	sort.Strings(files)
	return files, nil
}

// RepoRoot returns the top level of the Git work tree containing dir.
func RepoRoot(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Check audits the Markdown files among files, which are slash-separated
// paths relative to root (see GitFiles).
func Check(root string, files []string) (*Report, error) {
	fileSet := map[string]bool{}
	lowerFiles := map[string]string{}
	dirs := map[string]bool{".": true}
	var mdFiles []string
	for _, f := range files {
		fileSet[f] = true
		lowerFiles[strings.ToLower(f)] = f
		for d := path.Dir(f); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
		if isMarkdown(f) && !skillDirs[topDir(f)] {
			mdFiles = append(mdFiles, f)
		}
	}
	for d := range dirs {
		lowerFiles[strings.ToLower(d)] = d
	}
	if len(mdFiles) == 0 {
		return nil, fmt.Errorf("no Markdown files under %s", root)
	}
	mdSet := map[string]bool{}
	for _, f := range mdFiles {
		mdSet[f] = true
	}

	docs := map[string]*document{}
	for _, f := range mdFiles {
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			return nil, err
		}
		docs[f] = parseDocument(src)
	}

	r := &Report{Markdown: len(mdFiles)}
	edges := map[string][]string{}
	for _, f := range mdFiles {
		d := docs[f]
		for _, l := range d.links {
			target, frag, ok := resolve(f, l.dest)
			if !ok {
				continue
			}
			// Only a fragment (#heading) or an empty destination (this
			// page) survives serving a topic outside the repository; any
			// file path, even this file's own name, breaks.
			if dest := strings.TrimSpace(l.dest); strings.HasPrefix(f, "docs/agent/") && dest != "" && !strings.HasPrefix(dest, "#") {
				r.Errors = append(r.Errors, Finding{f, l.line, fmt.Sprintf("relative link %q in docs/agent/ breaks when served through MCP or llms.txt", l.dest)})
			}
			if target == ".." || strings.HasPrefix(target, "../") {
				r.Errors = append(r.Errors, Finding{f, l.line, fmt.Sprintf("link %q points outside the repository", l.dest)})
				continue
			}
			if !fileSet[target] && !dirs[target] {
				r.Errors = append(r.Errors, Finding{f, l.line, fmt.Sprintf("broken link %q (%s)", l.dest, missingReason(root, target, lowerFiles))})
				continue
			}
			targetMD := ""
			if mdSet[target] {
				targetMD = target
			} else if dirs[target] && mdSet[path.Join(target, "README.md")] {
				targetMD = path.Join(target, "README.md")
			} else if readme := path.Join(path.Dir(target), "README.md"); mdSet[readme] {
				// A link to an example file leads readers to the directory
				// that holds it, where GitHub shows its README.
				edges[f] = append(edges[f], readme)
			}
			if targetMD != "" {
				edges[f] = append(edges[f], targetMD)
				if frag != "" && !docs[targetMD].anchors[frag] {
					r.Errors = append(r.Errors, Finding{f, l.line, fmt.Sprintf("broken anchor %q (#%s is not a heading or id in %s)", l.dest, frag, targetMD)})
				}
			}
		}
		for _, n := range d.hashLines {
			r.Hashes = append(r.Hashes, Finding{File: f, Line: n, Message: "40/64-character hex literal"})
		}
	}

	runtime, testOnly, err := codeReferences(root, files, mdSet)
	if err != nil {
		return nil, err
	}
	reached := map[string]bool{}
	queue := []string{}
	visit := func(f string) {
		if mdSet[f] && !reached[f] {
			reached[f] = true
			queue = append(queue, f)
		}
	}
	visit("README.md")
	for f := range runtime {
		visit(f)
	}
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		for _, t := range edges[f] {
			visit(t)
		}
	}
	for _, f := range mdFiles {
		switch {
		case reached[f]:
		case colocatedDirs[topDir(f)] || testOnly[f]:
			r.Colocated = append(r.Colocated, Colocated{Path: f, TestOnly: testOnly[f]})
		default:
			r.Orphans = append(r.Orphans, f)
		}
	}
	return r, nil
}

// resolve turns a link destination in file from into a repository path and
// fragment. ok is false for external links (scheme or protocol-relative).
// An empty destination is the current file, and a leading slash is the
// repository root, as GitHub renders them.
func resolve(from, dest string) (target, frag string, ok bool) {
	dest = strings.TrimSpace(dest)
	if strings.HasPrefix(dest, "//") || schemeRE.MatchString(dest) {
		return "", "", false
	}
	p, frag, _ := strings.Cut(dest, "#")
	p, _, _ = strings.Cut(p, "?")
	if u, err := url.PathUnescape(p); err == nil {
		p = u
	}
	if u, err := url.PathUnescape(frag); err == nil {
		frag = u
	}
	switch {
	case p == "":
		return from, frag, true
	case strings.HasPrefix(p, "/"):
		return path.Clean("." + p), frag, true
	default:
		return path.Clean(path.Join(path.Dir(from), p)), frag, true
	}
}

func missingReason(root, target string, lowerFiles map[string]string) string {
	if actual, ok := lowerFiles[strings.ToLower(target)]; ok {
		return fmt.Sprintf("case mismatch: the repository has %s, which breaks on Linux and GitHub", actual)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(target))); err == nil {
		return "the target exists but Git does not ship it (ignored or outside the listed files)"
	}
	return "file not found"
}

// codeReferences finds Markdown files that code reaches: //go:embed
// patterns and quoted .md paths in code and config files. Files only
// tests reach are returned separately.
func codeReferences(root string, files []string, mdSet map[string]bool) (runtime, testOnly map[string]bool, err error) {
	runtime, tests := map[string]bool{}, map[string]bool{}
	for _, f := range files {
		if !codeSuffix[strings.ToLower(path.Ext(f))] || skillDirs[topDir(f)] {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			return nil, nil, err
		}
		dst := runtime
		if isTestFile(f) {
			dst = tests
		}
		dir := path.Dir(f)
		if strings.HasSuffix(f, ".go") {
			for _, m := range embedRE.FindAllStringSubmatch(string(src), -1) {
				for _, pat := range embedPatterns(m[1]) {
					for md := range mdSet {
						if embedMatches(dir, pat, md) {
							dst[md] = true
						}
					}
				}
			}
		}
		for _, m := range quotedMDRE.FindAllStringSubmatch(string(src), -1) {
			for _, base := range []string{dir, "."} {
				if c := path.Clean(path.Join(base, m[1])); mdSet[c] {
					dst[c] = true
				}
			}
		}
	}
	testOnly = map[string]bool{}
	for f := range tests {
		if !runtime[f] {
			testOnly[f] = true
		}
	}
	return runtime, testOnly, nil
}

// embedPatterns splits a //go:embed argument list into patterns separated
// by spaces or tabs; a pattern may be a Go string literal in double quotes
// or backquotes.
func embedPatterns(args string) []string {
	var pats []string
	for args = strings.TrimLeft(args, " \t"); args != ""; args = strings.TrimLeft(args, " \t") {
		if q := args[0]; q == '"' || q == '`' {
			end := 1
			for end < len(args) && args[end] != q {
				if q == '"' && args[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(args) {
				return pats // unterminated: go build rejects it too
			}
			if p, err := strconv.Unquote(args[:end+1]); err == nil {
				pats = append(pats, p)
			}
			args = args[end+1:]
			continue
		}
		end := strings.IndexAny(args, " \t")
		if end < 0 {
			end = len(args)
		}
		pats = append(pats, args[:end])
		args = args[end:]
	}
	return pats
}

// embedMatches reports whether //go:embed pattern pat in package directory
// dir embeds file. A matched directory embeds its files recursively except
// names starting with . or _, unless the pattern has the all: prefix.
func embedMatches(dir, pat, file string) bool {
	all := strings.HasPrefix(pat, "all:")
	pat = strings.TrimPrefix(pat, "all:")
	rel, ok := strings.CutPrefix(file, dir+"/")
	if dir == "." {
		rel, ok = file, true
	}
	if !ok {
		return false
	}
	parts := strings.Split(rel, "/")
	for i := 1; i <= len(parts); i++ {
		if m, _ := path.Match(pat, strings.Join(parts[:i], "/")); !m {
			continue
		}
		if i == len(parts) {
			return true
		}
		if all {
			return true
		}
		for _, p := range parts[i:] {
			if strings.HasPrefix(p, ".") || strings.HasPrefix(p, "_") {
				return false
			}
		}
		return true
	}
	return false
}

func isMarkdown(f string) bool {
	ext := strings.ToLower(path.Ext(f))
	return ext == ".md" || ext == ".markdown"
}

func isTestFile(f string) bool {
	for _, p := range strings.Split(path.Dir(f), "/") {
		if p == "testdata" || p == "test" || p == "tests" || p == "__tests__" {
			return true
		}
	}
	return testFileRE.MatchString(path.Base(f))
}

func topDir(f string) string {
	top, _, _ := strings.Cut(f, "/")
	return top
}
