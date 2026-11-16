package webhook

import (
	"strings"
	"testing"
)

func TestSignVector(t *testing.T) {
	const (
		unix = 1700000000
		body = `{"id":"evt_test","type":"approval.created"}`
		want = "v1=edecf23a0b88e1b94a9e5c0e5534118dcd61eeaf578b04fa0696a8a3f9b79919"
	)
	if got := Sign(testSecret, unix, []byte(body)); got != want {
		t.Fatalf("Sign = %s, want the vector of the specification %s", got, want)
	}
	if got := refSign(testSecret, unix, []byte(body)); got != want {
		t.Fatalf("the recipe computed in the test = %s, want %s", got, want)
	}
}

func TestSignChangesWithEveryInput(t *testing.T) {
	const unix = 1700000000
	body := []byte(`{"id":"evt_test","type":"approval.created"}`)
	base := Sign(testSecret, unix, body)

	changed := append([]byte(nil), body...)
	changed[len(changed)-3] ^= 1
	other := map[string]string{
		"one byte of the body":   Sign(testSecret, unix, changed),
		"the timestamp":          Sign(testSecret, unix+1, body),
		"one byte of the secret": Sign(strings.Replace(testSecret, "0123", "0124", 1), unix, body),
		"a body with a suffix":   Sign(testSecret, unix, append(append([]byte(nil), body...), '\n')),
	}
	for what, sig := range other {
		if sig == base {
			t.Errorf("changing %s did not change the signature", what)
		}
		if !strings.HasPrefix(sig, "v1=") || len(sig) != 3+64 {
			t.Errorf("signature %q for %s is not v1= and 64 hex digits", sig, what)
		}
	}
	// The dot separates the timestamp from the body, so moving a digit across
	// it must not give the same signature.
	if Sign(testSecret, 17, []byte("0.x")) == Sign(testSecret, 170, []byte(".x")) {
		t.Error("the timestamp and the body run together")
	}
}

func TestCheckURL(t *testing.T) {
	long := "https://example.com/" + strings.Repeat("a", maxURL-len("https://example.com/")+1)
	if len(long) != maxURL+1 {
		t.Fatalf("the long URL has %d bytes, want %d", len(long), maxURL+1)
	}
	exact := "https://example.com/" + strings.Repeat("a", maxURL-len("https://example.com/"))

	accepted := []string{
		"https://example.com/hook?token=x",
		"https://user:pw@example.com",
		"https://203.0.113.5:8443/x",
		"http://localhost:8080/x",
		"http://127.0.0.1/x",
		"http://10.1.2.3/x",
		"http://192.168.1.1/x",
		"http://172.16.0.1/x",
		"http://100.64.0.1/x",
		"http://169.254.1.1/x",
		"http://[::1]/x",
		"http://[fd00::1]/x",
		"http://[fe80::1]/x",
		"HTTPS://EXAMPLE.COM",
		"HTTP://LOCALHOST/x",
		"https://example.com:443",
		"https://[2001:db8::1]:8443/x",
		"https://8.8.8.8/x",
		"http://100.127.255.254/x",
		"http://172.31.255.255/x",
		"http://[fe80::1%25eth0]/x",
		exact,
	}
	for _, raw := range accepted {
		if err := CheckURL(raw); err != nil {
			t.Errorf("CheckURL(%q) = %v, want it accepted", clip(raw), err)
		}
	}

	rejected := []string{
		"ftp://x",
		"ftp://example.com/x",
		"http://example.com/x",
		"http://8.8.8.8/x",
		"http://100.128.0.1/x",
		"http://172.32.0.1/x",
		"http://[2001:db8::1]/x",
		"http://[::ffff:8.8.8.8]/x",
		"http://localhost.example.com/x",
		"http://localhost./x",
		"http://127.0.0.1@example.com/x",
		"http://2130706433/x",
		"http://0.0.0.0/x",
		"https://",
		"https:///x",
		"https://:8443/x",
		"https://example.com/x#frag",
		"https://example.com/x#",
		long,
		"not a url",
		"",
		"example.com",
		"//example.com/x",
		"https:example.com",
		"https://exa mple.com",
		"https://example.com/\n",
		"https://example.com:0/x",
		"https://example.com:65536/x",
		"https://example.com:99999999999999999999/x",
		"https://[::1/x",
		"mailto:a@example.com",
	}
	for _, raw := range rejected {
		err := CheckURL(raw)
		if err == nil {
			t.Errorf("CheckURL(%q) was accepted, want an error", clip(raw))
			continue
		}
		// The text must not quote the value, which can hold a token.
		if raw != "" && len(raw) > 4 && strings.Contains(err.Error(), raw) {
			t.Errorf("the error for %q quotes the URL: %v", clip(raw), err)
		}
	}
}

func TestCheckURLErrorHoldsNoToken(t *testing.T) {
	for _, raw := range []string{
		"http://example.com/hook?token=TOKEN-9c1",
		"ftp://example.com/hook?token=TOKEN-9c1",
		"https://example.com/hook?token=TOKEN-9c1#x",
		"https://exa mple.com/hook?token=TOKEN-9c1",
		"https://[::1/hook?token=TOKEN-9c1",
	} {
		err := CheckURL(raw)
		if err == nil {
			t.Fatalf("CheckURL(%q) was accepted", raw)
		}
		if strings.Contains(err.Error(), "TOKEN-9c1") || strings.Contains(err.Error(), "example.com") {
			t.Errorf("the error for %q holds a part of the URL: %v", raw, err)
		}
	}
}

func TestCheckSecret(t *testing.T) {
	if err := CheckSecret(strings.Repeat("s", 15)); err == nil {
		t.Error("a secret of 15 bytes was accepted")
	}
	if err := CheckSecret(""); err == nil {
		t.Error("an empty secret was accepted")
	}
	if err := CheckSecret(strings.Repeat("s", 16)); err != nil {
		t.Errorf("a secret of 16 bytes was refused: %v", err)
	}
	// The limit counts bytes, not characters.
	if err := CheckSecret(strings.Repeat("é", 8)); err != nil {
		t.Errorf("a secret of 16 bytes in 8 characters was refused: %v", err)
	}
	if err := CheckSecret(testSecret); err != nil {
		t.Errorf("the secret of the vector was refused: %v", err)
	}
}

func clip(s string) string {
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
}
