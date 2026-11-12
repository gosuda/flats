package prompt

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadHiddenLine(t *testing.T) {
	// A line ends at "\n" on Unix and at "\r" on Windows; "\r\n" ends it on
	// both, and a backspace drops a byte.
	for _, c := range []struct{ in, want string }{
		{"pass word\r\n", "pass word"},
		{"pa\bass\r\nnext\r\n", "pass"},
		{"no line end", "no line end"},
	} {
		if got, err := readHiddenLine(strings.NewReader(c.in)); err != nil || got != c.want {
			t.Errorf("%q: %q %v", c.in, got, err)
		}
	}
	if _, err := readHiddenLine(strings.NewReader("")); !errors.Is(err, io.EOF) {
		t.Errorf("empty input: %v", err)
	}
	// It reads no further than its line.
	r := strings.NewReader("first\r\nsecond\r\n")
	if a, _ := readHiddenLine(r); a != "first" {
		t.Fatalf("first line %q", a)
	}
	if b, _ := readHiddenLine(r); b != "second" {
		t.Fatalf("second line %q", b)
	}
}
