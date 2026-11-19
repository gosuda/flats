package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gosuda/portal-tunnel/v2/sdk/appauth"
	"github.com/gosuda/portal-tunnel/v2/types"

	"github.com/gosuda/flats/internal/store"
	"github.com/gosuda/flats/internal/webhook"
)

// The sign-in of docs/spec/portal-sign-in.md on the routes of a flat: what an open of the Portal route carries
// (rule 38), the other public providers that stay closed (rule 10), the check of each request and the headers of the
// visitor (rules 44 to 46), the new token secret (rules 41, 42 and 54) and the access links (rules 49 to 55). The
// Portal provider builds the gate itself.

// The headers of rule 45, which the host sets for a visitor that a gate signed in.
const (
	HeaderVisitorProvider = "X-Flats-Visitor-Provider"
	HeaderVisitorSubject  = "X-Flats-Visitor-Subject"
	HeaderVisitorEmail    = "X-Flats-Visitor-Email"
)

// visitorHeaders are the headers of rule 46: no client copy of one reaches flat code on any route, also on a route
// without the gate, because flat code may trust the names of Portal.
var visitorHeaders = []string{HeaderVisitorProvider, HeaderVisitorSubject, HeaderVisitorEmail,
	types.HeaderPortalUser, types.HeaderPortalAuth, types.HeaderPortalProof, types.HeaderAccessCredential}

// The texts of rule 44.
const (
	signInNeededText   = "This flat needs sign-in. Open it at its Portal address."
	signInChangingText = "The sign-in of this flat is changing. Try again."
)

// The limits of an access link (rule 50).
const (
	signInLinkDefault = 7 * 24 * time.Hour
	signInLinkMin     = time.Minute
	signInLinkMax     = 90 * 24 * time.Hour
)

// dropVisitorHeaders removes each spelling of the headers of rule 46 from h. A spelling differs in case or has _ for
// -, which a worker may read as the same name.
func dropVisitorHeaders(h http.Header) {
	for key := range h {
		for _, name := range visitorHeaders {
			if strings.EqualFold(strings.ReplaceAll(key, "_", "-"), name) {
				delete(h, key)
				break
			}
		}
	}
}

// signInPassed is the check of rule 44 for a request of the public site of f while its sign-in is on: only a request
// that a gate of the stored sequence let in passes, on every public provider. It answers every other request and
// returns the visitor of one that passes.
func signInPassed(w http.ResponseWriter, r *http.Request, f store.Flat) (appauth.Principal, bool) {
	p, signedIn := appauth.PrincipalFromContext(r.Context())
	seq, gated := SignInGateSeq(r.Context())
	switch {
	case !signedIn || !gated:
		http.Error(w, signInNeededText, http.StatusForbidden)
		return appauth.Principal{}, false
	case seq != f.SignInSeq:
		// A gate of an older setting let it in, between the commit of a change and the new gate (rule 21).
		w.Header().Set("Retry-After", "5")
		http.Error(w, signInChangingText, http.StatusServiceUnavailable)
		return appauth.Principal{}, false
	}
	return p, true
}

// setVisitorHeaders sets the headers of rule 45 on r, which withRouteHeaders cleared of every client copy. A value
// that is not visible ASCII and the space is left out: rule 4 and the SDK keep every value so, and a header must
// never carry a line break into a worker.
func setVisitorHeaders(r *http.Request, p appauth.Principal) {
	for _, h := range []struct{ name, value string }{{HeaderVisitorProvider, p.Provider}, {HeaderVisitorSubject, p.Subject}, {HeaderVisitorEmail, p.Email}} {
		if h.value != "" && !strings.ContainsFunc(h.value, func(r rune) bool { return r != ' ' && !visibleASCII(r) }) {
			r.Header.Set(h.name, h.value)
		}
	}
}

// withoutSignInExcluded drops the public providers that sign-in excludes (rule 10): while sign-in is on, the host asks
// no public provider but Portal to serve the flat, also when a stored grant permits one.
func withoutSignInExcluded(ids []ProviderID) []ProviderID {
	return slices.DeleteFunc(slices.Clone(ids), func(id ProviderID) bool { return store.SignInExcludes(string(id)) })
}

