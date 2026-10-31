package core

import (
	"context"
	"net/http"
	"strings"

	"github.com/gosuda/flats/internal/slug"
)

// routeEpoch owns requests admitted during one exposure lifetime. Its context
// deliberately outlives individual requests and is cancelled at withdrawal.
type routeEpoch struct {
	ctx    context.Context
	cancel context.CancelFunc
	id     string
}

type routeEpochKey struct{}

func (s *Service) routeEpoch(key string) *routeEpoch {
	ctx, cancel := context.WithCancel(context.Background())
	e := &routeEpoch{ctx: ctx, cancel: cancel, id: slug.Random(24)}
	actual, loaded := s.routeEpochs.LoadOrStore(key, e)
	if loaded {
		cancel()
	}
	return actual.(*routeEpoch)
}

func (s *Service) revokeRoute(key string) {
	if e, ok := s.routeEpochs.LoadAndDelete(key); ok {
		e.(*routeEpoch).cancel()
	}
	if slugName, public := strings.CutPrefix(key, "public:"); public {
		s.withdrawNotebookPublic(slugName)
	}
}

func (e *routeEpoch) request(r *http.Request) (*http.Request, func()) {
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(e.ctx, cancel)
	if e.ctx.Err() != nil {
		cancel()
	}
	return r.WithContext(context.WithValue(ctx, routeEpochKey{}, e)), func() { stop(); cancel() }
}

func (s *Service) previewHandler(p *preview) http.Handler {
	e := s.routeEpoch("preview:" + p.host)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r, done := e.request(r)
		defer done()
		if r.Context().Err() != nil {
			http.Error(w, "preview is closed", http.StatusServiceUnavailable)
			return
		}
		p.last.Store(s.now().UnixMilli())
		w.Header().Set("X-Robots-Tag", "noindex")
		p.handler.ServeHTTP(w, withRouteHeaders(r, false))
	})
}
