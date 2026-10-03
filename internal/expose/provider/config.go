// Package provider serves flats on explicitly permitted networks.
// Local loopback needs no grant. Tailscale, Tailscale Funnel and Portal
// start only when the host file grants them and a caller passes the same
// provider in ExposureRequest.Permitted.
package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/core"
)

// ID is a network provider. The public Tailscale provider is tailscale-funnel.
// The shorter name "funnel" is not an alias and does not grant this one.
type ID = core.ProviderID

const (
	Local     = core.ProviderLocal
	Tailscale = core.ProviderTailscale
	Funnel    = core.ProviderFunnel
	Portal    = core.ProviderPortal
)

// Audience selects draft or current content.
type Audience = core.ExposureAudience

const (
	AudienceCurrent = core.AudienceCurrent
	AudienceDraft   = core.AudienceDraft
)

const fileName = "network-provider.json"

// File is the host permission record. It is not a flat's provider choice
// and it does not publish anything by itself.
type File struct {
	Version        int       `json:"version"`
	Permitted      []ID      `json:"permitted"`
	PrivateBackend string    `json:"private_backend,omitempty"`
	Migration      Migration `json:"migration"`
}

// Migration records how an existing data directory was read. Historical
// tsnet and portal files are not grants.
type Migration struct {
	AppliedAt        string `json:"applied_at,omitempty"`
	HistoricalTSNet  bool   `json:"historical_tsnet_state"`
	HistoricalPortal bool   `json:"historical_portal_state"`
	GrantsFromState  []ID   `json:"grants_from_state"`
	Note             string `json:"note,omitempty"`
}

// ParseID accepts only the four canonical ids.
func ParseID(s string) (ID, error) {
	switch ID(s) {
	case Local, Tailscale, Funnel, Portal:
		return ID(s), nil
	default:
		return "", fmt.Errorf("unknown provider %q", s)
	}
}

// Allows reports a stored grant. Local is always allowed and is not stored.
func (f File) Allows(id ID) bool {
	if id == Local {
		return true
	}
	return slices.Contains(f.Permitted, id)
}

// Grant adds id. It does not serve a flat and does not treat any other
// spelling as the same provider.
func (f File) Grant(id ID) (File, error) {
	if _, err := ParseID(string(id)); err != nil {
		return f, err
	}
	if id == Local || slices.Contains(f.Permitted, id) {
		return f, nil
	}
	f.Permitted = append(slices.Clone(f.Permitted), id)
	return f, nil
}

// Load reads the host file, or writes version 1 with nothing permitted when
// the file is absent. A missing file is not a grant, including when tsnet
// or portal directories already exist.
func Load(dir string) (File, error) {
	if dir == "" {
		return File{}, errors.New("provider: data directory is required")
	}
	path := filepath.Join(dir, fileName)
	b, err := os.ReadFile(path)
	if err == nil {
		var f File
		if err := json.Unmarshal(b, &f); err != nil {
			return File{}, fmt.Errorf("provider: read %s: %w", fileName, err)
		}
		if err := validate(f); err != nil {
			return File{}, err
		}
		if f.Permitted == nil {
			f.Permitted = []ID{}
		}
		if f.Migration.GrantsFromState == nil {
			f.Migration.GrantsFromState = []ID{}
		}
		return f, nil
	}
	if !os.IsNotExist(err) {
		return File{}, err
	}
	f := File{
		Version:   1,
		Permitted: []ID{},
		Migration: Migration{
			AppliedAt:        time.Now().UTC().Format(time.RFC3339),
			HistoricalTSNet:  historicalTSNet(dir),
			HistoricalPortal: historicalPortal(dir),
			GrantsFromState:  []ID{},
			Note:             "Historical tsnet and portal files were not treated as permission. Funnel is not implied by a tailnet node. The files were left in place so a later explicit grant keeps the same hostnames.",
		},
	}
	if err := Save(dir, f); err != nil {
		return File{}, err
	}
	return f, nil
}

// Save writes the host file atomically. It does not open a network.
func Save(dir string, f File) error {
	if err := validate(f); err != nil {
		return err
	}
	if f.Version == 0 {
		f.Version = 1
	}
	if f.Permitted == nil {
		f.Permitted = []ID{}
	}
	if f.Migration.GrantsFromState == nil {
		f.Migration.GrantsFromState = []ID{}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	path := filepath.Join(dir, fileName)
	tmp, err := os.CreateTemp(dir, "."+fileName+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmpName, 0o600)); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func validate(f File) error {
	if f.Version != 0 && f.Version != 1 {
		return fmt.Errorf("provider: unsupported configuration version %d", f.Version)
	}
	for _, id := range f.Permitted {
		if id == Local {
			return errors.New("provider: local is always allowed and is not stored as a grant")
		}
		if _, err := ParseID(string(id)); err != nil {
			return fmt.Errorf("provider: %w", err)
		}
	}
	if len(f.Migration.GrantsFromState) > 0 {
		return errors.New("provider: historical state cannot grant a provider")
	}
	switch f.PrivateBackend {
	case "", "local", "tailscale":
	default:
		return fmt.Errorf("provider: private backend %q is not local or tailscale", f.PrivateBackend)
	}
	return nil
}

func historicalTSNet(dir string) bool {
	entries, err := os.ReadDir(filepath.Join(dir, "tsnet"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			return true
		}
	}
	return false
}

func historicalPortal(dir string) bool {
	entries, err := os.ReadDir(filepath.Join(dir, "portal"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			return true
		}
	}
	return false
}
