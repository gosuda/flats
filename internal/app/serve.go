// Package app wires every Flats module into the `flats serve` process.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"maps"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gosuda/flats/internal/api"
	docsapp "github.com/gosuda/flats/internal/apps/docs"
	"github.com/gosuda/flats/internal/buildinfo"
	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/console"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/cfquick"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/expose/portal"
	"github.com/gosuda/flats/internal/expose/provider"
	tsnetx "github.com/gosuda/flats/internal/expose/tsnet"
	"github.com/gosuda/flats/internal/expose/zrok"
	"github.com/gosuda/flats/internal/forkwatch"
	"github.com/gosuda/flats/internal/mcpx"
	"github.com/gosuda/flats/internal/metrics"
	"github.com/gosuda/flats/internal/runtime"
	"github.com/gosuda/flats/internal/store"
)

// Options configure `flats serve`.
//
// With ConfigPath set the host runs in config mode: config.json is the only
// source of host configuration, and only the four host keys may be changed
// for one run (--listen, --local-addr, --console-host, --runtime); the key
// file flags seed flats.db. Without
// it the host runs in legacy mode from <DataDir>/config.json: the first run
// creates that file from the flags (migrating a legacy data directory), and
// later runs refuse flags that differ from it.
type Options struct {
	ConfigPath  string
	DataDir     string // legacy mode: the directory holding config.json
	Listen      string // loopback management address
	Network     string // tailscale | local; legacy private path
	LocalAddr   string // local network address
	AuthKeyFile string // legacy alias of TailscaleKeyFile
	// Key files whose keys are stored in flats.db when it holds none yet
	// (zrok and NetBird: used when no account or peer is connected).
	TailscaleKeyFile string
	ZrokTokenFile    string
	NetbirdKeyFile   string
	ConsoleHost      string
	Portal           bool
	Permit           []string // explicit host grants; does not publish a flat
	Relays           []string
	Runtime          bool
	// OperatorCredentialStdin and OperatorCredentialFile are accepted so
	// existing services keep starting; the operator credential is no longer
	// used and both are ignored with a warning.
	OperatorCredentialStdin bool
	OperatorCredentialFile  string
	// Set names the flags given on the command line. Only these flags take
	// effect: a field whose flag is not in Set is ignored.
	Set map[string]bool
	// Overrides are embedding-only per-run values of the host.* keys that
	// may be overridden (see config.Document.Effective), in both modes.
	// They are never stored or compared with the config.
	Overrides map[string]string
}

// DefaultDataDir returns $FLATS_DATA or ~/Library/Application Support/Flats,
// as an absolute path.
func DefaultDataDir() string {
	d := os.Getenv("FLATS_DATA")
	if d == "" {
		d = ".flats"
		if c, err := os.UserConfigDir(); err == nil {
			d = filepath.Join(c, "Flats")
		}
	}
	if abs, err := filepath.Abs(d); err == nil {
		return abs
	}
	return d
}

