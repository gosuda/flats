package netbird

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// enrollTimeout bounds registration and the first connection.
var enrollTimeout = 60 * time.Second

// DisconnectHint is appended to Disconnect's result: the NetBird client
// library has no way to deregister a peer, so the peer entry stays in the
// operator's NetBird account until removed there.
const DisconnectHint = "the peer stays registered in NetBird; remove it in the NetBird dashboard (Peers) and revoke the setup key if it is reusable"

// Enroll registers this host as a NetBird peer with setupKey, confirms that
// it connects, and persists only the generated peer identity (NetBird's
// config file, mode 0600) and account.json under dir. The setup key is used
// in memory only; it is neither stored nor logged, and is scrubbed from
// returned errors. Enroll refuses when dir already holds an enrollment
// (Disconnect first). Close any Net on dir before enrolling it again.
// A non-default management URL must be an HTTPS origin.
func Enroll(ctx context.Context, dir, managementURL, setupKey, deviceName string) (Account, error) {
	if dir == "" {
		return Account{}, errors.New("netbird: state directory is required")
	}
	setupKey = strings.TrimSpace(setupKey)
	if setupKey == "" {
		return Account{}, errors.New("netbird: setup key is required")
	}
	managementURL = strings.TrimSpace(managementURL)
	if err := validateManagementURL(managementURL); err != nil {
		return Account{}, fmt.Errorf("netbird: %w", err)
	}
	if deviceName == "" {
		deviceName, _ = os.Hostname()
		if deviceName == "" {
			deviceName = "flats"
		}
	}
	if _, ok, err := LoadAccount(dir); err != nil {
		return Account{}, fmt.Errorf("netbird: %w", err)
	} else if ok {
		return Account{}, errors.New("netbird: this host is already connected; disconnect it first")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Account{}, fmt.Errorf("netbird: state directory: %w", err)
	}
	cfgPath := filepath.Join(dir, configFile)
	// A leftover identity from an interrupted enrollment is not reusable
	// with a new key: start clean.
	_ = os.Remove(cfgPath)
	cleanup := func() {
		_ = os.Remove(cfgPath)
		_ = os.Remove(filepath.Join(dir, dnsFile))
	}
	scrub := func(err error) error {
		if err == nil {
			return nil
		}
		return errors.New(strings.ReplaceAll(err.Error(), setupKey, "***"))
	}
	c, err := newClient(clientOptions{
		DeviceName: deviceName, ManagementURL: managementURL, SetupKey: setupKey,
		ConfigPath: cfgPath, StatePath: filepath.Join(dir, dnsFile), LogOutput: io.Discard,
	})
	if err != nil {
		cleanup()
		return Account{}, fmt.Errorf("netbird: %w", scrub(err))
	}
	sctx, cancel := context.WithTimeout(ctx, enrollTimeout)
	defer cancel()
	if err := c.Start(sctx); err != nil {
		cleanup()
		return Account{}, fmt.Errorf("netbird: enroll: %w", scrub(err))
	}
	pub := c.PublicKey()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer stopCancel()
	stopErr := c.Stop(stopCtx)
	if pub == "" {
		cleanup()
		return Account{}, errors.New("netbird: enroll: the peer has no identity")
	}
	acct := Account{ManagementURL: managementURL, DeviceName: deviceName, Fingerprint: fingerprintOf(pub)}
	if err := writeJSON(filepath.Join(dir, accountFile), acct); err != nil {
		cleanup()
		return Account{}, fmt.Errorf("netbird: record account: %w", err)
	}
	_ = os.Chmod(cfgPath, 0o600)
	// A slow stop of the temporary client does not undo the enrollment.
	_ = stopErr
	return acct, nil
}

// Disconnect removes this host's local NetBird state under dir (peer
// identity, account record, DNS state). Close the Net using dir first.
// Port records are kept so a later enrollment serves the same ports.
//
// The embedded NetBird client cannot deregister its peer, so the peer remains
// in the operator's NetBird account; the returned error is nil on success and
// callers should show DisconnectHint. Without force, a host that is not
// connected is an error; with force, missing state is fine and every removal
// is attempted before the combined error is returned.
func Disconnect(_ context.Context, dir string, force bool) error {
	if dir == "" {
		return errors.New("netbird: state directory is required")
	}
	_, ok, lerr := LoadAccount(dir)
	if !ok && !force {
		if lerr != nil {
			return fmt.Errorf("netbird: %w", lerr)
		}
		return errors.New("netbird: this host is not connected")
	}
	var errs []error
	for _, name := range []string{accountFile, configFile, dnsFile} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("netbird: remove local state: %w", errors.Join(errs...))
	}
	return nil
}
