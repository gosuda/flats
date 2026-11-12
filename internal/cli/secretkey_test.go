package cli

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

type exit78 struct{ error }

func (exit78) ExitCode() int { return ExitConfig }

type exit2 struct{ error }

func (exit2) ExitCode() int { return ExitUsage }

// The subcommands that work on secret.key reach the wired command with their
// arguments, never the host, and keep its exit status.
func TestSecretKeySubcommandsAreWired(t *testing.T) {
	old := SecretKey
	t.Cleanup(func() { SecretKey = old })
	var got []string
	var reply error
	SecretKey = func(_ context.Context, args []string, in io.Reader, out io.Writer) error {
		got = args
		io.WriteString(out, "done\n")
		return reply
	}
	for _, sub := range []string{"backup-key", "restore-key", "reset"} {
		got, reply = nil, nil
		r := run(t, "http://127.0.0.1:1", "", "secret", sub, "--data", "/d")
		if r.code != 0 || !slices.Equal(got, []string{sub, "--data", "/d"}) || !strings.Contains(r.stdout, "done") {
			t.Fatalf("%s: code %d, args %q, out %q", sub, r.code, got, r.stdout)
		}
	}
	reply = exit78{errors.New("refused")}
	if r := run(t, "http://127.0.0.1:1", "", "secret", "reset"); r.code != ExitConfig || !strings.Contains(r.stderr, "refused") {
		t.Fatalf("refusal: %d %q", r.code, r.stderr)
	}
	reply = exit2{errors.New("bad flag")}
	if r := run(t, "http://127.0.0.1:1", "", "secret", "reset"); r.code != ExitUsage || !strings.Contains(r.stderr, "usage: flats secret") || !strings.Contains(r.stderr, "flats secret restore-key") {
		t.Fatalf("usage error: %d %q", r.code, r.stderr)
	}
}
