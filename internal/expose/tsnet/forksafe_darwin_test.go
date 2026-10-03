package tsnet

import (
	"testing"

	"tailscale.com/safesocket"
)

// Regression (real-tailnet gate): after New, tailscale's LocalAPI token
// lookup answers without running lsof (a fork that can wedge on macOS).
func TestMacTokenLookupDoesNotFork(t *testing.T) {
	if _, err := New(Config{Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	port, token, err := safesocket.LocalTCPPortAndToken()
	if err != nil || port != 1 || token != "flats-in-process" {
		t.Fatalf("token lookup = %d %q %v; want the fixed in-process credentials", port, token, err)
	}
}
