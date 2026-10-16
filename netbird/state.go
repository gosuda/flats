package netbird

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Files of the single shared peer earlier versions ran for every flat.
const (
	legacyAccountFile = "account.json"
	legacyConfigFile  = "netbird.json"
	legacyDNSFile     = "netbird-dns.json"
	legacyPortsFile   = "ports.json"
)

// LegacyPeer describes the shared peer an earlier version registered.
type LegacyPeer struct {
	ManagementURL string `json:"management_url"`
	DeviceName    string `json:"device_name"`
}

// CleanupLegacy deletes the local state of the shared peer earlier versions
// ran (identity, account record, DNS state and port records) and returns
// what it was, so the operator can delete it in the NetBird dashboard: the
// embedded client cannot deregister a peer. found is false when there was
// none. Every removal is attempted before an error is returned.
func CleanupLegacy(dir string) (p LegacyPeer, found bool, err error) {
	b, rerr := os.ReadFile(filepath.Join(dir, legacyAccountFile))
	if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return LegacyPeer{}, false, rerr
	}
	if rerr == nil {
		found = true
		_ = json.Unmarshal(b, &p) // an unreadable record still means a peer existed
	} else if _, serr := os.Stat(filepath.Join(dir, legacyConfigFile)); serr == nil {
		found = true // an identity without its account record
	}
	var errs []error
	for _, name := range []string{legacyAccountFile, legacyConfigFile, legacyDNSFile, legacyPortsFile} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return p, found, fmt.Errorf("netbird: remove the shared peer's state: %w", errors.Join(errs...))
	}
	return p, found, nil
}

// RetireStored deletes name's peer identity when no Net is running, for a
// flat deleted while NetBird is off. A name without a peer is not an error.
func RetireStored(dir, name string) error {
	if !validName(name) {
		return nil
	}
	if err := os.RemoveAll(peerDir(dir, name)); err != nil {
		return fmt.Errorf("netbird: %s: delete the peer identity: %w", name, err)
	}
	return nil
}
