// Package devreview records and checks the review evidence that the
// development protocol (docs/development-protocol.md, "Snapshot and
// evidence") requires: a manifest binding a review to an exact candidate,
// drift checks, a read-only materialized copy, invocation records for a
// user-configured reviewer command, and a task report skeleton.
//
// It is development tooling run with
// `go run ./internal/devreview/cmd/devreview`; the flats binary does not
// link it. The rules it implements are rules 40-50 of
// docs/spec/dev-workflow.md. It checks identities and records invocations;
// it never approves a stage.
package devreview

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RepoRoot returns the top of the Git work tree containing dir, with
// symbolic links resolved.
func RepoRoot(dir string) (string, error) {
	out, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(out)
}

// git runs a Git command in root and returns its trimmed output.
func git(root string, args ...string) (string, error) {
	out, err := gitBytes(root, args...)
	return strings.TrimSpace(string(out)), err
}

func gitBytes(root string, args ...string) ([]byte, error) {
	// Settings that make Git skip content or mode changes are overridden
	// so a candidate change cannot be hidden from the snapshot.
	base := []string{"-C", root, "-c", "core.fileMode=true", "-c", "core.ignoreStat=false"}
	cmd := exec.Command("git", append(base, args...)...)
	// Never let a read refresh and rewrite the index behind the snapshot.
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return out, nil
}

// gitZ runs a Git command whose output is NUL-separated.
func gitZ(root string, args ...string) ([]string, error) {
	out, err := gitBytes(root, args...)
	if err != nil {
		return nil, err
	}
	out = bytes.TrimSuffix(out, []byte{0})
	if len(out) == 0 {
		return nil, nil
	}
	return strings.Split(string(out), "\x00"), nil
}

// ErrInsideRepo marks evidence or configuration paths that resolve into the
// repository working tree.
var ErrInsideRepo = errors.New("resolves inside the repository working tree")

// OutsideRepo returns p as an absolute path with every existing component
// resolved, and an error wrapping ErrInsideRepo when that location is the
// repository root or beneath it. Components that do not exist yet are kept
// as written, so a destination is checked before it is created.
func OutsideRepo(root, p string) (string, error) {
	resolved, err := resolve(p)
	if err != nil {
		return "", err
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if within(rootResolved, resolved) {
		return "", fmt.Errorf("%s %w (%s)", p, ErrInsideRepo, rootResolved)
	}
	return resolved, nil
}

// resolve makes p absolute and resolves symbolic links in its longest
// existing prefix.
func resolve(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	var rest []string
	cur := abs
	for {
		r, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(append([]string{r}, rest...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		rest = append([]string{filepath.Base(cur)}, rest...)
		cur = parent
	}
}

// resolvePath resolves an absolute path component by component, following
// every symbolic link including intermediate ones the way the kernel does,
// and keeps components that do not exist as written.
func resolvePath(p string) (string, error) {
	vol := filepath.VolumeName(p)
	sep := string(filepath.Separator)
	cur := vol + sep
	todo := strings.Split(strings.TrimPrefix(p, vol), sep)
	for hops := 0; len(todo) > 0; {
		c := todo[0]
		todo = todo[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, c)
		fi, err := os.Lstat(next)
		if errors.Is(err, fs.ErrNotExist) {
			return filepath.Join(append([]string{next}, todo...)...), nil
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			cur = next
			continue
		}
		if hops++; hops > 255 {
			return "", fmt.Errorf("%s: too many levels of symbolic links", p)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			v := filepath.VolumeName(target)
			cur, target = v+sep, strings.TrimPrefix(target, v)
		}
		todo = append(strings.Split(target, sep), todo...)
	}
	return cur, nil
}

// noLinkAncestors refuses a relative path whose parent directories under
// base include a symbolic link, so writes and removals never pass through
// one.
func noLinkAncestors(base, rel string) error {
	dir := base
	parts := strings.Split(filepath.FromSlash(rel), string(filepath.Separator))
	for _, c := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, c)
		fi, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s lies beneath the symbolic link %s", rel, dir)
		}
	}
	return nil
}

// within reports whether p is dir or beneath it, comparing case-insensitively
// so a case-insensitive file system cannot slip a path past a check that
// refuses paths inside dir. Checks that accept paths inside dir use
// withinExact instead.
func within(dir, p string) bool { return withinExact(strings.ToLower(dir), strings.ToLower(p)) }

// withinExact reports whether p is dir or beneath it, comparing exactly.
func withinExact(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
