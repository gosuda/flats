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

// `flats audit export` reaches the same wired command with its arguments, prints what the command prints and nothing
// else on the standard output, and keeps the exit status of the command, as `flats audit ls` does.
func TestAuditExportIsWiredAndKeepsItsExitStatus(t *testing.T) {
	old := Audit
	t.Cleanup(func() { Audit = old })
	var got []string
	var reply error
	const line = `{"record":"audit","id":1}` + "\n"
	Audit = func(_ context.Context, args []string, out io.Writer) error {
		got = args
		if reply == nil {
			io.WriteString(out, line)
		}
		return reply
	}

	r := run(t, "http://127.0.0.1:1", "", "audit", "export", "--since", "1h")
	if r.code != 0 || !slices.Equal(got, []string{"export", "--since", "1h"}) || r.stdout != line || r.stderr != "" {
		t.Fatalf("code %d, args %q, stdout %q, stderr %q", r.code, got, r.stdout, r.stderr)
	}

	for name, tc := range map[string]struct {
		err  error
		code int
		text string
	}{
		"a refusal":       {exit78{errors.New("/d has no flats.db")}, ExitConfig, "/d has no flats.db"},
		"a failure":       {errors.New("write the export: broken pipe"), 1, "broken pipe"},
		"a usage mistake": {exit2{errors.New(`--since: "soon" is neither an RFC 3339 time nor a duration such as 24h`)}, ExitUsage, "flats audit export [--since time]"},
	} {
		reply = tc.err
		r := run(t, "http://127.0.0.1:1", "", "audit", "export", "--since", "soon")
		if r.code != tc.code || !strings.Contains(r.stderr, tc.text) || r.stdout != "" {
			t.Errorf("%s: code %d, stdout %q, stderr %q; want code %d, %q on the standard error and no standard output", name, r.code, r.stdout, r.stderr, tc.code, tc.text)
		}
	}
}

func TestAuditExportIsListedInTheHelp(t *testing.T) {
	r := run(t, "http://127.0.0.1:1", "", "help", "audit")
	for _, want := range []string{"usage: flats audit ls ", "       flats audit export [--since time] [--config path | --data dir]\n", "export"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("flats help audit lacks %q:\n%s", want, r.stdout)
		}
	}
	// The line of the command in the list of `flats help` says that it exports too.
	r = run(t, "http://127.0.0.1:1", "", "help")
	listed := false
	for _, l := range strings.Split(r.stdout, "\n") {
		if strings.HasPrefix(l, "  audit ") && strings.Contains(l, "export") {
			listed = true
		}
	}
	if !listed {
		t.Errorf("the audit line of flats help does not name the export:\n%s", r.stdout)
	}
}
