package tsnet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	ts "tailscale.com/tsnet"
)

// Funnel connection states. These describe the internet listener only.
// Private tailnet HTTP is reported separately by Status.
const (
	FunnelStarting    = "starting"
	FunnelReady       = "ready"
	FunnelError       = "error"
	FunnelUnavailable = "unavailable"
)

// FunnelReport is the internet route for one host.
type FunnelReport struct {
	URL    string
	State  string
	Detail string
}

// ServeFunnel serves h on the public internet with Tailscale Funnel.
// The listener is FunnelOnly on TCP 443, so tailnet peers are not accepted
// on it. Private ACL HTTP, when requested through Serve, stays on its own
// listener. Joining the tailnet does not publish this route; the caller must
// invoke ServeFunnel. Ephemeral preview hosts are rejected.
func (n *Net) ServeFunnel(ctx context.Context, host string, h http.Handler) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !hostRE.MatchString(host) {
		return "", fmt.Errorf("tsnet: invalid host %q (want a lowercase DNS label)", host)
	}
	if h == nil {
		return "", errors.New("tsnet: nil handler")
	}
	nd, minted, launch, err := n.beginFunnel(host, h)
	if err != nil {
		return "", err
	}
	if launch {
		go n.setupFunnel(nd, minted)
	}
	return n.funnelURL(nd), nil
}

