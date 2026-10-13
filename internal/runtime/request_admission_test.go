package runtime

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type forbiddenRequestBody struct{ reads int }

func (b *forbiddenRequestBody) Read([]byte) (int, error) {
	b.reads++
	return 0, errors.New("body must not be read before admission")
}

func (*forbiddenRequestBody) Close() error { return nil }

func TestWorkerRejectsBeforeReadingBody(t *testing.T) {
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	w := &worker{spec: workerSpec{TimeoutMS: 1}, admission: slots}
	body := &forbiddenRequestBody{}
	r := httptest.NewRequest(http.MethodPost, "http://flat.test/", nil)
	r.Body = body
	rw := httptest.NewRecorder()
	w.ServeHTTP(rw, r)
	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("saturated worker status = %d, want 503", rw.Code)
	}
	if body.reads != 0 {
		t.Fatalf("body was read %d times before admission", body.reads)
	}
}

func TestManagerRejectsBeforeWorkerStart(t *testing.T) {
	m := &Manager{pending: make(chan struct{}, 1)}
	in := &instance{m: m, pending: make(chan struct{}, 1)}
	in.pending <- struct{}{}
	rw := httptest.NewRecorder()
	in.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "http://flat.test/", nil))
	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("saturated flat status = %d, want 503", rw.Code)
	}

	// Global admission must also reject without starting a worker.
	<-in.pending
	m.pending <- struct{}{}
	rw = httptest.NewRecorder()
	in.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "http://flat.test/", nil))
	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("saturated manager status = %d, want 503", rw.Code)
	}
}

func TestManagerWebSocketAdmissionIsSeparate(t *testing.T) {
	m := &Manager{websockets: make(chan struct{}, 1)}
	in := &instance{m: m, websockets: make(chan struct{}, 1)}
	in.websockets <- struct{}{}
	r := httptest.NewRequest(http.MethodGet, "http://flat.test/", nil)
	r.Header.Set("Connection", "upgrade")
	r.Header.Set("Upgrade", "websocket")
	rw := httptest.NewRecorder()
	in.ServeHTTP(rw, r)
	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("saturated WebSocket status = %d, want 503", rw.Code)
	}
}

func TestRequestBodyReadDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := readRequestBody(w, r, 100*time.Millisecond)
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			http.Error(w, "body read did not time out", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusRequestTimeout)
	}))
	defer server.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "POST / HTTP/1.1\r\nHost: flat.test\r\nContent-Length: 10\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("stalled body status = %d, want 408", resp.StatusCode)
	}
}
