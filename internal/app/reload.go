package app

import (
	"context"
	"os"
	"slices"
	"strings"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/provider"
)

// ReloadConfig adopts config.json again, as SIGHUP asks: the file is
// validated and becomes the host's current truth, or the reload is refused
// and nothing changes. Provider grants are reconciled to the adopted file by
// the console's rules, and console domains the way the next start would set
// them. Every attempt is logged; keys are named, never valued.
func (h *Host) ReloadConfig(ctx context.Context) error {
	if h.Svc == nil || h.Providers == nil {
		return nil
	}
	h.reloadMu.Lock()
	if h.reloadClosed {
		h.reloadMu.Unlock()
		return nil
	}
	h.reloadWG.Add(1)
	h.reloadMu.Unlock()
	defer h.reloadWG.Done()
	rep, err := h.Svc.ReloadConfig(ctx)
	if err != nil {
		h.logf("config.json reload refused: %v", err)
		return err
	}
	h.logReload(rep)
	// The file is adopted. What is left, the provider and console domain
	// reconciliation, runs on a context Close cancels, so a closing host
	// starts no further provider changes or console nodes for it; the next
	// start does what it did not finish.
	ctx, done := h.reloadContext(ctx)
	defer done()
	h.reconcileProviders(ctx, rep)
	h.reconcileConsoleDomains(ctx)
	if rep.PreferredProblem != "" {
		h.logf("warning: %s", rep.PreferredProblem)
	}
	return nil
}

// reloadContext derives the context of a reload's reconciliation, which
// Close cancels. done releases it.
func (h *Host) reloadContext(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	h.reloadMu.Lock()
	defer h.reloadMu.Unlock()
	if h.reloadClosed {
		cancel()
		return ctx, cancel
	}
	if h.reloadCancels == nil {
		h.reloadCancels = map[*context.CancelFunc]struct{}{}
	}
	key := &cancel
	h.reloadCancels[key] = struct{}{}
	return ctx, func() {
		h.reloadMu.Lock()
		delete(h.reloadCancels, key)
		h.reloadMu.Unlock()
		cancel()
	}
}

func (h *Host) logReload(rep *core.ConfigReload) {
	h.logf("config.json reloaded")
	if len(rep.Applied) > 0 {
		h.logf("config reload: applied now: %s", strings.Join(rep.Applied, ", "))
	}
	if len(rep.Periodic) > 0 {
		h.logf("config reload: applied at their next occasion: %s", strings.Join(rep.Periodic, ", "))
	}
	if len(rep.AwaitRestart) > 0 {
		h.logf("config reload: applying at the next start: %s", strings.Join(rep.AwaitRestart, ", "))
	}
	for _, held := range rep.Held {
		h.logf("config reload: %s is held by %s; the file's value does not change this run", held.Key, held.By)
	}
}

// reconcileProviders applies an adopted network.permitted as the console's
// provider changes do: a removed provider that no flat allows loses its
// grant, and then a newly permitted provider gets its host grant and its
// backend. A removed provider that flats still allow keeps its grant and
// routes until the next start, as the console refuses to turn it off, and so
// does Tailscale while it is the private backend. A reverse proxy kind is
// not turned on while the other kind's grant stays on. A reload never changes a provider a legacy
// service flag pins: it refuses such a file before adopting it.
//
// network.permitted is read again under h.pmu rather than taken from the
// adopted file: a console provider change that ran after the adoption saved
// its own value and set its grant, and the reload must not undo it.
func (h *Host) reconcileProviders(ctx context.Context, rep *core.ConfigReload) {
	h.pmu.Lock()
	defer h.pmu.Unlock()
	permitted := h.Svc.PermittedNetworks()
	actual := h.Providers.File().Permitted
	// Removals first, so a provider whose grant comes off frees what a newly
	// permitted one needs, such as the shared reverse proxy listener.
	var allowing map[provider.ID][]string
	for _, id := range actual {
		if slices.Contains(permitted, string(id)) {
			continue
		}
		if id == provider.Tailscale && h.tsNet != nil && h.Private == core.PrivateNet(h.tsNet) {
			// network.private_backend applies at the next start, which
			// moves private routes off Tailscale and then drops its grant.
			h.logf("config reload: network.permitted: tailscale stays on until the next start, because private routes run on it; network.private_backend changes at a start")
			continue
		}
		if h.reconcileStopped(ctx) {
			return
		}
		if allowing == nil {
			var err error
			if allowing, err = h.flatsAllowingErr(ctx); err != nil {
				h.logf("config reload: network.permitted: %s stays on until the next start, because the flats that allow it cannot be listed: %v", id, err)
				allowing = nil
				continue
			}
		}
		// As in the console's turn-off, a flat allowed the provider after
		// this list was read is not seen here: per-flat permission changes
		// take the flat's lock, not h.pmu.
		if names := allowing[id]; len(names) > 0 {
			// As the console refuses to turn it off: the routes it serves,
			// and any exposure starting now, stay until the next start,
			// which opens none for it.
			h.logf("config reload: network.permitted: %s stays on until the next start, because flats still allow it: %s", id, strings.Join(names, ", "))
			continue
		}
		h.providerOffReloadLocked(id)
	}
	for _, raw := range permitted {
		id := provider.ID(raw)
		if h.Providers.HostAllows(id) {
			continue
		}
		if proxyKind(id) && h.Providers.HostAllows(otherProxyKind(id)) {
			// The two kinds share one listener and are never on together.
			h.logf("config reload: network.permitted: %s is not turned on while %s stays on; a later reload or the next start turns it on once %s is off", id, otherProxyKind(id), otherProxyKind(id))
			continue
		}
		if h.reconcileStopped(ctx) {
			return
		}
		h.providerOnReloadLocked(id)
	}
}

