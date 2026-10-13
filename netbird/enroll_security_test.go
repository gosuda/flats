package netbird

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnrollRejectsInsecureManagementURLBeforeClientCreation(t *testing.T) {
	cs, mu := install(t)
	tests := []struct {
		name string
		url  string
	}{
		{name: "HTTP", url: "http://netbird.example"},
		{name: "URL credentials", url: "https://" + testSetupKey + "@netbird.example"},
		{name: "URL path", url: "https://netbird.example/other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			_, err := Enroll(context.Background(), dir, tt.url, testSetupKey, "peer")
			if err == nil || !strings.Contains(err.Error(), "HTTPS") {
				t.Fatalf("Enroll(%q) = %v, want HTTPS validation error", tt.url, err)
			}
			if strings.Contains(err.Error(), testSetupKey) {
				t.Fatalf("enrollment error disclosed the setup key: %v", err)
			}
			if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("invalid enrollment created state directory: %v", statErr)
			}
		})
	}
	if last(cs, mu) != nil {
		t.Fatal("invalid enrollment constructed a client")
	}
}

func TestNewEmbedClientRejectsInsecureManagementURL(t *testing.T) {
	for _, tt := range []struct {
		name     string
		setupKey string
	}{
		{name: "enrollment", setupKey: testSetupKey},
		{name: "restart"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newEmbedClient(clientOptions{
				DeviceName:    "peer",
				ManagementURL: "http://netbird.example",
				SetupKey:      tt.setupKey,
				ConfigPath:    filepath.Join(t.TempDir(), "peer.json"),
			})
			if err == nil || !strings.Contains(err.Error(), "HTTPS") {
				t.Fatalf("newEmbedClient returned %v, want HTTPS validation error", err)
			}
		})
	}
}
