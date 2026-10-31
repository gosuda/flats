package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/buildinfo"
	"github.com/gosuda/flats/internal/clientstate"
)

const archiveLimit = 128 << 20
const executableLimit = 256 << 20

// Identity is returned by a freshly executed candidate.
type Identity struct {
	buildinfo.Info
	ClientContract int `json:"client_contract"`
}

// Options contains local context; transport/probe seams never change release URLs.
type Options struct {
	Path, Version, GOOS, GOARCH, ConfigDir, Home string
	RequiredContract                             int
	Transport                                    http.RoundTripper
	Probe                                        func(context.Context, string) (Identity, error)
	Recheck                                      func() error
}

// ValidTag accepts only release tags usable in official release URLs.
func ValidTag(tag string) bool {
	return clientstate.ValidRelease(tag)
}
func releaseClient(transport http.RoundTripper) *http.Client {
	return &http.Client{Transport: transport, Timeout: 3 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if r.URL.Scheme != "https" {
			return failure("transport", "release redirect attempted HTTPS downgrade")
		}
		if len(via) >= 10 {
			return failure("transport", "too many release redirects")
		}
		return nil
	}}
}
func download(ctx context.Context, c *http.Client, url string, limit int64) ([]byte, error) {
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if e != nil {
		return nil, e
	}
	r, e := c.Do(req)
	if e != nil {
		return nil, failure("transport", "release download: %w", e)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return nil, failure("assets_unavailable", "release asset returned HTTP %d", r.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if e != nil {
		return nil, failure("transport", "read release: %w", e)
	}
	if int64(len(b)) > limit {
		return nil, failure("assets_unavailable", "release asset exceeds size limit")
	}
	return b, nil
}
func checksum(raw []byte, name string) (string, error) {
	found := ""
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		if found != "" || len(fields) != 2 || !regexp.MustCompile(`^[0-9a-fA-F]{64}$`).MatchString(fields[0]) {
			return "", failure("checksum", "invalid or duplicate checksum for %s", name)
		}
		found = strings.ToLower(fields[0])
	}
	if found == "" {
		return "", failure("checksum", "missing checksum for %s", name)
	}
	return found, nil
}

// extractTo runs the single decompression pass over the release archive,
// streaming the flats executable into w instead of materializing it in
// memory. Verification stays as before: the archive digest is checked before
// this runs, and the trailing drain below still confirms the remainder of the
// decompressed stream inside the budget.
func extractTo(w io.Writer, raw []byte) error {
	gz, e := gzip.NewReader(bytes.NewReader(raw))
	if e != nil {
		return failure("assets_unavailable", "invalid release archive: %w", e)
	}
	defer gz.Close()
	limited := &io.LimitedReader{R: gz, N: executableLimit + (8 << 20) + 1}
	tr := tar.NewReader(limited)
	found := false
	for {
		h, e := tr.Next()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return failure("assets_unavailable", "invalid release archive: %w", e)
		}
		if !path.IsAbs(h.Name) && path.Clean(h.Name) == h.Name && h.Name != ".." && !strings.HasPrefix(h.Name, "../") && !strings.Contains(h.Name, "\\") {
		} else {
			return failure("assets_unavailable", "unsafe archive entry %q", h.Name)
		}
		if h.Name != "flats" {
			continue
		}
		if found || h.Typeflag != tar.TypeReg || h.Size <= 0 || h.Size > executableLimit {
			return failure("assets_unavailable", "invalid flats executable entry")
		}
		if _, e = io.Copy(w, io.LimitReader(tr, executableLimit+1)); e != nil {
			return e
		}
		found = true
	}
	if _, err := io.Copy(io.Discard, limited); err != nil {
		return failure("assets_unavailable", "invalid compressed archive: %w", err)
	}
	if limited.N == 0 {
		return failure("assets_unavailable", "decompressed release exceeds size limit")
	}
	if !found {
		return failure("assets_unavailable", "release archive has no flats executable")
	}
	return nil
}

// stageStream fills the stage file through fill, so the candidate executable
// is written once from the archive stream and never buffered whole. It mirrors
// the create/chmod/sync/close sequence of stage in files.go. fill runs even
// when the stage file cannot be created or written, and its error comes first
// and alone: a bad archive is reported before any problem with the stage file.
func stageStream(path string, mode os.FileMode, fill func(w io.Writer) error) (string, error) {
	name := stageName(path)
	f, e := openStage(name, mode)
	if e != nil {
		if fe := fill(io.Discard); fe != nil {
			return "", fe
		}
		return "", e
	}
	pe := f.Chmod(mode)
	w := &stageWriter{w: f}
	if fe := fill(w); fe != nil {
		f.Close()
		os.Remove(name)
		return "", fe
	}
	se := f.Sync()
	ce := f.Close()
	if e = errors.Join(pe, w.err, se, ce); e != nil {
		os.Remove(name)
		return "", e
	}
	return name, nil
}

// stageFile is the part of *os.File that stageStream uses.
type stageFile interface {
	io.Writer
	Chmod(os.FileMode) error
	Sync() error
	Close() error
}

