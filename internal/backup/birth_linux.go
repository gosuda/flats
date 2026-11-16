//go:build linux

package backup

import (
	"io/fs"
	"time"

	"golang.org/x/sys/unix"
)

// birthTime returns the creation time of path where the file system keeps one.
func birthTime(path string, _ fs.FileInfo) (time.Time, bool) {
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &stx); err != nil {
		return time.Time{}, false
	}
	if stx.Mask&unix.STATX_BTIME == 0 {
		return time.Time{}, false
	}
	return time.Unix(stx.Btime.Sec, int64(stx.Btime.Nsec)), true
}
