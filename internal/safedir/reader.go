// Package safedir provides checked reads from an anchored directory tree.
package safedir

import (
	"fmt"
	"io/fs"
	"os"
)

// OpenRegular opens a walked file under root and checks that its identity is
// still the regular file seen by the walker. root must also have been used for
// traversal (fs.WalkDir(root.FS(), ".")); the caller must close the result.
func OpenRegular(root *os.Root, name string, seen fs.FileInfo) (*os.File, fs.FileInfo, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err == nil {
		// Reject a newly created link, even one pointing to the original
		// inode. Root confines resolution of intermediate links as well.
		var current fs.FileInfo
		current, err = root.Lstat(name)
		if err == nil && (!info.Mode().IsRegular() || !current.Mode().IsRegular() ||
			!os.SameFile(seen, info) || !os.SameFile(current, info)) {
			err = fmt.Errorf("%s changed during directory packing", name)
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, info, nil
}
