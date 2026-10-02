package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// packStats describes a packed directory.
type packStats struct {
	Files   int
	Bytes   int64 // uncompressed
	Skipped int
}

// skipEntry reports names that never belong in an upload. It matches the
// server's own skip list so local and remote counts agree.
func skipEntry(name string, dir bool) bool {
	switch {
	case name == ".git", name == "__MACOSX":
		return true
	case dir && name == "node_modules":
		return true
	case !dir && (name == ".DS_Store" || strings.HasPrefix(name, "._")):
		return true
	}
	return false
}

// packDir writes the regular files under root to an in-memory tar.gz with
// slash-separated paths relative to root. Links and special files are
// rejected, because the server refuses them and a link could pull files from
// outside the build into a public site. limit > 0 bounds the uncompressed
// size.
func packDir(root string, limit int64) ([]byte, packStats, error) {
	var st packStats
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if skipEntry(d.Name(), d.IsDir()) {
			st.Skipped++
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link; uploads cannot contain links (copy the target into the build output instead)", rel)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file; remove it from the build output", rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		st.Files++
		st.Bytes += info.Size()
		if limit > 0 && st.Bytes > limit {
			return fmt.Errorf("build is larger than the server's %d-byte upload limit (remove source maps, large media or dependencies from the build output)", limit)
		}
		hdr := &tar.Header{
			Name:     rel,
			Mode:     int64(info.Mode().Perm()),
			Size:     info.Size(),
			ModTime:  info.ModTime().Truncate(time.Second),
			Typeflag: tar.TypeReg,
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		n, err := io.Copy(tw, f)
		if err != nil {
			return err
		}
		if n != info.Size() {
			return fmt.Errorf("%s changed while packing", rel)
		}
		return nil
	})
	if err != nil {
		return nil, st, err
	}
	if st.Files == 0 {
		return nil, st, fmt.Errorf("%s has no files to upload", root)
	}
	if err := tw.Close(); err != nil {
		return nil, st, err
	}
	if err := gz.Close(); err != nil {
		return nil, st, err
	}
	return buf.Bytes(), st, nil
}

// gitInfo is the commit a build came from.
type gitInfo struct {
	SHA   string
	Dirty bool
}

// detectGit reads HEAD and the dirty flag of the repository containing dir.
// It returns ok=false when git is missing or dir is not in a repository with
// commits.
func detectGit(ctx context.Context, dir string) (gitInfo, bool) {
	git, err := exec.LookPath("git")
	if err != nil {
		return gitInfo{}, false
	}
	// Inside a git hook, GIT_DIR and friends point at the hook's repository
	// (often as a relative path); drop them so git finds dir's own repository.
	var env []string
	for _, kv := range os.Environ() {
		switch k, _, _ := strings.Cut(kv, "="); k {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_PREFIX":
			continue
		}
		env = append(env, kv)
	}
	// Avoid taking the index lock: a status refresh must not race the user's
	// own git commands.
	env = append(env, "GIT_OPTIONAL_LOCKS=0")
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, git, args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.Output()
		return string(out), err
	}
	sha, err := run("rev-parse", "HEAD")
	if err != nil {
		return gitInfo{}, false
	}
	gi := gitInfo{SHA: strings.TrimSpace(sha)}
	if out, err := run("status", "--porcelain"); err == nil {
		gi.Dirty = strings.TrimSpace(out) != ""
	}
	return gi, true
}
