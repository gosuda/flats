package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
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
	// TailscaleStateDir is the local tsnet state root. Manager uses it only
	// when no tailnet backend is configured, so deleting a legacy Local-only
	// flat can discard its old identity without contacting control.
	TailscaleStateDir string
	// Configuration is a canonical, non-secret desired backend configuration.
	// It must exclude credentials and transient connection readiness.
	Configuration string
	// Permission reads current per-flat opt-ins for status, without opening routes.
	// Nil is supported by standalone adapters; core still overlays current policy.
	Permission func(context.Context, string, ID) (bool, error)
	// Grants, when set, are the host grants (Permitted and PrivateBackend)
	// from config.json. The manager then neither reads nor writes the host
	// file, and Reload keeps these grants until the process restarts.
	Grants *File
}

// Manager opens routes. It does not implement PrivateNet or PublicNet.
type Manager struct {
	dir   string
	file  File
	fixed bool // grants came from Options.Grants
	local *local.Net
	// ts and portal may be attached after New (nil to set, never removed);
	// read them through tailnet and portalNet.
	bmu           sync.RWMutex
	ts            Tailnet
	portal        PortalNet
	tailscaleDir  string
	retirementDir string
	configuration string
	permission    func(context.Context, string, ID) (bool, error)

	gmu    sync.Mutex // serializes SetGrant's read-modify-write of the host file
	mu     sync.Mutex
	routes map[string]*route
	states map[string][]observedEndpoint
	// pendingTailnet records node retirement failures after Funnel itself is
	// confirmed closed. These are cleanup obligations, not public routes.
	pendingTailnet map[string]map[string]struct{} // slug -> hosts
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
	var f File
	if opts.Grants != nil {
		f = File{Version: 1, Permitted: slices.Clone(opts.Grants.Permitted), PrivateBackend: opts.Grants.PrivateBackend}
		if f.Permitted == nil {
			f.Permitted = []ID{}
		}
		f.Migration.GrantsFromState = []ID{}
		if err := validate(f); err != nil {
			return nil, err
		}
	} else {
		var err error
		if f, err = Load(dir); err != nil {
			return nil, err
		}
	}
	if opts.Local == nil {
		return nil, errors.New("provider: local network is required")
	}
	tailscaleDir := opts.TailscaleStateDir
	if tailscaleDir == "" {
		tailscaleDir = filepath.Join(dir, "tsnet")
	}
	return &Manager{
		dir:            dir,
		file:           f,
		fixed:          opts.Grants != nil,
		local:          opts.Local,
		ts:             opts.Tailscale,
		portal:         opts.Portal,
		tailscaleDir:   tailscaleDir,
		retirementDir:  filepath.Join(dir, "network-retirements"),
		configuration:  opts.Configuration,
		permission:     opts.Permission,
		routes:         map[string]*route{},
		states:         map[string][]observedEndpoint{},
		pendingTailnet: map[string]map[string]struct{}{},
	}, nil
}

func (m *Manager) tailnet() Tailnet {
	m.bmu.RLock()
	defer m.bmu.RUnlock()
	return m.ts
}

func (m *Manager) portalNet() PortalNet {
	m.bmu.RLock()
	defer m.bmu.RUnlock()
	return m.portal
}

// AttachTailnet configures the Tailscale backend once, for a provider the
// operator enables while the host runs. It does not grant the provider.
func (m *Manager) AttachTailnet(t Tailnet) {
	m.bmu.Lock()
	defer m.bmu.Unlock()
	if m.ts == nil {
		m.ts = t
	}
}

// AttachPortal configures the Portal backend once. It does not grant Portal.
func (m *Manager) AttachPortal(p PortalNet) {
	m.bmu.Lock()
	defer m.bmu.Unlock()
	if m.portal == nil {
		m.portal = p
	}
}

// SetGrant records or removes a host grant. With Options.Grants (from
// config.json) it changes only this process's grants; the caller saves
// config.json. Otherwise it rewrites the host file. Local is always
// permitted. Removing a grant does not tear routes down; callers refuse a
// removal while flats still permit the provider.
func (m *Manager) SetGrant(id ID, permitted bool) (File, error) {
	if _, err := ParseID(string(id)); err != nil {
		return File{}, err
	}
	if id == Local {
		if !permitted {
			return File{}, errors.New("provider: local is always permitted")
		}
		return m.File(), nil
	}
	m.gmu.Lock()
	defer m.gmu.Unlock()
	f := m.File()
	if !m.fixed {
		var err error
		if f, err = Load(m.dir); err != nil {
			return File{}, err
		}
	}
	if permitted {
		var err error
		if f, err = f.Grant(id); err != nil {
			return File{}, err
		}
	} else {
		f.Permitted = slices.DeleteFunc(slices.Clone(f.Permitted), func(x ID) bool { return x == id })
	}
	if m.fixed {
		m.mu.Lock()
		m.file = f
		m.mu.Unlock()
		return m.File(), nil
	}
	if err := Save(m.dir, f); err != nil {
		return File{}, err
	}
	if err := m.Reload(); err != nil {
		return File{}, err
	}
	return m.File(), nil
}

