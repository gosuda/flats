package netbird

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNewRejectsInsecureManagementURL(t *testing.T) {
	for _, url := range []string{"http://netbird.example", "https://" + testSetupKey + "@netbird.example", "https://netbird.example/other"} {
		_, err := New(Config{Dir: t.TempDir(), ManagementURL: url})
		if err == nil || !strings.Contains(err.Error(), "HTTPS") || strings.Contains(err.Error(), testSetupKey) {
			t.Fatalf("New(%q) = %v, want an HTTPS validation error without the key", url, err)
		}
	}
}

func TestNewEmbedClientRejectsInsecureManagementURL(t *testing.T) {
	for _, key := range []string{testSetupKey, ""} {
		_, err := newEmbedClient(clientOptions{
			DeviceName:    "peer",
			ManagementURL: "http://netbird.example",
			SetupKey:      key,
			ConfigPath:    filepath.Join(t.TempDir(), "peer.json"),
		})
		if err == nil || !strings.Contains(err.Error(), "HTTPS") {
			t.Fatalf("newEmbedClient returned %v, want HTTPS validation error", err)
		}
	}
}
