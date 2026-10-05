package core

import (
	"context"
	"fmt"

	"github.com/gosuda/flats/internal/egress"
	"github.com/gosuda/flats/internal/store"
)

// NetworkPolicy reports desired permissions, which apply at the next activation.
func (s *Service) NetworkPolicy(ctx context.Context, slugName string) (store.NetworkPolicy, error) {
	if _, err := s.st.GetFlat(ctx, slugName); err != nil {
		return store.NetworkPolicy{}, err
	}
	return s.st.NetworkPolicy(ctx, slugName)
}

// SetNetworkPolicy is operator-only. JS server activations and new previews
// capture it alongside environment settings. Running workers keep their captured
// permissions through automatic restarts. WASI and static apps gain no host I/O.
func (s *Service) SetNetworkPolicy(ctx context.Context, slugName string, origins []string, via Via) error {
	if via != ViaConsole && via != ViaCLI {
		return forbiddenf("network permissions are set by the operator in the web console or with `flats network set`")
	}
	normalized, err := egress.NormalizeOrigins(origins)
	if err != nil {
		return invalidf("%s", err.Error())
	}
	if _, err := s.st.GetFlat(ctx, slugName); err != nil {
		return err
	}
	if err := s.st.SetNetworkPolicy(ctx, slugName, store.NetworkPolicy{Origins: normalized, UpdatedAt: s.now()}); err != nil {
		return err
	}
	s.Event(ctx, slugName, "info", "network", fmt.Sprintf("network permissions set to %d origins via %s; redeploy to apply", len(normalized), via), nil)
	return nil
}
