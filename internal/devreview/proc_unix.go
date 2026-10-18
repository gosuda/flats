//go:build unix

package devreview

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
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

// waitGroupGone polls until no process remains in the group or the wait
// ends, and reports whether the group is gone.
func waitGroupGone(pgid int, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for groupAlive(pgid) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
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
	// O_NOFOLLOW: a symbolic link planted as the lock file must not make
	// the helper create or lock its target.
	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f, false); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// lockHeld reports whether another open file description holds file's
// lock. A process that forks briefly holds every descriptor of its parent
// until it executes, so a held lock is retried before it counts.
func lockHeld(file string) (bool, error) {
	for i := 0; ; i++ {
		held, err := tryLock(file)
		if err != nil || !held || i == 20 {
			return held, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func tryLock(file string) (bool, error) {
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
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
