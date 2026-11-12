package selfupdate

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const runningHelperEnv = "FLATS_TEST_RUNNING_HELPER"

func TestRunningHelper(t *testing.T) {
	if os.Getenv(runningHelperEnv) == "" {
		t.Skip("helper of TestCommitExecutableReplacesARunningBinary")
	}
	os.Stdout.WriteString("running\n")
	time.Sleep(time.Minute)
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
}

func TestCommitExecutableReplacesARunningBinary(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "flats.exe")
	copyFile(t, os.Args[0], target)
	cmd := exec.Command(target, "-test.run=^TestRunningHelper$")
	cmd.Env = append(os.Environ(), runningHelperEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop := func() { cmd.Process.Kill(); cmd.Wait() }
	defer stop()
	buf := make([]byte, 64)
	if _, err := stdout.Read(buf); err != nil {
		t.Fatal(err)
	}

	commitNew := func(body string) {
		t.Helper()
		staged := filepath.Join(dir, ".stage")
		if err := os.WriteFile(staged, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := commitExecutable(context.Background(), staged, target); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != body {
			t.Fatalf("target holds %q", got)
		}
	}
	asides := func() []string {
		t.Helper()
		names, err := filepath.Glob(escapeGlob(target) + ".old-*")
		if err != nil {
			t.Fatal(err)
		}
		return names
	}

	commitNew("first")
	if got := asides(); len(got) != 1 {
		t.Fatalf("the running binary must stay aside: %v", got)
	}
	// A second update while the old image still runs must work too.
	commitNew("second")
	if got := asides(); len(got) != 1 {
		t.Fatalf("after a second update: %v", got)
	}
	stop()
	commitNew("third")
	if got := asides(); len(got) != 0 {
		t.Fatalf("an aside that no longer runs stayed: %v", got)
	}
}

func TestCommitExecutableRestoresTheTargetOnFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "flats.exe")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := commitExecutable(context.Background(), filepath.Join(dir, "missing"), target); err == nil {
		t.Fatal("no error")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Fatalf("target holds %q", got)
	}
}

func TestEscapeGlobKeepsALiteralPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a[1]")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "flats.exe.old-1")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	names, err := filepath.Glob(escapeGlob(filepath.Join(dir, "flats.exe")) + ".old-*")
	if err != nil || len(names) != 1 {
		t.Fatalf("names %v, err %v", names, err)
	}
}
