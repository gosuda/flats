package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
	tsnetx "github.com/gosuda/flats/internal/expose/tsnet"
	"github.com/gosuda/flats/internal/store"
)

// The console runs on the tailnet as one tsnet node per Tailscale console
// domain (tailscale.console_hosts), and on NetBird as one peer per NetBird
// console domain (netbird.console_hosts). console_nodes in flats.db records
// every node or peer Flats runs for the console, so a domain removed from a
// list, even offline, is retired at the next start.

const (
	providerTailscale = core.ConsoleProviderTailscale
	providerNetbird   = core.ConsoleProviderNetbird
)

// consoleProviders lists the networks console domains run on, in the order
// links prefer them: Tailscale serves HTTPS.
var consoleProviders = []string{providerTailscale, providerNetbird}

var providerLabel = map[string]string{providerTailscale: "Tailscale", providerNetbird: "NetBird"}

// cdKey names one console domain: its network and name.
type cdKey struct{ prov, name string }

// consoleDomainKey marks a request that came through a console domain.
type consoleDomainKey struct{}

// consoleDomainOf returns the console domain a request came through.
func consoleDomainOf(ctx context.Context) (cdKey, bool) {
	k, ok := ctx.Value(consoleDomainKey{}).(cdKey)
	return k, ok
}

func withConsoleDomain(k cdKey, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := core.WithConsoleDomain(context.WithValue(r.Context(), consoleDomainKey{}, k), k.prov, k.name)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// consoleState is what the host knows about its console nodes. Guarded by
// Host.cdNamesMu.
type consoleState struct {
	owned     map[cdKey]bool   // domains with a console_nodes row
	served    map[cdKey]bool   // domains whose node this process serves
	conflicts map[cdKey]string // listed domains not served, and why
}

func newConsoleState() consoleState {
	return consoleState{owned: map[cdKey]bool{}, served: map[cdKey]bool{}, conflicts: map[cdKey]string{}}
}

// consoleBackend runs console domains on one network.
type consoleBackend interface {
	serve(ctx context.Context, name string, h http.Handler) error
	retire(name string) error
	url(name string) string
	state(name string) (core.HostInfo, bool)
	// hosts are the names a domain's node answers to, for HostGuard.
	hosts(name string) []string
	// handler wraps the console mux for a domain of this network.
	handler(name string, mux http.Handler) http.Handler
	ports() []string
}

// tsConsole runs console domains as tsnet nodes.
type tsConsole struct{ ts core.PrivateNet }

func (b tsConsole) serve(ctx context.Context, name string, h http.Handler) error {
	_, err := b.ts.Serve(ctx, name, h, false)
	return err
}
func (b tsConsole) retire(name string) error { return b.ts.Stop(name) }
func (b tsConsole) url(name string) string   { return b.ts.URL(name) }
func (b tsConsole) state(name string) (core.HostInfo, bool) {
	for _, hi := range b.ts.Status().Hosts {
		if hi.Host == name {
			return hi, true
		}
	}
	return core.HostInfo{}, false
}

// hosts are the configured name, and the node's MagicDNS name and first
// label once it knows them (control may have renamed a duplicate to e.g.
// flats-1).
func (b tsConsole) hosts(name string) []string {
	names := []string{name}
	if u, err := url.Parse(b.ts.URL(name)); err == nil {
		if fqdn := u.Hostname(); fqdn != "" && !strings.Contains(fqdn, "<") {
			short, _, _ := strings.Cut(fqdn, ".")
			names = append(names, fqdn, short)
		}
	}
	return names
}

// handler records the tailnet user of console decisions.
func (b tsConsole) handler(_ string, mux http.Handler) http.Handler { return api.TailnetIdentity(mux) }
func (b tsConsole) ports() []string                                 { return []string{"", "80", "443"} }

// nbConsole runs console domains as NetBird peers, over plain HTTP.
type nbConsole struct{ nn netbirdConsoleNet }

// netbirdConsoleNet is the part of provider.NetbirdNet console domains use.
type netbirdConsoleNet interface {
	ServeConsole(ctx context.Context, name string, h http.Handler) (string, error)
	RetireConsole(name string) error
	ConsoleURL(name string) string
	ConsoleStatus() core.NetStatus
	Hostnames(name string) []string
	IdentityForIP(name, ip string) string
}

func (b nbConsole) serve(ctx context.Context, name string, h http.Handler) error {
	_, err := b.nn.ServeConsole(ctx, name, h)
	return err
}
func (b nbConsole) retire(name string) error { return b.nn.RetireConsole(name) }
func (b nbConsole) url(name string) string   { return b.nn.ConsoleURL(name) }
func (b nbConsole) state(name string) (core.HostInfo, bool) {
	for _, hi := range b.nn.ConsoleStatus().Hosts {
		if hi.Host == name {
			return hi, true
		}
	}
	return core.HostInfo{}, false
}
func (b nbConsole) hosts(name string) []string {
	return append([]string{name}, b.nn.Hostnames(name)...)
}

// handler accepts console changes over plain HTTP (the browser sends no
// Sec-Fetch-Site there) and records the requesting NetBird peer as the
// author of console decisions and, for agent decisions, as the calling peer.
func (b nbConsole) handler(name string, mux http.Handler) http.Handler {
	inner := api.WithApprover(func(r *http.Request) string {
		if fqdn := core.NetbirdPeerOf(r.Context()); fqdn != "" {
			return "netbird:" + fqdn
		}
		return ""
	}, mux)
	return api.PlainHTTPConsole(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			if fqdn := b.nn.IdentityForIP(name, host); fqdn != "" {
				r = r.WithContext(core.WithNetbirdPeer(r.Context(), fqdn))
			}
		}
		inner.ServeHTTP(w, r)
	}))
}
func (b nbConsole) ports() []string { return []string{"", "80"} }

