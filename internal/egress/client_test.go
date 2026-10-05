package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeOrigins(t *testing.T) {
	got, err := NormalizeOrigins([]string{"HTTPS://API.Example:443/", "http://api.example:80", "https://api.example"})
	if err != nil || fmt.Sprint(got) != "[http://api.example https://api.example]" {
		t.Fatalf("%v %v", got, err)
	}
	bad := []string{"*", "https://*.example", "ftp://api.example", "https://user:password@api.example", "https://api.example/path", "https://api.example?", "https://api.example?q=secret", "https://api.example#", "https://api.example:8443", "http://api.example:443", "https://api.example:", "https://api.example.", "https://é.example", "https://127.0.0.1", "https://[::1]", "https://[fe80::1%25en0]"}
	for _, input := range bad {
		t.Run(input, func(t *testing.T) {
			_, err := NormalizeOrigins([]string{input})
			if err == nil {
				t.Fatal("accepted invalid origin")
			}
			if strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "secret") {
				t.Fatal("input leaked")
			}
		})
	}
	if _, err := NormalizeOrigins(make([]string, MaxOrigins+1)); err == nil {
		t.Fatal("origin cap")
	}
	if got, err := NormalizeOrigins(nil); err != nil || len(got) != 0 {
		t.Fatalf("empty %v %v", got, err)
	}
}

func TestPublicIP(t *testing.T) {
	for _, raw := range []string{"0.0.0.1", "10.1.2.3", "100.64.1.2", "100.100.100.200", "127.0.0.2", "169.254.169.254", "172.31.1.1", "192.0.0.9", "192.0.2.1", "192.88.99.1", "192.168.1.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "240.1.1.1", "255.255.255.255", "168.63.129.16", "::1", "::ffff:8.8.8.8", "64:ff9b::808:808", "fc00::1", "fe80::1", "ff02::1", "2001::1", "2001:20::1", "2001:db8::1", "2002:808:808::1", "3ffe::1", "3fff::1", "4000::1"} {
		if publicIP(netip.MustParseAddr(raw)) {
			t.Errorf("allowed %s", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "93.184.216.34", "1.1.1.1", "2001:4860:4860::8888", "2606:4700:4700::1111"} {
		if !publicIP(netip.MustParseAddr(raw)) {
			t.Errorf("blocked %s", raw)
		}
	}
}

func fixture(t *testing.T, handler http.Handler) (*Client, *httptest.Server, *atomic.Int32) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New([]string{"http://api.example"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	client.lookup = func(ctx context.Context, network, host string) ([]netip.Addr, error) {
		if network != "ip" || host != "api.example" {
			t.Errorf("lookup %s %s", network, host)
		}
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	var calls atomic.Int32
	client.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		calls.Add(1)
		if network != "tcp" || address != "93.184.216.34:80" {
			t.Errorf("not pinned: %s %s", network, address)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	return client, server, &calls
}

func TestFetchPinnedAndFreshDNS(t *testing.T) {
	c, _, calls := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "api.example" || r.Header.Get("Authorization") != "Bearer fake-token" {
			t.Errorf("request %s %v", r.Host, r.Header)
		}
		if r.Header.Get("Accept-Encoding") != "" {
			t.Error("implicit compression")
		}
		w.Header().Set("X-Reply", "yes")
		io.WriteString(w, "response")
	}))
	var resolutions atomic.Int32
	c.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		if resolutions.Add(1) == 1 {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	res, err := c.Fetch(context.Background(), Request{URL: "http://api.example/path?q=test", Headers: map[string][]string{"Authorization": {"Bearer fake-token"}}})
	if err != nil || res.Status != 200 || string(res.Body) != "response" || res.Headers["X-Reply"][0] != "yes" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err = c.Fetch(context.Background(), Request{URL: "http://api.example"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("rebind %v", err)
	}
	if calls.Load() != 1 || resolutions.Load() != 2 {
		t.Fatalf("dial=%d lookup=%d", calls.Load(), resolutions.Load())
	}
}

func TestFetchRejectsMixedAndUnpermittedBeforeDial(t *testing.T) {
	c, _, calls := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unexpected request") }))
	c.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("169.254.169.254")}, nil
	}
	for _, raw := range []string{"http://api.example", "https://api.example", "http://other.example", "http://api.example:8080", "http://user:secret@api.example"} {
		if _, err := c.Fetch(context.Background(), Request{URL: raw}); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("dialed denied target")
	}
}

func TestRequestValidation(t *testing.T) {
	c, _, calls := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unexpected request") }))
	requests := []Request{
		{URL: "http://api.example", Headers: map[string][]string{strings.Repeat("X", MaxHeaders+1): {"x"}}},
		{URL: "http://api.example", Headers: map[string][]string{"X-Many-Values": make([]string, MaxHeaderFields+1)}},
		{URL: "http://api.example", Method: strings.Repeat("A", 1024)},
		{URL: "http://api.example", Headers: map[string][]string{"X-Empty": {}}},
		{URL: "http://api.example", Body: []byte("body")}, {URL: "http://api.example", Method: "CONNECT"}, {URL: "http://api.example", Method: "TRACE"},
		{URL: "http://api.example", Redirect: "follow"}, {URL: "http://api.example", Method: "POST", Body: make([]byte, MaxRequestBody+1)},
		{URL: "http://api.example/" + strings.Repeat("x", MaxURL)},
		{URL: "http://api.example", Headers: map[string][]string{"X-Large": {strings.Repeat("x", MaxHeaders)}}},
		{URL: "http://api.example", Headers: map[string][]string{"X-Test": {"bad\r\nsecret"}}},
	}
	for _, header := range []string{"Host", "Connection", "Content-Length", "Transfer-Encoding", "Upgrade", "Trailer", "TE", "Keep-Alive", "Expect", "Accept-Encoding", "Proxy-Authorization", "Sec-Fetch-Site", "Bad Name"} {
		requests = append(requests, Request{URL: "http://api.example", Headers: map[string][]string{header: {"x"}}})
	}
	many := map[string][]string{}
	for i := range MaxHeaderFields + 1 {
		many[fmt.Sprintf("X-%d", i)] = []string{"x"}
	}
	requests = append(requests, Request{URL: "http://api.example", Headers: many})
	for i, input := range requests {
		if _, err := c.Fetch(context.Background(), input); err == nil {
			t.Errorf("accepted %d", i)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("dialed invalid request")
	}
}

func TestHeaderAliasesShareValueLimit(t *testing.T) {
	c, _, calls := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unexpected request") }))
	c.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		t.Error("overlimit header aliases resolved DNS")
		return nil, errors.New("unexpected DNS")
	}
	_, err := c.Fetch(context.Background(), Request{URL: "http://api.example", Headers: map[string][]string{
		"X-Alias": make([]string, MaxHeaderFields), "x-alias": {"one more"},
	}})
	if !errors.Is(err, ErrLimit) || calls.Load() != 0 {
		t.Fatalf("header aliases: err=%v dials=%d", err, calls.Load())
	}
}

func TestRedirectsNeverFollow(t *testing.T) {
	var count atomic.Int32
	c, _, _ := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.Header().Set("Location", "http://169.254.169.254/latest/meta-data")
		w.WriteHeader(302)
	}))
	if _, err := c.Fetch(context.Background(), Request{URL: "http://api.example", Headers: map[string][]string{"Authorization": {"fake"}}}); !errors.Is(err, ErrRedirect) {
		t.Fatalf("%v", err)
	}
	res, err := c.Fetch(context.Background(), Request{URL: "http://api.example", Redirect: "manual"})
	if err != nil || res.Status != 302 || res.Redirected || count.Load() != 2 {
		t.Fatalf("%+v %v count%d", res, err, count.Load())
	}
}