// portalSignIn reads the stored sign-in of a flat for an open of its Portal route (rule 38). The caller holds the
// lock of the flat. A setting or a client secret that cannot be read gives an open that the Portal route refuses
// (rule 35), never one without a gate.
func (s *Service) portalSignIn(ctx context.Context, slugName string) *PortalSignIn {
	st, err := s.st.GetSignIn(ctx, slugName)
	if err != nil {
		return &PortalSignIn{Seq: -1, Err: fmt.Errorf("the sign-in setting of %s cannot be read: %v; a deploy, a grant or a restart tries again", slugName, err)}
	}
	g, err := s.SignInGate(ctx, slugName, st.SignIn)
	if err != nil {
		return &PortalSignIn{Seq: st.Seq, Err: err}
	}
	return &PortalSignIn{Seq: st.Seq, Gate: g}
}

// servePublicNet serves h for slug on the Portal backend without a provider manager, with the stored sign-in. A
// backend that cannot run the gate never serves a flat with sign-in on (rule 35).
func (s *Service) servePublicNet(ctx context.Context, slugName string, h http.Handler, hidden, rebuild bool) (string, error) {
	si := s.portalSignIn(ctx, slugName)
	si.Rebuild = rebuild
	if g, ok := s.cfg.Public.(SignInPublicNet); ok {
		return g.ServeSignIn(ctx, slugName, h, hidden, si)
	}
	if !si.On() {
		return s.cfg.Public.Serve(ctx, slugName, h, hidden)
	}
	ge := &SignInGateError{Reason: "this Portal backend cannot run a sign-in gate"}
	if s.state(slugName).publicServed {
		ge.StopErr = s.cfg.Public.Stop(slugName)
	}
	return "", ge
}

// reopenPublic serves the public routes that a flat uses now again, with a new public handler and the stored sign-in,
// so the Portal route builds its gate from the stored setting (rules 20 and 38). After revokeRoute the new handler
// binds a new epoch; without it the epoch of the open connections stays. rebuild builds the gate again also when one
// of the stored sequence is in place. It runs under the lock of the flat. Its error says that the Portal route stopped
// (a *SignInGateError), or that a read failed and stopped it (a *routesUnread). A flat without a public route has
// nothing to open: its gate is built when one opens (rule 7).
func (s *Service) reopenPublic(ctx context.Context, slugName string, rebuild bool) error {
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return s.closePortal(ctx, slugName, err)
	}
	if !f.Visibility.Public() || s.state(slugName).cur.Load() == nil {
		return nil
	}
	ln, ok := s.lifecycleNet()
	if !ok {
		lf := s.state(slugName)
		if !lf.publicServed || s.cfg.Public == nil {
			return nil
		}
		hidden, err := s.portalHidden(ctx, slugName)
		if err == nil {
			_, err = s.servePublicNet(ctx, slugName, s.siteHandler(slugName, true), hidden, rebuild)
		} else {
			err = s.closePortal(ctx, slugName, err)
		}
		if ge := (*SignInGateError)(nil); errors.As(err, &ge) && ge.StopErr == nil {
			lf.publicServed = false
		}
		return err
	}
	observer, ok := s.cfg.Lifecycle.(LifecycleObserver)
	if !ok {
		return s.closePortal(ctx, slugName, errors.New("the provider manager does not report the current routes"))
	}
	permitted, err := s.permittedIDs(ctx, slugName)
	if err != nil {
		return s.closePortal(ctx, slugName, err)
	}
	si := s.portalSignIn(ctx, slugName)
	si.Rebuild = rebuild
	if si.On() {
		permitted = withoutSignInExcluded(permitted)
	}
	status, err := observer.ExposureStatus(ctx, slugName)
	if err != nil {
		return s.closePortal(ctx, slugName, fmt.Errorf("read current routes: %w", err))
	}
	// The public routes that the approved exposure uses, as reapplyExposure takes them: a Portal route whose gate
	// failed before is among them, so a sign-in or a client secret change tries it again (rule 35).
	serving, public := privateProviders(permitted), false
	for _, ep := range status.Endpoints {
		if PublicProvider(ep.Provider) && ep.Audience == AudienceCurrent && ep.Host == slugName &&
			slices.Contains(permitted, ep.Provider) && !slices.Contains(serving, ep.Provider) && ep.State != "" && ep.State != "stopped" {
			serving = append(serving, ep.Provider)
			public = true
		}
	}
	if !public {
		return nil
	}
	hidden, err := s.portalHidden(ctx, slugName)
	if err != nil {
		return s.closePortal(ctx, slugName, err)
	}
	_, err = ln.ServeExposure(ctx, ExposureRequest{Slug: slugName, Host: slugName, Visibility: "public", Audience: AudienceCurrent,
		Handler: s.siteHandler(slugName, true), PrivateHandler: s.siteHandler(slugName, false), Permitted: serving, Hidden: hidden, SignIn: si})
	if ge := (*SignInGateError)(nil); errors.As(err, &ge) {
		return ge
	}
	if err != nil {
		s.Event(ctx, slugName, "warn", "exposure", "a public route did not open again after a sign-in change: "+err.Error(), nil)
	}
	return nil
}

