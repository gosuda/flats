package core

import (
	"bufio"
	"context"
	"net"
	"net/http"

	"github.com/gosuda/flats/internal/wsconn"
)

func (s *Service) privateEpoch() *routeEpoch {
	if e := s.pi.epoch.Load(); e != nil {
		return e
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &routeEpoch{ctx: ctx, cancel: cancel}
	if s.pi.epoch.CompareAndSwap(nil, e) {
		return e
	}
	cancel()
	return s.pi.epoch.Load()
}

func (s *Service) rotatePrivateEpoch() {
	old := s.privateEpoch()
	ctx, cancel := context.WithCancel(context.Background())
	s.pi.epoch.Store(&routeEpoch{ctx: ctx, cancel: cancel})
	old.cancel()
}

func (s *Service) linkGate(fallbackPublic bool, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		public := fallbackPublic
		if id, marked := RouteProvider(r.Context()); marked {
			public = PublicProvider(id)
		}
		if public {
			h.ServeHTTP(w, r)
			return
		}
		e := s.privateEpoch()
		if s.PublicInstance().Enabled {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("<!doctype html><title>Private links are turned off</title><p>Private links are turned off on this host.</p>"))
			return
		}
		r, done := e.request(r)
		defer done()
		if wsconn.IsUpgrade(r) {
			w = &privateLinkWriter{ResponseWriter: w, epoch: e}
		}
		h.ServeHTTP(w, r)
	})
}

type privateLinkWriter struct {
	http.ResponseWriter
	epoch *routeEpoch
}

func (w *privateLinkWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *privateLinkWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	c := &privateLinkConn{Conn: conn}
	c.stop = context.AfterFunc(w.epoch.ctx, func() { conn.Close() })
	if w.epoch.ctx.Err() != nil {
		c.Close()
	}
	return c, rw, nil
}

type privateLinkConn struct {
	net.Conn
	stop func() bool
}

func (c *privateLinkConn) Close() error {
	c.stop()
	return c.Conn.Close()
}
