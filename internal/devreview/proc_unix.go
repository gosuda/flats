//go:build unix

package devreview

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func runSupported() error { return nil }

// setProcessGroup starts cmd as the leader of a new process group, so the
// whole reviewer tree can be signalled and its existence checked.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateGroup(pgid int) { _ = syscall.Kill(-pgid, syscall.SIGTERM) }

func killGroup(pgid int) { _ = syscall.Kill(-pgid, syscall.SIGKILL) }

// groupAlive reports whether any process remains in the group.
func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// lockFile takes an exclusive flock on f. The lock belongs to the open file
// description, so a child that inherits the descriptor keeps it held.
func lockFile(f *os.File, nonblock bool) error {
	how := syscall.LOCK_EX
	if nonblock {
		how |= syscall.LOCK_NB
	}
	for {
		err := syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// lockPath takes an exclusive lock on file, creating it, and returns the
// function that releases it.
func lockPath(file string) (func(), error) {
	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f, false); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// lockHeld reports whether another open file description holds file's lock.
func lockHeld(file string) (bool, error) {
	f, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	err = lockFile(f, true)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}
