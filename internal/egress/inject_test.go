package egress

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// The checks of docs/spec/injected-fetch-secrets.md for the outbound client. One TLS server answers for every host
// name of the test, with a certificate that the client trusts through its roots seam, and one plain server answers
// on port 80. The resolver and the dialer seams send every request to them, so no test needs the network.

const injectedValue = "Bearer injected-fixture-7f3a"

var injectTestNames = []string{"api.example.com", "eu.api.example.com", "api.example.com.evil.test", "other.example.com"}

type seenRequest struct {
	host, path, auth, extra, key string
}

type injectFixture struct {
	client *Client
	dials  atomic.Int32
	mu     sync.Mutex
	seen   []seenRequest
}

func (f *injectFixture) requests() []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenRequest(nil), f.seen...)
}

func testCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "injected secrets test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true, DNSNames: injectTestNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// newInjectFixture starts the servers and a client for origins with the injected secret of Authorization at
// https://api.example.com.
// With no headers, the list is the one injected secret of Authorization at https://api.example.com.
func newInjectFixture(t *testing.T, origins []string, headers ...Header) *injectFixture {
	t.Helper()
	f := &injectFixture{}
	record := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, seenRequest{host: r.Host, path: r.URL.Path, auth: strings.Join(r.Header.Values("Authorization"), "|"), extra: r.Header.Get("X-Fill"), key: strings.Join(r.Header.Values("X-Api-Key"), "|")})
		f.mu.Unlock()
		if r.URL.Path == "/redirect" {
			w.Header().Set("Location", "https://other.example.com/next")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.Write([]byte("ok"))
	})
	cert, pool := testCertificate(t)
	tlsServer := httptest.NewUnstartedServer(record)
	tlsServer.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	tlsServer.StartTLS()
	t.Cleanup(tlsServer.Close)
	plain := httptest.NewServer(record)
	t.Cleanup(plain.Close)
	if len(headers) == 0 {
		headers = []Header{{Origin: "https://api.example.com", Name: "Authorization", Value: injectedValue}}
	}
	c, err := New(origins, headers...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	c.roots = pool
	c.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	c.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		f.dials.Add(1)
		to := plain.Listener.Addr().String()
		if strings.HasSuffix(address, ":443") {
			to = tlsServer.Listener.Addr().String()
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", to)
	}
	f.client = c
	return f
}

var allInjectOrigins = []string{"https://api.example.com", "http://api.example.com", "https://eu.api.example.com", "https://api.example.com.evil.test", "https://other.example.com"}

func TestInjectedHeaderGoesOnlyToItsOrigin(t *testing.T) {
	t.Parallel()
	f := newInjectFixture(t, allInjectOrigins)
	ctx := context.Background()
	for _, url := range []string{"https://api.example.com/v1", "https://API.example.com/v1", "https://api.example.com:443/v1"} {
		if _, err := f.client.Fetch(ctx, Request{URL: url}); err != nil {
			t.Fatalf("%s: %v", url, err)
		}
	}
	for _, url := range []string{"http://api.example.com/v1", "https://eu.api.example.com/v1", "https://api.example.com.evil.test/v1", "https://other.example.com/v1"} {
		if _, err := f.client.Fetch(ctx, Request{URL: url}); err != nil {
			t.Fatalf("%s: %v", url, err)
		}
	}
	got := f.requests()
	if len(got) != 7 {
		t.Fatalf("requests: %+v", got)
	}
	for i, r := range got {
		bound := i < 3
		if bound && (r.host != "api.example.com" || r.auth != injectedValue) {
			t.Errorf("request %d to the bound origin: %+v, want the header with the stored bytes", i, r)
		}
		if !bound && r.auth != "" {
			t.Errorf("request %d to %s got the injected header", i, r.host)
		}
	}
}

