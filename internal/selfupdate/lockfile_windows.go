package selfupdate

import "os"

func openLockFile(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
}
