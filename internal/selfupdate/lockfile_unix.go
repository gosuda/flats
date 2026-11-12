//go:build !windows

package selfupdate

import (
	"os"
	"syscall"
)

func openLockFile(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
}
