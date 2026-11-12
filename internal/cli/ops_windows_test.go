package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/filelock"
	"github.com/gosuda/flats/internal/winservice"
)

// fakeTask stands in for schtasks.exe. A missing task exits with an error, as
// the real command does.
type fakeTask struct {
	mu     sync.Mutex
	exists bool
	calls  []string
	fail   string // the first argument of a call that fails
}

func (f *fakeTask) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	if args[0] == f.fail {
		return []byte("refused"), exitFailure()
	}
	switch args[0] {
	case "/Create":
		f.exists = true
	case "/Delete":
		f.exists = false
	case "/Query":
		if !f.exists && slices.Contains(args, "/TN") {
			return nil, exitFailure()
		}
	}
	return nil, nil
}

func (f *fakeTask) called(arg string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, arg) })
}

// order reports whether the calls hold these arguments in this order.
func (f *fakeTask) order(args ...string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	at := 0
	for _, call := range f.calls {
		if at < len(args) && strings.HasPrefix(call, args[at]) {
			at++
		}
	}
	return at == len(args)
}

func exitFailure() error { return exec.Command("cmd.exe", "/c", "exit 1").Run() }

func windowsEnv(t *testing.T, task *fakeTask) (Env, string) {
	t.Helper()
	home := t.TempDir()
	hermeticInstall(t, home)
	exe := filepath.Join(home, "flats-bin.exe")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Env{Launchd: task.run, Home: home, GOOS: "windows", ServiceDir: t.TempDir(), InstallWait: time.Second}, exe
}

