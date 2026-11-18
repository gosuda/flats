package webhook

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// responseMarker is written into every error response, so that a test can show
// that no log line quotes a response body.
const responseMarker = "RESPONSE-BODY-MARKER-77f2"

// tokenMarker sits in the query of every test URL, like the token of a chat
// webhook URL.
const tokenMarker = "TOKEN-4f9d"

// statusServer answers the statuses in order, and the last one for every
// further request.
func statusServer(t *testing.T, statuses ...int) (*httptest.Server, *capture) {
	t.Helper()
	var capt capture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := capt.record(r)
		code := statuses[min(n, len(statuses)-1)]
		w.WriteHeader(code)
		if code >= 300 {
			io.WriteString(w, responseMarker)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &capt
}

func TestAttemptHeadersAndSignature(t *testing.T) {
	var capt capture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capt.record(r) == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	var clock stepClock
	client, _ := newCountingClient(t)
	h := newHarness(t, Config{
		Keys:    keysFor(srv.URL + "/hook?token=" + tokenMarker),
		Client:  client,
		Now:     clock.now,
		Waits:   []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
		Version: "9.9.9",
	})
	h.start(t.Context())

	// The event has a time, so the clock is read once for each attempt only.
	h.sender.Notify(sampleEvent("evt_headers"))
	h.wait(1)

	reqs := capt.all()
	if len(reqs) != 2 {
		t.Fatalf("%d attempts, want 2 (a 500 and then a success)", len(reqs))
	}
	for k, r := range reqs {
		hd := r.header
		if r.method != http.MethodPost {
			t.Errorf("attempt %d: method %s, want POST", k+1, r.method)
		}
		want := map[string]string{
			"Content-Type":     "application/json",
			"User-Agent":       "flats/9.9.9",
			"X-Flats-Event":    "approval.created",
			"X-Flats-Event-Id": "evt_headers",
		}
		for name, v := range want {
			if got := hd.Get(name); got != v {
				t.Errorf("attempt %d: %s = %q, want %q", k+1, name, got, v)
			}
		}
		body := decodeMap(t, r.body)
		if body["id"] != hd.Get("X-Flats-Event-Id") {
			t.Errorf("attempt %d: header id %q differs from body id %v", k+1, hd.Get("X-Flats-Event-Id"), body["id"])
		}
		stamp := hd.Get("X-Flats-Timestamp")
		unix, err := strconv.ParseInt(stamp, 10, 64)
		if err != nil {
			t.Fatalf("attempt %d: timestamp %q is not a number: %v", k+1, stamp, err)
		}
		if wantUnix := epoch.Unix() + int64(k); unix != wantUnix {
			t.Errorf("attempt %d: timestamp %d, want %d from the injected clock", k+1, unix, wantUnix)
		}
		if got, want := hd.Get("X-Flats-Signature"), refSign(testSecret, unix, r.body); got != want {
			t.Errorf("attempt %d: signature %q does not verify, want %q", k+1, got, want)
		}
	}
	if string(reqs[0].body) != string(reqs[1].body) {
		t.Errorf("the retry changed the body:\n%s\n%s", reqs[0].body, reqs[1].body)
	}
	if reqs[0].header.Get("X-Flats-Event-Id") != reqs[1].header.Get("X-Flats-Event-Id") {
		t.Error("the retry changed the event id")
	}
	if reqs[0].header.Get("X-Flats-Timestamp") == reqs[1].header.Get("X-Flats-Timestamp") ||
		reqs[0].header.Get("X-Flats-Signature") == reqs[1].header.Get("X-Flats-Signature") {
		t.Error("the retry did not take a new timestamp and a new signature")
	}
}

// caseTarget is what one case of TestDeliveryOutcomes sends to.
type caseTarget struct {
	url string
	// client is nil for the default client of the sender.
	client *http.Client
	// attempts counts the attempts that the sender made.
	attempts func() int
	// untouched counts requests to a server that must never be reached.
	untouched func() int
	// signatures are the signature headers that a receiver saw.
	signatures func() []string
	// hosts are strings of the receiver's address that no log line may hold.
	hosts []string
}

func hostsOf(srv *httptest.Server) []string {
	u, _ := url.Parse(srv.URL)
	return []string{u.Host, u.Hostname(), "127.0.0.1"}
}

// TestDeliveryOutcomes covers the retry policy of rules 13 and 14 and the log
// lines of rule 10 together: each case states how many attempts it must see,
// and every failure must leave exactly one log line with the event ID, the type
// and the class, and none of the URL, the secret, a signature or a response.
func TestDeliveryOutcomes(t *testing.T) {
	const eventID = "evt_outcome"
	waits := []time.Duration{0, time.Millisecond, 0}

	plain := func(statuses ...int) func(*testing.T) caseTarget {
		return func(t *testing.T) caseTarget {
			srv, capt := statusServer(t, statuses...)
			client, ct := newCountingClient(t)
			return caseTarget{
				url: srv.URL + "/hook?token=" + tokenMarker, client: client,
				attempts:   func() int { return int(ct.n.Load()) },
				signatures: capt.signatures, hosts: hostsOf(srv),
			}
		}
	}
	redirect := func(follow bool) func(*testing.T) caseTarget {
		return func(t *testing.T) caseTarget {
			other, otherCapt := statusServer(t, http.StatusOK)
			var capt capture
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capt.record(r)
				http.Redirect(w, r, other.URL+"/elsewhere", http.StatusFound)
			}))
			t.Cleanup(srv.Close)
			client, ct := newCountingClient(t)
			if !follow {
				// The client of the test does not follow either: the sender must
				// not depend on that.
				client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			}
			return caseTarget{
				url: srv.URL + "/hook?token=" + tokenMarker, client: client,
				attempts:   func() int { return int(ct.n.Load()) },
				untouched:  otherCapt.count,
				signatures: capt.signatures, hosts: append(hostsOf(srv), hostsOf(other)...),
			}
		}
	}

	cases := []struct {
		name     string
		limit    time.Duration
		serial   bool
		setup    func(*testing.T) caseTarget
		attempts int
		class    string // empty when the event succeeds
	}{
		{name: "500", setup: plain(500), attempts: 4, class: "status 500"},
		{name: "502", setup: plain(502), attempts: 4, class: "status 502"},
		{name: "429", setup: plain(429), attempts: 4, class: "status 429"},
		{name: "408", setup: plain(408), attempts: 4, class: "status 408"},
		{name: "500 then 200", setup: plain(500, 200), attempts: 2},
		{name: "500 500 then 204", setup: plain(500, 500, 204), attempts: 3},
		{name: "400", setup: plain(400), attempts: 1, class: "status 400"},
		{name: "401", setup: plain(401), attempts: 1, class: "status 401"},
		{name: "404", setup: plain(404), attempts: 1, class: "status 404"},
		{name: "410", setup: plain(410), attempts: 1, class: "status 410"},
		{name: "200", setup: plain(200), attempts: 1},
		{name: "204", setup: plain(204), attempts: 1},
		{name: "302", setup: redirect(false), attempts: 1, class: "status 302"},
		{name: "302 with a client that follows redirects", setup: redirect(true), attempts: 1, class: "status 302"},
		{
			name: "attempt over the limit", limit: 50 * time.Millisecond, attempts: 4, class: "timeout",
			setup: func(t *testing.T) caseTarget {
				var capt capture
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					capt.record(r)
					// The receiver never answers. It ends when the sender gives up
					// and closes the connection.
					<-r.Context().Done()
				}))
				t.Cleanup(srv.Close)
				client, ct := newCountingClient(t)
				return caseTarget{
					url: srv.URL + "/hook?token=" + tokenMarker, client: client,
					attempts:   func() int { return int(ct.n.Load()) },
					signatures: capt.signatures, hosts: hostsOf(srv),
				}
			},
		},
		{
			// The case frees a port and must not run beside a test that opens
			// listeners, or another listener could take the port.
			name: "refused connection", serial: true, attempts: 4, class: "connect",
			setup: func(t *testing.T) caseTarget {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				addr := ln.Addr().String()
				ln.Close()
				client, ct := newCountingClient(t)
				return caseTarget{
					url: "http://" + addr + "/hook?token=" + tokenMarker, client: client,
					attempts: func() int { return int(ct.n.Load()) },
					hosts:    []string{addr, "127.0.0.1"},
				}
			},
		},
		{
			// The default client of the sender does not trust the test server.
			name: "certificate failure", attempts: 4, class: "tls",
			setup: func(t *testing.T) caseTarget {
				var conns atomic.Int32
				srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					t.Error("the handler ran, so the certificate was accepted")
				}))
				srv.Config.ErrorLog = log.New(io.Discard, "", 0)
				srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
					if st == http.StateNew {
						conns.Add(1)
					}
				}
				srv.StartTLS()
				t.Cleanup(srv.Close)
				return caseTarget{
					url:      srv.URL + "/hook?token=" + tokenMarker,
					attempts: func() int { return int(conns.Load()) },
					hosts:    hostsOf(srv),
				}
			},
		},
		{
			name: "an error of no known class", attempts: 4, class: "other",
			setup: func(t *testing.T) caseTarget {
				var n atomic.Int32
				client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					n.Add(1)
					return nil, errors.New("boom while sending to https://leak.example.net/hook?token=" + tokenMarker)
				})}
				return caseTarget{
					url: "https://receiver.example.org/hook?token=" + tokenMarker, client: client,
					attempts: func() int { return int(n.Load()) },
					hosts:    []string{"leak.example.net", "receiver.example.org"},
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.serial {
				t.Parallel()
			}
			target := tc.setup(t)
			h := newHarness(t, Config{
				Keys:         keysFor(target.url),
				Client:       target.client,
				Now:          fixedClock,
				Waits:        waits,
				AttemptLimit: tc.limit,
			})
			h.start(t.Context())
			p := sampleEvent(eventID)
			h.sender.Notify(p)
			h.wait(1)

			if got := target.attempts(); got != tc.attempts {
				t.Errorf("%d attempts, want %d", got, tc.attempts)
			}
			if target.untouched != nil {
				if n := target.untouched(); n != 0 {
					t.Errorf("the redirect target got %d requests, want none", n)
				}
			}

			lines := h.log.snapshot()
			if tc.class == "" {
				if len(lines) != 0 {
					t.Errorf("log lines for a delivered event: %q", lines)
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("%d log lines %q, want exactly one for the failure", len(lines), lines)
			}
			line := lines[0]
			for _, want := range []string{eventID, p.Type, tc.class} {
				if !strings.Contains(line, want) {
					t.Errorf("log line %q lacks %q", line, want)
				}
			}
			forbidden := []string{testSecret, "v1=", responseMarker, tokenMarker, "/hook", "http://", "https://"}
			forbidden = append(forbidden, target.hosts...)
			if target.signatures != nil {
				forbidden = append(forbidden, target.signatures()...)
			}
			if bad, found := containsAny(line, forbidden); found {
				t.Errorf("log line %q holds %q", line, bad)
			}
		})
	}
}

