package sheet

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestStalledResponseEnds sends the requests of a CSV download and of a
// History tab over a connection that then reads nothing. Each response
// fills the connection and its write waits. Once a write has waited for
// responseWriteWait the response ends: its capture gives its bytes back and
// the request no longer keeps the sheet in use.
func TestStalledResponseEnds(t *testing.T) {
	old := responseWriteWait
	responseWriteWait = 300 * time.Millisecond
	t.Cleanup(func() { responseWriteWait = old })
	e := start(t)
	// 100 cells of 32,767 four-byte characters: 13 MB of CSV, more than
	// the buffers of a connection hold.
	cell := strings.Repeat("\U0001F600", 32767)
	for row := 1; row <= 100; row += 25 {
		list := make([]string, 25)
		for i := range list {
			list[i] = `["` + cell + `"]`
		}
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, row, strings.Join(list, ",")))
	}
	rev := e.b.importAll(t)
	tab := e.tabID("Sheet1")
	s := serve(e)
	host := strings.TrimPrefix(s.server.URL, "http://")
	// The revision workbook is calculated before the stalled request.
	for {
		res, err := http.Get(fmt.Sprintf("%s/private/_sheet/api/revisions/%d/tabs/%d", s.server.URL, rev, tab))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, c := range []struct{ name, path string }{
		{"download", fmt.Sprintf("/private/_sheet/csv?tab=%d", tab)},
		{"History", fmt.Sprintf("/private/_sheet/api/revisions/%d/tabs/%d", rev, tab)},
	} {
		t.Run(c.name, func(t *testing.T) {
			base := captureBytes.Load()
			conn, err := net.Dial("tcp", host)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", c.path, host); err != nil {
				t.Fatal(err)
			}
			held := false
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				charged, inUse := captureBytes.Load()-base, e.in.inflight.Load()
				held = held || charged > 0
				if held && charged == 0 && inUse == 0 {
					return
				}
				if time.Now().After(deadline) {
					if !held {
						t.Fatal("the response never held a capture")
					}
					t.Fatalf("10 s after the request %d bytes are charged and %d requests are in flight", charged, inUse)
				}
			}
		})
	}
}
