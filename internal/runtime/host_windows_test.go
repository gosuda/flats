package runtime

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/ostest"
)

// A Unix socket on Windows can miss the end of its stream, so the proxy must not
// wait for a worker after the visitor left. The worker here never reacts, as one
// that missed the end.
func TestProxyEndsWithItsVisitorWhenTheWorkerMissedTheEnd(t *testing.T) {
	dir, err := os.MkdirTemp(ostest.TempRoot(), "proxy-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "w.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	defer close(release)
	worker := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		<-release
	})}
	go worker.Serve(l)
	defer worker.Close()
	in := &instance{sock: sock}
	in.connect()
	ended := make(chan struct{})
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(ended)
		in.serveChild(w, r)
	}))
	defer front.Close()
	visitor, err := net.Dial("tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer visitor.Close()
	io.WriteString(visitor, "GET / HTTP/1.1\r\nHost: flat\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	response, err := http.ReadResponse(bufio.NewReader(visitor), nil)
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", response, err)
	}
	visitor.Close()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy still waits for the worker after its visitor left")
	}
}

func TestFilesKeysThatWindowsCannotStore(t *testing.T) {
	for _, key := range []string{"NUL", "a/COM1", "dot.", "space ", "a:b", "LONGFI~1.TXT", "a?b"} {
		if err := checkKey(key); err == nil {
			t.Errorf("%q was accepted", key)
		}
	}
	for _, key := range []string{"a", "keys/a.txt", "nullable", "com.json"} {
		if err := checkKey(key); err != nil {
			t.Errorf("%q: %v", key, err)
		}
	}
}

// The worker's idle timeout ends a connection whose close it missed. It must
// stay above the host's, or the host could reuse a connection as the worker ends it.
func TestWorkerClosesAnIdleConnectionAfterTheHost(t *testing.T) {
	t.Parallel()
	if idle := newWorkerServer(nil).IdleTimeout; idle <= workerIdleClose {
		t.Fatalf("idle timeout %v, and the host closes after %v", idle, workerIdleClose)
	}
}
