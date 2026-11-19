package portal

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gosuda/portal-tunnel/v2/portal/identity"
	"github.com/gosuda/portal-tunnel/v2/sdk/appauth"
	"github.com/gosuda/portal-tunnel/v2/types"
	"github.com/gosuda/portal-tunnel/v2/utils"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/durable"
)

// The sign-in gate of docs/spec/portal-sign-in.md on the Portal route (rules 33 to 40). The gate is built where the
// identity is: around the handler box of the entry, so a redeploy or a rollback replaces the handler inside the gate.
// RunHTTP serves the front of the entry, which is the gate or, while sign-in is off, the handler box as before.

// checkTimeout bounds the background check of an oidc provider (rule 37). Tests shorten it.
var checkTimeout = 15 * time.Second

var (
	_ core.SignInPublicNet    = (*Net)(nil)
	_ core.LifecycleSignInNet = (*Net)(nil)
)

// gateState is what the front of an entry was built from. The slug lock guards it.
type gateState struct {
	set      bool  // an open carried a setting or the mark redirect
	redirect bool  // the mark redirect of rule 41: no gate
	seq      int64 // the sign-in sequence of the setting
	on       bool  // a gate is in front
	cfg      core.SignInGate
	// stale says that the gate in front does not hold the identity of the entry, after a rebuild for a new token
	// secret failed: the front refuses every request, and the next open builds the gate again.
	stale bool
	// kept says that the identity file holds the token secret of the identity (rule 36).
	kept bool
}

// frontHandler is what RunHTTP of an entry serves through.
type frontHandler struct{ h http.Handler }

// serveFront serves a request of the exposure through the current front.
func (e *entry) serveFront(w http.ResponseWriter, r *http.Request) {
	e.front.Load().h.ServeHTTP(w, r)
}

// closedFront answers while a route whose gate failed stops, so no request reaches the flat without the gate.
var closedFront = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Retry-After", "30")
	http.Error(w, "This flat is not served on this address now.", http.StatusServiceUnavailable)
})

// markSeq marks each request that the gate let in with the sequence that the gate was built from (rule 33), which
// the public site handler compares with the stored one (rule 44).
func markSeq(seq int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(core.WithSignInGate(r.Context(), seq)))
	})
}

// wantsFront reports whether the open si must build the front of e again (rule 38). An open without a setting never
// removes a gate, the mark redirect always does, and a gate of a higher sequence stays.
func wantsFront(e *entry, si *core.PortalSignIn) bool {
	g := e.gate
	switch {
	case e.front.Load() == nil:
		return true
	case si == nil:
		return false
	case si.Redirect || g.redirect || !g.set:
		return true
	case si.Seq < 0:
		// The setting could not be read, so the gate in front may be of a setting that changed.
		return true
	case si.Seq < g.seq:
		return false
	case si.Seq > g.seq:
		return true
	}
	return si.Rebuild || g.stale
}

// applySignIn puts in front of e what the open si asks for, under the slug lock. It reports whether it built a new
// front. When it fails, the front is unchanged and the caller must not serve the flat.
func (n *Net) applySignIn(e *entry, si *core.PortalSignIn) (bool, error) {
	if !wantsFront(e, si) {
		return false, nil
	}
	front, err := n.frontFor(e, si)
	if err != nil {
		return false, err
	}
	e.front.Store(&frontHandler{front})
	g := gateState{set: si != nil, kept: e.gate.kept}
	if si != nil {
		g.redirect, g.seq, g.on = si.Redirect, si.Seq, si.On()
		if g.on {
			g.cfg = si.Gate
		}
	}
	e.gate = g
	if !g.on || g.cfg.Provider != appauth.ProviderOIDC {
		// No check runs for this front: a failure of an earlier oidc gate is no longer its state, and a check of that
		// gate that is still running must not report it.
		e.mu.Lock()
		e.checkGen++
		e.checkErr = ""
		e.mu.Unlock()
	}
	return true, nil
}

// frontFor builds the front of e for the open si: the handler box without a gate while sign-in is off and for the
// mark redirect, and otherwise the gate (rule 33). Each error is a reason of rule 35 with the next action.
func (n *Net) frontFor(e *entry, si *core.PortalSignIn) (http.Handler, error) {
	plain := publicHandler(e.handler)
	if !si.On() {
		return plain, nil
	}
	if si.Err != nil {
		return nil, si.Err
	}
	if err := n.keepTokenSecret(e); err != nil {
		return nil, err
	}
	h, err := appauth.New(markSeq(si.Seq, plain), e.id, gateConfig(e, si.Gate))
	if err != nil {
		return nil, fmt.Errorf("the gate refuses the stored setting: %v; request one that it accepts with `flats signin set %s`", err, e.slug)
	}
	return h, nil
}

