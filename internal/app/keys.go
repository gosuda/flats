package app

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/expose/zrok"
	"github.com/gosuda/flats/internal/store"
)

// keyInput is a key given to `flats serve` from outside flats.db: a flag
// naming a file, an environment variable, or the deprecated config key.
type keyInput struct {
	value  string
	source string // core.KeySource*
	from   string // what supplied it, for messages
}

func (k keyInput) given() bool { return k.value != "" }

// keyInputs are the external values of each key, already resolved by
// priority (flag over environment over the deprecated config file).
type keyInputs struct {
	tailscale, zrok, netbird keyInput
}

// keyEnv lists, per key, the variables holding the value and naming a file.
var keyEnv = []struct {
	id, value, file string
}{
	{core.HostKeyTailscale, "FLATS_TAILSCALE_AUTHKEY", "FLATS_TAILSCALE_AUTHKEY_FILE"},
	{core.HostKeyZrok, "FLATS_ZROK_TOKEN", "FLATS_ZROK_TOKEN_FILE"},
	{core.HostKeyNetbird, "FLATS_NETBIRD_SETUP_KEY", "FLATS_NETBIRD_SETUP_KEY_FILE"},
}

// legacyTailscaleEnv are the auth key variables tsnet itself falls back to
// when a node has no key. Legacy mode reads them as the environment input.
var legacyTailscaleEnv = []string{"TS_AUTHKEY", "TS_AUTH_KEY"}

// readKeyFile reads a key from path, trimmed of surrounding white space.
// A relative path (from an environment variable) is taken from the working
// directory; the path is cleaned before use.
func readKeyFile(path, from string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", actionf("%s: %v", from, err)
	}
	// Flats runs on Unix only, where abs already starts with "/"; this is the
	// form static analysis accepts as free of ".." traversal.
	path = filepath.Clean("/" + abs)
	b, err := os.ReadFile(path)
	if err != nil {
		return "", actionf("%s: read the key file: %v", from, err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", actionf("%s: the key file %s is empty", from, path)
	}
	return v, nil
}

