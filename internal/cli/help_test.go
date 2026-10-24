package cli

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestCommandHelpDescribesTheCommand(t *testing.T) {
	for _, tc := range []struct {
		name   string
		usage  string
		detail string
	}{
		{"connect", "usage: flats connect [<url>] [--clear]", "It does not register an agent."},
		{"mcp", "usage: flats mcp [--url url]", "Claude Code, Codex and Cursor runs this command"},
		{"mcp-config", "usage: flats mcp-config [--url url]", "as JSON, without the plugin commands."},
		{"client", "usage: flats client check [--skill-file path]", "The check changes nothing."},
	} {
		r := run(t, "", "", "help", tc.name)
		if r.code != 0 || !strings.HasPrefix(r.stdout, tc.usage+"\n\n") || !strings.Contains(r.stdout, tc.detail) {
			t.Errorf("help %s: %d\n%s", tc.name, r.code, r.stdout)
		}
	}

	// A command without a longer description shows its synopsis.
	r := run(t, "", "", "help", "list")
	if r.code != 0 || r.stdout != "usage: flats list\n\nList flats\n" {
		t.Errorf("help list: %d %q", r.code, r.stdout)
	}
}

func TestCommandHelpFlagsFollowTheDescription(t *testing.T) {
	r := run(t, "", "", "connect", "-h")
	if r.code != 0 {
		t.Fatalf("connect -h: %d %s", r.code, r.stderr)
	}
	usage := strings.Index(r.stdout, "usage: flats connect")
	about := strings.Index(r.stdout, "It does not register an agent.")
	flags := strings.Index(r.stdout, "\n  -clear\n")
	if usage != 0 || about < usage || flags < about {
		t.Errorf("order usage=%d description=%d flags=%d\n%s", usage, about, flags, r.stdout)
	}
	// The argument is named URL, not by the backticked words of the usage text.
	if !strings.Contains(r.stdout, "-url URL") || strings.Contains(r.stdout, "-url flats connect") {
		t.Errorf("--url is not named URL:\n%s", r.stdout)
	}
}

func TestUsagePointsToAgentSetup(t *testing.T) {
	r := run(t, "", "", "help")
	if r.code != 0 {
		t.Fatalf("help: %d %s", r.code, r.stderr)
	}
	for _, want := range []string{
		"(to connect an agent, see mcp-config)",
		"`flats status` checks the host. `flats mcp-config` shows how to connect an agent.\nRun `flats help <command>` for details. Exit codes: 0 ok, 1 error, 2 usage, 3 pending approval.\n",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("help is missing %q:\n%s", want, r.stdout)
		}
	}
	// The pointer comes after the command list and before the exit codes, and
	// the exit codes stay last.
	if !strings.HasSuffix(r.stdout, "Exit codes: 0 ok, 1 error, 2 usage, 3 pending approval.\n") || strings.Index(r.stdout, "`flats status` checks the host.") < strings.Index(r.stdout, "worker      internal") {
		t.Errorf("pointer is misplaced:\n%s", r.stdout)
	}
}

func TestMCPConfigNamesPluginInstall(t *testing.T) {
	r := run(t, "", "", "mcp-config")
	if r.code != 0 {
		t.Fatal(r.stderr)
	}
	for _, want := range []string{
		"claude plugin marketplace add gosuda/flats && claude plugin install flats@flats",
		"codex plugin marketplace add gosuda/flats && codex plugin add flats@flats",
		"docs/agents.md#plugin",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("mcp-config is missing %q:\n%s", want, r.stdout)
		}
	}
	// JSON output keeps its shape: the same keys as before the plugin text, and
	// no plugin commands.
	j := run(t, "", "", "mcp-config", "--json")
	if j.code != 0 || strings.Contains(j.stdout, "plugin") {
		t.Fatalf("mcp-config --json: %d\n%s", j.code, j.stdout)
	}
	var v map[string]json.RawMessage
	if err := json.Unmarshal([]byte(j.stdout), &v); err != nil {
		t.Fatalf("mcp-config --json: %v\n%s", err, j.stdout)
	}
	keys := func(raw []byte) []string {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("mcp-config --json: %v\n%s", err, raw)
		}
		return slices.Sorted(maps.Keys(m))
	}
	for name, want := range map[string][]string{
		"":       {"claude", "codex", "cursor", "url"},
		"claude": {"command"},
		"codex":  {"command", "file", "toml"},
		"cursor": {"file", "json"},
	} {
		raw := []byte(j.stdout)
		if name != "" {
			raw = v[name]
		}
		if got := keys(raw); !slices.Equal(got, want) {
			t.Errorf("mcp-config --json %q keys = %v, want %v", name, got, want)
		}
	}
}