// gateConfig is the configuration of a gate of rule 33: no identity headers and no identity proof, because the host
// sets its own headers (rule 45), and the host names of rule 34.
func gateConfig(e *entry, g core.SignInGate) appauth.Config {
	cfg := appauth.Config{Provider: g.Provider, PublicHosts: func() []appauth.PublicHost { return publicHosts(e) }}
	if g.Provider == appauth.ProviderOIDC {
		cfg.EmailList = slices.Clone(g.Allow)
		cfg.OIDCIssuerURL, cfg.OIDCClientID, cfg.OIDCClientSecret = g.Issuer, g.ClientID, g.ClientSecret
	} else {
		cfg.Allow = slices.Clone(g.Allow)
	}
	return cfg
}

// publicHosts gives, on each call, every relay that the exposure reports, whatever its state (rule 34): its canonical
// URL, which only this identity can hold, and its alias since this exposure came to hold it. A relay reports
// connecting while a burst of visitors takes its idle reverse sessions, so its state is no signal; a lost alias leaves
// the list, and one that comes back is a new hold.
func publicHosts(e *entry) []appauth.PublicHost {
	var hosts []appauth.PublicHost
	for _, s := range e.exp.Relays() {
		if s.PublicURL != "" {
			hosts = append(hosts, appauth.PublicHost{URL: s.PublicURL})
		}
		if s.AliasURL != "" {
			hosts = append(hosts, appauth.PublicHost{URL: s.AliasURL, Since: s.AliasSince})
		}
	}
	return hosts
}

// hostOf returns the host name of a public URL as the gate compares it: lower case, without the default https port
// and a trailing dot, or "" when raw has none.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(u.Host), ":443"), ".")
}

// startCheck runs appauth.Check for an oidc gate of e in the background, outside the slug lock and the path of a
// request, within checkTimeout (rule 37). A failure is a warning and the detail of the endpoint until a check of the
// latest build passes; the gate stays as it was built.
func (n *Net) startCheck(e *entry) {
	g := e.gate
	if !g.on || g.cfg.Provider != appauth.ProviderOIDC {
		return
	}
	cfg := gateConfig(e, g.cfg)
	cfg.PublicHosts = nil
	e.mu.Lock()
	e.checkGen++
	gen := e.checkGen
	e.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(n.ctx, checkTimeout)
		defer cancel()
		err := appauth.Check(ctx, cfg)
		if n.ctx.Err() != nil {
			return
		}
		e.mu.Lock()
		latest := e.checkGen == gen
		if latest {
			e.checkErr = ""
			if err != nil {
				e.checkErr = err.Error()
			}
		}
		e.mu.Unlock()
		if latest && err != nil {
			n.logf("portal: %s: sign-in provider check failed: %v; visitors cannot sign in until the provider answers", e.slug, err)
		}
	}()
}

// gateFailed records a gate that could not be built (rule 35): the detail of the endpoint, one line in the host log
// with the slug and the reason, and the error of the open.
func (n *Net) gateFailed(slug string, cause, stopErr error) error {
	reason := cause.Error()
	n.mu.Lock()
	n.gateErrs[slug] = reason
	n.mu.Unlock()
	n.logf("portal: %s: sign-in gate: %s", slug, reason)
	return &core.SignInGateError{Reason: reason, StopErr: stopErr}
}

// keepTokenSecret checks, before the first build of a gate for the identity of e, that the identity file holds its
// token secret (rule 36). The SDK makes a new random one at each load of a file without one, which would end every
// session and link at each restart, so the file is written again with the identity that was loaded: the address,
// the keys and the canonical host name stay.
func (n *Net) keepTokenSecret(e *entry) error {
	if e.gate.kept {
		return nil
	}
	path, err := n.identityPath(e.slug)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("the identity file %s cannot be read: %v; deploy again or restart when it can", path, err)
	}
	var stored struct {
		TokenSecret string `json:"token_secret"`
	}
	if json.Unmarshal(raw, &stored) == nil && strings.TrimSpace(stored.TokenSecret) != "" {
		e.gate.kept = true
		return nil
	}
	data, err := identity.Marshal(e.id)
	if err == nil {
		err = writePrivateFile(path, data)
	}
	if err != nil {
		return fmt.Errorf("the identity file %s has no token secret, and it cannot be written with one: %v; make it writable, then deploy again or restart", path, err)
	}
	e.gate.kept = true
	return nil
}

