//go:build !windows

package app

import (
	"context"
	"errors"
	"flag"
	"os"

	"github.com/gosuda/flats/internal/filelock"
)

func serviceFlags(*flag.FlagSet, *Options) {}

// redirectLog is never reached: only Windows defines --log-file.
func redirectLog(string) error { return errors.New("--log-file exists only on Windows") }

// watchParent is never reached: only Windows defines --watch-parent.
func watchParent(context.Context, func()) error {
	return errors.New("--watch-parent exists only on Windows")
}

func recordPID(*os.File) {}

func tryLockDataDir(f *os.File) error { return filelock.TryLock(f) }
