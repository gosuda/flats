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
		u, err := url.Parse(origin)
		if err != nil {
			return nil, fieldErr(path, "use http:// or https:// followed by a lowercase DNS hostname", "invalid origin %q", origin)
		}
		allowedScheme := u.Scheme == "http" || u.Scheme == "https"
		cleanURL := u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery
		if !allowedScheme || !cleanURL || origin != u.Scheme+"://"+u.Host {
			return nil, fieldErr(path, "omit user info, paths, queries and fragments", "invalid origin %q", origin)
		}
		host := u.Hostname()
		if host == "" || len(host) > 253 || net.ParseIP(host) != nil {
			return nil, fieldErr(path, "use a lowercase DNS hostname, not an IP address", "invalid hostname %q", host)
		}
		for label := range strings.SplitSeq(host, ".") {
			if !ConsoleHostRE.MatchString(label) {
				return nil, fieldErr(path, "use lowercase DNS labels without wildcards or a trailing dot", "invalid hostname %q", host)
			}
		}
		labels := strings.Split(host, ".")
		last := labels[len(labels)-1]
		localHost := host == "localhost" || strings.HasSuffix(host, ".localhost")
		if localHost || numericOriginLabel.MatchString(last) || !strings.ContainsAny(last, "abcdefghijklmnopqrstuvwxyz") {
			return nil, fieldErr(path, "use a non-local DNS hostname with an alphabetic final label", "invalid hostname %q", host)
		}
		authority := host
		if port := u.Port(); port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
				return nil, fieldErr(path, "use a port from 1 to 65535 without leading zeros", "invalid port %q", port)
			}
			authority += ":" + port
		}
		if u.Host != authority {
			return nil, fieldErr(path, "omit an empty port or brackets around DNS hostnames", "invalid authority %q", u.Host)
		}
		// Browsers omit a scheme's default port from Host. Reserve both
		// representations so the request authority identifies one origin.
		defaultPort := "443"
		if u.Scheme == "http" {
			defaultPort = "80"
		}
		keys := []string{authority}
		if u.Port() == "" || u.Port() == defaultPort {
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
