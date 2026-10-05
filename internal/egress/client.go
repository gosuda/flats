// Package egress provides a deny-by-default, bounded HTTP client for guest code.
// It never uses proxies, ambient cookies, or a guest-supplied socket address.
package egress

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
	"golang.org/x/time/rate"
)

const (
	MaxOrigins      = 32
	MaxURL          = 8 << 10
	MaxHeaders      = 16 << 10
	MaxHeaderFields = 128
	MaxRequestBody  = 1 << 20
	MaxResponseBody = 4 << 20
	Timeout         = 5 * time.Second
)

// Errors deliberately contain no guest data or underlying network diagnostics.
var (
	ErrDenied      = errors.New("network target is not permitted")
	ErrInvalid     = errors.New("invalid outbound request")
	ErrLimit       = errors.New("outbound request limit exceeded")
	ErrUnavailable = errors.New("outbound request failed")
	ErrRedirect    = errors.New("outbound redirects are disabled")
	ErrCanceled    = errors.New("outbound request canceled")
	ErrTimeout     = errors.New("outbound request timed out")
)

type Request struct {
	URL      string              `json:"url"`
	Method   string              `json:"method"`
	Headers  map[string][]string `json:"headers"`
	Body     []byte              `json:"body"`
	Redirect string              `json:"redirect"`
}

type Response struct {
	Status     int                 `json:"status"`
	Headers    map[string][]string `json:"headers"`
	Body       []byte              `json:"body"`
	URL        string              `json:"url"`
	Redirected bool                `json:"redirected"`
}

type Client struct {
	origins  map[string]struct{}
	lookup   func(context.Context, string, string) ([]netip.Addr, error)
	dial     func(context.Context, string, string) (net.Conn, error)
	slots    chan struct{}
	limiter  *rate.Limiter
	lifetime context.Context
	close    context.CancelFunc
}

// NormalizeOrigins validates exact HTTP(S) origins, sorts and deduplicates them.
// Only standard ports are allowed. Empty permissions deny all outbound access.
func NormalizeOrigins(origins []string) ([]string, error) {
	if len(origins) > MaxOrigins {
		return nil, errors.New("too many network origins")
	}
	result := make([]string, 0, len(origins))
	for _, origin := range origins {
		u, normalized, err := target(origin)
		if err != nil || (u != nil && (u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.ForceQuery)) {
			return nil, errors.New("invalid network origin")
		}
		result = append(result, normalized)
	}
	slices.Sort(result)
	return slices.Compact(result), nil
}

func New(origins []string) (*Client, error) {
	normalized, err := NormalizeOrigins(origins)
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(context.Background())
	c := &Client{origins: make(map[string]struct{}, len(normalized)), lookup: net.DefaultResolver.LookupNetIP,
		dial: (&net.Dialer{Timeout: Timeout}).DialContext, slots: make(chan struct{}, 5),
		limiter: rate.NewLimiter(20, 20), lifetime: lifetime, close: cancel}
	for _, origin := range normalized {
		c.origins[origin] = struct{}{}
	}
	return c, nil
}

// Close cancels active requests and prevents new requests. It is safe to repeat.
func (c *Client) Close() { c.close() }

func target(raw string) (*url.URL, string, error) {
	if len(raw) == 0 || len(raw) > MaxURL {
		return nil, "", ErrInvalid
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Fragment != "" || strings.Contains(raw, "#") ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, "", ErrInvalid
	}
	host := strings.ToLower(u.Hostname())
	if !validHost(host) {
		return nil, "", ErrInvalid
	}
	port := u.Port()
	if port != "" && (u.Scheme == "http" && port != "80" || u.Scheme == "https" && port != "443") {
		return nil, "", ErrInvalid
	}
	// Empty explicit ports and noncanonical authority forms are rejected.
	authority := host
	if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}
	if !strings.EqualFold(u.Host, authority) && !strings.EqualFold(u.Host, authority+":"+port) || strings.HasSuffix(u.Host, ":") {
		return nil, "", ErrInvalid
	}
	if ip, err := netip.ParseAddr(host); err == nil && !publicIP(ip) {
		return nil, "", ErrDenied
	}
	u.Host = authority
	return u, u.Scheme + "://" + authority, nil
}

func validHost(host string) bool {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Zone() == ""
	}
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if ch < 'a' || ch > 'z' {
				if ch < '0' || ch > '9' {
					if ch != '-' {
						return false
					}
				}
			}
		}
	}
	return true
}

var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
	// Azure's platform virtual address is globally numbered but host-local infrastructure.
	netip.MustParsePrefix("168.63.129.16/32"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3ffe::/16"), netip.MustParsePrefix("3fff::/20"),
}

func publicIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.Is4In6() {
		return false
	}
	// Only the currently allocated global IPv6 unicast range is supported; this
	// also excludes NAT64, compatible/mapped addresses and other transition space.
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range blocked {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func forbiddenHeader(name string) bool {
	name = strings.ToLower(name)
	switch name {
	case "host", "connection", "content-length", "transfer-encoding", "upgrade", "trailer", "te", "keep-alive", "expect", "accept-encoding":
		return true
	}
	return strings.HasPrefix(name, "proxy-") || strings.HasPrefix(name, "sec-")
}

// Keep each addition bounded before performing it, including guest-sized strings.
func addHeaderSize(total *int, name, value string) bool {
	for _, size := range []int{len(name), len(value), 4} {
		if size > MaxHeaders-*total {
			return false
		}
		*total += size
	}
	return true
}

func (c *Client) Fetch(parent context.Context, input Request) (Response, error) {
	var empty Response
	if c.lifetime.Err() != nil {
		return empty, ErrCanceled
	}
	u, origin, err := target(input.URL)
	if err != nil {
		return empty, err
	}
	if _, ok := c.origins[origin]; !ok {
		return empty, ErrDenied
	}
	if len(input.Body) > MaxRequestBody {
		return empty, ErrLimit
	}
	if input.Redirect != "" && input.Redirect != "error" && input.Redirect != "manual" {
		return empty, ErrInvalid
	}
	method := input.Method
	if method == "" {
		method = "GET"
	}
	if len(method) > 7 || !httpguts.ValidHeaderFieldName(method) {
		return empty, ErrInvalid
	}
	method = strings.ToUpper(method)
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
	default:
		return empty, ErrInvalid
	}
	if (method == "GET" || method == "HEAD") && len(input.Body) != 0 {
		return empty, ErrInvalid
	}
	if len(input.Headers) > MaxHeaderFields {
		return empty, ErrLimit
	}
	headers := make(http.Header, len(input.Headers))
	size := 0
	for name, values := range input.Headers {
		if len(name) > MaxHeaders {
			return empty, ErrLimit
		}
		if len(values) == 0 {
			return empty, ErrInvalid
		}
		if len(values) > MaxHeaderFields {
			return empty, ErrLimit
		}
		if !httpguts.ValidHeaderFieldName(name) || forbiddenHeader(name) {
			return empty, ErrInvalid
		}
		canonical := http.CanonicalHeaderKey(name)
		if len(values) > MaxHeaderFields-len(headers[canonical]) {
			return empty, ErrLimit
		}
		for _, value := range values {
			if !addHeaderSize(&size, name, value) {
				return empty, ErrLimit
			}
			if !httpguts.ValidHeaderFieldValue(value) {
				return empty, ErrInvalid
			}
			headers.Add(name, value)
		}
	}
	if !c.limiter.Allow() {
		return empty, ErrLimit
	}
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		return empty, ErrLimit
	}
	ctx, cancel := context.WithTimeout(parent, Timeout)
	defer cancel()
	stop := context.AfterFunc(c.lifetime, cancel)
	defer stop()
	safeError := func() error {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ErrTimeout
		}
		if ctx.Err() != nil {
			return ErrCanceled
		}
		return ErrUnavailable
	}
	if ctx.Err() != nil {
		return empty, safeError()
	}
	hostname := u.Hostname()
	var addresses []netip.Addr
	if ip, err := netip.ParseAddr(hostname); err == nil {
		addresses = []netip.Addr{ip}
	} else {
		addresses, err = c.lookup(ctx, "ip", hostname)
		if err != nil {
			return empty, safeError()
		}
	}
	if len(addresses) == 0 {
		return empty, ErrUnavailable
	}
	if len(addresses) > 32 {
		return empty, ErrLimit
	}
	for _, ip := range addresses {
		if !publicIP(ip) {
			return empty, ErrDenied
		}
	}
	port := "80"
	if u.Scheme == "https" {
		port = "443"
	}
	tr := &http.Transport{
		Proxy: nil, DisableKeepAlives: true, DisableCompression: true,
		MaxResponseHeaderBytes: MaxHeaders, ResponseHeaderTimeout: Timeout, TLSHandshakeTimeout: Timeout,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: hostname},
		TLSNextProto:    make(map[string]func(string, *tls.Conn) http.RoundTripper),
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			for _, ip := range addresses {
				conn, err := c.dial(ctx, "tcp", net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				if ctx.Err() != nil {
					break
				}
			}
			return nil, ErrUnavailable
		},
	}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(input.Body))
	if err != nil {
		return empty, ErrInvalid
	}
	req.Header = headers
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return empty, safeError()
	}
	defer res.Body.Close()
	if isRedirect(res.StatusCode) && input.Redirect != "manual" {
		return empty, ErrRedirect
	}
	size = 0
	for name, values := range res.Header {
		for _, value := range values {
			if !addHeaderSize(&size, name, value) {
				return empty, ErrLimit
			}
		}
	}
	if res.ContentLength > MaxResponseBody {
		return empty, ErrLimit
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, MaxResponseBody+1))
	if err != nil {
		return empty, safeError()
	}
	if len(body) > MaxResponseBody {
		return empty, ErrLimit
	}
	// Hop headers do not become guest-visible response metadata.
	for _, value := range res.Header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			res.Header.Del(strings.TrimSpace(name))
		}
	}
	for name := range res.Header {
		if forbiddenHeader(name) {
			res.Header.Del(name)
		}
	}
	return Response{Status: res.StatusCode, Headers: map[string][]string(res.Header), Body: body, URL: u.String()}, nil
}

func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}
