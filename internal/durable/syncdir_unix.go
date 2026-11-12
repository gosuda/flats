//go:build !windows

package durable

import "os"

// SyncDir flushes dir, so that a file created or renamed in it stays.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}
