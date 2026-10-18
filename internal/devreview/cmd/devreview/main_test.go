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

type cli struct {
	t                       *testing.T
	root, ev, manifest, cfg string
	prompt, copy            string
	stdout, stderr          bytes.Buffer
}

// newCLI builds a repository with a candidate change, a manifest and a
// materialized copy through the CLI itself.
func newCLI(t *testing.T) *cli {
	t.Helper()
	c := &cli{t: t}
	c.root, _ = filepath.EvalSymlinks(t.TempDir())
	c.ev, _ = filepath.EvalSymlinks(t.TempDir())
	git(t, c.root, "init", "-q")
	os.WriteFile(filepath.Join(c.root, "a.txt"), []byte("a\n"), 0o644)
	git(t, c.root, "add", "-A")
	git(t, c.root, "commit", "-q", "-m", "init")
	os.WriteFile(filepath.Join(c.root, "a.txt"), []byte("b\n"), 0o644)
	c.manifest = filepath.Join(c.ev, "s.json")
	c.prompt = filepath.Join(c.ev, "p.txt")
	os.WriteFile(c.prompt, []byte("p\n"), 0o600)
	c.must("snapshot", "-o", c.manifest)
	c.copy = filepath.Join(c.ev, "copy")
	c.must("materialize", "-manifest", c.manifest, "-o", c.copy)
	t.Cleanup(func() {
		filepath.Walk(c.ev, func(p string, _ os.FileInfo, _ error) error { os.Chmod(p, 0o700); return nil })
		exec.Command("git", "-C", c.root, "worktree", "remove", "--force", c.copy).Run()
	})
	return c
}

// run invokes the CLI with -C set to the repository.
func (c *cli) run(args ...string) int {
	c.stdout.Reset()
	c.stderr.Reset()
	return run(append([]string{args[0], "-C", c.root}, args[1:]...), &c.stdout, &c.stderr)
}

func (c *cli) must(args ...string) {
	c.t.Helper()
	if code := c.run(args...); code != exitOK {
		c.t.Fatalf("%v: exit %d\n%s%s", args, code, c.stdout.String(), c.stderr.String())
	}
}

// reviewer configures a reviewer script and returns the run arguments.
func (c *cli) reviewer(name, script string, extra ...string) []string {
	sh := filepath.Join(c.ev, name+".sh")
	os.WriteFile(sh, []byte("#!/bin/sh\ncat >/dev/null\n"+script+"\n"), 0o700)
	cfg := filepath.Join(c.ev, name+".json")
	os.WriteFile(cfg, []byte(`{"reviewers":{"r":{"command":["`+sh+`"],"requested_model":"m-r","timeout":"2s"}}}`), 0o600)
	return append([]string{"run", "-manifest", c.manifest, "-reviewer", "r", "-prompt", c.prompt,
		"-runs", filepath.Join(c.ev, "runs-"+name), "-author", "m-a", "-copy", c.copy, "-config", cfg, "-budget", "5"}, extra...)
}

const metadata = `printf '{"effective_model":"%s","session":"s","source":"t"}' "$MODEL" > "$FLATS_REVIEW_IDENTITY_FILE"`

func TestCLIExitCodes(t *testing.T) {
	c := newCLI(t)
	cases := []struct {
		name, script string
		extra        []string
		want         int
	}{
		{"complete", "MODEL=m-r; " + metadata, nil, exitOK},
		{"unknown identity", "echo 'I am m-r'", nil, exitIncomplete},
		{"ineligible", "MODEL=m-a; " + metadata, nil, exitIncomplete},
		{"surface drift", "MODEL=m-r; " + metadata + `; echo x > "$PWD/extra.txt"`, nil, exitIncomplete},
		{"command failure", "MODEL=m-r; " + metadata + "; exit 3", nil, exitFailed},
		{"timeout", "sleep 10", nil, exitFailed},
	}
	for _, tc := range cases {
		if got := c.run(c.reviewer(strings.ReplaceAll(tc.name, " ", "-"), tc.script, tc.extra...)...); got != tc.want {
			t.Errorf("%s: exit %d, want %d\n%s%s", tc.name, got, tc.want, c.stdout.String(), c.stderr.String())
		}
		// Undo drift a case left in the copy.
		os.Remove(filepath.Join(c.copy, "extra.txt"))
	}

	args := c.reviewer("refused", "MODEL=m-r; "+metadata)
	for i, a := range args {
		if a == "-author" {
			args = append(args[:i:i], args[i+2:]...)
			break
		}
	}
	if got := c.run(args...); got != exitUsage || !strings.Contains(c.stderr.String(), "refused to start") {
		t.Errorf("refusal: exit %d, stderr %q", got, c.stderr.String())
	}
	if got := c.run("run", "-manifest", c.manifest); got != exitUsage {
		t.Errorf("missing flags: exit %d", got)
	}
	if got := c.run("nosuch"); got != exitUsage {
		t.Errorf("unknown command: exit %d", got)
	}
}

func TestCLIVerifyAndReport(t *testing.T) {
	c := newCLI(t)
	c.must("verify", "-manifest", c.manifest)
	c.must("verify", "-manifest", c.manifest, "-copy", c.copy)
	os.WriteFile(filepath.Join(c.root, "a.txt"), []byte("c\n"), 0o644)
	if got := c.run("verify", "-manifest", c.manifest); got != exitFailed || !strings.Contains(c.stdout.String(), "DRIFT") {
		t.Errorf("drift: exit %d, stdout %q", got, c.stdout.String())
	}
	os.WriteFile(filepath.Join(c.copy, "new.txt"), []byte("x\n"), 0o644)
	if got := c.run("verify", "-manifest", c.manifest, "-copy", c.copy); got != exitFailed {
		t.Errorf("copy drift: exit %d", got)
	}
	report := filepath.Join(c.ev, "report.md")
	c.must("report", "-manifest", c.manifest, "-o", report)
	if got := c.run("report", "-manifest", c.manifest, "-o", report); got != exitUsage {
		t.Errorf("report overwrite: exit %d", got)
	}
	if got := c.run("snapshot", "-o", filepath.Join(c.root, "inside.json")); got != exitUsage {
		t.Errorf("manifest inside the repository: exit %d", got)
	}
}

// A reviewer that completes while its evidence cannot be saved must not
// produce exit 0.
func TestRunExitWhenRecordCannotBeSaved(t *testing.T) {
	c := newCLI(t)
	// The reviewer makes its invocation directory read-only before
	// exiting, so the final record cannot be written.
	args := c.reviewer("unsaved", "MODEL=m-r; "+metadata+`; chmod 500 "$(dirname "$FLATS_REVIEW_IDENTITY_FILE")"`)
	if code := c.run(args...); code != exitFailed || !strings.Contains(c.stderr.String(), "saving its record failed") {
		t.Errorf("exit %d, stderr %q", code, c.stderr.String())
	}
}
