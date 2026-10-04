package core

import (
	"context"
	"errors"
	"fmt"
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

// ProviderID names a network provider. Public Tailscale is Funnel, not Serve.
type ProviderID string

const (
	ProviderLocal     ProviderID = "local"
	ProviderTailscale ProviderID = "tailscale"
	ProviderFunnel    ProviderID = "tailscale-funnel"
	ProviderPortal    ProviderID = "portal"
)

// ExposureAudience selects the content a provider serves.
type ExposureAudience string

const (
	AudienceCurrent ExposureAudience = "current"
	AudienceDraft   ExposureAudience = "draft"
)

// ExposureRequest is the core-to-network serving request.
// Draft and private audiences must not be placed on Funnel or Portal.
// A non-local provider is used only when it appears in Permitted.
type ExposureRequest struct {
	Slug           string
	Host           string
	Visibility     string // private | public
	Audience       ExposureAudience
	PrivateHandler http.Handler // private routes retained by a Public request
	Handler        http.Handler
	Ephemeral      bool
	Permitted      []ProviderID
}

// ExposureEndpoint is one route the network actually opened.
type ExposureEndpoint struct {
	Provider   ProviderID       `json:"provider"`
	URL        string           `json:"url,omitempty"`
	State      string           `json:"state"`
	Detail     string           `json:"detail,omitempty"`
	Configured bool             `json:"configured"`
	Permitted  bool             `json:"permitted"`
	Ready      bool             `json:"ready"`
	Audience   ExposureAudience `json:"audience,omitempty"`
	Host       string           `json:"host,omitempty"`
}

// ExposureResult is the set of routes opened for one request.
type ExposureResult struct {
	Endpoints []ExposureEndpoint
}

// PublicStopResult reports public routes after a private transition.
// Unconfirmed routes are still reachable; core does not mark the flat private.
type PublicStopResult struct {
	Stopped     []ProviderID
	Unconfirmed []ProviderID
}

// LifecycleNet is the optional network contract wired through Config.Lifecycle.
// Legacy PrivateNet and PublicNet adapters retain their existing methods.
type LifecycleNet interface {
	ServeExposure(ctx context.Context, req ExposureRequest) (ExposureResult, error)
	StopPublicRoutes(ctx context.Context, slug string) (PublicStopResult, error)
}

// ErrProviderNotPermitted is returned when a non-local provider is used
// without an explicit permission, or when a draft is sent to a public provider.
var ErrProviderNotPermitted = errors.New("provider not permitted")

// LifecycleObserver exposes manager-owned policy and current endpoint state.
// Policy must change whenever host grants or relevant configuration changes.
type LifecycleObserver interface {
	ExposurePolicy(context.Context) (string, error)
	ExposureStatus(context.Context, string) (ExposureResult, error)
}

// LifecyclePreviewNet stops only an exact isolated host and its private routes.
type LifecyclePreviewNet interface {
	StopExposure(context.Context, string) error
}

// LifecycleSlugNet retires every route and provider identity owned by one
// slug. It returns only after teardown is confirmed; a failure must retain
// enough registration state for the same call to be retried safely.
type LifecycleSlugNet interface {
	StopSlug(context.Context, string) error
}

// LifecycleRouteInspector reports registered routes, including routes which
// are still connecting or whose previous stop was not confirmed.
type LifecycleRouteInspector interface {
	HasProviderRoute(context.Context, string, ProviderID) (bool, error)
}

// ErrProviderInUse refuses permission removal until the provider's routes
// have been stopped. Permission and visibility remain unchanged on refusal.
var ErrProviderInUse = errors.New("provider has an active route")

// Provider and runtime availability remain compatible with ErrUnavailable,
// while transports can distinguish the operator action each requires.
var ErrProviderUnavailable = fmt.Errorf("%w: provider unavailable", ErrUnavailable)
var ErrRuntimeUnavailable = fmt.Errorf("%w: runtime unavailable", ErrUnavailable)

// LifecycleProviderStopper confirms removal of one flat's provider routes,
// including current, previews and redirect aliases owned by that flat.
type LifecycleProviderStopper interface {
	StopProviderRoutes(context.Context, string, ProviderID) error
}
