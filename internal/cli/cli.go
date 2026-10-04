// Package cli implements the `flats` command line. Every command except
// serve, worker, install and uninstall is a thin client of the HTTP API.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/launchd"
)

// Serve runs the server (`flats serve`). It is wired by the server package;
// SERVE HOOK: assign cli.Serve from the package that assembles core.Service,
// the API, MCP and the networks.
var Serve func(args []string) error

// Worker runs a server-flat worker (`flats worker`), wired like Serve.
var Worker func(args []string) error

// Config runs `flats config <subcommand>` against config.json and the data
// directory, wired like Serve.
var Config func(ctx context.Context, args []string, out io.Writer) error

// ConfigUsage is the usage of `flats config`, wired like Serve.
var ConfigUsage = "config <subcommand> [args]"

// EnsureConfig prepares the config `flats install` runs the service with
// and returns its path and the data directory, wired like Serve. Empty
// configPath and dataDir select the defaults.
var EnsureConfig func(ctx context.Context, configPath, dataDir string, serveArgs []string, out io.Writer) (string, string, error)

// exitCoder is an error that selects the exit code, such as 78 for a
// configuration the operator must fix.
type exitCoder interface{ ExitCode() int }

// Version is the release version, set with
// -ldflags "-X github.com/gosuda/flats/internal/cli.Version=v1.2.3".
var Version = "dev"

// Exit codes.
const (
	ExitOK      = 0
	ExitError   = 1
	ExitUsage   = 2
	ExitPending = 3
	ExitConfig  = 78 // the operator must fix the configuration (EX_CONFIG)
)

// Env is the process environment of one invocation; tests replace parts.
type Env struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	HTTP           *http.Client
	Launchd        launchd.Runner // nil: the real launchctl
	Home           string         // launchd home; "" = os.UserHomeDir
	GOOS           string         // "" = runtime.GOOS
	PollInterval   time.Duration  // logs --follow; 0 = 2s
	InstallWait    time.Duration  // how long install waits for the server; 0 = 10s
}

// OSEnv is the real process environment.
func OSEnv() Env {
	return Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv}
}

// app is one invocation.
type app struct {
	env     Env
	ctx     context.Context
	url     string
	jsonOut bool
	out     io.Writer
	errw    io.Writer
}

type command struct {
	name     string
	synopsis string
	usage    string
	run      func(a *app, args []string) error
}

var commands []*command

func init() {
	commands = []*command{
		{"draft", "read or save a Private Draft", "draft <slug> [<dir|archive>] [--expected-revision n] [--message m]", (*app).draft},
		{"publish", "request operator approval to publish Draft", "publish <slug> [--revision n] [--hash hash]", (*app).publish},
		{"deploy", "save Draft and request publish approval", "deploy <dir|archive> --flat <slug> [--save-only] [--message m]\n       flats deploy --flat <slug> --version <n>", (*app).deploy},
		{"list", "list flats", "list", (*app).list},
		{"info", "show one flat", "info <slug>", (*app).info},
		{"versions", "list published versions", "versions <slug>", (*app).versions},
		{"rollback", "request approval to activate an earlier version", "rollback <slug> [--to n] [--restore-data]", (*app).rollback},
		{"preview", "open a private preview of a version", "preview <slug> [--version n]", (*app).preview},
		{"visibility", "change who can open a flat", "visibility <slug> private|public [--reason r]", (*app).visibility},
		{"rename", "change a flat's slug", "rename <slug> <new-slug>", (*app).rename},
		{"delete", "request deletion of a flat", "delete <slug> [--reason r]", (*app).delete},
		{"logs", "show a flat's events", "logs <slug> [--follow] [--kind k] [--limit n]", (*app).logs},
		{"secret", "manage server-flat secrets", "secret set <slug> <NAME>   (value from stdin)\n       flats secret rm <slug> <NAME>\n       flats secret ls <slug>", (*app).secret},
		{"approvals", "list approval requests", "approvals [--status pending|approved|rejected|failed]", (*app).approvals},
		{"status", "show server and service status", "status", (*app).status},
		{"install", "run `flats serve` at login (launchd/systemd)", "install [--config path] [--data dir] [--executable path] [-- legacy serve flags...]", (*app).install},
		{"uninstall", "remove the launchd agent", "uninstall", (*app).uninstall},
		{"mcp-config", "print MCP setup for agent tools", "mcp-config [--url url]", (*app).mcpConfig},
		{"config", "manage the host configuration (config.json)", "", (*app).config},
		{"version", "print the flats version", "version", (*app).version},
	}
}