// signInNet is the sign-in part of the Portal route: the provider manager, or a Portal backend without one.
func (s *Service) signInNet() (LifecycleSignInNet, bool) {
	if _, ok := s.lifecycleNet(); ok {
		n, ok := s.cfg.Lifecycle.(LifecycleSignInNet)
		return n, ok
	}
	n, ok := s.cfg.Public.(SignInPublicNet)
	return n, ok
}

// closePortal is reopenPublic for a read that failed with cause: the host cannot tell which gate the Portal route
// needs, so it stops the route as rule 35 stops one whose gate cannot be built, and no gate of an earlier setting,
// client secret or token secret stays in front (rules 28 and 54).
func (s *Service) closePortal(ctx context.Context, slugName string, cause error) error {
	u := &routesUnread{slug: slugName, cause: cause}
	if n, ok := s.signInNet(); ok {
		reason := fmt.Sprintf("the routes of %s cannot be read to build its gate again: %v; a deploy, a grant, a restart or a sign-in change tries again", slugName, cause)
		u.portal = n.FailPortalGate(context.WithoutCancel(ctx), slugName, reason)
	}
	return u
}

// routesUnread is the error of a reopenPublic whose read failed. portal is the stop of the Portal route of the flat, a
// *SignInGateError, and nil when the flat had no Portal route.
type routesUnread struct {
	slug   string
	cause  error
	portal error
}

func (e *routesUnread) Error() string {
	if e.portal != nil {
		return e.portal.Error()
	}
	return fmt.Sprintf("the routes of %s cannot be read: %v", e.slug, e.cause)
}

func (e *routesUnread) Unwrap() error { return e.portal }

// SignInRoutesNotReopened is the phrase of the message of a revocation (rule 54) whose public routes did not open
// again: its Portal route is stopped, or a read failed. The console shows such a message as an error.
const SignInRoutesNotReopened = "its public routes did not open again"

// routesText is the clause of a message that says the state of the public routes of a flat after reopenPublic failed
// with err (rules 21, 28 and 54). revoked says that revokeRoute ran before, so a public route that did not open again
// answers 503 on the cancelled epoch.
func routesText(slugName string, err error, revoked bool) string {
	u := (*routesUnread)(nil)
	if !errors.As(err, &u) {
		return fmt.Sprintf("the Portal route of %s is stopped: %v", slugName, err)
	}
	switch {
	case u.portal != nil && revoked:
		return fmt.Sprintf("the Portal route of %s is stopped: %v; any other public route of %s answers 503 until a deploy, a grant or a restart serves it again",
			slugName, u.portal, slugName)
	case u.portal != nil:
		return fmt.Sprintf("the Portal route of %s is stopped: %v", slugName, u.portal)
	case revoked:
		return fmt.Sprintf("the routes of %s cannot be read: %v; it has no Portal route, and its public routes answer 503 until a deploy, a grant or a restart serves them again",
			slugName, u.cause)
	}
	return fmt.Sprintf("the routes of %s cannot be read: %v; it has no Portal route, so no gate was built again", slugName, u.cause)
}

// SignInHosts returns the host names of the Portal route of a flat (rule 56): every host name that its gate accepts,
// also of a relay that reports connecting. It is empty without a Portal route.
func (s *Service) SignInHosts(slugName string) []string {
	if n, ok := s.signInNet(); ok {
		if hosts := n.PortalHosts(slugName); hosts != nil {
			return hosts
		}
	}
	return []string{}
}

// SignInCallbackURLs returns the OIDC callback URL of each host name, which the operator registers at the provider
// (rule 56): the SDK sends https://<request host>/_portal/auth/callback.
func SignInCallbackURLs(hosts []string) []string {
	out := make([]string, len(hosts))
	for i, h := range hosts {
		out[i] = "https://" + h + types.PathApplicationAuthCallback
	}
	return out
}

