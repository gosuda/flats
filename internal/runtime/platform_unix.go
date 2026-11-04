//go:build !windows

package runtime

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// workerServerIdle is the idle timeout of a worker's HTTP server. The socket here
// reports every close, so the host alone ends an idle connection.
const workerServerIdle = 0

// listenFile binds a Unix socket at path and returns its listening file
// for a child's fd 3. The socket file outlives the returned file.
func listenFile(path string) (*os.File, error) {
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("worker socket: %w", err)
	}
	ul := ln.(*net.UnixListener)
	ul.SetUnlinkOnClose(false)
	lf, err := ul.File()
	ul.Close()
	if err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("worker socket: %w", err)
	}
	return lf, nil
}

// workerConn is the connection to a worker as the proxy uses it.
func workerConn(c net.Conn) net.Conn { return c }

// specSocket is the socket path for a worker's spec: empty, as it inherits
// its socket.
func specSocket(string) string { return "" }

// moveWorkerSocket gives a pre-started worker's bound socket another path:
// the socket follows its file.
func moveWorkerSocket(from, to string) error { return os.Rename(from, to) }

func ownedByOther(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) != os.Getuid()
}

func fileInode(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

// processCPU is the process's own CPU time (user+system) in nanoseconds.
func processCPU() (int64, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, false
	}
	return ru.Utime.Nano() + ru.Stime.Nano(), true
}
