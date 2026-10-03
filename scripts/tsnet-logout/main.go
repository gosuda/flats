// Command tsnet-logout logs a stopped tsnet node out of its tailnet, given
// the node's state directory (for example <data>/tsnet/<host>). The gate
// scripts use it to remove nodes Flats keeps across restarts, such as the
// console node, so a test run leaves no devices behind.
//
// Usage: go run ./scripts/tsnet-logout <state-dir>...
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	ts "tailscale.com/tsnet"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: tsnet-logout <state-dir>...")
		os.Exit(2)
	}
	failed := false
	for _, dir := range os.Args[1:] {
		if err := logout(dir); err != nil {
			log.Printf("%s: %v", dir, err)
			failed = true
		} else {
			fmt.Printf("logged out %s\n", filepath.Base(dir))
		}
	}
	if failed {
		os.Exit(1)
	}
}

func logout(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "tailscaled.state")); err != nil {
		return fmt.Errorf("no node state: %w", err)
	}
	srv := &ts.Server{Dir: dir, Hostname: filepath.Base(dir), Logf: func(string, ...any) {}, UserLogf: log.New(io.Discard, "", 0).Printf}
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := srv.Up(ctx); err != nil {
		return fmt.Errorf("start node: %w", err)
	}
	lc, err := srv.LocalClient()
	if err != nil {
		return err
	}
	return lc.Logout(ctx)
}