// setupFunnel waits for the node and opens Funnel independently of the caller.
// Restore and management startup must not wait for control, login, or listener
// readiness; FunnelStatus reports the later outcome.
func (n *Net) setupFunnel(nd *node, minted bool) {
	select {
	case <-nd.booted:
	case <-nd.ctx.Done():
		n.mu.Lock()
		nd.funnelOpening = false
		n.mu.Unlock()
		return
	}

	nd.funnelMu.Lock()
	n.mu.Lock()
	active := n.nodes[nd.host] == nd && nd.funnelH.Load() != nil && !nd.funnelCleanup
	bootOK := nd.bootOK && nd.err == nil && nd.srv != nil
	already := nd.funnelLn != nil && (nd.funnelState == FunnelReady || nd.funnelState == FunnelStarting)
	bootErr := nd.err
	n.mu.Unlock()
	if !active {
		n.mu.Lock()
		nd.funnelOpening = false
		n.mu.Unlock()
		nd.funnelMu.Unlock()
		return
	}
	if !bootOK {
		if bootErr == nil {
			bootErr = errors.New("node did not come up")
		}
		nd.funnelMu.Unlock()
		n.failFunnelSetup(nd, minted, fmt.Errorf("tsnet: funnel %s: %w", nd.host, bootErr))
		return
	}
	if already {
		n.mu.Lock()
		nd.funnelOpening = false
		n.mu.Unlock()
		nd.funnelMu.Unlock()
		return
	}

	// FunnelOnly registers only the funnel listen key. ListenFunnel without it
	// uses listen-on-both and refuses :443 when the private tailnet listener
	// already holds that port (pinned tsnet registerListener). That refusal
	// can still persist AllowFunnel, because SetServeConfig runs before
	// listen. The two keys share the port number and stay separate listeners.
	opts := []ts.FunnelOption{ts.FunnelOnly()}
	if n.getCert != nil {
		opts = append(opts, ts.FunnelTLSConfig(&tls.Config{GetCertificate: n.getCert}))
	}
	priorAllow, _ := n.allowFunnelSet(nd)
	ln, err := n.openFunnel(nd.srv, opts)
	if err != nil {
		err = n.undoNewAllowFunnel(nd, priorAllow, err)
		nd.funnelMu.Unlock()
		n.failFunnelSetup(nd, minted, fmt.Errorf("tsnet: funnel %s: %w", nd.host, err))
		return
	}
	if n.afterFunnelListen != nil {
		if err := n.afterFunnelListen(); err != nil {
			ln.Close()
			err = n.undoNewAllowFunnel(nd, priorAllow, err)
			nd.funnelMu.Unlock()
			n.failFunnelSetup(nd, minted, fmt.Errorf("tsnet: funnel %s: %w", nd.host, err))
			return
		}
	}
	hs := &http.Server{
		Handler:           n.publicHandler(nd),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	n.mu.Lock()
	nd.funnelLn = ln
	nd.funnelSrv = hs
	nd.funnelURL = n.funnelURLLocked(nd)
	nd.funnelOpening = false
	nd.funnelState = FunnelStarting
	nd.funnelDetail = "Funnel listener is up; waiting for its HTTPS certificate"
	if n.getCert != nil {
		nd.funnelState = FunnelReady
		nd.funnelDetail = "Funnel is accepting internet connections on port 443; tailnet peers use the private listener"
	}
	delete(n.funnelReports, nd.host)
	n.notifyLocked()
	n.mu.Unlock()
	nd.funnelMu.Unlock()
	go n.serveFunnel(nd, hs, ln)
	if n.getCert == nil {
		go n.warmFunnel(nd)
	}
}

func (n *Net) failFunnelSetup(nd *node, minted bool, cause error) {
	n.mu.Lock()
	active := n.nodes[nd.host] == nd && nd.funnelH.Load() != nil
	cleanup := active && minted && !nd.private
	if active {
		nd.funnelOpening = false
		nd.funnelCleanup = cleanup
		nd.funnelState = FunnelError
		nd.funnelDetail = cause.Error()
		if cleanup {
			nd.funnelH.Store(nil)
			n.funnelReports[nd.host] = FunnelReport{URL: n.funnelURLLocked(nd), State: FunnelError, Detail: cause.Error()}
		}
		n.notifyLocked()
	}
	n.mu.Unlock()
	if cleanup {
		if err := n.Stop(nd.host); err != nil {
			n.mu.Lock()
			report := n.funnelReports[nd.host]
			report.Detail = errors.Join(cause, err).Error()
			n.funnelReports[nd.host] = report
			n.notifyLocked()
			n.mu.Unlock()
		}
	}
}

// StopFunnel closes the internet listener and the AllowFunnel entry that
// ListenFunnel added. The private tailnet listener and the node's tailnet
// membership stay as they were.
func (n *Net) StopFunnel(host string) error {
	// Do not wait for a node that is still joining. setupFunnel takes this
	// lock only after boot, then rechecks the request before opening a listener.
	n.mu.Lock()
	nd := n.nodes[host]
	if nd == nil {
		delete(n.funnelReports, host)
		n.mu.Unlock()
		return nil
	}
	n.mu.Unlock()
	nd.funnelMu.Lock()
	defer nd.funnelMu.Unlock()
	n.mu.Lock()
	if n.nodes[host] != nd || nd.funnelLn == nil && nd.funnelState == "" {
		n.mu.Unlock()
		return nil
	}
	ln := nd.funnelLn
	hs := nd.funnelSrv
	mayHaveAllow := nd.bootOK && (ln != nil || nd.funnelState == FunnelError)
	nd.funnelLn = nil
	nd.funnelSrv = nil
	nd.funnelH.Store(nil)
	nd.funnelOpening = false
	nd.funnelCleanup = false
	delete(n.funnelReports, host)
	n.mu.Unlock()
	// A confirmed close must not leave the internet route in the node config.
	// An entry that is still present is an error, so the caller reports the
	// stop as unconfirmed instead of claiming the route is gone.
	var clearErr error
	if mayHaveAllow {
		clearErr = n.clearAllowFunnel(nd)
	}
	err := errors.Join(closeFunnel(hs, ln), clearErr)
	if err != nil {
		n.setFunnelStopResult(nd, err)
		return err
	}
	if mayHaveAllow {
		if on, err := n.allowFunnelSet(nd); err != nil {
			n.setFunnelStopResult(nd, err)
			return err
		} else if on {
			err := errors.New("tsnet: funnel AllowFunnel entry is still set")
			n.setFunnelStopResult(nd, err)
			return err
		}
	}
	n.setFunnelStopResult(nd, nil)
	return nil
}

func (n *Net) setFunnelStopResult(nd *node, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.nodes[nd.host] != nd {
		return
	}
	if err != nil {
		nd.funnelState = FunnelError
		nd.funnelDetail = "Funnel teardown is unconfirmed: " + err.Error()
	} else {
		nd.funnelState = FunnelUnavailable
		nd.funnelDetail = "Funnel route closed; private tailnet access was not removed"
	}
	n.notifyLocked()
}

// FunnelStatus reports the internet route. Hosts with no Funnel listener
// are unavailable, including hosts that are serving private tailnet HTTP.
func (n *Net) FunnelStatus(host string) FunnelReport {
	n.mu.Lock()
	defer n.mu.Unlock()
	nd, ok := n.nodes[host]
	if !ok || nd.funnelState == "" {
		if report, ok := n.funnelReports[host]; ok {
			return report
		}
		return FunnelReport{State: FunnelUnavailable, Detail: "Funnel is not enabled for this host"}
	}
	report := FunnelReport{URL: nd.funnelURL, State: nd.funnelState, Detail: nd.funnelDetail}
	if report.URL == "" {
		report.URL = n.funnelURLLocked(nd)
	}
	if nd.funnelOpening && (nd.backend == ipn.NeedsLogin.String() || nd.backend == ipn.NeedsMachineAuth.String()) {
		hi := n.hostInfoLocked(nd, time.Now())
		report.State, report.Detail = hi.State, hi.Detail
	}
	return report
}

func (n *Net) beginFunnel(host string, h http.Handler) (nd *node, minted, launch bool, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil, false, false, errors.New("tsnet: network is closed")
	}
	if pending := n.retiring[host]; pending != nil {
		select {
		case <-pending.attempt.done:
			if pending.attempt.err != nil {
				return nil, false, false, fmt.Errorf("tsnet: %s teardown failed; retry the removal before enabling Funnel: %w", host, pending.attempt.err)
			}
			delete(n.retiring, host)
		default:
			return nil, false, false, fmt.Errorf("tsnet: %s teardown is still unconfirmed", host)
		}
	}
	if nd, ok := n.nodes[host]; ok {
		if nd.ephemeral {
			return nil, false, false, fmt.Errorf("tsnet: funnel %s: ephemeral previews stay on the private network", host)
		}
		if nd.err != nil {
			return nil, false, false, fmt.Errorf("tsnet: funnel %s: %w", host, nd.err)
		}
		if nd.funnelCleanup {
			return nil, false, false, fmt.Errorf("tsnet: funnel %s: failed setup is retiring its node", host)
		}
		nd.funnelH.Store(&h)
		delete(n.funnelReports, host)
		if nd.funnelLn == nil && !nd.funnelOpening {
			nd.funnelOpening = true
			nd.funnelState = FunnelStarting
			nd.funnelDetail = "joining the tailnet before Funnel can listen"
			n.notifyLocked()
			return nd, false, true, nil
		}
		return nd, false, false, nil
	}
	_, statErr := os.Stat(filepath.Join(n.cfg.Dir, host, "tailscaled.state"))
	preexisting := statErr == nil || !os.IsNotExist(statErr)
	nd = n.newNode(host, false, false)
	nd.funnelH.Store(&h)
	nd.funnelOpening = true
	nd.funnelState = FunnelStarting
	nd.funnelDetail = "joining the tailnet before Funnel can listen"
	n.nodes[host] = nd
	delete(n.funnelReports, host)
	n.notifyLocked()
	go n.run(nd)
	return nd, !preexisting, true, nil
}

