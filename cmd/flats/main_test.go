package main

import (
	"testing"

	"github.com/gosuda/flats/internal/cli"
)

// The commands that work on the data directory are wired in init: a command whose hook is nil answers "not wired".
func TestTheCommandsOfTheDataDirectoryAreWired(t *testing.T) {
	for name, wired := range map[string]bool{
		"audit": cli.Audit != nil, "backup": cli.Backup != nil, "config": cli.Config != nil, "key": cli.Key != nil, "secret": cli.SecretKey != nil,
	} {
		if !wired {
			t.Errorf("flats %s is not wired in cmd/flats", name)
		}
	}
}
