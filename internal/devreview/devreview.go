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
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
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

// within reports whether p is dir or beneath it, comparing case-insensitively
// so a case-insensitive file system cannot slip a path past the check.
func within(dir, p string) bool {
	rel, err := filepath.Rel(strings.ToLower(dir), strings.ToLower(p))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