// undoNewAllowFunnel drops an AllowFunnel entry this attempt introduced.
// An entry that was already set belongs to an existing route and is left up.
func (n *Net) undoNewAllowFunnel(nd *node, prior bool, cause error) error {
	if prior {
		return cause
	}
	if err := n.clearAllowFunnel(nd); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (n *Net) funnelHostPort(nd *node) (ipn.HostPort, *local.Client, error) {
	n.mu.Lock()
	domain := nd.dnsName
	srv := nd.srv
	n.mu.Unlock()
	if srv == nil {
		return "", nil, errors.New("tsnet: funnel node is not running")
	}
	if domains := srv.CertDomains(); len(domains) > 0 {
		domain = domains[0]
	}
	if domain == "" {
		return "", nil, errors.New("tsnet: funnel has no HTTPS name")
	}
	lc, err := srv.LocalClient()
	if err != nil {
		return "", nil, err
	}
	return ipn.HostPort(domain + ":443"), lc, nil
}

func (n *Net) allowFunnelSet(nd *node) (bool, error) {
	hp, lc, err := n.funnelHostPort(nd)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sc, err := lc.GetServeConfig(ctx)
	if err != nil || sc == nil {
		return false, err
	}
	return sc.AllowFunnel[hp], nil
}

func (n *Net) clearAllowFunnel(nd *node) error {
	hp, lc, err := n.funnelHostPort(nd)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sc, err := lc.GetServeConfig(ctx)
	if err != nil || sc == nil || !sc.AllowFunnel[hp] {
		return err
	}
	delete(sc.AllowFunnel, hp)
	return lc.SetServeConfig(ctx, sc)
}

func (n *Net) openFunnel(srv *ts.Server, opts []ts.FunnelOption) (net.Listener, error) {
	if n.listenFunnel != nil {
		return n.listenFunnel(srv, opts)
	}
	return srv.ListenFunnel("tcp", ":443", opts...)
}

func (n *Net) publicHandler(nd *node) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k := range r.Header {
			if isIdentityHeader(k) {
				delete(r.Header, k)
			}
		}
		h := nd.funnelH.Load()
		if h == nil || *h == nil {
			http.Error(w, "not published", http.StatusNotFound)
			return
		}
		(*h).ServeHTTP(w, r)
	})
}