// consoleBackendOf is the backend console domains of prov run on, or nil
// while that network does not run.
func (h *Host) consoleBackendOf(prov string) consoleBackend {
	switch prov {
	case providerTailscale:
		if ts := h.tailnet(); ts != nil {
			return tsConsole{ts}
		}
	case providerNetbird:
		h.pmu.Lock()
		defer h.pmu.Unlock()
		if h.testNetbirdConsole != nil {
			return nbConsole{h.testNetbirdConsole}
		}
		if h.netbirdNet != nil {
			return nbConsole{h.netbirdNet}
		}
	}
	return nil
}

// tailnet is the Tailscale backend console nodes run on, or nil.
func (h *Host) tailnet() core.PrivateNet {
	h.pmu.Lock()
	defer h.pmu.Unlock()
	if h.testTailnet != nil {
		return h.testTailnet
	}
	if h.tsNet == nil {
		return nil
	}
	return h.tsNet
}

// providerOn reports whether prov runs and is turned on, so console domains
// may be added on it.
func (h *Host) providerOn(prov string) bool {
	return h.consoleBackendOf(prov) != nil && slices.Contains(h.Svc.PermittedNetworks(), prov)
}

// consoleHostList returns the Tailscale console domains in effect.
func (h *Host) consoleHostList() []string { return h.consoleHostsOf(providerTailscale) }

// consoleHostsOf returns the console domains of prov in effect.
func (h *Host) consoleHostsOf(prov string) []string {
	if h.Svc == nil {
		if h.Config == nil {
			return nil
		}
		if prov == providerNetbird {
			return slices.Clone(h.Config.Netbird.ConsoleHosts)
		}
		return slices.Clone(h.Config.Tailscale.ConsoleHosts)
	}
	l, _ := h.Svc.ConsoleHostsOf(prov)
	return l
}

// consoleReserved returns the names flats may not use: the console domains
// of both networks and every console node not yet retired.
func (h *Host) consoleReserved() []string {
	var names []string
	for _, prov := range consoleProviders {
		for _, n := range h.consoleHostsOf(prov) {
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}
	h.cdNamesMu.Lock()
	for k := range h.cd.owned {
		if !slices.Contains(names, k.name) {
			names = append(names, k.name)
		}
	}
	h.cdNamesMu.Unlock()
	return names
}

func (h *Host) setOwned(k cdKey, owned bool) {
	h.cdNamesMu.Lock()
	defer h.cdNamesMu.Unlock()
	if owned {
		h.cd.owned[k] = true
	} else {
		delete(h.cd.owned, k)
		delete(h.cd.served, k)
	}
}

func (h *Host) isOwned(k cdKey) bool {
	h.cdNamesMu.Lock()
	defer h.cdNamesMu.Unlock()
	return h.cd.owned[k]
}

func (h *Host) setServed(k cdKey, served bool, conflict string) {
	h.cdNamesMu.Lock()
	defer h.cdNamesMu.Unlock()
	if served {
		h.cd.served[k] = true
	} else {
		delete(h.cd.served, k)
	}
	if conflict != "" {
		h.cd.conflicts[k] = conflict
	} else {
		delete(h.cd.conflicts, k)
	}
}

func (h *Host) isServed(k cdKey) bool {
	h.cdNamesMu.Lock()
	defer h.cdNamesMu.Unlock()
	return h.cd.served[k]
}

// consoleHandler is the handler of a console domain's node: only that
// node's names are accepted as Host, and the request records the domain.
// Requests are counted for /metrics outside the guard, so refused ones count.
func (h *Host) consoleHandler(k cdKey) http.Handler {
	return h.metrics.Management(&api.HostGuard{
		Hosts: func() []string {
			if b := h.consoleBackendOf(k.prov); b != nil {
				return b.hosts(k.name)
			}
			return []string{k.name}
		},
		Ports: h.consolePorts(k.prov),
		Next: withConsoleDomain(k, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b := h.consoleBackendOf(k.prov)
			if b == nil {
				http.Error(w, "the console domain's network is not running", http.StatusServiceUnavailable)
				return
			}
			b.handler(k.name, h.Mux).ServeHTTP(w, r)
		})),
	})
}

