package ostest

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// PTY returns both ends of a new pseudo-terminal, closed when the test ends.
// It skips the test where none can be opened.
func PTY(t *testing.T) (ptm, pts *os.File) {
	t.Helper()
	ptm, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { ptm.Close() })
	if err := unix.IoctlSetPointerInt(int(ptm.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetUint32(int(ptm.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	pts, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pts.Close() })
	return ptm, pts
}

// Echoing reports whether the terminal f echoes what is typed.
func Echoing(t *testing.T, f *os.File) bool {
	t.Helper()
	mode, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return mode.Lflag&unix.ECHO != 0
}

// Prompts is a writer that passes on each write that is not blank, such as
// the prompt a terminal question prints.
type Prompts chan string

func (p Prompts) Write(b []byte) (int, error) {
	if s := string(b); strings.TrimSpace(s) != "" {
		p <- s
	}
	return len(b), nil
}
