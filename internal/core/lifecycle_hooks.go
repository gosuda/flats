//go:build !lifecycle_testhooks

package core

import "context"

func lifecyclePhase(context.Context, string, string, string) {}