// roundTripFunc is a transport made of a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClassify(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"wrapped deadline", &url.Error{Op: "Post", URL: "https://x.example/y", Err: context.DeadlineExceeded}, "timeout"},
		{"net timeout", &net.OpError{Op: "read", Err: timeoutError{}}, "timeout"},
		{"dial timeout", &net.OpError{Op: "dial", Err: timeoutError{}}, "timeout"},
		{"dial refused", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connectex: refused")}, "connect"},
		{"refused errno", fmt.Errorf("send: %w", syscall.ECONNREFUSED), "connect"},
		{"dns", &net.DNSError{Err: "no such host", Name: "x.example"}, "connect"},
		{"certificate", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, "tls"},
		{"wrapped certificate", &url.Error{Op: "Post", URL: "https://x.example/y", Err: &tls.CertificateVerificationError{}}, "tls"},
		{"unknown authority", x509.UnknownAuthorityError{}, "tls"},
		{"bad hostname", x509.HostnameError{Host: "x.example"}, "tls"},
		{"invalid certificate", x509.CertificateInvalidError{Reason: x509.Expired}, "tls"},
		{"not tls", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, "tls"},
		{"alert", tls.AlertError(42), "tls"},
		{"reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, "other"},
		{"eof", io.EOF, "other"},
		{"plain", errors.New("boom"), "other"},
	}
	for _, tc := range cases {
		if got := classify(tc.err); got != tc.want {
			t.Errorf("%s: classify = %q, want %q", tc.name, got, tc.want)
		}
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestKeysOffDropsQuietly(t *testing.T) {
	t.Parallel()
	cases := map[string]func(context.Context) (Credentials, error){
		"off": func(context.Context) (Credentials, error) { return Credentials{}, ErrOff },
		"wrapped off": func(context.Context) (Credentials, error) {
			return Credentials{}, fmt.Errorf("reading the keys: %w", ErrOff)
		},
		"empty url": func(context.Context) (Credentials, error) {
			return Credentials{Secret: testSecret}, nil
		},
		"empty secret": func(context.Context) (Credentials, error) {
			return Credentials{URL: "http://127.0.0.1:1/x"}, nil
		},
	}
	for name, keys := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, capt := statusServer(t, 200)
			_ = srv
			client, ct := newCountingClient(t)
			h := newHarness(t, Config{Keys: keys, Client: client, Waits: []time.Duration{0, 0, 0}, Now: fixedClock})
			h.start(t.Context())
			h.sender.Notify(sampleEvent("evt_off"))
			h.wait(1)
			if n := ct.n.Load(); n != 0 || capt.count() != 0 {
				t.Errorf("%d attempts for a webhook that is off", n)
			}
			if lines := h.log.snapshot(); len(lines) != 0 {
				t.Errorf("log lines while the webhook is off: %q", lines)
			}
		})
	}
	t.Run("nil Keys", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, Config{Now: fixedClock})
		h.start(t.Context())
		h.sender.Notify(sampleEvent("evt_nil"))
		h.wait(1)
		if lines := h.log.snapshot(); len(lines) != 0 {
			t.Errorf("log lines: %q", lines)
		}
	})
}