// HostAllows reports the host grant for id.
func (m *Manager) HostAllows(id ID) bool { return m.allows(id) }

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
// a grant removed here does not by itself tear a route down. Grants from
// Options.Grants are kept: config.json changes apply on restart.
func (m *Manager) Reload() error {
	if m.fixed {
		return nil
	}
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
		return m.tailnet() != nil
	case Portal:
		return m.portalNet() != nil
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
	}{f.Permitted, f.PrivateBackend, m.tailnet() != nil, m.portalNet() != nil, m.configuration}
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
				fresh = fromStatus(Tailscale, m.tailnet().Status(), r.host, m.tailnet().URL(r.host))
			case Funnel:
				fresh = m.tailnet().FunnelState(r.host)
			case Portal:
				fresh = fromStatus(Portal, m.portalNet().Status(), r.host, m.portalNet().URL(r.host))
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
		return fromStatus(Tailscale, m.tailnet().Status(), host, m.tailnet().URL(host)), nil
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
	if m.tailnet() == nil {
		return refused(Tailscale, "tailscale is permitted but not configured"), fmt.Errorf("%w: tailscale", ErrNotConfigured)
	}
	host := requestHost(req)
	url, err := m.tailnet().Serve(ctx, host, req.Handler, req.Ephemeral)
	if err != nil {
		return ExposureEndpoint{Provider: Tailscale, State: stateError, Detail: err.Error()}, err
	}
	m.track(req, Tailscale, host)
	ep := fromStatus(Tailscale, m.tailnet().Status(), host, url)
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
		if m.tailnet() == nil {
			return refused(Funnel, "funnel is permitted but tailscale is not configured"), fmt.Errorf("%w: tailscale-funnel", ErrNotConfigured)
		}
		host := requestHost(req)
		pending, err := m.hasPendingTailnet(req.Slug, host)
		if err != nil {
			return ExposureEndpoint{Provider: Funnel, State: stateError, Detail: err.Error()}, fmt.Errorf("%w: tailscale-funnel cleanup state: %w", core.ErrProviderNotReady, err)
		}
		if pending {
			if err := m.tailnet().Stop(host); err != nil {
				return ExposureEndpoint{Provider: Funnel, State: stateError, Detail: "previous Funnel identity retirement is still pending: " + err.Error()}, fmt.Errorf("%w: tailscale-funnel cleanup: %w", core.ErrProviderNotReady, err)
			}
			if err := m.clearPendingTailnet(req.Slug, host); err != nil {
				return ExposureEndpoint{Provider: Funnel, State: stateError, Detail: err.Error()}, fmt.Errorf("%w: tailscale-funnel cleanup record: %w", core.ErrProviderNotReady, err)
			}
		}
		url, err := m.tailnet().ServeFunnel(ctx, host, req.Handler)
		if err != nil {
			return ExposureEndpoint{Provider: Funnel, State: stateError, Detail: err.Error()}, fmt.Errorf("%w: tailscale-funnel: %w", core.ErrProviderNotReady, err)
		}
		m.track(req, Funnel, host)
		ep := m.tailnet().FunnelState(host)
		ep.Provider = Funnel
		if ep.URL == "" {
			ep.URL = url
		}
		if ep.State == "" {
			ep.State = stateStarting
		}
		return ep, nil
	case Portal:
		if m.portalNet() == nil {
			return refused(Portal, "portal is permitted but not configured"), fmt.Errorf("%w: portal", ErrNotConfigured)
		}
		url, err := m.portalNet().Serve(ctx, req.Slug, req.Handler, false)
		if err != nil {
			return ExposureEndpoint{Provider: Portal, State: stateError, Detail: err.Error()}, fmt.Errorf("%w: portal: %w", core.ErrProviderNotReady, err)
		}
		m.track(req, Portal, req.Slug)
		ep := fromStatus(Portal, m.portalNet().Status(), req.Slug, url)
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