func TestRedirectStatusMembership(t *testing.T) {
	for _, status := range []int{300, 301, 302, 303, 304, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			c, _, _ := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "http://169.254.169.254/latest/meta-data")
				w.Header().Set("ETag", "fixture-tag")
				w.WriteHeader(status)
				if status == 300 {
					io.WriteString(w, "choices")
				}
			}))
			res, err := c.Fetch(context.Background(), Request{URL: "http://api.example"})
			if status == 300 || status == 304 {
				if err != nil || res.Status != status || res.Headers["Etag"][0] != "fixture-tag" {
					t.Fatalf("ordinary status: %+v %v", res, err)
				}
				if status == 300 && string(res.Body) != "choices" {
					t.Fatalf("body: %q", res.Body)
				}
			} else if !errors.Is(err, ErrRedirect) {
				t.Fatalf("redirect status: %+v %v", res, err)
			}
			if calls.Load() != 1 {
				t.Fatalf("redirect followed: %d requests", calls.Load())
			}
		})
	}
}

func TestDefaultPortCanonicalRequest(t *testing.T) {
	for _, scheme := range []string{"http", "https"} {
		port := "80"
		if scheme == "https" {
			port = "443"
		}
		plain, origin, err := target(scheme + "://api.example/path?q=test")
		if err != nil {
			t.Fatal(err)
		}
		explicit, explicitOrigin, err := target(scheme + "://api.example:" + port + "/path?q=test")
		if err != nil || explicit.Host != plain.Host || explicit.String() != plain.String() || explicitOrigin != origin {
			t.Fatalf("different request target: %v %v %v", plain, explicit, err)
		}
	}
	c, _, _ := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "api.example" {
			t.Errorf("noncanonical Host: %q", r.Host)
		}
		io.WriteString(w, "ok")
	}))
	for _, raw := range []string{"http://api.example/path", "http://api.example:80/path"} {
		res, err := c.Fetch(context.Background(), Request{URL: raw})
		if err != nil || res.URL != "http://api.example/path" || string(res.Body) != "ok" {
			t.Fatalf("canonical response URL: %+v %v", res, err)
		}
	}
}

