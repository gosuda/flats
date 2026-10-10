package zrok

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/go-openapi/runtime"
	httptransport "github.com/go-openapi/runtime/client"
	"github.com/openziti/sdk-golang/ziti"
	"github.com/openziti/zrok/v2/environment"
	"github.com/openziti/zrok/v2/environment/env_core"
	"github.com/openziti/zrok/v2/rest_client_zrok/share"
	"github.com/openziti/zrok/v2/rest_model_zrok"
)

// errNameTaken reports a name another zrok account already owns.
var errNameTaken = errors.New("another zrok account owns this name")

// backend is the zrok account and overlay operations Net uses. Tests
// substitute a fake; the real one is sdkBackend.
type backend interface {
	// ReserveName makes sure the account owns name in the namespace. It
	// returns the token of the share that currently holds the name, or "".
	ReserveName(ctx context.Context, name string) (holder string, err error)
	// NameHolder reports whether the account holds name, and the token of
	// the share that currently holds it, or "".
	NameHolder(ctx context.Context, name string) (holder string, found bool, err error)
	// ReleaseName deletes the account's reservation of name. A name the
	// account does not hold is not an error.
	ReleaseName(ctx context.Context, name string) error
	// Share creates a public share under name and returns its token and
	// frontend endpoints.
	Share(ctx context.Context, name string) (token string, endpoints []string, err error)
	// Unshare deletes a share of this environment. An unknown share is not
	// an error.
	Unshare(ctx context.Context, token string) error
	// Listen binds the share on the zrok overlay.
	Listen(token string) (net.Listener, error)
	Close() error
}

// sdkBackend talks to the zrok controller of an enabled environment.
type sdkBackend struct {
	root      env_core.Root
	namespace string

	mu   sync.Mutex
	zctx ziti.Context // created on the first Listen
}

// envMu serializes environment loading: zrok selects the environment
// directory through a package-level setting.
var envMu sync.Mutex

// loadRoot reads the zrok environment in dir ("" is the zrok default,
// ~/.zrok2). It reads files only.
func loadRoot(dir string) (env_core.Root, error) {
	envMu.Lock()
	defer envMu.Unlock()
	if dir == "" {
		dir = ".zrok2" // relative: under the home directory
	}
	environment.SetRootDirName(dir)
	root, err := environment.LoadRoot()
	if err != nil {
		return nil, err
	}
	if !root.IsEnabled() {
		return nil, errors.New("the zrok environment is not enabled; run `zrok2 enable <account token>` as the user that runs flats")
	}
	return root, nil
}

func newSDKBackend(cfg Config) (*sdkBackend, error) {
	root, err := loadRoot(cfg.Environment)
	if err != nil {
		return nil, err
	}
	return &sdkBackend{root: root, namespace: cfg.Namespace}, nil
}

func (b *sdkBackend) auth() runtime.ClientAuthInfoWriter {
	return httptransport.APIKeyAuth("X-TOKEN", "header", b.root.Environment().AccountToken)
}

func (b *sdkBackend) ReserveName(ctx context.Context, name string) (string, error) {
	c, err := b.root.Client()
	if err != nil {
		return "", err
	}
	holder, found, err := b.NameHolder(ctx, name)
	if err != nil || found {
		return holder, err
	}
	req := share.NewCreateShareNameParamsWithContext(ctx)
	req.Body = share.CreateShareNameBody{NamespaceToken: b.namespace, Name: name}
	if _, err := c.Share.CreateShareName(req, b.auth()); err != nil {
		var conflict *share.CreateShareNameConflict
		if errors.As(err, &conflict) {
			// Lost a race with ourselves, or another account owns it.
			if holder, found, ferr := b.NameHolder(ctx, name); ferr == nil && found {
				return holder, nil
			}
			return "", fmt.Errorf("%w: %q in namespace %q", errNameTaken, name, b.namespace)
		}
		return "", fmt.Errorf("reserve name %q: %w", name, err)
	}
	return "", nil
}

