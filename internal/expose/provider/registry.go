package provider

import (
	"fmt"
	"sync"

	"github.com/gosuda/flats/internal/core"
)

// loadBackend reads a backend field guarded by mu.
func loadBackend[T any](mu *sync.RWMutex, p *T) T {
	mu.RLock()
	defer mu.RUnlock()
	return *p
}

// attachBackend stores v in p once: a backend already attached is kept, so
// late attachment never replaces the process's backend.
func attachBackend[T any](mu *sync.RWMutex, p *T, v T) {
	mu.Lock()
	defer mu.Unlock()
	if any(*p) == nil {
		*p = v
	}
}

// backendView is one provider's registered backend wiring: whether a backend
// is attached and how a registered route's endpoint is re-read from it.
type backendView struct {
	// configured reports whether the provider's backend is attached.
	configured func(m *Manager) bool
	// view re-reads a registered route's endpoint from the backend.
	view func(m *Manager, id ID, host string) ExposureEndpoint
	// missingDetail replaces view while the backend is detached after the
	// snapshot. zrok's account can be replaced and NetBird's peer can
	// disconnect while a route stays registered; the reverse proxy detaches
	// only once every proxy route is gone. "" marks a backend attached for
	// the whole process (Local, the tailnet and Portal), whose view runs
	// unguarded exactly as before.
	missingDetail string
}

func tailnetConfigured(m *Manager) bool      { return m.tailnet() != nil }
func netbirdConfigured(m *Manager) bool      { return m.netbirdNet() != nil }
func proxyBackendConfigured(m *Manager) bool { return m.proxyNet() != nil }

// proxyBackendView is the shared wiring of the proxy and proxy-public
// providers: one listener serves both, and a route's endpoint is reported
// under its own provider id.
func proxyBackendView() backendView {
	return backendView{
		configured: proxyBackendConfigured,
		view: func(m *Manager, id ID, host string) ExposureEndpoint {
			return fromStatus(id, m.proxyNet().Status(), host, m.proxyNet().URL(host))
		},
		missingDetail: "the reverse proxy is not configured",
	}
}

// backendViews is the provider registry: one row per provider id drives
// configured() and refreshed(). Adding a provider means a row here, its
// interface, its Attach method and its open and stop wiring.
var backendViews = map[ID]backendView{
	Local: {
		configured: func(m *Manager) bool { return m.local != nil },
		view: func(m *Manager, id ID, host string) ExposureEndpoint {
			return fromStatus(id, m.local.Status(), host, m.local.URL(host))
		},
	},
	Tailscale: {
		configured: tailnetConfigured,
		view: func(m *Manager, id ID, host string) ExposureEndpoint {
			return fromStatus(id, m.tailnet().Status(), host, m.tailnet().URL(host))
		},
	},
	Funnel: {
		configured: tailnetConfigured,
		view: func(m *Manager, id ID, host string) ExposureEndpoint {
			return m.tailnet().FunnelState(host)
		},
	},
	Portal: {
		configured: func(m *Manager) bool { return m.portalNet() != nil },
		view: func(m *Manager, id ID, host string) ExposureEndpoint {
			return fromStatus(id, m.portalNet().Status(), host, m.portalNet().URL(host))
		},
	},
	Zrok: {
		configured: func(m *Manager) bool { return m.zrokNet() != nil },
		view: func(m *Manager, id ID, host string) ExposureEndpoint {
			return fromStatus(id, m.zrokNet().Status(), host, m.zrokNet().URL(host))
		},
		missingDetail: "zrok is not configured",
	},
	CFQuick: {
		configured: func(m *Manager) bool { return m.quickNet() != nil },
		view: func(m *Manager, id ID, host string) ExposureEndpoint {
			return fromStatus(id, m.quickNet().Status(), host, m.quickNet().URL(host))
		},
		missingDetail: "cloudflare quick tunnels are not configured",
	},
	Netbird: {
		configured: netbirdConfigured,
		view: func(m *Manager, id ID, host string) ExposureEndpoint {
			return fromStatus(id, m.netbirdNet().PrivateStatus(), host, m.netbirdNet().PrivateURL(host))
		},
		missingDetail: "netbird is not configured",
	},
	NetbirdExpose: {
		configured: netbirdConfigured,
		view: func(m *Manager, id ID, host string) ExposureEndpoint {
			return fromStatus(id, m.netbirdNet().PublicStatus(), host, m.netbirdNet().PublicURL(host))
		},
		missingDetail: "netbird is not configured",
	},
	Proxy:       proxyBackendView(),
	ProxyPublic: proxyBackendView(),
}

// unconfiguredRefusal is the open refusal while a provider is permitted but
// this process has no backend for it: detail reports the situation, label
// names the provider in the ErrNotConfigured wrap.
func unconfiguredRefusal(id ID, detail, label string) (ExposureEndpoint, error) {
	return refused(id, detail), fmt.Errorf("%w: %s", ErrNotConfigured, label)
}

// openFailed reports a failed open whose error surfaces unwrapped, as Local
// and Tailscale serve errors do.
func openFailed(id ID, err error) (ExposureEndpoint, error) {
	return ExposureEndpoint{Provider: id, State: stateError, Detail: err.Error()}, err
}

// serveFailed reports a failed open wrapped as the provider being not ready,
// with label naming it in the wrap.
func serveFailed(id ID, label string, err error) (ExposureEndpoint, error) {
	return ExposureEndpoint{Provider: id, State: stateError, Detail: err.Error()},
		fmt.Errorf("%w: %s: %w", core.ErrProviderNotReady, label, err)
}

// finishOpen registers the opened route and returns its endpoint as the
// backend's status reports it.
func (m *Manager) finishOpen(req ExposureRequest, id ID, host, url string, st core.NetStatus) ExposureEndpoint {
	m.track(req, id, host)
	return fromStatus(id, st, host, url)
}

// collectOpen appends one open's endpoint and error to a Serve result: a
// failed open still contributes its endpoint.
func collectOpen(eps []ExposureEndpoint, errs []error, ep ExposureEndpoint, err error) ([]ExposureEndpoint, []error) {
	if err != nil {
		errs = append(errs, err)
	}
	return append(eps, ep), errs
}

// stopRoutedPublic stops a public listener whose route record is tracked:
// on success the record is forgotten and the provider lands in stopped; a
// missing backend or a failed stop keeps the record for retry and lands the
// provider in unconfirmed.
func (m *Manager) stopRoutedPublic(res *PublicStopResult, slug, host string, id ID, stop func() error) {
	if err := stop(); err != nil {
		res.Unconfirmed = append(res.Unconfirmed, id)
		return
	}
	m.dropRoute(slug, id)
	m.pruneState(slug, id)
	res.Stopped = append(res.Stopped, id)
}

// stopPrivateBackend stops host on the NetBird mesh or the reverse proxy
// listener; without the backend it reports ErrNotConfigured.
func (m *Manager) stopPrivateBackend(id ID, host string) error {
	if id == Netbird {
		if n := m.netbirdNet(); n != nil {
			return n.StopPrivate(host)
		}
	} else if id == Proxy {
		if p := m.proxyNet(); p != nil {
			return p.Stop(host)
		}
	}
	return ErrNotConfigured
}

// forgetRegistration drops the stopped route key and its observed state.
func (m *Manager) forgetRegistration(k string, r route) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.routes, k)
	m.pruneStateLocked(r.slug, r.audience, r.host, r.provider)
}