// ParseServeFlags parses `flats serve` arguments.
func ParseServeFlags(args []string) (Options, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	o := Options{}
	var relays, permit string
	fs.StringVar(&o.ConfigPath, "config", "", "absolute path of config.json (env FLATS_CONFIG); without it, legacy mode uses <data>/config.json")
	fs.StringVar(&o.DataDir, "data", DefaultDataDir(), "legacy mode: data directory holding config.json")
	fs.StringVar(&o.Listen, "listen", "127.0.0.1:7878", "loopback address for the CLI, local agents and the console")
	fs.StringVar(&o.Network, "network", "local", "legacy mode: private path, tailscale or local. tailscale records a host grant and preserves Private Tailscale for pre-lifecycle flats; does not enable Funnel")
	fs.StringVar(&o.LocalAddr, "local-addr", "127.0.0.1:7879", "address of the local network")
	fs.StringVar(&o.AuthKeyFile, "authkey-file", "", "legacy alias of --tailscale-authkey-file")
	fs.StringVar(&o.TailscaleKeyFile, "tailscale-authkey-file", "", "file holding a reusable Tailscale auth key; stored in flats.db when none is stored yet (also FLATS_TAILSCALE_AUTHKEY or FLATS_TAILSCALE_AUTHKEY_FILE)")
	fs.StringVar(&o.ZrokTokenFile, "zrok-token-file", "", "file holding a zrok account token, used when no zrok account is connected (also FLATS_ZROK_TOKEN or FLATS_ZROK_TOKEN_FILE)")
	fs.StringVar(&o.NetbirdKeyFile, "netbird-setup-key-file", "", "file holding the NetBird setup key, stored in flats.db when none is stored yet (also FLATS_NETBIRD_SETUP_KEY or FLATS_NETBIRD_SETUP_KEY_FILE)")
	fs.StringVar(&o.ConsoleHost, "console-host", "flats", "tailnet host name of the console for this run (replaces tailscale.console_hosts)")
	fs.BoolVar(&o.Portal, "portal", false, "legacy mode: grant Portal and attach it as the legacy public network")
	fs.StringVar(&permit, "permit", "", "legacy mode: comma-separated host grants: tailscale, tailscale-funnel, portal, zrok, cloudflare-quick. Local needs no grant. Recorded in config.json and does not publish a flat")
	fs.StringVar(&relays, "relays", "", "legacy mode: comma-separated Portal relays (default: Portal CLI default discovery)")
	fs.BoolVar(&o.Runtime, "runtime", true, "enable server flats (wazero runtime)")
	fs.BoolVar(&o.OperatorCredentialStdin, "operator-credential-stdin", false, "ignored: the operator credential is no longer used")
	fs.StringVar(&o.OperatorCredentialFile, "operator-credential-file", "", "ignored: the operator credential is no longer used")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	o.Set = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { o.Set[f.Name] = true })
	if o.Set["config"] {
		// Like --data, a relative path is resolved against the working
		// directory once, so later chdirs cannot change the file.
		abs, err := filepath.Abs(o.ConfigPath)
		if err != nil {
			return o, fmt.Errorf("--config: %w", err)
		}
		o.ConfigPath = abs
	}
	for _, path := range []*string{&o.OperatorCredentialFile, &o.AuthKeyFile, &o.TailscaleKeyFile, &o.ZrokTokenFile, &o.NetbirdKeyFile} {
		if *path != "" {
			abs, err := filepath.Abs(*path)
			if err != nil {
				return o, err
			}
			*path = abs
		}
	}
	if permit != "" {
		for _, part := range strings.Split(permit, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if _, err := provider.ParseID(part); err != nil {
				return o, err
			}
			o.Permit = append(o.Permit, part)
		}
	}
	if relays != "" {
		for _, r := range strings.Split(relays, ",") {
			if r = strings.TrimSpace(r); r != "" {
				o.Relays = append(o.Relays, r)
			}
		}
	}
	if o.Network != "tailscale" && o.Network != "local" {
		return o, fmt.Errorf("--network must be tailscale or local")
	}
	// Server-flat workers run with "/" as their working directory, so a
	// relative data directory would point somewhere else for them.
	abs, err := filepath.Abs(o.DataDir)
	if err != nil {
		return o, fmt.Errorf("--data: %w", err)
	}
	o.DataDir = abs
	return o, nil
}