func (c *command) commandUsage() string {
	if c.name == "config" {
		return ConfigUsage
	}
	return c.usage
}

func findCommand(name string) *command {
	for _, c := range commands {
		if c.name == name {
			return c
		}
	}
	return nil
}

// usageError is a command-line mistake (exit 2).
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

// exitCode ends a command with a code after it printed its own output.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

// Run executes one command line (without the program name) and returns the
// process exit code.
func Run(ctx context.Context, args []string, env Env) int {
	if env.Getenv == nil {
		env.Getenv = func(string) string { return "" }
	}
	if env.Stdin == nil {
		env.Stdin = strings.NewReader("")
	}
	if env.Stdout == nil {
		env.Stdout = io.Discard
	}
	if env.Stderr == nil {
		env.Stderr = io.Discard
	}
	if env.HTTP == nil {
		env.HTTP = &http.Client{Timeout: 10 * time.Minute}
	}
	a := &app{env: env, ctx: ctx, out: newSafeWriter(env.Stdout), errw: newSafeWriter(env.Stderr)}

	top := flag.NewFlagSet("flats", flag.ContinueOnError)
	top.SetOutput(io.Discard)
	a.globalFlags(top)
	if err := top.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			a.printUsage(a.out)
			return ExitOK
		}
		return a.finish(usageError{err.Error()})
	}
	rest := top.Args()
	if len(rest) == 0 {
		a.printUsage(a.errw)
		return ExitUsage
	}
	name, rest := rest[0], rest[1:]
	switch name {
	case "serve":
		return a.runHook("serve", Serve, rest)
	case "worker":
		return a.runHook("worker", Worker, rest)
	case "help", "-h", "--help":
		if len(rest) > 0 {
			if c := findCommand(rest[0]); c != nil {
				fmt.Fprintf(a.out, "usage: flats %s\n", c.commandUsage())
				return ExitOK
			}
		}
		a.printUsage(a.out)
		return ExitOK
	}
	c := findCommand(name)
	if c == nil {
		a.printUsage(a.errw)
		return a.finish(usagef("unknown command %q", name))
	}
	err := c.run(a, rest)
	var ue usageError
	if errors.As(err, &ue) {
		fmt.Fprintf(a.errw, "flats %s: %s\nusage: flats %s\n", c.name, ue.msg, c.commandUsage())
		return ExitUsage
	}
	return a.finish(err)
}

func (a *app) runHook(name string, fn func([]string) error, args []string) int {
	if fn == nil {
		fmt.Fprintf(a.errw, "flats: %s is not wired yet\n", name)
		return ExitError
	}
	if err := fn(args); err != nil {
		fmt.Fprintf(a.errw, "flats %s: %v\n", name, err)
		var ec exitCoder
		if errors.As(err, &ec) {
			return ec.ExitCode()
		}
		return ExitError
	}
	return ExitOK
}

// finish prints err and maps it to an exit code.
func (a *app) finish(err error) int {
	if err == nil {
		return ExitOK
	}
	var ec exitCode
	if errors.As(err, &ec) {
		return int(ec)
	}
	if errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	var ue usageError
	if errors.As(err, &ue) {
		fmt.Fprintf(a.errw, "flats: %s\n", ue.msg)
		return ExitUsage
	}
	var coded exitCoder
	if errors.As(err, &coded) && !a.jsonOut {
		fmt.Fprintf(a.errw, "flats: %v\n", err)
		return coded.ExitCode()
	}
	var ae *apiError
	if errors.As(err, &ae) {
		if a.jsonOut {
			a.out.Write(ensureNewline(ae.Raw))
		} else {
			printAPIError(a.errw, ae)
		}
		return ExitError
	}
	if a.jsonOut {
		a.writeJSON(map[string]string{"error": err.Error()})
		if errors.As(err, &coded) {
			return coded.ExitCode()
		}
	} else {
		fmt.Fprintf(a.errw, "flats: %v\n", err)
	}
	return ExitError
}

