package netbird

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"

	"github.com/netbirdio/netbird/client/embed"
)

// clientOptions is what the package needs to build an embedded NetBird peer.
type clientOptions struct {
	DeviceName    string
	ManagementURL string
	ConfigPath    string
	StatePath     string
	// SetupKey registers the peer when management does not know it. The
	// identity in ConfigPath, when present, is kept either way. Empty means:
	// log in with the identity persisted in ConfigPath only.
	SetupKey string
	// DNSLabels are extra DNS labels the peer asks NetBird for.
	DNSLabels []string
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
	// Start connects the peer and returns when it is up, fails, or ctx ends.
	// finished is called exactly once, with the outcome, when the underlying
	// start has really ended, which may be after Start returned.
	Start(ctx context.Context, finished func(error)) error
	Stop(ctx context.Context) error
	// ListenTCP listens on the peer's overlay address; only the port of
	// addr is used.
	ListenTCP(addr string) (net.Listener, error)
	Expose(ctx context.Context, req exposeRequest) (session, error)
	// Identity returns the peer's overlay FQDN and IP (either may be "").
	Identity() (fqdn, ip string, err error)
	// PeerFQDN returns the FQDN of the NetBird peer with overlay address ip,
	// or "".
	PeerFQDN(ip string) string
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
// embed.New demands exactly one of SetupKey, JWTToken or PrivateKey. With a
// setup key, embed loads (or creates) the identity in ConfigPath, and Start
// first attempts an ordinary management Login with it; the setup key is used
// only when management answers that the peer is unknown, and registers the
// same identity again. Without a setup key, the private key stored in
// ConfigPath is passed as PrivateKey, which equals what New would load from
// the file anyway; if management no longer knows the peer, Login then fails
// with a misleading setup-key error, which Net reports as ErrPeerRejected.
type embedClient struct {
	c *embed.Client
	// stop runs the embedded client's Stop; a field so tests can replace it.
	stop func(context.Context) error

	stopOnce sync.Once
	stopDone chan struct{} // closed when the one teardown has ended
	stopErr  error         // its error, readable after stopDone
}

// teardownError reports that a peer's teardown ended with an error: the peer
// no longer runs, but NetBird reported a problem while stopping it.
type teardownError struct{ err error }

func (e teardownError) Error() string { return "the peer stopped with an error: " + e.err.Error() }
func (e teardownError) Unwrap() error { return e.err }

func newEmbedClient(o clientOptions) (*embedClient, error) {
	if err := validateManagementURL(o.ManagementURL); err != nil {
		return nil, fmt.Errorf("netbird: %w", err)
	}
	opts := embed.Options{
		DeviceName:    o.DeviceName,
		ManagementURL: o.ManagementURL,
		ConfigPath:    o.ConfigPath,
		StatePath:     o.StatePath,
		LogOutput:     o.LogOutput,
		LogLevel:      "warn",
		DNSLabels:     o.DNSLabels,
		// Several peers run in one process: let each pick a free port.
		WireguardPort: new(int),
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
	return &embedClient{c: c, stop: c.Stop}, nil
}

// Start runs the embedded client's Start and returns when ctx ends. NetBird
// v0.80.0 authenticates with its own background context before it looks at
// ctx, retrying for up to two minutes, so against an unreachable management
// server Start alone ignores the deadline. The abandoned call finishes in the
// background; its client is then stopped, so a late success does not keep a
// connection nobody owns. finished reports when that call really ended.
func (e *embedClient) Start(ctx context.Context, finished func(error)) error {
	done := make(chan error, 1)
	go func() { done <- e.c.Start(ctx) }()
	select {
	case err := <-done:
		finished(err)
		return err
	case <-ctx.Done():
		go func() {
			err := <-done
			if err == nil {
				// Connected late: tear down and wait for the teardown, so
				// finished means nothing of this start runs any more.
				_ = e.Stop(context.Background())
			}
			finished(err)
		}()
		return ctx.Err()
	}
}

// Stop tears the peer down once and waits for that teardown, or ctx. NetBird
// v0.80.0's Stop forgets the client when its context ends, while the
// teardown goes on, and answers ErrClientNotStarted to every later call; so
// the teardown runs here without a deadline, and a later Stop after a timeout
// waits for the same teardown instead of starting another. A teardown that
// ended with an error is reported as teardownError: the peer no longer runs.
func (e *embedClient) Stop(ctx context.Context) error {
	e.stopOnce.Do(func() {
		e.stopDone = make(chan struct{})
		go func() {
			err := e.stop(context.Background())
			if errors.Is(err, embed.ErrClientNotStarted) {
				err = nil
			}
			e.stopErr = err
			close(e.stopDone)
		}()
	})
	select {
	case <-e.stopDone:
		if e.stopErr != nil {
			return teardownError{e.stopErr}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

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

func (e *embedClient) PeerFQDN(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	_, fqdn, ok := e.c.IdentityForIP(a.Unmap())
	if !ok {
		return ""
	}
	return strings.TrimSuffix(fqdn, ".")
}
