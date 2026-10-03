package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testOperatorCredential = "independent-test-only-credential-32-bytes"

func browserRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("X-Flats-Console", "1")
	r.Header.Set("Origin", "http://localhost")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Content-Type", "application/json")
	return r
}

func testAuthority(t *testing.T) *OperatorAuthority {
	t.Helper()
	a, err := NewOperatorAuthority(testOperatorCredential)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func establishSession(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	w := httptest.NewRecorder()
	s.operatorSession(w, browserRequest("POST", "/console/api/operator/session", `{"credential":"`+testOperatorCredential+`"}`))
	if w.Code != 200 {
		t.Fatalf("session: %d %s", w.Code, w.Body)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/console/api" {
		t.Fatalf("session cookie attributes: %v", cookies)
	}
	if strings.Contains(w.Body.String(), cookies[0].Value) || strings.Contains(w.Body.String(), testOperatorCredential) {
		t.Fatal("session response leaked authority")
	}
	return cookies[0]
}

func requireCategory(t *testing.T, w *httptest.ResponseRecorder, category string) {
	t.Helper()
	var b ErrorBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if w.Code != 403 || b.Category != category {
		t.Fatalf("want 403 %s, got %d %+v", category, w.Code, b)
	}
}

func TestOperatorRoutesFailClosedAgainstForgedBrowserHeaders(t *testing.T) {
	for _, configured := range []bool{false, true} {
		s := &Server{}
		category := "operator_unavailable"
		if configured {
			s.Operator = testAuthority(t)
			category = "operator_required"
		}
		// Nil service is deliberate: unauthorized requests must never reach core.
		for _, route := range []struct{ method, path string }{
			{"POST", "/console/api/approvals/apr-test/approve"},
			{"POST", "/console/api/approvals/apr-test/reject"},
			{"PUT", "/console/api/settings"},
			{"DELETE", "/console/api/flats/test"},
		} {
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, browserRequest(route.method, route.path, `{}`))
			requireCategory(t, w, category)
		}
	}
}

func TestOperatorAuthorityCannotBeSelfGranted(t *testing.T) {
	s := &Server{Operator: testAuthority(t)}
	for _, credential := range []string{"", "agent-credential", testOperatorCredential + "wrong"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, browserRequest("POST", "/console/api/operator/session", `{"credential":"`+credential+`"}`))
		requireCategory(t, w, "invalid_operator_credential")
		if len(w.Result().Cookies()) != 0 {
			t.Fatal("invalid credential issued cookie")
		}
	}
	for _, path := range []string{"/console/api/operator/session", "/api/operator/session"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, browserRequest("GET", path, ""))
		if w.Code != 404 && w.Code != 405 {
			t.Fatalf("GET bootstrap %s: %d", path, w.Code)
		}
		if len(w.Result().Cookies()) != 0 {
			t.Fatal("GET issued authority")
		}
	}
	if err := s.Operator.ValidateDecision(context.Background()); err == nil {
		t.Fatal("agent context acquired decision authority")
	}
}

func TestOperatorSessionBoundRevokedExpiredAndRestarted(t *testing.T) {
	s := &Server{Operator: testAuthority(t)}
	cookie := establishSession(t, s)
	r := browserRequest("POST", "/console/api/approvals/apr-test/approve", `{}`)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	if !s.requireOperator(w, r) || s.Operator.ValidateDecision(r.Context()) != nil {
		t.Fatalf("legitimate session was refused: %s", w.Body)
	}
	other := testAuthority(t)
	if other.ValidateDecision(r.Context()) == nil {
		t.Fatal("proof crossed authority/restart boundary")
	}
	logout := browserRequest("DELETE", "/console/api/operator/session", "")
	logout.AddCookie(cookie)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, logout)
	if w.Code != 200 || s.Operator.ValidateDecision(r.Context()) == nil {
		t.Fatal("logout did not revoke outstanding context proof")
	}
	cookie = establishSession(t, s)
	r = browserRequest("POST", "/console/api/approvals/apr-test/reject", `{}`)
	r.AddCookie(cookie)
	if !s.Operator.authorize(r) {
		t.Fatal("positive control session rejected")
	}
	for hash := range s.Operator.sessions {
		s.Operator.sessions[hash] = time.Now().Add(-time.Second)
	}
	if s.Operator.ValidateDecision(r.Context()) == nil || s.Operator.authorize(r) {
		t.Fatal("expired session authorized decision")
	}
}

func TestOperatorSessionDoesNotOverrideCSRFOrAgentClient(t *testing.T) {
	s := &Server{Operator: testAuthority(t)}
	cookie := establishSession(t, s)
	for _, header := range []string{"X-Flats-Client", "Origin", "Sec-Fetch-Site"} {
		r := browserRequest("POST", "/console/api/approvals/apr-test/approve", `{}`)
		r.AddCookie(cookie)
		r.Header.Set(header, map[string]string{"X-Flats-Client": "cli", "Origin": "https://attacker.invalid", "Sec-Fetch-Site": "cross-site"}[header])
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("valid cookie bypassed %s: %d", header, w.Code)
		}
	}
}

func TestOperatorCredentialRefusesRemotePlaintext(t *testing.T) {
	s := &Server{Operator: testAuthority(t)}
	r := browserRequest("POST", "/console/api/operator/session", `{"credential":"`+testOperatorCredential+`"}`)
	r.RemoteAddr = "100.64.0.7:12345"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	requireCategory(t, w, "operator_secure_transport_required")
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("remote plaintext login issued authority")
	}
}