func TestKeysErrorDropsWithOneLine(t *testing.T) {
	t.Parallel()
	_, capt := statusServer(t, 200)
	client, ct := newCountingClient(t)
	var calls atomic.Int32
	h := newHarness(t, Config{
		Keys: func(context.Context) (Credentials, error) {
			calls.Add(1)
			return Credentials{}, errors.New("cannot open /home/operator/.flats/secret.key for " + testSecret)
		},
		Client: client,
		Waits:  []time.Duration{0, 0, 0},
		Now:    fixedClock,
	})
	h.start(t.Context())
	h.sender.Notify(sampleEvent("evt_keys"))
	h.wait(1)

	if n := ct.n.Load(); n != 0 || capt.count() != 0 {
		t.Errorf("%d attempts for keys that could not be read", n)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("Keys was called %d times, want once: a failed read is not retried", n)
	}
	lines := h.log.snapshot()
	if len(lines) != 1 {
		t.Fatalf("%d log lines %q, want exactly one", len(lines), lines)
	}
	for _, want := range []string{"evt_keys", "approval.created"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("log line %q lacks %q", lines[0], want)
		}
	}
	if bad, found := containsAny(lines[0], []string{testSecret, "/home/operator", "secret.key"}); found {
		t.Errorf("log line %q quotes the error of Keys (%q)", lines[0], bad)
	}
}

