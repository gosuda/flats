package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"
)

// maxDrain is how much of a response the sender reads before it closes the
// body. The sender needs the status only, and a receiver that sends more must
// not cost memory or time.
const maxDrain = 4 << 10

// Classes of failure. These are the only reasons a log line carries besides a
// status, because the text of an error from net/http holds the URL.
const (
	classConnect = "connect"
	classTimeout = "timeout"
	classTLS     = "tls"
	classOther   = "other"
)

type verdict int

const (
	delivered verdict = iota
	retry
	permanent
	cancelled
)

type outcome struct {
	verdict verdict
	class   string
}

// deliver makes the attempts of one event. It reads the keys once and uses the
// same values for every attempt, so a key that changes between the attempts
// of an event cannot split the event between two receivers.
func (s *Sender) deliver(ctx context.Context, ev *event) {
	if s.keys == nil {
		return
	}
	creds, err := s.keys(ctx)
	if err != nil {
		// A stop cancels the read, and a stop is not a fault of the keys.
		if !errors.Is(err, ErrOff) && ctx.Err() == nil {
			s.logf("webhook: event %s (%s) dropped: the keys could not be read", ev.id, ev.typ)
		}
		return
	}
	if creds.URL == "" || creds.Secret == "" {
		return
	}

	attempts := len(s.waits) + 1
	for n := 1; ; n++ {
		res := s.attempt(ctx, creds, ev)
		switch res.verdict {
		case delivered, cancelled:
			return
		case permanent:
			s.logf("webhook: event %s (%s) dropped: %s (not retried)", ev.id, ev.typ, res.class)
			return
		}
		if n == attempts {
			s.logf("webhook: event %s (%s) dropped: %s (attempt %d of %d)", ev.id, ev.typ, res.class, n, attempts)
			return
		}
		if !sleep(ctx, s.waits[n-1]) {
			return
		}
	}
}

// sleep waits for d and reports false when ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// attempt makes one request. Its time limit covers the connection, the
// request, the response headers and the part of the body that it reads.
func (s *Sender) attempt(ctx context.Context, creds Credentials, ev *event) outcome {
	actx, cancel := context.WithTimeout(ctx, s.limit)
	defer cancel()

	req, err := http.NewRequestWithContext(actx, http.MethodPost, creds.URL, bytes.NewReader(ev.body))
	if err != nil {
		// The URL does not parse, and a second try would not change that.
		return outcome{permanent, classOther}
	}
	stamp := s.now().Unix()
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", s.userAgent)
	h.Set("X-Flats-Event", ev.typ)
	h.Set("X-Flats-Event-Id", ev.id)
	h.Set("X-Flats-Timestamp", strconv.FormatInt(stamp, 10))
	h.Set("X-Flats-Signature", Sign(creds.Secret, stamp, ev.body))

	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return outcome{cancelled, ""}
		}
		return outcome{retry, classify(err)}
	}
	_, _ = io.CopyN(io.Discard, resp.Body, maxDrain)
	_ = resp.Body.Close()

	code := resp.StatusCode
	switch {
	case code >= 200 && code < 300:
		return outcome{delivered, ""}
	case code == http.StatusRequestTimeout, code == http.StatusTooManyRequests, code >= 500 && code < 600:
		return outcome{retry, "status " + strconv.Itoa(code)}
	}
	// Every other status and every redirect is the receiver's answer, and
	// asking again gives the same answer.
	return outcome{permanent, "status " + strconv.Itoa(code)}
}

// classify reduces an error of the HTTP client to a class that holds no part
// of the URL. An error that fits no class is "other" and is retried like a
// network error.
func classify(err error) string {
	var (
		verify  *tls.CertificateVerificationError
		unknown x509.UnknownAuthorityError
		invalid x509.CertificateInvalidError
		name    x509.HostnameError
		header  tls.RecordHeaderError
		alert   tls.AlertError
		ech     *tls.ECHRejectionError
		netErr  net.Error
		opErr   *net.OpError
		dnsErr  *net.DNSError
	)
	switch {
	case errors.As(err, &verify), errors.As(err, &unknown), errors.As(err, &invalid), errors.As(err, &name),
		errors.As(err, &header), errors.As(err, &alert), errors.As(err, &ech):
		return classTLS
	case errors.Is(err, context.DeadlineExceeded):
		return classTimeout
	case errors.As(err, &netErr) && netErr.Timeout():
		return classTimeout
	// On Windows a refused connection is not syscall.ECONNREFUSED, so the
	// failed dial is the sign that works on every system.
	case errors.Is(err, syscall.ECONNREFUSED), errors.As(err, &opErr) && opErr.Op == "dial", errors.As(err, &dnsErr):
		return classConnect
	}
	return classOther
}

// noRedirect makes the client return a 3xx response as it is.
func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// newDefaultClient builds the client of rule 13. It verifies certificates
// with the system roots and has no setting that turns that off, it follows no
// redirect, and it carries no time limit of its own because every attempt has
// a context deadline. It connects to the receiver itself and ignores the proxy
// variables of the environment: a proxy would get the URL of a plain-http
// receiver on the local network, with the token that the URL can hold.
func newDefaultClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:      true,
			TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:    10 * time.Second,
			MaxIdleConns:           2,
			MaxIdleConnsPerHost:    1,
			IdleConnTimeout:        30 * time.Second,
			MaxResponseHeaderBytes: 16 << 10,
		},
		CheckRedirect: noRedirect,
	}
}
