package zrok

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/go-openapi/runtime"
	httptransport "github.com/go-openapi/runtime/client"
	"github.com/go-openapi/strfmt"
	"github.com/openziti/sdk-golang/ziti"
	"github.com/openziti/zrok/v2/build"
	"github.com/openziti/zrok/v2/environment"
	"github.com/openziti/zrok/v2/environment/env_core"
	"github.com/openziti/zrok/v2/rest_client_zrok"
	"github.com/openziti/zrok/v2/rest_client_zrok/metadata"
	"github.com/openziti/zrok/v2/rest_client_zrok/share"
	"github.com/openziti/zrok/v2/rest_model_zrok"
)

// errNameTaken reports a name another zrok account already owns.
var errNameTaken = errors.New("another zrok account owns this name")

// errOtherEnvironment reports a share of another environment of the account.
var errOtherEnvironment = errors.New("the share belongs to zrok environment")

// errNameExists reports that CreateName found the name already existing.
var errNameExists = errors.New("the name exists")

// backend is the zrok account and overlay operations Net uses. Tests
// substitute a fake; the real one is sdkBackend.
type backend interface {
	// NameHolder reports whether the account holds name in namespace, and
	// the token of the share that currently holds it, or "".
	NameHolder(ctx context.Context, namespace, name string) (holder string, found bool, err error)
	// CreateName reserves name in namespace for the account. It returns
	// errNameExists when the name exists already, or the controller's
	// reason for another conflict, such as the account's name limit.
	CreateName(ctx context.Context, namespace, name string) error
	// ReleaseName deletes the account's reservation of name in namespace. A
	// name the account does not hold is not an error.
	ReleaseName(ctx context.Context, namespace, name string) error
	// Share creates a public share under name in namespace and returns its
	// token and frontend endpoints.
	Share(ctx context.Context, namespace, name string) (token string, endpoints []string, err error)
	// Unshare deletes a share of this environment. An unknown share is not
	// an error.
	Unshare(ctx context.Context, token string) error
	// ShareOwned reports whether token is a share of this environment with
	// target, which is how Flats marks the shares it creates.
	ShareOwned(ctx context.Context, token, target string) (bool, error)
	// Account identifies the zrok account of the environment without
	// revealing its token.
	Account() string
	// Listen binds the share on the zrok overlay.
	Listen(token string) (net.Listener, error)
	Close() error
}

// sdkBackend talks to the zrok controller of an enabled environment.
type sdkBackend struct {
	root env_core.Root

	mu     sync.Mutex
	client *rest_client_zrok.Zrok // set after the first successful version check
	zctx   ziti.Context           // created on the first Listen
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
	return &sdkBackend{root: root}, nil
}

func (b *sdkBackend) auth() runtime.ClientAuthInfoWriter {
	return httptransport.APIKeyAuth("X-TOKEN", "header", b.root.Environment().AccountToken)
}

// zrokClient returns the controller client. Like env_core.Root.Client it
// checks the client version first, but under ctx, and only once: Root.Client
// repeats that check on every call with its own timeout.
func (b *sdkBackend) zrokClient(ctx context.Context) (*rest_client_zrok.Zrok, error) {
	b.mu.Lock()
	c := b.client
	b.mu.Unlock()
	if c != nil {
		return c, nil
	}
	endpoint, _ := b.root.ApiEndpoint()
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("zrok api endpoint %q: %w", endpoint, err)
	}
	transport := httptransport.New(u.Host, "/api/v2", []string{u.Scheme})
	transport.Producers["application/zrok.v1+json"] = runtime.JSONProducer()
	transport.Consumers["application/zrok.v1+json"] = runtime.JSONConsumer()
	c = rest_client_zrok.New(transport, strfmt.Default)
	req := metadata.NewClientVersionCheckParamsWithContext(ctx)
	req.Body = metadata.ClientVersionCheckBody{ClientVersion: build.String()}
	if _, err := c.Metadata.ClientVersionCheck(req); err != nil {
		return nil, fmt.Errorf("zrok api endpoint %q: %w", endpoint, err)
	}
	b.mu.Lock()
	if b.client == nil {
		b.client = c
	}
	c = b.client
	b.mu.Unlock()
	return c, nil
}

// nameConflict explains a CreateShareName conflict. The controller sends no
// reason only when the name exists; a reason means another conflict, such as
// the account's name limit.
func nameConflict(name, reason string) error {
	if reason != "" {
		return fmt.Errorf("reserve name %q: %s", name, reason)
	}
	return errNameExists
}

