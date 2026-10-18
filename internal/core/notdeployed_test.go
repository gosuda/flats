package core

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The "not deployed" page escapes the slug it names. Valid slugs hold no
// markup, but the page must not rely on that.
func TestNotDeployedPageEscapesSlug(t *testing.T) {
	s, _ := newTestService(t)
	rec := httptest.NewRecorder()
	s.siteHandler(`a<script>"x"`, false).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<script>") || !strings.Contains(body, "a&lt;script&gt;&#34;x&#34;") {
		t.Fatalf("slug not escaped: %s", body)
	}
}
