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
// and nothing changes. Provider grants and console domains are reconciled to
// the adopted file the way the next start would set them, except what a
// legacy service flag pins. Every attempt is logged; keys are named, never
// valued.
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
	h.reconcileProviders(ctx, rep.PermittedAfter)
	h.reconcileConsoleDomains(ctx, rep)
	if rep.PreferredProblem != "" {
		h.logf("warning: %s", rep.PreferredProblem)
	}
	return nil
}

// logReload logs a successful reload's outcome by key name.
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

// reconcileProviders applies an adopted network.permitted as the next start
// would: a newly permitted provider gets its host grant and its backend, a
// removed one loses the grant and its routes come down. Unlike the console,
// removing a provider that flats still allow is not refused: the file is the
// operator's explicit instruction, and the flats keep their permission for
// it. A provider a legacy service flag pins keeps its grant.
func (h *Host) reconcileProviders(ctx context.Context, permitted []string) {
	h.pmu.Lock()
	defer h.pmu.Unlock()
	actual := h.Providers.File().Permitted
	for id, by := range h.networkPins {
		if slices.Contains(actual, id) != slices.Contains(permitted, string(id)) {
			h.logf("config reload: network.permitted: %s is set by %s; the file's change to it does not apply while the service passes the flag", id, by)
		}
	}
	for _, raw := range permitted {
		id := provider.ID(raw)
		if h.Providers.HostAllows(id) {
			continue
		}
		if _, pinned := h.networkPins[id]; pinned {
			continue
		}
		h.providerOnReloadLocked(id)
	}
	for _, id := range actual {
		if slices.Contains(permitted, string(id)) {
			continue
		}
		if _, pinned := h.networkPins[id]; pinned {
			continue
		}
		h.providerOffReloadLocked(ctx, id)
	}
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

// providerOffReloadLocked removes id's host grant and takes its routes down,
// as the next start would. h.pmu is held.
func (h *Host) providerOffReloadLocked(ctx context.Context, id provider.ID) {
	if err := h.setGrantLocked(id, false); err != nil {
		h.logf("config reload: %s: remove the host grant: %v; its routes stay up", id, err)
		return
	}
	// No new routes open once the grant is gone; the routes it served come
	// down. A failure is retried at the next reload or start.
	if err := h.Providers.StopProvider(ctx, id); err != nil {
		h.logf("config reload: %s: stop its routes: %v; retried at the next reload or start", id, err)
	}
	if proxyKind(id) && h.proxyNet != nil &&
		!h.Providers.HostAllows(provider.Proxy) && !h.Providers.HostAllows(provider.ProxyPublic) {
		if !h.Providers.DetachProxy() {
			h.logf("config reload: %s: a reverse proxy route is still registered, so its listener stays open; a restart clears it", id)
		} else {
			if err := h.proxyNet.Close(); err != nil {
				h.logf("reverse proxy listener shutdown: %v", err)
			}
			h.proxyNet = nil
		}
	}
	h.providerChangedLocked(id, false, "", "by config reload")
}

// reconcileConsoleDomains applies the adopted console domain lists the way a
// start does: a listed name that is not served starts being served, and a
// node no longer listed is retired, with failures retried at the next reload
// or start.
func (h *Host) reconcileConsoleDomains(ctx context.Context, rep *core.ConfigReload) {
	if slices.Equal(rep.TailscaleHostsBefore, rep.TailscaleHostsAfter) &&
		slices.Equal(rep.NetbirdHostsBefore, rep.NetbirdHostsAfter) {
		return
	}
	if err := h.startConsoleDomains(ctx); err != nil {
		h.logf("config reload: reconcile console domains: %v", err)
	}
}

// reloadOnSIGHUP reloads config.json on each SIGHUP until ctx ends. hup is
// buffered, so signals that arrive while a reload runs coalesce into one
// further reload. A refusal is logged by ReloadConfig. The reload runs on a
// background context: it must finish atomically, since cancelling it
// mid-flight would break the all-or-nothing adoption; Close waits for it.
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
