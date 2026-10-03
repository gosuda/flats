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
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/console"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/expose/portal"
	"github.com/gosuda/flats/internal/expose/provider"
	tsnetx "github.com/gosuda/flats/internal/expose/tsnet"
	"github.com/gosuda/flats/internal/forkwatch"
	"github.com/gosuda/flats/internal/mcpx"
	"github.com/gosuda/flats/internal/runtime"
	"github.com/gosuda/flats/internal/store"
	"golang.org/x/term"
)

// Version is set at build time with -ldflags "-X github.com/gosuda/flats/internal/app.Version=...".
var Version = "dev"

// Options configure `flats serve`.
type Options struct {
	DataDir                 string
	Listen                  string // loopback management address
	Network                 string // tailscale | local; legacy private path
	NetworkSet              bool   // true when --network was present on the command line
	LocalAddr               string // local network address
	AuthKeyFile             string
	ConsoleHost             string
	Portal                  bool
	PortalSet               bool     // true when --portal was present on the command line
	Permit                  []string // explicit host grants; does not publish a flat
	Relays                  []string
	Runtime                 bool
	OperatorCredentialStdin bool
	OperatorCredentialFile  string
	// OperatorCredential is an embedding-only input; Start clears it before
	// retaining options. CLI credentials are accepted through stdin or an operator-owned 0600 file.
	OperatorCredential string
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
	fs.StringVar(&o.DataDir, "data", DefaultDataDir(), "data directory")
	fs.StringVar(&o.Listen, "listen", "127.0.0.1:7878", "loopback address for the CLI, local agents and the console")
	fs.StringVar(&o.Network, "network", "local", "legacy private path: tailscale or local. tailscale records a host grant and preserves Private Tailscale for pre-lifecycle flats; does not enable Funnel")
	fs.StringVar(&o.LocalAddr, "local-addr", "127.0.0.1:7879", "address of the local network")
	fs.StringVar(&o.AuthKeyFile, "authkey-file", "", "file holding a reusable, untagged Tailscale auth key for new nodes (else TS_AUTHKEY or interactive login)")
	fs.StringVar(&o.ConsoleHost, "console-host", "flats", "tailnet host name of the console")
	fs.BoolVar(&o.Portal, "portal", false, "grant Portal and attach it as the legacy public network")
	fs.StringVar(&permit, "permit", "", "comma-separated host grants: tailscale, tailscale-funnel, portal. Local needs no grant. Stored in the data directory and does not publish a flat")
	fs.StringVar(&relays, "relays", "", "comma-separated Portal relays (default: Portal CLI default discovery)")
	fs.BoolVar(&o.Runtime, "runtime", true, "enable server flats (wazero runtime)")
	fs.BoolVar(&o.OperatorCredentialStdin, "operator-credential-stdin", false, "read a separately provisioned operator credential from hidden terminal input or stdin; required to enable console decisions")
	fs.StringVar(&o.OperatorCredentialFile, "operator-credential-file", "", "operator-owned regular 0600 credential file for noninteractive services; mutually exclusive with stdin")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if o.OperatorCredentialStdin && o.OperatorCredentialFile != "" {
		return o, errors.New("choose only one operator credential source")
	}
	if o.OperatorCredentialFile != "" {
		var err error
		o.OperatorCredentialFile, err = filepath.Abs(o.OperatorCredentialFile)
		if err != nil {
			return o, err
		}
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "network":
			o.NetworkSet = true
		case "portal":
			o.PortalSet = true
		}
	})
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
	log.Printf("flats %s serving; console %s (loopback http://%s)", Version, h.ConsoleURL(), o.Listen)
	<-ctx.Done()
	log.Printf("shutting down")
	return h.Close()
}

// Host is a running Flats process.
type Host struct {
	Opts      Options
	Svc       *core.Service
	Store     *store.Store
	Private   core.PrivateNet
	Public    core.PublicNet
	Providers *provider.Manager
	Operator  *api.OperatorAuthority
	Mux       http.Handler
	srv       *http.Server
	ln        net.Listener
	localNet  *local.Net
	tsNet     *tsnetx.Net
	portalNet *portal.Net
	console   string
	dataLock  *os.File
	closeOnce sync.Once
	closeErr  error
}

// ConsoleURL is the operator console address (tailnet URL when available).
func (h *Host) ConsoleURL() string { return h.console }

// Addr is the loopback management address actually bound.
func (h *Host) Addr() string { return h.ln.Addr().String() }

