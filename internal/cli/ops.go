package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/launchd"
	"github.com/gosuda/flats/internal/systemd"
)

// DefaultDataDir returns the data directory: FLATS_DATA, else the user
// config directory + Flats (~/Library/Application Support/Flats on macOS,
// $XDG_CONFIG_HOME/Flats or ~/.config/Flats on Linux), matching `flats serve`.
func DefaultDataDir(getenv func(string) string) (string, error) {
	if d := getenv("FLATS_DATA"); d != "" {
		return d, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "Flats"), nil
}

func (a *app) launchdOpts() launchd.Options {
	return launchd.Options{Home: a.env.Home, Run: a.env.Launchd}
}

func (a *app) requireMacOS() error {
	switch a.goos() {
	case "darwin", "linux":
		return nil
	}
	return errors.New("install/uninstall support macOS (launchd) and Linux (systemd user units); elsewhere run `flats serve` under your service manager")
}

func (a *app) install(args []string) error {
	fs := a.flags("install")
	data := fs.String("data", "", "data directory (default: FLATS_DATA or ~/Library/Application Support/Flats)")
	exe := fs.String("executable", "", "flats binary to run (default: this executable)")
	extra, err := a.parse(fs, args, 0, -1)
	if err != nil {
		return err
	}
	if err := a.requireMacOS(); err != nil {
		return err
	}
	dataDir := *data
	if dataDir == "" {
		// launchd does not inherit the shell environment, so an explicit
		// FLATS_DATA becomes a --data flag of the agent.
		dataDir = a.env.Getenv("FLATS_DATA")
	}
	var serveArgs []string
	if dataDir != "" {
		abs, err := filepath.Abs(dataDir)
		if err != nil {
			return err
		}
		dataDir = abs
		serveArgs = append(serveArgs, "--data", dataDir)
	} else if dataDir, err = DefaultDataDir(a.env.Getenv); err != nil {
		return err
	}
	// Validate the documented serve-argument passthrough before service writes.
	for i, arg := range extra {
		var path string
		if arg == "--operator-credential-file" {
			if i+1 >= len(extra) {
				return errors.New("--operator-credential-file requires an absolute path")
			}
			path = extra[i+1]
		} else if strings.HasPrefix(arg, "--operator-credential-file=") {
			path = strings.TrimPrefix(arg, "--operator-credential-file=")
		} else {
			continue
		}
		if !filepath.IsAbs(path) {
			return errors.New("--operator-credential-file requires an absolute path")
		}
	}
	serveArgs = append(serveArgs, extra...)

	if a.goos() == "linux" {
		path, err := systemd.Install(a.ctx, systemd.Options{Executable: *exe, DataDir: dataDir, Args: extra, Env: map[string]string{"PATH": a.env.Getenv("PATH")}, Home: a.env.Home, Run: systemd.Runner(a.env.Launchd)})
		if err != nil {
			return err
		}
		up := a.waitForServer()
		if a.jsonOut {
			a.writeJSON(map[string]any{"unit": path, "url": a.url, "running": up})
			return nil
		}
		fmt.Fprintf(a.out, "Installed systemd user service %s (%s)\n", systemd.Unit, path)
		fmt.Fprintln(a.out, "  logs:   journalctl --user -u flats.service")
		fmt.Fprintln(a.out, "  To keep Flats running while you are logged out: loginctl enable-linger $USER")
		if up {
			fmt.Fprintf(a.out, "Flats is running at %s\n", a.url)
		} else {
			fmt.Fprintf(a.out, "The service is installed but %s is not answering yet; check `systemctl --user status flats`.\n", a.url)
		}
		return nil
	}
	opts := a.launchdOpts()
	opts.Executable = *exe
	opts.DataDir = dataDir
	opts.Args = serveArgs
	opts.Env = map[string]string{"PATH": a.env.Getenv("PATH")}
	job, err := launchd.BuildJob(opts)
	if err != nil {
		return err
	}
	if strings.Contains(job.Program[0], string(filepath.Separator)+"go-build") {
		return fmt.Errorf("%s looks like a temporary `go run` build; install a built binary (go build -o /usr/local/bin/flats ./cmd/flats) and run its install, or pass --executable", job.Program[0])
	}
	res, err := launchd.Install(a.ctx, opts)
	if err != nil {
		return err
	}
	up := a.waitForServer()
	if a.jsonOut {
		a.writeJSON(map[string]any{"plist": res.PlistPath, "method": res.Method, "program": res.Job.Program,
			"stdout": res.Job.Stdout, "stderr": res.Job.Stderr, "url": a.url, "running": up})
		return nil
	}
	fmt.Fprintf(a.out, "Installed launchd agent %s (%s)\n", launchd.Label, res.PlistPath)
	fmt.Fprintf(a.out, "  runs:   %s\n", strings.Join(res.Job.Program, " "))
	fmt.Fprintf(a.out, "  logs:   %s\n          %s\n", res.Job.Stdout, res.Job.Stderr)
	if up {
		fmt.Fprintf(a.out, "Flats is running at %s\n", a.url)
	} else {
		fmt.Fprintf(a.out, "The agent is loaded but %s is not answering yet; check the logs above or run `flats status`.\n", a.url)
	}
	return nil
}

