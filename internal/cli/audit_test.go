package cli

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

// `flats audit ls` reaches the wired command with its arguments and keeps the exit status of the command: 2 for a usage mistake,
// 78 for a refusal, 1 for another failure.
func TestAuditIsWiredAndKeepsItsExitStatus(t *testing.T) {
	old := Audit
	t.Cleanup(func() { Audit = old })
	var got []string
	var reply error
	Audit = func(_ context.Context, args []string, out io.Writer) error {
		got = args
		io.WriteString(out, "listed\n")
		return reply
	}

	r := run(t, "http://127.0.0.1:1", "", "audit", "ls", "--limit", "3", "--kind", "key.set")
	if r.code != 0 || !slices.Equal(got, []string{"ls", "--limit", "3", "--kind", "key.set"}) || !strings.Contains(r.stdout, "listed") {
		t.Fatalf("code %d, args %q, out %q", r.code, got, r.stdout)
	}

	for name, tc := range map[string]struct {
		err  error
		code int
		text string
	}{
		"a refusal":       {exit78{errors.New("/d has no flats.db")}, ExitConfig, "/d has no flats.db"},
		"a failure":       {errors.New("read the log: disk error"), 1, "disk error"},
		"a usage mistake": {exit2{errors.New("--limit must be 0 or more")}, ExitUsage, "usage: flats audit"},
	} {
		reply = tc.err
		if r := run(t, "http://127.0.0.1:1", "", "audit", "ls", "--limit", "-1"); r.code != tc.code || !strings.Contains(r.stderr, tc.text) {
			t.Errorf("%s: code %d, stderr %q; want code %d and %q", name, r.code, r.stderr, tc.code, tc.text)
		}
	}
}

func TestAuditIsListedInTheHelp(t *testing.T) {
	r := run(t, "http://127.0.0.1:1", "", "help")
	if !strings.Contains(r.stdout, "audit") {
		t.Errorf("flats help does not list audit:\n%s", r.stdout)
	}
	r = run(t, "http://127.0.0.1:1", "", "help", "audit")
	if !strings.Contains(r.stdout, "audit ls") || !strings.Contains(r.stdout, "--flat-id") {
		t.Errorf("flats help audit = %q", r.stdout)
	}
}