func (h *Host) consolePorts(prov string) []string {
	if prov == providerNetbird {
		return nbConsole{}.ports()
	}
	return tsConsole{}.ports()
}

// consoleURLOf is the URL of a served console domain, or "".
func (h *Host) consoleURLOf(k cdKey) string {
	b := h.consoleBackendOf(k.prov)
	if b == nil || !h.isServed(k) {
		return ""
	}
	return b.url(k.name)
}

// ConsoleURL is the operator console address for a link that cannot follow
// the request: the preferred console (console.preferred_host) while it is
// served, else the first served console domain, Tailscale first, else the
// loopback address.
func (h *Host) ConsoleURL() string {
	if u := h.preferredURL(); u != "" {
		return u
	}
	for _, prov := range consoleProviders {
		for _, name := range h.consoleHostsOf(prov) {
			if u := h.consoleURLOf(cdKey{prov, name}); u != "" {
				return u
			}
		}
	}
	return h.console
}

// preferredConsole returns console.preferred_host as stored, and the console
// domain it names when that is listed: a zero cdKey with local true for the
// loopback address. ok is false without a preference in effect.
func (h *Host) preferredConsole() (value string, k cdKey, local, ok bool) {
	var network, name string
	switch {
	case h.Svc != nil:
		value, network, name, ok = h.Svc.ConsolePreferred()
	case h.Config != nil:
		value = h.Config.Console.PreferredHost
		network, name, ok = h.Config.Preferred()
	}
	if !ok {
		return value, cdKey{}, false, false
	}
	if network == "local" {
		return value, cdKey{}, true, true
	}
	return value, cdKey{network, name}, false, true
}

// preferredURL is the URL of the preferred console while it is ready, or ""
// without a preference in effect or while its domain is not ready. A domain
// counts as served from the moment its node is started, while a tailnet node
// may still be logging in or getting its certificate; a link that the
// operator chose must open, so the preferred domain needs its node ready
// (consoleReady, as the redirect after a self-delete does). The fallback
// order keeps its served test unchanged.
func (h *Host) preferredURL() string {
	_, k, local, ok := h.preferredConsole()
	switch {
	case !ok:
		return ""
	case local:
		return h.console
	case !h.consoleReady(k):
		return ""
	}
	return h.consoleURLOf(k)
}

// ConsoleURLFor is the console address for a request: the console domain it
// came through while that is still served, else ConsoleURL.
func (h *Host) ConsoleURLFor(ctx context.Context) string {
	if origin := api.ExternalOrigin(ctx); origin != "" && slices.Contains(h.Svc.ExternalConsoleOrigins(), origin) {
		return origin
	}
	if k, ok := consoleDomainOf(ctx); ok && slices.Contains(h.consoleHostsOf(k.prov), k.name) {
		if u := h.consoleURLOf(k); u != "" {
			return u
		}
	}
	return h.ConsoleURL()
}

// ConsoleURLs lists the URLs of every served console domain, Tailscale
// first, each list in order.
func (h *Host) ConsoleURLs() []string {
	var out []string
	for _, prov := range consoleProviders {
		for _, name := range h.consoleHostsOf(prov) {
			if u := h.consoleURLOf(cdKey{prov, name}); u != "" {
				out = append(out, u)
			}
		}
	}
	return out
}

