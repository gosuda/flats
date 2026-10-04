// Package app wires every Flats module into the `flats serve` process.
package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
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
	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/console"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/expose/portal"
	"github.com/gosuda/flats/internal/expose/provider"
	tsnetx "github.com/gosuda/flats/internal/expose/tsnet"
	"github.com/gosuda/flats/internal/forkwatch"
	"github.com/gosuda/flats/internal/mcpx"
	"github.com/gosuda/flats/internal/runtime"
	"github.com/gosuda/flats/internal/slug"
	"github.com/gosuda/flats/internal/store"
	"golang.org/x/term"
)

// Version is set at build time with -ldflags "-X github.com/gosuda/flats/internal/app.Version=...".
var Version = "dev"

// Options configure `flats serve`.
//
// With ConfigPath set the host runs in config mode: config.json is the only
// source of host configuration, and only the four host keys may be changed
// for one run (--listen, --local-addr, --console-host, --runtime). Without
// it the host runs in legacy mode from <DataDir>/config.json: the first run
// creates that file from the flags (migrating a legacy data directory), and
// later runs refuse flags that differ from it.
type Options struct {
	ConfigPath              string
	DataDir                 string // legacy mode: the directory holding config.json
	Listen                  string // loopback management address
	Network                 string // tailscale | local; legacy private path
	LocalAddr               string // local network address
	AuthKeyFile             string
	ConsoleHost             string
	Portal                  bool
	Permit                  []string // explicit host grants; does not publish a flat
	Relays                  []string
	Runtime                 bool
	OperatorCredentialStdin bool
	OperatorCredentialFile  string
	// OperatorCredential is an embedding-only input; Start clears it before
	// retaining options. CLI credentials are accepted through stdin or an operator-owned 0600 file.
	OperatorCredential string
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
	fs.StringVar(&o.AuthKeyFile, "authkey-file", "", "legacy mode: file holding a reusable, untagged Tailscale auth key for new nodes (else TS_AUTHKEY or interactive login)")
	fs.StringVar(&o.ConsoleHost, "console-host", "flats", "tailnet host name of the console")
	fs.BoolVar(&o.Portal, "portal", false, "legacy mode: grant Portal and attach it as the legacy public network")
	fs.StringVar(&permit, "permit", "", "legacy mode: comma-separated host grants: tailscale, tailscale-funnel, portal. Local needs no grant. Recorded in config.json and does not publish a flat")
	fs.StringVar(&relays, "relays", "", "legacy mode: comma-separated Portal relays (default: Portal CLI default discovery)")
	fs.BoolVar(&o.Runtime, "runtime", true, "enable server flats (wazero runtime)")
	fs.BoolVar(&o.OperatorCredentialStdin, "operator-credential-stdin", false, "read a separately provisioned operator credential from hidden terminal input or stdin; required to enable console decisions")
	fs.StringVar(&o.OperatorCredentialFile, "operator-credential-file", "", "legacy mode: operator-owned regular 0600 credential file for noninteractive services; mutually exclusive with stdin")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	o.Set = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { o.Set[f.Name] = true })
	if o.Set["config"] && !filepath.IsAbs(o.ConfigPath) {
		return o, errors.New("--config must be an absolute path")
	}
	if o.OperatorCredentialStdin && o.OperatorCredentialFile != "" {
		return o, errors.New("choose only one operator credential source")
	}
	for _, path := range []*string{&o.OperatorCredentialFile, &o.AuthKeyFile} {
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
	if o.OperatorCredentialStdin {
		o.OperatorCredential, err = readOperatorCredential(os.Stdin, os.Stderr)
		if err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	h, err := Start(ctx, o)
	o.OperatorCredential = ""
	if err != nil {
		return err
	}
	log.Printf("flats %s serving; console %s (loopback http://%s)", Version, h.ConsoleURL(), h.Addr())
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
	Operator   *api.OperatorAuthority
	Mux        http.Handler
	srv        *http.Server
	ln         net.Listener
	localNet   *local.Net
	tsNet      *tsnetx.Net
	portalNet  *portal.Net
	console    string
	dataLock   *os.File
	closeOnce  sync.Once
	closeErr   error
}

// ConsoleURL is the operator console address (tailnet URL when available).
func (h *Host) ConsoleURL() string { return h.console }

// Addr is the loopback management address actually bound.
func (h *Host) Addr() string { return h.ln.Addr().String() }

// configModeFlags are the serve flags config mode accepts. The host flags
// override config.json for this run only.
var configModeFlags = map[string]string{
	"config": "", "operator-credential-stdin": "",
	"listen": "host.management_addr", "local-addr": "host.local_addr",
	"console-host": "host.console_host", "runtime": "host.server_runtime",
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

// tailscaleAuthKey returns the auth key for new tailnet nodes. In config
// mode the key comes only from credentials.tailscale_authkey_file: tsnet
// would otherwise log in with a shell's TS_* variables or reach another
// control server, so they are refused. Legacy mode still reads TS_AUTHKEY.
func tailscaleAuthKey(c *config.Config, legacy bool, logf func(string, ...any)) (string, error) {
	if !legacy {
		var set []string
		for _, name := range tailscaleEnv {
			if os.Getenv(name) != "" {
				set = append(set, name)
			}
		}
		if len(set) > 0 {
			return "", actionf("%s set in the environment, but tailscale is permitted in config.json: put the auth key in a 0600 file named by credentials.tailscale_authkey_file and unset the variables", strings.Join(set, ", "))
		}
	}
	if path := c.Credentials.TailscaleAuthKeyFile; path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read auth key file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	if key := os.Getenv("TS_AUTHKEY"); key != "" {
		logf("warning: using TS_AUTHKEY from the environment; config.json cannot record it, so move the key to a 0600 file and set credentials.tailscale_authkey_file")
		return key, nil
	}
	return "", nil
}

// Start builds and starts every component.
func Start(ctx context.Context, o Options) (*Host, error) {
	credential := o.OperatorCredential
	o.OperatorCredential = ""
	if o.OperatorCredentialFile != "" && (o.OperatorCredentialStdin || credential != "") {
		return nil, errors.New("choose only one operator credential source")
	}
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
	h := &Host{Opts: o, Config: cfg, ConfigPath: p.path, Store: p.st, dataLock: p.lock}
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
	var authKey string
	if permits(provider.Tailscale) || permits(provider.Funnel) {
		if authKey, err = tailscaleAuthKey(cfg, s.legacy, logf); err != nil {
			return nil, err
		}
	}
	if err := core.CheckSecretKey(ctx, dataDir, st); err != nil {
		if errors.Is(err, core.ErrSecretKeyMissing) {
			return nil, &ActionError{err}
		}
		return nil, err
	}
	if path := cfg.Credentials.OperatorFile; path != "" {
		if o.OperatorCredentialStdin || credential != "" {
			return nil, errors.New("choose only one operator credential source: credentials.operator_file is set")
		}
		if credential, err = readOperatorCredentialFile(path); err != nil {
			return nil, err
		}
	}
	if credential != "" {
		h.Operator, err = api.NewOperatorAuthority(credential)
		credential = ""
		if err != nil {
			return nil, err
		}
	}
	operator := h.Operator

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
		Discovery: cfg.Portal.Discovery, MaxActiveRelays: int(cfg.Portal.MaxActiveRelays), Logf: logf}
	if permits(provider.Portal) {
		pn, err := portal.New(portalOptions)
		if err != nil {
			return nil, fmt.Errorf("portal: %w", err)
		}
		h.portalNet = pn
	}
	var tail provider.Tailnet
	if h.tsNet != nil {
		tail = provider.TSNet{Net: h.tsNet}
	}
	grants := &provider.File{PrivateBackend: storedPrivateBackend(cfg)}
	for _, id := range cfg.Network.Permitted {
		grants.Permitted = append(grants.Permitted, provider.ID(id))
	}
	backends := provider.Options{Local: loop, Tailscale: tail, TailscaleStateDir: filepath.Join(dataDir, "tsnet"), Configuration: providerConfiguration(portalOptions), Grants: grants, Permission: func(ctx context.Context, slug string, id provider.ID) (bool, error) {
		return st.ProviderPermitted(ctx, slug, string(id))
	}}
	if h.portalNet != nil {
		backends.Portal = h.portalNet
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
		rt = &runtime.Manager{DataDir: dataDir, Logf: logf}
	}
	// Settings are saved to config.json against the hash this process last
	// read or wrote. SettingsSource serializes saves, so hash needs no lock.
	writer, hash := config.NewWriter(p.path), p.hash
	settings, err := core.NewSettingsSource(p.doc, func(_ context.Context, apply func(*config.Document) error) error {
		h, err := writer.Save(hash, apply)
		if err == nil {
			hash = h
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	h.console = "http://" + cfg.Host.ManagementAddr
	coreCfg := core.Config{DataDir: dataDir, Store: st, Private: h.Private, Lifecycle: mgr, Runtime: rt, Settings: settings,
		ConsoleURL: func() string { return h.console }, Reserved: []string{cfg.Host.ConsoleHost}, Logf: logf}
	if operator != nil {
		coreCfg.ValidateOperatorDecision = operator.ValidateDecision
		coreCfg.OperatorIdentity = operator.DecisionIdentity
	}
	svc, err := core.New(ctx, coreCfg)
	if err != nil {
		return nil, err
	}
	h.Svc = svc

	apiSrv := &api.Server{Svc: svc, System: h, Operator: operator}
	mux := http.NewServeMux()
	apiH := apiSrv.Handler()
	mux.Handle("/api/", apiH)
	mux.Handle("/console/api/", apiH)
	mux.Handle("/mcp", mcpx.Handler(svc, mcpx.Options{Version: Version}))
	llms := mcpx.LLMsHandler(svc, mcpx.Options{Version: Version})
	for _, p := range mcpx.LLMsPaths {
		mux.Handle(p, llms)
	}
	mux.Handle("/", console.Handler())
	h.Mux = mux

	h.ln, err = net.Listen("tcp", cfg.Host.ManagementAddr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", cfg.Host.ManagementAddr, err)
	}
	if useTailscale {
		h.console = h.Private.URL(cfg.Host.ConsoleHost)
	} else {
		h.console = "http://" + h.ln.Addr().String()
	}
	if tcp, ok := h.ln.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		logf("warning: the management API listens on %s, which is not a loopback address", h.ln.Addr())
	}
	if err := slug.Validate(cfg.Host.ConsoleHost); err != nil {
		logf("warning: host.console_host %q is not a valid host name; flats cannot use it as a tailnet name", cfg.Host.ConsoleHost)
	}
	h.srv = &http.Server{Handler: loopbackGuard(h.ln.Addr(), mux), ReadHeaderTimeout: 15 * time.Second}
	go func() {
		if err := h.srv.Serve(h.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logf("management server: %v", err)
		}
	}()
	if useTailscale {
		// The tsnet identity middleware sets Tailscale-User-Login from WhoIs
		// on this node, so console decisions here record who made them.
		node := &api.HostGuard{Hosts: h.consoleNames, Ports: []string{"", "80", "443"}, Next: api.TailnetIdentity(mux)}
		if _, err := h.Private.Serve(ctx, cfg.Host.ConsoleHost, node, false); err != nil {
			logf("console node %s: %v", cfg.Host.ConsoleHost, err)
		}
	}
	ok = true
	return h, nil
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

func readOperatorCredential(input *os.File, output io.Writer) (string, error) {
	var raw []byte
	var err error
	if term.IsTerminal(int(input.Fd())) {
		fmt.Fprint(output, "Operator credential (hidden): ")
		raw, err = term.ReadPassword(int(input.Fd()))
		fmt.Fprintln(output)
	} else {
		// Explicit out-of-band provisioning for embedded/disposable hosts.
		var value string
		value, err = bufio.NewReader(io.LimitReader(input, 4097)).ReadString('\n')
		if errors.Is(err, io.EOF) && len(value) > 0 {
			err = nil
		}
		raw = []byte(strings.TrimRight(value, "\r\n"))
	}
	if err != nil {
		return "", errors.New("could not read operator credential")
	}
	if len(raw) < 32 || len(raw) > 4096 {
		return "", errors.New("operator credential must contain 32 to 4096 bytes")
	}
	return string(raw), nil
}

func providerConfiguration(cfg portal.Config) string {
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
	}{origins, cfg.Discovery, cfg.MaxActiveRelays})
	return string(raw)
}

// loopbackGuard accepts only the loopback names of the bound address as
// Host (and Origin): 127.0.0.1, localhost and ::1 with its port, plus the
// listen IP itself when it is a specific address.
func loopbackGuard(addr net.Addr, next http.Handler) http.Handler {
	host, port, _ := net.SplitHostPort(addr.String())
	hosts := []string{"127.0.0.1", "localhost", "::1"}
	if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() && !slices.Contains(hosts, ip.String()) {
		hosts = append(hosts, ip.String())
	}
	ports := []string{port}
	if port == "80" {
		ports = append(ports, "")
	}
	return &api.HostGuard{Hosts: func() []string { return hosts }, Ports: ports, Next: next}
}

// consoleNames are the tailnet names of the console node: the configured
// host, and its MagicDNS name and first label once the node knows them
// (control may have renamed a duplicate to e.g. flats-1).
func (h *Host) consoleNames() []string {
	names := []string{h.Config.Host.ConsoleHost}
	if u, err := url.Parse(h.Private.URL(h.Config.Host.ConsoleHost)); err == nil {
		if fqdn := u.Hostname(); fqdn != "" && !strings.Contains(fqdn, "<") {
			short, _, _ := strings.Cut(fqdn, ".")
			names = append(names, fqdn, short)
		}
	}
	return names
}

// SystemStatus is returned by /api/status and the console settings page.
type SystemStatus struct {
	Version    string                  `json:"version"`
	DataDir    string                  `json:"data_dir"`
	ConsoleURL string                  `json:"console_url"`
	MCPURL     string                  `json:"mcp_url"`
	LocalURL   string                  `json:"local_url"`
	Private    core.NetStatus          `json:"private"`
	Public     *core.NetStatus         `json:"public,omitempty"`
	Runtime    bool                    `json:"server_flats"`
	Grants     []string                `json:"provider_grants,omitempty"`
	Providers  []core.ExposureEndpoint `json:"providers"`
	Redirects  map[string]string       `json:"redirects,omitempty"`
}

// Status implements api.System.
func (h *Host) Status(ctx context.Context) any {
	s := SystemStatus{Version: Version, DataDir: h.Config.Host.DataDir, ConsoleURL: h.console, MCPURL: strings.TrimSuffix(h.console, "/") + "/mcp",
		LocalURL: "http://" + h.Addr(), Private: h.Private.Status(), Runtime: h.Config.Host.ServerRuntime, Redirects: h.Svc.Redirects()}
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
		if h.srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := h.srv.Shutdown(ctx)
			cancel()
			collect("management shutdown", err)
			if err != nil {
				collect("management close", h.srv.Close())
			}
		}
		if h.Svc != nil {
			collect("core shutdown", h.Svc.Close())
		}
		if h.Providers != nil {
			collect("provider routes", h.Providers.Close())
		}
		if h.Public != nil {
			collect("public network shutdown", h.Public.Close())
		} else if h.portalNet != nil {
			collect("portal shutdown", h.portalNet.Close())
		}
		if h.Private != nil {
			collect("private network shutdown", h.Private.Close())
		}
		if h.tsNet != nil && h.Private != h.tsNet {
			collect("tailscale shutdown", h.tsNet.Close())
		}
		if h.localNet != nil && h.Private != h.localNet {
			collect("local shutdown", h.localNet.Close())
		}
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

// readOperatorCredentialFile validates the opened descriptor, avoiding a
// pathname-check/read race and refusing symlinks and blocking special files.
func readOperatorCredentialFile(path string) (string, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", errors.New("could not open operator credential file")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", errors.New("could not inspect operator credential file")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > 4098 || owner.Uid != uint32(os.Geteuid()) {
		return "", errors.New("operator credential file must be a regular 0600 file owned by the service user")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 4099))
	if err != nil {
		return "", errors.New("could not read operator credential file")
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if len(value) < 32 || len(value) > 4096 || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("operator credential file must contain one credential of 32 to 4096 bytes")
	}
	return value, nil
}