// Start builds and starts every component.
func Start(ctx context.Context, o Options) (*Host, error) {
	var operator *api.OperatorAuthority
	if o.OperatorCredentialFile != "" {
		if o.OperatorCredentialStdin || o.OperatorCredential != "" {
			return nil, errors.New("choose only one operator credential source")
		}
		var err error
		o.OperatorCredential, err = readOperatorCredentialFile(o.OperatorCredentialFile)
		if err != nil {
			return nil, err
		}
	}
	if o.OperatorCredential != "" {
		var err error
		operator, err = api.NewOperatorAuthority(o.OperatorCredential)
		o.OperatorCredential = ""
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		return nil, err
	}
	lock, err := lockDataDir(o.DataDir)
	if err != nil {
		return nil, err
	}
	h := &Host{Opts: o, dataLock: lock, Operator: operator}
	ok := false
	defer func() {
		if !ok {
			if err := h.Close(); err != nil {
				log.Printf("startup cleanup: %v", err)
			}
		}
	}()
	logf := log.Printf
	forkwatch.Start(ctx, logf)
	st, err := store.Open(filepath.Join(o.DataDir, "flats.db"))
	if err != nil {
		return nil, err
	}
	h.Store = st
	// Read migration evidence before this startup records new host grants.
	previousGrants, err := provider.Load(o.DataDir)
	if err != nil {
		return nil, err
	}
	if err := prepareLegacyPrivateUpgrade(ctx, st, o, previousGrants); err != nil {
		return nil, err
	}
	grants, err := hostGrants(o)
	if err != nil {
		return nil, err
	}
	loop, err := local.Listen(o.LocalAddr)
	if err != nil {
		return nil, fmt.Errorf("local network: %w", err)
	}
	h.localNet = loop
	if grants.Allows(provider.Tailscale) || grants.Allows(provider.Funnel) {
		key := os.Getenv("TS_AUTHKEY")
		if o.AuthKeyFile != "" {
			b, err := os.ReadFile(o.AuthKeyFile)
			if err != nil {
				return nil, fmt.Errorf("read auth key file: %w", err)
			}
			key = strings.TrimSpace(string(b))
		}
		n, err := tsnetx.New(tsnetx.Config{Dir: filepath.Join(o.DataDir, "tsnet"), AuthKey: key, Logf: logf})
		if err != nil {
			return nil, fmt.Errorf("tailscale: %w", err)
		}
		h.tsNet = n
	}
	// An explicit --portal=false keeps a stored grant and does not start Portal
	// for this process. Omitting the flag uses a grant that is already stored.
	usePortal := grants.Allows(provider.Portal) && !(o.PortalSet && !o.Portal)
	portalOptions := portalConfig(ctx, st, o, logf)
	if usePortal {
		p, err := portal.New(portalOptions)
		if err != nil {
			return nil, fmt.Errorf("portal: %w", err)
		}
		h.portalNet = p
	}
	var tail provider.Tailnet
	if h.tsNet != nil {
		tail = provider.TSNet{Net: h.tsNet}
	}
	backends := provider.Options{Local: loop, Tailscale: tail, TailscaleStateDir: filepath.Join(o.DataDir, "tsnet"), Configuration: providerConfiguration(portalOptions), Permission: func(ctx context.Context, slug string, id provider.ID) (bool, error) {
		return st.ProviderPermitted(ctx, slug, string(id))
	}}
	if h.portalNet != nil {
		backends.Portal = h.portalNet
	}
	mgr, err := provider.New(o.DataDir, backends)
	if err != nil {
		return nil, err
	}
	h.Providers = mgr
	useTailscale := o.Network == "tailscale" || (!o.NetworkSet && grants.PrivateBackend == "tailscale" && grants.Allows(provider.Tailscale))
	if useTailscale {
		if h.tsNet == nil {
			return nil, errors.New("tailscale is selected but not permitted")
		}
		h.Private = h.tsNet
	} else {
		h.Private = loop
	}
	if usePortal {
		h.Public = h.portalNet
	}
	var rt core.Runtime
	if o.Runtime {
		rt = &runtime.Manager{DataDir: o.DataDir, Logf: logf}
	}
	h.console = "http://" + o.Listen
	cfg := core.Config{DataDir: o.DataDir, Store: st, Private: h.Private, Lifecycle: mgr, Runtime: rt,
		ConsoleURL: func() string { return h.console }, Reserved: []string{o.ConsoleHost}, Logf: logf}
	if operator != nil {
		cfg.ValidateOperatorDecision = operator.ValidateDecision
		cfg.OperatorIdentity = operator.DecisionIdentity
	}
	svc, err := core.New(ctx, cfg)
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
	mux.Handle("/", console.Handler())
	h.Mux = mux

	h.ln, err = net.Listen("tcp", o.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", o.Listen, err)
	}
	if useTailscale {
		h.console = h.Private.URL(o.ConsoleHost)
	} else {
		h.console = "http://" + h.ln.Addr().String()
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
		if _, err := h.Private.Serve(ctx, o.ConsoleHost, node, false); err != nil {
			logf("console node %s: %v", o.ConsoleHost, err)
		}
	}
	ok = true
	return h, nil
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

// portalConfig builds the Portal configuration from flags and settings. Bad
// stored settings are logged and ignored, so a typo saved in the console
// cannot keep `flats serve` (and every flat) from starting.
func portalConfig(ctx context.Context, st *store.Store, o Options, logf func(string, ...any)) portal.Config {
	get := func(key string) string {
		v, err := st.GetSetting(ctx, key, core.Defaults[key])
		if err != nil {
			logf("setting %s: %v; using the default", key, err)
			return core.Defaults[key]
		}
		return strings.TrimSpace(v)
	}
	cfg := portal.Config{Dir: filepath.Join(o.DataDir, "portal"), Relays: o.Relays, Discovery: true, Logf: logf}
	if len(cfg.Relays) == 0 {
		if v := get(core.SetPortalRelays); v != "" {
			relays, err := portal.NormalizeRelays(strings.Split(v, ","))
			if err != nil {
				logf("setting %s=%q ignored: %v; using the Portal default relays", core.SetPortalRelays, v, err)
			} else {
				cfg.Relays = relays
			}
		}
	}
	if v := get(core.SetPortalDiscover); v != "" {
		on, err := strconv.ParseBool(v)
		switch {
		case err != nil:
			logf("setting %s=%q ignored: want true or false", core.SetPortalDiscover, v)
		case !on && len(cfg.Relays) == 0:
			logf("setting %s=false ignored: no relays are configured, so discovery stays on", core.SetPortalDiscover)
		default:
			cfg.Discovery = on
		}
	}
	if v := get(core.SetPortalMaxRelay); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			logf("setting %s=%q ignored: want a non-negative integer", core.SetPortalMaxRelay, v)
		} else {
			cfg.MaxActiveRelays = n // 0 keeps the Portal default
		}
	}
	return cfg
}