// startConsoleDomains retires console nodes no longer listed, or still being
// retired, and serves the listed ones, on both networks. A listed name a
// flat, preview or redirect already uses is not served: the network would
// hand that node to the console. A listed name whose earlier retirement has
// not finished is served only once it succeeds, with a new identity. It runs
// at start and at every config reload; a name this process already serves
// is left as it is, and a conflict or failure of an earlier pass is retried.
func (h *Host) startConsoleDomains(ctx context.Context) error {
	// Retirement workers take cdMu too, so none changes a name's state before
	// this pass has set every listed name's.
	h.cdMu.Lock()
	defer h.cdMu.Unlock()
	nodes, err := h.Store.ListConsoleNodes(ctx)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		h.setOwned(cdKey{n.Provider, n.Name}, true)
	}
	retiring := map[cdKey]bool{}
	for _, n := range nodes {
		k := cdKey{n.Provider, n.Name}
		if slices.Contains(h.consoleHostsOf(k.prov), k.name) && !n.Retiring {
			continue
		}
		retiring[k] = true
		if h.consoleBackendOf(k.prov) == nil {
			h.logf("console domain %s was removed; its %s node is retired once %s is on again", k.name, providerLabel[k.prov], providerLabel[k.prov])
			continue
		}
		if !n.Retiring {
			if err := h.Store.PutConsoleNode(ctx, store.ConsoleNode{Provider: k.prov, Name: k.name, Retiring: true}); err != nil {
				return err
			}
		}
		h.background(func(ctx context.Context) { h.retireQueuedNode(ctx, k) })
	}
	for _, prov := range consoleProviders {
		for _, name := range h.consoleHostsOf(prov) {
			k := cdKey{prov, name}
			switch {
			case h.consoleBackendOf(prov) == nil:
				h.setServed(k, false, providerLabel[prov]+" is not running")
			case retiring[k]:
				h.setServed(k, false, "its previous node is still being retired")
			case h.isServed(k):
				// Served by this process already, by a start or an earlier
				// reload.
			default:
				if err := h.serveConsoleDomain(ctx, k); err != nil {
					h.logf("console domain %s (%s): %v", name, providerLabel[prov], err)
				}
			}
		}
	}
	return nil
}

// retireQueuedNode retires the node of k that a pass of startConsoleDomains
// marked retiring, and serves k again if it is listed once more.
func (h *Host) retireQueuedNode(ctx context.Context, k cdKey) {
	h.cdMu.Lock()
	defer h.cdMu.Unlock()
	// A closing host starts no further retirement; the node stays recorded
	// as retiring, so the next start retires it.
	if ctx.Err() != nil {
		h.logf("console domain %s: retirement of its %s node left to the next start: the host is shutting down", k.name, providerLabel[k.prov])
		return
	}
	// A later pass may have queued the same node again: only a node still
	// recorded as retiring is retired, so a finished retirement is not
	// repeated, its record not written back, and a node served since then
	// not stopped.
	if retiring, err := h.consoleNodeRetiring(ctx, k); err != nil {
		h.logf("console domain %s: retire its %s node: %v; retried at the next reload or start", k.name, providerLabel[k.prov], err)
		return
	} else if !retiring {
		return
	}
	if err := h.retireConsoleNode(ctx, k); err != nil {
		h.logf("console domain %s: retire its %s node: %v; retried at the next reload or start", k.name, providerLabel[k.prov], err)
		return
	}
	// Listed again (offline) while its retirement was unfinished: serve it now.
	if slices.Contains(h.consoleHostsOf(k.prov), k.name) {
		if err := h.serveConsoleDomain(ctx, k); err != nil {
			h.logf("console domain %s: %v", k.name, err)
		}
	}
}

// consoleNodeRetiring reports whether the node of k is still recorded as
// retiring. h.cdMu is held.
func (h *Host) consoleNodeRetiring(ctx context.Context, k cdKey) (bool, error) {
	nodes, err := h.Store.ListConsoleNodes(ctx)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(nodes, func(n store.ConsoleNode) bool {
		return n.Provider == k.prov && n.Name == k.name && n.Retiring
	}), nil
}

// retryConsoleDomains serves again the listed console domains of prov that
// are not served, for a NetBird setup key registered while the host runs.
func (h *Host) retryConsoleDomains(ctx context.Context, prov string) {
	h.cdMu.Lock()
	defer h.cdMu.Unlock()
	nodes, err := h.Store.ListConsoleNodes(ctx)
	if err != nil {
		h.logf("console domains (%s): %v", providerLabel[prov], err)
		return
	}
	for _, name := range h.consoleHostsOf(prov) {
		k := cdKey{prov, name}
		if h.isServed(k) {
			continue
		}
		// A name listed again while its earlier node is still being retired
		// is served only once that retirement succeeds, with a new identity.
		if slices.ContainsFunc(nodes, func(n store.ConsoleNode) bool { return n.Provider == prov && n.Name == name && n.Retiring }) {
			if err := h.retireConsoleNode(ctx, k); err != nil {
				h.logf("console domain %s (%s): retire its previous node: %v", name, providerLabel[prov], err)
				continue
			}
		}
		if err := h.serveConsoleDomain(ctx, k); err != nil {
			h.logf("console domain %s (%s): %v", name, providerLabel[prov], err)
		}
	}
}

