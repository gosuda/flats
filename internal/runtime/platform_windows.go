package runtime

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path"
	"syscall"

	"golang.org/x/sys/windows"
)

// listenFile returns no file on Windows, where a child cannot inherit a
// listening socket: the worker binds the path of its spec. It removes what an
// unclean exit left at that path.
func listenFile(path string) (*os.File, error) {
	os.Remove(path)
	return nil, nil
}

// noHalfClose hides CloseWrite from the proxy. When a visitor's upgraded
// connection ends, the proxy half-closes its side to the worker and waits for
// the worker to close. A Unix socket on Windows can miss the end of its stream,
// and the lease of a document then lasts until the worker's next ping. Without
// CloseWrite the proxy closes both sides at once. A WebSocket has no half-close,
// so a client that ends its side after its Close frame gets no reply from the
// worker here.
type noHalfClose struct{ net.Conn }

// workerConn is the connection to a worker as the proxy uses it.
func workerConn(c net.Conn) net.Conn { return noHalfClose{c} }

// keyUnderFile is the error of a key below a stored file. Windows reports such a
// path as missing, and Unix refuses it, which the runtime reference documents.
func keyUnderFile(r *os.Root, key string) error {
	for dir := path.Dir(key); dir != "."; dir = path.Dir(dir) {
		if fi, err := r.Stat(dir); err == nil && fi.Mode().IsRegular() {
			return &fs.PathError{Op: "open", Path: key, Err: syscall.ENOTDIR}
		}
	}
	return nil
}

// workerServerIdle is the idle timeout of a worker's HTTP server. The host closes
// an idle connection after workerIdleClose, and a Unix socket here can miss that
// close. The timeout then ends the connection. It is longer than the host's, so
// the host normally closes first.
const workerServerIdle = 2 * workerIdleClose

// specSocket is the socket path for a worker's spec.
func specSocket(path string) string { return path }

// moveWorkerSocket has nothing to move: a pre-started worker binds only once
// it has its spec, at the path that names. It removes what an unclean exit left
// at that path, or the bind fails. On Unix the rename replaces it.
func moveWorkerSocket(from, to string) error {
	if err := os.Remove(to); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ownedByOther does not apply: a directory has no POSIX owner here, and
// privatedir.Restrict checks its owner and its list.
func ownedByOther(os.FileInfo) bool { return false }

func fileInode(os.FileInfo) uint64 { return 0 }

// processCPU is the process's own CPU time (user+system) in nanoseconds.
func processCPU() (int64, bool) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &creation, &exit, &kernel, &user); err != nil {
		return 0, false
	}
	return filetime100ns(kernel)*100 + filetime100ns(user)*100, true
}

func filetime100ns(t windows.Filetime) int64 {
	return int64(t.HighDateTime)<<32 | int64(t.LowDateTime)
}