// Serve runs `flats serve` until SIGINT/SIGTERM.
func Serve(args []string) error {
	o, err := ParseServeFlags(args)
	if err != nil {
		return err
	}
	if o.ConfigPath == "" {
		if o.ConfigPath = os.Getenv("FLATS_CONFIG"); o.ConfigPath != "" && !filepath.IsAbs(o.ConfigPath) {
			return actionf("FLATS_CONFIG must be an absolute path, not %q", o.ConfigPath)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	h, err := Start(ctx, o)
	if err != nil {
		return err
	}
	consoles := h.ConsoleURLs()
	if len(consoles) == 0 {
		consoles = []string{h.ConsoleURL()}
	}
	log.Printf("flats %s serving; console %s (loopback %s)", buildinfo.Get().Version, strings.Join(consoles, ", "), h.console)
	<-ctx.Done()
	log.Printf("shutting down")
	return h.Close()
}

// Host is a running Flats process.
type Host struct {
	Opts       Options
	Config     *config.Config // effective configuration of this run
	ConfigPath string
	Svc        *core.Service
	Store      *store.Store
	Private    core.PrivateNet
	Public     core.PublicNet
	Providers  *provider.Manager
	Mux        http.Handler
	srv        *http.Server
	ln         net.Listener
	localNet   *local.Net
	pmu        sync.Mutex // guards tsNet, portalNet, zrokNet and quickNet, which the console can attach
	tsNet      *tsnetx.Net

	// metrics serves /metrics and counts management requests; nil in hosts
	// built without Start.
	metrics *metrics.Metrics

	// testTailnet replaces tsNet for console nodes in tests.
	testTailnet core.PrivateNet
	// testNetbirdConsole replaces netbirdNet for console peers in tests.
	testNetbirdConsole netbirdConsoleNet
	portalNet          *portal.Net
	zrokNet            *zrok.Net
	quickNet           *cfquick.Net
	netbirdNet         provider.NetbirdNet
	workers            *runtime.Manager // nil without server flats
	// For providers the console turns on while the host runs.
	logf           func(string, ...any)
	legacy         bool
	portalOptions  portal.Config
	zrokOptions    zrok.Config
	netbirdOptions provider.NetbirdConfig
	networkPins    map[provider.ID]string // provider -> service flag that fixes it
	console        string                 // the loopback console address
	// Console domains: cdMu serializes adding and deleting them; cdNamesMu
	// guards cd.
	cdMu      sync.Mutex
	cdNamesMu sync.Mutex
	cd        consoleState
	// Operator keys: keyMu guards the Tailscale key state the console shows.
	keyMu         sync.Mutex
	tsKeyExternal string // the flag or environment Tailscale key of this run
	tsKeyDiffers  bool   // tsKeyExternal differs from the stored key
	// nbMu guards the NetBird key state below. It is a leaf lock: nothing
	// else is acquired while it is held, so it nests under pmu and keyMu.
	nbMu          sync.Mutex
	nbKey         string            // the NetBird setup key in use
	nbKeyExternal string            // the flag or environment NetBird key of this run
	nbKeyDiffers  bool              // nbKeyExternal differs from the stored key
	enrollErr     map[string]string // zrok/netbird enrollment errors from start inputs
	// Background work (enrollment, node logouts) stops with the host.
	bgMu      sync.Mutex // guards bgClosed against new background work
	bgClosed  bool
	bgCtx     context.Context
	bgCancel  context.CancelFunc
	bg        sync.WaitGroup
	dataLock  *os.File
	closeOnce sync.Once
	closeErr  error
}

// background runs fn until it returns; Close cancels its context and
// waits for it before closing the store it may use.
func (h *Host) background(fn func(context.Context)) {
	h.bgMu.Lock()
	defer h.bgMu.Unlock()
	if h.bgClosed {
		return
	}
	if h.bgCtx == nil {
		h.bgCtx, h.bgCancel = context.WithCancel(context.Background())
	}
	h.bg.Add(1)
	go func() {
		defer h.bg.Done()
		fn(h.bgCtx)
	}()
}

// Addr is the management address actually bound.
func (h *Host) Addr() string { return h.ln.Addr().String() }

// configModeFlags are the serve flags config mode accepts. The host flags
// override config.json for this run only.
var configModeFlags = map[string]string{
	"config": "", "operator-credential-stdin": "",
	"listen": "host.console_addr", "local-addr": "host.local_addr",
	"console-host": "tailscale.console_hosts", "runtime": "host.server_runtime",
	"tailscale-authkey-file": "", "zrok-token-file": "", "netbird-setup-key-file": "",
}

// setup turns the options into the startup inputs of their mode.
func (o Options) setup(logf func(string, ...any)) (setup, error) {
	s := setup{logf: logf, flags: o, overrides: maps.Clone(o.Overrides)}
	if s.overrides == nil {
		s.overrides = map[string]string{}
	}
	if o.ConfigPath == "" {
		s.legacy, s.dataDir = true, o.DataDir
		s.configPath = filepath.Join(o.DataDir, "config.json")
		return s, nil
	}
	s.configPath = o.ConfigPath
	var rejected []string
	for _, name := range slices.Sorted(maps.Keys(o.Set)) {
		key, ok := configModeFlags[name]
		switch {
		case !ok:
			rejected = append(rejected, "--"+name)
		case key != "":
			for _, hf := range hostFlags {
				if hf.flag == name {
					s.overrides[key] = hf.value(o)
				}
			}
		}
	}
	if len(rejected) > 0 {
		return s, actionf("%s cannot be used with --config: config.json is the only source of host configuration; change it with `flats config set` while the host is stopped", strings.Join(rejected, ", "))
	}
	return s, nil
}

// tailscaleEnv are the variables tsnet reads when it has no auth key.
var tailscaleEnv = []string{"TS_AUTHKEY", "TS_AUTH_KEY", "TS_CLIENT_SECRET", "TS_CLIENT_ID", "TS_ID_TOKEN", "TS_CONTROL_URL"}

// Start builds and starts every component.
func Start(ctx context.Context, o Options) (*Host, error) {
	logf := log.Printf
	s, err := o.setup(logf)
	if err != nil {
		return nil, err
	}
	p, err := prepare(ctx, s)
	if err != nil {
		return nil, err
	}
	cfg := p.cfg
	h := &Host{Opts: o, Config: cfg, ConfigPath: p.path, Store: p.st, dataLock: p.lock, logf: logf, legacy: s.legacy,
		enrollErr: map[string]string{}, cd: newConsoleState()}
	h.bgCtx, h.bgCancel = context.WithCancel(context.Background())
	ok := false
	defer func() {
		if !ok {
			if err := h.Close(); err != nil {
				log.Printf("startup cleanup: %v", err)
			}
		}
	}()
	for _, n := range p.notes {
		logf("%s", n)
	}
	if p.flagsChecked {
		logf("warning: flats serve runs without --config; its flags match %s. Reinstall the service with `flats install --config %s` so it runs from the config file alone", p.path, p.path)
	}
	dataDir := cfg.Host.DataDir
	st := p.st

	// The environment, the secret key and the credential files are checked
	// before any network or worker starts.
	permits := func(id provider.ID) bool { return slices.Contains(cfg.Network.Permitted, string(id)) }
	if permits(provider.Tailscale) || permits(provider.Funnel) {
		if err := refuseTailscaleEnv(s.legacy); err != nil {
			return nil, err
		}
	}
	if err := core.CheckSecretKey(ctx, dataDir, st); err != nil {
		if errors.Is(err, core.ErrSecretKeyMissing) {
			return nil, &ActionError{err}
		}
		return nil, err
	}
	// Keys given to this run seed flats.db; the stored Tailscale key wins.
	keys, err := readKeyInputs(o, s.legacy, deprecatedAuthKeyFile(cfg))
	if err != nil {
		return nil, err
	}
	authKey, differs, err := resolveTailscaleKey(ctx, dataDir, st, keys.tailscale, time.Now(), logf)
	if err != nil {
		return nil, err
	}
	if keys.tailscale.source != core.KeySourceConfig {
		h.tsKeyExternal = keys.tailscale.value
	}
	h.tsKeyDiffers = differs
	if cfg.Credentials.OperatorFile != "" || o.OperatorCredentialStdin || o.OperatorCredentialFile != "" {
		logf("warning: the operator credential is no longer used; ignoring credentials.operator_file and --operator-credential-*. Console decisions rely on the console's same-origin checks")
	}
	// A file edit can leave the preferred console naming an unlisted domain:
	// it is ignored, never a reason not to start.
	if p := cfg.PreferredHostProblem(); p != "" {
		logf("warning: %s", p)
	}

	forkwatch.Start(ctx, logf)
	loop, err := local.Listen(cfg.Host.LocalAddr)
	if err != nil {
		return nil, fmt.Errorf("local network: %w", err)
	}
	h.localNet = loop
	if permits(provider.Tailscale) || permits(provider.Funnel) {
		n, err := tsnetx.New(tsnetx.Config{Dir: filepath.Join(dataDir, "tsnet"), AuthKey: authKey, Logf: logf})
		if err != nil {
			return nil, fmt.Errorf("tailscale: %w", err)
		}
		h.tsNet = n
	}
	portalOptions := portal.Config{Dir: filepath.Join(dataDir, "portal"), Relays: cfg.Portal.Relays,
		Discovery: cfg.Portal.Discovery, MaxActiveRelays: relayLimit(cfg.Portal.MaxActiveRelays), Logf: logf}
	h.portalOptions = portalOptions
	if permits(provider.Portal) {
		pn, err := portal.New(portalOptions)
		if err != nil {
			return nil, fmt.Errorf("portal: %w", err)
		}
		h.portalNet = pn
	}
	zrokOptions := zrok.Config{Dir: filepath.Join(dataDir, "zrok"), Environment: zrokEnvironment(cfg), Namespace: cfg.Zrok.Namespace,
		Instance: cfg.Host.InstanceID, Logf: logf}
	h.zrokOptions = zrokOptions
	if permits(provider.Zrok) {
		// A missing account must not keep the console down: the operator
		// connects one there. zrok stays unconfigured until then.
		if zn, err := zrok.New(zrokOptions); err != nil {
			logf("warning: zrok is permitted but not started: %v", err)
		} else {
			h.zrokNet = zn
		}
	}
	if permits(provider.CFQuick) {
		qn, err := cfquick.New(cfquickConfig(dataDir, logf))
		if err != nil {
			return nil, fmt.Errorf("cloudflare-quick: %w", err)
		}
		h.quickNet = qn
	}
	h.netbirdOptions = provider.NetbirdConfig{Dir: filepath.Join(dataDir, "netbird"), ManagementURL: cfg.Netbird.ManagementURL,
		SetupKey: h.currentNetbirdKey, Instance: cfg.Host.InstanceID, Logf: logf}
	if err := h.prepareNetbird(ctx, dataDir, st, keys.netbird, logf); err != nil {
		return nil, err
	}
	if permits(provider.Netbird) || permits(provider.NetbirdExpose) {
		// Without a setup key routes report it; the console stays up.
		if nn, err := newNetbird(h.netbirdOptions); err != nil {
			logf("warning: netbird is permitted but not started: %v", err)
		} else {
			h.netbirdNet = nn
		}
	}
	var tail provider.Tailnet
	if h.tsNet != nil {
		tail = provider.TSNet{Net: h.tsNet}
	}
	grants := &provider.File{PrivateBackend: storedPrivateBackend(cfg)}
	for _, id := range cfg.Network.Permitted {
		grants.Permitted = append(grants.Permitted, provider.ID(id))
	}
	backends := provider.Options{Local: loop, Tailscale: tail, TailscaleStateDir: filepath.Join(dataDir, "tsnet"), Configuration: providerConfiguration(portalOptions, zrokConfiguration(cfg, h.zrokNet), netbirdConfiguration(cfg, h.currentNetbirdKey())), Grants: grants, Permission: func(ctx context.Context, slug string, id provider.ID) (bool, error) {
		return st.ProviderPermitted(ctx, slug, string(id))
	}}
	if h.portalNet != nil {
		backends.Portal = h.portalNet
	}
	if h.zrokNet != nil {
		backends.Zrok = h.zrokNet
	}
	if h.quickNet != nil {
		backends.CFQuick = h.quickNet
	}
	if h.netbirdNet != nil {
		backends.Netbird = h.netbirdNet
	}
	mgr, err := provider.New(dataDir, backends)
	if err != nil {
		return nil, err
	}
	h.Providers = mgr
	useTailscale := cfg.Network.PrivateBackend == "tailscale"
	if useTailscale {
		if h.tsNet == nil {
			return nil, errors.New("tailscale is selected but not permitted")
		}
		h.Private = h.tsNet
	} else {
		h.Private = loop
	}
	if h.portalNet != nil {
		h.Public = h.portalNet
	}
	var rt core.Runtime
	if cfg.Host.ServerRuntime {
		h.workers = &runtime.Manager{DataDir: dataDir, Logf: logf}
		rt = h.workers
	}
	// Settings are saved to config.json against the hash this process last
	// read or wrote; SettingsSource keeps that hash as the settings ETag.
	mode := "config"
	if s.legacy {
		mode = "legacy"
	}
	settings, err := core.NewSettingsSource(p.doc, &core.SettingsFile{Mode: mode, Hash: p.hash, Overrides: s.overrides,
		Save: config.NewWriter(p.path).Save, DiskHash: func() (string, error) { return configHash(p.path) }})
	if err != nil {
		return nil, err
	}
	// A legacy service started with --relays compares it with the config on
	// every start, so the console must not change the relays under it.
	if r, _ := flagRelays(o); s.legacy && len(r) > 0 {
		settings.Pin(core.SetPortalRelays, "the service's --relays flag")
	}
	// Likewise for network.permitted: the console must not turn off a
	// provider a legacy service's flags grant, or change one its --portal sets.
	if s.legacy {
		h.networkPins = legacyNetworkPins(o)
	}
	if s.legacy && o.Set["console-host"] {
		settings.PinConsoleHosts("the service's --console-host flag")
	}
	// The management listener comes first: core may need the console address
	// while it starts, and that address is the one actually bound.
	h.ln, err = net.Listen("tcp", cfg.Host.ConsoleAddr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", cfg.Host.ConsoleAddr, err)
	}
	h.console = managementConsoleURL(h.ln.Addr())
	coreCfg := core.Config{DocsApp: core.DocsApp{FS: docsapp.FS(), Hash: docsapp.Hash(), Entry: docsapp.Entry, ContentModule: docsapp.ContentModule}, DataDir: dataDir, Store: st, Private: h.Private, Lifecycle: mgr, Runtime: rt, Settings: settings,
		ConsoleURL: h.ConsoleURL, ConsoleURLFor: h.ConsoleURLFor, PrivateBackend: cfg.Network.PrivateBackend,
		// "flats" is the console's usual tailnet name: kept free for it, as before.
		Reserved: []string{"flats"}, ReservedNames: h.consoleReserved, Logf: logf}
	svc, err := core.New(ctx, coreCfg)
	if err != nil {
		return nil, err
	}
	h.Svc = svc
	// The deprecated key file path goes once its key is in flats.db.
	if cfg.Credentials.TailscaleAuthKeyFile != "" {
		if err := svc.DropDeprecatedAuthKeyFile(); err != nil {
			logf("warning: remove credentials.tailscale_authkey_file from %s: %v; it is read again at the next start", p.path, err)
		} else {
			logf("removed credentials.tailscale_authkey_file from %s: the Tailscale auth key is kept in flats.db", p.path)
		}
	}

	// The scrape reads core's in-memory snapshot and the endpoint states the
	// metrics refresh loop keeps; it never calls a provider backend.
	h.metrics = metrics.New(metrics.Options{Source: svc, Endpoints: mgr})
	h.metrics.Start()

	apiSrv := &api.Server{Svc: svc, System: h}
	mux := http.NewServeMux()
	apiH := apiSrv.Handler()
	mux.Handle("/api/", apiH)
	mux.Handle("/console/api/", apiH)
	mux.Handle("/mcp", mcpx.Handler(svc, mcpx.Options{Version: buildinfo.Get().Version}))
	llms := mcpx.LLMsHandler(svc, mcpx.Options{Version: buildinfo.Get().Version})
	for _, p := range mcpx.LLMsPaths {
		mux.Handle(p, llms)
	}
	// Without a method in the pattern, so other methods get 405 here
	// instead of reaching the console page.
	mux.Handle("/metrics", h.metrics.Handler())
	mux.Handle("/", console.Handler())
	h.Mux = mux

	if tcp, ok := h.ln.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		logf("warning: the management API listens on %s, which is not a loopback address", h.ln.Addr())
	}
	// Requests are counted outside the guard, so refused ones count too.
	h.srv = &http.Server{Handler: h.metrics.Management(loopbackGuard(h.ln.Addr(), mux, h.Svc.ExternalConsoleOrigins)), ReadHeaderTimeout: 15 * time.Second}
	go func() {
		if err := h.srv.Serve(h.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logf("management server: %v", err)
		}
	}()
	// Each console domain is its own tailnet node. The tsnet identity
	// middleware sets Tailscale-User-Login from WhoIs on it, so console
	// decisions there record who made them.
	if err := h.startConsoleDomains(ctx); err != nil {
		return nil, err
	}
	h.background(func(ctx context.Context) { h.enrollWaiting(ctx, keys) })
	ok = true
	return h, nil
}

// configHash returns the config hash of the file at path.
func configHash(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return config.Hash(b), nil
}

// storedPrivateBackend is network.private_backend as the operator chose
// it, or "" when config.json leaves it at the default. Exposure policy
// tokens include it, and approvals requested before config.json existed
// recorded "" for hosts that never chose a private backend.
func storedPrivateBackend(c *config.Config) string {
	v, src, _ := c.Lookup("network.private_backend")
	if src == config.SourceDefault {
		return ""
	}
	return v.(string)
}

// zrokEnvironment is the zrok environment directory: zrok.environment, or the
// one Flats manages in the data directory when the operator connects an
// account in the console.
func zrokEnvironment(cfg *config.Config) string {
	if cfg.Zrok.Environment != "" {
		return cfg.Zrok.Environment
	}
	return filepath.Join(cfg.Host.DataDir, "zrok-environment")
}

// zrokConfiguration is zrok's part of the provider configuration token, or
// "" when zrok is not permitted, so hosts without zrok keep their token. It
// names the environment directory, the account (a hash of its token, never
// the token) and the namespace, so an approval granted for one zrok account
// does not apply to another.
func zrokConfiguration(cfg *config.Config, zn *zrok.Net) string {
	if !slices.Contains(cfg.Network.Permitted, string(provider.Zrok)) {
		return ""
	}
	account := ""
	if zn != nil {
		account = zn.Account()
	}
	return zrokConfigurationOf(cfg.Zrok.Environment, account, cfg.Zrok.Namespace)
}

func zrokConfigurationOf(environment, account, namespace string) string {
	return strings.Join([]string{environment, account, namespace}, "\x00")
}

// cfquickConfig is the Cloudflare quick tunnel backend's configuration: its
// tunnel records live in <data>/cloudflare-quick.
func cfquickConfig(dataDir string, logf func(string, ...any)) cfquick.Config {
	return cfquick.Config{Dir: filepath.Join(dataDir, "cloudflare-quick"), Logf: logf}
}

// netbirdConfiguration is NetBird's part of the provider configuration
// token, or "" when neither NetBird provider is permitted. It names the
// management server and the setup key (a one-way fingerprint, never the key),
// so an approval granted under one key does not apply to another.
func netbirdConfiguration(cfg *config.Config, setupKey string) string {
	if !slices.Contains(cfg.Network.Permitted, string(provider.Netbird)) && !slices.Contains(cfg.Network.Permitted, string(provider.NetbirdExpose)) {
		return ""
	}
	return netbirdConfigurationOf(cfg.Netbird.ManagementURL, netbirdKeyFingerprint(setupKey))
}

func netbirdConfigurationOf(managementURL, keyFingerprint string) string {
	return strings.Join([]string{managementURL, keyFingerprint}, "\x00")
}

func providerConfiguration(cfg portal.Config, zrokConfig, netbirdConfig string) string {
	// Only public relay origins and non-secret desired settings participate.
	// Do not include userinfo, query strings, identities, keys or readiness.
	origins := make([]string, 0, len(cfg.Relays))
	for _, relay := range cfg.Relays {
		if u, err := url.Parse(relay); err == nil {
			origins = append(origins, u.Scheme+"://"+u.Host)
		}
	}
	slices.Sort(origins)
	raw, _ := json.Marshal(struct {
		Relays          []string
		Discovery       bool
		MaxActiveRelays int
		Zrok            string `json:",omitempty"`
		Netbird         string `json:",omitempty"`
	}{origins, cfg.Discovery, cfg.MaxActiveRelays, zrokConfig, netbirdConfig})
	return string(raw)
}

// managementConsoleURL turns a bind address into a usable operator link.
// Wildcard addresses are not destinations and are rejected by loopbackGuard.
// Use IPv4 loopback for Docker's published IPv4 ports; preserve explicit IPs.
func managementConsoleURL(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
			return "http://" + net.JoinHostPort("127.0.0.1", port)
		}
	}
	return "http://" + addr.String()
}