// serveConsoleDomain records and serves the node of a listed domain, unless
// a flat, preview or redirect uses that name. The name's check and the start
// of its node run under the name's reservation, as a console addition's do,
// so a flat created meanwhile cannot take the name too.
func (h *Host) serveConsoleDomain(ctx context.Context, k cdKey) error {
	b := h.consoleBackendOf(k.prov)
	if b == nil {
		h.setServed(k, false, providerLabel[k.prov]+" is not running")
		return nil
	}
	return h.Svc.ServeConsoleName(ctx, k.name, func(holder string) error {
		if holder != "" {
			h.logf("warning: console domain %s is not served: %s uses that name; delete the domain or rename that flat", k.name, holder)
			h.setServed(k, false, "used by "+holder)
			return nil
		}
		if err := h.Store.PutConsoleNode(ctx, store.ConsoleNode{Provider: k.prov, Name: k.name}); err != nil {
			return err
		}
		h.setOwned(k, true)
		if err := b.serve(ctx, k.name, h.consoleHandler(k)); err != nil {
			h.setServed(k, false, err.Error())
			return err
		}
		h.setServed(k, true, "")
		return nil
	})
}

// retireConsoleNode stops a console node and deletes its identity and
// record. On failure the record stays, marked retiring, with the error.
// h.cdMu is held, so retirements and domain changes do not interleave.
func (h *Host) retireConsoleNode(ctx context.Context, k cdKey) error {
	b := h.consoleBackendOf(k.prov)
	if b == nil {
		return fmt.Errorf("%s is not running", providerLabel[k.prov])
	}
	h.setServed(k, false, "")
	if err := b.retire(k.name); err != nil {
		if perr := h.Store.PutConsoleNode(ctx, store.ConsoleNode{Provider: k.prov, Name: k.name, Retiring: true, LastError: err.Error()}); perr != nil {
			h.logf("console domain %s: record the failed retirement: %v", k.name, perr)
		}
		return err
	}
	if err := h.Store.DeleteConsoleNode(ctx, k.prov, k.name); err != nil {
		return err
	}
	h.setOwned(k, false)
	h.logf("console domain %s: %s node retired", k.name, providerLabel[k.prov])
	return nil
}

// selfRetireDelay is how long the node of a console domain deleted through
// itself keeps serving after the response; a variable for tests.
var selfRetireDelay = 2 * time.Second

// consoleReady reports whether a console domain is served and its node is
// ready, so a browser sent there finds the console.
func (h *Host) consoleReady(k cdKey) bool {
	b := h.consoleBackendOf(k.prov)
	if b == nil || !h.isServed(k) {
		return false
	}
	hi, ok := b.state(k.name)
	return ok && serving(hi.State)
}

// serving reports whether a node state means the node serves the console:
// ready, or ready with a Tailscale node key in its last days (key-expiring),
// as tsnet's WaitReady counts it.
func serving(state string) bool {
	return state == tsnetx.StateReady || state == tsnetx.StateKeyExpiring
}

