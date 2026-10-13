package runtime

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Tests that change bodyGrace are not parallel.

func TestWorkerBodyBudgetRefusesOnlyBodies(t *testing.T) {
	t.Parallel()
	w := &worker{}
	w.bodies.used = maxBufferedBodies - 5
	rw := httptest.NewRecorder()
	w.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "http://flat.test/", strings.NewReader("0123456789")))
	if rw.Code != http.StatusServiceUnavailable || rw.Header().Get("Retry-After") == "" || rw.Header().Get("Connection") != "close" {
		t.Fatalf("over budget: %d %v", rw.Code, rw.Header())
	}
	// A chunked body is counted as it arrives.
	r := httptest.NewRequest(http.MethodPost, "http://flat.test/", io.MultiReader(strings.NewReader("abc"), strings.NewReader("defgh")))
	r.ContentLength = -1
	if _, release, err := w.readBody(httptest.NewRecorder(), r); err != errBodiesBusy {
		t.Fatalf("chunked over budget: %v", err)
	} else {
		release()
	}
	if w.bodies.used != maxBufferedBodies-5 {
		t.Fatalf("budget leaked: %d", maxBufferedBodies-w.bodies.used)
	}
	// Requests without a body never need the budget.
	body, release, err := w.readBody(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://flat.test/", nil))
	release()
	if err != nil || len(body) != 0 {
		t.Fatalf("GET: %q %v", body, err)
	}
	w.bodies.used = 0
	r = httptest.NewRequest(http.MethodPost, "http://flat.test/", strings.NewReader("abcde"))
	r.ContentLength = -1
	body, release, err = w.readBody(httptest.NewRecorder(), r)
	if err != nil || string(body) != "abcde" || w.bodies.used != 5 {
		t.Fatalf("chunked: %q %v used %d", body, err, w.bodies.used)
	}
	release()
	if w.bodies.used != 0 {
		t.Fatalf("not released: %d", w.bodies.used)
	}
}

func TestWorkerRejectsDeclaredOversizeBodyUnread(t *testing.T) {
	t.Parallel()
	w := &worker{}
	r := httptest.NewRequest(http.MethodPost, "http://flat.test/", nil)
	r.Body = io.NopCloser(readFunc(func([]byte) (int, error) { t.Fatal("body read"); return 0, nil }))
	r.ContentLength = MaxRequestBody + 1
	rw := httptest.NewRecorder()
	w.ServeHTTP(rw, r)
	if rw.Code != http.StatusRequestEntityTooLarge || w.bodies.used != 0 {
		t.Fatalf("status %d, used %d", rw.Code, w.bodies.used)
	}
}

type readFunc func([]byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }

// bodyServer serves readBody over a real connection and reports each result.
func bodyServer(t *testing.T, after func(r *http.Request)) (*worker, string) {
	t.Helper()
	w := &worker{}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, release, err := w.readBody(rw, r)
		defer release()
		if err != nil {
			rw.Header().Set("Connection", "close")
			http.Error(rw, err.Error(), http.StatusRequestTimeout)
			return
		}
		if after != nil {
			after(r)
		}
		fmt.Fprintf(rw, "%d", len(body))
	}))
	t.Cleanup(srv.Close)
	return w, strings.TrimPrefix(srv.URL, "http://")
}

func sendSlowly(t *testing.T, addr string, chunks int, size int, gap time.Duration) (int, string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: flat.test\r\nContent-Length: %d\r\n\r\n", chunks*size)
	for i := 0; i < chunks; i++ {
		time.Sleep(gap)
		if _, err := conn.Write(bytes.Repeat([]byte("u"), size)); err != nil {
			break // the server gave up
		}
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestRequestBodyMinimumRate(t *testing.T) {
	old := bodyGrace
	bodyGrace = 300 * time.Millisecond
	defer func() { bodyGrace = old }()
	w, addr := bodyServer(t, nil)

	// A stalled body times out after the grace period.
	if code, _ := sendSlowly(t, addr, 1, 10, 3*time.Second); code != http.StatusRequestTimeout {
		t.Fatalf("stalled body: %d", code)
	}
	// An upload that keeps arriving at the minimum rate may take longer than
	// the grace period: 5 x 16 KiB over 1 s is about 80 KiB/s.
	if code, body := sendSlowly(t, addr, 5, 16<<10, 200*time.Millisecond); code != http.StatusOK || body != fmt.Sprint(80<<10) {
		t.Fatalf("steady upload: %d %q", code, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for w.bodies.used != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if w.bodies.used != 0 {
		t.Fatalf("budget not released: %d", w.bodies.used)
	}
}

// After the body is read, its deadline must not end the request's context
// while the handler still runs.
func TestRequestBodyDeadlineCleared(t *testing.T) {
	old := bodyGrace
	bodyGrace = 100 * time.Millisecond
	defer func() { bodyGrace = old }()
	var ctxErr error
	_, addr := bodyServer(t, func(r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		ctxErr = r.Context().Err()
	})
	if code, _ := sendSlowly(t, addr, 1, 10, 0); code != http.StatusOK || ctxErr != nil {
		t.Fatalf("status %d, context %v", code, ctxErr)
	}
}
