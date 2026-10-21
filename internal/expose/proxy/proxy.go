// Package proxy serves flats to an operator's reverse proxy on a listener of
// its own. The proxy forwards the visitor's Host, and the listener routes it
// by proxy.url_template: <label>.<domain> reaches the route registered for
// label. Only routes the provider Manager registers are served there; the
// console, the API, MCP and /metrics never are.
package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
)

// Kind is the status kind of the listener.
const Kind = "proxy"

// Detail is the status detail of every route: Flats cannot see whether the
// proxy, its DNS or its certificate work.
const Detail = "Served through your reverse proxy. Flats cannot check that the proxy reaches it."

// notFound is the body of every unmatched request. It never repeats the Host.
const notFound = "no flat is served at this address"

// Net is the listener a reverse proxy forwards to.
type Net struct {
	ln       net.Listener
	srv      *http.Server
	template func() config.Template
	mu       sync.RWMutex
	routes   map[string]http.Handler // route host label -> handler
}

// Listen starts the listener on addr. template returns the template in
// effect; it is read on every request and every URL call, so a changed
// template applies at once.
func Listen(addr string, template func() config.Template) (*Net, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	n := &Net{ln: ln, template: template, routes: map[string]http.Handler{}}
	n.srv = &http.Server{Handler: n, ReadHeaderTimeout: 10 * time.Second}
	go n.srv.Serve(ln)
	return n, nil
}

// Addr returns the address the listener is bound to.
func (n *Net) Addr() string { return n.ln.Addr().String() }

// ServeHTTP routes by the label of the Host the proxy forwarded. A matched
// request reaches its handler with Host and X-Forwarded-Proto taken from the
// template, whatever the proxy or the client sent, and without
// Tailscale-User-* headers.
func (n *Net) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t := n.template()
	label, ok := t.Label(r.Host)
	n.mu.RLock()
	h := n.routes[label]
	n.mu.RUnlock()
	if !ok || h == nil {
		http.Error(w, notFound, http.StatusNotFound)
		return
	}
	r.Host = t.Authority(label)
	for k := range r.Header {
		if isIdentityHeader(k) {
			delete(r.Header, k)
		}
	}
	r.Header.Set("X-Forwarded-Proto", t.Scheme())
	h.ServeHTTP(w, r)
}

// isIdentityHeader reports whether a request header name could be read as a
// Tailscale-User-* identity header, ignoring case and treating "_" as "-".
func isIdentityHeader(name string) bool {
	const prefix = "tailscale-user-"
	return len(name) >= len(prefix) && strings.EqualFold(strings.ReplaceAll(name[:len(prefix)], "_", "-"), prefix)
}

// Serve registers h for route host label host and returns its address.
func (n *Net) Serve(_ context.Context, host string, h http.Handler) (string, error) {
	if host == "" {
		return "", errors.New("empty host")
	}
	n.mu.Lock()
	n.routes[host] = h
	n.mu.Unlock()
	return n.URL(host), nil
}

// Stop removes the route of host. It is a no-op for an unknown host.
func (n *Net) Stop(host string) error {
	n.mu.Lock()
	delete(n.routes, host)
	n.mu.Unlock()
	return nil
}

// URL returns the address of host under the current template, or "" without
// a template.
func (n *Net) URL(host string) string { return n.template().URL(host) }

// Status reports every route ready, with Detail.
func (n *Net) Status() core.NetStatus {
	n.mu.RLock()
	hosts := make([]string, 0, len(n.routes))
	for h := range n.routes {
		hosts = append(hosts, h)
	}
	n.mu.RUnlock()
	slices.Sort(hosts)
	st := core.NetStatus{Kind: Kind, Enabled: true, Hosts: []core.HostInfo{}}
	for _, h := range hosts {
		st.Hosts = append(st.Hosts, core.HostInfo{Host: h, URL: n.URL(h), State: "ready", Detail: Detail})
	}
	return st
}

// Close stops the listener and frees its address.
func (n *Net) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := n.srv.Shutdown(ctx)
	// Shutdown closes only the listeners Serve already tracks; one that the
	// Serve goroutine has not reached yet would stay bound until it does.
	n.ln.Close()
	return err
}