// requestOnLoopback reports whether a request reached the management
// listener on a loopback address, as opposed to a console domain or a
// management address on another interface. Calls without a request (tests,
// the host itself) count as loopback.
func requestOnLoopback(ctx context.Context) bool {
	if api.ExternalOrigin(ctx) != "" {
		return false
	}
	if _, ok := consoleDomainOf(ctx); ok {
		return false
	}
	la, ok := ctx.Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || la == nil {
		return true
	}
	host, _, err := net.SplitHostPort(la.String())
	if err != nil {
		host = la.String()
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// ConsoleDomainInfo is one console domain as the console shows it.
type ConsoleDomainInfo struct {
	Name     string `json:"name"`
	Assigned string `json:"assigned,omitempty"` // the network's name, when it differs (e.g. flats-1)
	URL      string `json:"url,omitempty"`
	State    string `json:"state,omitempty"`  // the node's state while served
	Detail   string `json:"detail,omitempty"` // e.g. a login link
	Conflict string `json:"conflict,omitempty"`
	Error    string `json:"error,omitempty"` // a failed retirement of a removed name
	Current  bool   `json:"current,omitempty"`
}

// ConsoleProviderView is one network's console domains.
type ConsoleProviderView struct {
	Domains  []ConsoleDomainInfo `json:"domains"`
	Retiring []ConsoleDomainInfo `json:"retiring"`
	On       bool                `json:"on"` // the network runs and is turned on
	Pinned   string              `json:"pinned,omitempty"`
}

// ConsoleDomainRef names a console domain.
type ConsoleDomainRef struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

// ConsoleDomainsView is GET /console/api/console-domains.
type ConsoleDomainsView struct {
	Providers        map[string]*ConsoleProviderView `json:"providers"`     // "tailscale", "netbird"
	Via              *ConsoleDomainRef               `json:"via,omitempty"` // the console domain of this request
	Loopback         bool                            `json:"loopback"`      // the request came over the loopback address
	PendingApprovals int                             `json:"pending_approvals"`
	// Preferred is console.preferred_host: "", "local", "tailscale:NAME" or
	// "netbird:NAME".
	Preferred string `json:"preferred"`
	// PreferredState is "" without a preference, "served" while links use
	// it, "starting" while its domain's node runs but is not ready yet and
	// "not_served" while it is not served (links fall back in both), or
	// "unlisted" when it names a domain its list does not hold (ignored).
	PreferredState string `json:"preferred_state,omitempty"`
	// LinkURL is where links go when they cannot follow the request, as for
	// a request on the loopback address.
	LinkURL string `json:"link_url"`
	ETag    string `json:"etag"`
}

// Preferred states of ConsoleDomainsView.PreferredState.
const (
	preferredServed    = "served"
	preferredStarting  = "starting"
	preferredNotServed = "not_served"
	preferredUnlisted  = "unlisted"
)

// ConsoleDomains implements api.ConsoleDomainAdmin.
func (h *Host) ConsoleDomains(ctx context.Context) (any, error) { return h.consoleDomains(ctx) }

func (h *Host) consoleDomains(ctx context.Context) (ConsoleDomainsView, error) {
	v := ConsoleDomainsView{Providers: map[string]*ConsoleProviderView{}, ETag: h.Svc.SettingsETag()}
	via, viaOK := consoleDomainOf(ctx)
	if viaOK {
		v.Via = &ConsoleDomainRef{via.prov, via.name}
	}
	v.Loopback = requestOnLoopback(ctx)
	h.cdNamesMu.Lock()
	conflicts := make(map[cdKey]string, len(h.cd.conflicts))
	for k, c := range h.cd.conflicts {
		conflicts[k] = c
	}
	h.cdNamesMu.Unlock()
	nodes, err := h.Store.ListConsoleNodes(ctx)
	if err != nil {
		return v, err
	}
	for _, prov := range consoleProviders {
		list, pin := h.Svc.ConsoleHostsOf(prov)
		pv := &ConsoleProviderView{Domains: []ConsoleDomainInfo{}, Retiring: []ConsoleDomainInfo{}, Pinned: pin, On: h.providerOn(prov)}
		b := h.consoleBackendOf(prov)
		for _, name := range list {
			k := cdKey{prov, name}
			d := ConsoleDomainInfo{Name: name, URL: h.consoleURLOf(k), Conflict: conflicts[k], Current: viaOK && via == k}
			if b != nil && d.Conflict == "" {
				if hi, ok := b.state(name); ok {
					d.State, d.Detail = hi.State, hi.Detail
					if d.URL == "" {
						d.URL = hi.URL
					}
				}
			}
			if u, err := url.Parse(d.URL); err == nil && u.Hostname() != "" && !strings.Contains(u.Hostname(), "<") && net.ParseIP(u.Hostname()) == nil {
				if first, _, _ := strings.Cut(u.Hostname(), "."); first != name {
					d.Assigned = first
				}
			}
			pv.Domains = append(pv.Domains, d)
		}
		for _, n := range nodes {
			if n.Provider == prov && !slices.Contains(list, n.Name) {
				pv.Retiring = append(pv.Retiring, ConsoleDomainInfo{Name: n.Name, Error: n.LastError})
			}
		}
		v.Providers[prov] = pv
	}
	if as, err := h.Svc.ListApprovals(ctx, "pending"); err == nil {
		v.PendingApprovals = len(as)
	}
	value, k, local, ok := h.preferredConsole()
	v.Preferred = value
	switch {
	case value == "":
	case !ok:
		v.PreferredState = preferredUnlisted
	case local || h.consoleReady(k):
		v.PreferredState = preferredServed
	case h.isServed(k):
		v.PreferredState = preferredStarting
	default:
		v.PreferredState = preferredNotServed
	}
	v.LinkURL = h.ConsoleURL()
	return v, nil
}

// SetPreferredConsole implements api.ConsoleDomainAdmin: it saves
// console.preferred_host ("" unsets it). A console domain must be listed;
// it need not be served yet.
func (h *Host) SetPreferredConsole(ctx context.Context, value, ifMatch string) (string, error) {
	// Serialized with domain changes, so a name is never preferred while a
	// concurrent deletion removes it.
	h.cdMu.Lock()
	defer h.cdMu.Unlock()
	etag, err := h.Svc.SetConsolePreferred(value, ifMatch)
	if err != nil {
		return "", err
	}
	if value == "" {
		h.logf("preferred console unset from the console")
	} else {
		h.logf("preferred console set to %s from the console", value)
	}
	return etag, nil
}

func checkConsoleProvider(prov string) error {
	if !slices.Contains(consoleProviders, prov) {
		return fmt.Errorf("%w: unknown console domain network %q: use tailscale or netbird", core.ErrInvalid, prov)
	}
	return nil
}

// AddConsoleDomain implements api.ConsoleDomainAdmin: it starts the node of
// name on prov and adds name to that network's console domain list.
func (h *Host) AddConsoleDomain(ctx context.Context, prov, name, ifMatch string) (string, error) {
	if err := checkConsoleProvider(prov); err != nil {
		return "", err
	}
	h.cdMu.Lock()
	defer h.cdMu.Unlock()
	list, pin := h.Svc.ConsoleHostsOf(prov)
	if pin != "" {
		return "", fmt.Errorf("%w: the console domains are set by %s; reinstall the service with `flats install` to manage them here", core.ErrConflict, pin)
	}
	if err := h.Svc.CheckSettingsMatch(ifMatch); err != nil {
		return "", err
	}
	if !config.ConsoleHostRE.MatchString(name) {
		return "", fmt.Errorf("%w: %q is not a lowercase DNS label: use 1 to 63 lowercase letters, digits and '-', not starting or ending with '-'", core.ErrInvalid, name)
	}
	k := cdKey{prov, name}
	if slices.Contains(list, name) {
		return "", fmt.Errorf("%w: %q is already a %s console domain", core.ErrConflict, name, providerLabel[prov])
	}
	if h.isOwned(k) {
		return "", fmt.Errorf("%w: %q is still being removed; delete it again to retry", core.ErrConflict, name)
	}
	b := h.consoleBackendOf(prov)
	if b == nil || !slices.Contains(h.Svc.PermittedNetworks(), prov) {
		return "", fmt.Errorf("%w: turn %s on (Settings > Network providers) before adding a console domain", core.ErrConflict, providerLabel[prov])
	}
	// The other network's console domain may use the same name.
	reserve := h.Svc.ReserveConsoleName
	if h.consoleNameOfOtherOnly(k) {
		reserve = h.Svc.ReserveSharedConsoleName
	}
	var etag string
	err := reserve(ctx, name, func() error {
		if err := h.Store.PutConsoleNode(ctx, store.ConsoleNode{Provider: prov, Name: name}); err != nil {
			return err
		}
		h.setOwned(k, true)
		if err := b.serve(ctx, name, h.consoleHandler(k)); err != nil {
			// The node may have created an identity: retire it before the
			// name is released. If that fails, the record stays as retiring
			// and keeps the name reserved until a later retirement.
			if rerr := h.retireConsoleNode(context.WithoutCancel(ctx), k); rerr != nil {
				h.logf("console domain %s: retire the node after a failed start: %v", name, rerr)
			}
			return fmt.Errorf("%w: %v", core.ErrConflict, err)
		}
		h.setServed(k, true, "")
		var err error
		if etag, err = h.Svc.SetConsoleHostsOf(prov, append(list, name), ifMatch); err != nil {
			if rerr := h.retireConsoleNode(context.Background(), k); rerr != nil {
				h.logf("console domain %s: stop the node after a failed save: %v", name, rerr)
			}
			return err
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	h.logf("console domain %s (%s) added from the console", name, providerLabel[prov])
	return etag, nil
}

// consoleNameOfOtherOnly reports whether k's name is a console domain (or
// node) only of the other network, which may share it.
func (h *Host) consoleNameOfOtherOnly(k cdKey) bool {
	other := providerNetbird
	if k.prov == providerNetbird {
		other = providerTailscale
	}
	o := cdKey{other, k.name}
	return slices.Contains(h.consoleHostsOf(other), k.name) || h.isOwned(o)
}

// DeleteResult is the outcome of deleting a console domain. After, when set,
// retires the node that served the request itself; the caller runs it once
// the response is written.
type DeleteResult struct {
	ETag     string `json:"etag"`
	Redirect string `json:"redirect,omitempty"`
	After    func() `json:"-"`
}

// DeleteConsoleDomain implements api.ConsoleDomainAdmin.
func (h *Host) DeleteConsoleDomain(ctx context.Context, prov, name, ifMatch string) (string, string, func(), error) {
	if err := checkConsoleProvider(prov); err != nil {
		return "", "", nil, err
	}
	res, err := h.deleteConsoleDomain(ctx, cdKey{prov, name}, ifMatch)
	return res.ETag, res.Redirect, res.After, err
}

// deleteConsoleDomain removes a domain from its network's list, then retires
// its node.
func (h *Host) deleteConsoleDomain(ctx context.Context, k cdKey, ifMatch string) (DeleteResult, error) {
	h.cdMu.Lock()
	defer h.cdMu.Unlock()
	list, pin := h.Svc.ConsoleHostsOf(k.prov)
	if pin != "" {
		return DeleteResult{}, fmt.Errorf("%w: the console domains are set by %s; reinstall the service with `flats install` to manage them here", core.ErrConflict, pin)
	}
	if !slices.Contains(list, k.name) {
		// A removed name whose retirement failed: retry it.
		if !h.isOwned(k) {
			return DeleteResult{}, fmt.Errorf("%w: %q is not a %s console domain", store.ErrNotFound, k.name, providerLabel[k.prov])
		}
		if err := h.retireConsoleNode(ctx, k); err != nil {
			return DeleteResult{}, fmt.Errorf("%w: retire the %s node of %s: %v", core.ErrUnavailable, providerLabel[k.prov], k.name, err)
		}
		return DeleteResult{ETag: h.Svc.SettingsETag()}, nil
	}
	via, viaOK := consoleDomainOf(ctx)
	next := slices.DeleteFunc(slices.Clone(list), func(n string) bool { return n == k.name })
	remaining := len(next)
	for _, prov := range consoleProviders {
		if prov != k.prov {
			remaining += len(h.consoleHostsOf(prov))
		}
	}
	if remaining == 0 && !requestOnLoopback(ctx) {
		return DeleteResult{}, fmt.Errorf("%w: %s is the console's last domain; add another one first, or delete it from the console on the loopback address", core.ErrConflict, k.name)
	}
	redirect := ""
	if viaOK && via == k {
		// The page moves to a remaining domain that already answers: the
		// preferred one if it does, else the first in the fixed order.
		candidates := []cdKey{}
		if _, p, local, ok := h.preferredConsole(); ok && !local {
			candidates = append(candidates, p)
		}
		for _, prov := range consoleProviders {
			for _, n := range h.consoleHostsOf(prov) {
				candidates = append(candidates, cdKey{prov, n})
			}
		}
		for _, o := range candidates {
			if o != k && h.consoleReady(o) {
				redirect = h.consoleURLOf(o)
				break
			}
		}
		if redirect == "" {
			return DeleteResult{}, fmt.Errorf("%w: no other console domain is ready yet; wait until one is, or delete %s from another console address", core.ErrConflict, k.name)
		}
	}
	before, _, _, _ := h.preferredConsole()
	// Saving the list also unsets console.preferred_host when it names k.
	etag, err := h.Svc.SetConsoleHostsOf(k.prov, next, ifMatch)
	if err != nil {
		return DeleteResult{}, err
	}
	h.logf("console domain %s (%s) deleted from the console", k.name, providerLabel[k.prov])
	if after, _, _, _ := h.preferredConsole(); before != after {
		h.logf("preferred console %s unset: its domain was deleted", before)
	}
	res := DeleteResult{ETag: etag, Redirect: redirect}
	if !h.isOwned(k) {
		// Never served (its name was in use elsewhere): there is no node of
		// the console to retire, and the name's owner keeps its own.
		h.setServed(k, false, "")
		return res, nil
	}
	if err := h.Store.PutConsoleNode(ctx, store.ConsoleNode{Provider: k.prov, Name: k.name, Retiring: true}); err != nil {
		h.logf("console domain %s: mark retiring: %v", k.name, err)
	}
	if viaOK && via == k {
		res.After = func() {
			h.background(func(ctx context.Context) {
				// Stopping the node closes the connection that carries this
				// response; give the response time to reach the browser.
				select {
				case <-time.After(selfRetireDelay):
				case <-ctx.Done():
				}
				h.cdMu.Lock()
				defer h.cdMu.Unlock()
				if err := h.retireConsoleNode(ctx, k); err != nil {
					h.logf("console domain %s: retire its %s node: %v; retry from the console or at the next start", k.name, providerLabel[k.prov], err)
				}
			})
		}
		return res, nil
	}
	if err := h.retireConsoleNode(ctx, k); err != nil {
		return res, fmt.Errorf("%w: %s was removed from the console domains, but its %s node was not retired: %v; delete it again to retry, or it is retried at the next start", core.ErrUnavailable, k.name, providerLabel[k.prov], err)
	}
	return res, nil
}
