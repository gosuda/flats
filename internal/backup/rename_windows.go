//go:build windows

package backup

import (
	"time"

	"golang.org/x/sys/windows"
)

// renameNoReplace moves the directory from to to, and fails when anything exists
// at to, an empty directory included (rule 21). MoveFileEx without the replace
// flag does that. It tries again for two seconds on a sharing violation, as the
// durable package does, because a scanner can hold the new directory open.
func renameNoReplace(from, to string) error {
	f, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	t, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = windows.MoveFileEx(f, t, 0)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		if err != windows.ERROR_ACCESS_DENIED && err != windows.ERROR_SHARING_VIOLATION && err != windows.ERROR_LOCK_VIOLATION {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}
