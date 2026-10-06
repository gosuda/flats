package docs

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Minimal test client avoids adding a Go dependency for the app.
type socket struct {
	net.Conn
	reader *bufio.Reader
	inbox  []map[string]any
}

func connect(t *testing.T, url, access string, documents ...string) *socket {
	t.Helper()
	host := strings.TrimPrefix(url, "http://")
	c, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 16)
	rand.Read(key)
	doc := "index.md"
	if len(documents) > 0 {
		doc = documents[0]
	}
	request := "GET /_docs/ws?doc=" + doc + " HTTP/1.1\r\nHost: " + host + "\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(key) + "\r\n"
	if access != "" {
		request += "X-Flats-Access: " + access + "\r\n"
	}
	request += "\r\n"
	if _, err = c.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(c)
	r, err := http.ReadResponse(reader, nil)
	if err != nil || r.StatusCode != 101 {
		t.Fatalf("upgrade: %v %v", r, err)
	}
	s := &socket{Conn: c, reader: reader}
	t.Cleanup(func() { c.Close() })
	return s
}
func (s *socket) write(data []byte) error {
	s.SetWriteDeadline(time.Now().Add(10 * time.Second))
	header := []byte{0x81}
	n := len(data)
	switch {
	case n < 126:
		header = append(header, 0x80|byte(n))
	case n <= 65535:
		header = append(header, 0xfe, byte(n>>8), byte(n))
	default:
		header = append(header, 0xff)
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(n))
		header = append(header, size[:]...)
	}
	var mask [4]byte
	rand.Read(mask[:])
	header = append(header, mask[:]...)
	for i, b := range data {
		header = append(header, b^mask[i%4])
	}
	_, err := s.Write(header)
	return err
}
func (s *socket) send(t *testing.T, m any) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.write(b); err != nil {
		t.Fatal(err)
	}
}
func (s *socket) read() (map[string]any, error) {
	s.SetReadDeadline(time.Now().Add(15 * time.Second))
	var h [2]byte
	if _, err := io.ReadFull(s.reader, h[:]); err != nil {
		return nil, err
	}
	n := int(h[1] & 127)
	if n == 126 {
		var size [2]byte
		if _, err := io.ReadFull(s.reader, size[:]); err != nil {
			return nil, err
		}
		n = int(binary.BigEndian.Uint16(size[:]))
	} else if n == 127 {
		var size [8]byte
		if _, err := io.ReadFull(s.reader, size[:]); err != nil {
			return nil, err
		}
		n = int(binary.BigEndian.Uint64(size[:]))
	}
	if n > 4<<20 {
		return nil, fmt.Errorf("large frame: %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(s.reader, b); err != nil {
		return nil, err
	}
	if h[0]&15 == 8 {
		if len(b) < 2 {
			return nil, fmt.Errorf("invalid close")
		}
		return nil, &socketClose{binary.BigEndian.Uint16(b[:2]), string(b[2:])}
	}
	var m map[string]any
	err := json.Unmarshal(b, &m)
	return m, err
}
func (s *socket) next(t *testing.T, kind string) map[string]any {
	t.Helper()
	for i, m := range s.inbox {
		if m["t"] == kind {
			s.inbox = append(s.inbox[:i], s.inbox[i+1:]...)
			return m
		}
	}
	for {
		m, err := s.read()
		if err != nil {
			t.Fatalf("waiting for %s: %v", kind, err)
		}
		if n, ok := m["n"]; ok {
			s.send(t, map[string]any{"t": "received", "n": n})
		}
		if m["t"] == kind {
			return m
		}
		if m["t"] == "error" {
			t.Fatalf("server error: %v", m)
		}
		s.inbox = append(s.inbox, m)
	}
}
func (s *socket) hello(t *testing.T, known any) map[string]any {
	t.Helper()
	s.send(t, map[string]any{"t": "hello", "v": 1, "doc": "index.md", "sv": "AA==", "name": "Test user", "known": known})
	return s.next(t, "welcome")
}

type socketClose struct {
	code   uint16
	reason string
}

func (c *socketClose) Error() string { return fmt.Sprintf("closed %d: %s", c.code, c.reason) }
func (s *socket) rejected(t *testing.T, code string) {
	t.Helper()
	m, err := s.read()
	if err != nil || m["t"] != "error" || m["code"] != code {
		t.Fatalf("expected error %s, got %v %v", code, m, err)
	}
	s.closed(t, 1008, code)
}
func (s *socket) closed(t *testing.T, code uint16, reason string) {
	t.Helper()
	m, err := s.read()
	c, ok := err.(*socketClose)
	if !ok || c.code != code || c.reason != reason {
		t.Fatalf("expected close %d %s, got %v %v", code, reason, m, err)
	}
}
