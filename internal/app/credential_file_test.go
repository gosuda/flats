package app

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperatorCredentialFileFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		mode    os.FileMode
		valid   bool
	}{
		{"valid", strings.Repeat("x", 32) + "\n", 0600, true},
		{"max", strings.Repeat("x", 4096) + "\r\n", 0600, true},
		{"readable_by_group", strings.Repeat("x", 32), 0640, false},
		{"readable_by_all", strings.Repeat("x", 32), 0644, false},
		{"short", "short", 0600, false},
		{"oversized", strings.Repeat("x", 4097), 0600, false},
		{"multiline", strings.Repeat("x", 32) + "\nsecond", 0600, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "operator")
			if err := os.WriteFile(path, []byte(tc.content), tc.mode); err != nil {
				t.Fatal(err)
			}
			got, err := readOperatorCredentialFile(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t err=%v", tc.valid, err)
			}
			if tc.valid && got != strings.TrimRight(tc.content, "\r\n") {
				t.Fatal("credential changed")
			}
			if !tc.valid && got != "" {
				t.Fatal("failure returned credential")
			}
		})
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	os.WriteFile(target, []byte(strings.Repeat("x", 32)), 0600)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, dir, filepath.Join(dir, "missing")} {
		if _, err := readOperatorCredentialFile(path); err == nil {
			t.Fatal("nonregular source accepted", path)
		}
	}
	if _, err := ParseServeFlags([]string{"--operator-credential-stdin", "--operator-credential-file", target}); err == nil {
		t.Fatal("ambiguous credential sources")
	}
	opts := localOptions(filepath.Join(dir, "data"))
	opts.OperatorCredentialFile = target
	opts.OperatorCredential = "another source"
	if h, err := Start(context.Background(), opts); err == nil {
		h.Close()
		t.Fatal("embedding+file sources accepted")
	}
}

func TestNoninteractiveCredentialFileEnablesOperatorSession(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "operator")
	credential := strings.Repeat("fixture-only-", 4)
	if err := os.WriteFile(path, []byte(credential+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := localOptions(filepath.Join(dir, "data"))
	opts.OperatorCredentialFile = path
	h, err := Start(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if h.Operator == nil || h.Opts.OperatorCredential != "" {
		t.Fatal("operator unavailable or retained raw credential")
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	if code := operatorCall(t, h, client, "POST", "/console/api/operator/session", strings.NewReader(`{"credential":"`+credential+`"}`), nil); code != 200 {
		t.Fatal("unlock", code)
	}
	if code := operatorCall(t, h, client, "PUT", "/console/api/settings", strings.NewReader(`{"keep_versions":"3"}`), nil); code != 200 {
		t.Fatal("operator operation", code)
	}
	// A missing/insecure requested source fails before binding any listener.
	h.Close()
	os.Chmod(path, 0644)
	if h, err := Start(t.Context(), opts); err == nil {
		h.Close()
		t.Fatal("insecure file silently started without authority")
	}
}