// loopbackGuard accepts only the loopback names of the bound address as
// Host (and Origin): 127.0.0.1, localhost and ::1 with its port, plus the
// listen IP itself when it is a specific address.
func loopbackGuard(addr net.Addr, next http.Handler, external ...func() []string) http.Handler {
	host, port, _ := net.SplitHostPort(addr.String())
	hosts := []string{"127.0.0.1", "localhost", "::1"}
	if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() && !slices.Contains(hosts, ip.String()) {
		hosts = append(hosts, ip.String())
	}
	ports := []string{port}
	if port == "80" {
		ports = append(ports, "")
	}
	g := &api.HostGuard{Hosts: func() []string { return hosts }, Ports: ports, Next: next}
	if len(external) > 0 {
		g.ExternalOrigins = external[0]
	}
	return g
}

// SystemStatus is returned by /api/status and the console settings page.
type SystemStatus struct {
	// Version is Build.Version, kept for clients that read it here.
	Version    string         `json:"version"`
	Build      buildinfo.Info `json:"build"`
	DataDir    string         `json:"data_dir"`
	ConsoleURL string         `json:"console_url"`
	// ConsoleURLs lists every console domain the host serves, in order.
	ConsoleURLs []string                `json:"console_urls"`
	MCPURL      string                  `json:"mcp_url"`
	LocalURL    string                  `json:"local_url"`
	Private     core.NetStatus          `json:"private"`
	Public      *core.NetStatus         `json:"public,omitempty"`
	Runtime     bool                    `json:"server_flats"`
	Grants      []string                `json:"provider_grants,omitempty"`
	Providers   []core.ExposureEndpoint `json:"providers"`
	Redirects   map[string]string       `json:"redirects,omitempty"`
}

