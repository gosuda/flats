// Package local serves flats on one loopback port, routing by Host header
// (<host>.localhost:<port>). Browsers resolve *.localhost to loopback and treat
// every subdomain as its own origin, so flats stay isolated. It is the
// development and test stand-in for the Tailscale network.
package local

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/core"
)

// Net is a Host-routed loopback server.
type Net struct {
	ln    net.Listener
	srv   *http.Server
	port  int
	mu    sync.RWMutex
	hosts map[string]entry
	// Identity, when set, simulates Tailscale identity headers (tests).
	Identity func(r *http.Request) (login, name string)
}

type entry struct {
	h         http.Handler
	ephemeral bool
}

// Listen starts the loopback server on addr (e.g. "127.0.0.1:0").
func Listen(addr string) (*Net, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	n := &Net{ln: ln, port: ln.Addr().(*net.TCPAddr).Port, hosts: map[string]entry{}}
	n.srv = &http.Server{Handler: n, ReadHeaderTimeout: 10 * time.Second}
	go n.srv.Serve(ln)
	return n, nil
}

// Port returns the listening port.
func (n *Net) Port() int { return n.port }

// ServeHTTP routes by the first label of the Host header.
func (n *Net) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	label := strings.TrimSuffix(host, ".localhost")
	if label == host || strings.Contains(label, ".") {
		http.Error(w, "unknown host; use http://<flat>.localhost:"+fmt.Sprint(n.port), http.StatusNotFound)
		return
	}
	n.mu.RLock()
	e, ok := n.hosts[label]
	n.mu.RUnlock()
	if !ok {
		http.Error(w, "no flat is served at "+label, http.StatusNotFound)
		return
	}
	for k := range r.Header {
		if strings.HasPrefix(k, "Tailscale-User-") {
			r.Header.Del(k)
		}
	}
	if n.Identity != nil {
		if login, name := n.Identity(r); login != "" {
			r.Header.Set("Tailscale-User-Login", login)
			r.Header.Set("Tailscale-User-Name", name)
		}
	}
	e.h.ServeHTTP(w, r)
}

// Serve implements core.PrivateNet.
func (n *Net) Serve(_ context.Context, host string, h http.Handler, ephemeral bool) (string, error) {
	if host == "" {
		return "", errors.New("empty host")
	}
	n.mu.Lock()
	n.hosts[host] = entry{h: h, ephemeral: ephemeral}
	n.mu.Unlock()
	return n.URL(host), nil
}

// Stop implements core.PrivateNet.
func (n *Net) Stop(host string) error {
	n.mu.Lock()
	delete(n.hosts, host)
	n.mu.Unlock()
	return nil
}

// URL implements core.PrivateNet.
func (n *Net) URL(host string) string {
	return fmt.Sprintf("http://%s.localhost:%d", host, n.port)
}

// Serving reports whether host is served.
func (n *Net) Serving(host string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	_, ok := n.hosts[host]
	return ok
}

// Status implements core.PrivateNet.
func (n *Net) Status() core.NetStatus {
	n.mu.RLock()
	defer n.mu.RUnlock()
	st := core.NetStatus{Kind: "local", Enabled: true, Detail: fmt.Sprintf("loopback only, port %d (no Tailscale)", n.port)}
	for h, e := range n.hosts {
		st.Hosts = append(st.Hosts, core.HostInfo{Host: h, URL: n.URL(h), State: "ready", Ephemeral: e.ephemeral})
	}
	sort.Slice(st.Hosts, func(i, j int) bool { return st.Hosts[i].Host < st.Hosts[j].Host })
	return st
}

// Close implements core.PrivateNet.
func (n *Net) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return n.srv.Shutdown(ctx)
}

// Public is an in-memory stand-in for Portal used in tests: it serves public
// flats under <slug>.public.localhost and records listing state.
type Public struct {
	*Net
	mu     sync.Mutex
	hidden map[string]bool
}

// NewPublic wraps a loopback Net as a fake public network.
func NewPublic(n *Net) *Public { return &Public{Net: n, hidden: map[string]bool{}} }

// Serve implements core.PublicNet.
func (p *Public) Serve(ctx context.Context, slug string, h http.Handler, hidden bool) (string, error) {
	p.mu.Lock()
	p.hidden[slug] = hidden
	p.mu.Unlock()
	return p.Net.Serve(ctx, "pub-"+slug, h, false)
}

// SetHidden implements core.PublicNet.
func (p *Public) SetHidden(slug string, hidden bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.hidden[slug]; !ok {
		return fmt.Errorf("%s is not served", slug)
	}
	p.hidden[slug] = hidden
	return nil
}

// Hidden reports the listing state of a served slug.
func (p *Public) Hidden(slug string) (hidden, served bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.hidden[slug]
	return h, ok
}

// Stop implements core.PublicNet.
func (p *Public) Stop(slug string) error {
	p.mu.Lock()
	delete(p.hidden, slug)
	p.mu.Unlock()
	return p.Net.Stop("pub-" + slug)
}

// URL implements core.PublicNet.
func (p *Public) URL(slug string) string { return p.Net.URL("pub-" + slug) }

// Status implements core.PublicNet.
func (p *Public) Status() core.NetStatus {
	st := p.Net.Status()
	st.Kind = "local-public"
	return st
}
