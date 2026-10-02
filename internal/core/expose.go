package core

import (
	"context"
	"net/http"
)

// PrivateNet serves handlers on the private network (one host per flat or
// preview). The Tailscale implementation gives every host its own tsnet node;
// the local implementation routes <host>.localhost on a loopback port.
type PrivateNet interface {
	// Serve starts serving h at host and returns the base URL. Ephemeral
	// hosts (previews) are removed from the network when stopped.
	Serve(ctx context.Context, host string, h http.Handler, ephemeral bool) (string, error)
	// Stop stops serving host. It is a no-op for unknown hosts.
	Stop(host string) error
	// URL returns the base URL host would have.
	URL(host string) string
	// Status reports per-host state for the console.
	Status() NetStatus
	Close() error
}

// PublicNet serves handlers to the internet (Portal).
type PublicNet interface {
	// Serve exposes h for slug. hidden=true keeps it out of relay listings.
	Serve(ctx context.Context, slug string, h http.Handler, hidden bool) (string, error)
	// SetHidden toggles relay listing for a served slug.
	SetHidden(slug string, hidden bool) error
	Stop(slug string) error
	URL(slug string) string
	Status() NetStatus
	Close() error
}

// NetStatus is a snapshot for the console.
type NetStatus struct {
	Kind    string     `json:"kind"`
	Enabled bool       `json:"enabled"`
	Detail  string     `json:"detail,omitempty"`
	Hosts   []HostInfo `json:"hosts"`
}

// HostInfo describes one served host.
type HostInfo struct {
	Host      string `json:"host"`
	URL       string `json:"url"`
	State     string `json:"state"` // starting | ready | error | needs-login | key-expiring
	Detail    string `json:"detail,omitempty"`
	KeyExpiry string `json:"key_expiry,omitempty"`
	Ephemeral bool   `json:"ephemeral,omitempty"`
}
