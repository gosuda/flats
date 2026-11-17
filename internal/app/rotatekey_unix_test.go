//go:build !windows

package app

import (
	"fmt"
	"os"
	"syscall"
)

// ownerOf names the user and the group that a file belongs to.
func ownerOf(fi os.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Uid, st.Gid)
}