func TestResponseLimits(t *testing.T) {
	for _, kind := range []string{"length", "chunked", "headers"} {
		t.Run(kind, func(t *testing.T) {
			c, _, _ := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "length":
					w.Header().Set("Content-Length", fmt.Sprint(MaxResponseBody+1))
					w.WriteHeader(200)
				case "chunked":
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					io.WriteString(w, strings.Repeat("x", MaxResponseBody+1))
				case "headers":
					w.Header().Set("X-Large", strings.Repeat("x", MaxHeaders+1))
					w.WriteHeader(200)
				}
			}))
			if _, err := c.Fetch(context.Background(), Request{URL: "http://api.example"}); err == nil {
				t.Fatal("unbounded response")
			}
		})
	}
}

func TestCancellationAndClose(t *testing.T) {
	started := make(chan struct{}, 1)
	c, _, _ := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { started <- struct{}{}; <-r.Context().Done() }))
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := c.Fetch(ctx, Request{URL: "http://api.example"}); result <- err }()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, ErrCanceled) {
		t.Fatalf("cancel %v", err)
	}
	go func() { _, err := c.Fetch(context.Background(), Request{URL: "http://api.example"}); result <- err }()
	<-started
	c.Close()
	c.Close()
	if err := <-result; !errors.Is(err, ErrCanceled) {
		t.Fatalf("close %v", err)
	}
	if _, err := c.Fetch(context.Background(), Request{URL: "http://api.example"}); !errors.Is(err, ErrCanceled) {
		t.Fatalf("afterclose %v", err)
	}
}

func TestAlreadyCanceledDoesNotResolveOrDial(t *testing.T) {
	c, err := New([]string{"http://api.example", "http://8.8.8.8"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		t.Error("canceled request resolved DNS")
		return nil, errors.New("unexpected lookup")
	}
	c.dial = func(context.Context, string, string) (net.Conn, error) {
		t.Error("canceled request dialed socket")
		return nil, errors.New("unexpected dial")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, raw := range []string{"http://api.example", "http://8.8.8.8"} {
		if _, err := c.Fetch(ctx, Request{URL: raw}); !errors.Is(err, ErrCanceled) {
			t.Fatalf("%v", err)
		}
	}
}

func TestParentDeadlineAndDNSFailuresSafe(t *testing.T) {
	c, _, _ := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c.lookup = func(ctx context.Context, _, _ string) ([]netip.Addr, error) { <-ctx.Done(); return nil, ctx.Err() }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Fetch(ctx, Request{URL: "http://api.example?credential=fake-secret"}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("%v", err)
	}
	c.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return nil, errors.New("private diagnostic fake-secret")
	}
	if _, err := c.Fetch(context.Background(), Request{URL: "http://api.example?credential=fake-secret"}); !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("%v", err)
	}
}

func TestConcurrencyAndRateLimits(t *testing.T) {
	c, _, _ := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	entered := make(chan struct{}, 5)
	c.lookup = func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() { c.Fetch(context.Background(), Request{URL: "http://api.example"}) })
	}
	for range 5 {
		<-entered
	}
	if _, err := c.Fetch(context.Background(), Request{URL: "http://api.example"}); !errors.Is(err, ErrLimit) {
		t.Fatalf("concurrency %v", err)
	}
	c.Close()
	wg.Wait()
	c2, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	c2.origins["http://api.example"] = struct{}{}
	c2.lookup = func(context.Context, string, string) ([]netip.Addr, error) { return nil, errors.New("fake") }
	c2.limiter.SetLimit(0)
	for range 20 {
		if _, err := c2.Fetch(context.Background(), Request{URL: "http://api.example"}); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("burst %v", err)
		}
	}
	if _, err := c2.Fetch(context.Background(), Request{URL: "http://api.example"}); !errors.Is(err, ErrLimit) {
		t.Fatalf("rate %v", err)
	}
}

func TestTLSCertificateValidation(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("TLS validation bypass") }))
	defer server.Close()
	c, err := New([]string{"https://api.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	c.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "8.8.8.8:443" {
			t.Errorf("pin %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	if _, err := c.Fetch(context.Background(), Request{URL: "https://api.example"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("untrusted TLS %v", err)
	}
}

func TestDenyAllAndLiteralDoesNotResolve(t *testing.T) {
	c, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Fetch(context.Background(), Request{URL: "http://8.8.8.8"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("empty permissions %v", err)
	}
	c.origins["http://8.8.8.8"] = struct{}{}
	c.lookup = func(context.Context, string, string) ([]netip.Addr, error) { t.Error("literal DNS"); return nil, nil }
	c.dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("safe fake") }
	if _, err := c.Fetch(context.Background(), Request{URL: "http://8.8.8.8"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("literal %v", err)
	}
}
