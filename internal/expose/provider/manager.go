package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
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
	stateStopped     = "stopped"
	stateUnavailable = "unavailable"
)

// ErrProviderNotPermitted means a non-local provider was requested without
// a host grant, without a per-flat permit, or on a draft/private route that
// cannot use a public provider. Callers should use errors.Is.
var ErrProviderNotPermitted = core.ErrProviderNotPermitted

// ErrNotConfigured means the host granted a provider but this process has
// no backend for it. The manager does not substitute another provider.
var ErrNotConfigured = fmt.Errorf("%w: provider is not configured", core.ErrProviderUnavailable)

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
	// Configuration is a canonical, non-secret desired backend configuration.
	// It must exclude credentials and transient connection readiness.
	Configuration string
	// Permission reads current per-flat opt-ins for status, without opening routes.
	// Nil is supported by standalone adapters; core still overlays current policy.
	Permission func(context.Context, string, ID) (bool, error)
}

// Manager opens routes. It does not implement PrivateNet or PublicNet.
type Manager struct {
	dir           string
	file          File
	local         *local.Net
	ts            Tailnet
	portal        PortalNet
	configuration string
	permission    func(context.Context, string, ID) (bool, error)

	mu     sync.Mutex
	routes map[string]*route
	states map[string][]observedEndpoint
}

type observedEndpoint struct {
	endpoint   ExposureEndpoint
	registered bool
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
		dir:           dir,
		file:          f,
		local:         opts.Local,
		ts:            opts.Tailscale,
		portal:        opts.Portal,
		configuration: opts.Configuration,
		permission:    opts.Permission,
		routes:        map[string]*route{},
		states:        map[string][]observedEndpoint{},
	}, nil
}

