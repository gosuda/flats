//go:build unix

package sheet

import "syscall"

func setReceiveBuffer(fd uintptr, n int) error {
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, n)
}
