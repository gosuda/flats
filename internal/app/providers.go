package app

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/portal"
	"github.com/gosuda/flats/internal/expose/provider"
	tsnetx "github.com/gosuda/flats/internal/expose/tsnet"
	"github.com/gosuda/flats/internal/expose/zrok"
)

// ProviderInfo is one network provider as the console's settings show it.
// Local is always enabled. Every other provider is off until the operator
// enables it for the host, and a flat uses it only after it is also allowed
// for that flat.
type ProviderInfo struct {
	ID         string          `json:"id"`
	Scope      string          `json:"scope"`                // private or public
	Enabled    bool            `json:"enabled"`              // in network.permitted
	Configured bool            `json:"configured"`           // backend exists in this process
	Locked     string          `json:"locked,omitempty"`     // why the console cannot change it
	Flats      []string        `json:"flats"`                // flats that allow it
	Status     *core.NetStatus `json:"status,omitempty"`     // backend hosts
	Address    string          `json:"address,omitempty"`    // local listener
	AuthKey    bool            `json:"auth_key,omitempty"`   // tailscale: an auth key file is configured
	ConsoleOn  bool            `json:"console_on,omitempty"` // tailscale: the console and private routes use it
	Namespace  string          `json:"namespace,omitempty"`  // zrok: the namespace names are reserved in
}

// legacyNetworkPins names the legacy service flags that fix a provider's
// grant: the next start compares them with network.permitted.
func legacyNetworkPins(o Options) map[provider.ID]string {
	pins := map[provider.ID]string{}
	if o.Set["portal"] {
		pins[provider.Portal] = "the service's --portal flag"
	}
	if o.Set["network"] && o.Network == "tailscale" {
		pins[provider.Tailscale] = "the service's --network flag"
	}
	for _, raw := range o.Permit {
		if id, err := provider.ParseID(raw); err == nil && id != provider.Local {
			pins[id] = "the service's --permit flag"
		}
	}
	return pins
}

// NetworkProviders implements api.ProviderAdmin.
func (h *Host) NetworkProviders(ctx context.Context) any {
	flats := h.flatsAllowing(ctx)
	h.pmu.Lock()
	ts, pn, zn := h.tsNet, h.portalNet, h.zrokNet
	h.pmu.Unlock()
	var grants provider.File
	if h.Providers != nil {
		grants = h.Providers.File()
	}
	out := make([]ProviderInfo, 0, 5)
	for _, id := range []provider.ID{provider.Local, provider.Tailscale, provider.Funnel, provider.Portal, provider.Zrok} {
		p := ProviderInfo{ID: string(id), Scope: "private", Enabled: grants.Allows(id), Flats: flats[id]}
		if p.Flats == nil {
			p.Flats = []string{}
		}
		if by, ok := h.networkPins[id]; ok {
			p.Locked = "Set by " + by + ". Reinstall the service with `flats install` to manage it here."
		}
		switch id {
		case provider.Local:
			p.Configured = true
			p.Address = "http://" + h.Config.Host.LocalAddr
			if h.localNet != nil {
				st := h.localNet.Status()
				p.Status = &st
			}
		case provider.Tailscale, provider.Funnel:
			if id == provider.Funnel {
				p.Scope = "public"
			}
			p.Configured = ts != nil
			if id == provider.Tailscale {
				if ts != nil {
					st := ts.Status()
					p.Status = &st
				}
				p.AuthKey = h.Config.Credentials.TailscaleAuthKeyFile != ""
				p.ConsoleOn = ts != nil && h.Private == core.PrivateNet(ts)
				if p.ConsoleOn && p.Locked == "" {
					p.Locked = "The console and private routes run on Tailscale (network.private_backend)."
				}
			}
		case provider.Portal:
			p.Scope = "public"
			p.Configured = pn != nil
			if pn != nil {
				st := pn.Status()
				p.Status = &st
			}
		case provider.Zrok:
			p.Scope = "public"
			p.Configured = zn != nil
			p.Namespace = h.zrokOptions.Namespace
			if zn != nil {
				st := zn.Status()
				p.Status = &st
			}
		}
		out = append(out, p)
	}
	return out
}

