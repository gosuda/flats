package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
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
	ProviderZrok      ProviderID = "zrok"
	// ProviderCloudflareQuick is a Cloudflare quick tunnel: anonymous, on a
	// random trycloudflare.com hostname.
	ProviderCloudflareQuick ProviderID = "cloudflare-quick"
	// ProviderNetbird is the private NetBird mesh; ProviderNetbirdExpose is
	// NetBird's public reverse proxy. Both use the host's one NetBird peer.
	ProviderNetbird       ProviderID = "netbird"
	ProviderNetbirdExpose ProviderID = "netbird-expose"
	// ProviderProxy and ProviderProxyPublic are the operator's reverse proxy,
	// as a Private and a Public provider. They share one listener, so the
	// host permits at most one of them.
	ProviderProxy       ProviderID = "proxy"
	ProviderProxyPublic ProviderID = "proxy-public"
)

// PublicProvider reports whether id serves visitors on the internet
// (Tailscale Funnel, Portal, zrok, a Cloudflare quick tunnel, NetBird Expose
// or a public reverse proxy).
func PublicProvider(id ProviderID) bool {
	return id == ProviderFunnel || id == ProviderPortal || id == ProviderZrok || id == ProviderCloudflareQuick || id == ProviderNetbirdExpose || id == ProviderProxyPublic
}

// PrivateMeshProvider reports whether id is a private overlay network that
// serves a flat's Private route besides loopback (Tailscale or NetBird).
func PrivateMeshProvider(id ProviderID) bool {
	return id == ProviderTailscale || id == ProviderNetbird
}

// PrivateRouteProvider reports whether id carries a flat's Private route
// besides loopback: a private mesh or the Private reverse proxy.
func PrivateRouteProvider(id ProviderID) bool {
	return PrivateMeshProvider(id) || id == ProviderProxy
}

type routeProviderKey struct{}

// WithRouteProvider marks a route request with the provider it arrived on.
func WithRouteProvider(ctx context.Context, id ProviderID) context.Context {
	return context.WithValue(ctx, routeProviderKey{}, id)
}

// RouteProvider returns the provider a route request arrived on. Requests
// served without the provider manager are not marked.
func RouteProvider(ctx context.Context) (ProviderID, bool) {
	id, ok := ctx.Value(routeProviderKey{}).(ProviderID)
	return id, ok
}

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
	// Hidden keeps a Portal route out of relay listings. It is not access
	// control: anyone with the URL can still open it.
	Hidden bool
	// SignIn is what the open of a Portal route carries for its sign-in
	// gate (docs/spec/portal-sign-in.md, rule 38). Nil carries neither the
	// setting nor the mark redirect, and keeps the gate that the route has.
	SignIn *PortalSignIn
}

// PortalSignIn is the sign-in that an open of the Portal route of a flat
// carries (docs/spec/portal-sign-in.md, rule 38): the stored setting and its
// sequence, which the host reads under the lock of the flat, or the mark
// redirect of rule 41. Gate holds the client secret, so a PortalSignIn is
// never logged.
type PortalSignIn struct {
	// Redirect is the mark of rule 41: the route serves the redirect of an
	// old slug and has no gate.
	Redirect bool
	// Seq is the sign-in sequence of the setting, and -1 when the setting
	// could not be read.
	Seq int64
	// Gate is the gate to build. Its zero value is sign-in off.
	Gate SignInGate
	// Err is why no gate can be built although sign-in may be on: the
	// setting or the client secret could not be read (rule 35). The route
	// then serves nothing.
	Err error
	// Rebuild builds the gate again also when a gate of Seq is in place,
	// because its client secret changed (rule 28).
	Rebuild bool
}

// On reports whether the open asks for a gate. A nil open does not.
func (p *PortalSignIn) On() bool {
	return p != nil && !p.Redirect && (p.Err != nil || p.Gate.Provider != "")
}

