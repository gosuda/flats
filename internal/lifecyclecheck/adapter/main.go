// Command lifecyclecheck is a test-only HTTP host for deterministic provider
// faults. It never connects to a relay or tailnet; every listener is loopback.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/cli"
	"github.com/gosuda/flats/internal/console"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/mcpx"
	"github.com/gosuda/flats/internal/runtime"
	"github.com/gosuda/flats/internal/store"
)

type faults struct {
	mu                                               sync.Mutex
	stop, serve                                      bool
	portalStop, funnelStop, portalServe, funnelServe bool
	async, ready                                     bool
	calls                                            map[string]int
}

func (f *faults) called(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
}

type publicNet struct {
	*local.Public
	faults *faults
}

func (p *publicNet) Stop(slug string) error {
	p.faults.called("portal_stop")
	p.faults.mu.Lock()
	fail := p.faults.stop || p.faults.portalStop
	p.faults.mu.Unlock()
	if fail {
		return errors.New("deterministic public teardown failure: route remains reachable")
	}
	return p.Public.Stop(slug)
}

func (p *publicNet) Serve(ctx context.Context, slug string, handler http.Handler, hidden bool) (string, error) {
	p.faults.called("portal_serve")
	p.faults.mu.Lock()
	fail := p.faults.serve || p.faults.portalServe
	p.faults.mu.Unlock()
	if fail {
		return "", errors.New("deterministic provider connection failure")
	}
	url, err := p.Public.Serve(ctx, slug, handler, hidden)
	p.faults.mu.Lock()
	connecting := p.faults.async && !p.faults.ready
	p.faults.mu.Unlock()
	if connecting {
		url = ""
	}
	return url, err
}

func (p *publicNet) Status() core.NetStatus {
	st := p.Public.Status()
	p.faults.mu.Lock()
	connecting := p.faults.async && !p.faults.ready
	p.faults.mu.Unlock()
	if connecting {
		for i := range st.Hosts {
			st.Hosts[i].State = "starting"
			st.Hosts[i].URL = ""
		}
	}
	return st
}
func (p *publicNet) URL(host string) string {
	p.faults.mu.Lock()
	connecting := p.faults.async && !p.faults.ready
	p.faults.mu.Unlock()
	if connecting {
		return ""
	}
	return p.Public.URL(host)
}

// tailnet doubles only the backend: policy remains in provider.Manager.
type tailnet struct {
	*local.Net
	public *local.Net
	faults *faults
	mu     sync.Mutex
	funnel map[string]string
}

func (t *tailnet) ServeFunnel(ctx context.Context, host string, handler http.Handler) (string, error) {
	t.faults.called("funnel_serve")
	t.faults.mu.Lock()
	fail := t.faults.serve || t.faults.funnelServe
	t.faults.mu.Unlock()
	if fail {
		return "", errors.New("deterministic provider connection failure")
	}
	url, err := t.public.Serve(ctx, "funnel-"+host, handler, false)
	if err == nil {
		t.mu.Lock()
		t.funnel[host] = url
		t.mu.Unlock()
	}
	return url, err
}

func (t *tailnet) StopFunnel(host string) error {
	t.faults.called("funnel_stop")
	t.faults.mu.Lock()
	fail := t.faults.stop || t.faults.funnelStop
	t.faults.mu.Unlock()
	if fail {
		return errors.New("deterministic public teardown failure: route remains reachable")
	}
	if err := t.public.Stop("funnel-" + host); err != nil {
		return err
	}
	t.mu.Lock()
	delete(t.funnel, host)
	t.mu.Unlock()
	return nil
}

func (t *tailnet) FunnelState(host string) core.ExposureEndpoint {
	t.mu.Lock()
	defer t.mu.Unlock()
	if url := t.funnel[host]; url != "" {
		t.faults.mu.Lock()
		connecting := t.faults.async && !t.faults.ready
		t.faults.mu.Unlock()
		if connecting {
			return core.ExposureEndpoint{Provider: core.ProviderFunnel, State: "starting"}
		}
		return core.ExposureEndpoint{Provider: core.ProviderFunnel, URL: url, State: "ready"}
	}
	return core.ExposureEndpoint{Provider: core.ProviderFunnel, State: "unavailable"}
}

// Private Tailscale has a distinct listener and namespace from Local. Tests
// can therefore detect incorrect backend selection and DTO URLs.
func (t *tailnet) Serve(ctx context.Context, host string, h http.Handler, ephemeral bool) (string, error) {
	t.faults.called("tailscale_serve")
	return t.Net.Serve(ctx, "tailnet-"+host, h, ephemeral)
}
func (t *tailnet) URL(host string) string { return t.Net.URL("tailnet-" + host) }
func (t *tailnet) Stop(host string) error { return t.Net.Stop("tailnet-" + host) }
func (t *tailnet) Status() core.NetStatus {
	st := t.Net.Status()
	st.Kind = "tailscale"
	for i := range st.Hosts {
		st.Hosts[i].Host = strings.TrimPrefix(st.Hosts[i].Host, "tailnet-")
	}
	return st
}

// managerSystem exposes concrete host configuration and endpoint DTOs even in
// the loopback fault lane; it never invents production relay or ACL readiness.
type managerSystem struct {
	manager *provider.Manager
	private *local.Net
}

