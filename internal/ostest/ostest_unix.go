//go:build !windows

package ostest

import "syscall"

// Alive reports whether the process exists.
func Alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func isPrivilegeError(error) bool { return false }