// renewForChange gives the Portal identity of a flat a new token secret before a rename or a deletion commits (rules
// 41 and 42), also while sign-in is off, because links of an earlier credential setting can exist. It returns the
// audit of the signin.revoke event that the change writes in its transaction, nil when the flat has no identity
// file. When the write fails, the change must fail and change nothing; the message names the file.
func (s *Service) renewForChange(ctx context.Context, slugName string, via Via, change string) (*store.Audit, error) {
	n, ok := s.signInNet()
	if !ok {
		return nil, nil
	}
	path, renewed, err := n.RenewPortalIdentity(ctx, slugName)
	if err != nil {
		return nil, fmt.Errorf("the %s of %s changed nothing: the Portal identity file %s cannot get a new token secret, which ends the sessions and access links of its host names: %w",
			change, slugName, path, err)
	}
	if !renewed {
		return nil, nil
	}
	a := s.audit(ctx, via)
	return &a, nil
}

// renewalOutlived records the new token secret of a rename or a deletion that failed after renewForChange, before its
// commit (rules 41, 42 and 67): the flat stays as it was, but every session and access link of its identity ended.
// The signin.revoke row of the cause, with the audit a of renewForChange, and a flat event say so, and so does the
// returned error. a is nil when the flat had no identity file, and err comes back as it is.
func (s *Service) renewalOutlived(ctx context.Context, slugName, cause string, a *store.Audit, err error) error {
	if a == nil {
		return err
	}
	change := "rename"
	if cause == store.SignInRevokeDelete {
		change = "deletion"
	}
	bg := context.WithoutCancel(ctx)
	text := fmt.Sprintf("every sign-in session and access link of %s ended: its Portal identity got a new token secret before the %s failed", slugName, change)
	if auditErr := s.st.AppendSignInRevoke(bg, slugName, cause, *a); auditErr != nil {
		text += ", and its audit row could not be written: " + auditErr.Error()
	}
	s.Event(bg, slugName, "warn", "signin", text, nil)
	return fmt.Errorf("%w; %s", err, text)
}

// RevokeSignIn ends every sign-in session and every access link of a flat (rule 54): the Portal identity gets a new
// token secret, the open connections of the public routes end, and the gate is built again. The host names stay. It
// needs no approval, because it narrows access. MCP cannot call it.
func (s *Service) RevokeSignIn(ctx context.Context, slugName string, via Via) (ActionResult, error) {
	switch via {
	case ViaCLI, ViaAPI, ViaConsole:
	default:
		return ActionResult{}, forbiddenf("the sessions and access links of a flat are ended with `flats signin revoke` on the Flats host or in the console")
	}
	unlock, err := s.lockCtx(ctx, slugName)
	if err != nil {
		return ActionResult{}, err
	}
	defer unlock()
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return ActionResult{}, err
	}
	n, ok := s.signInNet()
	if !ok {
		return ActionResult{Status: "done", Message: "no Portal identity"}, nil
	}
	path, renewed, err := n.RenewPortalIdentity(ctx, slugName)
	if err != nil {
		return ActionResult{}, fmt.Errorf("the sessions and access links of %s did not end: the Portal identity file %s cannot get a new token secret: %w", slugName, path, err)
	}
	if !renewed {
		return ActionResult{Status: "done", Message: "no Portal identity"}, nil
	}
	// The row follows the write of the file (rule 67); a failure does not undo the new token secret.
	auditErr := s.st.AppendSignInRevoke(ctx, slugName, "", s.audit(ctx, via))
	s.revokeRoute("public:" + slugName)
	msg := fmt.Sprintf("every sign-in session and access link of %s ended via %s", slugName, via)
	if err := s.reopenPublic(ctx, slugName, false); err != nil {
		msg += "; " + SignInRoutesNotReopened + ": " + routesText(slugName, err, true)
	}
	s.Event(ctx, slugName, "info", "signin", msg, nil)
	s.notifySignInAction(ctx, f, "revoke_signin", &webhook.Change{Empty: true})
	if auditErr != nil {
		return ActionResult{}, fmt.Errorf("%s, but its audit row could not be written: %w", msg, auditErr)
	}
	return ActionResult{Status: "done", Message: msg}, nil
}

// SignInLink is an access link of rule 51. The link is in this response only (rule 52).
type SignInLink struct {
	URL       string    `json:"url"`
	Subject   string    `json:"subject"`
	Host      string    `json:"host"`
	ExpiresAt time.Time `json:"expires_at"`
}

