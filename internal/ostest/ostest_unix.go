//go:build !windows

package ostest

import "syscall"

// Alive reports whether the process exists.
func Alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func isPrivilegeError(error) bool { return false }

// CPUCounted reports whether the system has counted CPU time for the process.
// The count is in microseconds here, and no run has read zero for a process
// that started, so there is nothing to wait for.
func CPUCounted(int) bool { return true }
