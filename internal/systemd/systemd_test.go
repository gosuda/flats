package systemd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderQuotesAndRestarts(t *testing.T) {
	u, err := Render(Options{Executable: "/opt/flats bin/flats", DataDir: "/var/lib/flats", Args: []string{"--network", "local"}, Env: map[string]string{"PATH": "/usr/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`ExecStart="/opt/flats bin/flats" serve --data /var/lib/flats --network local`, "Restart=always", "WantedBy=default.target", `Environment=PATH=/usr/bin`} {
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

func TestInstallUpdateRetainsNoninteractiveCredentialFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	home := t.TempDir()
	opts := Options{Home: home, DataDir: filepath.Join(home, "data"), Args: []string{"--operator-credential-file", filepath.Join(home, "operator credentials", "credential")}, Run: func(context.Context, string, ...string) ([]byte, error) { return nil, nil }}
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
		if !strings.Contains(string(raw), "ExecStart="+exe) || !strings.Contains(string(raw), "--operator-credential-file "+quote(opts.Args[1])) {
			t.Fatal("update lost noninteractive source")
		}
	}
}
