package systemd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderQuotesAndRestarts(t *testing.T) {
	u, err := Render(Options{Executable: "/opt/flats bin/flats", DataDir: "/var/lib/flats", Args: []string{"--config", "/etc/flats dir/config.json"}, Env: map[string]string{"PATH": "/usr/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	// The unit runs from config.json alone, and exit 78 (fix the config)
	// does not restart in a loop.
	for _, want := range []string{"ExecStart=\"/opt/flats bin/flats\" serve --config \"/etc/flats dir/config.json\"\n", "Restart=always", "RestartPreventExitStatus=78", "WantedBy=default.target", `Environment=PATH=/usr/bin`} {
		if !strings.Contains(u, want) {
			t.Errorf("unit missing %q:\n%s", want, u)
		}
	}
	if q := quote("a$b%c"); q != `"a$$b%%c"` {
		t.Errorf("quote = %s", q)
	}
}

func TestInstallUninstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	var calls []string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return []byte("active\n"), nil
	}
	data := filepath.Join(home, "data")
	path, err := Install(context.Background(), Options{Executable: "/usr/local/bin/flats", DataDir: data, Home: home, Run: run})
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(home, ".config/systemd/user/flats.service") {
		t.Fatalf("path %s", path)
	}
	if st, _ := os.Stat(data); st.Mode().Perm() != 0o700 {
		t.Fatalf("data dir mode %v", st.Mode())
	}
	if strings.Join(calls, ";") != "systemctl --user daemon-reload;systemctl --user enable flats.service;systemctl --user restart flats.service" {
		t.Fatalf("calls %v", calls)
	}
	if s, _ := Active(context.Background(), Options{Home: home, Run: run}); s != "active" {
		t.Fatalf("active %q", s)
	}
	removed, err := Uninstall(context.Background(), Options{Home: home, Run: run})
	if err != nil || !removed {
		t.Fatalf("uninstall %v %v", removed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("unit file left behind")
	}
}

func TestInstallUpdateRetainsServeArgs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	home := t.TempDir()
	opts := Options{Home: home, DataDir: filepath.Join(home, "data"), Args: []string{"--authkey-file", filepath.Join(home, "auth keys", "key")}, Run: func(context.Context, string, ...string) ([]byte, error) { return nil, nil }}
	for _, exe := range []string{"/opt/flats-v1", "/opt/flats-v2"} {
		opts.Executable = exe
		path, err := Install(t.Context(), opts)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "ExecStart="+exe) || !strings.Contains(string(raw), "--authkey-file "+quote(opts.Args[1])) {
			t.Fatal("update lost serve arguments")
		}
	}
}

func TestStopKeepsUnitAndStartRuns(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	home := t.TempDir()
	state := "active"
	var calls []string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[1] {
		case "is-active":
			return []byte(state + "\n"), nil
		case "stop":
			state = "inactive"
		case "start", "restart":
			state = "active"
		}
		return nil, nil
	}
	opts := Options{Executable: "/usr/local/bin/flats", DataDir: filepath.Join(home, "data"), Home: home, Run: run}
	if stopped, err := Stop(context.Background(), opts); err != nil || stopped || len(calls) != 0 {
		t.Fatalf("stop without a unit: %v %v %v", stopped, err, calls)
	}
	path, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	calls = nil
	if stopped, err := Stop(context.Background(), opts); err != nil || !stopped || state != "inactive" {
		t.Fatalf("stop: %v %v %s", stopped, err, state)
	}
	if stopped, _ := Stop(context.Background(), opts); stopped {
		t.Fatal("stopped an inactive unit")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unit removed: %v", err)
	}
	if err := Start(context.Background(), opts); err != nil || state != "active" {
		t.Fatalf("start: %v %s", err, state)
	}
	if strings.Join(calls, ";") != "--user is-active flats.service;--user stop flats.service;--user is-active flats.service;--user daemon-reload;--user start flats.service" {
		t.Fatalf("calls %v", calls)
	}
}
