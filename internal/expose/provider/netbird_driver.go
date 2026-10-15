package provider

import (
	"errors"
	"sync"
)

// ErrNetbirdNotBuilt means this binary does not contain NetBird. NetBird's
// client needs `replace` directives in go.mod, which Go refuses for
// `go install pkg@version`, so it lives in the separate module ./netbird and
// is linked only by the build in ./netbird/cmd/flats, which the release
// archives and container images ship (docs/networking.md).
var ErrNetbirdNotBuilt = errors.New("this Flats build does not include NetBird; use a release binary (install.sh or the container image) or build ./netbird/cmd/flats from the source tree (see docs/networking.md)")

// NetbirdLegacyPeer describes the single shared peer earlier versions
// registered for the whole host. It holds no secret.
type NetbirdLegacyPeer struct {
	ManagementURL string `json:"management_url"`
	DeviceName    string `json:"device_name"`
}

// NetbirdConfig configures a NetBird backend.
type NetbirdConfig struct {
	// Dir holds one directory per peer (its identity and state).
	Dir           string
	ManagementURL string
	// SetupKey returns the setup key stored now, or "". It is read at every
	// peer start.
	SetupKey func() string
	// Instance is the Flats host instance id.
	Instance string
	Logf     func(string, ...any)
}

// NetbirdDriver is the NetBird implementation a build links in with
// RegisterNetbird. The main module has none.
type NetbirdDriver interface {
	New(NetbirdConfig) (NetbirdNet, error)
	// RetireStored deletes the identity of name's peer in dir when no backend
	// runs, so deleting a flat while NetBird is off does not keep it.
	RetireStored(dir, name string) error
	// CleanupLegacy deletes the local state of the shared peer earlier
	// versions ran in dir and describes it; found is false when there was
	// none.
	CleanupLegacy(dir string) (peer NetbirdLegacyPeer, found bool, err error)
}

var (
	netbirdMu     sync.RWMutex
	netbirdDriver NetbirdDriver
)

// RegisterNetbird links a NetBird implementation into this binary. Call it
// before the host starts.
func RegisterNetbird(d NetbirdDriver) {
	netbirdMu.Lock()
	netbirdDriver = d
	netbirdMu.Unlock()
}

// LinkedNetbird returns the linked NetBird driver, or ErrNetbirdNotBuilt.
func LinkedNetbird() (NetbirdDriver, error) {
	netbirdMu.RLock()
	defer netbirdMu.RUnlock()
	if netbirdDriver == nil {
		return nil, ErrNetbirdNotBuilt
	}
	return netbirdDriver, nil
}

// NetbirdBuilt reports whether this binary contains NetBird.
func NetbirdBuilt() bool {
	_, err := LinkedNetbird()
	return err == nil
}
