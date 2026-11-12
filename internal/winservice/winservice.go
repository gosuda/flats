// Package winservice installs `flats serve` as a per-user service on Windows
// (the counterpart of package launchd on macOS and package systemd on Linux).
//
// The service is a scheduled task, because a real Windows service needs
// administrator rights. A task with the user's interactive token registers
// without them. Its action runs the server in a headless console, so no window
// appears. A trigger that repeats every minute starts the task when it no
// longer runs, which brings a crashed server back: Task Scheduler's own
// restart setting does not react to a process that exits with an error.
//
// schtasks text is localized, so nothing here parses it. Exit codes say whether
// the task exists, and the data directory lock says whether a server runs.
package winservice

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/gosuda/flats/internal/filelock"
)

// Task is the scheduled task's name.
const Task = "Flats"

// ServiceRole follows the process ID in the data directory lock of a server that
// the task started. A stop acts on such a server only: a foreground flats serve
// is the user's.
const ServiceRole = "service"

// ErrNoDefinition reports a task whose definition file is gone, so that nothing
// says which data directory it serves.
var ErrNoDefinition = errors.New("the service definition is missing")

// Runner runs a command and returns its combined output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs commands for real.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Options configure the task.
type Options struct {
	Executable string          // default: the running executable
	Args       []string        // `flats serve` flags, such as --config PATH
	DataDir    string          // required for Install; else read from the definition
	UserID     string          // default: the current user's SID
	UserName   string          // default: the current user's name
	Dir        string          // where the definition is kept; default: DefinitionDir
	Run        Runner          // default: ExecRunner
	StopWait   time.Duration   // how long a stop waits for an orderly exit; default 20 s
	Kill       func(int) error // ends a server by process ID; default: the system's
}

func (o *Options) fill() error {
	if o.Run == nil {
		o.Run = ExecRunner
	}
	if o.Kill == nil {
		o.Kill = killProcess
	}
	if o.StopWait <= 0 {
		o.StopWait = 20 * time.Second
	}
	if o.Dir == "" {
		dir, err := DefinitionDir()
		if err != nil {
			return err
		}
		o.Dir = dir
	}
	return nil
}

// DefinitionDir holds the task's definition, which plays the part of the
// launchd plist and the systemd unit: the installer and `flats update` read it
// to learn what the service runs.
func DefinitionDir() (string, error) {
	dir, err := os.UserCacheDir() // %LocalAppData%
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "Flats"), nil
}

// DefinitionPath is the file that holds the task's XML.
func DefinitionPath(dir string) string { return filepath.Join(dir, "service.xml") }

// LogPath is where the service writes its log.
func LogPath(dataDir string) string { return filepath.Join(dataDir, "logs", "serve.log") }

// conhost starts the server in a console without a window.
func conhost() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "conhost.exe")
}

func xmlText(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s)) // a Builder does not fail
	return b.String()
}

