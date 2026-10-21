package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ExternalOriginsKey declares the origins through which a reverse proxy serves
// the management listener. These origins grant no authentication by themselves.
const ExternalOriginsKey = "console.external_origins"

// Browsers interpret numeric and hexadecimal final labels as IPv4 candidates.
var numericOriginLabel = regexp.MustCompile(`^(?:[0-9]+|0x[0-9a-f]+)$`)

func checkExternalOrigins(v any) (any, error) {
	origins := v.([]string)
	authorities := make(map[string]string, len(origins))
	for i, origin := range origins {
		path := fmt.Sprintf("%s[%d]", ExternalOriginsKey, i)
		scheme, host, port, err := checkOriginURL(origin, path)
		if err != nil {
			return nil, err
		}
		// Browsers omit a scheme's default port from Host. Reserve both
		// representations so the request authority identifies one origin.
		defaultPort := defaultPortOf(scheme)
		keys := []string{host + ":" + port}
		if port == "" || port == defaultPort {
			keys = []string{host, host + ":" + defaultPort}
		}
		for _, key := range keys {
			if previous, exists := authorities[key]; exists {
				return nil, fieldErr(path, "list each authority once with one scheme", "%q conflicts with %q", origin, previous)
			}
		}
		for _, key := range keys {
			authorities[key] = origin
		}
	}
	return slices.Clone(origins), nil
}

// checkOriginURL accepts an http or https origin whose host is a non-local
// lowercase DNS name, with an optional port and nothing else. It returns the
// scheme, the host and the port ("" when absent). Errors use path.
func checkOriginURL(origin, path string) (scheme, host, port string, err error) {
	u, err := url.Parse(origin)
	if err != nil {
		return "", "", "", fieldErr(path, "use http:// or https:// followed by a lowercase DNS hostname", "invalid origin %q", origin)
	}
	allowedScheme := u.Scheme == "http" || u.Scheme == "https"
	cleanURL := u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery
	if !allowedScheme || !cleanURL || origin != u.Scheme+"://"+u.Host {
		return "", "", "", fieldErr(path, "omit user info, paths, queries and fragments", "invalid origin %q", origin)
	}
	host = u.Hostname()
	if host == "" || len(host) > 253 || net.ParseIP(host) != nil {
		return "", "", "", fieldErr(path, "use a lowercase DNS hostname, not an IP address", "invalid hostname %q", host)
	}
	for label := range strings.SplitSeq(host, ".") {
		if !ConsoleHostRE.MatchString(label) {
			return "", "", "", fieldErr(path, "use lowercase DNS labels without wildcards or a trailing dot", "invalid hostname %q", host)
		}
	}
	labels := strings.Split(host, ".")
	last := labels[len(labels)-1]
	localHost := host == "localhost" || strings.HasSuffix(host, ".localhost")
	if localHost || numericOriginLabel.MatchString(last) || !strings.ContainsAny(last, "abcdefghijklmnopqrstuvwxyz") {
		return "", "", "", fieldErr(path, "use a non-local DNS hostname with an alphabetic final label", "invalid hostname %q", host)
	}
	authority := host
	if port = u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return "", "", "", fieldErr(path, "use a port from 1 to 65535 without leading zeros", "invalid port %q", port)
		}
		authority += ":" + port
	}
	if u.Host != authority {
		return "", "", "", fieldErr(path, "omit an empty port or brackets around DNS hostnames", "invalid authority %q", u.Host)
	}
	return u.Scheme, host, port, nil
}

// defaultPortOf is the port browsers leave out of Host for scheme.
func defaultPortOf(scheme string) string {
	if scheme == "http" {
		return "80"
	}
	return "443"
}