// SetNetworkProvider implements api.ProviderAdmin. It saves network.permitted
// to config.json (ifMatch is the settings ETag, as for a settings save) and
// applies it to this process: turning a provider on starts its backend if
// needed, and nothing is served until a flat allows it. Turning one off is
// refused while a flat still allows it. It returns the new settings ETag.
func (h *Host) SetNetworkProvider(ctx context.Context, raw string, enabled bool, ifMatch string) (string, error) {
	id, err := provider.ParseID(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %v", core.ErrInvalid, err)
	}
	if id == provider.Local {
		return "", fmt.Errorf("%w: local is always enabled", core.ErrInvalid)
	}
	if h.Providers == nil || h.Svc == nil {
		return "", fmt.Errorf("%w: network providers are not available", core.ErrUnavailable)
	}
	if by, ok := h.networkPins[id]; ok {
		return "", fmt.Errorf("%w: %s is set by %s; reinstall the service with `flats install` to manage it here", core.ErrConflict, id, by)
	}
	h.pmu.Lock()
	defer h.pmu.Unlock()
	if enabled {
		if err := h.attachLocked(id); err != nil {
			return "", err
		}
	} else {
		if names := h.flatsAllowing(ctx)[id]; len(names) > 0 {
			return "", fmt.Errorf("%w: %s is allowed on %s; stop allowing it there first", core.ErrProviderInUse, id, strings.Join(names, ", "))
		}
		if id == provider.Tailscale && h.tsNet != nil && h.Private == core.PrivateNet(h.tsNet) {
			return "", fmt.Errorf("%w: the console and private routes run on Tailscale; set network.private_backend to local first", core.ErrConflict)
		}
	}
	permitted := h.Svc.PermittedNetworks()
	if enabled {
		if !slices.Contains(permitted, string(id)) {
			permitted = append(permitted, string(id))
		}
	} else {
		permitted = slices.DeleteFunc(permitted, func(p string) bool { return p == string(id) })
	}
	etag, err := h.Svc.SetPermittedNetworks(permitted, ifMatch)
	if err != nil {
		return "", err
	}
	if _, err := h.Providers.SetGrant(id, enabled); err != nil {
		return "", err
	}
	if id == provider.Zrok {
		// The next start includes zrok in the policy token only while it is
		// granted; match that now, in both directions.
		zc := ""
		if enabled && h.zrokNet != nil {
			zc = zrokConfigurationOf(h.Config.Zrok.Environment, h.zrokNet.Account(), h.zrokOptions.Namespace)
		}
		h.Providers.SetConfiguration(providerConfiguration(h.portalOptions, zc))
	}
	h.logf("provider %s enabled=%t from the console", id, enabled)
	return etag, nil
}

// attachLocked starts the backend id needs, if this process has none yet.
// Constructing a backend does not contact a tailnet or relay. h.pmu is held.
func (h *Host) attachLocked(id provider.ID) error {
	switch id {
	case provider.Tailscale, provider.Funnel:
		if h.tsNet != nil {
			return nil
		}
		key, err := tailscaleAuthKey(h.Config, h.legacy, h.logf)
		if err != nil {
			return err
		}
		n, err := tsnetx.New(tsnetx.Config{Dir: filepath.Join(h.Config.Host.DataDir, "tsnet"), AuthKey: key, Logf: h.logf})
		if err != nil {
			return fmt.Errorf("%w: tailscale: %v", core.ErrUnavailable, err)
		}
		h.tsNet = n
		h.Providers.AttachTailnet(provider.TSNet{Net: n})
	case provider.Portal:
		if h.portalNet != nil {
			return nil
		}
		p, err := portal.New(h.currentPortalOptions())
		if err != nil {
			return fmt.Errorf("%w: portal: %v", core.ErrUnavailable, err)
		}
		h.portalNet = p
		h.Providers.AttachPortal(p)
	case provider.Zrok:
		if h.zrokNet != nil {
			return nil
		}
		z, err := zrok.New(h.zrokOptions)
		if err != nil {
			return fmt.Errorf("%w: %v", core.ErrUnavailable, err)
		}
		h.zrokNet = z
		h.Providers.AttachZrok(z)
	}
	return nil
}

// currentPortalOptions is the Portal configuration as saved now, so Portal
// turned on while the host runs uses the relays set since it started.
func (h *Host) currentPortalOptions() portal.Config {
	o := h.portalOptions
	if h.Svc == nil {
		return o
	}
	st, err := h.Svc.Settings(context.Background())
	if err != nil {
		return o
	}
	o.Relays = nil
	for _, r := range strings.Split(st[core.SetPortalRelays], ",") {
		if r = strings.TrimSpace(r); r != "" {
			o.Relays = append(o.Relays, r)
		}
	}
	o.Discovery = st[core.SetPortalDiscover] != "false"
	if n, err := strconv.ParseInt(st[core.SetPortalMaxRelay], 10, 64); err == nil {
		o.MaxActiveRelays = relayLimit(n)
	}
	return o
}

// flatsAllowing maps each non-local provider to the flats that allow it.
func (h *Host) flatsAllowing(ctx context.Context) map[provider.ID][]string {
	out := map[provider.ID][]string{}
	if h.Svc == nil {
		return out
	}
	fs, err := h.Svc.ListFlats(ctx)
	if err != nil {
		return out
	}
	for _, f := range fs {
		for _, p := range f.Providers {
			id := provider.ID(p)
			if id != provider.Local && !slices.Contains(out[id], f.Slug) {
				out[id] = append(out[id], f.Slug)
			}
		}
	}
	return out
}
