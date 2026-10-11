package netbird

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/netbirdio/netbird/client/embed"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// clientOptions is what the package needs to build an embedded NetBird peer.
type clientOptions struct {
	DeviceName    string
	ManagementURL string
	ConfigPath    string
	StatePath     string
	// SetupKey enrolls a new peer. It is only set by Enroll and never
	// stored. Empty means: restart from the identity persisted in
	// ConfigPath.
	SetupKey  string
	LogOutput io.Writer
}

// exposeRequest is the subset of NetBird's expose request Flats uses: an
// HTTP service on a local overlay port, with no Pin, Password or UserGroups
// (a Public flat is open to anyone).
type exposeRequest struct {
	NamePrefix string
	Port       uint16
}

// session is a live NetBird expose session.
type session interface {
	// URL is the public address the NetBird reverse proxy assigned.
	URL() string
	// Wait keeps the session alive. It returns when ctx is canceled (after
	// terminating the session) or when keep-alive fails.
	Wait(ctx context.Context) error
}

// client is the part of an embedded NetBird peer that Net needs.
type client interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	// ListenTCP listens on the peer's overlay address; only the port of
	// addr is used.
	ListenTCP(addr string) (net.Listener, error)
	Expose(ctx context.Context, req exposeRequest) (session, error)
	// Identity returns the peer's overlay FQDN and IP (either may be "").
	Identity() (fqdn, ip string, err error)
	// PublicKey returns the peer's WireGuard public key.
	PublicKey() string
}

// newClient builds a client; tests replace it with a fake.
var newClient = func(o clientOptions) (client, error) { return newEmbedClient(o) }

// storedPrivateKey reads the peer identity embed persisted in path.
func storedPrivateKey(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var c struct{ PrivateKey string }
	if err := json.Unmarshal(b, &c); err != nil {
		return "", fmt.Errorf("unreadable netbird config: %w", err)
	}
	if c.PrivateKey == "" {
		return "", errors.New("netbird config holds no peer identity")
	}
	return c.PrivateKey, nil
}

// embedClient adapts embed.Client.
//
// Restart from persisted state: embed.New demands exactly one of SetupKey,
// JWTToken or PrivateKey, but a registered peer needs none of them. embed's
// Start first attempts an ordinary management Login with the stored key
// pair; the setup key is consulted only when management answers that the
// peer is unknown (registration needed). So a restart passes the private key
// stored in ConfigPath as PrivateKey, which equals what New would load from
// the file anyway and satisfies the validation without any new secret. If
// management no longer knows the peer (removed in the dashboard), Login
// fails with a registration error and the operator must enroll again.
type embedClient struct {
	c *embed.Client
}

func newEmbedClient(o clientOptions) (*embedClient, error) {
	opts := embed.Options{
		DeviceName:    o.DeviceName,
		ManagementURL: o.ManagementURL,
		ConfigPath:    o.ConfigPath,
		StatePath:     o.StatePath,
		LogOutput:     o.LogOutput,
		LogLevel:      "warn",
	}
	if o.SetupKey != "" {
		opts.SetupKey = o.SetupKey
	} else {
		key, err := storedPrivateKey(o.ConfigPath)
		if err != nil {
			return nil, fmt.Errorf("load the stored NetBird identity: %w", err)
		}
		opts.PrivateKey = key
	}
	c, err := embed.New(opts)
	if err != nil {
		return nil, err
	}
	return &embedClient{c: c}, nil
}

// Start runs the embedded client's Start and returns when ctx ends. NetBird
// v0.80.0 authenticates with its own background context before it looks at
// ctx, so against an unreachable management server Start alone ignores the
// deadline. The abandoned call finishes in the background; its client is then
// stopped, so a late success does not keep a connection nobody owns. (The
// management server may still register the peer if the abandoned login later
// succeeds; the operator removes such a peer in the NetBird dashboard.)
func (e *embedClient) Start(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- e.c.Start(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		go func() {
			if err := <-done; err == nil {
				sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = e.c.Stop(sctx)
			}
		}()
		return ctx.Err()
	}
}
func (e *embedClient) Stop(ctx context.Context) error { return e.c.Stop(ctx) }

func (e *embedClient) ListenTCP(addr string) (net.Listener, error) { return e.c.ListenTCP(addr) }

func (e *embedClient) Expose(ctx context.Context, req exposeRequest) (session, error) {
	s, err := e.c.Expose(ctx, embed.ExposeRequest{
		NamePrefix: req.NamePrefix,
		Port:       req.Port,
		Protocol:   embed.ExposeProtocolHTTP,
	})
	if err != nil {
		return nil, err
	}
	return embedSession{s}, nil
}

type embedSession struct{ s *embed.ExposeSession }

func (s embedSession) URL() string {
	if s.s.ServiceURL != "" {
		return s.s.ServiceURL
	}
	return s.s.Domain
}
func (s embedSession) Wait(ctx context.Context) error { return s.s.Wait(ctx) }

func (e *embedClient) Identity() (string, string, error) {
	st, err := e.c.Status()
	if err != nil {
		return "", "", err
	}
	fqdn := strings.TrimSuffix(st.LocalPeerState.FQDN, ".")
	ip := st.LocalPeerState.IP
	if p, err := netip.ParsePrefix(ip); err == nil {
		ip = p.Addr().String()
	}
	return fqdn, ip, nil
}

func (e *embedClient) PublicKey() string {
	cfg, err := e.c.GetConfig()
	if err != nil {
		return ""
	}
	k, err := wgtypes.ParseKey(cfg.PrivateKey)
	if err != nil {
		return ""
	}
	return k.PublicKey().String()
}

// fingerprintOf derives the account fingerprint from the peer public key.
// The hash keeps the value stable and short; it is not a secret.
func fingerprintOf(publicKey string) string {
	sum := sha256.Sum256([]byte("flats-netbird:" + publicKey))
	return hex.EncodeToString(sum[:16])
}
