package ostest

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"

	"golang.org/x/sys/windows"
)

const stillActive = 259

// Alive reports whether the process exists.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == stillActive
}

func isPrivilegeError(err error) bool {
	return errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD)
}

// The Windows resolver does not map *.localhost to loopback, as browsers do
// and as flat URLs need. A test that imports this package reaches them through
// http.DefaultTransport and through LoopbackDial.
func init() {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		t.DialContext = LoopbackDial
	}
}

// LoopbackDial dials address, with any *.localhost name on loopback.
func LoopbackDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err == nil && strings.HasSuffix(strings.ToLower(host), ".localhost") {
		address = net.JoinHostPort("127.0.0.1", port)
	}
	return new(net.Dialer).DialContext(ctx, network, address)
}

// CPUCounted reports whether the system has counted CPU time for the process.
// Windows counts it in clock ticks of about 16 ms, so a new process reads zero
// for a moment.
func CPUCounted(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var creation, exit, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &creation, &exit, &kernel, &user) != nil {
		return false
	}
	return kernel.HighDateTime|kernel.LowDateTime|user.HighDateTime|user.LowDateTime != 0
}
