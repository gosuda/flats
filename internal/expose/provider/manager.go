package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/expose/portal"
	"github.com/gosuda/flats/internal/expose/tsnet"
)

const (
	stateReady       = "ready"
	stateStarting    = "starting"
	stateError       = "error"
	stateUnavailable = "unavailable"
)

// ErrProviderNotPermitted means a non-local provider was requested without
// a host grant, without a per-flat permit, or on a draft/private route that
// cannot use a public provider. Callers should use errors.Is.
var ErrProviderNotPermitted = core.ErrProviderNotPermitted

// ErrNotConfigured means the host granted a provider but this process has
// no backend for it. The manager does not substitute another provider.
var ErrNotConfigured = errors.New("provider is not configured")

// ExposureRequest uses the exact core-defined DTO and permission identifiers.
type ExposureRequest = core.ExposureRequest

// ExposureEndpoint is one route this call opened or refused.
type ExposureEndpoint = core.ExposureEndpoint

// ExposureResult is every route considered for one request.
type ExposureResult = core.ExposureResult

// PublicStopResult reports Funnel and Portal after a public route is removed.
// Unconfirmed entries may still be reachable. Private routes are not listed
// because they are left up.
type PublicStopResult = core.PublicStopResult

// Tailnet is the private tailnet plus Funnel on the same nodes.
// A non-nil value is not permission to publish.
type Tailnet interface {
	Serve(ctx context.Context, host string, h http.Handler, ephemeral bool) (string, error)
	Stop(host string) error
	URL(host string) string
	Status() core.NetStatus
	Close() error
	ServeFunnel(ctx context.Context, host string, h http.Handler) (string, error)
	StopFunnel(host string) error
	FunnelState(host string) ExposureEndpoint
}

// PortalNet is the public Portal backend. A non-nil value is not permission.
type PortalNet interface {
	Serve(ctx context.Context, slug string, h http.Handler, hidden bool) (string, error)
	Stop(slug string) error
	URL(slug string) string
	Status() core.NetStatus
	Close() error
}

// TSNet adapts the concrete tailnet so callers outside this module can pass
// *tsnet.Net without a second implementation.
type TSNet struct{ *tsnet.Net }

// FunnelState implements Tailnet.
func (t TSNet) FunnelState(host string) ExposureEndpoint {
	if t.Net == nil {
		return ExposureEndpoint{Provider: Funnel, State: stateUnavailable, Detail: "tailscale is not configured"}
	}
	r := t.FunnelStatus(host)
	return ExposureEndpoint{Provider: Funnel, URL: r.URL, State: r.State, Detail: r.Detail}
}

// Options selects backends. Nil Tailscale or Portal leaves that provider
// unconfigured even when the host file grants it.
type Options struct {
	Local     *local.Net
	Tailscale Tailnet
	Portal    PortalNet
}

// Manager opens routes. It does not implement PrivateNet or PublicNet.
type Manager struct {
	dir    string
	file   File
	local  *local.Net
	ts     Tailnet
	portal PortalNet

	mu     sync.Mutex
	routes map[string]*route
}

type route struct {
	slug     string
	host     string
	audience Audience
	provider ID
}

// New loads dir's host file and keeps the supplied backends. It does not
// serve a flat and it does not contact a relay or a tailnet beyond whatever
// the backends already did.
func New(dir string, opts Options) (*Manager, error) {
	f, err := Load(dir)
	if err != nil {
		return nil, err
	}
	if opts.Local == nil {
		return nil, errors.New("provider: local network is required")
	}
	return &Manager{
		dir:    dir,
		file:   f,
		local:  opts.Local,
		ts:     opts.Tailscale,
		portal: opts.Portal,
		routes: map[string]*route{},
	}, nil
}

// File returns the host grants currently loaded.
func (m *Manager) File() File {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.file
}

func (m *Manager) allows(id ID) bool {
	m.mu.Lock()
	f := m.file
	m.mu.Unlock()
	return f.Allows(id)
}

// Reload reads the host file again. Existing routes are left as they are;
// a grant removed here does not by itself tear a route down.
func (m *Manager) Reload() error {
	f, err := Load(m.dir)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.file = f
	m.mu.Unlock()
	return nil
}

// ServeExposure opens every provider this request is allowed to use.
// Draft and private requests use local and, when both gates allow it, tailscale.
// Current public requests use tailscale-funnel and portal the same way.
// A provider that fails is not replaced by another one.
func (m *Manager) ServeExposure(ctx context.Context, req ExposureRequest) (ExposureResult, error) {
	if err := ctx.Err(); err != nil {
		return ExposureResult{}, err
	}
	if req.Slug == "" || req.Handler == nil {
		return ExposureResult{}, errors.New("provider: slug and handler are required")
	}
	switch req.Visibility {
	case "private", "public":
	default:
		return ExposureResult{}, fmt.Errorf("provider: visibility %q is not private or public", req.Visibility)
	}
	switch req.Audience {
	case AudienceDraft, AudienceCurrent:
	default:
		return ExposureResult{}, fmt.Errorf("provider: audience %q is not draft or current", req.Audience)
	}
	if req.Audience == AudienceDraft || req.Visibility == "private" {
		return m.servePrivate(ctx, req)
	}
	return m.servePublic(ctx, req)
}

