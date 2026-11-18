package egress

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/http/httpguts"
)

// The injected secrets of docs/spec/injected-fetch-secrets.md: the host adds a header to each request to one origin,
// and guest code never gets its value.

const (
	// MaxInjected is the number of injected secrets that one flat can have (rule 8).
	MaxInjected = 32
	// MaxInjectedValue is the size of the value of an injected secret (rule 7).
	MaxInjectedValue = 8 << 10
	// MaxInjectedName is the size of the header name of an injected secret (rule 6).
	MaxInjectedName = 128
)

// Header is one injected secret: the host sends Name with Value on each request to Origin. Origin is normalized and
// Name is canonical.
type Header struct {
	Origin string `json:"origin"`
	Name   string `json:"name"`
	Value  string `json:"value"`
}

// HostHeaderError is the refusal of a request whose guest headers hold a header that the host sets for its origin
// (rule 15). It names the header and never holds its value.
type HostHeaderError struct{ Name string }

func (e *HostHeaderError) Error() string {
	return "outbound header " + e.Name + " is set by the host for this origin: do not supply it"
}

func (e *HostHeaderError) Unwrap() error { return ErrInvalid }

// InjectOrigin returns the normalized origin of an injected secret (rule 5). Its error never repeats raw, which can
// hold user information or a query.
func InjectOrigin(raw string) (string, error) {
	u, normalized, err := target(raw)
	if err != nil || u == nil || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.ForceQuery {
		return "", errors.New("the origin must be one exact public origin such as https://api.example.com, with no path, query, fragment or user information")
	}
	if u.Scheme != "https" {
		return "", errors.New("the origin must use https, because an http request sends the header unencrypted")
	}
	return normalized, nil
}

// InjectHeader returns the canonical header name of an injected secret (rule 6).
func InjectHeader(name string) (string, error) {
	if name == "" || len(name) > MaxInjectedName || !httpguts.ValidHeaderFieldName(name) {
		return "", fmt.Errorf("the header must be a valid HTTP header name of at most %d bytes", MaxInjectedName)
	}
	canonical := http.CanonicalHeaderKey(name)
	if forbiddenHeader(name) || strings.EqualFold(name, "content-type") {
		return "", fmt.Errorf("the header %s cannot carry an injected secret, because the host or the runtime sets it", canonical)
	}
	return canonical, nil
}

// CheckInjectValue reports why value cannot be the value of an injected secret (rule 7), or nil. The error never
// holds the value. A value that starts or ends with a space or a tab is refused, because net/http removes those
// characters when it writes a header, and the host sends the stored bytes.
func CheckInjectValue(value string) error {
	switch {
	case value == "":
		return errors.New("the value of an injected secret cannot be empty")
	case len(value) > MaxInjectedValue:
		return fmt.Errorf("the value of an injected secret is larger than %d KiB", MaxInjectedValue>>10)
	case !utf8.ValidString(value):
		return errors.New("the value of an injected secret must be valid UTF-8")
	case !httpguts.ValidHeaderFieldValue(value):
		return errors.New("the value of an injected secret must be a valid HTTP header value, with no line break or other control character")
	case strings.ContainsAny(value[:1], " \t") || strings.ContainsAny(value[len(value)-1:], " \t"):
		return errors.New("the value of an injected secret cannot start or end with a space or a tab")
	}
	return nil
}

// injectedHeaders checks the list of a worker (rule 21) and groups it by origin.
func injectedHeaders(headers []Header) (map[string][]Header, error) {
	if len(headers) == 0 {
		return nil, nil
	}
	if len(headers) > MaxInjected {
		return nil, errors.New("too many injected secrets")
	}
	byOrigin := make(map[string][]Header, len(headers))
	for _, h := range headers {
		origin, err := InjectOrigin(h.Origin)
		if err != nil || origin != h.Origin {
			return nil, errors.New("invalid injected secret origin")
		}
		name, err := InjectHeader(h.Name)
		if err != nil || name != h.Name {
			return nil, errors.New("invalid injected secret header")
		}
		if CheckInjectValue(h.Value) != nil {
			return nil, errors.New("invalid injected secret value")
		}
		for _, other := range byOrigin[origin] {
			if other.Name == name {
				return nil, errors.New("two injected secrets for one origin and header")
			}
		}
		byOrigin[origin] = append(byOrigin[origin], h)
	}
	return byOrigin, nil
}