// escapeArg quotes one word of a command line as the Windows C runtime parses
// it, so that a path with spaces stays one argument.
func escapeArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes+1))
			slashes = 0
		default:
			slashes = 0
		}
		b.WriteByte(s[i])
	}
	b.WriteString(strings.Repeat(`\`, slashes))
	b.WriteByte('"')
	return b.String()
}

// Render returns the task's XML.
func Render(o Options) (string, error) {
	if o.Executable == "" || o.DataDir == "" {
		return "", errors.New("winservice: the executable and the data directory are required")
	}
	if o.UserID == "" || o.UserName == "" {
		return "", errors.New("winservice: the user is required")
	}
	words := []string{escapeArg(o.Executable), "serve"}
	for _, a := range o.Args {
		words = append(words, escapeArg(a))
	}
	words = append(words, "--log-file", escapeArg(LogPath(o.DataDir)), "--watch-parent")
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Runs Flats in the background.</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + xmlText(o.UserName) + `</UserId>
    </LogonTrigger>
    <TimeTrigger>
      <Repetition>
        <Interval>PT1M</Interval>
        <StopAtDurationEnd>false</StopAtDurationEnd>
      </Repetition>
      <StartBoundary>2020-01-01T00:00:00</StartBoundary>
      <Enabled>true</Enabled>
    </TimeTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + xmlText(o.UserID) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Hidden>true</Hidden>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + xmlText(conhost()) + `</Command>
      <Arguments>--headless ` + xmlText(strings.Join(words, " ")) + `</Arguments>
    </Exec>
  </Actions>
</Task>
`, nil
}

// encode returns the text as UTF-16 with a byte order mark, which is what the
// XML declaration names.
func encode(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, 2+2*len(units))
	out = append(out, 0xff, 0xfe)
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

// decode is the reverse of encode.
func decode(b []byte) string {
	if len(b) < 2 || b[0] != 0xff || b[1] != 0xfe {
		return string(b)
	}
	b = b[2:]
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		units = append(units, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return string(utf16.Decode(units))
}

func (o *Options) schtasks(ctx context.Context, args ...string) ([]byte, error) {
	out, err := o.Run(ctx, "schtasks.exe", args...)
	if err != nil {
		return out, fmt.Errorf("schtasks %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// register creates or replaces the task from the definition file. A created
// task is enabled, which undoes Stop.
func (o *Options) register(ctx context.Context) error {
	_, err := o.schtasks(ctx, "/Create", "/TN", Task, "/XML", DefinitionPath(o.Dir), "/F")
	return err
}

// Install writes the definition, registers the task and tries to start it. It
// returns the definition's path and whether the task started: a task of the
// user's interactive token cannot run without a logged-on session, for example
// over SSH. It then starts at the next login, so that is no failure.
func Install(ctx context.Context, opts Options) (string, bool, error) {
	if err := opts.fill(); err != nil {
		return "", false, err
	}
	if opts.Executable == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", false, err
		}
		opts.Executable = exe
	}
	if opts.UserID == "" || opts.UserName == "" {
		id, name, err := currentUser()
		if err != nil {
			return "", false, err
		}
		opts.UserID, opts.UserName = id, name
	}
	text, err := Render(opts)
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(filepath.Dir(LogPath(opts.DataDir)), 0o700); err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return "", false, err
	}
	path := DefinitionPath(opts.Dir)
	if err := os.WriteFile(path, encode(text), 0o644); err != nil {
		return "", false, err
	}
	if err := opts.register(ctx); err != nil {
		return path, false, err
	}
	_, err = opts.schtasks(ctx, "/Run", "/TN", Task)
	return path, err == nil, nil
}

// Installed reports whether the task exists.
func Installed(ctx context.Context, opts Options) (bool, error) {
	if err := opts.fill(); err != nil {
		return false, err
	}
	_, err := opts.Run(ctx, "schtasks.exe", "/Query", "/TN", Task)
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return err == nil, err
	}
	// schtasks exits with 1 for a missing task and for a failed query, and its
	// text is localized. The list of all tasks fails only for the second.
	if _, err := opts.schtasks(ctx, "/Query", "/FO", "CSV", "/NH"); err != nil {
		return false, err
	}
	return false, nil
}

// lockPath is the data directory lock of `flats serve`.
func lockPath(dataDir string) string { return filepath.Join(dataDir, "flats.lock") }

// holder is the server that holds a data directory.
type holder struct {
	running    bool
	pid        int  // 0: it recorded none yet
	foreground bool // it recorded that the task did not start it
}

// locked reads the data directory lock.
func locked(dataDir string) holder {
	f, err := os.OpenFile(lockPath(dataDir), os.O_RDWR, 0)
	if err != nil {
		return holder{}
	}
	defer f.Close()
	if err := filelock.TryLock(f); err != nil {
		h := holder{running: errors.Is(err, filelock.ErrLocked)}
		b, _ := os.ReadFile(lockPath(dataDir))
		if words := strings.Fields(string(b)); len(words) > 0 {
			h.pid, _ = strconv.Atoi(words[0])
			h.foreground = len(words) < 2 || words[1] != ServiceRole
		}
		return h
	}
	filelock.Unlock(f)
	return holder{}
}

// dataDir is the option, else what the definition names.
func (o *Options) dataDir() (string, error) {
	if o.DataDir != "" {
		return o.DataDir, nil
	}
	return InstalledDataDir(o.Dir)
}

// Running reports whether the service's server runs. A foreground flats serve on
// its data directory is not the service.
func Running(opts Options) (bool, error) {
	if err := opts.fill(); err != nil {
		return false, err
	}
	dir, err := opts.dataDir()
	if err != nil {
		return false, err
	}
	if dir == "" {
		return false, ErrNoDefinition
	}
	h := locked(dir)
	return h.running && !h.foreground, nil
}

// Stop stops the service's server and keeps the task's definition. It reports
// whether such a server ran. Without one it changes nothing, so that a failed
// install never leaves a task disabled that the user did not stop, and a
// foreground flats serve stays as it is. With one it disables the task first, or
// the repeating trigger would start the server again, so a stopped service stays
// stopped until Start. Ending the task ends the headless console, and the server
// takes that as a request to shut down in order. schtasks /End never reaches the
// server itself, so one that does not exit in time is ended through the process
// ID in its lock file. Without a definition no lock says whether a server runs,
// and only ending the task can reach one.
func Stop(ctx context.Context, opts Options) (bool, error) {
	if err := opts.fill(); err != nil {
		return false, err
	}
	installed, err := Installed(ctx, opts)
	if err != nil {
		return false, err
	}
	if !installed {
		return false, nil
	}
	dir, err := opts.dataDir()
	if err != nil {
		return false, err
	}
	if dir == "" {
		_, _ = opts.Run(ctx, "schtasks.exe", "/End", "/TN", Task)
		return false, nil
	}
	if h := locked(dir); !h.running || h.foreground {
		return false, nil
	}
	if _, err := opts.schtasks(ctx, "/Change", "/TN", Task, "/DISABLE"); err != nil {
		return false, err
	}
	// It fails when the task does not run, while the server does: only the
	// process ID reaches such a server, after the wait.
	_, _ = opts.Run(ctx, "schtasks.exe", "/End", "/TN", Task)
	deadline := time.Now().Add(opts.StopWait)
	for {
		h := locked(dir)
		if !h.running {
			return true, nil
		}
		if time.Now().After(deadline) {
			if h.pid == 0 {
				return true, fmt.Errorf("the Flats service did not stop within %s", opts.StopWait)
			}
			if err := opts.Kill(h.pid); err != nil {
				return true, fmt.Errorf("end the Flats server %d: %w", h.pid, err)
			}
			return true, nil
		}
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Start registers the task from its definition file and runs it.
func Start(ctx context.Context, opts Options) error {
	if err := opts.fill(); err != nil {
		return err
	}
	if err := opts.register(ctx); err != nil {
		return err
	}
	_, err := opts.schtasks(ctx, "/Run", "/TN", Task)
	return err
}

// Uninstall stops the server, removes the task and its definition, and reports
// whether the task existed. The data stays.
func Uninstall(ctx context.Context, opts Options) (bool, error) {
	if err := opts.fill(); err != nil {
		return false, err
	}
	installed, err := Installed(ctx, opts)
	if err != nil {
		return false, err
	}
	if _, err := Stop(ctx, opts); err != nil {
		return installed, err
	}
	if installed {
		if _, err := opts.schtasks(ctx, "/Delete", "/TN", Task, "/F"); err != nil {
			return true, err
		}
	}
	if err := os.Remove(DefinitionPath(opts.Dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return installed, err
	}
	return installed, nil
}

var definitionCommand = regexp.MustCompile(`(?s)<Arguments>--headless (.*?)</Arguments>`)

// definitionWords returns the words of the command of the definition in dir,
// or nothing when there is no definition.
func definitionWords(dir string) ([]string, error) {
	b, err := os.ReadFile(DefinitionPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m := definitionCommand.FindStringSubmatch(decode(b))
	if m == nil {
		return nil, errors.New("winservice: the service definition names no command")
	}
	return splitWords(html.UnescapeString(m[1])), nil
}

// InstalledExecutable returns the executable the definition in dir runs, or ""
// when there is no definition.
func InstalledExecutable(dir string) (string, error) {
	words, err := definitionWords(dir)
	if err != nil || len(words) == 0 {
		return "", err
	}
	return words[0], nil
}

// InstalledConfig returns the --config path of the definition in dir, or "" when
// there is no definition or it names none.
func InstalledConfig(dir string) (string, error) {
	words, err := definitionWords(dir)
	if err != nil {
		return "", err
	}
	for i, w := range words {
		if w == "--config" && i+1 < len(words) {
			return words[i+1], nil
		}
	}
	return "", nil
}

// InstalledDataDir returns the data directory of the definition in dir, or ""
// when there is no definition.
func InstalledDataDir(dir string) (string, error) {
	words, err := definitionWords(dir)
	if err != nil {
		return "", err
	}
	for i, w := range words {
		if w == "--log-file" && i+1 < len(words) {
			return filepath.Dir(filepath.Dir(words[i+1])), nil
		}
	}
	return "", nil
}

// splitWords is the reverse of joining words that escapeArg quoted.
func splitWords(line string) []string {
	var words []string
	var b strings.Builder
	inWord, quoted := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\':
			n := 0
			for i+n < len(line) && line[i+n] == '\\' {
				n++
			}
			i += n - 1
			inWord = true
			if i+1 < len(line) && line[i+1] == '"' {
				b.WriteString(strings.Repeat(`\`, n/2))
				if n%2 == 1 {
					b.WriteByte('"')
					i++
				}
				continue
			}
			b.WriteString(strings.Repeat(`\`, n))
		case c == '"':
			quoted = !quoted
			inWord = true
		case (c == ' ' || c == '\t') && !quoted:
			if inWord {
				words = append(words, b.String())
				b.Reset()
				inWord = false
			}
		default:
			b.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, b.String())
	}
	return words
}
