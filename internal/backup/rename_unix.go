//go:build !windows

package backup

import (
	"os"
	"syscall"
)

// renameNoReplace moves the directory from to to, and fails when anything exists
// at to, an empty directory included (rule 21). It claims the name with an
// exclusive Mkdir, and the rename then replaces that empty directory. os.Rename
// refuses to replace a directory, so the call is syscall.Rename.
func renameNoReplace(from, to string) error {
	if err := os.Mkdir(to, 0o700); err != nil {
		return err
	}
	if err := syscall.Rename(from, to); err != nil {
		os.Remove(to)
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}
