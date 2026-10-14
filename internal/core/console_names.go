package core

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/gosuda/flats/internal/slug"
	"github.com/gosuda/flats/internal/store"
)

// consoleNameReserved reports whether name belongs to the console: a fixed
// reserved name, a console domain or node, or a domain being added.
func (s *Service) consoleNameReserved(name string) bool {
	return slices.Contains(s.cfg.Reserved, name) || s.consoleNameInUse(name)
}

// consoleNameInUse reports whether name is a console domain or node, or a
// domain being added. Fixed reserved names stay free for console domains.
func (s *Service) consoleNameInUse(name string) bool {
	if s.cfg.ReservedNames != nil && slices.Contains(s.cfg.ReservedNames(), name) {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.consoleAdding[name]
}

// previewHost returns a new random preview host for slugName that is not a
// console name.
func (s *Service) previewHost(slugName string) string {
	for {
		if host := slug.PreviewHost(slugName); !s.consoleNameReserved(host) {
			return host
		}
	}
}

// ConsoleNameHolder describes what already uses name as a host name: a flat,
// an open preview or a rename redirect. It returns "" when nothing does.
// Console names themselves are not reported.
func (s *Service) ConsoleNameHolder(ctx context.Context, name string) (string, error) {
	if _, err := s.st.GetFlat(ctx, name); err == nil {
		return fmt.Sprintf("the flat %q", name), nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	s.mu.Lock()
	_, isPreview := s.prevs[name]
	r := s.redir[name]
	s.mu.Unlock()
	if isPreview {
		return "an open preview", nil
	}
	if r != nil {
		return fmt.Sprintf("a redirect to %q", r.cur), nil
	}
	if red, err := s.st.RedirectFor(ctx, name, s.now()); err == nil {
		return fmt.Sprintf("a redirect to %q", red.Flat), nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	return "", nil
}

// ReserveConsoleName runs commit while name is reserved for a console
// domain being added. It holds name's operation lock, the one flat creation
// and renames take, so no flat can take name in between; previews skip it.
// It refuses a name a flat, preview, redirect or another console name holds.
// After commit returns, the console list or node record holds the name.
func (s *Service) ReserveConsoleName(ctx context.Context, name string, commit func() error) error {
	unlock := s.lock(name)
	defer unlock()
	if s.consoleNameInUse(name) {
		return fmt.Errorf("%w: %q is already a console domain or is still being removed", ErrConflict, name)
	}
	holder, err := s.ConsoleNameHolder(ctx, name)
	if err != nil {
		return err
	}
	if holder != "" {
		return fmt.Errorf("%w: %q is used by %s; choose another name", ErrConflict, name, holder)
	}
	s.mu.Lock()
	if s.consoleAdding == nil {
		s.consoleAdding = map[string]bool{}
	}
	s.consoleAdding[name] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.consoleAdding, name)
		s.mu.Unlock()
	}()
	return commit()
}
