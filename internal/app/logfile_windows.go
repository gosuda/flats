package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/windows"

	"github.com/gosuda/flats/internal/filelock"
	"github.com/gosuda/flats/internal/winservice"
)

// maxLogBytes bounds serve.log: a server that runs for months, or fails at
// start every minute, would otherwise grow it without end. One older file stays.
const maxLogBytes = 10 << 20

// serviceFlags adds what a scheduled task needs: it captures no output, and
// ending it does not reach the server. launchd and systemd need neither flag.
func serviceFlags(fs *flag.FlagSet, o *Options) {
	fs.StringVar(&o.LogFile, "log-file", "", "append the log to this file (the Windows service sets it)")
	fs.BoolVar(&o.WatchParent, "watch-parent", false, "shut down when the parent process ends (the Windows service sets it)")
}

// rotatingLog keeps the log file below maxLogBytes while the server runs. It
// copies the file and truncates it, because the standard error handle points at
// this file and must stay valid for a panic of the Go runtime. The file is not
// opened with O_APPEND: on Windows that handle cannot truncate.
type rotatingLog struct {
	mu   sync.Mutex
	f    *os.File
	path string
	size int64
}

func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size+int64(len(p)) > maxLogBytes {
		l.rotate()
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// rotate moves the content to the older file. When that fails the log says so
// and keeps growing, and the next attempt comes after another maxLogBytes.
func (l *rotatingLog) rotate() {
	if err := l.moveAside(); err != nil {
		fmt.Fprintf(l.f, "the log did not rotate: %v\n", err)
	}
	l.size = 0
}

func (l *rotatingLog) moveAside() error {
	in, err := os.Open(l.path)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(l.path+".1", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	_, err = l.f.Seek(0, io.SeekStart)
	return err
}

// redirectLog appends the process's log to path. The standard error handle
// moves too, so that a panic of the Go runtime reaches the file.
func redirectLog(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	l, err := openLog(path)
	if err != nil {
		return err
	}
	if err := windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(l.f.Fd())); err != nil {
		l.f.Close()
		return err
	}
	os.Stderr = l.f
	log.SetOutput(l)
	return nil
}

// openLog opens the log at its end, and rotates one that is already too long.
func openLog(path string) (*rotatingLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return nil, err
	}
	l := &rotatingLog{f: f, path: path, size: size}
	if l.size > maxLogBytes {
		l.rotate()
	}
	return l, nil
}

// startedByTask records --watch-parent, which only the service's task passes.
var startedByTask bool

// watchParent calls stop when the parent process ends. The service's parent is
// its headless console, and ending the scheduled task ends only that console:
// without this the server would keep running, out of reach of the task. A parent
// that is already gone is an error, so that the next run of the task starts over.
func watchParent(ctx context.Context, stop func()) error {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(os.Getppid()))
	if err != nil {
		return fmt.Errorf("open the parent process: %w", err)
	}
	startedByTask = true
	go func() {
		defer windows.CloseHandle(h)
		if s, err := windows.WaitForSingleObject(h, windows.INFINITE); err == nil && s == windows.WAIT_OBJECT_0 && ctx.Err() == nil {
			log.Printf("the parent process ended; shutting down")
			stop()
		}
	}()
	return nil
}

// recordPID leaves only this process's ID in the lock file, and for the service
// its role. A stop that cannot reach the server otherwise reads the ID there, and
// it acts only on a server with that role.
func recordPID(f *os.File) {
	if err := f.Truncate(0); err != nil {
		return
	}
	line := strconv.Itoa(os.Getpid())
	if startedByTask {
		line += " " + winservice.ServiceRole
	}
	if _, err := f.WriteAt([]byte(line+"\n"), 0); err != nil {
		log.Printf("record the process ID in the data directory lock: %v", err)
	}
}

// tryLockDataDir waits briefly for the lock: a status check holds it for a moment
// to see whether a server runs, and must not make a starting server fail.
func tryLockDataDir(f *os.File) error {
	for i := 0; ; i++ {
		err := filelock.TryLock(f)
		if i == 5 || !errors.Is(err, filelock.ErrLocked) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}
