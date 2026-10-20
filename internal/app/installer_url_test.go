package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise only the installer's URL reader; never install or touch a service.
func TestInstallerConfigURL(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(source), "\nconfig_url() {\n")
	if !ok {
		t.Fatal("config_url function not found")
	}
	body, _, ok := strings.Cut(rest, "\n}\n")
	if !ok {
		t.Fatal("config_url function end not found")
	}
	script := "set -eu\nDEFAULT_LISTEN=127.0.0.1:7878\nconfig_url() {\n" + body + "\n}\nconfig_url \"$1\"\n"
	for _, tc := range []struct{ name, raw, want string }{
		{"canonical", `{"schema_version":5,"host":{"console_addr":"127.0.0.1:9000"}}`, "http://127.0.0.1:9000\n"},
		{"legacy", `{"schema_version":4,"host":{"management_addr":"[::1]:9001"}}`, "http://[::1]:9001\n"},
		{"canonical first", `{"host":{"console_addr":"127.0.0.1:9002","management_addr":"127.0.0.1:9003"}}`, "http://127.0.0.1:9002\n"},
		{"canonical last", `{"host":{"management_addr":"127.0.0.1:9003","console_addr":"127.0.0.1:9002"}}`, "http://127.0.0.1:9002\n"},
		{"empty canonical", `{"host":{"console_addr":"","management_addr":"127.0.0.1:9003"}}`, "http://127.0.0.1:7878\n"},
		{"escaped canonical", `{"host":{"console_addr":"localhost\\:9002","management_addr":"127.0.0.1:9003"}}`, "http://127.0.0.1:7878\n"},
		{"multiline", "{\r\n  \"host\": {\r\n    \"console_addr\": \"localhost:9004\"\r\n  }\r\n}", "http://localhost:9004\n"},
		{"default", `{"schema_version":5,"host":{}}`, "http://127.0.0.1:7878\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", script, "installer-url-test", path)
			out, err := cmd.CombinedOutput()
			if err != nil || string(out) != tc.want {
				t.Fatalf("URL = %q (%v), want %q", out, err, tc.want)
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.json")
		out, err := exec.Command("sh", "-c", script, "installer-url-test", path).CombinedOutput()
		if err != nil || string(out) != "http://127.0.0.1:7878\n" {
			t.Fatalf("missing URL = %q (%v)", out, err)
		}
	})

	t.Run("unreadable file", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses file permission checks")
		}
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"host":{"console_addr":"127.0.0.1:9000"}}`), 0000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0600) })
		out, err := exec.Command("sh", "-c", script, "installer-url-test", path).CombinedOutput()
		if err != nil || string(out) != "http://127.0.0.1:7878\n" {
			t.Fatalf("unreadable URL = %q (%v)", out, err)
		}
	})
}
