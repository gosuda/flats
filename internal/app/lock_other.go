//go:build !darwin && !linux

package app

import (
	"fmt"
	"os"
)

func lockDataDir(dir string) (*os.File, error) {
	return nil, fmt.Errorf("data directory locking is unsupported on this operating system")
}
