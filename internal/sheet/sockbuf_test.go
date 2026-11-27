package sheet

import (
	"context"
	"net"
	"net/http"
	"syscall"
)

// A reader that stops stalls its sender only while the kernel cannot take the
// unread rest. The stall tests (capture_test.go, delta_fanout_test.go) hold
// 18 to 36 MB per connection, so a host whose tcp_wmem and tcp_rmem maxima
// are larger takes it all: every sender finishes while its reader is still
// stopped, and the tests give every attempt up. These helpers keep the
// buffers of a connection small wherever the host's maxima lie; production
// sockets keep kernel autotuning.
const (
	testSendBuffer    = 256 << 10
	testReceiveBuffer = 32 << 10
)

// cappedSendListener caps the send buffer of each accepted socket.
type cappedSendListener struct{ net.Listener }

func (l cappedSendListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(testSendBuffer)
	}
	return c, err
}

// cappedDial dials network and addr with a small receive buffer. The cap is
// set before the connection is made, so the window the connection offers
// never exceeds the buffer. A cap set after connecting, on a host whose
// default receive buffer is large, can shrink the buffer under a window
// already offered; the kernel then drops what no longer fits and the sender
// retransmits it with growing delays.
func cappedDial(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{Control: func(_, _ string, rc syscall.RawConn) error {
		var err error
		if cerr := rc.Control(func(fd uintptr) { err = setReceiveBuffer(fd, testReceiveBuffer) }); cerr != nil {
			return cerr
		}
		return err
	}}
	return d.DialContext(ctx, network, addr)
}

// cappedHTTP is an HTTP client whose connections have a small receive buffer.
var cappedHTTP = &http.Client{Transport: &http.Transport{DialContext: cappedDial}}