// Status implements api.System.
func (h *Host) Status(ctx context.Context) any {
	console := h.ConsoleURLFor(ctx) // read once so console_url and mcp_url agree
	urls := h.ConsoleURLs()
	if urls == nil {
		urls = []string{}
	}
	s := SystemStatus{Version: buildinfo.Get().Version, Build: buildinfo.Get(), DataDir: h.Config.Host.DataDir, ConsoleURL: console, ConsoleURLs: urls, MCPURL: strings.TrimSuffix(console, "/") + "/mcp",
		LocalURL: h.console, Private: h.Private.Status(), Runtime: h.Config.Host.ServerRuntime, Redirects: h.Svc.Redirects()}
	if h.Providers != nil {
		s.Providers = h.Providers.HostStatus()
		for _, id := range h.Providers.File().Permitted {
			s.Grants = append(s.Grants, string(id))
		}
	}
	if h.Public != nil {
		ps := h.Public.Status()
		s.Public = &ps
	}
	return s
}

// Close drains management requests, stops workers, closes public then private
// networks and the store, and finally releases the data-directory lock.
// A teardown timeout retains the directory lock until process exit because
// background teardown may still access its state. Cleanup is best effort;
// repeated and concurrent calls return the same result.
func (h *Host) Close() error {
	h.closeOnce.Do(func() {
		var errs []error
		collect := func(component string, err error) {
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", component, err))
			}
		}
		h.bgMu.Lock()
		h.bgClosed = true
		h.bgMu.Unlock()
		if h.bgCancel != nil {
			h.bgCancel()
		}
		if h.srv == nil && h.ln != nil {
			collect("management listener", h.ln.Close())
		}
		if h.srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := h.srv.Shutdown(ctx)
			cancel()
			collect("management shutdown", err)
			if err != nil {
				collect("management close", h.srv.Close())
			}
		}
		if h.metrics != nil {
			h.metrics.Close()
		}
		if h.Svc != nil {
			collect("core shutdown", h.Svc.Close())
		}
		if h.workers != nil {
			h.workers.Close() // the pre-started worker; core stopped the others
		}
		if h.Providers != nil {
			collect("provider routes", h.Providers.Close())
		}
		h.pmu.Lock()
		tsNet, portalNet, zrokNet, quickNet, netbirdNet := h.tsNet, h.portalNet, h.zrokNet, h.quickNet, h.netbirdNet
		h.pmu.Unlock()
		if h.Public != nil {
			collect("public network shutdown", h.Public.Close())
		} else if portalNet != nil {
			collect("portal shutdown", portalNet.Close())
		}
		if zrokNet != nil {
			collect("zrok shutdown", zrokNet.Close())
		}
		if quickNet != nil {
			collect("cloudflare-quick shutdown", quickNet.Close())
		}
		if netbirdNet != nil {
			collect("netbird shutdown", netbirdNet.Close())
		}
		if h.Private != nil {
			collect("private network shutdown", h.Private.Close())
		}
		if tsNet != nil && h.Private != core.PrivateNet(tsNet) {
			collect("tailscale shutdown", tsNet.Close())
		}
		if h.localNet != nil && h.Private != h.localNet {
			collect("local shutdown", h.localNet.Close())
		}
		// Background work may wait on a console request served by a network
		// closed above; it needs the store until it returns.
		h.bg.Wait()
		if h.Store != nil {
			collect("store shutdown", h.Store.Close())
		}
		if h.dataLock != nil {
			if errors.Is(errors.Join(errs...), context.DeadlineExceeded) {
				retainDataLock(h.dataLock)
			} else {
				collect("data directory unlock", h.dataLock.Close())
			}
		}
		h.closeErr = errors.Join(errs...)
	})
	return h.closeErr
}

// Keep timed-out teardown's lock reachable, including after the Host is dropped.
var retainedLocks struct {
	sync.Mutex
	files []*os.File
}

func retainDataLock(f *os.File) {
	retainedLocks.Lock()
	retainedLocks.files = append(retainedLocks.files, f)
	retainedLocks.Unlock()
}

// relayLimit converts portal.max_active_relays, which config bounds to
// 2^31-1, to the int the Portal SDK takes.
func relayLimit(n int64) int {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < 0 {
		return 0
	}
	return int(n)
}
