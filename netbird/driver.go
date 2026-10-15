package netbird

import (
	"github.com/gosuda/flats/internal/expose/provider"
)

// Driver links this package into a Flats binary: pass it to
// provider.RegisterNetbird before the host starts.
type Driver struct{}

var _ provider.NetbirdDriver = Driver{}
var _ provider.NetbirdNet = (*Net)(nil)

// New implements provider.NetbirdDriver.
func (Driver) New(c provider.NetbirdConfig) (provider.NetbirdNet, error) {
	return New(Config{Dir: c.Dir, ManagementURL: c.ManagementURL, SetupKey: c.SetupKey, Instance: c.Instance, Logf: c.Logf})
}

// RetireStored implements provider.NetbirdDriver.
func (Driver) RetireStored(dir, name string) error { return RetireStored(dir, name) }

// CleanupLegacy implements provider.NetbirdDriver.
func (Driver) CleanupLegacy(dir string) (provider.NetbirdLegacyPeer, bool, error) {
	p, found, err := CleanupLegacy(dir)
	return provider.NetbirdLegacyPeer(p), found, err
}