func (s managerSystem) Status(context.Context) any {
	return map[string]any{"network": "local", "private": s.private.Status(),
		"providers": s.manager.HostStatus(), "loopback_provider_double": true}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	data := fs.String("data", "", "disposable data directory")
	listen := fs.String("listen", "127.0.0.1:0", "loopback management listener")
	localAddr := fs.String("local-addr", "127.0.0.1:0", "loopback flat listener")
	fs.String("network", "local", "compatibility flag; only loopback is used")
	fs.Bool("portal", false, "compatibility flag; no real Portal is used")
	credentialInput := fs.Bool("operator-credential-stdin", false, "read disposable operator authority from out-of-band stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for _, addr := range []string{*listen, *localAddr} {
		host, _, err := net.SplitHostPort(addr)
		if err != nil || host != "127.0.0.1" {
			return fmt.Errorf("test adapter requires literal 127.0.0.1 listeners: %q", addr)
		}
	}
	if *data == "" {
		return errors.New("explicit disposable --data required")
	}
	var authority *api.OperatorAuthority
	if *credentialInput {
		credential, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return errors.New("could not read test operator authority")
		}
		authority, err = api.NewOperatorAuthority(strings.TrimRight(credential, "\r\n"))
		credential = ""
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(*data, 0o700); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := store.Open(filepath.Join(*data, "flats.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	private, err := local.Listen(*localAddr)
	if err != nil {
		return err
	}
	defer private.Close()
	fault := &faults{calls: map[string]int{}}
	public := &publicNet{Public: local.NewPublic(private), faults: fault}
	tailPrivate, err := local.Listen("127.0.0.1:0")
	if err != nil {
		return err
	}
	defer tailPrivate.Close()
	ts := &tailnet{Net: tailPrivate, public: private, faults: fault, funnel: map[string]string{}}
	manager, err := provider.New(*data, provider.Options{Local: private, Tailscale: ts, Portal: public, Permission: func(ctx context.Context, slug string, id provider.ID) (bool, error) {
		return st.ProviderPermitted(ctx, slug, string(id))
	}})
	if err != nil {
		return err
	}
	defer manager.Close()
	// Public is deliberately nil: the legacy adapter must never satisfy this lane.
	var clockOffset atomic.Int64
	cfg := core.Config{Now: func() time.Time { return time.Now().Add(time.Duration(clockOffset.Load())).UTC() }, DataDir: *data, Store: st, Private: private, Lifecycle: manager,
		Runtime: &runtime.Manager{DataDir: *data}, Reserved: []string{"flats"}, Logf: log.Printf,
		ConsoleURL: func() string { return "http://" + *listen }}
	if authority != nil {
		cfg.ValidateOperatorDecision = authority.ValidateDecision
		cfg.OperatorIdentity = authority.DecisionIdentity
	}
	svc, err := core.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer svc.Close()
	apiServer := &api.Server{Svc: svc, Operator: authority, System: managerSystem{manager, private}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__gate/capabilities", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"production_provider_manager":true,"legacy_public_fallback":false}`))
	})
	mux.HandleFunc("POST /__gate/host-permission", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Permitted []core.ProviderID `json:"permitted"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		file := manager.File()
		file.Permitted = in.Permitted
		if err := provider.Save(*data, file); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := manager.Reload(); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("GET /__gate/state", func(w http.ResponseWriter, r *http.Request) {
		fault.mu.Lock()
		defer fault.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"calls": fault.calls, "host_permission": manager.File().Permitted})
	})
	mux.HandleFunc("POST /__gate/expire-redirects", func(w http.ResponseWriter, r *http.Request) {
		clockOffset.Add(int64(8 * 24 * time.Hour))
		svc.Sweep(r.Context())
		w.Write([]byte(`{"ok":true}`))
	})
	mux.Handle("/api/", apiServer.Handler())
	mux.Handle("/console/api/", apiServer.Handler())
	mux.Handle("/mcp", mcpx.Handler(svc, mcpx.Options{}))
	mux.Handle("/", console.Handler())
	// Fault control belongs only to this test binary. It cannot approve or
	// alter product state and is deliberately absent from the real binary.
	mux.HandleFunc("POST /__gate/fault", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Async       bool `json:"async"`
			Ready       bool `json:"ready"`
			Stop        bool `json:"stop"`
			Serve       bool `json:"serve"`
			PortalStop  bool `json:"portal_stop"`
			FunnelStop  bool `json:"funnel_stop"`
			PortalServe bool `json:"portal_serve"`
			FunnelServe bool `json:"funnel_serve"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		fault.mu.Lock()
		fault.async, fault.ready = in.Async, in.Ready
		fault.stop, fault.serve = in.Stop, in.Serve
		fault.portalStop, fault.funnelStop = in.PortalStop, in.FunnelStop
		fault.portalServe, fault.funnelServe = in.PortalServe, in.FunnelServe
		fault.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	})
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	guard := &api.HostGuard{Hosts: func() []string { return []string{"127.0.0.1", "localhost", "::1"} },
		Ports: []string{fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)}, Next: mux}
	srv := &http.Server{Handler: guard, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(closeCtx)
	case err := <-done:
		return err
	}
}

func main() {
	cli.Serve = serve
	cli.Worker = runtime.WorkerMain
	os.Exit(cli.Run(context.Background(), os.Args[1:], cli.OSEnv()))
}
