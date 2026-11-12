package winservice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/filelock"
)

func base(t *testing.T, run Runner) Options {
	t.Helper()
	data := t.TempDir()
	return Options{
		Executable: `C:\Program Files\Flats\flats.exe`,
		DataDir:    data,
		Args:       []string{"--config", filepath.Join(data, "config.json")},
		UserID:     "S-1-5-21-1-2-3-1001",
		UserName:   `HOST\me & co`,
		Dir:        t.TempDir(),
		Run:        run,
	}
}

func recorder(calls *[]string) Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
}

func holdLock(t *testing.T, dataDir string, pid int) *os.File {
	t.Helper()
	f, err := os.OpenFile(lockPath(dataDir), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := filelock.TryLock(f); err != nil {
		t.Fatal(err)
	}
	if pid != 0 {
		fmt.Fprintf(f, "%d %s\n", pid, ServiceRole)
	}
	return f
}

func TestRenderKeepsTheServiceProperties(t *testing.T) {
	text, err := Render(base(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<LogonType>InteractiveToken</LogonType>",
		"<RunLevel>LeastPrivilege</RunLevel>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
		"<Interval>PT1M</Interval>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>",
		"<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>",
		"<UserId>S-1-5-21-1-2-3-1001</UserId>",
		"<UserId>HOST\\me &amp; co</UserId>",
		"--watch-parent",
		"--headless ",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

func TestRenderNeedsItsInputs(t *testing.T) {
	for name, change := range map[string]func(*Options){
		"executable": func(o *Options) { o.Executable = "" },
		"data":       func(o *Options) { o.DataDir = "" },
		"user":       func(o *Options) { o.UserID = "" },
	} {
		o := base(t, nil)
		change(&o)
		if _, err := Render(o); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestDefinitionRoundTrips(t *testing.T) {
	for _, exe := range []string{
		`C:\Users\me\AppData\Local\Programs\Flats\flats.exe`,
		`C:\Program Files\Flats\flats.exe`,
		`C:\A & B\flats.exe`,
		`C:\quote"d\flats.exe`,
		`C:\ends with slash\`,
	} {
		o := base(t, recorder(new([]string)))
		o.Executable = exe
		o.DataDir = filepath.Join(t.TempDir(), "with space")
		if _, _, err := Install(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		got, err := InstalledExecutable(o.Dir)
		if err != nil {
			t.Fatal(err)
		}
		if got != exe {
			t.Errorf("executable %q, want %q", got, exe)
		}
		data, err := InstalledDataDir(o.Dir)
		if err != nil {
			t.Fatal(err)
		}
		if data != o.DataDir {
			t.Errorf("data %q, want %q", data, o.DataDir)
		}
	}
}

func TestInstalledConfigFollowsTheDefinition(t *testing.T) {
	o := base(t, recorder(new([]string)))
	o.Args = []string{"--config", filepath.Join(t.TempDir(), "my data", "config.json")}
	if _, _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	got, err := InstalledConfig(o.Dir)
	if err != nil || got != o.Args[1] {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestNoDefinitionIsNoError(t *testing.T) {
	dir := t.TempDir()
	if got, err := InstalledExecutable(dir); got != "" || err != nil {
		t.Fatalf("executable %q, %v", got, err)
	}
	if got, err := InstalledDataDir(dir); got != "" || err != nil {
		t.Fatalf("data %q, %v", got, err)
	}
	if got, err := InstalledConfig(dir); got != "" || err != nil {
		t.Fatalf("config %q, %v", got, err)
	}
}

func TestInstallRegistersThenRuns(t *testing.T) {
	var calls []string
	o := base(t, recorder(&calls))
	path, started, err := Install(context.Background(), o)
	if err != nil || !started {
		t.Fatalf("started %v, err %v", started, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if b[0] != 0xff || b[1] != 0xfe {
		t.Fatalf("definition has no UTF-16 byte order mark: % x", b[:4])
	}
	want := []string{
		"schtasks.exe /Create /TN " + Task + " /XML " + path + " /F",
		"schtasks.exe /Run /TN " + Task,
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestInstallReportsAFailedRegistration(t *testing.T) {
	o := base(t, func(context.Context, string, ...string) ([]byte, error) {
		return []byte("ERROR: Access is denied."), errors.New("exit status 1")
	})
	if _, _, err := Install(context.Background(), o); err == nil || !strings.Contains(err.Error(), "Access is denied") {
		t.Fatalf("got %v", err)
	}
}

// Without an interactive session the task registers and cannot run yet.
func TestInstallWithoutASessionIsNoFailure(t *testing.T) {
	o := base(t, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "/Run" {
			return []byte("ERROR"), errors.New("exit status 1")
		}
		return nil, nil
	})
	_, started, err := Install(context.Background(), o)
	if err != nil || started {
		t.Fatalf("started %v, err %v", started, err)
	}
}

func TestStopDisablesThenEndsAndWaits(t *testing.T) {
	var calls []string
	o := base(t, nil)
	lock := holdLock(t, o.DataDir, 4242)
	o.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "/End" {
			lock.Close() // the server takes the end of its console as a stop
		}
		return nil, nil
	}
	o.Kill = func(int) error { t.Fatal("killed a server that stopped"); return nil }
	stopped, err := Stop(context.Background(), o)
	if err != nil || !stopped {
		t.Fatalf("stopped %v, err %v", stopped, err)
	}
	want := []string{"/Query /TN " + Task, "/Change /TN " + Task + " /DISABLE", "/End /TN " + Task}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(calls, "\n"))
	}
}

func TestStopWithoutAServerLeavesTheTaskAlone(t *testing.T) {
	var calls []string
	o := base(t, recorder(&calls))
	stopped, err := Stop(context.Background(), o)
	if err != nil || stopped {
		t.Fatalf("stopped %v, err %v", stopped, err)
	}
	joined := strings.Join(calls, "\n")
	if strings.Contains(joined, "/End") || strings.Contains(joined, "/DISABLE") {
		t.Fatalf("calls:\n%s", joined)
	}
}

// An error that is no exit status is reported: a missing schtasks.exe is not a
// missing task.
func TestStopReportsAMissingSchtasks(t *testing.T) {
	o := base(t, func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("executable file not found")
	})
	if _, err := Stop(context.Background(), o); err == nil {
		t.Fatal("no error")
	}
}

func TestStopEndsAServerThatStaysThroughItsProcessID(t *testing.T) {
	o := base(t, recorder(new([]string)))
	holdLock(t, o.DataDir, 4242)
	o.StopWait = 10 * time.Millisecond
	killed := 0
	o.Kill = func(pid int) error { killed = pid; return nil }
	if _, err := Stop(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if killed != 4242 {
		t.Fatalf("killed %d", killed)
	}
}

func TestStopReportsAServerWithoutAProcessID(t *testing.T) {
	o := base(t, recorder(new([]string)))
	holdLock(t, o.DataDir, 0)
	o.StopWait = 10 * time.Millisecond
	if _, err := Stop(context.Background(), o); err == nil {
		t.Fatal("no error")
	}
}

// A foreground flats serve records no role. It stays up, and the task stays
// enabled: the same commands leave it alone on macOS and Linux.
func TestStopLeavesAForegroundServerAlone(t *testing.T) {
	var calls []string
	o := base(t, recorder(&calls))
	lock := holdLock(t, o.DataDir, 0)
	fmt.Fprintf(lock, "%d\n", 4242)
	o.Kill = func(int) error { t.Fatal("killed a foreground server"); return nil }
	stopped, err := Stop(context.Background(), o)
	if err != nil || stopped {
		t.Fatalf("stopped %v, err %v", stopped, err)
	}
	joined := strings.Join(calls, "\n")
	if strings.Contains(joined, "/End") || strings.Contains(joined, "/DISABLE") {
		t.Fatalf("calls:\n%s", joined)
	}
}

func TestStopWithoutADefinitionEndsTheTask(t *testing.T) {
	var calls []string
	o := base(t, recorder(&calls))
	o.DataDir = ""
	stopped, err := Stop(context.Background(), o)
	if err != nil || stopped {
		t.Fatalf("stopped %v, err %v", stopped, err)
	}
	if !strings.Contains(strings.Join(calls, "\n"), "/End /TN "+Task) {
		t.Fatalf("calls:\n%s", strings.Join(calls, "\n"))
	}
}

func TestRunningIsNotAForegroundServer(t *testing.T) {
	o := base(t, recorder(new([]string)))
	lock := holdLock(t, o.DataDir, 0)
	fmt.Fprintf(lock, "%d\n", 4242)
	if running, err := Running(o); err != nil || running {
		t.Fatalf("running %v, err %v", running, err)
	}
}

func TestRunningWithoutADefinitionSaysSo(t *testing.T) {
	o := base(t, recorder(new([]string)))
	o.DataDir = ""
	if running, err := Running(o); running || !errors.Is(err, ErrNoDefinition) {
		t.Fatalf("running %v, err %v", running, err)
	}
}

// exitError is what a command returns that exits with an error.
func exitError(t *testing.T) error {
	t.Helper()
	var err error
	if runtime.GOOS == "windows" {
		err = exec.Command("cmd.exe", "/c", "exit 1").Run()
	} else {
		err = exec.Command("sh", "-c", "exit 1").Run()
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("no exit error: %v", err)
	}
	return err
}

func TestInstalledTellsAMissingTaskFromAFailedQuery(t *testing.T) {
	failed := exitError(t)
	for _, tt := range []struct {
		name    string
		listErr error
		wantErr bool
	}{{"the task is missing", nil, false}, {"the query failed", failed, true}} {
		t.Run(tt.name, func(t *testing.T) {
			o := base(t, func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if slices.Contains(args, "/TN") {
					return nil, failed
				}
				return nil, tt.listErr
			})
			installed, err := Installed(context.Background(), o)
			if installed || (err != nil) != tt.wantErr {
				t.Fatalf("installed %v, err %v", installed, err)
			}
		})
	}
}

func TestRunningFollowsTheDataDirectoryLock(t *testing.T) {
	o := base(t, recorder(new([]string)))
	if running, err := Running(o); err != nil || running {
		t.Fatalf("no lock file: %v, %v", running, err)
	}
	lock := holdLock(t, o.DataDir, 1)
	if running, err := Running(o); err != nil || !running {
		t.Fatalf("held: %v, %v", running, err)
	}
	lock.Close()
	if running, err := Running(o); err != nil || running {
		t.Fatalf("released: %v, %v", running, err)
	}
}

func TestUninstallRemovesTheTaskAndDefinition(t *testing.T) {
	var calls []string
	o := base(t, recorder(&calls))
	path, _, err := Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	calls = nil
	removed, err := Uninstall(context.Background(), o)
	if err != nil || !removed {
		t.Fatalf("removed %v, err %v", removed, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("definition: %v", err)
	}
	if !strings.Contains(strings.Join(calls, "\n"), "/Delete /TN "+Task+" /F") {
		t.Fatalf("calls:\n%s", strings.Join(calls, "\n"))
	}
}
