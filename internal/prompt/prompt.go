// Package prompt asks a question on a terminal and reads the answer, without
// echo for a password. An interrupt ends the wait at once and leaves echo on.
package prompt

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
)

// Hidden writes question to w and reads one line from the terminal f without
// echo. Echo goes off and comes back in this goroutine, and the read changes
// no terminal mode, so an interrupt at any moment leaves echo on.
func Hidden(ctx context.Context, f *os.File, w io.Writer, question string) (string, error) {
	restore, err := echoOff(int(f.Fd()))
	if err != nil {
		return "", err
	}
	defer restore()
	return ask(ctx, w, question, func() (string, error) { return readHiddenLine(f) })
}

// Line writes question to w and reads one line from f, line end included.
func Line(ctx context.Context, f *os.File, w io.Writer, question string) (string, error) {
	return ask(ctx, w, question, func() (string, error) {
		s, err := bufio.NewReader(f).ReadString('\n')
		if errors.Is(err, io.EOF) {
			err = nil
		}
		return s, err
	})
}

// ask writes question and waits for read or for ctx, whose error it returns:
// main catches SIGINT, so Ctrl-C ends the wait, not the read, which stays
// blocked until the process exits.
func ask(ctx context.Context, w io.Writer, question string, read func() (string, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fmt.Fprint(w, question)
	type reply struct {
		s   string
		err error
	}
	ch := make(chan reply, 1)
	go func() {
		s, err := read()
		ch <- reply{s, err}
	}()
	select {
	case r := <-ch:
		fmt.Fprintln(w)
		return r.s, r.err
	case <-ctx.Done():
		fmt.Fprintln(w)
		return "", ctx.Err()
	}
}

// readHiddenLine reads one line as term.ReadPassword does: it ends at a line
// feed, or a carriage return on Windows, and a backspace drops one byte.
func readHiddenLine(r io.Reader) (string, error) {
	var b [1]byte
	var line []byte
	for {
		n, err := r.Read(b[:])
		if n > 0 {
			switch c := b[0]; {
			case c == '\b':
				if len(line) > 0 {
					line = line[:len(line)-1]
				}
			case c == '\n' && runtime.GOOS != "windows", c == '\r' && runtime.GOOS == "windows":
				return string(line), nil
			case c == '\n', c == '\r':
			default:
				line = append(line, c)
			}
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return string(line), nil
			}
			return "", err
		}
	}
}
