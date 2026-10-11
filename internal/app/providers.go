package app

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	Scope      string          `json:"scope"`                    // private or public
	Enabled    bool            `json:"enabled"`                  // in network.permitted
	Configured bool            `json:"configured"`               // backend exists in this process
	Locked     string          `json:"locked,omitempty"`         // why the console cannot change it
	Flats      []string        `json:"flats"`                    // flats that allow it
	Status     *core.NetStatus `json:"status,omitempty"`         // backend hosts
	Address    string          `json:"address,omitempty"`        // local listener
	AuthKey    bool            `json:"auth_key,omitempty"`       // tailscale: an auth key file is configured
	ConsoleOn  bool            `json:"console_on,omitempty"`     // tailscale: the console and private routes use it
	Namespace  string          `json:"namespace,omitempty"`      // zrok: the namespace names are reserved in
	Connected  bool            `json:"connected,omitempty"`      // zrok: an account is connected
	Managed    bool            `json:"managed,omitempty"`        // zrok: Flats manages the environment (connect in the console)
	EnvID      string          `json:"environment_id,omitempty"` // zrok: the connected environment's identity
	APIURL     string          `json:"api_endpoint,omitempty"`   // zrok: the zrok service the account belongs to
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
			p.Managed = h.Config.Zrok.Environment == ""
			if a, err := zrok.Describe(h.zrokOptions.Environment); err == nil {
				p.Connected, p.EnvID, p.APIURL = a.Connected, a.EnvironmentID, a.APIEndpoint
			}
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
		h.syncZrokConfigurationLocked()
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

// syncZrokConfigurationLocked sets the zrok part of the policy token to what
// the next start computes: present only while zrok is granted and its backend
// runs. h.pmu is held.
func (h *Host) syncZrokConfigurationLocked() {
	zc := ""
	if h.zrokNet != nil && h.Providers.HostAllows(provider.Zrok) {
		zc = zrokConfigurationOf(h.Config.Zrok.Environment, h.zrokNet.Account(), h.zrokOptions.Namespace)
	}
	h.Providers.SetConfiguration(providerConfiguration(h.portalOptions, zc))
}

// ConnectZrok implements api.ZrokAccountAdmin. Without a connected account it
// enables a zrok environment for the account of token in the data directory;
// with one, it replaces that account. The token is stored only there, as zrok
// stores it, and never logged or returned. Replacing is refused while a flat
// is public through zrok. When zrok is granted, its backend runs on the new
// account afterwards.
func (h *Host) ConnectZrok(ctx context.Context, token string) error {
	if h.Config.Zrok.Environment != "" {
		return fmt.Errorf("%w: zrok.environment points to an environment managed outside Flats; enable it with the zrok CLI", core.ErrConflict)
	}
	h.pmu.Lock()
	defer h.pmu.Unlock()
	dir := h.zrokOptions.Environment
	current, err := zrok.Describe(dir)
	if err != nil {
		return fmt.Errorf("%w: %v", core.ErrUnavailable, err)
	}
	host, _ := os.Hostname()
	description := "flats@" + host
	if !current.Connected {
		if err := zrok.Connect(ctx, dir, token, description); err != nil {
			return zrokAccountError(err)
		}
		h.logf("zrok account connected from the console")
		return h.startZrokLocked(ctx)
	}
	// Replace: the running backend holds the old account; take it down first.
	wasRunning := h.zrokNet != nil
	if wasRunning {
		if !h.Providers.DetachZrok() {
			return fmt.Errorf("%w: a flat is public through zrok; make it private before replacing the zrok account", core.ErrProviderInUse)
		}
		if err := h.zrokNet.Close(); err != nil {
			h.logf("zrok shutdown before replacing the account: %v", err)
		}
		h.zrokNet = nil
	}
	if err := zrok.Replace(ctx, dir, token, description); err != nil {
		if wasRunning {
			if serr := h.startZrokLocked(ctx); serr != nil {
				h.logf("zrok restart on the previous account: %v", serr)
			}
		}
		return zrokAccountError(err)
	}
	h.logf("zrok account replaced from the console")
	return h.startZrokLocked(ctx)
}

// startZrokLocked moves name records made with an earlier token of the same
// account to the connected one, and starts the zrok backend when zrok is
// granted and not running. h.pmu is held.
func (h *Host) startZrokLocked(ctx context.Context) error {
	z, err := zrok.New(h.zrokOptions)
	if err != nil {
		return fmt.Errorf("%w: %v", core.ErrUnavailable, err)
	}
	if kept, err := z.Adopt(ctx); err != nil || len(kept) > 0 {
		h.logf("zrok: name records not moved to the connected account: %v %v", kept, err)
	}
	if h.zrokNet != nil || h.Providers == nil || !h.Providers.HostAllows(provider.Zrok) {
		return z.Close()
	}
	h.zrokNet = z
	h.Providers.AttachZrok(z)
	h.syncZrokConfigurationLocked()
	return nil
}

func zrokAccountError(err error) error {
	switch {
	case errors.Is(err, zrok.ErrConnected):
		return fmt.Errorf("%w: %v", core.ErrConflict, err)
	case errors.Is(err, zrok.ErrRejected):
		return fmt.Errorf("%w: %v", core.ErrInvalid, err)
	case errors.Is(err, zrok.ErrStale):
		return fmt.Errorf("%w: %v; remove the environment in the zrok console", core.ErrConflict, err)
	default:
		return fmt.Errorf("%w: %v", core.ErrUnavailable, err)
	}
}

// DisconnectZrok implements api.ZrokAccountAdmin. It unregisters the
// environment and removes it with the account token. zrok must be off.
func (h *Host) DisconnectZrok(ctx context.Context, force bool) error {
	if h.Config.Zrok.Environment != "" {
		return fmt.Errorf("%w: zrok.environment points to an environment managed outside Flats; disable it with the zrok CLI", core.ErrConflict)
	}
	// Checked under h.pmu, which SetNetworkProvider holds while it grants
	// zrok, so turning zrok on and disconnecting cannot both succeed.
	h.pmu.Lock()
	defer h.pmu.Unlock()
	if h.Providers != nil && h.Providers.HostAllows(provider.Zrok) {
		return fmt.Errorf("%w: turn zrok off before disconnecting its account", core.ErrConflict)
	}
	if h.zrokNet != nil {
		if h.Providers != nil && !h.Providers.DetachZrok() {
			return fmt.Errorf("%w: zrok routes are still registered; stop them first", core.ErrProviderInUse)
		}
		if err := h.zrokNet.Close(); err != nil {
			h.logf("zrok shutdown before disconnect: %v", err)
		}
		h.zrokNet = nil
	}
	if err := zrok.Disconnect(ctx, h.zrokOptions.Environment, force); err != nil {
		return zrokAccountError(err)
	}
	h.logf("zrok account disconnected from the console")
	return nil
}
