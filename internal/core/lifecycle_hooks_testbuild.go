//go:build lifecycle_testhooks

package core

import (
	"context"
	"encoding/json"
	"os"
)

// lifecyclePhase exists only in explicitly built test artifacts. It pauses once
// per marker path so a killed process can restart without pausing again.
func lifecyclePhase(ctx context.Context, phase, flat, approval string) {
	if os.Getenv("FLATS_TEST_PHASE") != phase {
		return
	}
	path := os.Getenv("FLATS_TEST_PHASE_MARKER")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	_ = json.NewEncoder(f).Encode(map[string]string{"phase": phase, "flat": flat, "approval": approval})
	_ = f.Close()
	<-ctx.Done()
}
