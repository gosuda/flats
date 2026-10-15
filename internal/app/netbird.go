package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/store"
)

// NetBird runs one embedded peer per flat and per NetBird console domain
// (docs/spec/netbird-peers.md). Every peer registers with the setup key
// stored in flats.db, which this file keeps in memory for the backend.

// legacyPeerFile records the shared peer an earlier version registered, so
// the console can ask the operator to delete it in the NetBird dashboard.
const legacyPeerFile = "legacy-peer.json"

// prepareNetbird runs at start before the NetBird backend is built: it drops
// the shared peer of earlier versions and resolves the setup key with the
// seed rule.
func (h *Host) prepareNetbird(ctx context.Context, dataDir string, st *store.Store, in keyInput, logf func(string, ...any)) error {
	dir := filepath.Join(dataDir, "netbird")
	if d, err := provider.LinkedNetbird(); err == nil {
		p, found, err := d.CleanupLegacy(dir)
		if found {
			name := p.DeviceName
			if name == "" {
				name = "(unknown)"
			}
			logf("netbird: the shared peer %s of an earlier version is no longer used; its local state was removed. Delete it in the NetBird dashboard (Peers)", name)
			if werr := writeLegacyPeer(dir, p); werr != nil {
				logf("netbird: record the old shared peer: %v", werr)
			}
		}
		if err != nil {
			return err
		}
	}
	// A row without a value is the connection record of the shared peer;
	// a row with one (a waiting enrollment) is now the stored key.
	if row, err := st.GetHostKey(ctx, core.HostKeyNetbird); err == nil && !row.HasValue() {
		if err := st.DeleteHostKey(ctx, core.HostKeyNetbird); err != nil {
			return err
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	key, differs, err := resolveStoredKey(ctx, dataDir, st, core.HostKeyNetbird, in, time.Now(), logf)
	if err != nil {
		return err
	}
	h.nbMu.Lock()
	h.nbKey, h.nbKeyExternal, h.nbKeyDiffers = key, in.value, differs
	h.nbMu.Unlock()
	return nil
}

func writeLegacyPeer(dir string, p provider.NetbirdLegacyPeer) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, legacyPeerFile), b, 0o600)
}

// netbirdLegacyPeer is the old shared peer the console asks the operator to
// delete, or nil.
func (h *Host) netbirdLegacyPeer() *provider.NetbirdLegacyPeer {
	b, err := os.ReadFile(filepath.Join(h.netbirdOptions.Dir, legacyPeerFile))
	if err != nil {
		return nil
	}
	var p provider.NetbirdLegacyPeer
	if json.Unmarshal(b, &p) != nil {
		return &provider.NetbirdLegacyPeer{}
	}
	return &p
}

// DismissNetbirdLegacyPeer implements api.KeyAdmin.
func (h *Host) DismissNetbirdLegacyPeer(ctx context.Context) error {
	if err := os.Remove(filepath.Join(h.netbirdOptions.Dir, legacyPeerFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %v", core.ErrUnavailable, err)
	}
	return nil
}

// netbirdKeyFingerprint identifies a setup key for the approval policy token
// without revealing it: a one-way hash, "" for no key.
func netbirdKeyFingerprint(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("flats-netbird-key:" + key))
	return hex.EncodeToString(sum[:16])
}

// retryNetbird opens again, in the background, the NetBird routes and
// console domains that could not start without a setup key.
func (h *Host) retryNetbird() {
	h.background(func(ctx context.Context) {
		if h.Svc != nil {
			h.Svc.RetryProviderRoutes(ctx, core.ProviderID(provider.Netbird), core.ProviderID(provider.NetbirdExpose))
		}
		h.retryConsoleDomains(ctx, providerNetbird)
	})
}
