package cli

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/gosuda/flats/internal/ostest"
)

// `flats secret set` leaves echo on when an interrupt ends its question,
// before the question is shown or while it waits.
func TestReadSecretLeavesEchoOn(t *testing.T) {
	_, pts := ostest.PTY(t)
	early, cancel := context.WithCancel(context.Background())
	cancel()
	a := &app{env: Env{Stdin: pts}, ctx: early, out: io.Discard, errw: io.Discard}
	for range 50 {
		if _, err := a.readSecret("API_KEY"); !errors.Is(err, context.Canceled) {
			t.Fatalf("early interrupt: %v", err)
		}
		if !ostest.Echoing(t, pts) {
			t.Fatal("echo is off after an early interrupt")
		}
	}

	// The read of the first terminal stays blocked, so this case has its own.
	_, pts = ostest.PTY(t)
	ctx, cancel := context.WithCancel(context.Background())
	shown := make(ostest.Prompts, 4)
	a = &app{env: Env{Stdin: pts}, ctx: ctx, out: io.Discard, errw: shown}
	done := make(chan error, 1)
	go func() {
		_, err := a.readSecret("API_KEY")
		done <- err
	}()
	<-shown
	if ostest.Echoing(t, pts) {
		t.Fatal("echo is on at the prompt")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupt at the prompt: %v", err)
	}
	if !ostest.Echoing(t, pts) {
		t.Fatal("echo is off after an interrupt at the prompt")
	}
}