// writePrivateFile replaces path with data atomically through a new private file: mode 0600, and on Windows the
// access list that the SDK gives an identity file, which the rename keeps (docs/spec/windows-support.md, rule 18).
func writePrivateFile(path string, data []byte) error {
	var b [8]byte
	_, _ = rand.Read(b[:])
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp-"+hex.EncodeToString(b[:]))
	if err := utils.CreatePrivateFile(tmp, data); err != nil {
		return err
	}
	// The token secret signs every session: a power loss must not leave an empty identity file behind the rename.
	if err := syncFile(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := durable.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return durable.SyncDir(dir)
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// renewTokenSecret gives the identity in path a new token secret and writes it with writePrivateFile. It reports
// false without an error when there is no identity file.
func renewTokenSecret(path string) (types.Identity, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return types.Identity{}, false, nil
	}
	if err != nil {
		return types.Identity{}, false, err
	}
	id, err := identity.Parse(raw)
	if err != nil {
		return types.Identity{}, false, err
	}
	// The SDK makes a token secret of 32 random bytes; a new one ends every session and credential of the identity.
	var secret [32]byte
	_, _ = rand.Read(secret[:])
	id.TokenSecret = base64.RawURLEncoding.EncodeToString(secret[:])
	data, err := identity.Marshal(id)
	if err != nil {
		return types.Identity{}, false, err
	}
	if err := writePrivateFile(path, data); err != nil {
		return types.Identity{}, false, err
	}
	return id, true, nil
}

// RenewTokenSecretFile gives the identity file of slug in dir a new token secret, for a host whose Portal backend
// does not run (rules 41 and 42). It returns the path of the file, and false without an error when there is none.
func RenewTokenSecretFile(dir, slug string) (string, bool, error) {
	path, err := identityFile(dir, slug)
	if err != nil {
		return "", false, err
	}
	_, renewed, err := renewTokenSecret(path)
	return path, renewed, err
}

// ServeSignIn is Serve with the sign-in that the open carries (rule 38). A gate that cannot be built gives the slug
// no route: a new entry opens none, and a served entry stops and unregisters (rule 35). The error is then a
// *core.SignInGateError.
func (n *Net) ServeSignIn(ctx context.Context, slug string, h http.Handler, hidden bool, si *core.PortalSignIn) (string, error) {
	return n.serve(ctx, slug, h, hidden, si)
}

// RenewPortalIdentity gives the identity of slug a new token secret (rules 41, 42 and 54) and builds the gate of a
// served entry again with it, so no session or credential of the old token secret passes it.
func (n *Net) RenewPortalIdentity(ctx context.Context, slug string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	l := n.slugLock(slug)
	l.Lock()
	defer l.Unlock()
	path, err := n.identityPath(slug)
	if err != nil {
		return "", false, err
	}
	id, renewed, err := renewTokenSecret(path)
	if err != nil || !renewed {
		return path, false, err
	}
	e := n.entry(slug)
	if e == nil {
		return path, true, nil
	}
	e.id = id
	e.gate.kept = true
	if !e.gate.on {
		return path, true, nil
	}
	front, err := n.frontFor(e, &core.PortalSignIn{Seq: e.gate.seq, Gate: e.gate.cfg})
	if err != nil {
		// Never the old token secret: refuse every request until an open builds the gate.
		e.front.Store(&frontHandler{closedFront})
		e.gate.stale = true
		n.logf("portal: %s: sign-in gate: %v", slug, err)
		return path, true, nil
	}
	e.front.Store(&frontHandler{front})
	e.gate.stale = false
	n.startCheck(e)
	return path, true, nil
}

// IssuePortalCredential makes an access link from the identity of the entry of slug for the canonical host name of
// its first relay (rule 51). A link never names an alias, which can change hands.
func (n *Net) IssuePortalCredential(ctx context.Context, slug, subject string, expiresAt time.Time) (core.PortalCredential, error) {
	if err := ctx.Err(); err != nil {
		return core.PortalCredential{}, err
	}
	l := n.slugLock(slug)
	l.Lock()
	defer l.Unlock()
	e := n.entry(slug)
	if e == nil {
		return core.PortalCredential{}, core.ErrNoPortalRoute
	}
	e.mu.Lock()
	stopping, pinned, primary := e.stopping, e.pinned, e.primary
	e.mu.Unlock()
	if stopping {
		return core.PortalCredential{}, core.ErrNoPortalRoute
	}
	host := ""
	for _, s := range n.ordered(e.exp.Relays(), pinned, primary) {
		if host = hostOf(s.PublicURL); host != "" {
			break
		}
	}
	if host == "" {
		return core.PortalCredential{}, core.ErrNoPortalRoute
	}
	cred, err := appauth.IssueCredential(e.id, host, subject, expiresAt)
	if err != nil {
		return core.PortalCredential{}, err
	}
	link, err := appauth.CredentialRedeemURL(host, cred)
	if err != nil {
		return core.PortalCredential{}, err
	}
	return core.PortalCredential{URL: link, Host: host}, nil
}

// PortalHosts returns the host names of the PublicHosts of the entry of slug (rule 34), in relay order without
// duplicates, and nil when slug is not served.
func (n *Net) PortalHosts(slug string) []string {
	e := n.entry(slug)
	if e == nil {
		return nil
	}
	out := []string{}
	for _, h := range publicHosts(e) {
		if host := hostOf(h.URL); host != "" && !slices.Contains(out, host) {
			out = append(out, host)
		}
	}
	return out
}
