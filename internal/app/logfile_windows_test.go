package app

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/filelock"
)

const logHelperEnv = "FLATS_TEST_LOG_HELPER"

// TestMain is not defined in this package, so the helper runs as a test: it
// redirects the log to the file its parent names and then does what the parent
// checks.
func TestLogHelper(t *testing.T) {
	path := os.Getenv(logHelperEnv)
	if path == "" {
		t.Skip("helper of the tests below")
	}
	if err := redirectLog(path); err != nil {
		t.Fatal(err)
	}
	log.Printf("a line of the log")
	panic("the panic of the helper")
}

func TestLogFileHoldsThePanicOfTheGoRuntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "serve.log")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLogHelper$")
	cmd.Env = append(os.Environ(), logHelperEnv+"="+path)
	if err := cmd.Run(); err == nil {
		t.Fatal("the helper did not fail")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a line of the log", "panic: the panic of the helper"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("no %q in the log:\n%s", want, b)
		}
	}
}

func TestLogFileRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.log")
	if err := os.WriteFile(path, make([]byte, maxLogBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.f.Close()
	st, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != maxLogBytes+1 {
		t.Fatalf("rotated size %d", st.Size())
	}
	if _, err := l.Write([]byte("next\n")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "next\n" {
		t.Fatalf("the log holds %d bytes", len(b))
	}
}

func TestLogFileStaysBoundedWhileTheServerRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.log")
	l, err := openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.f.Close()
	line := []byte(strings.Repeat("x", 1<<20))
	for i := 0; i < 25; i++ {
		if _, err := l.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{path, path + ".1"} {
		st, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() > maxLogBytes {
			t.Errorf("%s has %d bytes", name, st.Size())
		}
	}
}

func TestFailedRotationSaysSoAndContinues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.log")
	if err := os.Mkdir(path+".1", 0o700); err != nil { // the older file cannot be written
		t.Fatal(err)
	}
	l, err := openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.f.Close()
	l.size = maxLogBytes
	if _, err := l.Write([]byte("after\n")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "the log did not rotate") || !strings.Contains(string(b), "after") {
		t.Fatalf("log: %q", b)
	}
}

func TestRecordPIDLeavesOneNumber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flats.lock")
	if err := os.WriteFile(path, []byte("99999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recordPID(f)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("lock file holds %q", b)
	}
}

func TestStatusCheckDoesNotFailAStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flats.lock")
	probe, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if err := filelock.TryLock(probe); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(60 * time.Millisecond)
		filelock.Unlock(probe)
	}()
	f, err := lockDataDir(dir)
	if err != nil {
		t.Fatalf("the start failed behind a status check: %v", err)
	}
	f.Close()
}

func TestServiceFlagsAreAcceptedWithConfig(t *testing.T) {
	o, err := ParseServeFlags([]string{"--config", filepath.Join(t.TempDir(), "config.json"), "--log-file", "x.log", "--watch-parent"})
	if err != nil {
		t.Fatal(err)
	}
	if o.LogFile != "x.log" || !o.WatchParent {
		t.Fatalf("options: %+v", o)
	}
}

func TestWatchParentStopsWhenTheParentEnds(t *testing.T) {
	if os.Getenv(watchHelperEnv) != "" {
		return
	}
	stopped := filepath.Join(t.TempDir(), "stopped")
	marker := exec.Command(os.Args[0], "-test.run=^TestWatchHelperParent$")
	marker.Env = append(os.Environ(), watchHelperEnv+"="+stopped)
	stdout, err := marker.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := marker.Start(); err != nil {
		t.Fatal(err)
	}
	defer marker.Process.Kill()
	buf := make([]byte, 64)
	stdout.Read(buf) // the parent reports that its child runs
	if err := marker.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stopped); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the child did not notice the end of its parent")
}

const watchHelperEnv = "FLATS_TEST_WATCH_HELPER"

// TestWatchHelperParent starts a child that watches it, and waits.
func TestWatchHelperParent(t *testing.T) {
	path := os.Getenv(watchHelperEnv)
	if path == "" {
		t.Skip("helper of TestWatchParentStopsWhenTheParentEnds")
	}
	child := exec.Command(os.Args[0], "-test.run=^TestWatchHelperChild$")
	child.Env = os.Environ()
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString("started\n")
	time.Sleep(time.Minute)
}

// TestWatchHelperChild records that its parent ended.
func TestWatchHelperChild(t *testing.T) {
	path := os.Getenv(watchHelperEnv)
	if path == "" {
		t.Skip("helper of TestWatchParentStopsWhenTheParentEnds")
	}
	done := make(chan struct{})
	if err := watchParent(t.Context(), func() { close(done) }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		os.WriteFile(path, nil, 0o600)
	case <-time.After(30 * time.Second):
		t.Fatal("no end of the parent")
	}
}
