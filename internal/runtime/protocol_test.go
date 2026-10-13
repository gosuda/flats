package runtime

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"testing"
)

func readLogMessages(t *testing.T, b []byte) []childMsg {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(b))
	var out []childMsg
	for {
		var msg childMsg
		if err := d.Decode(&msg); err != nil {
			if err == io.EOF {
				return out
			}
			t.Fatal(err)
		}
		out = append(out, msg)
	}
}

func TestChildLogOmitsLongMessagesWithoutPrefix(t *testing.T) {
	const prefix = "sensitive-secret-prefix"
	for _, size := range []int{1, maxLogLine - 1, maxLogLine, maxLogLine + 1, 64 << 10} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			secret := strings.Repeat("z", size)
			if size >= len(prefix) {
				secret = prefix + strings.Repeat("z", size-len(prefix))
			}
			var raw bytes.Buffer
			log := newChildLog(&raw)
			log.Log("info", secret)
			got := readLogMessages(t, raw.Bytes())
			if len(got) != 1 {
				t.Fatalf("message count = %d", len(got))
			}
			want := secret
			if size > maxLogLine {
				want = omittedLogLine
			}
			if got[0].Msg != want {
				t.Fatalf("log = %q, want %q", got[0].Msg, want)
			}
			if size > maxLogLine && strings.Contains(raw.String(), prefix) {
				t.Fatal("partial secret leaked")
			}
		})
	}
}

func TestLineWriterDropsWholeOversizedLine(t *testing.T) {
	const prefix = "sensitive-secret-prefix"
	secret := prefix + strings.Repeat("z", (64<<10)-len(prefix))
	for _, chunk := range []int{1, maxLogLine - 1, maxLogLine + 1, len(secret) + 10} {
		var raw bytes.Buffer
		log := newChildLog(&raw)
		w := &lineWriter{log: log, level: "info"}
		for i := 0; i < len(secret); i += chunk {
			end := i + chunk
			if end > len(secret) {
				end = len(secret)
			}
			n, err := w.Write([]byte(secret[i:end]))
			if err != nil || n != end-i {
				t.Fatalf("Write = %d, %v", n, err)
			}
		}
		if raw.Len() != 0 {
			t.Fatal("partial overlong line was logged before newline")
		}
		if _, err := w.Write([]byte("\r\nokay\r\n")); err != nil {
			t.Fatal(err)
		}
		w.Flush()
		got := readLogMessages(t, raw.Bytes())
		if len(got) != 2 || got[0].Msg != omittedLogLine || got[1].Msg != "okay" {
			t.Fatalf("chunk %d: log = %#v", chunk, got)
		}
		if strings.Contains(raw.String(), prefix) {
			t.Fatalf("chunk %d leaked secret", chunk)
		}
	}
}

func TestLineWriterFlushDropsOverlongUnterminatedLine(t *testing.T) {
	var raw bytes.Buffer
	w := &lineWriter{log: newChildLog(&raw), level: "error"}
	secret := strings.Repeat("S", 64<<10)
	w.Write([]byte(secret))
	w.Flush()
	w.Write([]byte("safe\n"))
	got := readLogMessages(t, raw.Bytes())
	if len(got) != 2 || got[0].Msg != omittedLogLine || got[1].Msg != "safe" {
		t.Fatalf("log = %#v", got)
	}
	if strings.Contains(raw.String(), "SSSSSSSS") {
		t.Fatal("partial secret leaked")
	}
}

func TestLineWriterAcceptsBoundedLines(t *testing.T) {
	for _, size := range []int{maxLogLine - 1, maxLogLine} {
		var raw bytes.Buffer
		w := &lineWriter{log: newChildLog(&raw), level: "info"}
		msg := strings.Repeat("y", size)
		w.Write([]byte(msg[:3]))
		w.Write([]byte(msg[3:] + "\n"))
		got := readLogMessages(t, raw.Bytes())
		if len(got) != 1 || got[0].Msg != msg {
			t.Fatalf("size %d: %v", size, got)
		}
	}
}