// File returns the host grants currently loaded.
func (m *Manager) File() File {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.file
	f.Permitted = slices.Clone(f.Permitted)
	f.Migration.GrantsFromState = slices.Clone(f.Migration.GrantsFromState)
	return f
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
func (m *Manager) ServeExposure(ctx context.Context, req ExposureRequest) (res ExposureResult, err error) {
	defer m.observe(req, &res)
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

func (m *Manager) configured(id ID) bool {
	switch id {
	case Local:
		return m.local != nil
	case Tailscale, Funnel:
		return m.ts != nil
	case Portal:
		return m.portal != nil
	}
	return false
}

func (m *Manager) observe(req ExposureRequest, res *ExposureResult) {
	if req.Slug == "" || (req.Audience != AudienceCurrent && req.Audience != AudienceDraft) {
		return
	}
	for i := range res.Endpoints {
		ep := &res.Endpoints[i]
		ep.Configured = m.configured(ep.Provider)
		ep.Permitted = ep.Provider == Local || (m.allows(ep.Provider) && listed(req.Permitted, ep.Provider))
		ep.Ready = ep.State == stateReady || ep.State == "key-expiring"
		ep.Audience, ep.Host = req.Audience, requestHost(req)
	}
	m.mu.Lock()
	observed := make([]observedEndpoint, len(res.Endpoints))
	for i, ep := range res.Endpoints {
		_, registered := m.routes[key(req.Slug, req.Audience, ep.Provider, requestHost(req))]
		observed[i] = observedEndpoint{endpoint: ep, registered: registered}
	}
	m.states[stateKey(req.Slug, req.Audience, requestHost(req))] = observed
	m.mu.Unlock()
}

// ExposurePolicy binds approval to host permission and effective runtime
// configuration. A backend skipped by --portal=false is absent from this token.
func (m *Manager) ExposurePolicy(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := m.Reload(); err != nil {
		return "", err
	}
	f := m.File()
	slices.Sort(f.Permitted)
	value := struct {
		Permitted         []ID   `json:"permitted"`
		PrivateBackend    string `json:"private_backend"`
		Tailscale, Portal bool
		Configuration     string
	}{f.Permitted, f.PrivateBackend, m.ts != nil, m.portal != nil, m.configuration}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// ExposureStatus reports observed routes, refreshing backend readiness without
// changing permission or opening a new route.
func (m *Manager) ExposureStatus(ctx context.Context, slug string) (ExposureResult, error) {
	if err := ctx.Err(); err != nil {
		return ExposureResult{}, err
	}
	m.mu.Lock()
	var observedEndpoints []observedEndpoint
	for k, observed := range m.states {
		if strings.HasPrefix(k, slug+"\x00") {
			observedEndpoints = append(observedEndpoints, observed...)
		}
	}
	slices.SortFunc(observedEndpoints, func(a, b observedEndpoint) int {
		ae, be := a.endpoint, b.endpoint
		return strings.Compare(string(ae.Audience)+"\x00"+ae.Host+"\x00"+string(ae.Provider), string(be.Audience)+"\x00"+be.Host+"\x00"+string(be.Provider))
	})
	routes := make(map[string]route)
	for k, r := range m.routes {
		if r.slug == slug {
			routes[k] = *r
		}
	}
	m.mu.Unlock()
	endpoints := make([]ExposureEndpoint, len(observedEndpoints))
	for i, observed := range observedEndpoints {
		ep := observed.endpoint
		if r, ok := routes[key(slug, ep.Audience, ep.Provider, ep.Host)]; ok {
			var fresh ExposureEndpoint
			switch ep.Provider {
			case Local:
				fresh = fromStatus(Local, m.local.Status(), r.host, m.local.URL(r.host))
			case Tailscale:
				fresh = fromStatus(Tailscale, m.ts.Status(), r.host, m.ts.URL(r.host))
			case Funnel:
				fresh = m.ts.FunnelState(r.host)
			case Portal:
				fresh = fromStatus(Portal, m.portal.Status(), r.host, m.portal.URL(r.host))
			}
			ep.URL, ep.State, ep.Detail = fresh.URL, fresh.State, fresh.Detail
		} else if observed.registered {
			ep.URL, ep.State, ep.Detail = "", stateStopped, "route stopped"
		}
		permitted := ep.Permitted
		if m.permission != nil && ep.Provider != Local {
			var err error
			permitted, err = m.permission(ctx, slug, ep.Provider)
			if err != nil {
				return ExposureResult{}, err
			}
		}
		ep.Permitted = permitted && m.allows(ep.Provider)
		ep.Ready = ep.State == stateReady || ep.State == "key-expiring"
		endpoints[i] = ep
	}
	if endpoints == nil {
		endpoints = []ExposureEndpoint{}
	}
	return ExposureResult{Endpoints: endpoints}, nil
}

// HostStatus separates backend configuration and host permission from a flat's
// opt-in and actual route readiness. Setup alone never opens a route.
func (m *Manager) HostStatus() []ExposureEndpoint {
	f := m.File()
	out := make([]ExposureEndpoint, 0, 4)
	for _, id := range []ID{Local, Tailscale, Funnel, Portal} {
		configured := m.configured(id)
		detail := "backend is not configured for this process"
		if configured {
			detail = "backend configured; route readiness is reported per flat"
		}
		out = append(out, ExposureEndpoint{Provider: id, Configured: configured,
			Permitted: f.Allows(id), State: stateUnavailable, Detail: detail})
	}
	return out
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
	// Public current versions retain their independent Private route, including
	// on restart when this manager has no previously tracked Local listener.
	private, privateErr := m.retainPrivate(ctx, req, Local)
	eps = append(eps, private)
	if privateErr != nil {
		errs = append(errs, privateErr)
	}
	if listed(req.Permitted, Tailscale) {
		tail, err := m.retainPrivate(ctx, req, Tailscale)
		eps = append(eps, tail)
		if err != nil {
			errs = append(errs, err)
		}
	}
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

func (m *Manager) retainPrivate(ctx context.Context, req ExposureRequest, id ID) (ExposureEndpoint, error) {
	m.mu.Lock()
	r := m.routes[key(req.Slug, req.Audience, id, requestHost(req))]
	var host string
	if r != nil {
		host = r.host
	}
	m.mu.Unlock()
	if host != "" {
		if id == Local {
			return fromStatus(Local, m.local.Status(), host, m.local.URL(host)), nil
		}
		return fromStatus(Tailscale, m.ts.Status(), host, m.ts.URL(host)), nil
	}
	if id == Local {
		return m.openLocal(ctx, req)
	}
	return m.openTailscale(ctx, req)
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
			return ExposureEndpoint{Provider: Funnel, State: stateError, Detail: err.Error()}, fmt.Errorf("%w: tailscale-funnel: %w", core.ErrProviderNotReady, err)
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
			return ExposureEndpoint{Provider: Portal, State: stateError, Detail: err.Error()}, fmt.Errorf("%w: portal: %w", core.ErrProviderNotReady, err)
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
			m.pruneState(slug, Funnel)
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
			m.pruneState(slug, Portal)
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

// StopProviderRoutes stops only Private Tailscale registrations owned by slug.
// Successful stops are forgotten; failed registrations remain available for retry.
func (m *Manager) StopProviderRoutes(ctx context.Context, slug string, id ID) error {
	if id != Tailscale {
		return fmt.Errorf("unsupported private provider stop: %s", id)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	routes := make(map[string]route)
	for k, r := range m.routes {
		if r.slug == slug && r.provider == id {
			routes[k] = *r
		}
	}
	m.mu.Unlock()
	var errs []error
	for k, r := range routes {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if m.ts == nil {
			errs = append(errs, ErrNotConfigured)
			continue
		}
		var stopErr error
		if private, ok := m.ts.(interface{ StopPrivate(string) error }); ok {
			stopErr = private.StopPrivate(r.host)
		} else {
			// A legacy backend Stop may retire a shared Funnel node. Refuse rather
			// than silently altering the separately approved Public route.
			m.mu.Lock()
			sharedFunnel := false
			for _, sibling := range m.routes {
				if sibling.host == r.host && sibling.provider == Funnel {
					sharedFunnel = true
					break
				}
			}
			m.mu.Unlock()
			if sharedFunnel {
				stopErr = errors.New("private-only teardown is unavailable; approve Private access to stop Funnel first")
			} else {
				stopErr = m.ts.Stop(r.host)
			}
		}
		if err := stopErr; err != nil {
			errs = append(errs, err)
			continue
		}
		m.mu.Lock()
		delete(m.routes, k)
		m.pruneStateLocked(r.slug, r.audience, r.host, r.provider)
		m.mu.Unlock()
	}
	return errors.Join(errs...)
}

var _ core.LifecycleProviderStopper = (*Manager)(nil)

// StopExposure removes private routes for this exact host. Failed
// stops stay tracked so cleanup can be retried without losing honest status.
func (m *Manager) StopExposure(ctx context.Context, host string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	routes := make(map[string]route)
	for k, r := range m.routes {
		if r.host == host && (r.provider == Local || r.provider == Tailscale) {
			routes[k] = *r
		}
	}
	m.mu.Unlock()
	var errs []error
	for k, r := range routes {
		var err error
		if r.provider == Local {
			err = m.local.Stop(host)
		} else if m.ts != nil {
			err = m.ts.Stop(host)
		} else {
			err = ErrNotConfigured
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		m.mu.Lock()
		delete(m.routes, k)
		m.pruneStateLocked(r.slug, r.audience, r.host, r.provider)
		m.mu.Unlock()
	}
	return errors.Join(errs...)
}

var _ core.LifecyclePreviewNet = (*Manager)(nil)

// Close forgets every registration this manager opened. The process owner
// closes the backends afterwards. In particular, it must not call Tailnet.Stop:
// that operation logs a node out and deletes its persistent identity, while a
// normal process shutdown must let Tailnet.Close preserve that identity.
func (m *Manager) Close() error {
	m.mu.Lock()
	m.routes = map[string]*route{}
	m.states = map[string][]observedEndpoint{}
	m.mu.Unlock()
	return nil
}

// pruneState removes successfully stopped endpoints while retaining state for
// routes whose teardown is still unconfirmed. That keeps status honest without
// accumulating every preview or stopped route for the life of the process.
func (m *Manager) pruneState(slug string, id ID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, endpoints := range m.states {
		if !strings.HasPrefix(k, slug+"\x00") {
			continue
		}
		filtered := endpoints[:0]
		for _, observed := range endpoints {
			if observed.endpoint.Provider != id {
				filtered = append(filtered, observed)
			}
		}
		if len(filtered) == 0 {
			delete(m.states, k)
		} else {
			m.states[k] = slices.Clone(filtered)
		}
	}
}

func (m *Manager) pruneStateLocked(slug string, audience Audience, host string, id ID) {
	k := stateKey(slug, audience, host)
	endpoints := m.states[k]
	filtered := endpoints[:0]
	for _, observed := range endpoints {
		if observed.endpoint.Provider != id {
			filtered = append(filtered, observed)
		}
	}
	if len(filtered) == 0 {
		delete(m.states, k)
	} else {
		m.states[k] = slices.Clone(filtered)
	}
}

func (m *Manager) track(req ExposureRequest, id ID, host string) {
	m.mu.Lock()
	m.routes[key(req.Slug, req.Audience, id, requestHost(req))] = &route{
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

func stateKey(slug string, audience Audience, host string) string {
	return slug + "\x00" + string(audience) + "\x00" + host
}

func key(slug string, audience Audience, id ID, host string) string {
	return stateKey(slug, audience, host) + "\x00" + string(id)
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
	_ PortalNet              = (*portal.Net)(nil)
	_ Tailnet                = TSNet{}
	_ core.LifecycleNet      = (*Manager)(nil)
	_ core.LifecycleObserver = (*Manager)(nil)
)

// HasProviderRoute inspects registration rather than readiness: connecting or
// failed routes can still recover, so revocation must not persist over them.
func (m *Manager) HasProviderRoute(ctx context.Context, slug string, id ID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.routes {
		if r.slug == slug && r.provider == id {
			return true, nil
		}
	}
	return false, nil
}

var _ core.LifecycleRouteInspector = (*Manager)(nil)