func printAPIError(w io.Writer, e *apiError) {
	msg := e.Error()
	if len(e.Body.Problems) > 0 {
		// The message repeats the problems; print only its first line.
		msg, _, _ = strings.Cut(msg, "\n")
	}
	fmt.Fprintf(w, "error: %s\n", msg)
	printProblems(w, e.Body.Problems)
	if h := e.Body.Health; h != nil {
		printHealth(w, *h)
	}
}

func printProblems(w io.Writer, ps []problem) {
	for _, p := range ps {
		if p.Path != "" {
			fmt.Fprintf(w, "  - %s: %s\n", p.Path, p.Message)
		} else {
			fmt.Fprintf(w, "  - %s\n", p.Message)
		}
		if p.Fix != "" {
			fmt.Fprintf(w, "    fix: %s\n", p.Fix)
		}
	}
}

func (a *app) globalFlags(fs *flag.FlagSet) {
	def := a.env.Getenv("FLATS_URL")
	if def == "" {
		def = DefaultURL
	}
	if a.url == "" {
		a.url = def
	}
	fs.StringVar(&a.url, "url", a.url, "Flats server URL (env FLATS_URL)")
	fs.BoolVar(&a.jsonOut, "json", a.jsonOut, "machine-readable JSON output")
}

func (a *app) printUsage(w io.Writer) {
	fmt.Fprintf(w, "flats hosts agent-built websites on this machine.\n\nusage: flats [--url url] [--json] <command> [args]\n\ncommands:\n")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-11s %s\n", c.name, c.synopsis)
	}
	fmt.Fprintf(w, "  %-11s %s\n  %-11s %s\n\nRun `flats help <command>` for details. Exit codes: 0 ok, 1 error, 2 usage, 3 pending approval.\n",
		"serve", "run the server", "worker", "internal: server-flat worker process")
}

// flags creates a subcommand flag set that also accepts the global flags.
func (a *app) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	a.globalFlags(fs)
	return fs
}

// parse parses flags that may appear before, between or after positional
// arguments, and checks the positional count.
func (a *app) parse(fs *flag.FlagSet, args []string, minPos, maxPos int) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				if c := findCommand(fs.Name()); c != nil {
					fmt.Fprintf(a.out, "usage: flats %s\n", c.commandUsage())
				}
				fs.SetOutput(a.out)
				fs.PrintDefaults()
				return nil, exitCode(ExitOK)
			}
			return nil, usageError{err.Error()}
		}
		rest := fs.Args()
		consumed := len(args) - len(rest)
		if consumed > 0 && args[consumed-1] == "--" {
			pos = append(pos, rest...)
			break
		}
		if len(rest) == 0 {
			break
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
	a.url = strings.TrimRight(a.url, "/")
	if len(pos) < minPos {
		return nil, usagef("missing arguments")
	}
	if maxPos >= 0 && len(pos) > maxPos {
		return nil, usagef("unexpected argument %q", pos[maxPos])
	}
	return pos, nil
}

func (a *app) client() *client {
	return &client{base: strings.TrimRight(a.url, "/"), hc: a.env.HTTP}
}

func (a *app) writeJSON(v any) {
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// emitRaw prints an API reply verbatim in --json mode.
func (a *app) emitRaw(resp response) {
	a.out.Write(ensureNewline(resp.Body))
}

func ensureNewline(b []byte) []byte {
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return append(append([]byte{}, b...), '\n')
	}
	return b
}

func (a *app) goos() string {
	if a.env.GOOS != "" {
		return a.env.GOOS
	}
	return runtime.GOOS
}

func (a *app) version(args []string) error {
	fs := a.flags("version")
	if _, err := a.parse(fs, args, 0, 0); err != nil {
		return err
	}
	rev, modified := "", false
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
	}
	if a.jsonOut {
		a.writeJSON(map[string]any{"version": Version, "commit": rev, "dirty": modified, "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH})
		return nil
	}
	line := "flats " + Version
	if rev != "" {
		line += " (" + shortSHA(rev)
		if modified {
			line += "+dirty"
		}
		line += ")"
	}
	fmt.Fprintf(a.out, "%s %s %s/%s\n", line, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return nil
}

func sortedKeys[M ~map[string]V, V any](m M) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
