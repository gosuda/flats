package app

import (
	"net/http"
	"slices"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/config"
)

// metricsHandler runs inside the management HostGuard, which marks admitted
// external origins. A proxy connected over loopback remains external.
func metricsHandler(next http.Handler, cfg config.MetricsConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && api.RefuseCrossSite(w, r) {
			return
		}
		access := "localhost"
		if api.ExternalOrigin(r.Context()) != "" {
			access = "external"
		} else if _, ok := consoleDomainOf(r.Context()); ok {
			access = "console"
		}
		if !cfg.Enabled || !slices.Contains(cfg.ExposeOn, access) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
