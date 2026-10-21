// Package metrics serves the host metrics of Flats at GET /metrics on the
// management server, in the Prometheus exposition format, and counts the
// management server's own requests. See docs/spec/metrics.md.
//
// It keeps a private registry: Go runtime and process figures of the host
// plus one collector that reads Flats' own values at collect time. It never
// uses the default registerer, which bundled libraries register into (and
// libtunnel replaces at startup), so no library metric is exposed.
package metrics

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/provider"
)

// Source is what core exposes to /metrics. Metrics must build its snapshot
// from memory only: it is called on every scrape.
type Source interface {
	Metrics() core.MetricsSnapshot
}

// EndpointSource reports route endpoints by provider and state. It may call
// network backends, so it is only called by the refresh loop, never by a
// scrape.
type EndpointSource interface {
	EndpointStates() []provider.EndpointCount
}

// EndpointInterval is how often the route endpoint states are refreshed, as
// often as the host sweep refreshes its snapshot.
const EndpointInterval = 30 * time.Second

// Options configure New.
type Options struct {
	// PerFlat opts into Prometheus series labelled with a flat slug.
	PerFlat bool
	// Source is core's in-memory snapshot; nil exposes no per-flat or
	// host-snapshot series.
	Source Source
	// Endpoints is the provider manager; nil exposes no endpoint series.
	Endpoints EndpointSource
	// EndpointInterval overrides EndpointInterval (tests).
	EndpointInterval time.Duration
}

// Metrics is the /metrics endpoint and the management request counters.
type Metrics struct {
	reg      *prometheus.Registry
	src      Source
	eps      EndpointSource
	interval time.Duration
	perFlat  bool
	mgmt     managementCounts

	emu       sync.Mutex
	endpoints []provider.EndpointCount

	lmu     sync.Mutex
	started bool
	stop    chan struct{}
	done    chan struct{}
}

// New builds the registry. Call Start to begin refreshing endpoint states
// and Close to stop.
func New(o Options) *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(), src: o.Source, eps: o.Endpoints,
		interval: o.EndpointInterval, perFlat: o.PerFlat,
	}
	if m.interval <= 0 {
		m.interval = EndpointInterval
	}
	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		&collector{m: m},
	)
	return m
}

// Registry is the private registry (tests).
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// Start refreshes the endpoint states now and then every interval, in its
// own goroutine, until Close. It does nothing without an EndpointSource or
// when already started.
func (m *Metrics) Start() {
	m.lmu.Lock()
	defer m.lmu.Unlock()
	if m.started || m.eps == nil {
		return
	}
	m.started = true
	stop, done := make(chan struct{}), make(chan struct{})
	m.stop, m.done = stop, done
	go func() {
		defer close(done)
		t := time.NewTicker(m.interval)
		defer t.Stop()
		for {
			m.RefreshEndpoints()
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()
}

// Close stops the refresh loop and waits for it. It is safe to call more
// than once, and without Start.
func (m *Metrics) Close() {
	m.lmu.Lock()
	stop, done := m.stop, m.done
	m.stop = nil
	m.lmu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
}

// RefreshEndpoints reads the endpoint states from the EndpointSource and
// keeps them for scrapes.
func (m *Metrics) RefreshEndpoints() {
	if m.eps == nil {
		return
	}
	states := m.eps.EndpointStates()
	m.emu.Lock()
	m.endpoints = states
	m.emu.Unlock()
}

func (m *Metrics) cachedEndpoints() []provider.EndpointCount {
	m.emu.Lock()
	defer m.emu.Unlock()
	return m.endpoints
}

// Handler serves GET and HEAD /metrics. Other methods are 405, and a
// cross-site or foreign-Origin browser request is 403, as on /api. Mount it
// behind the management server's host guard.
func (m *Metrics) Handler() http.Handler {
	h := promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{EnableOpenMetrics: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if api.RefuseCrossSite(w, r) {
			return
		}
		h.ServeHTTP(w, r)
	})
}
