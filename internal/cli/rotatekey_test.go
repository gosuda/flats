package cli

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

// docs/spec/secret-key-rotation.md: `flats secret rotate-key` works on the data directory. It reaches the wired
// command with its arguments, never the host, and keeps the exit status of the command: 2 for a usage mistake, 78
// for a refusal, 1 for another failure.
func TestRotateKeyIsWiredAndKeepsItsExitStatus(t *testing.T) {
	old := SecretKey
	t.Cleanup(func() { SecretKey = old })
	var got []string
	var reply error
	SecretKey = func(_ context.Context, args []string, in io.Reader, out io.Writer) error {
		got = args
		io.WriteString(out, "sealed 2 flat secrets and 1 operator key again\n")
		return reply
	}
	// The address is one that nothing listens on: a command that called the host would fail.
	r := run(t, "http://127.0.0.1:1", "", "secret", "rotate-key", "--out", "/f/new.key", "--password-file", "/p", "--yes", "--data", "/d")
	want := []string{"rotate-key", "--out", "/f/new.key", "--password-file", "/p", "--yes", "--data", "/d"}
	if r.code != 0 || !slices.Equal(got, want) || !strings.Contains(r.stdout, "sealed 2 flat secrets") {
		t.Fatalf("code %d, args %q, out %q", r.code, got, r.stdout)
	}
	for name, tc := range map[string]struct {
		err  error
		code int
		text string
	}{
		"a refusal":       {exit78{errors.New("stop the running Flats host first")}, ExitConfig, "stop the running Flats host first"},
		"a failure":       {errors.New("the re-seal failed: disk error"), 1, "the re-seal failed"},
		"a usage mistake": {exit2{errors.New("no terminal to confirm on: run again with --yes to rotate the key")}, ExitUsage, "usage: flats secret"},
	} {
		reply = tc.err
		if r := run(t, "http://127.0.0.1:1", "", "secret", "rotate-key"); r.code != tc.code || !strings.Contains(r.stderr, tc.text) {
			t.Errorf("%s: code %d, stderr %q; want code %d and %q", name, r.code, r.stderr, tc.code, tc.text)
		}
	}
	// The usage that follows a usage mistake lists the command.
	reply = exit2{errors.New("unexpected argument \"more\"")}
	if r := run(t, "http://127.0.0.1:1", "", "secret", "rotate-key", "more"); r.code != ExitUsage || !strings.Contains(r.stderr, "flats secret rotate-key [--out file] [--password-file path] [--yes] [--config path | --data dir]") {
		t.Errorf("the usage after a usage mistake does not list rotate-key: %q", r.stderr)
	}
}

func TestRotateKeyIsListedInTheHelp(t *testing.T) {
	r := run(t, "http://127.0.0.1:1", "", "help", "secret")
	if want := "       flats secret rotate-key [--out file] [--password-file path] [--yes] [--config path | --data dir]\n"; !strings.Contains(r.stdout, want) {
		t.Errorf("flats help secret lacks %q:\n%s", want, r.stdout)
	}
	// Between restore-key and reset, as the docs list the key commands.
	if a, b, c := strings.Index(r.stdout, "secret restore-key"), strings.Index(r.stdout, "secret rotate-key"), strings.Index(r.stdout, "secret reset"); a < 0 || a > b || b > c {
		t.Errorf("the order of the key commands in flats help secret:\n%s", r.stdout)
	}
	// The two messages that name the subcommands name it too.
	for _, args := range [][]string{{"secret"}, {"secret", "rotate"}} {
		r := run(t, "http://127.0.0.1:1", "", args...)
		if r.code != ExitUsage || !strings.Contains(r.stderr, "restore-key, rotate-key or reset") {
			t.Errorf("flats %s: code %d, stderr %q", strings.Join(args, " "), r.code, r.stderr)
		}
	}
}
