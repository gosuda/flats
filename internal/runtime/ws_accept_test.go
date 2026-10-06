package runtime

import "testing"

// Independent RFC 6455 section 4.2.2 vector protects the wire algorithm;
// server/client round trips alone could both adopt an incompatible hash.
func TestWSAcceptRFC6455(t *testing.T) {
	const key = "dGhlIHNhbXBsZSBub25jZQ=="
	const want = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	if got := wsAccept(key); got != want {
		t.Fatalf("Sec-WebSocket-Accept = %q, want %q", got, want)
	}
}