type retirementMarker struct {
	slug string
	host string
}

var retirementComponentRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func newRetirementMarker(slug, host string) (retirementMarker, error) {
	for label, value := range map[string]string{"slug": slug, "host": host} {
		if value == "" || value == "." || value == ".." || !filepath.IsLocal(value) ||
			strings.ContainsAny(value, `/\`) || strings.Contains(value, "..") || filepath.Base(value) != value ||
			!retirementComponentRE.MatchString(value) {
			return retirementMarker{}, fmt.Errorf("provider: invalid retirement %s %q", label, value)
		}
	}
	return retirementMarker{slug: slug, host: host}, nil
}

func (r retirementMarker) name() string { return filepath.Join(r.slug, r.host) }

func (m *Manager) openRetirementRoot(create bool) (*os.Root, error) {
	parent, name := filepath.Dir(m.retirementDir), filepath.Base(m.retirementDir)
	parentRoot, err := os.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	defer parentRoot.Close()
	if create {
		if err := parentRoot.MkdirAll(name, 0o700); err != nil {
			return nil, err
		}
	}
	info, err := parentRoot.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("provider: retirement root is not a directory")
	}
	return parentRoot.OpenRoot(name)
}

func (m *Manager) markPendingTailnet(slug, host string) error {
	marker, err := newRetirementMarker(slug, host)
	if err != nil {
		return err
	}
	root, err := m.openRetirementRoot(true)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(marker.slug, 0o700); err != nil {
		return err
	}
	if info, err := root.Lstat(marker.slug); err != nil {
		return err
	} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("provider: retirement slug %q is not a directory", marker.slug)
	}
	f, err := root.OpenFile(marker.name(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if !os.IsExist(err) {
			return err
		}
		info, statErr := root.Lstat(marker.name())
		if statErr != nil {
			return statErr
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("provider: retirement marker %q is not a regular file", marker.name())
		}
	} else if err := f.Close(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	hosts := m.pendingTailnet[slug]
	if hosts == nil {
		hosts = map[string]struct{}{}
		m.pendingTailnet[slug] = hosts
	}
	hosts[host] = struct{}{}
	return nil
}

func (m *Manager) clearPendingTailnet(slug, host string) error {
	marker, err := newRetirementMarker(slug, host)
	if err != nil {
		return err
	}
	root, err := m.openRetirementRoot(false)
	if err == nil {
		if err := root.Remove(marker.name()); err != nil && !os.IsNotExist(err) {
			root.Close()
			return err
		}
		_ = root.Remove(marker.slug)
		if err := root.Close(); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	hosts := m.pendingTailnet[slug]
	delete(hosts, host)
	if len(hosts) == 0 {
		delete(m.pendingTailnet, slug)
	}
	return nil
}

func (m *Manager) hasPendingTailnet(slug, host string) (bool, error) {
	m.mu.Lock()
	_, ok := m.pendingTailnet[slug][host]
	m.mu.Unlock()
	if ok {
		return true, nil
	}
	marker, err := newRetirementMarker(slug, host)
	if err != nil {
		return false, err
	}
	root, err := m.openRetirementRoot(false)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer root.Close()
	info, err := root.Lstat(marker.name())
	if err == nil {
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("provider: retirement marker %q is not a regular file", marker.name())
		}
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (m *Manager) pendingTailnetHosts(slug string) ([]string, error) {
	if _, err := newRetirementMarker(slug, "host"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	set := make(map[string]struct{}, len(m.pendingTailnet[slug]))
	for host := range m.pendingTailnet[slug] {
		set[host] = struct{}{}
	}
	m.mu.Unlock()
	root, err := m.openRetirementRoot(false)
	if os.IsNotExist(err) {
		return sortedKeys(set), nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat(slug)
	if os.IsNotExist(err) {
		return sortedKeys(set), nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("provider: retirement slug %q is not a directory", slug)
	}
	dir, err := root.Open(slug)
	if err != nil {
		return nil, err
	}
	entries, err := dir.ReadDir(-1)
	closeErr := dir.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("provider: retirement marker %q is not a regular file", filepath.Join(slug, entry.Name()))
		}
		if _, err := newRetirementMarker(slug, entry.Name()); err != nil {
			return nil, err
		}
		set[entry.Name()] = struct{}{}
	}
	return sortedKeys(set), nil
}

func sortedKeys(set map[string]struct{}) []string {
	hosts := make([]string, 0, len(set))
	for host := range set {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}

// StopPublicRoutes closes Funnel and Portal for slug. Local and tailscale
// routes for that slug keep serving. Listener stop failures are Unconfirmed
// and stay tracked. Once Funnel is confirmed closed, a failed node retirement
// is retained separately for StopSlug or a later Funnel activation to retry;
// it does not mean the public route remains reachable.
func (m *Manager) StopPublicRoutes(ctx context.Context, slug string) (PublicStopResult, error) {
	funnelHost, hadFunnel := m.take(slug, Funnel, false)
	_, hadPortal := m.take(slug, Portal, false)
	var res PublicStopResult
	if hadFunnel {
		if m.tailnet() == nil {
			res.Unconfirmed = append(res.Unconfirmed, Funnel)
		} else {
			sharedPrivate := m.hasRoute(slug, funnelHost, Tailscale)
			marked := sharedPrivate
			if !sharedPrivate {
				// Persist the non-public cleanup obligation before changing the
				// listener. A local durability failure leaves the public route up.
				marked = m.markPendingTailnet(slug, funnelHost) == nil
			}
			if !marked {
				res.Unconfirmed = append(res.Unconfirmed, Funnel)
			} else if err := m.tailnet().StopFunnel(funnelHost); err != nil {
				if !sharedPrivate {
					_ = m.clearPendingTailnet(slug, funnelHost)
				}
				res.Unconfirmed = append(res.Unconfirmed, Funnel)
			} else if st := m.tailnet().FunnelState(funnelHost); st.State == stateReady || st.State == stateStarting {
				if !sharedPrivate {
					_ = m.clearPendingTailnet(slug, funnelHost)
				}
				res.Unconfirmed = append(res.Unconfirmed, Funnel)
			} else {
				m.take(slug, Funnel, true)
				m.pruneState(slug, Funnel)
				res.Stopped = append(res.Stopped, Funnel)
				if !sharedPrivate && ctx.Err() == nil {
					// Funnel is already unreachable. Identity retirement is a separate
					// cleanup obligation: failure retains the durable marker but does
					// not claim public access is still live.
					if err := m.tailnet().Stop(funnelHost); err == nil {
						_ = m.clearPendingTailnet(slug, funnelHost)
					}
				}
			}
		}
	}
	if hadPortal {
		if m.portalNet() == nil {
			res.Unconfirmed = append(res.Unconfirmed, Portal)
		} else if err := m.portalNet().Stop(slug); err != nil {
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

func (m *Manager) hasRoute(slug, host string, id ID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.routes {
		if r.slug == slug && r.host == host && r.provider == id {
			return true
		}
	}
	return false
}

// StopSlug authoritatively retires every registered route and tailnet node
// owned by slug. It is the delete/redirect-expiry operation: unlike
// StopPublicRoutes it retires Funnel-only nodes after their public listener is
// confirmed closed. Any failed route remains registered so the same process
// can retry without reporting false success.
func (m *Manager) StopSlug(ctx context.Context, slug string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	routes := make([]route, 0)
	for _, r := range m.routes {
		if r.slug == slug {
			routes = append(routes, *r)
		}
	}
	m.mu.Unlock()

	type hostRoutes struct {
		local, tailscale, funnel, portal bool
	}
	byHost := make(map[string]*hostRoutes)
	// Current and redirect tsnet identities use the exact lifecycle slug as
	// their host. Include it even when a prior visibility transition or a
	// restart pruned every Funnel/Tailscale route record.
	byHost[slug] = new(hostRoutes)
	pendingHosts, err := m.pendingTailnetHosts(slug)
	if err != nil {
		return fmt.Errorf("provider: inspect pending tailnet retirement for %s: %w", slug, err)
	}
	for _, host := range pendingHosts {
		byHost[host] = new(hostRoutes)
	}
	for _, r := range routes {
		h := byHost[r.host]
		if h == nil {
			h = new(hostRoutes)
			byHost[r.host] = h
		}
		switch r.provider {
		case Local:
			h.local = true
		case Tailscale:
			h.tailscale = true
		case Funnel:
			h.funnel = true
		case Portal:
			h.portal = true
		}
	}
	hosts := make([]string, 0, len(byHost))
	for host := range byHost {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)

	// Funnel listeners must be confirmed down before their shared node is
	// retired. Do this for every host before removing any registration or
	// Private Local route.
	var errs []error
	for _, host := range hosts {
		owned := byHost[host]
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if owned.funnel {
			if m.tailnet() == nil {
				errs = append(errs, fmt.Errorf("%w: funnel %s: %w", core.ErrPublicStopUnconfirmed, host, ErrNotConfigured))
			} else if err := m.tailnet().StopFunnel(host); err != nil {
				errs = append(errs, fmt.Errorf("%w: funnel %s: %w", core.ErrPublicStopUnconfirmed, host, err))
			} else if state := m.tailnet().FunnelState(host); state.State == stateReady || state.State == stateStarting {
				errs = append(errs, fmt.Errorf("%w: funnel %s teardown remains %s", core.ErrPublicStopUnconfirmed, host, state.State))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}

	// Retire every deterministic identity candidate, including persisted-only
	// state and tracked preview/alias hosts. Complete the whole node phase
	// before touching Local so a failed destructive operation leaves the flat
	// or redirect reachable for retry.
	for _, host := range hosts {
		owned := byHost[host]
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if m.tailnet() == nil {
			if owned.tailscale || owned.funnel {
				errs = append(errs, fmt.Errorf("tailscale %s: %w", host, ErrNotConfigured))
				continue
			}
			// No grant means no backend may contact control. Delete only the
			// deterministic local state so a later flat with this slug cannot
			// inherit the legacy identity.
			if err := tsnet.RemoveLocalState(m.tailscaleDir, host); err != nil {
				errs = append(errs, fmt.Errorf("tailscale %s local state: %w", host, err))
			} else if err := m.clearPendingTailnet(slug, host); err != nil {
				errs = append(errs, fmt.Errorf("tailscale %s cleanup record: %w", host, err))
			}
			continue
		}
		if err := m.tailnet().Stop(host); err != nil {
			errs = append(errs, fmt.Errorf("tailscale %s: %w", host, err))
		} else if err := m.clearPendingTailnet(slug, host); err != nil {
			errs = append(errs, fmt.Errorf("tailscale %s cleanup record: %w", host, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}

	for _, host := range hosts {
		owned := byHost[host]
		if !owned.portal {
			continue
		}
		if m.portalNet() == nil {
			errs = append(errs, fmt.Errorf("%w: portal %s: %w", core.ErrPublicStopUnconfirmed, host, ErrNotConfigured))
		} else if err := m.portalNet().Stop(host); err != nil {
			errs = append(errs, fmt.Errorf("%w: portal %s: %w", core.ErrPublicStopUnconfirmed, host, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}

	for _, host := range hosts {
		m.forgetRoutes(slug, host, Tailscale, Funnel, Portal)
	}
	for _, host := range hosts {
		owned := byHost[host]
		if !owned.local {
			continue
		}
		if err := m.local.Stop(host); err != nil {
			errs = append(errs, fmt.Errorf("local %s: %w", host, err))
		} else {
			m.forgetRoutes(slug, host, Local)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) forgetRoutes(slug, host string, providers ...ID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, r := range m.routes {
		if r.slug != slug || r.host != host || !slices.Contains(providers, r.provider) {
			continue
		}
		delete(m.routes, k)
		m.pruneStateLocked(r.slug, r.audience, r.host, r.provider)
	}
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
		if m.tailnet() == nil {
			errs = append(errs, ErrNotConfigured)
			continue
		}
		var stopErr error
		if private, ok := m.tailnet().(interface{ StopPrivate(string) error }); ok {
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
				stopErr = m.tailnet().Stop(r.host)
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
		} else if m.tailnet() != nil {
			err = m.tailnet().Stop(host)
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
	m.pendingTailnet = map[string]map[string]struct{}{}
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
// A confirmed-closed Funnel can also have a pending identity retirement. The
// visibility approval already authorized that cleanup, so a later permission
// revocation inspection retries it and refuses revocation until it confirms.
func (m *Manager) HasProviderRoute(ctx context.Context, slug string, id ID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	m.mu.Lock()
	for _, r := range m.routes {
		if r.slug == slug && r.provider == id {
			m.mu.Unlock()
			return true, nil
		}
	}
	m.mu.Unlock()
	if id == Funnel {
		hosts, err := m.pendingTailnetHosts(slug)
		if err != nil {
			return false, err
		}
		sort.Strings(hosts)
		for _, host := range hosts {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if m.tailnet() == nil {
				return false, fmt.Errorf("pending Funnel identity %s: %w", host, ErrNotConfigured)
			}
			if err := m.tailnet().Stop(host); err != nil {
				return false, fmt.Errorf("pending Funnel identity %s: %w", host, err)
			}
			if err := m.clearPendingTailnet(slug, host); err != nil {
				return false, err
			}
		}
	}
	return false, nil
}

var _ core.LifecycleRouteInspector = (*Manager)(nil)
