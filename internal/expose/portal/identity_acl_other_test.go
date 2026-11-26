//go:build !windows

package portal

import "testing"

// identityFileACL has nothing to check outside Windows: there the Portal SDK leaves the mode of an existing identity
// file to Flats (TestIdentityPersistedAndReused checks mode 0600).
func identityFileACL(*testing.T, string) {}