func (m *Manager) servePrivate(ctx context.Context, req ExposureRequest) (ExposureResult, error) {
	var eps []ExposureEndpoint
	var errs []error
	ep, err := m.openLocal(ctx, req)
	eps = append(eps, ep)
	if err != nil {
		errs = append(errs, err)
	}
	if listed(req.Permitted, Tailscale) {
		ep, err := m.openTailscale(ctx, req)
		eps = append(eps, ep)
		if err != nil {
			errs = append(errs, err)
		}
	}
	for _, id := range []ID{Funnel, Portal} {
		if listed(req.Permitted, id) {
			eps = append(eps, refused(id, "drafts and private routes cannot use a public provider"))
			errs = append(errs, fmt.Errorf("%w: %s", ErrProviderNotPermitted, id))
		}
	}
	return ExposureResult{Endpoints: eps}, errors.Join(errs...)
}

func (m *Manager) servePublic(ctx context.Context, req ExposureRequest) (ExposureResult, error) {
	if req.Ephemeral {
		return ExposureResult{}, fmt.Errorf("%w: previews cannot use a public provider", ErrProviderNotPermitted)
	}
	var want []ID
	for _, id := range []ID{Funnel, Portal} {
		if listed(req.Permitted, id) {
			want = append(want, id)
		}
	}
	if len(want) == 0 {
		return ExposureResult{}, fmt.Errorf("%w: public visibility has no permitted public provider", ErrProviderNotPermitted)
	}
	var eps []ExposureEndpoint
	var errs []error
	opened := 0
	for _, id := range want {
		ep, err := m.openPublic(ctx, req, id)
		eps = append(eps, ep)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ep.State == stateReady || ep.State == stateStarting || ep.State == "needs-login" || ep.State == "key-expiring" {
			opened++
		}
	}
	if opened == 0 {
		if len(errs) == 0 {
			errs = append(errs, fmt.Errorf("%w: no public route opened", ErrProviderNotPermitted))
		}
		return ExposureResult{Endpoints: eps}, errors.Join(errs...)
	}
	// A sibling that was itself permitted may be up while another failed.
	// The error stays attached so the caller can see the failure; nothing
	// unpermitted was started in its place.
	return ExposureResult{Endpoints: eps}, errors.Join(errs...)
}

func (m *Manager) openLocal(ctx context.Context, req ExposureRequest) (ExposureEndpoint, error) {
	host := requestHost(req)
	url, err := m.local.Serve(ctx, host, req.Handler, req.Ephemeral)
	if err != nil {
		return ExposureEndpoint{Provider: Local, State: stateError, Detail: err.Error()}, err
	}
	m.track(req, Local, host)
	return fromStatus(Local, m.local.Status(), host, url), nil
}

func (m *Manager) openTailscale(ctx context.Context, req ExposureRequest) (ExposureEndpoint, error) {
	if !m.allows(Tailscale) {
		return refused(Tailscale, "host permission absent"), fmt.Errorf("%w: tailscale", ErrProviderNotPermitted)
	}
	if m.ts == nil {
		return refused(Tailscale, "tailscale is permitted but not configured"), fmt.Errorf("%w: tailscale", ErrNotConfigured)
	}
	host := requestHost(req)
	url, err := m.ts.Serve(ctx, host, req.Handler, req.Ephemeral)
	if err != nil {
		return ExposureEndpoint{Provider: Tailscale, State: stateError, Detail: err.Error()}, err
	}
	m.track(req, Tailscale, host)
	ep := fromStatus(Tailscale, m.ts.Status(), host, url)
	if ep.State == "" {
		ep.State = stateStarting
	}
	return ep, nil
}

func (m *Manager) openPublic(ctx context.Context, req ExposureRequest, id ID) (ExposureEndpoint, error) {
	if !m.allows(id) {
		return refused(id, "host permission absent"), fmt.Errorf("%w: %s", ErrProviderNotPermitted, id)
	}
	switch id {
	case Funnel:
		if m.ts == nil {
			return refused(Funnel, "funnel is permitted but tailscale is not configured"), fmt.Errorf("%w: tailscale-funnel", ErrNotConfigured)
		}
		url, err := m.ts.ServeFunnel(ctx, requestHost(req), req.Handler)
		if err != nil {
			return ExposureEndpoint{Provider: Funnel, State: stateError, Detail: err.Error()}, err
		}
		m.track(req, Funnel, requestHost(req))
		ep := m.ts.FunnelState(requestHost(req))
		ep.Provider = Funnel
		if ep.URL == "" {
			ep.URL = url
		}
		if ep.State == "" {
			ep.State = stateStarting
		}
		return ep, nil
	case Portal:
		if m.portal == nil {
			return refused(Portal, "portal is permitted but not configured"), fmt.Errorf("%w: portal", ErrNotConfigured)
		}
		url, err := m.portal.Serve(ctx, req.Slug, req.Handler, false)
		if err != nil {
			return ExposureEndpoint{Provider: Portal, State: stateError, Detail: err.Error()}, err
		}
		m.track(req, Portal, req.Slug)
		ep := fromStatus(Portal, m.portal.Status(), req.Slug, url)
		if ep.URL == "" {
			ep.URL = url
		}
		if ep.State == "" {
			ep.State = stateStarting
		}
		return ep, nil
	default:
		return refused(id, "not a public provider"), fmt.Errorf("%w: %s", ErrProviderNotPermitted, id)
	}
}

