package wsconn

import (
	"bufio"
	"net"
	"testing"
	"time"
)

func TestLiveAbortDiscardsQueuedMessages(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close bool
	}{
		{"abort", false},
		{"interrupt graceful close", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			ws := newConn(a, bufio.NewReader(a), false)
			ws.sharedBudget = new(Budget)
			for _, message := range []string{"content one", "content two"} {
				if err := ws.Send(message); err != nil {
					t.Fatal(err)
				}
			}
			if tc.close {
				ws.Close(1008, "access_changed")
			}
			ws.Abort()
			go ws.writer()
			var buf [1]byte
			if n, err := b.Read(buf[:]); n != 0 || err == nil {
				t.Fatalf("abort delivered queued bytes: %d, %v", n, err)
			}
			select {
			case <-ws.writerDone:
			case <-time.After(time.Second):
				t.Fatal("writer did not discard its queue")
			}
			select {
			case <-ws.Done():
			case <-time.After(time.Second):
				t.Fatal("abort did not complete")
			}
			if ws.Budget().Used() != 0 || ws.sharedBudget.Used() != 0 {
				t.Fatal("abort leaked queue budget")
			}
			if err := ws.Send("later content"); err == nil {
				t.Fatal("abort accepted more content")
			}
		})
	}
}
