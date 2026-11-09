package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/ostest"
)

// An interrupt that ends a question on the terminal says that nothing was
// written, and leaves echo on.
func TestTerminalInterruptWritesNothing(t *testing.T) {
	_, pts := ostest.PTY(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &terminal{ctx: ctx, f: pts, out: io.Discard}
	for name, ask := range map[string]func(string) (string, error){"hidden": p.hidden, "line": p.line} {
		if _, err := ask("Recovery password: "); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "nothing was written") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !ostest.Echoing(t, pts) {
		t.Fatal("echo is off after an interrupt")
	}
}
