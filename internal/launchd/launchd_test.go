package launchd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type call struct {
	name string
	args []string
}

func TestMain(m *testing.M) {
	settleTimeout, settlePoll, retryDelay = 500*time.Millisecond, time.Millisecond, time.Millisecond
	os.Exit(m.Run())
}

// fakeLaunchctl models one launchd service: bootstrap loads it, bootout
// unloads it (after `lingering` more print calls still see it), and print
// fails while it is not loaded. Subcommands in fail always fail; those in
// failOnce fail the first time only.
type fakeLaunchctl struct {
	calls     []call
	fail      map[string]bool
	failOnce  map[string]bool
	print     string
	loaded    bool
	lingering int
}

func (f *fakeLaunchctl) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, call{name, args})
	sub := args[0]
	errOut := func() ([]byte, error) {
		return []byte(sub + " failed: 5: Input/output error"), errors.New("exit status 5")
	}
	if f.fail[sub] {
		return errOut()
	}
	if f.failOnce[sub] {
		delete(f.failOnce, sub)
		return errOut()
	}
	switch sub {
	case "print":
		if !f.loaded {
			if f.lingering == 0 {
				return []byte("Could not find service"), errors.New("exit status 113")
			}
			f.lingering--
		}
		return []byte(f.print), nil
	case "bootout":
		if !f.loaded {
			return []byte("Boot-out failed: 3: No such process"), errors.New("exit status 3")
		}
		f.loaded = false
	case "bootstrap", "load":
		f.loaded = true
	}
	return nil, nil
}

func (f *fakeLaunchctl) subcommands() []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, c.args[0])
	}
	return out
}

func testOpts(t *testing.T, f *fakeLaunchctl) Options {
	t.Helper()
	home := t.TempDir()
	exe := filepath.Join(home, "bin", "flats")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Options{
		Executable: exe,
		Args:       []string{"--config", filepath.Join(home, "data & more", "config.json")},
		DataDir:    filepath.Join(home, "data & more"),
		Env:        map[string]string{"PATH": "/usr/bin:/bin", "FLATS_X": "<a>"},
		Home:       home,
		UID:        501,
		Run:        f.run,
	}
}

func TestPlistContent(t *testing.T) {
	opts := testOpts(t, &fakeLaunchctl{})
	job, err := BuildJob(opts)
	if err != nil {
		t.Fatal(err)
	}
	// flats install runs the agent from config.json alone.
	if !slices.Equal(job.Program, []string{opts.Executable, "serve", "--config", filepath.Join(opts.DataDir, "config.json")}) {
		t.Fatalf("program = %q", job.Program)
	}
	p := string(job.Plist())
	for _, want := range []string{
		"<key>Label</key>\n\t<string>dev.flats.serve</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>KeepAlive</key>\n\t<true/>",
		"<string>serve</string>",
		"data &amp; more/logs/serve.out.log</string>",
		"data &amp; more/logs/serve.err.log</string>",
		"<key>PATH</key>\n\t\t<string>/usr/bin:/bin</string>",
		"<string>&lt;a&gt;</string>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "data & more") {
		t.Error("unescaped ampersand in plist")
	}

	// plutil validates the XML and round-trips it to JSON for exact checks.
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Log("plutil not available; skipping lint")
		return
	}
	file := filepath.Join(t.TempDir(), "job.plist")
	if err := os.WriteFile(file, job.Plist(), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(plutil, "-lint", file).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v: %s", err, out)
	}
	out, err := exec.Command(plutil, "-convert", "json", "-o", "-", file).Output()
	if err != nil {
		t.Fatalf("plutil -convert: %v", err)
	}
	var got struct {
		Label                string
		ProgramArguments     []string
		RunAtLoad, KeepAlive bool
		StandardOutPath      string
		StandardErrorPath    string
		EnvironmentVariables map[string]string
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Label != Label || !got.RunAtLoad || !got.KeepAlive {
		t.Errorf("decoded plist = %+v", got)
	}
	if !reflect.DeepEqual(got.ProgramArguments, job.Program) {
		t.Errorf("ProgramArguments = %q, want %q", got.ProgramArguments, job.Program)
	}
	if got.StandardOutPath != job.Stdout || got.EnvironmentVariables["FLATS_X"] != "<a>" {
		t.Errorf("decoded plist = %+v", got)
	}
}