// hostGrants loads the on-disk permission file and applies explicit flags.
// A flag records a grant. It does not serve a flat, and a tailscale grant
// does not grant Funnel. Historical tsnet or portal directories are not grants.
func hostGrants(o Options) (provider.File, error) {
	f, err := provider.Load(o.DataDir)
	if err != nil {
		return f, err
	}
	if o.Network == "tailscale" {
		f, err = f.Grant(provider.Tailscale)
		if err != nil {
			return f, err
		}
		f.PrivateBackend = "tailscale"
	} else if o.NetworkSet && o.Network == "local" {
		f.PrivateBackend = "local"
	}
	if o.Portal {
		f, err = f.Grant(provider.Portal)
		if err != nil {
			return f, err
		}
	}
	for _, raw := range o.Permit {
		id, err := provider.ParseID(raw)
		if err != nil {
			return f, err
		}
		f, err = f.Grant(id)
		if err != nil {
			return f, err
		}
	}
	if err := provider.Save(o.DataDir, f); err != nil {
		return f, err
	}
	return f, nil
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
	names := []string{h.Opts.ConsoleHost}
	if u, err := url.Parse(h.Private.URL(h.Opts.ConsoleHost)); err == nil {
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
	s := SystemStatus{Version: Version, DataDir: h.Opts.DataDir, ConsoleURL: h.console, MCPURL: strings.TrimSuffix(h.console, "/") + "/mcp",
		LocalURL: "http://" + h.Addr(), Private: h.Private.Status(), Runtime: h.Opts.Runtime, Redirects: h.Svc.Redirects()}
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

// prepareLegacyPrivateUpgrade requires an explicit choice if the old default
// was Tailscale. This runs before backend creation, so an ambiguous upgrade
// neither silently removes tailnet access nor contacts a live provider.
func prepareLegacyPrivateUpgrade(ctx context.Context, st *store.Store, o Options, grants provider.File) error {
	pending, err := st.LegacyPrivateUpgradePending(ctx)
	if err != nil {
		return err
	}
	if !pending {
		return nil
	}
	if o.NetworkSet && o.Network == "local" {
		return st.DeclineLegacyTailscale(ctx)
	}
	if (o.Network == "tailscale" && grants.Migration.HistoricalTSNet) || (grants.PrivateBackend == "tailscale" && grants.Allows(provider.Tailscale)) {
		return st.PreserveLegacyTailscale(ctx)
	}

	if grants.Migration.HistoricalTSNet {
		return errors.New("legacy Tailscale flats require an explicit upgrade choice: use --network tailscale to preserve Private tailnet access, or --network local to stop using it; neither choice grants Funnel or Portal")
	}
	return st.DeclineLegacyTailscale(ctx)
}
