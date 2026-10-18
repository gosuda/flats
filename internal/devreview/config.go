package devreview

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ConfigEnv names an environment variable that overrides the default
// reviewer configuration path.
const ConfigEnv = "FLATS_REVIEW_CONFIG"

// Config is the user-level reviewer configuration. It lives outside the
// repository so that no reviewer command, program or model identity is
// committed.
type Config struct {
	Reviewers map[string]Reviewer `json:"reviewers"`
}

// Reviewer is one configured reviewer command.
type Reviewer struct {
	// Command is the program and arguments, run without a shell.
	Command []string `json:"command"`
	// RequestedModel is the canonical model ID the command requests.
	RequestedModel string `json:"requested_model"`
	// Timeout bounds one invocation, in time.ParseDuration syntax.
	Timeout string `json:"timeout,omitempty"`
	// FixedBinding, when set, is the user's reference to documentation
	// stating that the command's requested model ID is bound without
	// aliasing. It stands in for effective model metadata, never for the
	// session identity.
	FixedBinding string `json:"fixed_binding,omitempty"`
}

// DefaultTimeout bounds an invocation whose reviewer sets no timeout.
const DefaultTimeout = time.Hour

// DefaultConfigPath returns <user config dir>/flats-dev/reviewers.json.
func DefaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "flats-dev", "reviewers.json"), nil
}

// LoadReviewer reads the configuration at file, which must resolve outside
// the repository at root and must not be writable by other users, and
// returns the named reviewer and the resolved configuration path.
func LoadReviewer(root, file, name string) (Reviewer, string, error) {
	resolved, err := OutsideRepo(root, file)
	if err != nil {
		return Reviewer{}, "", err
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return Reviewer{}, "", err
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return Reviewer{}, "", fmt.Errorf("%s is writable by other users; it selects the command to run", resolved)
	}
	b, err := os.ReadFile(resolved)
	if err != nil {
		return Reviewer{}, "", err
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Reviewer{}, "", fmt.Errorf("%s: %w", resolved, err)
	}
	r, ok := c.Reviewers[name]
	if !ok {
		return Reviewer{}, "", fmt.Errorf("%s has no reviewer %q", resolved, name)
	}
	if len(r.Command) == 0 || r.Command[0] == "" {
		return Reviewer{}, "", fmt.Errorf("reviewer %q has no command", name)
	}
	if r.RequestedModel == "" {
		return Reviewer{}, "", fmt.Errorf("reviewer %q has no requested_model", name)
	}
	if r.Timeout != "" {
		if d, err := time.ParseDuration(r.Timeout); err != nil || d <= 0 {
			return Reviewer{}, "", fmt.Errorf("reviewer %q: timeout %q is not a positive duration", name, r.Timeout)
		}
	}
	return r, resolved, nil
}

func (r Reviewer) timeout() time.Duration {
	if d, err := time.ParseDuration(r.Timeout); err == nil && d > 0 {
		return d
	}
	return DefaultTimeout
}

// Identity is the metadata file a reviewer command or the user's adapter
// writes to the path in FLATS_REVIEW_IDENTITY_FILE.
type Identity struct {
	EffectiveModel string `json:"effective_model"`
	Session        string `json:"session"`
	// Source says where the values came from, such as the execution
	// metadata field or API response that reported them.
	Source string `json:"source"`
}

func readIdentity(file string) (*Identity, error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var id Identity
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&id); err != nil {
		return nil, fmt.Errorf("identity file: %w", err)
	}
	return &id, nil
}
