package zrok

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	httptransport "github.com/go-openapi/runtime/client"
	"github.com/openziti/zrok/v2/environment"
	"github.com/openziti/zrok/v2/environment/env_core"
	restEnvironment "github.com/openziti/zrok/v2/rest_client_zrok/environment"
)

// ErrConnected reports that dir already holds an enabled zrok environment.
var ErrConnected = errors.New("a zrok account is already connected")

// ErrRejected reports that zrok refused the account token.
var ErrRejected = errors.New("zrok rejected the account token, or the account has reached its environment limit")

// Connected reports whether dir holds an enabled zrok environment. It reads
// files only.
func Connected(dir string) (bool, error) {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	_, _, err := loadRoot(dir)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, errNotEnabled):
		return false, nil
	default:
		return false, err
	}
}

// Connect enables a zrok environment for the account of token in dir, as
// `zrok2 enable` does in ~/.zrok2: zrok registers this machine as an
// environment of the account and issues its overlay identity. The token is
// kept only in dir (mode 0600), as zrok itself keeps it. description names the
// environment in the zrok console.
func Connect(ctx context.Context, dir, token, description string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("zrok: environment directory %q is not absolute", dir)
	}
	if token == "" {
		return errors.New("zrok: account token is required")
	}
	envMu.Lock()
	defer envMu.Unlock()
	environment.SetRootDirName(dir)
	root, err := environment.LoadRoot()
	if err != nil {
		return fmt.Errorf("zrok: %w", err)
	}
	if root.IsEnabled() {
		return ErrConnected
	}
	endpoint, _ := root.ApiEndpoint()
	c, err := controllerClient(ctx, endpoint)
	if err != nil {
		return fmt.Errorf("zrok: %w", err)
	}
	host, _ := os.Hostname()
	req := restEnvironment.NewEnableParamsWithContext(ctx)
	req.Body.Description = description
	req.Body.Host = host
	auth := httptransport.APIKeyAuth("X-TOKEN", "header", token)
	resp, err := c.Environment.Enable(req, auth)
	if err != nil {
		var unauthorized *restEnvironment.EnableUnauthorized
		if errors.As(err, &unauthorized) {
			return fmt.Errorf("zrok: %w", ErrRejected)
		}
		return fmt.Errorf("zrok: enable the environment: %w", err)
	}
	identity := resp.Payload.Identity
	save := func() error {
		if err := root.SetEnvironment(&env_core.Environment{AccountToken: token, ZitiIdentity: identity, ApiEndpoint: endpoint}); err != nil {
			return err
		}
		return root.SaveZitiIdentityNamed(root.EnvironmentIdentityName(), resp.Payload.Cfg)
	}
	if err := save(); err != nil {
		// Do not leave an environment registered that this host cannot use.
		// Roll back under its own deadline, even if ctx was cancelled, and
		// delete the saved token only once zrok confirmed: otherwise keep it
		// so a later Disconnect can still unregister the environment.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		dreq := restEnvironment.NewDisableParamsWithContext(rctx)
		dreq.Body.Identity = identity
		if _, derr := c.Environment.Disable(dreq, auth); derr != nil {
			return fmt.Errorf("zrok: save the environment: %w (and unregistering it failed: %v; Disconnect retries it)", err, derr)
		}
		_ = root.Obliterate()
		return fmt.Errorf("zrok: save the environment: %w", err)
	}
	return nil
}

// ErrStale reports that zrok refused every token offered for unregistering
// an environment. The environment may still be registered at zrok, for
// example after its account token was regenerated elsewhere.
var ErrStale = errors.New("zrok refused the environment's account token, so it may still be registered at zrok")

// Disconnect unregisters the zrok environment in dir from its account and
// removes dir, including the account token and identity. A dir without an
// enabled environment is removed without contacting zrok. When zrok refuses
// the stored token, dir is kept and ErrStale returned, unless force is set:
// then dir is removed although the environment may stay registered.
func Disconnect(ctx context.Context, dir string, force bool) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("zrok: environment directory %q is not absolute", dir)
	}
	envMu.Lock()
	defer envMu.Unlock()
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	err := unregisterLocked(ctx, dir)
	if err != nil && !(force && errors.Is(err, ErrStale)) {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("zrok: remove the environment: %w", err)
	}
	return nil
}

