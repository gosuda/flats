package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// openPTY returns both ends of a new pseudo-terminal.
func openPTY(t *testing.T) (ptm, pts *os.File) {
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

func echoing(t *testing.T, f *os.File) bool {
	t.Helper()
	mode, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return mode.Lflag&unix.ECHO != 0
}

// prompts passes on each prompt the terminal prints; hidden prints it once
// echo is off.
type prompts chan string

func (p prompts) Write(b []byte) (int, error) {
	if s := string(b); strings.TrimSpace(s) != "" {
		p <- s
	}
	return len(b), nil
}

// A password prompt leaves echo on however it ends: interrupted before it
// is shown, interrupted while it waits, or answered.
func TestTerminalLeavesEchoOn(t *testing.T) {
	_, pts := openPTY(t)
	if !echoing(t, pts) {
		t.Fatal("a new terminal does not echo")
	}
	early, cancel := context.WithCancel(context.Background())
	cancel()
	for range 50 {
		if _, err := (&terminal{ctx: early, f: pts, out: io.Discard}).hidden("Recovery password: "); !errors.Is(err, context.Canceled) {
			t.Fatalf("early interrupt: %v", err)
		}
		if !echoing(t, pts) {
			t.Fatal("echo is off after an early interrupt")
		}
	}

	// Each case below leaves a read blocked on its terminal, so each has its
	// own.
	_, pts = openPTY(t)
	ctx, cancel := context.WithCancel(context.Background())
	shown := make(prompts, 4)
	done := make(chan error, 1)
	go func() {
		_, err := (&terminal{ctx: ctx, f: pts, out: shown}).hidden("Recovery password: ")
		done <- err
	}()
	<-shown
	if echoing(t, pts) {
		t.Fatal("echo is on at the prompt")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupt at the prompt: %v", err)
	}
	if !echoing(t, pts) {
		t.Fatal("echo is off after an interrupt at the prompt")
	}

	ptm, pts := openPTY(t)
	shown = make(prompts, 4)
	type reply struct {
		s   string
		err error
	}
	answer := make(chan reply, 1)
	go func() {
		s, err := (&terminal{ctx: context.Background(), f: pts, out: shown}).hidden("Recovery password: ")
		answer <- reply{s, err}
	}()
	<-shown
	if echoing(t, pts) {
		t.Fatal("echo is on at the prompt")
	}
	if _, err := ptm.WriteString("a long recovery password\n"); err != nil {
		t.Fatal(err)
	}
	if r := <-answer; r.err != nil || r.s != "a long recovery password" {
		t.Fatalf("answer %q %v", r.s, r.err)
	}
	if !echoing(t, pts) {
		t.Fatal("echo is off after the answer")
	}
}
