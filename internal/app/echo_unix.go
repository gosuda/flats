//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || zos

package app

import "golang.org/x/sys/unix"

// echoOff turns echo off on the terminal fd with the mode term.ReadPassword
// sets, and returns the function that puts the saved mode back.
func echoOff(fd int) (restore func(), err error) {
	saved, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return nil, err
	}
	mode := *saved
	mode.Lflag &^= unix.ECHO
	mode.Lflag |= unix.ICANON | unix.ISIG
	mode.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, &mode); err != nil {
		return nil, err
	}
	return func() { _ = unix.IoctlSetTermios(fd, ioctlWriteTermios, saved) }, nil
}
