package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/flats/internal/egress"
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
// captured environment through automatic worker restarts. Approved deployment
// activations (including rollback and data restore), new previews, and Flats
// host restarts capture stored settings again.
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

// runtimeEnvironment includes operator network grants and is captured at activation,
// not approval: health checking
// and live startup use the same settings. Secrets determine the redactor before
// ordinary values are merged. Runtime.Start receives clones of values and origins.
type runtimeEnvironment struct {
	values         map[string]string
	networkOrigins []string
	fetchHeaders   []egress.Header // injected secrets, never in values
	redact         func(string) string
}

func (s *Service) captureEnvironment(ctx context.Context, slugName string, v store.Version) (*runtimeEnvironment, error) {
	if v.Kind != "server" {
		return nil, nil
	}
	vars, secs, origins, err := s.st.RuntimeSettingsSnapshot(ctx, slugName)
	if err != nil {
		return nil, err
	}
	secrets, err := s.decryptSecrets(secs)
	if err != nil {
		return nil, err
	}
	origins, err = egress.NormalizeOrigins(origins)
	if err != nil {
		return nil, invalidf("invalid stored network policy")
	}
	// The redactor knows every secret value, injected or readable (rule 22).
	snapshot := &runtimeEnvironment{redact: newRedactor(secrets), networkOrigins: origins}
	snapshot.fetchHeaders, err = injectedSecrets(slugName, secs, secrets)
	if err != nil {
		return nil, err
	}
	env := make(map[string]string, len(secrets)+len(vars))
	for _, sec := range secs {
		if !sec.Inject.Injected() {
			env[sec.Name] = secrets[sec.Name]
		}
	}
	snapshot.values = env
	for _, v := range vars {
		if err := validateEnvName(v.Name, "environment variable"); err != nil {
			return nil, err
		}
		if err := validateEnvValue(v.Value, "environment variable"); err != nil {
			return nil, err
		}
		if _, ok := secrets[v.Name]; ok {
			return nil, invalidf("environment variable %q conflicts with an existing secret", v.Name)
		}
		env[v.Name] = v.Value
	}
	return snapshot, nil
}

// The store and the outbound client count the same limit (rule 8).
const _ = uint(store.MaxInjectedSecrets-egress.MaxInjected) + uint(egress.MaxInjected-store.MaxInjectedSecrets)

// injectedSecrets checks the injected rows of a capture again (rule 20) and returns them as the list of the worker.
// A row that fails refuses the activation, and no value of it goes into env. No message holds a value.
func injectedSecrets(slugName string, secs []store.SealedSecret, values map[string]string) ([]egress.Header, error) {
	var out []egress.Header
	seen := map[store.Binding]string{}
	for _, sec := range secs {
		b := sec.Inject
		if !b.Injected() {
			continue
		}
		refuse := func(reason string) error {
			return invalidf("secret %q has a stored binding that the host cannot use: %s. Remove it with `flats secret rm %s %s` and set it again with --origin and --header",
				sec.Name, reason, slugName, sec.Name)
		}
		if b.Origin == "" || b.Header == "" {
			return nil, refuse("it has only one of its origin and its header")
		}
		if origin, err := egress.InjectOrigin(b.Origin); err != nil || origin != b.Origin {
			return nil, refuse("its origin is not a normalized https origin")
		}
		if header, err := egress.InjectHeader(b.Header); err != nil || header != b.Header {
			return nil, refuse("its header is not a canonical header name that can carry a secret")
		}
		if err := egress.CheckInjectValue(values[sec.Name]); err != nil {
			return nil, refuse(err.Error())
		}
		// The checks above leave the header canonical, so equal bindings are equal values.
		if other, ok := seen[b]; ok {
			return nil, refuse(fmt.Sprintf("secret %q has the same origin and header", other))
		}
		seen[b] = sec.Name
		out = append(out, egress.Header{Origin: b.Origin, Name: b.Header, Value: values[sec.Name]})
	}
	if len(out) > store.MaxInjectedSecrets {
		return nil, invalidf("flat %s has %d injected secrets, and a flat can have %d. List them with `flats secret ls %s` and remove some with `flats secret rm`",
			slugName, len(out), store.MaxInjectedSecrets, slugName)
	}
	return out, nil
}