// StopPublicRoutes closes Funnel and Portal for slug. Local and tailscale
// routes for that slug keep serving. A stop error is Unconfirmed and the
// route stays tracked so a later call can retry it.
func (m *Manager) StopPublicRoutes(_ context.Context, slug string) (PublicStopResult, error) {
	funnelHost, hadFunnel := m.take(slug, Funnel, false)
	_, hadPortal := m.take(slug, Portal, false)
	var res PublicStopResult
	if hadFunnel {
		if m.ts == nil {
			res.Unconfirmed = append(res.Unconfirmed, Funnel)
		} else if err := m.ts.StopFunnel(funnelHost); err != nil {
			res.Unconfirmed = append(res.Unconfirmed, Funnel)
		} else if st := m.ts.FunnelState(funnelHost); st.State == stateReady || st.State == stateStarting {
			res.Unconfirmed = append(res.Unconfirmed, Funnel)
		} else {
			m.take(slug, Funnel, true)
			res.Stopped = append(res.Stopped, Funnel)
		}
	}
	if hadPortal {
		if m.portal == nil {
			res.Unconfirmed = append(res.Unconfirmed, Portal)
		} else if err := m.portal.Stop(slug); err != nil {
			res.Unconfirmed = append(res.Unconfirmed, Portal)
		} else {
			m.take(slug, Portal, true)
			res.Stopped = append(res.Stopped, Portal)
		}
	}
	if res.Stopped == nil {
		res.Stopped = []ID{}
	}
	if res.Unconfirmed == nil {
		res.Unconfirmed = []ID{}
	}
	return res, nil
}

// Close drops every route this manager opened. It does not close backends
// and it does not cancel a Portal parent context; the process owner closes
// those backends afterwards.
func (m *Manager) Close() error {
	m.mu.Lock()
	routes := m.routes
	m.routes = map[string]*route{}
	m.mu.Unlock()
	var errs []error
	seenPortal := map[string]bool{}
	seenFunnel := map[string]bool{}
	for _, r := range routes {
		switch r.provider {
		case Funnel:
			if seenFunnel[r.host] || m.ts == nil {
				continue
			}
			seenFunnel[r.host] = true
			if err := m.ts.StopFunnel(r.host); err != nil {
				errs = append(errs, err)
			}
		case Portal:
			if seenPortal[r.slug] || m.portal == nil {
				continue
			}
			seenPortal[r.slug] = true
			if err := m.portal.Stop(r.slug); err != nil {
				errs = append(errs, err)
			}
		case Tailscale:
			if m.ts != nil {
				if err := m.ts.Stop(r.host); err != nil {
					errs = append(errs, err)
				}
			}
		case Local:
			if err := m.local.Stop(r.host); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) track(req ExposureRequest, id ID, host string) {
	m.mu.Lock()
	m.routes[key(req.Slug, req.Audience, id)] = &route{
		slug: req.Slug, host: host, audience: req.Audience, provider: id,
	}
	m.mu.Unlock()
}

// take reports whether a route exists. drop removes it.
func (m *Manager) take(slug string, id ID, drop bool) (host string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, r := range m.routes {
		if r.slug == slug && r.provider == id {
			if drop {
				delete(m.routes, k)
			}
			return r.host, true
		}
	}
	return "", false
}

func key(slug string, audience Audience, id ID) string {
	return slug + "\x00" + string(audience) + "\x00" + string(id)
}

func requestHost(r ExposureRequest) string {
	if r.Host != "" {
		return r.Host
	}
	return r.Slug
}

func listed(ids []ID, id ID) bool {
	return slices.Contains(ids, id)
}

func refused(id ID, detail string) ExposureEndpoint {
	return ExposureEndpoint{Provider: id, State: stateUnavailable, Detail: detail}
}

func fromStatus(id ID, st core.NetStatus, host, url string) ExposureEndpoint {
	for _, h := range st.Hosts {
		if h.Host == host {
			ep := ExposureEndpoint{Provider: id, URL: h.URL, State: h.State, Detail: h.Detail}
			if ep.URL == "" {
				ep.URL = url
			}
			if ep.State == "" {
				ep.State = stateStarting
			}
			return ep
		}
	}
	state := stateStarting
	if url != "" && id == Local {
		state = stateReady
	}
	return ExposureEndpoint{Provider: id, URL: url, State: state}
}

// Compile-time checks for the real backends. Portal's concrete type is
// assigned below; TSNet covers Funnel.
var (
	_ PortalNet         = (*portal.Net)(nil)
	_ Tailnet           = TSNet{}
	_ core.LifecycleNet = (*Manager)(nil)
)
