// Package systemd installs `flats serve` as a systemd user service on Linux
// (the counterpart of package launchd on macOS).
package systemd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Unit is the service name.
const Unit = "flats.service"

// Runner runs a command and returns its combined output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs commands for real.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Options configure the unit.
type Options struct {
	OperatorCredentialFile string            // optional noninteractive operator-owned 0600 file
	Executable             string            // default: the running executable
	Args                   []string          // extra `flats serve` flags
	DataDir                string            // required
	Env                    map[string]string // extra environment
	Home                   string            // default: os.UserHomeDir
	Run                    Runner            // default: ExecRunner
}

func (o *Options) fill() error {
	if o.Run == nil {
		o.Run = ExecRunner
	}
	if o.Home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		o.Home = h
	}
	return nil
}

// UnitPath is where the user unit is written.
func UnitPath(home string) string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "systemd", "user", Unit)
	}
	return filepath.Join(home, ".config", "systemd", "user", Unit)
}

// quote escapes one ExecStart word per systemd.syntax rules.
func quote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'\\$%;") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`, `%`, `%%`)
	return `"` + r.Replace(s) + `"`
}

// Render returns the unit file contents.
func Render(opts Options) (string, error) {
	if opts.DataDir == "" {
		return "", errors.New("systemd: data directory is required")
	}
	exe := opts.Executable
	if exe == "" {
		e, err := os.Executable()
		if err != nil {
			return "", err
		}
		exe = e
	}
	exe, err := filepath.Abs(exe)
	if err != nil {
		return "", err
	}
	data, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return "", err
	}
	words := []string{quote(exe), "serve", "--data", quote(data)}
	for _, a := range opts.Args {
		words = append(words, quote(a))
	}
	if opts.OperatorCredentialFile != "" {
		path, err := filepath.Abs(opts.OperatorCredentialFile)
		if err != nil {
			return "", err
		}
		words = append(words, "--operator-credential-file", quote(path))
	}
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=Flats: self-hosted sites for coding agents\nAfter=network-online.target\nWants=network-online.target\n\n")
	b.WriteString("[Service]\nType=simple\n")
	b.WriteString("ExecStart=" + strings.Join(words, " ") + "\n")
	keys := make([]string, 0, len(opts.Env))
	for k := range opts.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("Environment=" + quote(k+"="+opts.Env[k]) + "\n")
	}
	b.WriteString("Restart=always\nRestartSec=5\nUMask=0077\n\n[Install]\nWantedBy=default.target\n")
	return b.String(), nil
}

// Install writes the unit and (re)starts it.
func Install(ctx context.Context, opts Options) (string, error) {
	if err := opts.fill(); err != nil {
		return "", err
	}
	unit, err := Render(opts)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return "", err
	}
	path := UnitPath(opts.Home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(unit), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	for _, args := range [][]string{{"--user", "daemon-reload"}, {"--user", "enable", Unit}, {"--user", "restart", Unit}} {
		if out, err := opts.Run(ctx, "systemctl", args...); err != nil {
			return path, fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return path, nil
}

// Uninstall stops and removes the unit. It reports whether one existed.
func Uninstall(ctx context.Context, opts Options) (bool, error) {
	if err := opts.fill(); err != nil {
		return false, err
	}
	path := UnitPath(opts.Home)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil
	}
	_, _ = opts.Run(ctx, "systemctl", "--user", "disable", "--now", Unit)
	if err := os.Remove(path); err != nil {
		return true, err
	}
	_, _ = opts.Run(ctx, "systemctl", "--user", "daemon-reload")
	return true, nil
}

// Active reports `systemctl --user is-active flats.service`.
func Active(ctx context.Context, opts Options) (string, error) {
	if err := opts.fill(); err != nil {
		return "", err
	}
	out, _ := opts.Run(ctx, "systemctl", "--user", "is-active", Unit)
	return strings.TrimSpace(string(out)), nil
}