func (b *sdkBackend) CreateName(ctx context.Context, namespace, name string) error {
	c, err := b.zrokClient(ctx)
	if err != nil {
		return err
	}
	req := share.NewCreateShareNameParamsWithContext(ctx)
	req.Body = share.CreateShareNameBody{NamespaceToken: namespace, Name: name}
	if _, err := c.Share.CreateShareName(req, b.auth()); err != nil {
		var conflict *share.CreateShareNameConflict
		if errors.As(err, &conflict) {
			return nameConflict(name, string(conflict.GetPayload()))
		}
		return fmt.Errorf("reserve name %q: %w", name, err)
	}
	return nil
}

// NameHolder looks name up among the account's names in the namespace.
func (b *sdkBackend) NameHolder(ctx context.Context, namespace, name string) (holder string, found bool, err error) {
	c, err := b.zrokClient(ctx)
	if err != nil {
		return "", false, err
	}
	req := share.NewListNamesForNamespaceParamsWithContext(ctx)
	req.NamespaceToken = namespace
	resp, err := c.Share.ListNamesForNamespace(req, b.auth())
	if err != nil {
		return "", false, fmt.Errorf("list names in namespace %q: %w", namespace, err)
	}
	for _, n := range resp.Payload {
		if n != nil && n.Name == name {
			return n.ShareToken, true, nil
		}
	}
	return "", false, nil
}

func (b *sdkBackend) ReleaseName(ctx context.Context, namespace, name string) error {
	c, err := b.zrokClient(ctx)
	if err != nil {
		return err
	}
	req := share.NewDeleteShareNameParamsWithContext(ctx)
	req.Body = share.DeleteShareNameBody{NamespaceToken: namespace, Name: name}
	if _, err := c.Share.DeleteShareName(req, b.auth()); err != nil {
		var missing *share.DeleteShareNameNotFound
		if errors.As(err, &missing) {
			return nil
		}
		return fmt.Errorf("release name %q: %w", name, err)
	}
	return nil
}

func (b *sdkBackend) Share(ctx context.Context, namespace, name string) (string, []string, error) {
	c, err := b.zrokClient(ctx)
	if err != nil {
		return "", nil, err
	}
	req := share.NewShareParamsWithContext(ctx)
	req.Body = &rest_model_zrok.ShareRequest{
		EnvZID:         b.root.Environment().ZitiIdentity,
		ShareMode:      "public",
		BackendMode:    "proxy",
		Target:         shareTarget(name),
		AuthScheme:     "none",
		PermissionMode: "closed",
		NameSelections: []*rest_model_zrok.NameSelection{{NamespaceToken: namespace, Name: name}},
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
	c, err := b.zrokClient(ctx)
	if err != nil {
		return err
	}
	req := share.NewUnshareParamsWithContext(ctx)
	req.Body.EnvZID = b.root.Environment().ZitiIdentity
	req.Body.ShareToken = token
	if _, err := c.Share.Unshare(req, b.auth()); err != nil {
		var missing *share.UnshareNotFound
		if !errors.As(err, &missing) {
			return fmt.Errorf("unshare %s: %w", token, err)
		}
		// Not found in this environment: deleted already, or a share of
		// another environment of the account, which only that environment
		// can delete.
		detail := metadata.NewGetShareDetailParamsWithContext(ctx)
		detail.ShareToken = token
		resp, derr := c.Metadata.GetShareDetail(detail, b.auth())
		var gone *metadata.GetShareDetailNotFound
		switch {
		case errors.As(derr, &gone):
			return nil
		case derr != nil:
			return fmt.Errorf("unshare %s: confirm it is gone: %w", token, derr)
		default:
			return fmt.Errorf("unshare %s: %w %s; remove it from that environment", token, errOtherEnvironment, resp.Payload.EnvZID)
		}
	}
	return nil
}

// shareTarget marks a share Flats created for name.
func shareTarget(name string) string { return "flats:" + name }

func (b *sdkBackend) ShareOwned(ctx context.Context, token, target string) (bool, error) {
	c, err := b.zrokClient(ctx)
	if err != nil {
		return false, err
	}
	req := metadata.NewGetShareDetailParamsWithContext(ctx)
	req.ShareToken = token
	resp, err := c.Metadata.GetShareDetail(req, b.auth())
	if err != nil {
		var missing *metadata.GetShareDetailNotFound
		if errors.As(err, &missing) {
			return false, nil
		}
		return false, fmt.Errorf("share %s detail: %w", token, err)
	}
	return resp.Payload.EnvZID == b.root.Environment().ZitiIdentity && resp.Payload.Target == target, nil
}

func (b *sdkBackend) Account() string {
	sum := sha256.Sum256([]byte(b.root.Environment().AccountToken))
	return hex.EncodeToString(sum[:8])
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
		ConnectTimeout:               20 * time.Second, // below the 30s shutdown bound
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
