package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/launchd"
	"github.com/gosuda/flats/internal/systemd"
)

// uninstallAgentLine is the last line of every text outcome of `flats
// uninstall`. It is spelled out here so that a change to the product string
// fails these tests.
const uninstallAgentLine = "Agent files are left as they are. `flats agent-setup docs --undo` reverts a setup made with `flats agent-setup docs`.\n"

// uninstallEnv returns an Env whose service manager is faked and whose service
// definition exists only when installed is true. Nothing here reaches the real
// launchd, systemd or home directory.
func uninstallEnv(t *testing.T, goos string, installed bool) Env {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", "")
	home := t.TempDir()
	if installed {
		definition := launchd.PlistPath(home)
		if goos == "linux" {
			definition = systemd.UnitPath(home)
		}
		if err := os.MkdirAll(filepath.Dir(definition), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(definition, []byte("service definition\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fl := &fakeLaunchctl{loaded: installed && goos == "darwin", active: installed && goos == "linux"}
	return Env{Home: home, GOOS: goos, Launchd: fl.run}
}

func TestUninstallTextEndsWithAgentFileLine(t *testing.T) {
	for _, tc := range []struct {
		name      string
		goos      string
		installed bool
		message   string
	}{
		{"launchd removed", "darwin", true, fmt.Sprintf("Removed launchd agent %s. Your flats and data are kept.\n", launchd.Label)},
		{"launchd not installed", "darwin", false, "The launchd agent was not installed.\n"},
		{"systemd removed", "linux", true, fmt.Sprintf("Removed systemd user service %s. Your flats and data are kept.\n", systemd.Unit)},
		{"systemd not installed", "linux", false, "The systemd user service was not installed.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, srv := newFakeAPI(t)
			r := runEnv(t, uninstallEnv(t, tc.goos, tc.installed), srv.URL, "uninstall")
			if r.code != ExitOK || r.stderr != "" {
				t.Fatalf("uninstall: %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
			}
			if want := tc.message + uninstallAgentLine; r.stdout != want {
				t.Errorf("stdout = %q, want %q", r.stdout, want)
			}
		})
	}
}

// The macOS --json branch is a machine-readable reply. It carries no prose.
func TestUninstallJSONHasNoAgentFileLine(t *testing.T) {
	for _, installed := range []bool{true, false} {
		t.Run(fmt.Sprintf("removed=%t", installed), func(t *testing.T) {
			_, srv := newFakeAPI(t)
			for _, args := range [][]string{{"--json", "uninstall"}, {"uninstall", "--json"}} {
				r := runEnv(t, uninstallEnv(t, "darwin", installed), srv.URL, args...)
				if r.code != ExitOK || r.stderr != "" {
					t.Fatalf("%v: %d\nstdout: %s\nstderr: %s", args, r.code, r.stdout, r.stderr)
				}
				if strings.Contains(r.stdout, "Agent files") || strings.Contains(r.stdout, "agent-setup") {
					t.Errorf("%v: --json output has the agent-file line:\n%s", args, r.stdout)
				}
				var got map[string]any
				if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
					t.Fatalf("%v: output is not one JSON value: %v\n%s", args, err, r.stdout)
				}
				if want := map[string]any{"removed": installed}; !reflect.DeepEqual(got, want) {
					t.Errorf("%v: JSON = %v, want %v", args, got, want)
				}
			}
		})
	}
}
