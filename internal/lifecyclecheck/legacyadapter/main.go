// Command legacyadapter seeds historical exposure and approval references.
// Build only against the archived pre-lifecycle source. Its PublicNet is a
// loopback test double, never a production provider or acceptance lane.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/cli"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/mcpx"
	"github.com/gosuda/flats/internal/runtime"
	"github.com/gosuda/flats/internal/store"
)

type faults struct {
	mu          sync.Mutex
	stop, serve bool
}

type publicNet struct {
	*local.Public
	faults *faults
}

func (p *publicNet) Stop(slug string) error {
	p.faults.mu.Lock()
	fail := p.faults.stop
	p.faults.mu.Unlock()
	if fail {
		return errors.New("deterministic public teardown failure: route remains reachable")
	}
	return p.Public.Stop(slug)
}

func (p *publicNet) Serve(ctx context.Context, slug string, handler http.Handler, hidden bool) (string, error) {
	p.faults.mu.Lock()
	fail := p.faults.serve
	p.faults.mu.Unlock()
	if fail {
		return "", errors.New("deterministic provider connection failure")
	}
	return p.Public.Serve(ctx, slug, handler, hidden)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	data := fs.String("data", "", "disposable data directory")
	listen := fs.String("listen", "127.0.0.1:0", "loopback management listener")
	localAddr := fs.String("local-addr", "127.0.0.1:0", "loopback flat listener")
	fs.String("network", "local", "compatibility flag; only loopback is used")
	fs.Bool("portal", false, "compatibility flag; no real Portal is used")
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
	fault := &faults{}
	public := &publicNet{Public: local.NewPublic(private), faults: fault}
	svc, err := core.New(ctx, core.Config{DataDir: *data, Store: st, Private: private, Public: public,
		Runtime: &runtime.Manager{DataDir: *data}, ConsoleURL: func() string { return "http://" + *listen }})
	if err != nil {
		return err
	}
	defer svc.Close()
	apiServer := &api.Server{Svc: svc}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__gate/capabilities", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Fail closed until the exact core-defined LifecycleNet is adopted.
		w.Write([]byte(`{"production_provider_manager":false,"legacy_public_fallback":true}`))
	})
	mux.Handle("/api/", apiServer.Handler())
	mux.Handle("/console/api/", apiServer.Handler())
	mux.Handle("/mcp", mcpx.Handler(svc, mcpx.Options{}))
	// Fault control belongs only to this test binary. It cannot approve or
	// alter product state and is deliberately absent from the real binary.
	mux.HandleFunc("POST /__gate/fault", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Stop  bool `json:"stop"`
			Serve bool `json:"serve"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		fault.mu.Lock()
		fault.stop, fault.serve = in.Stop, in.Serve
		fault.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	})
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
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