// String writes the open without the client secret.
func (p PortalSignIn) String() string {
	switch {
	case p.Redirect:
		return "sign-in open: redirect"
	case p.Err != nil:
		return fmt.Sprintf("sign-in open %d: %v", p.Seq, p.Err)
	}
	return fmt.Sprintf("sign-in open %d: %s", p.Seq, p.Gate)
}

// GoString keeps %#v from writing the client secret.
func (p PortalSignIn) GoString() string { return p.String() }

// SignInGateError is an open of a Portal route that serves no flat because its
// sign-in gate could not be built (docs/spec/portal-sign-in.md, rule 35). Its
// text is the detail of the Portal endpoint. StopErr is set when the route was
// served and its stop is not confirmed, so the route stays registered; it then
// answers 503 and never the flat.
type SignInGateError struct {
	Reason  string
	StopErr error
}

func (e *SignInGateError) Error() string {
	if e.StopErr != nil {
		return "sign-in gate: " + e.Reason + "; the stop of the route is not confirmed: " + e.StopErr.Error()
	}
	return "sign-in gate: " + e.Reason
}

// ErrNoPortalRoute reports a flat whose Portal route has no relay with a
// canonical host name, or that has no Portal route (rule 50).
var ErrNoPortalRoute = errors.New("no Portal route with a ready relay")

// PortalCredential is an access link of the Portal route (rule 51): the link
// and the canonical host name that it names. The link is in no log, event,
// audit row or webhook body (rule 52).
type PortalCredential struct {
	URL  string
	Host string
}

// String keeps the link out of a formatted credential.
func (c PortalCredential) String() string { return "access link for " + c.Host }

// GoString keeps %#v from writing the link.
func (c PortalCredential) GoString() string { return c.String() }

// LifecycleSignInNet is the sign-in part of the Portal route
// (docs/spec/portal-sign-in.md). A provider manager without it has no Portal
// identity for the host to renew and no access link to issue.
type LifecycleSignInNet interface {
	// RenewPortalIdentity gives the Portal identity of slug a new token
	// secret (rules 41, 42 and 54) with the write of rule 36, and builds the
	// gate of a served route again with it. It returns the path of the
	// identity file, also with an error, and false without an error when
	// slug has no identity file.
	RenewPortalIdentity(ctx context.Context, slug string) (path string, renewed bool, err error)
	// IssuePortalCredential makes an access link of rule 51 from the
	// identity of the served route of slug, for the canonical host name of
	// its first relay. ErrNoPortalRoute reports a slug without a route or a
	// route without a canonical host name.
	IssuePortalCredential(ctx context.Context, slug, subject string, expiresAt time.Time) (PortalCredential, error)
	// PortalHosts returns the host names of the PublicHosts of the Portal
	// route of slug (rule 34), and nil without a route.
	PortalHosts(slug string) []string
}

// SignInPublicNet is a Portal backend that runs the sign-in gate
// (docs/spec/portal-sign-in.md, rules 33 to 40). Serve of a backend without it
// never serves a flat with sign-in on.
type SignInPublicNet interface {
	// ServeSignIn is Serve with the sign-in that the open carries.
	ServeSignIn(ctx context.Context, slug string, h http.Handler, hidden bool, si *PortalSignIn) (string, error)
	LifecycleSignInNet
}

type signInGateKey struct{}

// WithSignInGate marks a request that a sign-in gate built from the setting of
// sequence seq let in (rule 33). The Portal route sets it inside its gate, and
// the public site handler compares it with the stored sequence (rule 44).
func WithSignInGate(ctx context.Context, seq int64) context.Context {
	return context.WithValue(ctx, signInGateKey{}, seq)
}

// SignInGateSeq returns the sequence of the gate that let a request in.
func SignInGateSeq(ctx context.Context) (int64, bool) {
	seq, ok := ctx.Value(signInGateKey{}).(int64)
	return seq, ok
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

// LifecycleListingNet changes the relay listing of a registered Portal route
// without reopening it. A slug without one is a no-op.
type LifecycleListingNet interface {
	SetPortalHidden(slug string, hidden bool) error
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
