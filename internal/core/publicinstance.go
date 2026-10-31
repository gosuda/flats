package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosuda/flats/internal/store"
)

// PublicInstance is a copy of the public instance settings.
type PublicInstance struct {
	Enabled   bool
	UpdatedAt time.Time
}

// PublicInstanceStatus includes the network and flats affected by the mode.
type PublicInstanceStatus struct {
	Enabled      bool
	Network      string
	Usable       bool
	Reason       string
	PrivateFlats int
	UpdatedAt    time.Time
}

// ErrPublicInstanceOn marks operations that require a private link.
var ErrPublicInstanceOn = errors.New("public instance mode is on")

type publicInstanceCache struct {
	mu      sync.Mutex
	current atomic.Pointer[PublicInstance]
	epoch   atomic.Pointer[routeEpoch]
}

// PublicInstance returns the last committed settings without store I/O.
func (s *Service) PublicInstance() PublicInstance {
	if p := s.pi.current.Load(); p != nil {
		return *p
	}
	return PublicInstance{}
}

func (s *Service) loadPublicInstance(ctx context.Context) error {
	s.pi.mu.Lock()
	defer s.pi.mu.Unlock()
	row, err := s.st.GetPublicInstance(ctx)
	if err != nil {
		return fmt.Errorf("public instance settings: %w", err)
	}
	p := PublicInstance{Enabled: row.Enabled, UpdatedAt: row.UpdatedAt}
	s.pi.current.Store(&p)
	s.privateEpoch()
	return nil
}

func (s *Service) publicNetwork() (id string, usable bool, reason string) {
	n := s.settings.network()
	return publicNetworkOf(n.DefaultPublic, n.Permitted)
}

func publicNetworkOf(id string, permitted []string) (string, bool, string) {
	if id == "" {
		return "", false, "no default public network is configured"
	}
	if !PublicProvider(ProviderID(id)) || !slices.Contains(permitted, id) {
		return id, false, fmt.Sprintf("the default public network %s is not turned on for this host", id)
	}
	return id, true, ""
}

// PublicInstanceStatus reads the stored settings, network and Private census.
func (s *Service) PublicInstanceStatus(ctx context.Context) (PublicInstanceStatus, error) {
	row, err := s.st.GetPublicInstance(ctx)
	if err != nil {
		return PublicInstanceStatus{}, fmt.Errorf("read the public instance settings: %w", err)
	}
	return s.publicInstanceStatus(ctx, PublicInstance{Enabled: row.Enabled, UpdatedAt: row.UpdatedAt})
}

func (s *Service) publicInstanceStatus(ctx context.Context, p PublicInstance) (PublicInstanceStatus, error) {
	status := PublicInstanceStatus{Enabled: p.Enabled, UpdatedAt: p.UpdatedAt}
	status.Network, status.Usable, status.Reason = s.publicNetwork()
	flats, err := s.st.ListFlats(ctx)
	if err != nil {
		return status, err
	}
	for _, f := range flats {
		if !f.Visibility.Public() {
			status.PrivateFlats++
		}
	}
	return status, nil
}

// SetPublicInstance commits the mode and gates private links before returning.
// Turning on requires the default public network the operator confirmed.
func (s *Service) SetPublicInstance(ctx context.Context, on bool, confirmedNetwork, who string) (PublicInstanceStatus, error) {
	ctx = context.WithoutCancel(ctx)
	s.pi.mu.Lock()
	row, changed, err := s.st.UpdatePublicInstance(ctx, s.now(), func(row store.PublicInstanceRow) (store.PublicInstanceRow, string, error) {
		state := "off"
		if on {
			network, usable, reason := s.publicNetwork()
			if !usable {
				return row, "", fmt.Errorf("%w: %s", ErrConflict, reason)
			}
			if confirmedNetwork != network {
				return row, "", fmt.Errorf("%w: the default public network changed to %s; review it and turn public instance mode on again", ErrConflict, network)
			}
			state = "on"
		}
		if row.Enabled == on {
			return row, "", nil
		}
		row.Enabled = on
		return row, fmt.Sprintf("public instance: turned %s by %s", state, changedBy(who)), nil
	})
	if err != nil {
		s.pi.mu.Unlock()
		return PublicInstanceStatus{}, err
	}
	p := PublicInstance{Enabled: row.Enabled, UpdatedAt: row.UpdatedAt}
	s.pi.current.Store(&p)
	if changed && on {
		s.rotatePrivateEpoch()
	}
	s.pi.mu.Unlock()
	// The committed cache must not depend on this log sync succeeding.
	s.syncAutopilot(ctx)
	return s.publicInstanceStatus(ctx, p)
}

type defaultGrant struct{ provider, why string }

type newFlatPolicy struct {
	visibility store.Visibility
	access     string
	providers  []string
	events     []store.Event
}

func (s *Service) newFlatPolicy(now time.Time, via Via) (newFlatPolicy, error) {
	s.pi.mu.Lock()
	defer s.pi.mu.Unlock()
	on := s.PublicInstance().Enabled
	n := s.settings.network()
	p := newFlatPolicy{visibility: store.Private}
	message := "flat created via " + string(via)
	var grants []defaultGrant
	if on {
		id, usable, reason := publicNetworkOf(n.DefaultPublic, n.Permitted)
		if !usable {
			return p, fmt.Errorf("%w: %s", ErrConflict, reason)
		}
		p.visibility, p.access = store.Public, store.AccessView
		message = fmt.Sprintf("flat created Public by public instance mode on %s via %s", id, via)
		grants = []defaultGrant{{id, "network.default_public is " + id}}
	} else {
		switch {
		case n.DefaultPrivate == store.ProviderTailscale && slices.Contains(n.Permitted, store.ProviderTailscale):
			grants = append(grants, defaultGrant{store.ProviderTailscale, "network.default_private is tailscale"})
		case n.DefaultPrivate == store.ProviderNetbird && slices.Contains(n.Permitted, store.ProviderNetbird):
			grants = append(grants, defaultGrant{store.ProviderNetbird, "network.default_private is netbird"})
		case n.DefaultPrivate == store.ProviderProxy && slices.Contains(n.Permitted, store.ProviderProxy):
			grants = append(grants, defaultGrant{store.ProviderProxy, "network.default_private is proxy"})
		case n.DefaultPrivate == "" && s.cfg.PrivateBackend == "tailscale":
			grants = append(grants, defaultGrant{store.ProviderTailscale, "network.private_backend is tailscale"})
		}
		if id := n.DefaultPublic; id != "" && slices.Contains(n.Permitted, id) {
			grants = append(grants, defaultGrant{id, "network.default_public is " + id})
		}
	}
	p.events = []store.Event{{Time: now, Level: "info", Kind: "flat", Message: message}}
	for _, d := range grants {
		p.providers = append(p.providers, d.provider)
		p.events = append(p.events, store.Event{Time: now, Level: "info", Kind: "provider", Message: fmt.Sprintf("provider %s permitted=true by default (%s)", d.provider, d.why)})
	}
	return p, nil
}
