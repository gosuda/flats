package cli

import (
	"fmt"
	"io"
	"unicode/utf8"
)

// safeWriter escapes control and bidi-override characters on their way to
// the terminal. Much of what the CLI prints comes from agents (flat names,
// version messages, approval reasons, health-check bodies, event messages);
// raw escape sequences there could erase or rewrite lines of the operator's
// terminal, for example to disguise an approval request. The CLI's own output
// uses only newlines and tabs, which pass through.
type safeWriter struct {
	w       io.Writer
	pending []byte // an incomplete UTF-8 sequence left by the previous Write
}

func newSafeWriter(w io.Writer) io.Writer {
	if _, ok := w.(*safeWriter); ok {
		return w
	}
	return &safeWriter{w: w}
}

func (s *safeWriter) Write(p []byte) (int, error) {
	buf := p
	if len(s.pending) > 0 {
		buf = append(s.pending, p...)
		s.pending = nil
	}
	out := make([]byte, 0, len(buf))
	for len(buf) > 0 {
		r, size := utf8.DecodeRune(buf)
		if r == utf8.RuneError && size <= 1 {
			if !utf8.FullRune(buf) {
				s.pending = append([]byte(nil), buf...)
				break
			}
			out = fmt.Appendf(out, `\x%02x`, buf[0])
			buf = buf[1:]
			continue
		}
		if unsafeRune(r) {
			if r < 0x100 {
				out = fmt.Appendf(out, `\x%02x`, r)
			} else {
				out = fmt.Appendf(out, `\u%04x`, r)
			}
		} else {
			out = append(out, buf[:size]...)
		}
		buf = buf[size:]
	}
	if _, err := s.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

func unsafeRune(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case r < 0x20 || r == 0x7f: // C0 controls (ESC, CR, BS, ...) and DEL
		return true
	case r >= 0x80 && r <= 0x9f: // C1 controls, e.g. the 8-bit CSI
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069: // bidi overrides
		return true
	}
	return false
}
