package cli

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

// `flats backup` reaches the wired command with its arguments and without the
// host, takes --json out of the arguments, and keeps the exit status of the
// command: 2 for a usage mistake, 78 for a refusal, 1 for another failure.
func TestBackupIsWiredAndKeepsItsExitStatus(t *testing.T) {
	old := Backup
	t.Cleanup(func() { Backup = old })
	var got []string
	var reply error
	Backup = func(_ context.Context, args []string, out io.Writer) error {
		got = args
		io.WriteString(out, "done\n")
		return reply
	}

	r := run(t, "http://127.0.0.1:1", "", "backup", "--out", "/c", "--json", "--exclude-secret-key")
	if r.code != 0 || !slices.Equal(got, []string{"--out", "/c", "--exclude-secret-key"}) || !strings.Contains(r.stdout, "done") {
		t.Fatalf("code %d, args %q, out %q", r.code, got, r.stdout)
	}

	for name, tc := range map[string]struct {
		err  error
		code int
		text string
	}{
		"a refusal":       {exit78{errors.New("refused: /c exists")}, ExitConfig, "refused: /c exists"},
		"a failure":       {errors.New("the copy failed its own check; run the backup again"), 1, "run the backup again"},
		"a usage mistake": {exit2{errors.New("say --out PATH")}, ExitUsage, "usage: flats backup"},
	} {
		reply = tc.err
		if r := run(t, "http://127.0.0.1:1", "", "backup", "--out", "/c"); r.code != tc.code || !strings.Contains(r.stderr, tc.text) {
			t.Errorf("%s: code %d, stderr %q; want code %d and %q", name, r.code, r.stderr, tc.code, tc.text)
		}
	}
}

func TestBackupIsListedInTheHelp(t *testing.T) {
	r := run(t, "http://127.0.0.1:1", "", "help")
	if !strings.Contains(r.stdout, "backup") {
		t.Errorf("flats help does not list backup:\n%s", r.stdout)
	}
	r = run(t, "http://127.0.0.1:1", "", "help", "backup")
	if !strings.Contains(r.stdout, "backup --out path") || !strings.Contains(r.stdout, "backup --verify path") {
		t.Errorf("flats help backup = %q", r.stdout)
	}
}
