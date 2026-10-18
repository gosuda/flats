//go:build !unix

package devreview

import (
	"errors"
	"os"
	"os/exec"
	"time"
)

var errUnsupported = errors.New("run needs process groups and file locks; it is supported on Unix systems only")

func runSupported() error                   { return errUnsupported }
func setProcessGroup(*exec.Cmd)             {}
func terminateGroup(int)                    {}
func killGroup(int)                         {}
func groupAlive(int) bool                   { return true }
func waitGroupGone(int, time.Duration) bool { return false }
func lockFile(*os.File, bool) error         { return errUnsupported }
func lockPath(string) (func(), error)       { return nil, errUnsupported }
func lockHeld(string) (bool, error)         { return true, errUnsupported }