func TestWindowsInstallUninstallAndStatus(t *testing.T) {
	_, srv := newFakeAPI(t)
	task := &fakeTask{}
	env, exe := windowsEnv(t, task)
	if r := runEnv(t, env, srv.URL, "status"); !strings.Contains(r.stdout, "service: not installed") {
		t.Errorf("before install: %d %s %s", r.code, r.stdout, r.stderr)
	}
	data := filepath.Join(env.Home, "data")
	r := runEnv(t, env, srv.URL, "install", "--executable", exe, "--data", data)
	if r.code != 0 || !strings.Contains(r.stdout, "Installed scheduled task "+winservice.Task) ||
		!strings.Contains(r.stdout, "Flats is running at "+srv.URL) {
		t.Fatalf("install: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if order := task.order("/Query", "/Create", "/Run"); !order {
		t.Fatalf("calls: %v", task.calls)
	}
	definition, err := winservice.InstalledConfig(env.ServiceDir)
	if want := filepath.Join(data, "config.json"); err != nil || definition != want {
		t.Fatalf("definition names %q, %v; want %q", definition, err, want)
	}
	if r := runEnv(t, env, srv.URL, "status"); !strings.Contains(r.stdout, "service: scheduled task "+winservice.Task+", stopped") {
		t.Errorf("after install: %d %s %s", r.code, r.stdout, r.stderr)
	}
	r = runEnv(t, env, srv.URL, "uninstall")
	if r.code != 0 || !strings.Contains(r.stdout, "Removed scheduled task "+winservice.Task) {
		t.Fatalf("uninstall: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if _, err := os.Stat(winservice.DefinitionPath(env.ServiceDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("definition left: %v", err)
	}
	if r := runEnv(t, env, srv.URL, "uninstall"); r.code != 0 || !strings.Contains(r.stdout, "was not installed") {
		t.Errorf("second uninstall: %d %s %s", r.code, r.stdout, r.stderr)
	}
}

// The installer restarts the service through flats install without flags, and a
// user does too. That stays on the host of the installed service.
func TestWindowsInstallWithoutFlagsStaysOnTheInstalledHost(t *testing.T) {
	_, srv := newFakeAPI(t)
	task := &fakeTask{}
	env, exe := windowsEnv(t, task)
	data := filepath.Join(env.Home, "custom data")
	if r := runEnv(t, env, srv.URL, "install", "--executable", exe, "--data", data); r.code != 0 {
		t.Fatalf("first install: %d %s %s", r.code, r.stdout, r.stderr)
	}
	t.Setenv("FLATS_DATA", filepath.Join(env.Home, "another"))
	if r := runEnv(t, env, srv.URL, "install", "--executable", exe); r.code != 0 {
		t.Fatalf("second install: %d %s %s", r.code, r.stdout, r.stderr)
	}
	config, err := winservice.InstalledConfig(env.ServiceDir)
	if want := filepath.Join(data, "config.json"); err != nil || config != want {
		t.Fatalf("definition names %q, %v; want %q", config, err, want)
	}
}

func TestWindowsFailedInstallReportsTheRegistration(t *testing.T) {
	_, srv := newFakeAPI(t)
	task := &fakeTask{fail: "/Create"}
	env, exe := windowsEnv(t, task)
	r := runEnv(t, env, srv.URL, "install", "--executable", exe, "--data", filepath.Join(env.Home, "data"))
	if r.code != ExitError || !strings.Contains(r.stderr, "schtasks /Create") {
		t.Fatalf("install: %d %s %s", r.code, r.stdout, r.stderr)
	}
}

func TestWindowsFailedInstallKeepsTheDefinition(t *testing.T) {
	_, srv := newFakeAPI(t)
	task := &fakeTask{}
	env, exe := windowsEnv(t, task)
	first := filepath.Join(env.Home, "first")
	if r := runEnv(t, env, srv.URL, "install", "--executable", exe, "--data", first); r.code != 0 {
		t.Fatalf("first install: %d %s %s", r.code, r.stdout, r.stderr)
	}
	before, err := os.ReadFile(winservice.DefinitionPath(env.ServiceDir))
	if err != nil {
		t.Fatal(err)
	}
	task.fail = "/Create"
	r := runEnv(t, env, srv.URL, "install", "--executable", exe, "--data", filepath.Join(env.Home, "second"))
	if r.code != ExitError {
		t.Fatalf("second install: %d %s %s", r.code, r.stdout, r.stderr)
	}
	after, err := os.ReadFile(winservice.DefinitionPath(env.ServiceDir))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a failed install changed the definition")
	}
}

// A foreground flats serve on the data directory of a stopped service holds the
// lock, and the service stays stopped.
func TestWindowsStatusDoesNotTakeAForegroundServerForTheService(t *testing.T) {
	_, srv := newFakeAPI(t)
	task := &fakeTask{}
	env, exe := windowsEnv(t, task)
	data := filepath.Join(env.Home, "data")
	if r := runEnv(t, env, srv.URL, "install", "--executable", exe, "--data", data); r.code != 0 {
		t.Fatalf("install: %d %s %s", r.code, r.stdout, r.stderr)
	}
	lock, err := os.OpenFile(filepath.Join(data, "flats.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := filelock.TryLock(lock); err != nil {
		t.Fatal(err)
	}
	if _, err := lock.WriteString("4242\n"); err != nil {
		t.Fatal(err)
	}
	r := runEnv(t, env, srv.URL, "status")
	if !strings.Contains(r.stdout, "service: scheduled task "+winservice.Task+", stopped") {
		t.Fatalf("status: %d %s %s", r.code, r.stdout, r.stderr)
	}
}

func TestWindowsStatusReportsAFailedQuery(t *testing.T) {
	_, srv := newFakeAPI(t)
	task := &fakeTask{fail: "/Query"}
	env, _ := windowsEnv(t, task)
	r := runEnv(t, env, srv.URL, "status")
	if strings.Contains(r.stdout, "not installed") || !strings.Contains(r.stdout, "service: scheduled task "+winservice.Task+": schtasks /Query") {
		t.Fatalf("status: %d %s %s", r.code, r.stdout, r.stderr)
	}
}

func TestWindowsStatusReportsAMissingDefinition(t *testing.T) {
	_, srv := newFakeAPI(t)
	task := &fakeTask{exists: true}
	env, _ := windowsEnv(t, task)
	r := runEnv(t, env, srv.URL, "status")
	if strings.Contains(r.stdout, "stopped") || !strings.Contains(r.stdout, winservice.ErrNoDefinition.Error()) {
		t.Fatalf("status: %d %s %s", r.code, r.stdout, r.stderr)
	}
}
