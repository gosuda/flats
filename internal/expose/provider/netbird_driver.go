package provider

import (
	"context"
	"errors"
	"sync"
)

// ErrNetbirdNotBuilt means this binary does not contain NetBird. NetBird's
// client needs `replace` directives in go.mod, which Go refuses for
// `go install pkg@version`, so it lives in the separate module ./netbird and
// is linked only by the build in ./netbird/cmd/flats, which the release
// archives and container images ship (docs/networking.md).
var ErrNetbirdNotBuilt = errors.New("this Flats build does not include NetBird; use a release binary (install.sh or the container image) or build ./netbird/cmd/flats from the source tree (see docs/networking.md)")

// NetbirdAccount describes the enrolled NetBird peer. It holds no secret.
type NetbirdAccount struct {
	ManagementURL string `json:"management_url"`
	DeviceName    string `json:"device_name"`
	// Fingerprint identifies the peer (a hash of its public key).
	Fingerprint string `json:"fingerprint"`
}

// NetbirdConfig configures a NetBird backend.
type NetbirdConfig struct {
	// Dir holds the peer identity, the account record and the port records.
	Dir           string
	ManagementURL string
	DeviceName    string
	// Instance is the Flats host instance id.
	Instance string
	Logf     func(string, ...any)
}

// NetbirdDriver is the NetBird implementation a build links in with
// RegisterNetbird. The main module has none.
type NetbirdDriver interface {
	New(NetbirdConfig) (NetbirdNet, error)
	// LoadAccount reads the account enrolled in dir; ok is false when none is.
	LoadAccount(dir string) (acct NetbirdAccount, ok bool, err error)
	// Enroll registers this host as a peer with a setup key, which it neither
	// stores nor logs.
	Enroll(ctx context.Context, dir, managementURL, setupKey, deviceName string) (NetbirdAccount, error)
	Disconnect(ctx context.Context, dir string, force bool) error
	// RetireStored releases the ports recorded for host in dir when no backend
	// runs, so deleting a flat while NetBird is off does not keep them.
	RetireStored(dir, host string) error
	// DisconnectHint tells the operator what remains after Disconnect.
	DisconnectHint() string
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