// openStage removes a stale stage file and creates a new one. Tests replace it
// to make the file's operations fail.
var openStage = func(name string, mode os.FileMode) (stageFile, error) {
	if e := os.Remove(name); e != nil && !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	f, e := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if e != nil {
		return nil, e
	}
	return f, nil
}

// stageWriter keeps the first write error and reports every write as done,
// so extraction reads the rest of the archive and reports its problems first.
type stageWriter struct {
	w   io.Writer
	err error
}

func (s *stageWriter) Write(p []byte) (int, error) {
	if s.err == nil {
		_, s.err = s.w.Write(p)
	}
	return len(p), nil
}

type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 64<<10 {
		return 0, fmt.Errorf("candidate output exceeds limit")
	}
	return b.Buffer.Write(p)
}
func probe(ctx context.Context, p string) (Identity, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p, "--json", "version")
	var out cappedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if e := cmd.Run(); e != nil {
		return Identity{}, e
	}
	var id Identity
	e := json.Unmarshal(out.Bytes(), &id)
	return id, e
}

// Update verifies and atomically replaces one standalone executable.
func Update(ctx context.Context, o Options) (Identity, string, error) {
	if !ValidTag(o.Version) {
		return Identity{}, "", failure("invalid_target", "invalid release tag %q", o.Version)
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.GOARCH == "" {
		o.GOARCH = runtime.GOARCH
	}
	if (o.GOOS != "darwin" && o.GOOS != "linux") || (o.GOARCH != "amd64" && o.GOARCH != "arm64") {
		return Identity{}, "", failure("invalid_target", "unsupported platform %s/%s", o.GOOS, o.GOARCH)
	}
	abs, e := filepath.Abs(o.Path)
	if e != nil {
		return Identity{}, "", e
	}
	resolved, e := filepath.EvalSymlinks(abs)
	if e != nil {
		return Identity{}, abs, e
	}
	if abs != resolved {
		return Identity{}, abs, failure("invalid_target", "executable path resolves elsewhere; use its installation manager")
	}
	if strings.Contains(resolved, "/Cellar/") || strings.Contains(resolved, "/Caskroom/") || strings.HasPrefix(resolved, "/nix/store/") {
		return Identity{}, abs, failure("package_managed", "package-managed executable; use its installation manager")
	}
	if strings.Contains(resolved, "/go-build") {
		return Identity{}, abs, failure("invalid_target", "temporary go-build executable cannot be updated")
	}
	st, e := regular(abs)
	if e != nil {
		return Identity{}, abs, e
	}
	if st.Mode().Perm()&0222 == 0 {
		return Identity{}, abs, failure("filesystem", "executable is not writable")
	}
	if e = checkService(abs, o.Home, o.GOOS); e != nil {
		return Identity{}, abs, e
	}
	unlock, e := lock(abs, o.ConfigDir)
	if e != nil {
		return Identity{}, abs, e
	}
	defer unlock()
	original, e := os.ReadFile(abs)
	if e != nil {
		return Identity{}, abs, e
	}
	c := releaseClient(o.Transport)
	base := "https://github.com/gosuda/flats/releases/download/" + o.Version + "/"
	name := "flats_" + o.GOOS + "_" + o.GOARCH + ".tar.gz"
	sums, e := download(ctx, c, base+"checksums.txt", 1<<20)
	if e != nil {
		return Identity{}, abs, e
	}
	sum, e := checksum(sums, name)
	if e != nil {
		return Identity{}, abs, e
	}
	archive, e := download(ctx, c, base+name, archiveLimit)
	if e != nil {
		return Identity{}, abs, e
	}
	if Digest(archive) != sum {
		return Identity{}, abs, failure("checksum", "release archive checksum mismatch")
	}
	staged, e := stageStream(abs, 0755, func(w io.Writer) error { return extractTo(w, archive) })
	if e != nil {
		return Identity{}, abs, e
	}
	defer os.Remove(staged)
	if o.Probe == nil {
		o.Probe = probe
	}
	id, e := o.Probe(ctx, staged)
	if e != nil {
		return Identity{}, abs, failure("candidate_mismatch", "candidate version probe failed: %w", e)
	}
	if id.Version != o.Version || !id.Release || id.Dirty || id.ClientContract <= 0 || (o.RequiredContract > 0 && id.ClientContract != o.RequiredContract) || id.OS != o.GOOS || id.Arch != o.GOARCH {
		return Identity{}, abs, failure("candidate_mismatch", "candidate identity does not match requested release/platform/contract")
	}
	if o.Recheck != nil {
		if e = o.Recheck(); e != nil {
			return Identity{}, abs, e
		}
	}
	if e = checkService(abs, o.Home, o.GOOS); e != nil {
		return Identity{}, abs, e
	}
	if _, e = regular(abs); e != nil {
		return Identity{}, abs, e
	}
	now, e := os.ReadFile(abs)
	if e != nil {
		return Identity{}, abs, e
	}
	if Digest(now) != Digest(original) {
		return Identity{}, abs, failure("file_conflict", "executable changed before replacement")
	}
	if e = commit(ctx, staged, abs); e != nil {
		return Identity{}, abs, e
	}
	return id, abs, nil
}
