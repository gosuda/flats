package durable

import (
	"io"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// open opens the file for reading and shares delete access, which os.Open
// does not.
func open(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// Open opens a file for reading, as ReadFile does, and returns it for a
// streaming read. It shares read, write and delete access with other openers.
func Open(path string) (*os.File, error) { return open(path) }

// ReadFile is os.ReadFile for a file that another program replaces while Flats
// runs. os.ReadFile holds the file open without delete sharing, and a
// ReplaceFile of that file then fails: the Tailscale state store writes that
// way and does not try again. This read lets a ReplaceFile and a removal
// through. A rename onto the file still waits until the read closed it.
func ReadFile(path string) ([]byte, error) {
	f, err := open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
