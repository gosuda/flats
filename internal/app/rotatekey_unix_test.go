//go:build !windows

package app

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ownerOf names the user and the group that a file belongs to.
func ownerOf(fi os.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Uid, st.Gid)
}

// Rule 19: when something that is no file is at the key path, a finish stops with status 1 and gives the two ways of
// step 3. A named pipe is the hard case: an open of it for writing waits for a reader, so the command must not open
// it.
func TestRotateKeyFinishWithANamedPipeAtTheKeyPath(t *testing.T) {
	h := newRotHost(t)
	pending := stateC(t, h, rotateCheckpointed, "nothing")
	if err := syscall.Mkfifo(h.keyPath, 0o600); err != nil {
		t.Skipf("no named pipes here: %v", err)
	}
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := rotateWith(t, h.config, nil)
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		failedWith(t, "the finish with a named pipe at the key path", r.err, h.keyPath+" is no regular file",
			"the pending key "+h.pending()+" holds the key of the values", "remove the cause", "copy the bytes of "+h.pending())
		if strings.Contains(r.err.Error(), "--out") {
			t.Errorf("the message names --out, which a finish refuses: %v", r.err)
		}
	case <-time.After(time.Minute):
		// The command waits in the open of the pipe. A reader lets it go, so that the test can end.
		if f, err := os.OpenFile(h.keyPath, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
			defer f.Close()
		}
		t.Fatal("the finish waits on the named pipe at the key path")
	}
	if got := mustRead(t, h.pending()); !bytes.Equal(got, pending) {
		t.Fatal("the finish changed the pending key")
	}
	// With the pipe gone, the next run finishes.
	if err := os.Remove(h.keyPath); err != nil {
		t.Fatal(err)
	}
	h.mustRotate(t)
	mustStart(t, h, rotSealed)
	mustHoldValues(t, h.dir, pending)
}