func TestInjectedHeaderNeedsTheNetworkGrant(t *testing.T) {
	t.Parallel()
	f := newInjectFixture(t, []string{"https://other.example.com"})
	if _, err := f.client.Fetch(context.Background(), Request{URL: "https://api.example.com/v1"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("bound origin without a grant: %v", err)
	}
	if n := f.dials.Load(); n != 0 {
		t.Fatalf("the client dialed %d times", n)
	}
	if _, err := f.client.Fetch(context.Background(), Request{URL: "https://other.example.com/v1"}); err != nil {
		t.Fatal(err)
	}
	if got := f.requests(); len(got) != 1 || got[0].auth != "" {
		t.Fatalf("requests: %+v", got)
	}
}

func TestSuppliedBoundHeaderFailsBeforeTheLimitAndTheDial(t *testing.T) {
	t.Parallel()
	f := newInjectFixture(t, allInjectOrigins)
	// One token: a refused call that took it would make the call after it fail.
	f.client.limiter = rate.NewLimiter(0, 1)
	for _, name := range []string{"Authorization", "authorization", "AUTHORIZATION"} {
		_, err := f.client.Fetch(context.Background(), Request{URL: "https://api.example.com/v1", Headers: map[string][]string{name: {"Bearer flat-code"}}})
		var host *HostHeaderError
		if !errors.As(err, &host) || !errors.Is(err, ErrInvalid) || host.Name != "Authorization" {
			t.Fatalf("%s: %v", name, err)
		}
		if msg := err.Error(); !strings.Contains(msg, "Authorization") || strings.Contains(msg, injectedValue) || strings.Contains(msg, "flat-code") {
			t.Fatalf("%s: message %q", name, msg)
		}
	}
	if n := f.dials.Load(); n != 0 {
		t.Fatalf("the client dialed %d times", n)
	}
	// The same header goes to another origin as flat code gave it.
	if _, err := f.client.Fetch(context.Background(), Request{URL: "https://other.example.com/v1", Headers: map[string][]string{"Authorization": {"Bearer flat-code"}}}); err != nil {
		t.Fatal(err)
	}
	if got := f.requests(); len(got) != 1 || got[0].auth != "Bearer flat-code" {
		t.Fatalf("requests: %+v", got)
	}
}

func TestInjectedHeaderWithAManualRedirect(t *testing.T) {
	t.Parallel()
	f := newInjectFixture(t, allInjectOrigins)
	res, err := f.client.Fetch(context.Background(), Request{URL: "https://api.example.com/redirect", Redirect: "manual"})
	if err != nil || res.Status != http.StatusFound {
		t.Fatalf("manual redirect: %+v %v", res, err)
	}
	if got := f.requests(); len(got) != 1 || got[0].auth != injectedValue {
		t.Fatalf("requests: %+v", got)
	}
	if strings.Contains(string(res.Body), injectedValue) || strings.Contains(res.URL, injectedValue) {
		t.Fatal("the response holds the value")
	}
}

func TestInjectedHeaderPassesWithGuestHeadersAtTheirLimit(t *testing.T) {
	t.Parallel()
	f := newInjectFixture(t, allInjectOrigins)
	fill := strings.Repeat("x", MaxHeaders-len("X-Fill")-4)
	if _, err := f.client.Fetch(context.Background(), Request{URL: "https://api.example.com/v1", Headers: map[string][]string{"X-Fill": {fill}}}); err != nil {
		t.Fatalf("guest headers at the limit: %v", err)
	}
	if got := f.requests(); len(got) != 1 || got[0].auth != injectedValue || got[0].extra != fill {
		t.Fatalf("requests: %d", len(got))
	}
	if _, err := f.client.Fetch(context.Background(), Request{URL: "https://api.example.com/v1", Headers: map[string][]string{"X-Fill": {fill + "x"}}}); !errors.Is(err, ErrLimit) {
		t.Fatalf("one byte more: %v", err)
	}
}

func TestNewRefusesABadInjectedList(t *testing.T) {
	t.Parallel()
	good := Header{Origin: "https://api.example.com", Name: "Authorization", Value: injectedValue}
	many := make([]Header, MaxInjected+1)
	for i := range many {
		many[i] = Header{Origin: "https://api.example.com", Name: fmt.Sprintf("X-Key-%d", i), Value: injectedValue}
	}
	for name, list := range map[string][]Header{
		"http origin":         {{Origin: "http://api.example.com", Name: "Authorization", Value: injectedValue}},
		"origin not normal":   {{Origin: "https://API.example.com", Name: "Authorization", Value: injectedValue}},
		"origin with a path":  {{Origin: "https://api.example.com/v1", Name: "Authorization", Value: injectedValue}},
		"private origin":      {{Origin: "https://127.0.0.1", Name: "Authorization", Value: injectedValue}},
		"forbidden header":    {{Origin: "https://api.example.com", Name: "Host", Value: injectedValue}},
		"content type":        {{Origin: "https://api.example.com", Name: "Content-Type", Value: injectedValue}},
		"name not canonical":  {{Origin: "https://api.example.com", Name: "authorization", Value: injectedValue}},
		"empty value":         {{Origin: "https://api.example.com", Name: "Authorization"}},
		"line break":          {{Origin: "https://api.example.com", Name: "Authorization", Value: injectedValue + "\r\nX: y"}},
		"leading space":       {{Origin: "https://api.example.com", Name: "Authorization", Value: " " + injectedValue}},
		"trailing tab":        {{Origin: "https://api.example.com", Name: "Authorization", Value: injectedValue + "\t"}},
		"too large":           {{Origin: "https://api.example.com", Name: "Authorization", Value: strings.Repeat("a", MaxInjectedValue+1)}},
		"not UTF-8":           {{Origin: "https://api.example.com", Name: "Authorization", Value: "Bearer \xff"}},
		"two for one header":  {good, good},
		"more than the limit": many,
	} {
		c, err := New([]string{"https://api.example.com"}, list...)
		if err == nil {
			c.Close()
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), injectedValue) {
			t.Errorf("%s: the error holds the value: %v", name, err)
		}
	}
	// The limit case fails for the count only: the same list without one entry passes.
	if c, err := New([]string{"https://api.example.com"}, many[:MaxInjected]...); err != nil {
		t.Fatalf("%d headers: %v", MaxInjected, err)
	} else {
		c.Close()
	}
	c, err := New([]string{"https://api.example.com"}, good, Header{Origin: "https://api.example.com", Name: "X-Api-Key", Value: "key"})
	if err != nil {
		t.Fatalf("two headers for one origin: %v", err)
	}
	c.Close()
}