// unregisterLocked unregisters the enabled environment in dir with its own
// token, then with each of extra (another token of the same account). envMu
// is held.
func unregisterLocked(ctx context.Context, dir string, extra ...string) error {
	environment.SetRootDirName(dir)
	root, err := environment.LoadRoot()
	if err != nil {
		return fmt.Errorf("zrok: %w", err)
	}
	if !root.IsEnabled() {
		return nil
	}
	env := root.Environment()
	endpoint, _ := root.ApiEndpoint()
	c, err := controllerClient(ctx, endpoint)
	if err != nil {
		return fmt.Errorf("zrok: %w", err)
	}
	for _, token := range append([]string{env.AccountToken}, extra...) {
		req := restEnvironment.NewDisableParamsWithContext(ctx)
		req.Body.Identity = env.ZitiIdentity
		_, err := c.Environment.Disable(req, httptransport.APIKeyAuth("X-TOKEN", "header", token))
		if err == nil {
			return nil
		}
		var unauthorized *restEnvironment.DisableUnauthorized
		if !errors.As(err, &unauthorized) {
			return fmt.Errorf("zrok: unregister environment %s: %w", env.ZitiIdentity, err)
		}
	}
	return fmt.Errorf("zrok: environment %s: %w", env.ZitiIdentity, ErrStale)
}

// discardStaged unregisters and removes a staged environment, keeping it when
// zrok does not confirm the unregistration.
func discardStaged(ctx context.Context, next string) error {
	envMu.Lock()
	defer envMu.Unlock()
	if _, err := os.Stat(next); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := unregisterLocked(ctx, next); err != nil {
		return fmt.Errorf("zrok: a staged environment from an earlier replacement is still registered: %w", err)
	}
	if err := os.RemoveAll(next); err != nil {
		return fmt.Errorf("zrok: %w", err)
	}
	return nil
}

// Account describes the zrok environment in a directory without its token.
type Account struct {
	Connected     bool   `json:"connected"`
	EnvironmentID string `json:"environment_id,omitempty"` // the environment's overlay identity at zrok
	APIEndpoint   string `json:"api_endpoint,omitempty"`
}

// Describe reports the zrok environment in dir. It reads files only and never
// returns the account token.
func Describe(dir string) (Account, error) {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return Account{}, nil
	}
	root, _, err := loadRoot(dir)
	if errors.Is(err, errNotEnabled) {
		return Account{}, nil
	}
	if err != nil {
		return Account{}, err
	}
	env := root.Environment()
	return Account{Connected: true, EnvironmentID: env.ZitiIdentity, APIEndpoint: env.ApiEndpoint}, nil
}

// Replace connects the account of token in place of the environment in dir.
// The new environment is enabled first, next to dir; only when that works is
// the old one unregistered, with its own token or else the new one, and
// replaced. A rejected new token, or an old environment neither token can
// unregister (ErrStale: another account whose token changed), leaves the
// current account connected.
func Replace(ctx context.Context, dir, token, description string) error {
	next := dir + ".next"
	// A staged environment left by an earlier replacement may still be
	// registered; unregister it before its credentials are deleted.
	if err := discardStaged(ctx, next); err != nil {
		return err
	}
	if err := Connect(ctx, next, token, description); err != nil {
		return err
	}
	envMu.Lock()
	defer envMu.Unlock()
	// The old token may have been regenerated: then the new token, of the
	// same account, unregisters the old environment.
	if err := unregisterLocked(ctx, dir, token); err != nil {
		// Roll the staged environment back even if ctx was cancelled; if
		// that fails too, it stays for the next replacement to retry.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if uerr := unregisterLocked(rctx, next); uerr == nil {
			_ = os.RemoveAll(next)
		}
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("zrok: remove the old environment: %w", err)
	}
	if err := os.Rename(next, dir); err != nil {
		return fmt.Errorf("zrok: install the new environment: %w", err)
	}
	return nil
}
