package wsconn

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestCloseBoundsReasonAtUTF8Boundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		want   string
	}{
		{"empty", "", ""},
		{"short", "bye", "bye"},
		{"at limit", strings.Repeat("a", 123), strings.Repeat("a", 123)},
		{"ascii over limit", strings.Repeat("a", 124), strings.Repeat("a", 123)},
		{"three byte boundary", strings.Repeat("a", 121) + "한", strings.Repeat("a", 121)},
		{"four byte boundary", strings.Repeat("a", 122) + "📝", strings.Repeat("a", 122)},
		{"complete final rune", strings.Repeat("a", 120) + "한x", strings.Repeat("a", 120) + "한"},
		{"large reason", strings.Repeat("a", 1<<20), strings.Repeat("a", 123)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			ws := newConn(a, bufio.NewReader(a), false)
			go ws.writer()
			ws.Close(1008, tc.reason)
			if err := b.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			var header [2]byte
			if _, err := io.ReadFull(b, header[:]); err != nil {
				t.Fatalf("close frame header: %v", err)
			}
			if header[0] != 0x80|OpClose || header[1] < 2 || header[1] > 125 {
				t.Fatalf("invalid close frame header: %x", header)
			}
			payload := make([]byte, int(header[1]))
			if _, err := io.ReadFull(b, payload); err != nil {
				t.Fatal(err)
			}
			if binary.BigEndian.Uint16(payload[:2]) != 1008 || string(payload[2:]) != tc.want || !utf8.Valid(payload[2:]) {
				t.Fatalf("close payload = %q, want code 1008 and %q", payload, tc.want)
			}
			<-ws.Done()
			if code, reason := ws.CloseStatus(); code != 1008 || reason != tc.want {
				t.Fatalf("close status = %d %q", code, reason)
			}
		})
	}
}
