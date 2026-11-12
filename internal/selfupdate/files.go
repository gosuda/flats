// Package selfupdate performs explicit local client resource updates.
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/durable"
	"github.com/gosuda/flats/internal/filelock"
)

// Error carries a stable machine-readable failure code.
type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }
func failure(code, format string, args ...any) error {
	return &Error{code, fmt.Errorf(format, args...)}
}

var codePrecedence = map[string]int{
	"cancelled":          1,
	"locked":             2,
	"package_managed":    3,
	"invalid_target":     4,
	"service_binary":     5,
	"file_conflict":      6,
	"candidate_mismatch": 7,
	"checksum":           8,
	"assets_unavailable": 9,
	"transport":          10,
	"server_changed":     11,
	"filesystem":         100,
}

func codeRank(code string) int {
	if r, ok := codePrecedence[code]; ok {
		return r
	}
	return 50
}

func walkErrors(err error, fn func(error)) {
	if err == nil {
		return
	}
	fn(err)
	switch u := err.(type) {
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			walkErrors(e, fn)
		}
	case interface{ Unwrap() error }:
		walkErrors(u.Unwrap(), fn)
	}
}

// Code returns the public error classification following precedence:
// cancelled, locked, package_managed, invalid_target, service_binary,
// file_conflict, candidate_mismatch, checksum, assets_unavailable,
// transport, server_changed, custom codes, filesystem.
func Code(err error) string {
	if err == nil {
		return ""
	}
	best := ""
	bestRank := 1000
	walkErrors(err, func(e error) {
		if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
			rank := codeRank("cancelled")
			if rank < bestRank || (rank == bestRank && (best == "" || "cancelled" < best)) {
				best = "cancelled"
				bestRank = rank
			}
		}
		if appErr, ok := errors.AsType[*Error](e); ok && appErr.Code != "" {
			rank := codeRank(appErr.Code)
			if rank < bestRank || (rank == bestRank && (best == "" || appErr.Code < best)) {
				best = appErr.Code
				bestRank = rank
			}
		}
	})
	if best != "" {
		return best
	}
	return "filesystem"
}

// Digest hashes exact bytes.
func Digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func canonical(path string) (string, error) {
	p, e := filepath.Abs(path)
	if e != nil {
		return "", e
	}
	parent, e := filepath.EvalSymlinks(filepath.Dir(p))
	if e != nil {
		return "", e
	}
	return filepath.Join(parent, filepath.Base(p)), nil
}
func lock(path, config string) (func(), error) {
	if config == "" {
		var e error
		config, e = os.UserConfigDir()
		if e != nil {
			return nil, e
		}
	}
	dir := filepath.Join(config, "flats-client", "locks")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	name := filepath.Join(dir, Digest([]byte(path))+".lock")
	f, e := openLockFile(name)
	if e != nil {
		return nil, e
	}
	if e = filelock.TryLock(f); e != nil {
		f.Close()
		return nil, failure("locked", "another update holds %s: %w", path, e)
	}
	return func() { filelock.Unlock(f); f.Close() }, nil
}
func regular(path string) (os.FileInfo, error) {
	st, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() {
		return nil, failure("invalid_target", "%s is not a regular file (symlinks are refused)", path)
	}
	return st, nil
}
func stageName(path string) string {
	return filepath.Join(filepath.Dir(path), ".flats-update-"+Digest([]byte(path))+".stage")
}
func stage(path string, data []byte, mode os.FileMode) (string, error) {
	name := stageName(path)
	if e := os.Remove(name); e != nil && !errors.Is(e, os.ErrNotExist) {
		return "", e
	}
	f, e := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if e != nil {
		return "", e
	}
	pe := f.Chmod(mode)
	_, we := f.Write(data)
	se := f.Sync()
	ce := f.Close()
	if e = errors.Join(pe, we, se, ce); e != nil {
		os.Remove(name)
		return "", e
	}
	return name, nil
}

// commitExecutable is commit for the running binary. Windows refuses to replace
// a running image but lets it move, so it steps aside first. Each aside gets
// its own name: the service or another process may still run the previous one,
// and Windows refuses to delete that. Asides of earlier updates that no longer
// run go now; a later update takes the rest.
func commitExecutable(ctx context.Context, staged, path string) error {
	if runtime.GOOS != "windows" {
		return commit(ctx, staged, path)
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	if old, e := filepath.Glob(escapeGlob(path) + ".old-*"); e == nil {
		for _, name := range old {
			_ = os.Remove(name) // a running one stays
		}
	}
	aside := fmt.Sprintf("%s.old-%d", path, time.Now().UnixNano())
	if e := os.Rename(path, aside); e != nil {
		return e
	}
	if e := os.Rename(staged, path); e != nil {
		return errors.Join(e, os.Rename(aside, path))
	}
	_ = os.Remove(aside)
	return nil
}

// escapeGlob quotes the pattern characters of a literal path. Windows has no
// escape character in patterns, so a bracket becomes a class of itself.
func escapeGlob(path string) string {
	return strings.NewReplacer("[", "[[]", "*", "[*]", "?", "[?]").Replace(path)
}

func commit(ctx context.Context, staged, path string) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if e := os.Rename(staged, path); e != nil {
		return e
	}
	if e := durable.SyncDir(filepath.Dir(path)); e != nil {
		return failure("filesystem", "new bytes may already be installed at %s; directory durability failed: %w", path, e)
	}
	return nil
}

// SyncSkill installs caller-validated bootstrap bytes at a standalone SKILL.md.
func SyncSkill(ctx context.Context, path string, data []byte, expected, config string, recheck func() error) (bool, string, error) {
	if filepath.Base(path) != "SKILL.md" || len(data) > 32<<10 {
		return false, "", failure("invalid_target", "output must be SKILL.md and content at most 32 KiB")
	}
	if expected != "" {
		b, e := hex.DecodeString(expected)
		if e != nil || len(b) != 32 {
			return false, "", failure("file_conflict", "expected SHA-256 must be 64 hexadecimal characters")
		}
	}
	if e := os.MkdirAll(filepath.Dir(path), 0755); e != nil {
		return false, "", e
	}
	p, e := canonical(path)
	if e != nil {
		return false, "", e
	}
	unlock, e := lock(p, config)
	if e != nil {
		return false, p, e
	}
	defer unlock()
	read := func() ([]byte, error) {
		_, e := regular(p)
		if errors.Is(e, os.ErrNotExist) {
			return nil, nil
		}
		if e != nil {
			return nil, e
		}
		return os.ReadFile(p)
	}
	old, e := read()
	if e != nil {
		return false, p, e
	}
	if old != nil && Digest(old) == Digest(data) {
		return false, p, nil
	}
	if old != nil && (expected == "" || Digest(old) != expected) {
		return false, p, failure("file_conflict", "existing skill differs; supply --expected-sha256 with its current exact digest")
	}
	if old == nil && expected != "" {
		return false, p, failure("file_conflict", "expected existing skill is missing")
	}
	staged, e := stage(p, data, 0644)
	if e != nil {
		return false, p, e
	}
	defer os.Remove(staged)
	if recheck != nil {
		if e = recheck(); e != nil {
			return false, p, e
		}
	}
	now, e := read()
	if e != nil {
		return false, p, e
	}
	if (old == nil) != (now == nil) || Digest(old) != Digest(now) {
		return false, p, failure("file_conflict", "skill changed before replacement")
	}
	if e = commit(ctx, staged, p); e != nil {
		return false, p, e
	}
	return true, p, nil
}
