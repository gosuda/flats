package netbird

import (
	"strings"
	"testing"
)

func TestValidateManagementURL(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		wantValid bool
	}{
		{name: "default server", url: "", wantValid: true},
		{name: "HTTPS hostname", url: "https://netbird.example", wantValid: true},
		{name: "HTTPS port", url: "https://netbird.example:443", wantValid: true},
		{name: "trailing slash", url: "https://netbird.example/", wantValid: true},
		{name: "IPv6 host", url: "https://[2001:db8::1]:443", wantValid: true},
		{name: "HTTP rejected", url: "http://netbird.example"},
		{name: "HTTP with port", url: "http://netbird.example:80"},
		{name: "FTP rejected", url: "ftp://netbird.example"},
		{name: "scheme relative", url: "//netbird.example"},
		{name: "no host", url: "https://"},
		{name: "no hostname", url: "https://:443"},
		{name: "opaque URL", url: "https:netbird.example"},
		{name: "userinfo", url: "https://user:password@netbird.example"},
		{name: "path", url: "https://netbird.example/other"},
		{name: "query", url: "https://netbird.example?key=secret"},
		{name: "empty query", url: "https://netbird.example?"},
		{name: "fragment", url: "https://netbird.example#fragment"},
		{name: "invalid authority", url: "https://%xx"},
		{name: "leading space", url: " https://netbird.example"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateManagementURL(tt.url)
			if (err == nil) != tt.wantValid {
				t.Fatalf("validateManagementURL(%q) = %v, wantValid=%t", tt.url, err, tt.wantValid)
			}
		})
	}
}

func TestValidateManagementURLDoesNotEchoCredentials(t *testing.T) {
	const secret = "SETUPKEY-do-not-leak-0123456789"
	err := validateManagementURL("https://" + secret + "@netbird.example")
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("invalid management URL returned %v", err)
	}
}