// TestKeysAreReadOncePerEvent shows rule 11: one read for the four attempts of
// an event, the values of that read for every attempt, and a new read for the
// next event.
func TestKeysAreReadOncePerEvent(t *testing.T) {
	t.Parallel()
	first, firstCapt := statusServer(t, 500)
	second, secondCapt := statusServer(t, 200)
	const (
		firstSecret  = "first-secret-0123456789"
		secondSecret = "second-secret-0123456789"
	)
	var calls atomic.Int32
	h := newHarness(t, Config{
		Keys: func(context.Context) (Credentials, error) {
			if calls.Add(1) == 1 {
				return Credentials{URL: first.URL, Secret: firstSecret}, nil
			}
			return Credentials{URL: second.URL, Secret: secondSecret}, nil
		},
		Waits: []time.Duration{0, 0, 0},
		Now:   fixedClock,
	})
	h.start(t.Context())

	h.sender.Notify(sampleEvent("evt_one"))
	h.wait(1)
	if n := calls.Load(); n != 1 {
		t.Fatalf("Keys was called %d times for an event with 4 attempts, want 1", n)
	}
	if n := firstCapt.count(); n != 4 {
		t.Fatalf("the first receiver got %d attempts, want 4", n)
	}
	if n := secondCapt.count(); n != 0 {
		t.Fatalf("the second receiver got %d attempts of the first event, want none", n)
	}
	for k, r := range firstCapt.all() {
		stamp, _ := strconv.ParseInt(r.header.Get("X-Flats-Timestamp"), 10, 64)
		if got, want := r.header.Get("X-Flats-Signature"), refSign(firstSecret, stamp, r.body); got != want {
			t.Errorf("attempt %d was not signed with the secret of the first read", k+1)
		}
	}

	h.sender.Notify(sampleEvent("evt_two"))
	h.wait(1)
	if n := calls.Load(); n != 2 {
		t.Fatalf("Keys was called %d times for two events, want 2: the values are not cached", n)
	}
	got := secondCapt.all()
	if len(got) != 1 {
		t.Fatalf("the second receiver got %d attempts, want 1", len(got))
	}
	stamp, _ := strconv.ParseInt(got[0].header.Get("X-Flats-Timestamp"), 10, 64)
	if got[0].header.Get("X-Flats-Signature") != refSign(secondSecret, stamp, got[0].body) {
		t.Error("the second event was not signed with the secret of its own read")
	}
}

