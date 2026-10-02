// Package app wires every Flats module into the `flats serve` process.
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	"syscall"
	"time"

	"github.com/oesni/flats/internal/api"
	"github.com/oesni/flats/internal/console"
	"github.com/oesni/flats/internal/core"
	"github.com/oesni/flats/internal/expose/local"
	"github.com/oesni/flats/internal/expose/portal"
	tsnetx "github.com/oesni/flats/internal/expose/tsnet"
	"github.com/oesni/flats/internal/mcpx"
	"github.com/oesni/flats/internal/runtime"
	"github.com/oesni/flats/internal/store"
)

// Version is set at build time with -ldflags "-X github.com/oesni/flats/internal/app.Version=...".
var Version = "dev"

// Options configure `flats serve`.
type Options struct {
	DataDir     string
	Listen      string // loopback management address
	Network     string // tailscale | local
	LocalAddr   string // local network address (network=local)
	AuthKeyFile string
	ConsoleHost string
	Portal      bool
	Relays      []string
	Runtime     bool
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
	var relays string
	fs.StringVar(&o.DataDir, "data", DefaultDataDir(), "data directory")
	fs.StringVar(&o.Listen, "listen", "127.0.0.1:7878", "loopback address for the CLI, local agents and the console")
	fs.StringVar(&o.Network, "network", "tailscale", "private network: tailscale or local (development; serves <flat>.localhost)")
	fs.StringVar(&o.LocalAddr, "local-addr", "127.0.0.1:7879", "address of the local network (network=local)")
	fs.StringVar(&o.AuthKeyFile, "authkey-file", "", "file holding a reusable, untagged Tailscale auth key for new nodes (else TS_AUTHKEY or interactive login)")
	fs.StringVar(&o.ConsoleHost, "console-host", "flats", "tailnet host name of the console")
	fs.BoolVar(&o.Portal, "portal", true, "enable public flats through Portal")
	fs.StringVar(&relays, "relays", "", "comma-separated Portal relays (default: Portal CLI default discovery)")
	fs.BoolVar(&o.Runtime, "runtime", true, "enable server flats (wazero runtime)")
	if err := fs.Parse(args); err != nil {
		return o, err
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	h, err := Start(ctx, o)
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
	Opts    Options
	Svc     *core.Service
	Store   *store.Store
	Private core.PrivateNet
	Public  core.PublicNet
	Mux     http.Handler
	srv     *http.Server
	ln      net.Listener
	console string
}

// ConsoleURL is the operator console address (tailnet URL when available).
func (h *Host) ConsoleURL() string { return h.console }

// Addr is the loopback management address actually bound.
func (h *Host) Addr() string { return h.ln.Addr().String() }

// Start builds and starts every component.
func Start(ctx context.Context, o Options) (*Host, error) {
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		return nil, err
	}
	logf := log.Printf
	st, err := store.Open(filepath.Join(o.DataDir, "flats.db"))
	if err != nil {
		return nil, err
	}
	h := &Host{Opts: o, Store: st}
	ok := false
	defer func() {
		if !ok {
			h.Close()
		}
	}()

	switch o.Network {
	case "local":
		ln, err := local.Listen(o.LocalAddr)
		if err != nil {
			return nil, fmt.Errorf("local network: %w", err)
		}
		h.Private = ln
	default:
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
		h.Private = n
	}
	if o.Portal {
		p, err := portal.New(portalConfig(ctx, st, o, logf))
		if err != nil {
			return nil, fmt.Errorf("portal: %w", err)
		}
		h.Public = p
	}
	var rt core.Runtime
	if o.Runtime {
		rt = &runtime.Manager{DataDir: o.DataDir, Logf: logf}
	}
	h.console = "http://" + o.Listen
	cfg := core.Config{DataDir: o.DataDir, Store: st, Private: h.Private, Runtime: rt,
		ConsoleURL: func() string { return h.console }, Reserved: []string{o.ConsoleHost}, Logf: logf}
	if h.Public != nil {
		cfg.Public = h.Public
	}
	svc, err := core.New(ctx, cfg)
	if err != nil {
		return nil, err
	}
	h.Svc = svc

	apiSrv := &api.Server{Svc: svc, System: h}
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
	if o.Network == "tailscale" {
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
	if o.Network == "tailscale" {
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
	Version    string            `json:"version"`
	DataDir    string            `json:"data_dir"`
	ConsoleURL string            `json:"console_url"`
	MCPURL     string            `json:"mcp_url"`
	LocalURL   string            `json:"local_url"`
	Private    core.NetStatus    `json:"private"`
	Public     *core.NetStatus   `json:"public,omitempty"`
	Runtime    bool              `json:"server_flats"`
	Redirects  map[string]string `json:"redirects,omitempty"`
}

// Status implements api.System.
func (h *Host) Status(ctx context.Context) any {
	s := SystemStatus{Version: Version, DataDir: h.Opts.DataDir, ConsoleURL: h.console, MCPURL: strings.TrimSuffix(h.console, "/") + "/mcp",
		LocalURL: "http://" + h.Addr(), Private: h.Private.Status(), Runtime: h.Opts.Runtime, Redirects: h.Svc.Redirects()}
	if h.Public != nil {
		ps := h.Public.Status()
		s.Public = &ps
	}
	return s
}

// Close stops everything.
func (h *Host) Close() error {
	if h.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		h.srv.Shutdown(ctx)
		cancel()
	}
	if h.Svc != nil {
		h.Svc.Close()
	}
	if h.Public != nil {
		h.Public.Close()
	}
	if h.Private != nil {
		h.Private.Close()
	}
	if h.Store != nil {
		h.Store.Close()
	}
	return nil
}
