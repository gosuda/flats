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
	"github.com/gosuda/flats/internal/expose/cfquick"
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
	ID          string          `json:"id"`
	Scope       string          `json:"scope"`                    // private or public
	Enabled     bool            `json:"enabled"`                  // in network.permitted
	Configured  bool            `json:"configured"`               // backend exists in this process
	Locked      string          `json:"locked,omitempty"`         // why the console cannot change it
	Flats       []string        `json:"flats"`                    // flats that allow it
	Status      *core.NetStatus `json:"status,omitempty"`         // backend hosts
	Address     string          `json:"address,omitempty"`        // local and reverse proxy listener
	Template    string          `json:"template,omitempty"`       // proxy: proxy.url_template in effect
	BindError   string          `json:"bind_error,omitempty"`     // proxy: why the listener did not bind at start
	AuthKey     bool            `json:"auth_key,omitempty"`       // tailscale: an auth key is stored
	ConsoleOn   bool            `json:"console_on,omitempty"`     // tailscale: the console or private routes use it
	Namespace   string          `json:"namespace,omitempty"`      // zrok: the namespace names are reserved in
	Connected   bool            `json:"connected,omitempty"`      // zrok: an account is connected
	Managed     bool            `json:"managed,omitempty"`        // zrok: Flats manages the environment (connect in the console)
	EnvID       string          `json:"environment_id,omitempty"` // zrok: the connected environment's identity
	APIURL      string          `json:"api_endpoint,omitempty"`   // zrok: the zrok service the account belongs to
	MgmtURL     string          `json:"management_url,omitempty"` // netbird: the management server
	KeySet      bool            `json:"key_set,omitempty"`        // netbird: a setup key is stored
	Peers       int             `json:"peers,omitempty"`          // netbird: running peers
	Unsupported string          `json:"unsupported,omitempty"`    // netbird: why this build cannot use it
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
	ts, pn, zn, qn, nn, px := h.tsNet, h.portalNet, h.zrokNet, h.quickNet, h.netbirdNet, h.proxyNet
	proxyAddr, bindErr := h.proxyAddressLocked(), h.proxyBindErrorLocked()
	h.pmu.Unlock()
	var grants provider.File
	if h.Providers != nil {
		grants = h.Providers.File()
	}
	out := make([]ProviderInfo, 0, 10)
	for _, id := range []provider.ID{provider.Local, provider.Tailscale, provider.Netbird, provider.Proxy, provider.Funnel, provider.Portal, provider.Zrok, provider.CFQuick, provider.NetbirdExpose, provider.ProxyPublic} {
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
				if k, err := h.Store.GetHostKey(ctx, core.HostKeyTailscale); err == nil && k.HasValue() {
					p.AuthKey = true
				}
				domains := h.consoleHostList()
				private := ts != nil && h.Private == core.PrivateNet(ts)
				p.ConsoleOn = len(domains) > 0 || private
				switch {
				case p.Locked != "":
				case len(domains) > 0:
					p.Locked = "The console runs on Tailscale (Settings > Console domains > Tailnet). Delete its tailnet domains first."
				case private:
					p.Locked = "Private routes run on Tailscale (network.private_backend)."
				}
			}
		case provider.Portal:
			p.Scope = "public"
			p.Configured = pn != nil
			if pn != nil {
				st := pn.Status()
				p.Status = &st
			}
		case provider.Netbird, provider.NetbirdExpose:
			if id == provider.NetbirdExpose {
				p.Scope = "public"
			}
			p.Configured = nn != nil
			p.MgmtURL = h.netbirdOptions.ManagementURL
			p.KeySet = h.currentNetbirdKey() != ""
			if _, err := provider.LinkedNetbird(); err != nil {
				p.Unsupported = err.Error()
			}
			if nn != nil {
				p.Peers = nn.PeerCount()
				if id == provider.Netbird {
					st := nn.PrivateStatus()
					p.Status = &st
				} else {
					st := nn.PublicStatus()
					p.Status = &st
				}
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
		case provider.CFQuick:
			p.Scope = "public"
			p.Configured = qn != nil
			if qn != nil {
				st := qn.Status()
				p.Status = &st
			}
		case provider.Proxy, provider.ProxyPublic:
			if id == provider.ProxyPublic {
				p.Scope = "public"
			}
			// Both kinds share one listener; it belongs to the one turned on.
			p.Configured = px != nil && p.Enabled
			p.Address = "http://" + proxyAddr
			p.Template = h.proxyTemplate().String()
			if p.Configured {
				p.BindError = bindErr
				st := px.Status()
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
	previous := h.Svc.PermittedNetworks()
	permitted := slices.Clone(previous)
	if enabled {
		if !slices.Contains(permitted, string(id)) {
			permitted = append(permitted, string(id))
		}
	} else {
		permitted = slices.DeleteFunc(permitted, func(p string) bool { return p == string(id) })
	}
	// A listener this call binds is closed again if the change is not saved.
	var bound provider.ProxyNet
	if enabled && proxyKind(id) && h.proxyNet == nil {
		// Check the other kind and the template before binding anything.
		if err := h.Svc.CheckPermittedNetworks(permitted); err != nil {
			return "", err
		}
		n, err := listenProxy(h.Svc.ProxyConfig().Addr, h.proxyTemplate)
		if err != nil {
			return "", fmt.Errorf("%w: %v", core.ErrConflict, err)
		}
		bound, h.proxyNet = n, n
		h.Providers.AttachProxy(n)
	}
	if enabled {
		if err := h.attachLocked(id); err != nil {
			return "", err
		}
	} else {
		if names := h.flatsAllowing(ctx)[id]; len(names) > 0 {
			return "", fmt.Errorf("%w: %s is allowed on %s; stop allowing it there first", core.ErrProviderInUse, id, strings.Join(names, ", "))
		}
		if id == provider.Tailscale && len(h.consoleHostList()) > 0 {
			return "", fmt.Errorf("%w: the console runs on Tailscale; delete its tailnet domains first (Settings > Console domains)", core.ErrConflict)
		}
		if id == provider.Netbird && len(h.consoleHostsOf(providerNetbird)) > 0 {
			return "", fmt.Errorf("%w: the console runs on NetBird; delete its NetBird domains first (Settings > Console domains)", core.ErrConflict)
		}
		if id == provider.Tailscale && h.tsNet != nil && h.Private == core.PrivateNet(h.tsNet) {
			return "", fmt.Errorf("%w: the console and private routes run on Tailscale; set network.private_backend to local first", core.ErrConflict)
		}
	}
	if enabled {
		etag, err := h.Svc.SetPermittedNetworks(permitted, ifMatch)
		if err != nil {
			if bound != nil {
				h.closeProxyLocked()
			}
			return "", err
		}
		if err := h.setGrantLocked(id, true); err != nil {
			// Undo the save, so a refused change saves nothing.
			if _, rerr := h.Svc.SetPermittedNetworks(previous, etag); rerr != nil {
				err = fmt.Errorf("%w; restoring network.permitted also failed: %v", err, rerr)
			}
			if bound != nil {
				h.closeProxyLocked()
			}
			return "", err
		}
		return h.providerChangedLocked(id, enabled, etag, "from the console"), nil
	}
	// Turning off: stop new routes and release the proxy backend before
	// saving, so a refusal on the way saves nothing.
	was := h.Providers.HostAllows(id)
	if err := h.setGrantLocked(id, false); err != nil {
		return "", err
	}
	detached := false
	if proxyKind(id) && h.proxyNet != nil {
		// Revoking every flat's grant stopped every proxy route, so a route
		// left here is a defect; keep its listener rather than close it.
		if !h.Providers.DetachProxy() {
			h.restoreGrantLocked(id, was)
			return "", fmt.Errorf("a reverse proxy route is still registered, so %s stays on and its listener open; restart Flats to clear it", id)
		}
		detached = true
	}
	etag, err := h.Svc.SetPermittedNetworks(permitted, ifMatch)
	if err != nil {
		if detached {
			h.Providers.AttachProxy(h.proxyNet)
		}
		h.restoreGrantLocked(id, was)
		return "", err
	}
	if detached {
		if err := h.proxyNet.Close(); err != nil {
			h.logf("reverse proxy listener shutdown: %v", err)
		}
		h.proxyNet = nil
	}
	return h.providerChangedLocked(id, enabled, etag, "from the console"), nil
}

// providerChangedLocked finishes a saved provider change: it refreshes the
// backend parts of the approval policy and logs the change, naming where it
// came from. It returns etag. h.pmu is held.
func (h *Host) providerChangedLocked(id provider.ID, enabled bool, etag, source string) string {
	if id == provider.Zrok {
		h.syncZrokConfigurationLocked()
	}
	if id == provider.Netbird || id == provider.NetbirdExpose {
		h.syncNetbirdConfigurationLocked()
	}
	h.logf("provider %s enabled=%t %s", id, enabled, source)
	return etag
}

// setGrantLocked records id's host grant in the provider manager. h.pmu is
// held.
func (h *Host) setGrantLocked(id provider.ID, permitted bool) error {
	if h.testGrantFailure != nil {
		if err := h.testGrantFailure(id, permitted); err != nil {
			return err
		}
	}
	_, err := h.Providers.SetGrant(id, permitted)
	return err
}

// restoreGrantLocked puts back the host grant of id that a refused turn-off
// removed. h.pmu is held.
func (h *Host) restoreGrantLocked(id provider.ID, permitted bool) {
	if _, err := h.Providers.SetGrant(id, permitted); err != nil {
		h.logf("restore the %s grant after a refused change: %v", id, err)
	}
}

// closeProxyLocked detaches and closes the reverse proxy listener, freeing
// its address. h.pmu is held.
func (h *Host) closeProxyLocked() {
	h.Providers.DetachProxy()
	if err := h.proxyNet.Close(); err != nil {
		h.logf("reverse proxy listener shutdown: %v", err)
	}
	h.proxyNet = nil
}

// attachLocked starts the backend id needs, if this process has none yet.
// Constructing a backend does not contact a tailnet or relay. h.pmu is held.
func (h *Host) attachLocked(id provider.ID) error {
	switch id {
	case provider.Tailscale, provider.Funnel:
		if h.tsNet != nil {
			return nil
		}
		if err := refuseTailscaleEnv(h.legacy); err != nil {
			return err
		}
		key, err := h.currentTailscaleKey(context.Background())
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
	case provider.Netbird, provider.NetbirdExpose:
		if h.netbirdNet != nil {
			return nil
		}
		nn, err := newNetbird(h.netbirdOptions)
		if err != nil {
			return fmt.Errorf("%w: %v", core.ErrUnavailable, err)
		}
		h.netbirdNet = nn
		h.Providers.AttachNetbird(nn)
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
	case provider.CFQuick:
		if h.quickNet != nil {
			return nil
		}
		q, err := cfquick.New(cfquickConfig(h.Config.Host.DataDir, h.logf))
		if err != nil {
			return fmt.Errorf("%w: cloudflare-quick: %v", core.ErrUnavailable, err)
		}
		h.quickNet = q
		h.Providers.AttachCFQuick(q)
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
	h.Providers.SetConfiguration(providerConfiguration(h.portalOptions, zc, h.netbirdConfigurationLocked()))
}

// netbirdConfigurationLocked is NetBird's part of the policy token as the next
// start computes it: the management server and a fingerprint of the setup
// key while a NetBird provider is granted. h.pmu is held.
func (h *Host) netbirdConfigurationLocked() string {
	if h.Providers.HostAllows(provider.Netbird) || h.Providers.HostAllows(provider.NetbirdExpose) {
		return netbirdConfigurationOf(h.Config.Netbird.ManagementURL, netbirdKeyFingerprint(h.currentNetbirdKey()))
	}
	return ""
}

// syncNetbirdConfigurationLocked refreshes the whole provider configuration
// token, as syncZrokConfigurationLocked does, after NetBird changed. h.pmu is held.
func (h *Host) syncNetbirdConfigurationLocked() { h.syncZrokConfigurationLocked() }

// ConnectZrok implements api.ZrokAccountAdmin. Without a connected account it
// enables a zrok environment for the account of token in the data directory;
// with one, it replaces that account. The token is stored only there, as zrok
// stores it, and never logged or returned. Replacing is refused while a flat
// is public through zrok. When zrok is granted, its backend runs on the new
// account afterwards.
func (h *Host) ConnectZrok(ctx context.Context, token string) error {
	return h.connectZrok(ctx, token, core.KeySourceConsole, true)
}

// errAlreadyConnected reports that an enrollment found an account or peer
// connected meanwhile; a start input never replaces one.
var errAlreadyConnected = errors.New("already connected")

// connectZrok connects the zrok account, or replaces a connected one when
// replace is set (console actions only), and records where the token came
// from.
func (h *Host) connectZrok(ctx context.Context, token, source string, replace bool) error {
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
		h.logf("zrok account connected (token from %s)", source)
		h.recordConnection(ctx, core.HostKeyZrok, source)
		return h.startZrokLocked(ctx)
	}
	if !replace {
		return errAlreadyConnected
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
	h.logf("zrok account replaced (token from %s)", source)
	h.recordConnection(ctx, core.HostKeyZrok, source)
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
	h.forgetConnection(ctx, core.HostKeyZrok)
	return nil
}

// newNetbird starts the NetBird backend of the linked driver. A binary
// without one reports provider.ErrNetbirdNotBuilt.
func newNetbird(cfg provider.NetbirdConfig) (provider.NetbirdNet, error) {
	d, err := provider.LinkedNetbird()
	if err != nil {
		return nil, err
	}
	return d.New(cfg)
}