// reconcileStopped reports whether a closing host cancelled the
// reconciliation, which then starts no further change.
func (h *Host) reconcileStopped(ctx context.Context) bool {
	if ctx.Err() == nil {
		return false
	}
	h.logf("config reload: stopped by shutdown before every provider and console domain change was applied; the next start applies the rest")
	return true
}

// providerOnReloadLocked grants id and starts its backend, as the console
// turning it on does. h.pmu is held.
func (h *Host) providerOnReloadLocked(id provider.ID) {
	if proxyKind(id) && h.proxyNet == nil {
		n, err := listenProxy(h.Svc.ProxyConfig().Addr, h.proxyTemplate)
		if err != nil {
			h.logf("config reload: %s: bind the reverse proxy listener: %v", id, err)
			return
		}
		h.proxyNet = n
		h.Providers.AttachProxy(n)
	}
	if err := h.attachLocked(id); err != nil {
		h.logf("config reload: %s: start the backend: %v", id, err)
		return
	}
	if err := h.setGrantLocked(id, true); err != nil {
		h.logf("config reload: %s: record the host grant: %v", id, err)
		return
	}
	h.providerChangedLocked(id, true, "", "by config reload")
}

// providerOffReloadLocked removes the host grant of id, which no flat
// allows, as the console turning it off does. h.pmu is held.
func (h *Host) providerOffReloadLocked(id provider.ID) {
	was := h.Providers.HostAllows(id)
	if err := h.setGrantLocked(id, false); err != nil {
		h.logf("config reload: %s: remove the host grant: %v; retried at the next reload or start", id, err)
		return
	}
	if proxyKind(id) && h.proxyNet != nil &&
		!h.Providers.HostAllows(provider.Proxy) && !h.Providers.HostAllows(provider.ProxyPublic) {
		// A route still registered, such as one a flat allowed after the
		// reload listed the flats, keeps the kind on with its listener, as
		// the console refuses to turn it off then; the other kind is then
		// not turned on beside it.
		if !h.Providers.DetachProxy() {
			h.restoreGrantLocked(id, was)
			h.logf("config reload: network.permitted: %s stays on until a later reload or the next start, because a reverse proxy route is still registered", id)
			return
		}
		if err := h.proxyNet.Close(); err != nil {
			h.logf("reverse proxy listener shutdown: %v", err)
		}
		h.proxyNet = nil
	}
	h.providerChangedLocked(id, false, "", "by config reload")
}

// reconcileConsoleDomains applies the adopted console domain lists the way a
// start does: a listed name that is not served starts being served, and a
// node no longer listed is retired. It runs at every reload, changed lists or
// not, so a failed retirement or serve, or a conflict whose holder has gone,
// of an earlier reload or the start is retried.
func (h *Host) reconcileConsoleDomains(ctx context.Context) {
	if h.reconcileStopped(ctx) {
		return
	}
	if err := h.startConsoleDomains(ctx); err != nil {
		h.logf("config reload: reconcile console domains: %v", err)
	}
}

type sighupAction int

const (
	// sighupReload reloads config.json and keeps serving.
	sighupReload sighupAction = iota
	// sighupStop shuts the host down, as Ctrl+C does.
	sighupStop
	// sighupIgnore leaves an inherited ignore in place.
	sighupIgnore
)

// sighupActionFor picks the SIGHUP action. A SIGHUP ignored when the host
// started (as under nohup) stays ignored. Otherwise a host whose standard
// input is a terminal was started by hand, so SIGHUP means the terminal
// closed and the host stops cleanly; without a terminal (a service manager,
// a container without -t) SIGHUP reloads config.json.
func sighupActionFor(ignored, interactive bool) sighupAction {
	switch {
	case ignored:
		return sighupIgnore
	case interactive:
		return sighupStop
	default:
		return sighupReload
	}
}

// startSIGHUPReloads reloads config.json on each SIGHUP of hup, which is nil
// when SIGHUP does not reload, until ctx ends. Serve calls it once Start has
// returned the host. A SIGHUP that arrived while the host started stays
// queued and reloads now: it may follow an edit that Start did not read.
func (h *Host) startSIGHUPReloads(ctx context.Context, hup <-chan os.Signal) {
	if hup == nil {
		return
	}
	go h.reloadOnSIGHUP(ctx, hup)
}

// reloadOnSIGHUP reloads config.json on each SIGHUP until ctx ends. hup is
// buffered, so signals that arrive while a reload runs coalesce into one
// further reload. A refusal is logged by ReloadConfig. The reload does not
// take ctx: the shutdown signal must not cancel an adoption mid-flight. Close
// cancels the reconciliation and waits for the reload to return.
func (h *Host) reloadOnSIGHUP(ctx context.Context, hup <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			_ = h.ReloadConfig(context.Background())
		}
	}
}
