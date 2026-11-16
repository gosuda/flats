//go:build !linux && !darwin && !windows

package backup

import (
	"io/fs"
	"time"
)

// birthTime reports that this system gives no creation time. The file identity
// alone then tells two directories apart.
func birthTime(string, fs.FileInfo) (time.Time, bool) { return time.Time{}, false }