func TestBuildJobDefaultsPath(t *testing.T) {
	opts := testOpts(t, &fakeLaunchctl{})
	opts.Env = nil
	job, err := BuildJob(opts)
	if err != nil {
		t.Fatal(err)
	}
	if job.Env["PATH"] != DefaultPath {
		t.Fatalf("PATH = %q", job.Env["PATH"])
	}
	if _, err := BuildJob(Options{}); err == nil {
		t.Fatal("expected error without a data dir")
	}
}

func TestInstallBootstrap(t *testing.T) {
	f := &fakeLaunchctl{}
	opts := testOpts(t, f)
	res, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != "bootstrap" {
		t.Fatalf("method = %q", res.Method)
	}
	want := PlistPath(opts.Home)
	if res.PlistPath != want || !strings.HasSuffix(want, "Library/LaunchAgents/dev.flats.serve.plist") {
		t.Fatalf("plist path = %q", res.PlistPath)
	}
	info, err := os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(opts.DataDir, "logs")); err != nil {
		t.Errorf("log dir not created: %v", err)
	}
	if info, err := os.Stat(opts.DataDir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("data dir must be created private: %v %v", info.Mode(), err)
	}
	if got := f.subcommands(); !reflect.DeepEqual(got, []string{"bootout", "enable", "bootstrap"}) {
		t.Fatalf("calls = %v", got)
	}
	last := f.calls[2]
	if last.name != "launchctl" || !reflect.DeepEqual(last.args, []string{"bootstrap", "gui/501", want}) {
		t.Fatalf("bootstrap call = %+v", last)
	}
	if f.calls[0].args[1] != "gui/501/dev.flats.serve" {
		t.Fatalf("bootout target = %q", f.calls[0].args[1])
	}
}