func (n *Net) serveFunnel(nd *node, hs *http.Server, ln net.Listener) {
	err := hs.Serve(ln)
	if err == nil || errors.Is(err, net.ErrClosed) || errors.Is(err, http.ErrServerClosed) {
		return
	}
	n.mu.Lock()
	if nd.funnelLn == ln {
		nd.funnelState = FunnelError
		nd.funnelDetail = err.Error()
		n.notifyLocked()
	}
	n.mu.Unlock()
	n.logf("tsnet %s: funnel: %v", nd.host, err)
}

// warmFunnel holds the internet route at starting until Let's Encrypt has
// issued the certificate, matching the private HTTPS path.
func (n *Net) warmFunnel(nd *node) {
	n.mu.Lock()
	domain := nd.dnsName
	srv := nd.srv
	n.mu.Unlock()
	if srv == nil {
		n.setFunnel(nd, FunnelError, "Funnel node is not running")
		return
	}
	if domain == "" && srv != nil {
		if domains := srv.CertDomains(); len(domains) > 0 {
			domain = domains[0]
		}
	}
	if domain == "" {
		n.mu.Lock()
		if nd.funnelState == FunnelStarting {
			nd.funnelState = FunnelError
			nd.funnelDetail = "Funnel has no HTTPS name yet"
			n.notifyLocked()
		}
		n.mu.Unlock()
		return
	}
	lc, err := srv.LocalClient()
	if err != nil {
		n.setFunnel(nd, FunnelError, err.Error())
		return
	}
	fetch := n.warmCert
	if fetch == nil {
		fetch = func(ctx context.Context, d string) error {
			_, _, err := lc.CertPair(ctx, d)
			return err
		}
	}
	wait := certRetryMin
	for {
		ctx, cancel := context.WithTimeout(nd.ctx, certWarmTimeout)
		err := fetch(ctx, domain)
		cancel()
		if nd.ctx.Err() != nil {
			return
		}
		n.mu.Lock()
		stopped := nd.funnelState == FunnelUnavailable || nd.funnelState == ""
		n.mu.Unlock()
		if stopped {
			return
		}
		if err == nil {
			n.setFunnel(nd, FunnelReady, "Funnel is accepting internet connections on port 443; tailnet peers use the private listener")
			return
		}
		n.setFunnel(nd, FunnelStarting, CertPending+"; last attempt: "+err.Error())
		n.logf("tsnet %s: funnel certificate for %s: %v (retrying in %s)", nd.host, domain, err, wait)
		select {
		case <-nd.ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, certRetryMax)
	}
}

func (n *Net) setFunnel(nd *node, state, detail string) {
	n.mu.Lock()
	if nd.funnelState != FunnelUnavailable {
		nd.funnelState = state
		nd.funnelDetail = detail
		n.notifyLocked()
	}
	n.mu.Unlock()
}

func (n *Net) clearFunnel(nd *node, state, detail string) {
	n.mu.Lock()
	ln := nd.funnelLn
	hs := nd.funnelSrv
	nd.funnelLn = nil
	nd.funnelSrv = nil
	nd.funnelState = state
	nd.funnelDetail = detail
	n.notifyLocked()
	n.mu.Unlock()
	if hs != nil || ln != nil {
		_ = closeFunnel(hs, ln)
	}
}

func closeFunnel(hs *http.Server, ln net.Listener) error {
	var errs []error
	if hs != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := hs.Shutdown(ctx)
		cancel()
		if err != nil {
			errs = append(errs, err)
			if cerr := hs.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
				errs = append(errs, cerr)
			}
		}
	}
	if ln != nil {
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (n *Net) funnelURL(nd *node) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if nd.funnelURL != "" {
		return nd.funnelURL
	}
	return n.funnelURLLocked(nd)
}

func (n *Net) funnelURLLocked(nd *node) string {
	name := nd.dnsName
	if name == "" {
		suffix := n.suffix
		if suffix == "" {
			suffix = "<tailnet>.ts.net"
		}
		name = nd.host + "." + suffix
	}
	return "https://" + name
}