func TestInjectChecksOfTheService(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"https://api.example.com":      "https://api.example.com",
		"HTTPS://API.Example.com:443/": "https://api.example.com",
		"https://8.8.8.8":              "https://8.8.8.8",
	} {
		if got, err := InjectOrigin(raw); err != nil || got != want {
			t.Errorf("InjectOrigin(%q) = %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"http://api.example.com", "https://user:pass-secret@api.example.com", "https://api.example.com/?q=query-secret", "https://api.example.com/path", "https://api.example.com:8443", "https://10.0.0.1", "api.example.com", ""} {
		_, err := InjectOrigin(raw)
		if err == nil {
			t.Errorf("InjectOrigin(%q) accepted", raw)
			continue
		}
		if strings.Contains(err.Error(), "pass-secret") || strings.Contains(err.Error(), "query-secret") {
			t.Errorf("InjectOrigin(%q) repeats the input: %v", raw, err)
		}
	}
	if got, err := InjectHeader("x-api-key"); err != nil || got != "X-Api-Key" {
		t.Errorf("InjectHeader = %q, %v", got, err)
	}
	for _, name := range []string{"", "Bad Name", strings.Repeat("a", MaxInjectedName+1), "Host", "Content-Length", "Proxy-Authorization", "Sec-Fetch-Mode", "content-type", "Accept-Encoding"} {
		if _, err := InjectHeader(name); err == nil {
			t.Errorf("InjectHeader(%q) accepted", name)
		}
	}
	if err := CheckInjectValue(strings.Repeat("a", MaxInjectedValue)); err != nil {
		t.Errorf("8 KiB: %v", err)
	}
	for _, v := range []string{"", "zq9\nzq9", "zq9\x00zq9", "zq9\xffzq9", " zq9", "zq9 ", "\tzq9", "zq9\t", strings.Repeat("zq9", MaxInjectedValue)} {
		err := CheckInjectValue(v)
		if err == nil {
			t.Errorf("CheckInjectValue(%q) accepted", v)
			continue
		}
		if strings.Contains(err.Error(), "zq9") {
			t.Errorf("CheckInjectValue(%q) holds the value: %v", v, err)
		}
	}
}

// Each request carries the values of its own origin only: one header name at two origins with two values, and two headers
// at one origin (T1 of the testing review).
func TestTwoInjectedSecretsKeepTheirOwnValues(t *testing.T) {
	t.Parallel()
	f := newInjectFixture(t, allInjectOrigins,
		Header{Origin: "https://api.example.com", Name: "Authorization", Value: "Bearer value-of-api"},
		Header{Origin: "https://api.example.com", Name: "X-Api-Key", Value: "key-of-api"},
		Header{Origin: "https://other.example.com", Name: "Authorization", Value: "Bearer value-of-other"})
	ctx := context.Background()
	for _, url := range []string{"https://api.example.com/v1", "https://other.example.com/v1", "https://eu.api.example.com/v1"} {
		if _, err := f.client.Fetch(ctx, Request{URL: url}); err != nil {
			t.Fatalf("%s: %v", url, err)
		}
	}
	want := map[string]seenRequest{
		"api.example.com":    {auth: "Bearer value-of-api", key: "key-of-api"},
		"other.example.com":  {auth: "Bearer value-of-other"},
		"eu.api.example.com": {},
	}
	got := f.requests()
	if len(got) != len(want) {
		t.Fatalf("requests: %+v", got)
	}
	for _, r := range got {
		w, ok := want[r.host]
		if !ok || r.auth != w.auth || r.key != w.key {
			t.Errorf("%s got Authorization %q and X-Api-Key %q, want %q and %q", r.host, r.auth, r.key, w.auth, w.key)
		}
	}
}
