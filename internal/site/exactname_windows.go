package site

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/gosuda/flats/internal/winname"
)

// allowedName refuses a request path that Windows would map to another name
// before it reaches the file system: a stream, a device, a trailing dot or
// space, an 8.3 alias.
func allowedName(rel string) bool { return winname.CheckPath(rel) == nil }

// exactName reports whether the open file is stored under exactly the
// requested relative path. Windows also opens a file under another case or
// under its short name, and those must not answer: the checks of a handler,
// such as the one that hides flats.json, compare exact names.
func exactName(f *os.File, rel string) bool {
	h := windows.Handle(f.Fd())
	n, err := windows.GetFinalPathNameByHandle(h, nil, 0, 0)
	if err != nil || n == 0 {
		return false
	}
	buf := make([]uint16, n)
	n, err = windows.GetFinalPathNameByHandle(h, &buf[0], n, 0)
	if err != nil || int(n) > len(buf) {
		return false
	}
	final := windows.UTF16ToString(buf[:n])
	return strings.HasSuffix(final, `\`+filepath.FromSlash(rel))
}
