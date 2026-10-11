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

	"golang.org/x/sys/unix"
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

// Code returns the public error classification.
func Code(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancelled"
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
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
	fd, e := unix.Open(name, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, e
	}
	if e = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); e != nil {
		unix.Close(fd)
		return nil, failure("locked", "another update holds %s: %w", path, e)
	}
	return func() { unix.Flock(fd, unix.LOCK_UN); unix.Close(fd) }, nil
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
func commit(ctx context.Context, staged, path string) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if e := os.Rename(staged, path); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(path))
	if e == nil {
		e = errors.Join(d.Sync(), d.Close())
	}
	if e != nil {
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
