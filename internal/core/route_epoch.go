package core

import (
	"context"
	"net/http"
)

// routeEpoch owns requests admitted during one exposure lifetime. Its context
// deliberately outlives individual requests and is cancelled at withdrawal.
type routeEpoch struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (s *Service) routeEpoch(key string) *routeEpoch {
	ctx, cancel := context.WithCancel(context.Background())
	e := &routeEpoch{ctx: ctx, cancel: cancel}
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
}

func (e *routeEpoch) request(r *http.Request) (*http.Request, func()) {
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(e.ctx, cancel)
	if e.ctx.Err() != nil {
		cancel()
	}
	return r.WithContext(ctx), func() { stop(); cancel() }
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
		p.handler.ServeHTTP(w, trustedAccess(r, false))
	})
}
