package app

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/expose/proxy"
)

// proxyKind reports whether id is one of the two reverse proxy providers,
// which share one listener.
func proxyKind(id provider.ID) bool { return id == provider.Proxy || id == provider.ProxyPublic }

// listenProxy binds the reverse proxy listener at addr. The listener reads
// the template through template on every request, so a template saved in
// the settings applies at once.
func listenProxy(addr string, template func() config.Template) (*proxy.Net, error) {
	n, err := proxy.Listen(addr, template)
	if err != nil {
		return nil, fmt.Errorf("the reverse proxy listener cannot bind proxy.addr %s: %w", addr, err)
	}
	return n, nil
}

// failedProxy stands in for a reverse proxy listener that could not bind at
// start, so the host still starts and every proxy route reports the bind
// error. A restart retries the bind.
type failedProxy struct{ err error }

func (f failedProxy) Serve(context.Context, string, http.Handler) (string, error) { return "", f.err }
func (f failedProxy) Stop(string) error                                           { return nil }
func (f failedProxy) URL(string) string                                           { return "" }
func (f failedProxy) Close() error                                                { return nil }
func (f failedProxy) Status() core.NetStatus {
	return core.NetStatus{Kind: proxy.Kind, Detail: f.err.Error(), Hosts: []core.HostInfo{}}
}

// proxyAddressLocked is the address the reverse proxy forwards to: the bound one
// while the listener runs, otherwise proxy.addr as saved. h.pmu is held.
func (h *Host) proxyAddressLocked() string {
	if n, ok := h.proxyNet.(*proxy.Net); ok {
		return n.Addr()
	}
	return h.Svc.ProxyConfig().Addr
}

// proxyBindErrorLocked is the start-time bind error of the reverse proxy listener,
// or "". h.pmu is held.
func (h *Host) proxyBindErrorLocked() string {
	if f, ok := h.proxyNet.(failedProxy); ok {
		return f.err.Error()
	}
	return ""
}

// loopbackAddr reports whether host:port addr names a loopback address.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
