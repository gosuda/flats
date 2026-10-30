package core

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/gosuda/flats/internal/slug"
	"github.com/gosuda/flats/internal/store"
)

// addingHosts holds the host names whose addition is in progress: console
// domains whose addition is under way and preview hosts chosen but not yet
// registered. Guarded by Service.mu.
type addingHosts struct {
	console map[string]bool
	preview map[string]bool
}

// consoleNameReserved reports whether name belongs to the console: a fixed
// reserved name, a console domain or node, or a domain being added.
func (s *Service) consoleNameReserved(name string) bool {
	return slices.Contains(s.cfg.Reserved, name) || s.consoleNameInUse(name)
}

// consoleNameListed reports whether name is a fixed reserved name or a
// console domain or node. It takes no lock of the Service.
func (s *Service) consoleNameListed(name string) bool {
	return slices.Contains(s.cfg.Reserved, name) || (s.cfg.ReservedNames != nil && slices.Contains(s.cfg.ReservedNames(), name))
}

// consoleNameInUse reports whether name is a console domain or node, or a
// domain being added. Fixed reserved names stay free for console domains.
func (s *Service) consoleNameInUse(name string) bool {
	if s.cfg.ReservedNames != nil && slices.Contains(s.cfg.ReservedNames(), name) {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adding.console[name]
}

// previewHost returns a new random preview host for slugName that is not a
// console name, reserved against console domain additions until release is
// called; the caller releases it once the preview is registered or failed.
func (s *Service) previewHost(slugName string) (string, func()) {
	for {
		host := slug.PreviewHost(slugName)
		// Checked and reserved under one lock: a console addition reserves its
		// name under s.mu too and keeps it until the list holds it.
		s.mu.Lock()
		if s.adding.console[host] || s.adding.preview[host] || s.consoleNameListed(host) {
			s.mu.Unlock()
			continue
		}
		if s.adding.preview == nil {
			s.adding.preview = map[string]bool{}
		}
		s.adding.preview[host] = true
		s.mu.Unlock()
		return host, func() {
			s.mu.Lock()
			delete(s.adding.preview, host)
			s.mu.Unlock()
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

// ServeConsoleName runs serve for a name that a console domain list already
// holds, while name's operation lock is held, the one flat creation and
// renames take. holder describes what already uses name, as
// ConsoleNameHolder does, or a preview being opened; serve must not start a
// node for the name unless holder is "". A flat creation that checked the
// name before the list held it has therefore either finished, and is the
// holder, or waits until serve returns and then finds the name reserved.
func (s *Service) ServeConsoleName(ctx context.Context, name string, serve func(holder string) error) error {
	unlock := s.lock(name)
	defer unlock()
	holder, err := s.ConsoleNameHolder(ctx, name)
	if err != nil {
		return err
	}
	if holder == "" {
		// Checked under one lock, as a reservation does: a preview
		// registers itself before it releases its own reservation.
		s.mu.Lock()
		_, isPreview := s.prevs[name]
		opening := s.adding.preview[name]
		s.mu.Unlock()
		if isPreview || opening {
			holder = "a preview"
		}
	}
	return serve(holder)
}

// ReserveConsoleName runs commit while name is reserved for a console
// domain being added. It holds name's operation lock, the one flat creation
// and renames take, so no flat can take name in between; previews skip it.
// It refuses a name a flat, preview, redirect or another console name holds.
// After commit returns, the console list or node record holds the name.
func (s *Service) ReserveConsoleName(ctx context.Context, name string, commit func() error) error {
	return s.reserveConsoleName(ctx, name, false, commit)
}

// ReserveSharedConsoleName is ReserveConsoleName for a name that may already
// be a console domain of the other network (the caller checked that it is
// not one of its own): the same name may serve the console on Tailscale and
// on NetBird.
func (s *Service) ReserveSharedConsoleName(ctx context.Context, name string, commit func() error) error {
	return s.reserveConsoleName(ctx, name, true, commit)
}

func (s *Service) reserveConsoleName(ctx context.Context, name string, shared bool, commit func() error) error {
	unlock := s.lock(name)
	defer unlock()
	if !shared && s.consoleNameInUse(name) {
		return fmt.Errorf("%w: %q is already a console domain or is still being removed", ErrConflict, name)
	}
	holder, err := s.ConsoleNameHolder(ctx, name)
	if err != nil {
		return err
	}
	if holder != "" {
		return fmt.Errorf("%w: %q is used by %s; choose another name", ErrConflict, name, holder)
	}
	// Checked and reserved under one lock: a preview registers itself before
	// it releases its own reservation, so one of the two always shows.
	s.mu.Lock()
	_, isPreview := s.prevs[name]
	if isPreview || s.adding.preview[name] || s.adding.console[name] {
		s.mu.Unlock()
		return fmt.Errorf("%w: %q is being used by a preview or another console domain; choose another name", ErrConflict, name)
	}
	if s.adding.console == nil {
		s.adding.console = map[string]bool{}
	}
	s.adding.console[name] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.adding.console, name)
		s.mu.Unlock()
	}()
	return commit()
}