// readKeyInputs reads every external key input and removes the key
// variables from the environment, so no child process or embedded client
// sees them. Each input present is read and checked, also one a
// higher-priority input overrides, so a broken one stops the start.
// deprecatedFile is credentials.tailscale_authkey_file ("" when unset).
func readKeyInputs(o Options, legacy bool, deprecatedFile string) (keyInputs, error) {
	var in keyInputs
	slot := map[string]*keyInput{core.HostKeyTailscale: &in.tailscale, core.HostKeyZrok: &in.zrok, core.HostKeyNetbird: &in.netbird}
	consider := func(id string, k keyInput) error {
		if err := core.CheckHostKeyValue(id, k.value); err != nil {
			return actionf("%s: %v", k.from, err)
		}
		if !slot[id].given() {
			*slot[id] = k
		}
		return nil
	}

	// Flags first: they win.
	flags := []struct{ id, name, path string }{
		{core.HostKeyTailscale, "tailscale-authkey-file", o.TailscaleKeyFile},
		{core.HostKeyTailscale, "authkey-file", o.AuthKeyFile},
		{core.HostKeyZrok, "zrok-token-file", o.ZrokTokenFile},
		{core.HostKeyNetbird, "netbird-setup-key-file", o.NetbirdKeyFile},
	}
	for _, f := range flags {
		if !o.Set[f.name] || f.path == "" {
			continue
		}
		v, err := readKeyFile(f.path, "--"+f.name)
		if err != nil {
			return in, err
		}
		if err := consider(f.id, keyInput{v, core.KeySourceFlag, "--" + f.name}); err != nil {
			return in, err
		}
	}

	// Then the environment. Every key variable is removed after reading.
	var errs []error
	for _, e := range keyEnv {
		val, file := os.Getenv(e.value), os.Getenv(e.file)
		os.Unsetenv(e.value)
		os.Unsetenv(e.file)
		switch {
		case val != "" && file != "":
			errs = append(errs, actionf("both %s and %s are set; set only one", e.value, e.file))
		case val != "":
			if err := consider(e.id, keyInput{strings.TrimSpace(val), core.KeySourceEnv, e.value}); err != nil {
				errs = append(errs, err)
			}
		case file != "":
			v, err := readKeyFile(file, e.file)
			if err == nil {
				err = consider(e.id, keyInput{v, core.KeySourceEnv, e.file})
			}
			if err != nil {
				errs = append(errs, err)
			}
		}
	}
	if legacy {
		for _, name := range legacyTailscaleEnv {
			v := os.Getenv(name)
			os.Unsetenv(name)
			if v == "" {
				continue
			}
			if err := consider(core.HostKeyTailscale, keyInput{strings.TrimSpace(v), core.KeySourceEnv, name}); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return in, err
	}

	if deprecatedFile != "" {
		from := "credentials.tailscale_authkey_file"
		v, err := readKeyFile(deprecatedFile, from)
		if err != nil {
			return in, err
		}
		if err := consider(core.HostKeyTailscale, keyInput{v, core.KeySourceConfig, from}); err != nil {
			return in, err
		}
	}
	return in, nil
}

// resolveTailscaleKey applies the seed rule: an external value is stored
// only when flats.db holds no Tailscale key. A stored key always wins; a
// different external value is reported. It returns the key to use.
func resolveTailscaleKey(ctx context.Context, dataDir string, st *store.Store, in keyInput, now time.Time, logf func(string, ...any)) (key string, differs bool, err error) {
	stored, _, err := core.StoredHostKey(ctx, dataDir, st, core.HostKeyTailscale)
	if err != nil {
		return "", false, &ActionError{err}
	}
	switch {
	case stored == "" && in.given():
		if err := core.PutHostKeyValue(ctx, dataDir, st, core.HostKeyTailscale, in.value, in.source, now); err != nil {
			return "", false, err
		}
		logf("stored the Tailscale auth key from %s in flats.db", in.from)
		return in.value, false, nil
	case stored != "" && in.given() && !core.SameKey(stored, in.value):
		logf("warning: %s differs from the Tailscale auth key stored in flats.db; using the stored key. Register a new key in the console (Settings > Keys) or with `flats key set tailscale` to replace it", in.from)
		return stored, true, nil
	}
	return stored, false, nil
}

// refuseTailscaleEnv refuses the TS_* variables tsnet would read on its own
// in config mode: they would bypass flats.db. Legacy mode has consumed the
// auth key ones already.
func refuseTailscaleEnv(legacy bool) error {
	if legacy {
		return nil
	}
	var set []string
	for _, name := range tailscaleEnv {
		if os.Getenv(name) != "" {
			set = append(set, name)
		}
	}
	if len(set) > 0 {
		return actionf("%s set in the environment, but tailscale is permitted in config.json: register the auth key with --tailscale-authkey-file, FLATS_TAILSCALE_AUTHKEY, `flats key set tailscale` or the console, and unset the variables", strings.Join(set, ", "))
	}
	return nil
}

// KeyInfo is one operator key as the console's Keys card shows it. It never
// holds a key value.
type KeyInfo struct {
	ID              string     `json:"id"`
	Set             bool       `json:"set"`               // a key, account or peer is in place
	Waiting         bool       `json:"waiting,omitempty"` // an enrollment waits for the next start
	Source          string     `json:"source,omitempty"`  // console, cli, flag, env, config or unknown
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`
	ExternalDiffers bool       `json:"external_differs,omitempty"`
	ExternalGiven   bool       `json:"external_given,omitempty"` // this run got the key from a flag or environment variable, which seeds it again if it is removed
	LastError       string     `json:"last_error,omitempty"`
	EnvironmentID   string     `json:"environment_id,omitempty"` // zrok
	APIEndpoint     string     `json:"api_endpoint,omitempty"`   // zrok
	DeviceName      string     `json:"device_name,omitempty"`    // netbird
	ManagementURL   string     `json:"management_url,omitempty"` // netbird
	Managed         bool       `json:"managed"`                  // Flats manages it (zrok: no zrok.environment; netbird: built in)
	Unsupported     string     `json:"unsupported,omitempty"`    // netbird: why this build cannot use it
}

// HostKeys implements api.KeyAdmin.
func (h *Host) HostKeys(ctx context.Context) (any, error) { return h.hostKeys(ctx) }

func (h *Host) hostKeys(ctx context.Context) ([]KeyInfo, error) {
	rows, err := h.Store.ListHostKeys(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]store.HostKey{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	h.keyMu.Lock()
	differs, given := h.tsKeyDiffers, h.tsKeyExternal != ""
	enrollErr := maps.Clone(h.enrollErr)
	h.keyMu.Unlock()
	out := make([]KeyInfo, 0, len(core.HostKeyIDs))
	for _, id := range core.HostKeyIDs {
		k := KeyInfo{ID: id, Managed: true}
		row, has := byID[id]
		if has {
			at := row.UpdatedAt
			k.Source, k.UpdatedAt, k.LastError = row.Source, &at, row.LastError
		}
		if k.LastError == "" {
			k.LastError = enrollErr[id]
		}
		switch id {
		case core.HostKeyTailscale:
			k.Set = has && row.HasValue()
			k.ExternalDiffers, k.ExternalGiven = differs, given
		case core.HostKeyZrok:
			k.Managed = h.Config.Zrok.Environment == ""
			a, err := zrok.Describe(h.zrokOptions.Environment)
			if err != nil {
				k.LastError = err.Error()
			}
			k.Set = a.Connected
			k.EnvironmentID, k.APIEndpoint = a.EnvironmentID, a.APIEndpoint
			k.Waiting = !a.Connected && has && row.HasValue()
		case core.HostKeyNetbird:
			k.ManagementURL = h.netbirdOptions.ManagementURL
			if d, err := provider.LinkedNetbird(); err != nil {
				k.Unsupported, k.Managed = err.Error(), false
			} else if a, ok, err := d.LoadAccount(h.netbirdOptions.Dir); err == nil && ok {
				k.Set, k.DeviceName = true, a.DeviceName
				if a.ManagementURL != "" {
					k.ManagementURL = a.ManagementURL
				}
			}
			k.Waiting = !k.Set && has && row.HasValue()
		}
		if k.Set && !has {
			k.Source = "unknown"
		}
		if !k.Set && !k.Waiting && has && !row.HasValue() {
			// A connection record whose account or peer is gone.
			k.Source, k.UpdatedAt = "", nil
		}
		out = append(out, k)
	}
	return out, nil
}

// SetTailscaleKey implements api.KeyAdmin: it stores key and uses it for
// tailnet nodes started from now on.
func (h *Host) SetTailscaleKey(ctx context.Context, key string) error {
	key = strings.TrimSpace(key)
	if err := core.CheckHostKeyValue(core.HostKeyTailscale, key); err != nil {
		return err
	}
	h.keyMu.Lock()
	defer h.keyMu.Unlock()
	if err := core.PutHostKeyValue(ctx, h.Config.Host.DataDir, h.Store, core.HostKeyTailscale, key, core.KeySourceConsole, time.Now()); err != nil {
		return err
	}
	h.tsKeyDiffers = h.tsKeyExternal != "" && !core.SameKey(h.tsKeyExternal, key)
	h.pmu.Lock()
	if h.tsNet != nil {
		h.tsNet.SetAuthKey(key)
	}
	h.pmu.Unlock()
	h.logf("Tailscale auth key registered from the console")
	return nil
}

// DeleteTailscaleKey implements api.KeyAdmin: new tailnet nodes then wait
// for an interactive login.
func (h *Host) DeleteTailscaleKey(ctx context.Context) error {
	h.keyMu.Lock()
	defer h.keyMu.Unlock()
	if err := h.Store.DeleteHostKey(ctx, core.HostKeyTailscale); err != nil {
		return err
	}
	h.tsKeyDiffers = false
	h.pmu.Lock()
	if h.tsNet != nil {
		h.tsNet.SetAuthKey("")
	}
	h.pmu.Unlock()
	h.logf("Tailscale auth key removed from the console")
	return nil
}

// currentTailscaleKey is the stored key, for a backend the console turns on.
func (h *Host) currentTailscaleKey(ctx context.Context) (string, error) {
	v, _, err := core.StoredHostKey(ctx, h.Config.Host.DataDir, h.Store, core.HostKeyTailscale)
	return v, err
}

// recordConnection notes in flats.db how a zrok account or NetBird peer was
// connected, without the key: an enrollment's value is never kept after it.
// It runs after the connection succeeded and must not fail it; a waiting
// value it could not clear is cleared at the next start (clearConnected).
func (h *Host) recordConnection(ctx context.Context, id, source string) {
	if err := h.Store.PutHostKey(context.WithoutCancel(ctx), store.HostKey{ID: id, Source: source, UpdatedAt: time.Now()}); err != nil {
		h.logf("record the %s connection: %v; the waiting key is removed at the next start", id, err)
	}
}

// clearConnected removes the value of a waiting enrollment whose account or
// peer is connected, keeping where it came from.
func (h *Host) clearConnected(ctx context.Context, id string) {
	r, err := h.Store.GetHostKey(ctx, id)
	if err != nil || !r.HasValue() {
		return
	}
	r.Nonce, r.Ciphertext, r.LastError = nil, nil, ""
	if err := h.Store.PutHostKey(ctx, r); err != nil {
		h.logf("remove the used %s key: %v", id, err)
	}
}

// forgetConnection removes the connection record of a disconnected account.
func (h *Host) forgetConnection(ctx context.Context, id string) {
	if err := h.Store.DeleteHostKey(ctx, id); err != nil {
		h.logf("forget the %s connection: %v", id, err)
	}
}

// enrollWaiting connects zrok and NetBird with a key given at start, or a
// waiting one from `flats key set`, when no account or peer is connected.
// It runs after the host started, so a slow or refused enrollment never
// keeps the console down.
func (h *Host) enrollWaiting(ctx context.Context, in keyInputs) {
	for _, e := range []struct {
		id      string
		input   keyInput
		connect func(context.Context, string, string, bool) error
		usable  func() (connected bool, why string)
	}{
		{core.HostKeyZrok, in.zrok, h.connectZrok, h.zrokEnrollable},
		{core.HostKeyNetbird, in.netbird, h.connectNetbird, h.netbirdEnrollable},
	} {
		connected, why := e.usable()
		stored, row, err := core.StoredHostKey(ctx, h.Config.Host.DataDir, h.Store, e.id)
		if err != nil {
			h.logf("read the waiting %s key: %v", e.id, err)
			stored = ""
		}
		if connected && stored != "" {
			// Enrolled already: a waiting value is never kept after it.
			h.logf("a waiting %s key is not needed: %s is already connected", e.id, e.id)
			h.clearConnected(ctx, e.id)
		}
		value, source := e.input.value, e.input.source
		waiting := false
		if !e.input.given() {
			value, source, waiting = stored, row.Source, stored != ""
		}
		if value == "" {
			continue
		}
		switch {
		case connected:
			if e.input.given() {
				h.logf("%s is ignored: %s is already connected; replace or disconnect it in the console", e.input.from, e.id)
			}
			continue
		case why != "":
			h.logf("warning: the %s key is ignored: %s", e.id, why)
			continue
		}
		// A start input never replaces an account or peer connected meanwhile.
		err = e.connect(ctx, value, source, false)
		switch {
		case errors.Is(err, errAlreadyConnected):
			h.logf("%s connected meanwhile; the key from %s is not used", e.id, source)
			if waiting {
				h.clearConnected(ctx, e.id)
			}
		case err == nil:
			h.logf("%s connected with the key from %s", e.id, source)
		case waiting:
			h.logf("connect %s with the waiting key: %v", e.id, err)
			if serr := h.Store.SetHostKeyError(ctx, e.id, err.Error()); serr != nil {
				h.logf("record the %s error: %v", e.id, serr)
			}
		default:
			h.logf("connect %s with the key from %s: %v", e.id, e.input.from, err)
			h.keyMu.Lock()
			h.enrollErr[e.id] = err.Error()
			h.keyMu.Unlock()
		}
	}
}

func (h *Host) zrokEnrollable() (bool, string) {
	if h.Config.Zrok.Environment != "" {
		return false, "zrok.environment names an environment managed with the zrok CLI"
	}
	a, err := zrok.Describe(h.zrokOptions.Environment)
	if err != nil {
		return false, err.Error()
	}
	return a.Connected, ""
}

func (h *Host) netbirdEnrollable() (bool, string) {
	d, err := provider.LinkedNetbird()
	if err != nil {
		return false, err.Error()
	}
	_, ok, err := d.LoadAccount(h.netbirdOptions.Dir)
	if err != nil {
		return false, err.Error()
	}
	return ok, ""
}

// deprecatedAuthKeyFile is credentials.tailscale_authkey_file of the config
// this run starts from.
func deprecatedAuthKeyFile(c *config.Config) string { return c.Credentials.TailscaleAuthKeyFile }
