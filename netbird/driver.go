package netbird

import (
	"context"

	"github.com/gosuda/flats/internal/expose/provider"
)

// Driver links this package into a Flats binary: pass it to
// provider.RegisterNetbird before the host starts.
type Driver struct{}

var _ provider.NetbirdDriver = Driver{}
var _ provider.NetbirdNet = (*Net)(nil)

// New implements provider.NetbirdDriver.
func (Driver) New(c provider.NetbirdConfig) (provider.NetbirdNet, error) {
	return New(Config{Dir: c.Dir, ManagementURL: c.ManagementURL, DeviceName: c.DeviceName, Instance: c.Instance, Logf: c.Logf})
}

// LoadAccount implements provider.NetbirdDriver.
func (Driver) LoadAccount(dir string) (provider.NetbirdAccount, bool, error) {
	a, ok, err := LoadAccount(dir)
	return provider.NetbirdAccount(a), ok, err
}

// Enroll implements provider.NetbirdDriver.
func (Driver) Enroll(ctx context.Context, dir, managementURL, setupKey, deviceName string) (provider.NetbirdAccount, error) {
	a, err := Enroll(ctx, dir, managementURL, setupKey, deviceName)
	return provider.NetbirdAccount(a), err
}

// Disconnect implements provider.NetbirdDriver.
func (Driver) Disconnect(ctx context.Context, dir string, force bool) error {
	return Disconnect(ctx, dir, force)
}

// RetireStored implements provider.NetbirdDriver.
func (Driver) RetireStored(dir, host string) error { return RetireStored(dir, host) }

// DisconnectHint implements provider.NetbirdDriver.
func (Driver) DisconnectHint() string { return DisconnectHint }
