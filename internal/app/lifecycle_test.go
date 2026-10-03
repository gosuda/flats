package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
)

func localOptions(dir string) Options {
	return Options{DataDir: dir, Listen: "127.0.0.1:0", Network: "local", LocalAddr: "127.0.0.1:0", ConsoleHost: "flats", Runtime: true}
}

func TestDataDirectoryLockSubprocess(t *testing.T) {
	if dir := os.Getenv("FLATS_LOCK_TEST_DIR"); dir != "" {
		h, err := Start(context.Background(), localOptions(dir))
		if err == nil {
			h.Close()
			t.Fatal("second process acquired live directory")
		}
		if !strings.Contains(err.Error(), "already in use") {
			t.Fatal(err)
		}
		return
	}
	dir, err := os.MkdirTemp("/tmp", "flats-lock-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	h, err := Start(context.Background(), localOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	// State belonging to the live process must survive the rejected startup.
	run := filepath.Join(dir, "run")
	if err := os.MkdirAll(run, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(run, "w-live.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	key, err := os.ReadFile(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Start(context.Background(), localOptions(dir)); err == nil {
		other.Close()
		t.Fatal("second host acquired directory")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestDataDirectoryLockSubprocess$", "-test.timeout=20s")
	cmd.Env = []string{"FLATS_LOCK_TEST_DIR=" + dir}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("subprocess: %v\n%s", err, out)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("live socket changed: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "secret.key")); err != nil || string(got) != string(key) {
		t.Fatal("key changed")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h2, err := Start(context.Background(), localOptions(dir))
	if err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
	if err := h2.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStartupFailureReleasesLock(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	o := localOptions(dir)
	o.Listen = ln.Addr().String()
	if h, err := Start(context.Background(), o); err == nil {
		h.Close()
		t.Fatal("occupied listen succeeded")
	}
	o.Listen = "127.0.0.1:0"
	h, err := Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

type failingPrivate struct {
	core.PrivateNet
	err   error
	calls int
}

func (n *failingPrivate) Close() error { n.calls++; return n.err }

type failingPublic struct {
	core.PublicNet
	err   error
	calls int
}

func (n *failingPublic) Close() error { n.calls++; return n.err }

func TestHostCloseReportsErrorsAndContinues(t *testing.T) {
	h := startLocal(t)
	private := h.Private
	pubErr, privErr := errors.New("relay unregister failed"), errors.New("node shutdown failed")
	pub := &failingPublic{err: pubErr}
	priv := &failingPrivate{PrivateNet: private, err: privErr}
	h.Public, h.Private = pub, priv
	defer private.Close()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			err := h.Close()
			if !errors.Is(err, pubErr) || !errors.Is(err, privErr) {
				t.Errorf("Close = %v", err)
			}
			if !strings.Contains(err.Error(), "public network") || !strings.Contains(err.Error(), "private network") {
				t.Errorf("missing component: %v", err)
			}
		})
	}
	wg.Wait()
	if pub.calls != 1 || priv.calls != 1 {
		t.Fatalf("close calls: public %d private %d", pub.calls, priv.calls)
	}
	if _, err := h.Store.GetSetting(context.Background(), "anything", ""); err == nil {
		t.Fatal("store still open after network errors")
	}
	if _, err := net.Dial("tcp", h.Addr()); err == nil {
		t.Fatal("management listener still open")
	}
	lock, err := lockDataDir(h.Opts.DataDir)
	if err != nil {
		t.Fatalf("directory lock not released: %v", err)
	}
	lock.Close()
}

func TestHostCloseTimeoutRetainsLock(t *testing.T) {
	h := startLocal(t)
	h.Public = &failingPublic{err: context.DeadlineExceeded}
	if err := h.Close(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if f, err := lockDataDir(h.Opts.DataDir); err == nil {
		f.Close()
		t.Fatal("released lock while background teardown may be live")
	}
	// The fake owns no background work; explicitly release its retained descriptor.
	retainedLocks.Lock()
	for i, f := range retainedLocks.files {
		if f == h.dataLock {
			retainedLocks.files = append(retainedLocks.files[:i], retainedLocks.files[i+1:]...)
			break
		}
	}
	retainedLocks.Unlock()
	h.dataLock.Close()
}

func TestDataDirectoryLockReleasedOnProcessExit(t *testing.T) {
	if dir := os.Getenv("FLATS_LOCK_HOLDER_DIR"); dir != "" {
		h, err := Start(context.Background(), localOptions(dir))
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		fmt.Println("ready")
		select {} // Parent kills only this disposable helper process.
	}
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestDataDirectoryLockReleasedOnProcessExit$")
	cmd.Env = []string{"FLATS_LOCK_HOLDER_DIR=" + dir}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("helper readiness %q: %v", line, err)
	}
	if f, err := lockDataDir(dir); err == nil {
		f.Close()
		t.Fatal("helper did not hold directory lock")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed helper unexpectedly exited successfully")
	}
	waited = true
	h, err := Start(context.Background(), localOptions(dir))
	if err != nil {
		t.Fatalf("process exit did not release lock: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}
