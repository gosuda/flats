package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

const (
	// maxURL is the longest webhook URL in bytes.
	maxURL = 2048
	// minSecret is the shortest signing secret in bytes.
	minSecret = 16
)

// The checks return fixed texts. A text that quoted the value or the error of
// net/url would put the URL, which can hold a token, into a message that the
// caller prints.
var (
	errURLEmpty     = errors.New("the webhook URL is empty")
	errURLTooLong   = errors.New("the webhook URL is longer than 2048 bytes")
	errURLSyntax    = errors.New("the webhook URL is not a valid URL")
	errURLScheme    = errors.New("the webhook URL must use https, or http for localhost or a private address")
	errURLHost      = errors.New("the webhook URL needs a host")
	errURLFragment  = errors.New("the webhook URL must not have a fragment")
	errURLPort      = errors.New("the webhook URL has a port that is not between 1 and 65535")
	errURLPlainHost = errors.New("http is allowed only for localhost and for loopback, private, link-local and shared address literals")
	errSecretShort  = errors.New("the webhook secret needs at least 16 bytes")
)

// cgnat is the shared address space of RFC 6598, which tailnets and carriers
// use. netip has no predicate for it.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// Sign returns the signature of an attempt: "v1=" and the lower-case hex of
// HMAC-SHA256, keyed with the secret, over the decimal Unix time, a dot and
// the exact bytes of the body.
func Sign(secret string, unix int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	var stamp [20]byte
	mac.Write(strconv.AppendInt(stamp[:0], unix, 10))
	mac.Write([]byte{'.'})
	mac.Write(body)
	var sum [sha256.Size]byte
	return "v1=" + hex.EncodeToString(mac.Sum(sum[:0]))
}

// CheckURL reports whether raw may be stored as the webhook URL. The scheme is
// https, or http when the host is localhost or an IP literal in the loopback,
// private, link-local or shared (100.64.0.0/10) ranges, so that a receiver on
// the same machine or network works without a certificate. The URL needs a
// host, no fragment and at most 2048 bytes. User information and a query are
// allowed. Names are never resolved: a name that points at a private address
// still needs https. The error text never quotes the value.
func CheckURL(raw string) error {
	if raw == "" {
		return errURLEmpty
	}
	if len(raw) > maxURL {
		return errURLTooLong
	}
	for i := range len(raw) {
		if c := raw[i]; c <= ' ' || c == 0x7f {
			return errURLSyntax
		}
	}
	// A '#' starts a fragment even when nothing follows it, which url.Parse
	// reports as an empty Fragment.
	if strings.Contains(raw, "#") {
		return errURLFragment
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return errURLSyntax
	}
	if u.Hostname() == "" {
		return errURLHost
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return errURLPort
		}
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if plainHostOK(u.Hostname()) {
			return nil
		}
		return errURLPlainHost
	}
	return errURLScheme
}

// plainHostOK reports whether a host may be reached over http: localhost or
// an address literal of a range that does not leave the machine or network.
// url.Parse already lowered the scheme, and the host is lowered here.
func plainHostOK(host string) bool {
	host = strings.ToLower(host)
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.WithZone("").Unmap()
	return addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || cgnat.Contains(addr)
}

// CheckSecret reports whether secret is long enough to sign with. The caller
// checks the generic limits of an operator key.
func CheckSecret(secret string) error {
	if len(secret) < minSecret {
		return errSecretShort
	}
	return nil
}
