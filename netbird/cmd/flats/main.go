// Command flats is the Flats host with NetBird linked in. It is the same
// program as ../../../cmd/flats plus the NetBird provider, built from its own
// module because NetBird's client needs replace directives in go.mod, which
// `go install pkg@version` refuses for the main module.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/gosuda/flats/internal/app"
	"github.com/gosuda/flats/internal/cli"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/runtime"
	"github.com/gosuda/flats/netbird"
)

func init() {
	provider.RegisterNetbird(netbird.Driver{})
	cli.Serve = app.Serve
	cli.Worker = runtime.WorkerMain
	cli.Config = app.ConfigCommand
	cli.ConfigUsage = app.ConfigUsage
	cli.EnsureConfig = app.EnsureConfig
}

func main() {
	args := os.Args[1:]
	// serve and worker install their own signal handling.
	if len(args) > 0 && (args[0] == "serve" || args[0] == "worker") {
		os.Exit(cli.Run(context.Background(), args, cli.OSEnv()))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, args, cli.OSEnv())
	stop()
	os.Exit(code)
}