// String keeps the link out of a formatted value.
func (l SignInLink) String() string {
	return fmt.Sprintf("access link for %s until %s", l.Host, l.ExpiresAt.Format(time.RFC3339))
}

// GoString keeps %#v from writing the link.
func (l SignInLink) GoString() string { return l.String() }

// IssueSignInLink issues an access link of the credential sign-in of a flat (rules 49 to 52): for a subject of the
// approved allowlist, on the canonical host name of the first relay of its Portal route, until expires (7 days from
// now when zero). Only the console issues one. No log line, flat event, audit row or webhook body holds the link.
func (s *Service) IssueSignInLink(ctx context.Context, slugName, subject string, expires time.Time, via Via) (SignInLink, error) {
	if via != ViaConsole {
		return SignInLink{}, forbiddenf("only the console issues an access link, on the Sign-in row of the Settings tab of the flat")
	}
	unlock, err := s.lockCtx(ctx, slugName)
	if err != nil {
		return SignInLink{}, err
	}
	defer unlock()
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return SignInLink{}, err
	}
	st, err := s.st.GetSignIn(ctx, slugName)
	if err != nil {
		return SignInLink{}, err
	}
	if st.SignIn.Provider != store.SignInCredential {
		return SignInLink{}, withKind(ErrConflict, fmt.Errorf("flat %s has no credential sign-in, and an access link needs one: request it with `flats signin set %s credential --allow SUBJECT`", slugName, slugName))
	}
	norm, err := sdkNormalizeAllow(store.SignInCredential, []string{subject})
	if err != nil || len(norm) != 1 || !slices.Contains(st.SignIn.Allow, norm[0]) {
		return SignInLink{}, invalidf("member subject is not on the allowlist of the sign-in of %s: a link names one subject of the approved allowlist (rules 3 and 49)", slugName)
	}
	now := s.now()
	if expires.IsZero() {
		expires = now.Add(signInLinkDefault)
	}
	// A credential holds whole seconds.
	expires = expires.UTC().Truncate(time.Second)
	if d := expires.Sub(now); d < signInLinkMin || d > signInLinkMax {
		return SignInLink{}, invalidf("member expires must be from 1 minute to 90 days from now (rule 50)")
	}
	n, ok := s.signInNet()
	if !ok {
		return SignInLink{}, signInNoRoute(slugName)
	}
	cred, err := n.IssuePortalCredential(ctx, slugName, norm[0], expires)
	if errors.Is(err, ErrNoPortalRoute) {
		return SignInLink{}, signInNoRoute(slugName)
	}
	if err != nil {
		return SignInLink{}, fmt.Errorf("issue an access link of %s: %w", slugName, err)
	}
	// A link that no audit row records is never shown.
	if err := s.st.AppendSignInLink(ctx, slugName, store.SignInLink{Subject: norm[0], Host: cred.Host, ExpiresAt: expires}, s.audit(ctx, ViaConsole)); err != nil {
		return SignInLink{}, err
	}
	// The flat event names no subject: get_logs shows it, and no MCP output holds an entry (rule 58).
	s.Event(ctx, slugName, "info", "signin", fmt.Sprintf("access link issued on %s via console; it expires at %s", cred.Host, expires.Format(time.RFC3339)), nil)
	s.notifySignInAction(ctx, f, "issue_signin_link", &webhook.Change{ExpiresAt: expires.Format(time.RFC3339)})
	return SignInLink{URL: cred.URL, Subject: norm[0], Host: cred.Host, ExpiresAt: expires}, nil
}

func signInNoRoute(slugName string) error {
	return withKind(ErrConflict, fmt.Errorf("flat %s has no Portal route with a ready relay: it must be Public on Portal with a ready relay to issue an access link", slugName))
}

// notifySignInAction sends signin.changed for an issued access link or a revocation (rule 68). The body names no
// subject, credential or link.
func (s *Service) notifySignInAction(ctx context.Context, f store.Flat, operation string, change *webhook.Change) {
	if !s.notifying() {
		return
	}
	s.notify(webhook.Payload{
		Type: eventSignInChange, Flat: webhook.FlatRef{Slug: f.Slug, ID: f.ID}, Operation: operation, State: "applied",
		ConsoleURL: s.consoleBase(ctx) + "/flats/" + f.Slug, Change: change,
	})
}
