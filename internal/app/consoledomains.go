package app

import (
	"context"
	"errors"
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
	"github.com/gosuda/flats/internal/store"
)

// The console runs on the tailnet as one tsnet node per console domain
// (tailscale.console_hosts). console_nodes in flats.db records every node
// Flats runs for the console, so a domain removed from the list, even
// offline, is logged out and its identity deleted at the next start.

// consoleDomainKey marks a request that came through a console domain.
type consoleDomainKey struct{}

// consoleDomainFrom returns the console domain a request came through.
func consoleDomainFrom(ctx context.Context) (string, bool) {
	name, ok := ctx.Value(consoleDomainKey{}).(string)
	return name, ok
}

func withConsoleDomain(name string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), consoleDomainKey{}, name)))
	})
}

// consoleState is what the host knows about its console nodes. Guarded by
// Host.cdNamesMu.
type consoleState struct {
	owned     map[string]bool   // names with a console_nodes row
	served    map[string]bool   // names whose node this process serves
	conflicts map[string]string // listed names not served, and why
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

// consoleHostList returns the console domains in effect.
func (h *Host) consoleHostList() []string {
	if h.Svc == nil {
		if h.Config == nil {
			return nil
		}
		return slices.Clone(h.Config.Tailscale.ConsoleHosts)
	}
	l, _ := h.Svc.ConsoleHosts()
	return l
}

// consoleReserved returns the names flats may not use: the console domains
// and every console node not yet retired.
func (h *Host) consoleReserved() []string {
	names := h.consoleHostList()
	h.cdNamesMu.Lock()
	for n := range h.cd.owned {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	h.cdNamesMu.Unlock()
	return names
}

func (h *Host) setOwned(name string, owned bool) {
	h.cdNamesMu.Lock()
	defer h.cdNamesMu.Unlock()
	if owned {
		h.cd.owned[name] = true
	} else {
		delete(h.cd.owned, name)
		delete(h.cd.served, name)
	}
}

func (h *Host) setServed(name string, served bool, conflict string) {
	h.cdNamesMu.Lock()
	defer h.cdNamesMu.Unlock()
	if served {
		h.cd.served[name] = true
	} else {
		delete(h.cd.served, name)
	}
	if conflict != "" {
		h.cd.conflicts[name] = conflict
	} else {
		delete(h.cd.conflicts, name)
	}
}

func (h *Host) isServed(name string) bool {
	h.cdNamesMu.Lock()
	defer h.cdNamesMu.Unlock()
	return h.cd.served[name]
}

// consoleHandler is the handler of console domain name's node: only that
// node's names are accepted as Host, the request records the domain, and
// console decisions record the tailnet user.
func (h *Host) consoleHandler(name string) http.Handler {
	return &api.HostGuard{Hosts: func() []string { return h.consoleNames(name) }, Ports: []string{"", "80", "443"},
		Next: withConsoleDomain(name, api.TailnetIdentity(h.Mux))}
}

// consoleNames are the tailnet names of console node name: the configured
// name, and its MagicDNS name and first label once the node knows them
// (control may have renamed a duplicate to e.g. flats-1).
func (h *Host) consoleNames(name string) []string {
	names := []string{name}
	ts := h.tailnet()
	if ts == nil {
		return names
	}
	if u, err := url.Parse(ts.URL(name)); err == nil {
		if fqdn := u.Hostname(); fqdn != "" && !strings.Contains(fqdn, "<") {
			short, _, _ := strings.Cut(fqdn, ".")
			names = append(names, fqdn, short)
		}
	}
	return names
}

// consoleURLOf is the URL of a served console domain, or "".
func (h *Host) consoleURLOf(name string) string {
	ts := h.tailnet()
	if ts == nil || !h.isServed(name) {
		return ""
	}
	return ts.URL(name)
}

// ConsoleURL is the operator console address: the first served console
// domain, else the loopback address.
func (h *Host) ConsoleURL() string {
	for _, name := range h.consoleHostList() {
		if u := h.consoleURLOf(name); u != "" {
			return u
		}
	}
	return h.console
}

// ConsoleURLFor is the console address for a request: the console domain it
// came through while that is still served, else ConsoleURL.
func (h *Host) ConsoleURLFor(ctx context.Context) string {
	if name, ok := consoleDomainFrom(ctx); ok && slices.Contains(h.consoleHostList(), name) {
		if u := h.consoleURLOf(name); u != "" {
			return u
		}
	}
	return h.ConsoleURL()
}

// ConsoleURLs lists the URLs of every served console domain, in order.
func (h *Host) ConsoleURLs() []string {
	var out []string
	for _, name := range h.consoleHostList() {
		if u := h.consoleURLOf(name); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// startConsoleDomains retires console nodes no longer listed, or still being
// retired, and serves the listed ones. A listed name a flat, preview or
// redirect already uses is not served: tsnet would hand that node to the
// console. A listed name whose earlier logout has not finished is served only
// once that logout succeeds, with a new identity.
func (h *Host) startConsoleDomains(ctx context.Context) error {
	// Retirement workers take cdMu too, so none changes a name's state before
	// this pass has set every listed name's.
	h.cdMu.Lock()
	defer h.cdMu.Unlock()
	nodes, err := h.Store.ListConsoleNodes(ctx)
	if err != nil {
		return err
	}
	list := h.consoleHostList()
	for _, n := range nodes {
		h.setOwned(n.Name, true)
	}
	ts := h.tailnet()
	retiring := map[string]bool{}
	for _, n := range nodes {
		if slices.Contains(list, n.Name) && !n.Retiring {
			continue
		}
		retiring[n.Name] = true
		if ts == nil {
			h.logf("console domain %s was removed; its tailnet node is logged out once Tailscale is on again", n.Name)
			continue
		}
		if !n.Retiring {
			if err := h.Store.PutConsoleNode(ctx, store.ConsoleNode{Name: n.Name, Retiring: true}); err != nil {
				return err
			}
		}
		name := n.Name
		h.background(func(ctx context.Context) {
			h.cdMu.Lock()
			defer h.cdMu.Unlock()
			if err := h.retireConsoleNode(ctx, name); err != nil {
				h.logf("console domain %s: log out its tailnet node: %v; retried at the next start", name, err)
				return
			}
			// Listed again (offline) while its logout was unfinished: serve it now.
			if slices.Contains(h.consoleHostList(), name) {
				if err := h.serveConsoleDomain(ctx, name); err != nil {
					h.logf("console domain %s: %v", name, err)
				}
			}
		})
	}
	for _, name := range list {
		switch {
		case ts == nil:
			h.setServed(name, false, "Tailscale is not running")
		case retiring[name]:
			h.setServed(name, false, "its previous tailnet node is still being logged out")
		default:
			if err := h.serveConsoleDomain(ctx, name); err != nil {
				h.logf("console domain %s: %v", name, err)
			}
		}
	}
	return nil
}

// serveConsoleDomain records and serves the node of a listed name, unless a
// flat, preview or redirect uses that name.
func (h *Host) serveConsoleDomain(ctx context.Context, name string) error {
	ts := h.tailnet()
	if ts == nil {
		h.setServed(name, false, "Tailscale is not running")
		return nil
	}
	holder, err := h.Svc.ConsoleNameHolder(ctx, name)
	if err != nil {
		return err
	}
	if holder != "" {
		h.logf("warning: console domain %s is not served: %s uses that name; delete the domain or rename that flat", name, holder)
		h.setServed(name, false, "used by "+holder)
		return nil
	}
	if err := h.Store.PutConsoleNode(ctx, store.ConsoleNode{Name: name}); err != nil {
		return err
	}
	h.setOwned(name, true)
	if _, err := ts.Serve(ctx, name, h.consoleHandler(name), false); err != nil {
		h.setServed(name, false, err.Error())
		return err
	}
	h.setServed(name, true, "")
	return nil
}

// retireConsoleNode logs a console node out and deletes its identity and
// record. On failure the record stays, marked retiring, with the error.
// h.cdMu is held, so retirements and domain changes do not interleave.
func (h *Host) retireConsoleNode(ctx context.Context, name string) error {
	ts := h.tailnet()
	if ts == nil {
		return errors.New("Tailscale is not running")
	}
	h.setServed(name, false, "")
	if err := ts.Stop(name); err != nil {
		if perr := h.Store.PutConsoleNode(ctx, store.ConsoleNode{Name: name, Retiring: true, LastError: err.Error()}); perr != nil {
			h.logf("console domain %s: record the failed logout: %v", name, perr)
		}
		return err
	}
	if err := h.Store.DeleteConsoleNode(ctx, name); err != nil {
		return err
	}
	h.setOwned(name, false)
	h.logf("console domain %s: tailnet node logged out and removed", name)
	return nil
}

// selfRetireDelay is how long the node of a console domain deleted through
// itself keeps serving after the response; a variable for tests.
var selfRetireDelay = 2 * time.Second

// consoleReady reports whether a console domain is served and its node is
// ready, so a browser sent there finds the console.
func (h *Host) consoleReady(name string) bool {
	ts := h.tailnet()
	if ts == nil || !h.isServed(name) {
		return false
	}
	for _, hi := range ts.Status().Hosts {
		if hi.Host == name {
			return hi.State == "ready"
		}
	}
	return false
}

// requestOnLoopback reports whether a request reached the management
// listener on a loopback address, as opposed to a console domain or a
// management address on another interface. Calls without a request (tests,
// the host itself) count as loopback.
func requestOnLoopback(ctx context.Context) bool {
	if _, ok := consoleDomainFrom(ctx); ok {
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
	Assigned string `json:"assigned,omitempty"` // the tailnet's name, when it differs (e.g. flats-1)
	URL      string `json:"url,omitempty"`
	State    string `json:"state,omitempty"`  // the node's state while served
	Detail   string `json:"detail,omitempty"` // e.g. a login link
	Conflict string `json:"conflict,omitempty"`
	Error    string `json:"error,omitempty"` // a failed logout of a removed name
	Current  bool   `json:"current,omitempty"`
}

// ConsoleDomainsView is GET /console/api/console-domains.
type ConsoleDomainsView struct {
	Domains          []ConsoleDomainInfo `json:"domains"`
	Retiring         []ConsoleDomainInfo `json:"retiring"`
	Via              string              `json:"via,omitempty"` // the console domain of this request
	Loopback         bool                `json:"loopback"`      // the request came over the loopback address
	TailscaleOn      bool                `json:"tailscale_on"`
	Pinned           string              `json:"pinned,omitempty"`
	PendingApprovals int                 `json:"pending_approvals"`
	ETag             string              `json:"etag"`
}

// ConsoleDomains implements api.ConsoleDomainAdmin.
func (h *Host) ConsoleDomains(ctx context.Context) (any, error) { return h.consoleDomains(ctx) }

func (h *Host) consoleDomains(ctx context.Context) (ConsoleDomainsView, error) {
	list, pin := h.Svc.ConsoleHosts()
	v := ConsoleDomainsView{Domains: []ConsoleDomainInfo{}, Retiring: []ConsoleDomainInfo{}, Pinned: pin, ETag: h.Svc.SettingsETag()}
	v.Via, _ = consoleDomainFrom(ctx)
	v.Loopback = requestOnLoopback(ctx)
	ts := h.tailnet()
	v.TailscaleOn = ts != nil && slices.Contains(h.Svc.PermittedNetworks(), "tailscale")
	states := map[string]core.HostInfo{}
	if ts != nil {
		for _, hi := range ts.Status().Hosts {
			states[hi.Host] = hi
		}
	}
	h.cdNamesMu.Lock()
	conflicts := make(map[string]string, len(h.cd.conflicts))
	for k, c := range h.cd.conflicts {
		conflicts[k] = c
	}
	h.cdNamesMu.Unlock()
	for _, name := range list {
		d := ConsoleDomainInfo{Name: name, URL: h.consoleURLOf(name), Conflict: conflicts[name], Current: name == v.Via}
		if hi, ok := states[name]; ok && d.Conflict == "" {
			d.State, d.Detail = hi.State, hi.Detail
			if d.URL == "" {
				d.URL = hi.URL
			}
		}
		if u, err := url.Parse(d.URL); err == nil && u.Hostname() != "" && !strings.Contains(u.Hostname(), "<") {
			if first, _, _ := strings.Cut(u.Hostname(), "."); first != name {
				d.Assigned = first
			}
		}
		v.Domains = append(v.Domains, d)
	}
	nodes, err := h.Store.ListConsoleNodes(ctx)
	if err != nil {
		return v, err
	}
	for _, n := range nodes {
		if !slices.Contains(list, n.Name) {
			v.Retiring = append(v.Retiring, ConsoleDomainInfo{Name: n.Name, Error: n.LastError})
		}
	}
	if as, err := h.Svc.ListApprovals(ctx, "pending"); err == nil {
		v.PendingApprovals = len(as)
	}
	return v, nil
}

// AddConsoleDomain implements api.ConsoleDomainAdmin: it starts the node of
// name and adds name to tailscale.console_hosts.
func (h *Host) AddConsoleDomain(ctx context.Context, name, ifMatch string) (string, error) {
	h.cdMu.Lock()
	defer h.cdMu.Unlock()
	list, pin := h.Svc.ConsoleHosts()
	if pin != "" {
		return "", fmt.Errorf("%w: the console domains are set by %s; reinstall the service with `flats install` to manage them here", core.ErrConflict, pin)
	}
	if err := h.Svc.CheckSettingsMatch(ifMatch); err != nil {
		return "", err
	}
	if !config.ConsoleHostRE.MatchString(name) {
		return "", fmt.Errorf("%w: %q is not a lowercase DNS label: use 1 to 63 lowercase letters, digits and '-', not starting or ending with '-'", core.ErrInvalid, name)
	}
	if slices.Contains(list, name) {
		return "", fmt.Errorf("%w: %q is already a console domain", core.ErrConflict, name)
	}
	ts := h.tailnet()
	if ts == nil || !slices.Contains(h.Svc.PermittedNetworks(), "tailscale") {
		return "", fmt.Errorf("%w: turn Tailscale on (Settings > Network providers) before adding a console domain", core.ErrConflict)
	}
	var etag string
	err := h.Svc.ReserveConsoleName(ctx, name, func() error {
		if err := h.Store.PutConsoleNode(ctx, store.ConsoleNode{Name: name}); err != nil {
			return err
		}
		h.setOwned(name, true)
		if _, err := ts.Serve(ctx, name, h.consoleHandler(name), false); err != nil {
			if derr := h.Store.DeleteConsoleNode(ctx, name); derr != nil {
				h.logf("console domain %s: %v", name, derr)
				return err
			}
			h.setOwned(name, false)
			return fmt.Errorf("%w: %v", core.ErrConflict, err)
		}
		h.setServed(name, true, "")
		var err error
		if etag, err = h.Svc.SetConsoleHosts(append(list, name), ifMatch); err != nil {
			if rerr := h.retireConsoleNode(context.Background(), name); rerr != nil {
				h.logf("console domain %s: stop the node after a failed save: %v", name, rerr)
			}
			return err
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	h.logf("console domain %s added from the console", name)
	return etag, nil
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
func (h *Host) DeleteConsoleDomain(ctx context.Context, name, ifMatch string) (string, string, func(), error) {
	res, err := h.deleteConsoleDomain(ctx, name, ifMatch)
	return res.ETag, res.Redirect, res.After, err
}

// deleteConsoleDomain removes name from tailscale.console_hosts, then logs
// its node out.
func (h *Host) deleteConsoleDomain(ctx context.Context, name, ifMatch string) (DeleteResult, error) {
	h.cdMu.Lock()
	defer h.cdMu.Unlock()
	list, pin := h.Svc.ConsoleHosts()
	if pin != "" {
		return DeleteResult{}, fmt.Errorf("%w: the console domains are set by %s; reinstall the service with `flats install` to manage them here", core.ErrConflict, pin)
	}
	if !slices.Contains(list, name) {
		// A removed name whose logout failed: retry it.
		h.cdNamesMu.Lock()
		owned := h.cd.owned[name]
		h.cdNamesMu.Unlock()
		if !owned {
			return DeleteResult{}, fmt.Errorf("%w: %q is not a console domain", store.ErrNotFound, name)
		}
		if err := h.retireConsoleNode(ctx, name); err != nil {
			return DeleteResult{}, fmt.Errorf("%w: log out the tailnet node of %s: %v", core.ErrUnavailable, name, err)
		}
		return DeleteResult{ETag: h.Svc.SettingsETag()}, nil
	}
	via, _ := consoleDomainFrom(ctx)
	next := slices.DeleteFunc(slices.Clone(list), func(n string) bool { return n == name })
	if len(next) == 0 && !requestOnLoopback(ctx) {
		return DeleteResult{}, fmt.Errorf("%w: %s is the console's last Tailscale domain; add another one first, or delete it from the console on the loopback address", core.ErrConflict, name)
	}
	redirect := ""
	if via == name {
		// The page moves to a remaining domain that already answers.
		for _, n := range next {
			if h.consoleReady(n) {
				redirect = h.consoleURLOf(n)
				break
			}
		}
		if redirect == "" {
			return DeleteResult{}, fmt.Errorf("%w: no other console domain is ready yet; wait until one is, or delete %s from another console address", core.ErrConflict, name)
		}
	}
	etag, err := h.Svc.SetConsoleHosts(next, ifMatch)
	if err != nil {
		return DeleteResult{}, err
	}
	h.logf("console domain %s deleted from the console", name)
	res := DeleteResult{ETag: etag, Redirect: redirect}
	h.cdNamesMu.Lock()
	owned := h.cd.owned[name]
	h.cdNamesMu.Unlock()
	if !owned {
		// Never served (its name was in use elsewhere): there is no node of
		// the console to log out, and the name's owner keeps its own.
		h.setServed(name, false, "")
		return res, nil
	}
	if err := h.Store.PutConsoleNode(ctx, store.ConsoleNode{Name: name, Retiring: true}); err != nil {
		h.logf("console domain %s: mark retiring: %v", name, err)
	}
	if via == name {
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
				if err := h.retireConsoleNode(ctx, name); err != nil {
					h.logf("console domain %s: log out its tailnet node: %v; retry from the console or at the next start", name, err)
				}
			})
		}
		return res, nil
	}
	if err := h.retireConsoleNode(ctx, name); err != nil {
		return res, fmt.Errorf("%w: %s was removed from the console domains, but its tailnet node was not logged out: %v; delete it again to retry, or it is retried at the next start", core.ErrUnavailable, name, err)
	}
	return res, nil
}