// waitForServer polls /api/status until it answers or InstallWait passes.
func (a *app) waitForServer() bool {
	wait := a.env.InstallWait
	if wait <= 0 {
		wait = 10 * time.Second
	}
	deadline := time.Now().Add(wait)
	c := a.client()
	for {
		if _, err := c.call(a.ctx, http.MethodGet, "/api/status", nil, nil, nil); err == nil {
			return true
		}
		if time.Now().After(deadline) || a.ctx.Err() != nil {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (a *app) uninstall(args []string) error {
	fs := a.flags("uninstall")
	if _, err := a.parse(fs, args, 0, 0); err != nil {
		return err
	}
	if err := a.requireMacOS(); err != nil {
		return err
	}
	if a.goos() == "linux" {
		removed, err := systemd.Uninstall(a.ctx, systemd.Options{Home: a.env.Home, Run: systemd.Runner(a.env.Launchd)})
		if err != nil {
			return err
		}
		if removed {
			fmt.Fprintf(a.out, "Removed systemd user service %s. Your flats and data are kept.\n", systemd.Unit)
		} else {
			fmt.Fprintln(a.out, "The systemd user service was not installed.")
		}
		return nil
	}
	removed, err := launchd.Uninstall(a.ctx, a.launchdOpts())
	if err != nil {
		return err
	}
	if a.jsonOut {
		a.writeJSON(map[string]any{"removed": removed})
		return nil
	}
	if removed {
		fmt.Fprintf(a.out, "Removed launchd agent %s. Your flats and data are kept.\n", launchd.Label)
	} else {
		fmt.Fprintln(a.out, "The launchd agent was not installed.")
	}
	return nil
}

func (a *app) status(args []string) error {
	fs := a.flags("status")
	if _, err := a.parse(fs, args, 0, 0); err != nil {
		return err
	}
	var sys json.RawMessage
	resp, serr := a.client().call(a.ctx, http.MethodGet, "/api/status", nil, nil, nil)
	if serr == nil {
		sys = resp.Body
	}
	var ld *launchd.Status
	if a.goos() == "darwin" {
		if st, err := launchd.GetStatus(a.ctx, a.launchdOpts()); err == nil {
			ld = &st
		}
	}
	if a.jsonOut {
		out := map[string]any{"url": a.url, "server": sys, "launchd": ld}
		if serr != nil {
			out["server_error"] = serr.Error()
		}
		a.writeJSON(out)
		if serr != nil {
			return exitCode(ExitError)
		}
		return nil
	}
	w := a.out
	if serr != nil {
		fmt.Fprintf(w, "server:  not reachable at %s\n", a.url)
	} else {
		fmt.Fprintf(w, "server:  running at %s\n", a.url)
		var body map[string]json.RawMessage
		if json.Unmarshal(sys, &body) == nil {
			for _, k := range sortedKeys(body) {
				if k == "ok" {
					continue
				}
				fmt.Fprintf(w, "  %s: %s\n", k, indentJSON(body[k], "  "))
			}
			if hasPublicHosts(body["system"]) {
				fmt.Fprintf(w, "  note: the public hosts above serve public flats. %s\n", publicURLNotice)
			}
		}
	}
	if ld != nil {
		switch {
		case ld.Loaded:
			line := "launchd: loaded"
			if ld.State != "" {
				line += ", " + ld.State
			}
			if ld.PID > 0 {
				line += fmt.Sprintf(", pid %d", ld.PID)
			}
			if ld.LastExit != "" {
				line += ", last exit " + ld.LastExit
			}
			fmt.Fprintln(w, line)
		case ld.Installed:
			fmt.Fprintf(w, "launchd: plist present but not loaded (%s); run `flats install` again\n", ld.PlistPath)
		default:
			fmt.Fprintln(w, "launchd: not installed (`flats install` runs the server at login)")
		}
	}
	if serr != nil {
		return serr
	}
	return nil
}

// hasPublicHosts reports whether a status "system" object lists public
// (Portal) hosts with URLs.
func hasPublicHosts(raw json.RawMessage) bool {
	var sys struct {
		Public *struct {
			Hosts []struct {
				URL string `json:"url"`
			} `json:"hosts"`
		} `json:"public"`
	}
	if json.Unmarshal(raw, &sys) != nil || sys.Public == nil {
		return false
	}
	for _, h := range sys.Public.Hosts {
		if h.URL != "" {
			return true
		}
	}
	return false
}

func indentJSON(raw json.RawMessage, prefix string) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, err := json.MarshalIndent(v, prefix, "  ")
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// --- MCP setup ---

func (a *app) mcpConfig(args []string) error {
	fs := a.flags("mcp-config")
	if _, err := a.parse(fs, args, 0, 0); err != nil {
		return err
	}
	// Verified 2026-10-03 by scripts/agent-gate.sh: Claude Code 2.1.287
	// (--mcp-config), codex-cli 0.160.0 (mcp_servers.flats.url plus
	// default_tools_approval_mode) and cursor-agent 2026.10.01 (.cursor/mcp.json)
	// each deployed a flat over this endpoint.
	endpoint := strings.TrimRight(a.url, "/") + "/mcp"
	claude := fmt.Sprintf("claude mcp add --transport http flats %s", endpoint)
	codexCmd := fmt.Sprintf("codex mcp add flats --url %s", endpoint)
	codexTOML := fmt.Sprintf("[mcp_servers.flats]\nurl = %q\n# needed for `codex exec`; interactive sessions can prompt instead\ndefault_tools_approval_mode = \"approve\"\n", endpoint)
	cursor, _ := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{"flats": map[string]string{"url": endpoint}}}, "", "  ")

	if a.jsonOut {
		a.writeJSON(map[string]any{
			"url":    endpoint,
			"claude": map[string]string{"command": claude},
			"codex":  map[string]string{"command": codexCmd, "file": "~/.codex/config.toml", "toml": codexTOML},
			"cursor": map[string]any{"file": ".cursor/mcp.json (project) or ~/.cursor/mcp.json (global)", "json": json.RawMessage(cursor)},
		})
		return nil
	}
	fmt.Fprintf(a.out, `Flats MCP endpoint: %s  (Streamable HTTP)

Claude Code:
  %s
  (add --scope user to make it available in every project)

Codex (~/.codex/config.toml):
%s
  or: %s
  Flats requires operator approval for every publish, activation, rollback,
  deletion and visibility change in both directions. Client auto-approval
  permits Draft edits and pending requests; it does not make a version live
  or grant operator authority. Use only with trusted agents.
  Discover runtime APIs: read flats://docs/runtime-api/v1 or call
  get_runtime_reference with {} (no installed skill required).

Cursor (.cursor/mcp.json in a project, or ~/.cursor/mcp.json for all projects):
%s
`, endpoint, claude, indent(codexTOML, "  "), codexCmd, indent(string(cursor)+"\n", "  "))
	if !isLoopbackURL(a.url) {
		fmt.Fprintln(a.out, "\nNote: save_version_from_dir only works for agents on the Flats host (loopback URL).")
	}
	return nil
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

func isLoopbackURL(u string) bool {
	for _, p := range []string{"http://127.0.0.1", "http://localhost", "http://[::1]"} {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}
