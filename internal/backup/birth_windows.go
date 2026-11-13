//go:build windows

package backup

import (
	"io/fs"
	"syscall"
	"time"
)

// birthTime returns the creation time of a file on Windows.
func birthTime(_ string, fi fs.FileInfo) (time.Time, bool) {
	st, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(0, st.CreationTime.Nanoseconds()), true
}
