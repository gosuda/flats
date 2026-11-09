package prompt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/gosuda/flats/internal/ostest"
)

// A hidden question leaves echo on however it ends: interrupted before it
// is shown, interrupted while it waits, or answered.
func TestHiddenLeavesEchoOn(t *testing.T) {
	_, pts := ostest.PTY(t)
	if !ostest.Echoing(t, pts) {
		t.Fatal("a new terminal does not echo")
	}
	early, cancel := context.WithCancel(context.Background())
	cancel()
	for range 50 {
		if _, err := Hidden(early, pts, io.Discard, "Recovery password: "); !errors.Is(err, context.Canceled) {
			t.Fatalf("early interrupt: %v", err)
		}
		if !ostest.Echoing(t, pts) {
			t.Fatal("echo is off after an early interrupt")
		}
	}

	// Each case below leaves a read blocked on its terminal, so each has its
	// own.
	_, pts = ostest.PTY(t)
	ctx, cancel := context.WithCancel(context.Background())
	shown := make(ostest.Prompts, 4)
	done := make(chan error, 1)
	go func() {
		_, err := Hidden(ctx, pts, shown, "Recovery password: ")
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

	ptm, pts := ostest.PTY(t)
	shown = make(ostest.Prompts, 4)
	type reply struct {
		s   string
		err error
	}
	answer := make(chan reply, 1)
	go func() {
		s, err := Hidden(context.Background(), pts, shown, "Recovery password: ")
		answer <- reply{s, err}
	}()
	<-shown
	if ostest.Echoing(t, pts) {
		t.Fatal("echo is on at the prompt")
	}
	if _, err := ptm.WriteString("a long recovery password\n"); err != nil {
		t.Fatal(err)
	}
	if r := <-answer; r.err != nil || r.s != "a long recovery password" {
		t.Fatalf("answer %q %v", r.s, r.err)
	}
	if !ostest.Echoing(t, pts) {
		t.Fatal("echo is off after the answer")
	}
}

// A plain question keeps echo on and returns the line with its end.
func TestLineAnswered(t *testing.T) {
	ptm, pts := ostest.PTY(t)
	if _, err := ptm.WriteString("reset\n"); err != nil {
		t.Fatal(err)
	}
	if s, err := Line(context.Background(), pts, io.Discard, "Type reset to delete them: "); err != nil || s != "reset\n" {
		t.Fatalf("answer %q %v", s, err)
	}
	if !ostest.Echoing(t, pts) {
		t.Fatal("echo is off after a plain question")
	}
	// An interrupt before the question shows nothing.
	early, cancel := context.WithCancel(context.Background())
	cancel()
	var shown bytes.Buffer
	if _, err := Line(early, pts, &shown, "Type reset to delete them: "); !errors.Is(err, context.Canceled) || shown.Len() != 0 {
		t.Fatalf("early interrupt: %v, shown %q", err, shown.String())
	}
}
