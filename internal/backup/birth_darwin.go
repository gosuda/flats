//go:build darwin

package backup

import (
	"io/fs"
	"syscall"
	"time"
)

// birthTime returns the creation time of a file on macOS.
func birthTime(_ string, fi fs.FileInfo) (time.Time, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(int64(st.Birthtimespec.Sec), int64(st.Birthtimespec.Nsec)), true
}
