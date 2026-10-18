package app

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestManagementConsoleURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		addr string
		want string
	}{
		{"wildcard IPv4", "0.0.0.0:7878", "http://127.0.0.1:7878"},
		{"wildcard IPv6", "[::]:17878", "http://127.0.0.1:17878"},
		{"loopback IPv4", "127.0.0.1:7878", "http://127.0.0.1:7878"},
		{"loopback IPv6", "[::1]:7878", "http://[::1]:7878"},
		{"explicit IPv4", "192.0.2.1:7878", "http://192.0.2.1:7878"},
		{"explicit IPv6", "[2001:db8::1]:7878", "http://[2001:db8::1]:7878"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			addr, err := net.ResolveTCPAddr("tcp", tc.addr)
			if err != nil {
				t.Fatal(err)
			}
			got := managementConsoleURL(addr)
			if got != tc.want {
				t.Fatalf("console URL = %q, want %q", got, tc.want)
			}
			// Generated console/approval links must pass the actual Host guard.
			handler := loopbackGuard(addr, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, got+"/approvals/example", nil))
			if rr.Code != http.StatusNoContent {
				t.Fatalf("generated link rejected: %d %s", rr.Code, rr.Body.String())
			}
		})
	}
}
