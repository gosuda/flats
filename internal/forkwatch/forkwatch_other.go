//go:build !darwin

// Package forkwatch unwedges child processes stuck between fork and exec; it
// is only needed on macOS.
package forkwatch

import "context"

// Start does nothing outside macOS.
func Start(context.Context, func(string, ...any)) {}