func TestInstallFallsBackToLoad(t *testing.T) {
	f := &fakeLaunchctl{fail: map[string]bool{"bootstrap": true}}
	opts := testOpts(t, f)
	res, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != "load" {
		t.Fatalf("method = %q", res.Method)
	}
	last := f.calls[len(f.calls)-1]
	if !reflect.DeepEqual(last.args, []string{"load", "-w", res.PlistPath}) {
		t.Fatalf("fallback call = %+v", last)
	}

	if got := f.subcommands(); !reflect.DeepEqual(got, []string{"bootout", "enable", "bootstrap", "bootstrap", "load"}) {
		t.Fatalf("calls = %v", got)
	}

	f2 := &fakeLaunchctl{fail: map[string]bool{"bootstrap": true, "load": true}}
	_, err = Install(context.Background(), testOpts(t, f2))
	if err == nil || !strings.Contains(err.Error(), "bootstrap failed") || !strings.Contains(err.Error(), "load failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestUninstall(t *testing.T) {
	f := &fakeLaunchctl{}
	opts := testOpts(t, f)
	if _, err := Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	removed, err := Uninstall(context.Background(), opts)
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	if _, err := os.Stat(PlistPath(opts.Home)); !os.IsNotExist(err) {
		t.Fatalf("plist still present: %v", err)
	}
	if got := f.subcommands(); !reflect.DeepEqual(got, []string{"bootout"}) {
		t.Fatalf("calls = %v", got)
	}
	// Second uninstall: nothing loaded, nothing to remove, still no error.
	f.fail = map[string]bool{"bootout": true}
	removed, err = Uninstall(context.Background(), opts)
	if err != nil || removed {
		t.Fatalf("second uninstall: removed=%v err=%v", removed, err)
	}
}

func TestUninstallFallsBackToUnload(t *testing.T) {
	f := &fakeLaunchctl{}
	opts := testOpts(t, f)
	if _, err := Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	f.fail = map[string]bool{"bootout": true}
	if _, err := Uninstall(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if got := f.subcommands(); !reflect.DeepEqual(got, []string{"bootout", "unload"}) {
		t.Fatalf("calls = %v", got)
	}
}

const printOutput = `gui/501/dev.flats.serve = {
	active count = 1
	path = /Users/x/Library/LaunchAgents/dev.flats.serve.plist
	type = LaunchAgent
	state = running

	program = /usr/local/bin/flats
	arguments = {
		/usr/local/bin/flats
		serve
	}

	environment = {
		state = should-be-ignored
	}

	pid = 4242
	last exit code = (never exited)
}
`

func TestStatus(t *testing.T) {
	f := &fakeLaunchctl{print: printOutput, loaded: true}
	opts := testOpts(t, f)
	st, err := GetStatus(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if st.Installed || !st.Loaded || st.State != "running" || st.PID != 4242 || st.LastExit != "(never exited)" {
		t.Fatalf("status = %+v", st)
	}
	if !reflect.DeepEqual(f.calls[0].args, []string{"print", "gui/501/dev.flats.serve"}) {
		t.Fatalf("call = %+v", f.calls[0])
	}

	f.loaded = false
	st, err = GetStatus(context.Background(), opts)
	if err != nil || st.Loaded || st.State != "" {
		t.Fatalf("not loaded: %+v %v", st, err)
	}
}

// A reinstall must not bootstrap while the old job is still leaving launchd.
func TestInstallWaitsForBootout(t *testing.T) {
	f := &fakeLaunchctl{loaded: true, lingering: 2}
	res, err := Install(context.Background(), testOpts(t, f))
	if err != nil || res.Method != "bootstrap" {
		t.Fatalf("method=%q err=%v", res.Method, err)
	}
	want := []string{"bootout", "print", "print", "print", "enable", "bootstrap"}
	if got := f.subcommands(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if !f.loaded {
		t.Fatal("service not loaded after install")
	}
}

func TestInstallRetriesBootstrapOnce(t *testing.T) {
	f := &fakeLaunchctl{failOnce: map[string]bool{"bootstrap": true}}
	res, err := Install(context.Background(), testOpts(t, f))
	if err != nil || res.Method != "bootstrap" {
		t.Fatalf("method=%q err=%v", res.Method, err)
	}
	if got := f.subcommands(); !reflect.DeepEqual(got, []string{"bootout", "enable", "bootstrap", "bootstrap"}) {
		t.Fatalf("calls = %v", got)
	}
}

// The plist keeps a symlinked executable path: package managers replace the
// link target on upgrade, and a resolved path would point at a deleted file.
func TestBuildJobKeepsSymlinkedExecutable(t *testing.T) {
	opts := testOpts(t, &fakeLaunchctl{})
	link := filepath.Join(t.TempDir(), "flats")
	if err := os.Symlink(opts.Executable, link); err != nil {
		t.Fatal(err)
	}
	opts.Executable = link
	job, err := BuildJob(opts)
	if err != nil {
		t.Fatal(err)
	}
	if job.Program[0] != link {
		t.Fatalf("program[0] = %q, want the link %q", job.Program[0], link)
	}
}

func TestInstallUpdateRetainsNoninteractiveCredentialFile(t *testing.T) {
	fake := &fakeLaunchctl{}
	opts := testOpts(t, fake)
	credential := filepath.Join(opts.Home, "operator credentials", "credential")
	opts.Args = append(opts.Args, "--operator-credential-file", credential)
	for _, exe := range []string{opts.Executable, filepath.Join(opts.Home, "flats-updated")} {
		opts.Executable = exe
		res, err := Install(t.Context(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Job.Program[len(res.Job.Program)-2:]; !reflect.DeepEqual(got, []string{"--operator-credential-file", credential}) {
			t.Fatal(got)
		}
		raw, err := os.ReadFile(res.PlistPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "operator-credential-file") || !strings.Contains(string(raw), "operator credentials/credential") {
			t.Fatal("installed configuration lost credential source")
		}
	}
}

func TestStopKeepsPlistAndStartReloads(t *testing.T) {
	f := &fakeLaunchctl{}
	opts := testOpts(t, f)
	if stopped, err := Stop(context.Background(), opts); err != nil || stopped || len(f.calls) != 0 {
		t.Fatalf("stop without an agent: %v %v calls=%v", stopped, err, f.calls)
	}
	if _, err := Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	f.calls, f.lingering = nil, 2
	stopped, err := Stop(context.Background(), opts)
	if err != nil || !stopped || f.loaded {
		t.Fatalf("stop: %v %v loaded=%v", stopped, err, f.loaded)
	}
	// Stop waits until launchd no longer knows the job.
	if got := f.subcommands(); !reflect.DeepEqual(got, []string{"print", "bootout", "print", "print", "print"}) {
		t.Fatalf("calls = %v", got)
	}
	if _, err := os.Stat(PlistPath(opts.Home)); err != nil {
		t.Fatalf("plist removed: %v", err)
	}
	if err := Start(context.Background(), opts); err != nil || !f.loaded {
		t.Fatalf("start: %v loaded=%v", err, f.loaded)
	}
}
