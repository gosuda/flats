// Package launchd installs `flats serve` as a per-user launchd agent on macOS.
//
// Every launchctl invocation goes through Options.Run, so tests can exercise
// Install, Uninstall and Status without touching the real launchd.
package launchd

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Label is the launchd job label.
const Label = "dev.flats.serve"

// DefaultPath is used for the agent's PATH when the caller's PATH is empty.
// launchd starts agents with a minimal PATH, which would hide git and other
// tools that flats may call.
const DefaultPath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// Runner runs a command and returns its combined output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs real commands.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Options configures Install, Uninstall and Status.
type Options struct {
	Executable string            // absolute path of the flats binary; default: the running executable
	Args       []string          // `flats serve` flags; install passes --config PATH
	DataDir    string            // logs go to DataDir/logs (required by Install)
	Env        map[string]string // extra environment; PATH is always set
	Home       string            // default: os.UserHomeDir
	UID        int               // default (0): os.Getuid
	Run        Runner            // default: ExecRunner
}

func (o *Options) fill() error {
	if o.Run == nil {
		o.Run = ExecRunner
	}
	if o.UID == 0 {
		o.UID = os.Getuid()
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

func (o *Options) domain() string  { return "gui/" + strconv.Itoa(o.UID) }
func (o *Options) service() string { return o.domain() + "/" + Label }

// PlistPath returns ~/Library/LaunchAgents/dev.flats.serve.plist under home.
func PlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
}

// LogPaths returns the stdout and stderr log files under dataDir.
func LogPaths(dataDir string) (stdout, stderr string) {
	logs := filepath.Join(dataDir, "logs")
	return filepath.Join(logs, "serve.out.log"), filepath.Join(logs, "serve.err.log")
}

// Job is the content of the agent plist.
type Job struct {
	Program []string
	Stdout  string
	Stderr  string
	Env     map[string]string
}

// Plist renders the job as an XML property list.
func (j Job) Plist() []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	key := func(k string) { fmt.Fprintf(&b, "\t<key>%s</key>\n", esc(k)) }
	str := func(indent, s string) { fmt.Fprintf(&b, "%s<string>%s</string>\n", indent, esc(s)) }

	key("Label")
	str("\t", Label)
	key("ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, a := range j.Program {
		str("\t\t", a)
	}
	b.WriteString("\t</array>\n")
	key("RunAtLoad")
	b.WriteString("\t<true/>\n")
	key("KeepAlive")
	b.WriteString("\t<true/>\n")
	key("ProcessType")
	str("\t", "Background")
	key("StandardOutPath")
	str("\t", j.Stdout)
	key("StandardErrorPath")
	str("\t", j.Stderr)
	if len(j.Env) > 0 {
		key("EnvironmentVariables")
		b.WriteString("\t<dict>\n")
		names := make([]string, 0, len(j.Env))
		for k := range j.Env {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Fprintf(&b, "\t\t<key>%s</key>\n", esc(k))
			str("\t\t", j.Env[k])
		}
		b.WriteString("\t</dict>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// BuildJob resolves the job for opts without side effects.
func BuildJob(opts Options) (Job, error) {
	if opts.DataDir == "" {
		return Job{}, errors.New("launchd: data directory is required")
	}
	exe := opts.Executable
	if exe == "" {
		e, err := os.Executable()
		if err != nil {
			return Job{}, fmt.Errorf("launchd: locate executable: %w", err)
		}
		exe = e
	}
	// Symlinks are kept on purpose: a package manager's stable link (e.g.
	// /opt/homebrew/bin/flats) survives upgrades, while the versioned target
	// it points at is deleted by the next upgrade.
	exe, err := filepath.Abs(exe)
	if err != nil {
		return Job{}, err
	}
	dataDir, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return Job{}, err
	}
	env := map[string]string{}
	for k, v := range opts.Env {
		env[k] = v
	}
	if env["PATH"] == "" {
		env["PATH"] = DefaultPath
	}
	args := append([]string{}, opts.Args...)
	out, errp := LogPaths(dataDir)
	return Job{
		Program: append([]string{exe, "serve"}, args...),
		Stdout:  out,
		Stderr:  errp,
		Env:     env,
	}, nil
}

// Result reports what Install did.
type Result struct {
	PlistPath string
	Job       Job
	Method    string // "bootstrap" or "load"
}

// Install writes the agent plist and loads it into the user's GUI domain,
// replacing any running instance.
func Install(ctx context.Context, opts Options) (Result, error) {
	if err := opts.fill(); err != nil {
		return Result{}, err
	}
	job, err := BuildJob(opts)
	if err != nil {
		return Result{}, err
	}
	// The data directory holds secret.key and node keys: create it private.
	if err := os.MkdirAll(filepath.Dir(job.Stdout), 0o700); err != nil {
		return Result{}, fmt.Errorf("launchd: create log directory: %w", err)
	}
	path := PlistPath(opts.Home)
	if err := writeFileAtomic(path, job.Plist(), 0o644); err != nil {
		return Result{}, fmt.Errorf("launchd: write %s: %w", path, err)
	}
	res := Result{PlistPath: path, Job: job}

	// Unload an older instance so the new plist takes effect, and clear a
	// disabled override left by `launchctl unload -w`. Both fail harmlessly
	// when there is nothing to undo.
	if _, err := opts.Run(ctx, "launchctl", "bootout", opts.service()); err == nil {
		// bootout returns before the job has left the domain; bootstrapping
		// too early fails with "Bootstrap failed: 5: Input/output error".
		waitGone(ctx, &opts)
	}
	_, _ = opts.Run(ctx, "launchctl", "enable", opts.service())

	out, err := opts.Run(ctx, "launchctl", "bootstrap", opts.domain(), path)
	if err != nil && sleepCtx(ctx, retryDelay) {
		out, err = opts.Run(ctx, "launchctl", "bootstrap", opts.domain(), path)
	}
	if err == nil {
		res.Method = "bootstrap"
		return res, nil
	}
	out2, err2 := opts.Run(ctx, "launchctl", "load", "-w", path)
	if err2 == nil {
		res.Method = "load"
		return res, nil
	}
	return res, fmt.Errorf("launchd: could not load %s: bootstrap: %v: %s; load -w: %v: %s",
		path, err, strings.TrimSpace(string(out)), err2, strings.TrimSpace(string(out2)))
}

// Timings of the bootout/bootstrap handoff; tests shorten them.
var (
	settleTimeout = 5 * time.Second
	settlePoll    = 100 * time.Millisecond
	retryDelay    = time.Second
)

// waitGone polls `launchctl print` until the service is no longer known to
// launchd, or settleTimeout passes.
func waitGone(ctx context.Context, opts *Options) {
	deadline := time.Now().Add(settleTimeout)
	for time.Now().Before(deadline) {
		if _, err := opts.Run(ctx, "launchctl", "print", opts.service()); err != nil {
			return
		}
		if !sleepCtx(ctx, settlePoll) {
			return
		}
	}
}

// sleepCtx sleeps for d and reports whether ctx is still live.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Uninstall unloads the agent and removes its plist. It reports whether a
// plist was removed; a missing agent is not an error.
func Uninstall(ctx context.Context, opts Options) (bool, error) {
	if err := opts.fill(); err != nil {
		return false, err
	}
	path := PlistPath(opts.Home)
	if _, err := opts.Run(ctx, "launchctl", "bootout", opts.service()); err != nil {
		if _, statErr := os.Stat(path); statErr == nil {
			_, _ = opts.Run(ctx, "launchctl", "unload", path)
		}
	}
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("launchd: remove %s: %w", path, err)
	}
	return true, nil
}

// Status is the agent state as launchd reports it.
type Status struct {
	PlistPath string `json:"plist_path"`
	Installed bool   `json:"installed"` // plist exists
	Loaded    bool   `json:"loaded"`    // launchd knows the service
	State     string `json:"state,omitempty"`
	PID       int    `json:"pid,omitempty"`
	LastExit  string `json:"last_exit,omitempty"`
}

// GetStatus queries `launchctl print gui/<uid>/dev.flats.serve`.
func GetStatus(ctx context.Context, opts Options) (Status, error) {
	if err := opts.fill(); err != nil {
		return Status{}, err
	}
	st := Status{PlistPath: PlistPath(opts.Home)}
	if _, err := os.Stat(st.PlistPath); err == nil {
		st.Installed = true
	}
	out, err := opts.Run(ctx, "launchctl", "print", opts.service())
	if err != nil {
		return st, nil
	}
	st.Loaded = true
	parsePrint(string(out), &st)
	return st, nil
}

// parsePrint reads the top-level fields of `launchctl print` output. Nested
// blocks (e.g. the environment) are indented further and ignored.
func parsePrint(out string, st *Status) {
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "\t\t") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch k {
		case "state":
			st.State = v
		case "pid":
			st.PID, _ = strconv.Atoi(v)
		case "last exit code":
			st.LastExit = v
		}
	}
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".flats-plist-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