func TestDefaultClient(t *testing.T) {
	t.Parallel()
	c := newDefaultClient()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.Transport)
	}
	if cfg := tr.TLSClientConfig; cfg != nil {
		if cfg.InsecureSkipVerify {
			t.Error("the default client skips certificate verification")
		}
		if cfg.VerifyConnection != nil || cfg.VerifyPeerCertificate != nil {
			t.Error("the default client replaces certificate verification")
		}
	}
	if c.CheckRedirect == nil {
		t.Fatal("the default client has no redirect policy, so it would follow redirects")
	}

	var target capture
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { target.record(r) }))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(c.CloseIdleConnections)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("status %d, want the 307 itself", resp.StatusCode)
	}
	if n := target.count(); n != 0 {
		t.Errorf("the default client followed a redirect (%d requests to the target)", n)
	}
}

// TestDefaultClientRejectsUntrustedCertificate shows that the client that the
// sender builds verifies a certificate, with a real TLS handshake.
func TestDefaultClientRejectsUntrustedCertificate(t *testing.T) {
	t.Parallel()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the handler ran for a client that does not trust the certificate")
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)

	c := newDefaultClient()
	t.Cleanup(c.CloseIdleConnections)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("the default client accepted a certificate that it does not trust")
	}
	if got := classify(err); got != "tls" {
		t.Errorf("class %q for %v, want tls", got, err)
	}
}

// TestGivenClientIsNotChanged shows that the sender copies a client before it
// disables redirects, because the caller may share it.
func TestGivenClientIsNotChanged(t *testing.T) {
	t.Parallel()
	given := &http.Client{Timeout: 5 * time.Second}
	New(Config{Client: given})
	if given.CheckRedirect != nil {
		t.Error("New changed the redirect policy of the client it was given")
	}
}

// endlessBody is a response body that never ends. It counts what is read from
// it and whether it was closed.
type endlessBody struct {
	read   atomic.Int64
	closed atomic.Bool
}

func (b *endlessBody) Read(p []byte) (int, error) {
	clear(p)
	b.read.Add(int64(len(p)))
	return len(p), nil
}

func (b *endlessBody) Close() error {
	b.closed.Store(true)
	return nil
}

// TestDrainIsBounded shows that a receiver that answers with a body that never
// ends does not make the attempt read it all: the sender reads at most 4 KiB
// and closes the body.
func TestDrainIsBounded(t *testing.T) {
	t.Parallel()
	body := &endlessBody{}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body, Request: r}, nil
	})}
	h := newHarness(t, Config{
		Keys: keysFor("https://receiver.example.org/hook"), Client: client, Waits: []time.Duration{}, Now: fixedClock,
	})
	h.start(t.Context())
	h.sender.Notify(sampleEvent("evt_drain"))
	h.wait(1)
	if lines := h.log.snapshot(); len(lines) != 0 {
		t.Errorf("a 200 left log lines: %q", lines)
	}
	if !body.closed.Load() {
		t.Error("the response body was not closed")
	}
	if n := body.read.Load(); n > maxDrain {
		t.Errorf("the sender read %d bytes of the response, want at most %d", n, maxDrain)
	}
}

// TestDefaultClientUsesNoProxy checks that the proxy variables of the
// environment do not reach the sender: a proxy would see the URL of a
// plain-http receiver, with the token that the URL can hold.
func TestDefaultClientUsesNoProxy(t *testing.T) {
	t.Parallel()
	tr, ok := newDefaultClient().Transport.(*http.Transport)
	if !ok {
		t.Fatal("the default transport is not an *http.Transport")
	}
	if tr.Proxy != nil {
		t.Error("the default client asks for a proxy")
	}
}