// NameHolder looks name up among the account's names in the namespace.
func (b *sdkBackend) NameHolder(ctx context.Context, name string) (holder string, found bool, err error) {
	c, err := b.root.Client()
	if err != nil {
		return "", false, err
	}
	req := share.NewListNamesForNamespaceParamsWithContext(ctx)
	req.NamespaceToken = b.namespace
	resp, err := c.Share.ListNamesForNamespace(req, b.auth())
	if err != nil {
		return "", false, fmt.Errorf("list names in namespace %q: %w", b.namespace, err)
	}
	for _, n := range resp.Payload {
		if n != nil && n.Name == name {
			return n.ShareToken, true, nil
		}
	}
	return "", false, nil
}

func (b *sdkBackend) ReleaseName(ctx context.Context, name string) error {
	_, found, err := b.NameHolder(ctx, name)
	if err != nil || !found {
		return err
	}
	c, err := b.root.Client()
	if err != nil {
		return err
	}
	req := share.NewDeleteShareNameParamsWithContext(ctx)
	req.Body = share.DeleteShareNameBody{NamespaceToken: b.namespace, Name: name}
	if _, err := c.Share.DeleteShareName(req, b.auth()); err != nil {
		var missing *share.DeleteShareNameNotFound
		if errors.As(err, &missing) {
			return nil
		}
		return fmt.Errorf("release name %q: %w", name, err)
	}
	return nil
}

func (b *sdkBackend) Share(ctx context.Context, name string) (string, []string, error) {
	c, err := b.root.Client()
	if err != nil {
		return "", nil, err
	}
	req := share.NewShareParamsWithContext(ctx)
	req.Body = &rest_model_zrok.ShareRequest{
		EnvZID:         b.root.Environment().ZitiIdentity,
		ShareMode:      "public",
		BackendMode:    "proxy",
		Target:         "flats:" + name,
		AuthScheme:     "none",
		PermissionMode: "closed",
		NameSelections: []*rest_model_zrok.NameSelection{{NamespaceToken: b.namespace, Name: name}},
	}
	resp, err := c.Share.Share(req, b.auth())
	if err != nil {
		var conflict *share.ShareConflict
		if errors.As(err, &conflict) {
			return "", nil, fmt.Errorf("share %q: %s", name, conflict.GetPayload())
		}
		return "", nil, fmt.Errorf("share %q: %w", name, err)
	}
	return resp.Payload.ShareToken, resp.Payload.FrontendProxyEndpoints, nil
}

func (b *sdkBackend) Unshare(ctx context.Context, token string) error {
	c, err := b.root.Client()
	if err != nil {
		return err
	}
	req := share.NewUnshareParamsWithContext(ctx)
	req.Body.EnvZID = b.root.Environment().ZitiIdentity
	req.Body.ShareToken = token
	if _, err := c.Share.Unshare(req, b.auth()); err != nil {
		var missing *share.UnshareNotFound
		if errors.As(err, &missing) {
			return nil
		}
		return fmt.Errorf("unshare %s: %w", token, err)
	}
	return nil
}

func (b *sdkBackend) context() (ziti.Context, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.zctx != nil {
		return b.zctx, nil
	}
	path, err := b.root.ZitiIdentityNamed(b.root.EnvironmentIdentityName())
	if err != nil {
		return nil, fmt.Errorf("zrok identity: %w", err)
	}
	cfg, err := ziti.NewConfigFromFile(path)
	if err != nil {
		return nil, fmt.Errorf("zrok identity: %w", err)
	}
	zctx, err := ziti.NewContext(cfg)
	if err != nil {
		return nil, fmt.Errorf("zrok overlay: %w", err)
	}
	b.zctx = zctx
	return zctx, nil
}

func (b *sdkBackend) Listen(token string) (net.Listener, error) {
	zctx, err := b.context()
	if err != nil {
		return nil, err
	}
	return zctx.ListenWithOptions(token, &ziti.ListenOptions{
		ConnectTimeout:               30 * time.Second,
		WaitForNEstablishedListeners: 1,
	})
}

func (b *sdkBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.zctx != nil {
		b.zctx.Close()
		b.zctx = nil
	}
	return nil
}
