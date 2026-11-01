package runtime

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func BenchmarkReadBodyChunked(b *testing.B) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 6000) // 96 KB
	w := &worker{}
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		r := httptest.NewRequest(http.MethodPost, "http://flat.test/", bytes.NewReader(data))
		r.ContentLength = -1
		body, release, err := w.readBody(httptest.NewRecorder(), r)
		if err != nil || len(body) != len(data) {
			b.Fatalf("failed: %v len=%d", err, len(body))
		}
		release()
	}
}

func BenchmarkReadBodyDeclared(b *testing.B) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 6000) // 96 KB
	w := &worker{}
	for b.Loop() {
		r := httptest.NewRequest(http.MethodPost, "http://flat.test/", bytes.NewReader(data))
		r.ContentLength = int64(len(data))
		body, release, err := w.readBody(httptest.NewRecorder(), r)
		if err != nil || len(body) != len(data) {
			b.Fatalf("failed: %v len=%d", err, len(body))
		}
		release()
	}
}
