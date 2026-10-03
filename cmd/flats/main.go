// Command flats hosts agent-built websites on this machine.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/gosuda/flats/internal/app"
	"github.com/gosuda/flats/internal/cli"
	"github.com/gosuda/flats/internal/runtime"
)

func init() {
	cli.Serve = app.Serve
	cli.Worker = runtime.WorkerMain
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
