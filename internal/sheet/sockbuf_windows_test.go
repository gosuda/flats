//go:build windows

package sheet

import "syscall"

func setReceiveBuffer(fd uintptr, n int) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, n)
}
