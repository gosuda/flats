// Package ostest gives tests one way to do what differs between systems.
package ostest

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// Kill ends the process at once.
func Kill(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer p.Release()
	return p.Kill()
}

// Windows reports whether the tests run on Windows.
const Windows = runtime.GOOS == "windows"

// WrongMode reports whether the file's mode differs from want. Windows has no
// POSIX modes, and the data directory's access list protects those files.
func WrongMode(fi os.FileInfo, want os.FileMode) bool {
	return !Windows && fi.Mode().Perm() != want
}

// WrongPerm is WrongMode for a mode that the test already read.
func WrongPerm(got, want os.FileMode) bool { return !Windows && got != want }

// Exposed reports whether others can reach the file.
func Exposed(fi os.FileInfo) bool { return !Windows && fi.Mode().Perm()&0o077 != 0 }

// TempRoot is the parent for test directories that need a short path, for Unix
// sockets: macOS's own temporary directory is too long for them.
func TempRoot() string {
	if runtime.GOOS == "windows" {
		return os.TempDir()
	}
	return "/tmp"
}

// SkipOnWindows skips a test of Unix behavior, with the reason.
func SkipOnWindows(t testing.TB, why string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows: " + why)
	}
}

// SkipPOSIXModes skips a test that reads a POSIX file mode: Windows keeps only
// a read-only bit, and the data directory's access list protects those files.
func SkipPOSIXModes(t testing.TB) {
	t.Helper()
	SkipOnWindows(t, "no POSIX file modes")
}

// LinkDir links the directory target at link. It needs no privilege: on Windows
// it makes a junction, for a test that does not depend on a symlink.
func LinkDir(t testing.TB, target, link string) error {
	t.Helper()
	if !Windows {
		return os.Symlink(target, link)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		return fmt.Errorf("mklink: %w: %s", err, out)
	}
	return nil
}

// Symlink creates a link, and skips the test where the user may not: Windows
// needs a privilege or Developer Mode for it.
func Symlink(t testing.TB, target, link string) error {
	t.Helper()
	err := os.Symlink(target, link)
	var le *os.LinkError
	if runtime.GOOS == "windows" && errors.As(err, &le) && isPrivilegeError(le.Err) {
		t.Skip("Windows: creating a symlink needs a privilege")
	}
	return err
}
