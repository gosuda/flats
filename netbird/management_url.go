package netbird

import (
	"errors"
	"net/url"
)

// validateManagementURL accepts NetBird's default (empty) or an HTTPS
// management origin, with no URL components that could carry credentials.
func validateManagementURL(raw string) error {
	if raw == "" {
		return nil // NetBird's default management server uses HTTPS.
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.RawFragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("management URL must be an HTTPS origin (host and optional port)")
	}
	return nil
}
