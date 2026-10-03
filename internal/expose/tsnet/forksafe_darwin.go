package tsnet

import (
	"sync"

	"tailscale.com/safesocket"
)

var macTokenOnce sync.Once

// disableMacTokenLookup stops tailscale's LocalAPI client from running lsof.
//
// On macOS every LocalAPI request (status polls, WhoIs on each page view,
// certificates) looks for the Tailscale GUI app's auth token, and without one
// it runs `lsof`. Forking in this multi-threaded process can wedge the child
// in Network.framework's atfork handler (golang/go#56784): the parent then
// waits forever holding syscall.ForkLock, which on darwin every new socket
// also takes, so all of the process's networking stops. A real-tailnet run
// hung this way twice. tsnet's clients dial their in-process LocalAPI, which
// needs no token, so fixed credentials are never used to connect anywhere:
// they only make the token lookup return without forking.
func disableMacTokenLookup() {
	macTokenOnce.Do(func() { safesocket.SetCredentials("flats-in-process", 1) })
}
