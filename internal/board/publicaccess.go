package board

import (
	"context"
	"errors"
)

// Interface for anonymous public editing (docs/spec/board-public-edit-TECH.md).
// The bodies are placeholders that the board server unit replaces.

// PublicAccessHeader carries the flat's public access level ("view" or
// "edit") on Public requests. Core sets it; clients cannot.
const PublicAccessHeader = "X-Flats-Public-Access"

// Public access levels.
const (
	AccessNone = ""
	AccessView = "view"
	AccessEdit = "edit"
)

// KindSafePoint is the outbox and revision kind of a safe point.
const KindSafePoint = "safe_point"

var errNotImplemented = errors.New("board: not implemented")

// SetPublicAccess changes the instance's public access level (TECH 6.1).
// It returns after the apply loop has bumped the access epoch.
func (in *Instance) SetPublicAccess(ctx context.Context, level string) error {
	return errNotImplemented
}

// SafePoint closes open groups and records a forced safe point entry unless
// the board's file list equals prev (TECH 6.2). It returns the entry's outbox
// seq, or reuse when nothing was recorded because the content equals prev.
func (in *Instance) SafePoint(ctx context.Context, prev []File) (seq int64, reuse bool, err error) {
	return 0, false, errNotImplemented
}

// PublicPresence reports how many guests and viewers are connected now.
// It must not block.
func (in *Instance) PublicPresence() (guests, viewers int) { return 0, 0 }
