//go:build unix

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// A reviewer that completes while its evidence cannot be saved must not
// produce exit 0.
func TestRunExitWhenRecordCannotBeSaved(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	ev, _ := filepath.EvalSymlinks(t.TempDir())
	git(t, root, "init", "-q")
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("a\n"), 0o644)
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "init")
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("b\n"), 0o644)

	runs := filepath.Join(ev, "runs")
	// The reviewer makes the evidence directory read-only before exiting,
	// so the final record cannot be written.
	script := "#!/bin/sh\ncat >/dev/null\nprintf '{\"effective_model\":\"m-r\",\"session\":\"s\",\"source\":\"t\"}' > \"$FLATS_REVIEW_IDENTITY_FILE\"\nchmod 500 \"$(dirname \"$FLATS_REVIEW_IDENTITY_FILE\")\"\n"
	sh := filepath.Join(ev, "r.sh")
	os.WriteFile(sh, []byte(script), 0o700)
	cfg := filepath.Join(ev, "cfg.json")
	os.WriteFile(cfg, []byte(`{"reviewers":{"r":{"command":["`+sh+`"],"requested_model":"m-r"}}}`), 0o600)
	prompt := filepath.Join(ev, "p.txt")
	os.WriteFile(prompt, []byte("p\n"), 0o600)
	var out, errb bytes.Buffer
	if code := run([]string{"snapshot", "-C", root, "-o", filepath.Join(ev, "s.json")}, &out, &errb); code != exitOK {
		t.Fatalf("snapshot exit %d: %s", code, errb.String())
	}
	t.Cleanup(func() {
		filepath.Walk(runs, func(p string, _ os.FileInfo, _ error) error { os.Chmod(p, 0o700); return nil })
	})
	code := run([]string{"run", "-C", root, "-manifest", filepath.Join(ev, "s.json"), "-reviewer", "r", "-prompt", prompt,
		"-runs", runs, "-author", "m-a", "-writes-prevented", "test", "-config", cfg, "-budget", "1"}, &out, &errb)
	if code != exitFailed || !strings.Contains(errb.String(), "saving its record failed") {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
}
