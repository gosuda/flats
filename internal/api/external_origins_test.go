package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExternalOriginGuard(t *testing.T) {
	origins := []string{"https://flats.example.org", "https://other.example.org:8443", "http://private.example.org"}
	g := &HostGuard{Hosts: func() []string { return []string{"localhost"} }, Ports: []string{"7878"}, ExternalOrigins: func() []string { return origins }, Next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accepted-Origin", ExternalOrigin(r.Context()))
		w.WriteHeader(200)
	})}
	for _, tc := range []struct {
		host, origin string
		want         int
	}{
		{"flats.example.org", "", 200}, {"flats.example.org", "https://flats.example.org", 200},
		{"flats.example.org:443", "https://flats.example.org", 200},
		{"flats.example.org", "https://flats.example.org:443", 200},
		{"flats.example.org:8443", "", 403}, {"evil.example.org", "", 403},
		{"flats.example.org", "http://flats.example.org", 403},
		{"flats.example.org", "https://other.example.org:8443", 403},
		{"flats.example.org", "https://flats.example.org?", 403},
		{"flats.example.org", "https://flats.example.org#", 403},
		{"flats.example.org", "https://user@flats.example.org", 403},
		{"flats.example.org", "https://flats.example.org/", 403},
		{"flats.example.org", "null", 403},
		{"localhost:7878", "https://flats.example.org", 403},
		{"private.example.org", "http://private.example.org", 200},
	} {
		r := httptest.NewRequest("GET", "http://"+tc.host+"/", nil)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-Forwarded-Host", "evil.example.org")
		r.Header.Set("X-Forwarded-Proto", "ftp")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("Host %s Origin %s: got %d want %d", tc.host, tc.origin, w.Code, tc.want)
		}
		if tc.want == 200 && tc.host == "flats.example.org" && w.Header().Get("Accepted-Origin") != origins[0] {
			t.Fatal("trusted origin lost")
		}
	}
	r := httptest.NewRequest("GET", "http://flats.example.org/", nil)
	r.Header.Add("Origin", origins[0])
	r.Header.Add("Origin", origins[1])
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("multiple Origin headers accepted")
	}
	origins = nil
	w = httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("GET", "http://flats.example.org/", nil))
	if w.Code != 403 {
		t.Fatal("removed origin still accepted")
	}
}

func TestExternalHTTPConsoleMutation(t *testing.T) {
	g := &HostGuard{ExternalOrigins: func() []string { return []string{"http://private.example.org"} }, Next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := consoleRequest(r); err != nil {
			http.Error(w, err.Error(), 403)
			return
		}
		w.WriteHeader(200)
	})}
	for _, header := range []string{"", "1"} {
		r := httptest.NewRequest("PUT", "http://private.example.org/console/api/settings", nil)
		r.Header.Set("Origin", "http://private.example.org")
		r.Header.Set("X-Flats-Console", header)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		want := 403
		if header == "1" {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("console header %q: %d", header, w.Code)
		}
	}
}
