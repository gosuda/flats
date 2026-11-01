package api_test

import (
	"bytes"
	"errors"
	"io"
	"net/http/httptest"
	"testing"
	"testing/iotest"
)

func TestUploadBodyReadFailureIsServerError(t *testing.T) {
	srv, _ := setup(t)
	raw := archive(map[string]string{"index.html": "ok"})
	for name, body := range map[string][]byte{"mid archive": raw[:len(raw)/2], "after archive": raw} {
		r := httptest.NewRequest("POST", "/api/flats/site/draft", io.MultiReader(bytes.NewReader(body), iotest.ErrReader(errors.New("connection reset"))))
		r.Header.Set("X-Flats-Client", "test")
		w := httptest.NewRecorder()
		srv.Config.Handler.ServeHTTP(w, r)
		if w.Code != 500 || !bytes.Contains(w.Body.Bytes(), []byte("connection reset")) {
			t.Fatalf("%s: want 500 with the read error, got %d %s", name, w.Code, w.Body)
		}
	}
}
