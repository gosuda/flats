package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/flats/internal/store"
)

func validateEnvName(name, kind string) error {
	if !secretName.MatchString(name) {
		return invalidf("%s name %q must match [A-Z_][A-Z0-9_]* (at most 64 characters)", kind, name)
	}
	switch strings.ToUpper(name) {
	case "DB", "FILES", "__PROTO__", "PROTOTYPE", "CONSTRUCTOR":
		return invalidf("%s name %q is reserved by the runtime", kind, name)
	}
	return nil
}

func validateEnvValue(value, kind string) error {
	if len(value) > 64<<10 {
		return invalidf("%s value is larger than 64 KiB", kind)
	}
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return invalidf("%s value must be valid UTF-8 without NUL bytes", kind)
	}
	return nil
}

// SetEnv stores ordinary readable configuration. Running instances keep their
// environment until the next runtime start (deploy, preview, or host restart).
func (s *Service) SetEnv(ctx context.Context, slugName, name, value string, via Via) error {
	if err := validateEnvName(name, "environment variable"); err != nil {
		return err
	}
	if err := validateEnvValue(value, "environment variable"); err != nil {
		return err
	}
	if _, err := s.st.GetFlat(ctx, slugName); err != nil {
		return err
	}
	if err := s.st.PutEnv(ctx, slugName, store.EnvVar{Name: name, Value: value, UpdatedAt: s.now()}); err != nil {
		if errors.Is(err, store.ErrEnvCollision) {
			return invalidf("environment variable %q conflicts with an existing secret", name)
		}
		return err
	}
	s.Event(ctx, slugName, "info", "env", fmt.Sprintf("environment variable %s set via %s; redeploy to apply", name, via), nil)
	return nil
}

func (s *Service) DeleteEnv(ctx context.Context, slugName, name string, via Via) error {
	if _, err := s.st.GetFlat(ctx, slugName); err != nil {
		return err
	}
	if err := s.st.DeleteEnv(ctx, slugName, name); err != nil {
		return err
	}
	s.Event(ctx, slugName, "info", "env", fmt.Sprintf("environment variable %s deleted via %s; redeploy to apply", name, via), nil)
	return nil
}

// ListEnv returns ordinary values only; secrets remain in SecretNames.
func (s *Service) ListEnv(ctx context.Context, slugName string) ([]store.EnvVar, error) {
	if _, err := s.st.GetFlat(ctx, slugName); err != nil {
		return nil, err
	}
	return s.st.ListEnv(ctx, slugName)
}

func (s *Service) mergeEnvironment(ctx context.Context, slugName string, env map[string]string) (map[string]string, error) {
	vars, err := s.st.ListEnv(ctx, slugName)
	if err != nil {
		return nil, err
	}
	for _, v := range vars {
		if err := validateEnvName(v.Name, "environment variable"); err != nil {
			return nil, err
		}
		if err := validateEnvValue(v.Value, "environment variable"); err != nil {
			return nil, err
		}
		if _, ok := env[v.Name]; ok {
			return nil, invalidf("environment variable %q conflicts with an existing secret", v.Name)
		}
		env[v.Name] = v.Value
	}
	return env, nil
}
