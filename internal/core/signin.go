package core

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/store"
	"github.com/gosuda/flats/internal/webhook"
)

// The sign-in of docs/spec/portal-sign-in.md in the service: the setting, its request and its approval, the refusals
// of rule 8 and the client secret. The gate on the Portal route and the check of each request belong to the Portal
// route, which signInApplied and signInSecretChanged reach.

// actionSetSignIn is the action of a sign-in approval (rule 16).
const actionSetSignIn = "set_signin"

// Limits of rules 2, 4, 14 and 26.
const (
	maxSignInEntries  = 100
	maxSignInIssuer   = 2048
	maxSignInClientID = 256
	maxSignInEntry    = 256
	maxSignInReason   = 1 << 10
	maxSignInSecret   = 4096
)

// ErrSignInSecretBinding reports a stored client secret that is bound to another issuer or client ID than the oidc
// setting (rule 27). The gate must not send it.
var ErrSignInSecretBinding = errors.New("the stored sign-in client secret belongs to another issuer or client ID")

// SignInRequest is a requested sign-in setting (rule 14). The empty Provider turns sign-in off.
type SignInRequest struct {
	Provider string
	Issuer   string
	ClientID string
	Allow    []string
	Reason   string
}

// DecodeSignInRequest reads the body of rule 14. A member that the provider does not take, a member that is missing
// and a member of the wrong type give ErrInvalid with a message that names the member. The values are checked by
// RequestSignIn.
func DecodeSignInRequest(body []byte) (SignInRequest, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(body, &members); err != nil || members == nil {
		return SignInRequest{}, invalidf("the body must be a JSON object with the member provider")
	}
	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		switch name {
		case "provider", "issuer", "client_id", "allow", "reason":
		default:
			return SignInRequest{}, invalidf("member %q is not a member of a sign-in request: the members are provider, issuer, client_id, allow and reason", name)
		}
	}
	if _, ok := members["provider"]; !ok {
		return SignInRequest{}, invalidf(`member provider is missing: it is oidc, credential, or "" to turn sign-in off`)
	}
	var req SignInRequest
	for _, m := range []struct {
		name string
		dst  *string
	}{{"provider", &req.Provider}, {"issuer", &req.Issuer}, {"client_id", &req.ClientID}, {"reason", &req.Reason}} {
		raw, ok := members[m.name]
		if !ok {
			continue
		}
		if err := json.Unmarshal(raw, m.dst); err != nil || string(raw) == "null" {
			return SignInRequest{}, invalidf("member %s must be a string", m.name)
		}
	}
	if raw, ok := members["allow"]; ok {
		if err := json.Unmarshal(raw, &req.Allow); err != nil || req.Allow == nil {
			return SignInRequest{}, invalidf("member allow must be a list of strings")
		}
	}
	var takes []string
	switch req.Provider {
	case store.SignInOIDC:
		takes = []string{"issuer", "client_id", "allow"}
	case store.SignInCredential:
		takes = []string{"allow"}
	case "":
	default:
		return SignInRequest{}, invalidf(`member provider must be oidc, credential, or "" to turn sign-in off`)
	}
	for _, name := range []string{"issuer", "client_id", "allow"} {
		_, has := members[name]
		switch want := slices.Contains(takes, name); {
		case has && !want:
			return SignInRequest{}, invalidf("member %s is not taken by %s", name, signInProviderText(req.Provider))
		case !has && want:
			return SignInRequest{}, invalidf("member %s is missing: %s needs it", name, signInProviderText(req.Provider))
		}
	}
	return req, nil
}

func signInProviderText(provider string) string {
	if provider == "" {
		return "a request that turns sign-in off"
	}
	return "the provider " + provider
}

// normalizeSignIn applies rules 2 to 4 to a request and returns the setting that the host stores. Each refusal is
// ErrInvalid and names the member and the rule that it breaks.
func normalizeSignIn(req SignInRequest) (store.SignIn, error) {
	var out store.SignIn
	switch req.Provider {
	case "":
		switch {
		case req.Issuer != "":
			return out, invalidf("member issuer is not taken by %s", signInProviderText(""))
		case req.ClientID != "":
			return out, invalidf("member client_id is not taken by %s", signInProviderText(""))
		case len(req.Allow) > 0:
			return out, invalidf("member allow is not taken by %s", signInProviderText(""))
		}
		out.Allow = []string{}
		return out, nil
	case store.SignInCredential:
		if req.Issuer != "" || req.ClientID != "" {
			return out, invalidf("members issuer and client_id are not taken by %s", signInProviderText(req.Provider))
		}
	case store.SignInOIDC:
		var err error
		if out.Issuer, err = signInIssuer(req.Issuer); err != nil {
			return out, err
		}
		if out.ClientID, err = signInClientID(req.ClientID); err != nil {
			return out, err
		}
	default:
		return out, invalidf(`member provider must be oidc, credential, or "" to turn sign-in off`)
	}
	out.Provider = req.Provider
	allow, err := signInAllow(req.Provider, req.Allow)
	if err != nil {
		return out, err
	}
	out.Allow = allow
	if err := sdkValidate(out); err != nil {
		// The checks above refuse each issuer that the SDK refuses, so its message never repeats the issuer of the
		// caller; the guard keeps it so if the SDK words it otherwise.
		msg := err.Error()
		if out.Issuer != "" && strings.Contains(msg, out.Issuer) {
			msg = "the issuer is not one that the sign-in gate accepts"
		}
		return out, invalidf("the sign-in gate refuses the setting: %s (every stored setting passes appauth.Validate)", msg)
	}
	return out, nil
}

// signInIssuer checks an issuer by rule 2 and returns the form that the host stores. No message repeats the text of
// the caller, which can hold user information or a query.
func signInIssuer(raw string) (string, error) {
	v := strings.TrimRight(strings.TrimSpace(raw), "/")
	switch {
	case v == "":
		return "", invalidf("member issuer is missing: an oidc setting needs the https URL of its issuer (rule 2)")
	case len(v) > maxSignInIssuer:
		return "", invalidf("member issuer has more than %d bytes (rule 2)", maxSignInIssuer)
	case strings.ContainsFunc(v, func(r rune) bool { return !urlChar(r) }):
		return "", invalidf("member issuer has a character that an https URL does not hold unescaped (rule 2)")
	case strings.ContainsAny(v, "?#"):
		return "", invalidf("member issuer must be an https URL without a query or a fragment (rule 2)")
	}
	u, err := url.Parse(v)
	switch {
	case err != nil:
		return "", invalidf("member issuer is not a URL (rule 2)")
	case u.Scheme != "https":
		return "", invalidf("member issuer must be an https URL; the host refuses http, also on a loopback host (rule 2)")
	case u.User != nil:
		return "", invalidf("member issuer must be an https URL without user information (rule 2)")
	case u.Hostname() == "":
		return "", invalidf("member issuer must be an https URL with a host name (rule 2)")
	}
	return v, nil
}

// urlChar reports whether r may stand unescaped in a URL (RFC 3986: the unreserved and the reserved characters, and
// % of an escape).
func urlChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("-._~:/?#[]@!$&'()*+,;=%", r)
}

// visibleASCII reports whether r is a visible ASCII character.
func visibleASCII(r rune) bool { return r > ' ' && r < 0x7f }

// signInClientID checks a client ID by rule 2.
func signInClientID(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	switch {
	case v == "":
		return "", invalidf("member client_id is missing: an oidc setting needs the client ID that the provider gave (rule 2)")
	case len(v) > maxSignInClientID:
		return "", invalidf("member client_id has more than %d bytes (rule 2)", maxSignInClientID)
	case strings.ContainsFunc(v, func(r rune) bool { return !visibleASCII(r) }):
		return "", invalidf("member client_id must have only visible ASCII characters (rule 2)")
	}
	return v, nil
}

// signInAllow normalizes an allowlist as appauth.Config.Normalize does, checks rule 4 on each entry, and returns the
// list without duplicates in byte order.
func signInAllow(provider string, raw []string) ([]string, error) {
	switch {
	case len(raw) == 0:
		return nil, invalidf("member allow needs at least one entry: %s with an empty list would let in any visitor that it signs in (rules 3 and 4)", signInProviderText(provider))
	case len(raw) > maxSignInEntries:
		return nil, invalidf("member allow has %d entries, and a list has at most %d (rule 4)", len(raw), maxSignInEntries)
	}
	list, err := sdkNormalizeAllow(provider, raw)
	if err != nil {
		return nil, invalidf("member allow: %v (rules 2 to 4)", err)
	}
	for _, e := range list {
		n := len(e)
		if provider == store.SignInOIDC {
			n = len(strings.TrimPrefix(e, "sub:"))
		}
		switch {
		case n == 0:
			return nil, invalidf("member allow has an empty entry (rule 4)")
		case n > maxSignInEntry:
			return nil, invalidf("member allow has an entry of more than %d bytes (rule 4)", maxSignInEntry)
		case strings.ContainsFunc(e, func(r rune) bool { return r != ' ' && !visibleASCII(r) }):
			return nil, invalidf("member allow has an entry with a character that is not visible ASCII or a space (rule 4)")
		}
	}
	slices.Sort(list)
	return slices.Compact(list), nil
}

// signInParams is the frozen state of a set_signin approval (rule 16). It holds no client secret.
type signInParams struct {
	FlatID       string       `json:"flat_id"`
	Seq          int64        `json:"seq"`
	From         store.SignIn `json:"from"`
	To           store.SignIn `json:"to"`
	PublicGrants []string     `json:"public_grants"`
}

// eventData is what the flat event of the request holds: the settings without their entries (rule 58).
func (p signInParams) eventData() any {
	return map[string]any{"seq": p.Seq, "from": SignInSummaryOf(p.From), "to": SignInSummaryOf(p.To), "public_grants": p.PublicGrants}
}

// SignInSummary is a sign-in setting without its entries, as the views that must not hold one show it (rules 22,
// 57 and 58).
type SignInSummary struct {
	Provider string `json:"provider"`
	Issuer   string `json:"issuer,omitempty"`
	ClientID string `json:"client_id,omitempty"`
	Count    int    `json:"count"`
}

// SignInSummaryOf counts the entries of a setting.
func SignInSummaryOf(in store.SignIn) SignInSummary {
	return SignInSummary{Provider: in.Provider, Issuer: in.Issuer, ClientID: in.ClientID, Count: len(in.Allow)}
}

// RequestSignIn requests a sign-in setting for a flat (rules 13 to 16). A request for the stored setting is done and
// makes no approval. Every other one makes a pending set_signin approval and changes nothing. MCP cannot request a
// change; the transports decide which other caller may.
func (s *Service) RequestSignIn(ctx context.Context, slugName string, req SignInRequest, via Via) (ActionResult, error) {
	switch via {
	case ViaCLI, ViaAPI, ViaConsole:
	default:
		return ActionResult{}, forbiddenf("sign-in is changed with `flats signin set` or `flats signin off` on the Flats host, and each change waits for the operator in the console")
	}
	if len(req.Reason) > maxSignInReason {
		return ActionResult{}, invalidf("member reason has %d bytes, at most %d (rule 14)", len(req.Reason), maxSignInReason)
	}
	to, err := normalizeSignIn(req)
	if err != nil {
		return ActionResult{}, err
	}
	// The state read here and the recorded request belong to one flat: a delete, a rename or a grant cannot finish
	// in between.
	unlock, err := s.lockCtx(ctx, slugName)
	if err != nil {
		return ActionResult{}, err
	}
	defer unlock()
	st, err := s.st.GetSignIn(ctx, slugName)
	if err != nil {
		return ActionResult{}, err
	}
	if st.SignIn.Same(to) {
		return ActionResult{Status: "done", Message: "sign-in unchanged"}, nil
	}
	if others := excludedGrants(st.PublicGrants); to.On() && len(others) > 0 {
		return ActionResult{}, signInRefusal(slugName, &store.SignInConflict{Reason: store.SignInOnRefused, Providers: others,
			Public: st.Visibility.Public(), PublicInstance: st.PublicInstance})
	}
	return s.requestFrozen(ctx, slugName, actionSetSignIn, signInParams{FlatID: st.FlatID, Seq: st.Seq, From: st.SignIn, To: to, PublicGrants: st.PublicGrants}, via, req.Reason)
}

func excludedGrants(grants []string) []string {
	var out []string
	for _, g := range grants {
		if store.SignInExcludes(g) {
			out = append(out, g)
		}
	}
	return out
}

// signInRefusal is the message of a refusal or a stale approval that sign-in causes (rules 8, 18 and 73). Each names
// the next action.
func signInRefusal(slugName string, c *store.SignInConflict) error {
	names := strings.Join(c.Providers, ", ")
	switch c.Reason {
	case store.SignInGrantRefused:
		return withKind(ErrConflict, fmt.Errorf("flat %s has Portal sign-in on, and visitors on %s do not sign in, so it cannot be granted. "+
			"Sign-in goes off only with `flats signin off %s` and an approval in the console", slugName, names, slugName))
	case store.SignInPublicRefused:
		return withKind(ErrConflict, fmt.Errorf("flat %s has Portal sign-in on and is granted %s, whose visitors do not sign in, so it cannot be made Public. "+
			"Revoke %s under Networks on the Settings tab of the flat, which works while the flat is Private, then request Public again", slugName, names, names))
	case store.SignInOnRefused:
		switch {
		case c.Public && c.PublicInstance:
			return withKind(ErrConflict, fmt.Errorf("public instance mode made flat %s Public on %s, and visitors on %s do not sign in, so sign-in cannot go on. "+
				"Make the flat Private, revoke %s under Networks on the Settings tab of the flat, and turn sign-in on before the flat is Public again", slugName, names, names, names))
		case c.Public:
			return withKind(ErrConflict, fmt.Errorf("flat %s is granted %s, whose visitors do not sign in, so sign-in cannot go on. "+
				"Make the flat Private first, then revoke %s under Networks on the Settings tab of the flat, because the host refuses a revocation while its route is registered, then request sign-in again",
				slugName, names, names))
		default:
			return withKind(ErrConflict, fmt.Errorf("flat %s is granted %s, whose visitors do not sign in, so sign-in cannot go on. "+
				"Revoke %s under Networks on the Settings tab of the flat, then request sign-in again", slugName, names, names))
		}
	case store.SignInOtherFlat:
		return fmt.Errorf("%w: %w: flat %s was deleted and created again after the request; request the sign-in change again", ErrConflict, ErrStaleApproval, slugName)
	case store.SignInSeqChanged:
		return fmt.Errorf("%w: %w: the sign-in of %s changed after the request (now at sequence %d); request the sign-in change again", ErrConflict, ErrStaleApproval, slugName, c.Seq)
	default:
		return fmt.Errorf("%w: %w: the public providers granted to %s changed after the request (now %s); request the sign-in change again", ErrConflict, ErrStaleApproval, slugName, grantsText(c.Grants))
	}
}

func grantsText(grants []string) string {
	if len(grants) == 0 {
		return "none"
	}
	return strings.Join(grants, ", ")
}

// signInConflict returns the message of err when it is a *store.SignInConflict, and err otherwise.
func signInConflict(slugName string, err error) error {
	var c *store.SignInConflict
	if errors.As(err, &c) {
		return signInRefusal(slugName, c)
	}
	return err
}

// refuseSignInPublic is the check of rule 8, second case, for a change of a flat to Public. The store checks it again
// in the transaction of the change.
func (s *Service) refuseSignInPublic(ctx context.Context, f store.Flat) error {
	if f.SignInProvider == "" {
		return nil
	}
	grants, err := s.st.PublicGrants(ctx, f.Slug)
	if err != nil {
		return err
	}
	if others := excludedGrants(grants); len(others) > 0 {
		return signInRefusal(f.Slug, &store.SignInConflict{Reason: store.SignInPublicRefused, Providers: others})
	}
	return nil
}

// signInStale checks the sign-in sequence that an approval of a visibility, an activation, a rollback or a version
// froze (rule 23). An approval stored before sign-in froze none.
func signInStale(slugName string, frozen *int64, f store.Flat) error {
	if frozen == nil || *frozen == f.SignInSeq {
		return nil
	}
	return fmt.Errorf("%w: %w: the sign-in of %s changed after the request (sequence %d, now %d), so its notices may be wrong; request the change again",
		ErrConflict, ErrStaleApproval, slugName, *frozen, f.SignInSeq)
}

// applySignInApproval applies an approved set_signin by rule 20.
func (s *Service) applySignInApproval(ctx context.Context, a store.Approval) (ActionResult, error) {
	var p signInParams
	if err := json.Unmarshal(a.Params, &p); err != nil {
		return ActionResult{}, err
	}
	unlock := s.lock(a.Flat)
	defer unlock()
	st, err := s.st.GetSignIn(ctx, a.Flat)
	if err != nil {
		return ActionResult{}, err
	}
	switch {
	case st.FlatID != p.FlatID:
		return ActionResult{}, signInRefusal(a.Flat, &store.SignInConflict{Reason: store.SignInOtherFlat})
	case st.Seq != p.Seq:
		return ActionResult{}, signInRefusal(a.Flat, &store.SignInConflict{Reason: store.SignInSeqChanged, Seq: st.Seq})
	case !slices.Equal(st.PublicGrants, p.PublicGrants):
		return ActionResult{}, signInRefusal(a.Flat, &store.SignInConflict{Reason: store.SignInGrantsChanged, Grants: st.PublicGrants})
	}
	if others := excludedGrants(st.PublicGrants); p.To.On() && len(others) > 0 {
		return ActionResult{}, fmt.Errorf("%w: %w", ErrStaleApproval, signInRefusal(a.Flat, &store.SignInConflict{Reason: store.SignInOnRefused, Providers: others,
			Public: st.Visibility.Public(), PublicInstance: st.PublicInstance}))
	}
	// Step 1. A client secret with another binding, or one that does not open, is not a reason to refuse the change:
	// the gate does not send it (rule 27), and the Portal route stops at step 4 until the operator sets it again. A
	// stored value passed rule 26, so the SDK checks nothing else of it.
	if p.To.On() {
		if err := sdkValidate(p.To); err != nil {
			return ActionResult{}, invalidf("the sign-in gate refuses the approved setting: %v; nothing changed", err)
		}
	}
	// Step 2.
	raw, _ := json.Marshal(ApprovalExecution{Status: "applied", DataImpact: "none", HealthData: "not_run", LiveData: "untouched"})
	from, err := s.st.CommitSignIn(ctx, store.SignInCommit{Flat: a.Flat, FlatID: p.FlatID, Seq: p.Seq, Grants: p.PublicGrants, To: p.To,
		Approval: a.ID, Receipt: raw, Audit: s.audit(ctx, ViaConsole)})
	if err != nil {
		return ActionResult{}, signInConflict(a.Flat, err)
	}
	lifecyclePhase(ctx, "after_signin_commit", a.Flat, a.ID)
	// Steps 3 to 5.
	msg := s.finishSignIn(ctx, a.Flat, from, p.To, p.Seq+1)
	s.notifySignIn(ctx, a, st.FlatID, from, p.To)
	return ActionResult{Status: "done", Message: msg}, nil
}

// finishSignIn does steps 3 to 5 of rule 20, without the webhook event, for a setting that is stored with sequence
// seq, and returns the result of the approval (rules 21 and 22). It runs under the lock of the flat.
func (s *Service) finishSignIn(ctx context.Context, slugName string, from, to store.SignIn, seq int64) string {
	msg := signInResult(to)
	if err := s.signInApplied(ctx, slugName, to, seq); err != nil {
		msg += fmt.Sprintf("; the setting is stored, and the Portal route of %s is stopped: %v", slugName, err)
	}
	s.Event(ctx, slugName, "info", "signin", msg, map[string]any{"from": SignInSummaryOf(from), "to": SignInSummaryOf(to)})
	return msg
}

// resumeSignIn finishes an approval whose commit of step 2 survived a stop of the host (rule 21): steps 3 to 5 from
// the stored setting, without the webhook event, because the host does not hold the sender yet.
func (s *Service) resumeSignIn(ctx context.Context, a store.Approval) string {
	var p signInParams
	_ = json.Unmarshal(a.Params, &p)
	unlock := s.lock(a.Flat)
	defer unlock()
	st, err := s.st.GetSignIn(ctx, a.Flat)
	if err != nil {
		s.logf("resume sign-in approval %s: %v", a.ID, err)
		return signInResult(p.To)
	}
	return s.finishSignIn(ctx, a.Flat, p.From, st.SignIn, st.Seq)
}

// signInResult is the result of an applied setting (rule 22): the provider, the issuer and the number of entries,
// never an entry.
func signInResult(in store.SignIn) string {
	entries := fmt.Sprintf("%d entries", len(in.Allow))
	if len(in.Allow) == 1 {
		entries = "1 entry"
	}
	switch in.Provider {
	case "":
		return "sign-in turned off"
	case store.SignInOIDC:
		return fmt.Sprintf("sign-in set to oidc with issuer %s and %s", in.Issuer, entries)
	default:
		return fmt.Sprintf("sign-in set to %s with %s", in.Provider, entries)
	}
}

// notifySignIn sends signin.changed for a change that approval a applied (rule 68). The body names no entry.
func (s *Service) notifySignIn(ctx context.Context, a store.Approval, flatID string, from, to store.SignIn) {
	if !s.notifying() {
		return
	}
	s.notify(webhook.Payload{
		Type: eventSignInChange, Flat: webhook.FlatRef{Slug: a.Flat, ID: flatID}, Operation: actionSetSignIn, State: "applied",
		ApprovalID: a.ID, DecidedBy: decidedWord(a.DecidedBy), ConsoleURL: s.consoleBase(ctx) + "/flats/" + a.Flat,
		Change: &webhook.Change{
			SignInFrom: &webhook.SignIn{Provider: from.Provider, Count: len(from.Allow)},
			SignInTo:   &webhook.SignIn{Provider: to.Provider, Count: len(to.Allow)},
		},
	})
}

// signInHooks replace the steps of sign-in that act on the routes, for a test. Their zero value runs the default.
type signInHooks struct {
	applied       func(ctx context.Context, slugName string, in store.SignIn, seq int64) error
	secretChanged func(ctx context.Context, slugName string) error
}

// signInApplied does steps 3 and 4 of rule 20 for the stored setting in of sequence seq: it ends the open connections
// of the public routes of the flat, as a change to Private ends them, and, when the flat has a Portal route, puts in
// place of the old gate one that is built from the setting (rules 33 and 35). It runs under the lock of the flat,
// after the commit of step 2, also when resumeApplying finishes an approval at a start. An error says that the Portal
// route of the flat is stopped, and why; the setting stays stored.
//
// The open of step 4 reads the stored setting again, which is in and seq under the lock of the flat.
func (s *Service) signInApplied(ctx context.Context, slugName string, in store.SignIn, seq int64) error {
	if h := s.signInHooks.applied; h != nil {
		return h(ctx, slugName, in, seq)
	}
	// Step 3: a visitor who holds a WebSocket or a long request comes back through the new gate.
	s.revokeRoute("public:" + slugName)
	// Step 4: a new public handler binds the new epoch, so the cancelled one answers only the connections that were open.
	return s.reopenPublic(ctx, slugName, false)
}

// signInSecretChanged builds the gate of a flat again after a set or a removal of its client secret (rule 28), when
// its sign-in is oidc and it has a Portal route. It keeps the open connections. It runs under the lock of the flat.
// An error says that the Portal route is stopped, and why.
func (s *Service) signInSecretChanged(ctx context.Context, slugName string) error {
	if h := s.signInHooks.secretChanged; h != nil {
		return h(ctx, slugName)
	}
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil || f.SignInProvider != store.SignInOIDC {
		return err
	}
	// The epoch stays, so the new public handler binds the one that the open connections use.
	return s.reopenPublic(ctx, slugName, true)
}

// SignInClientSecret opens the client secret that a gate of the oidc setting in sends to its issuer (rule 27). It
// returns false without an error when in is not oidc or no value is stored: the gate is then a public client with
// PKCE. A stored value with another binding is ErrSignInSecretBinding, whose message says to set the secret again for
// the issuer of the setting. No message holds the value.
func (s *Service) SignInClientSecret(ctx context.Context, slugName string, in store.SignIn) (string, bool, error) {
	if in.Provider != store.SignInOIDC {
		return "", false, nil
	}
	sec, ok, err := s.st.SignInSecret(ctx, slugName)
	if err != nil || !ok {
		return "", false, err
	}
	if sec.SignIn.Issuer != in.Issuer || sec.SignIn.ClientID != in.ClientID {
		return "", false, fmt.Errorf("%w: it is bound to issuer %s and client ID %s, and the sign-in has issuer %s and client ID %s. "+
			"Set it again for the issuer of the setting with `flats signin secret set %s --issuer %s --client-id %s`",
			ErrSignInSecretBinding, sec.SignIn.Issuer, sec.SignIn.ClientID, in.Issuer, in.ClientID, slugName, in.Issuer, in.ClientID)
	}
	g, err := s.aead()
	if err != nil {
		return "", false, err
	}
	// Open panics on a nonce of the wrong size; a damaged row opens nothing.
	if len(sec.Nonce) != g.NonceSize() {
		return "", false, fmt.Errorf("open the sign-in client secret of %s: the stored nonce is damaged; set it again with `flats signin secret set`", slugName)
	}
	pt, err := g.Open(nil, sec.Nonce, sec.Ciphertext, []byte(store.SignInSecretName))
	if err != nil {
		return "", false, fmt.Errorf("open the sign-in client secret of %s: %w%s", slugName, err, sealedByAnotherKey(sec.KeyID, s.secretKeyID))
	}
	return string(pt), true, nil
}

// SignInGate is what a gate of rule 33 is built from: the provider, the allowlist (EmailList of appauth.Config for
// oidc, Allow for credential), and for oidc the issuer, the client ID and the client secret of rule 27. The Portal
// route adds PublicHosts, IdentityHeaders false and no IdentityProof. It holds the client secret, so its String hides
// it, and it must never be logged.
type SignInGate struct {
	Provider     string
	Allow        []string
	Issuer       string
	ClientID     string
	ClientSecret string
}

// String writes the gate without its client secret.
func (g SignInGate) String() string {
	secret := "none"
	if g.ClientSecret != "" {
		secret = "set"
	}
	return fmt.Sprintf("sign-in gate %s (issuer %q, client ID %q, %d entries, client secret %s)", g.Provider, g.Issuer, g.ClientID, len(g.Allow), secret)
}

// GoString keeps %#v from writing the client secret.
func (g SignInGate) GoString() string { return g.String() }

// SignInGate returns the gate for the stored setting in of a flat, checked as appauth.Validate checks it. It is the
// zero SignInGate while sign-in is off. A client secret with another binding is ErrSignInSecretBinding (rule 27), and
// one that does not open is an error too: the Portal route then builds no gate (rule 35).
func (s *Service) SignInGate(ctx context.Context, slugName string, in store.SignIn) (SignInGate, error) {
	if !in.On() {
		return SignInGate{}, nil
	}
	if err := sdkValidate(in); err != nil {
		return SignInGate{}, fmt.Errorf("the stored sign-in of %s is not one that the gate accepts: %w", slugName, err)
	}
	g := SignInGate{Provider: in.Provider, Allow: slices.Clone(in.Allow), Issuer: in.Issuer, ClientID: in.ClientID}
	secret, ok, err := s.SignInClientSecret(ctx, slugName, in)
	if err != nil {
		return SignInGate{}, err
	}
	if ok {
		g.ClientSecret = secret
	}
	return g, nil
}

// checkSignInSecret checks a client secret by rule 26: the SDK trims a client secret, and the bytes that it sends must
// be the bytes that the host stores. No message holds the value.
func checkSignInSecret(value string) error {
	switch {
	case value == "":
		return invalidf("the client secret is empty; nothing changed")
	case len(value) > maxSignInSecret:
		return invalidf("the client secret has more than %d bytes; nothing changed", maxSignInSecret)
	case strings.ContainsFunc(value, func(r rune) bool { return !visibleASCII(r) }):
		return invalidf("the client secret has a space, a control character or a character that is not ASCII; nothing changed")
	}
	return nil
}

// SetSignInSecret stores the client secret of the oidc sign-in of a flat, bound to issuer and clientID (rules 24 to
// 29). Only the operator sets it, with `flats signin secret set` on the host; it needs no approval. A readable secret
// with its name refuses the set. No message holds the value.
func (s *Service) SetSignInSecret(ctx context.Context, slugName, value, issuer, clientID string, via Via) (ActionResult, error) {
	if via != ViaCLI {
		return ActionResult{}, forbiddenf("the sign-in client secret is set by the operator with `flats signin secret set` on the Flats host")
	}
	iss, err := signInIssuer(issuer)
	if err != nil {
		return ActionResult{}, err
	}
	cid, err := signInClientID(clientID)
	if err != nil {
		return ActionResult{}, err
	}
	if err := checkSignInSecret(value); err != nil {
		return ActionResult{}, err
	}
	unlock, err := s.lockCtx(ctx, slugName)
	if err != nil {
		return ActionResult{}, err
	}
	defer unlock()
	if _, err := s.st.GetFlat(ctx, slugName); err != nil {
		return ActionResult{}, err
	}
	g, err := s.aead()
	if err != nil {
		return ActionResult{}, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return ActionResult{}, err
	}
	sec := store.SealedSecret{Nonce: nonce, Ciphertext: g.Seal(nil, nonce, []byte(value), []byte(store.SignInSecretName)), KeyID: s.secretKeyID, UpdatedAt: s.now()}
	if _, err := s.st.PutSignInSecret(ctx, slugName, sec, store.SignInBinding{Issuer: iss, ClientID: cid}, s.audit(ctx, via)); err != nil {
		if errors.Is(err, store.ErrSignInSecretName) {
			return ActionResult{}, withKind(ErrConflict, fmt.Errorf("flat %s has a readable secret named %s from before the name check of `flats secret set`, so the sign-in client secret cannot take its name. "+
				"Remove it with `flats secret rm %s %s`, then set the client secret again", slugName, store.SignInSecretName, slugName, store.SignInSecretName))
		}
		return ActionResult{}, err
	}
	msg := fmt.Sprintf("sign-in client secret set via %s for issuer %s and client ID %s", via, iss, cid)
	s.Event(ctx, slugName, "info", "secret", msg, nil)
	return ActionResult{Status: "done", Message: s.afterSignInSecret(ctx, slugName, msg)}, nil
}

// DeleteSignInSecret removes the client secret of the sign-in of a flat (rule 25). Only the operator removes it, with
// `flats signin secret rm` on the host.
func (s *Service) DeleteSignInSecret(ctx context.Context, slugName string, via Via) (ActionResult, error) {
	if via != ViaCLI {
		return ActionResult{}, forbiddenf("the sign-in client secret is removed by the operator with `flats signin secret rm` on the Flats host")
	}
	unlock, err := s.lockCtx(ctx, slugName)
	if err != nil {
		return ActionResult{}, err
	}
	defer unlock()
	if _, err := s.st.GetFlat(ctx, slugName); err != nil {
		return ActionResult{}, err
	}
	if err := s.st.DeleteSignInSecret(ctx, slugName, s.audit(ctx, via)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ActionResult{}, fmt.Errorf("flat %s has no sign-in client secret: %w", slugName, store.ErrNotFound)
		}
		return ActionResult{}, err
	}
	msg := fmt.Sprintf("sign-in client secret removed via %s", via)
	s.Event(ctx, slugName, "info", "secret", msg, nil)
	return ActionResult{Status: "done", Message: s.afterSignInSecret(ctx, slugName, msg)}, nil
}

// afterSignInSecret runs the rebuild of rule 28 and adds its failure to msg.
func (s *Service) afterSignInSecret(ctx context.Context, slugName, msg string) string {
	if err := s.signInSecretChanged(ctx, slugName); err != nil {
		s.Event(ctx, slugName, "error", "signin", fmt.Sprintf("the Portal route of %s is stopped: %v", slugName, err), nil)
		return msg + fmt.Sprintf("; the Portal route of %s is stopped: %v", slugName, err)
	}
	return msg
}

// SignInView is the stored sign-in of a flat with the binding of its client secret and its sequence: the parts of
// rules 56, 57 and 59 that flats.db holds. It holds the entries of the allowlist, so only the views that rule 58
// allows show it whole.
type SignInView struct {
	Provider     string            `json:"provider"`
	Issuer       string            `json:"issuer"`
	ClientID     string            `json:"client_id"`
	Allow        []string          `json:"allow"`
	ClientSecret *SignInSecretView `json:"client_secret"`
	// Hosts are the host names of the PublicHosts of the Portal route (rule 34), empty without a route, and
	// CallbackURLs the callback URL of each for oidc, the URLs to register at the provider.
	Hosts        []string `json:"hosts"`
	CallbackURLs []string `json:"callback_urls"`
	Seq          int64    `json:"seq"`
}

// SignInSecretView says that a client secret is stored, for which issuer and client ID, and since when. It never
// holds the value.
type SignInSecretView struct {
	Name      string    `json:"name"`
	Issuer    string    `json:"issuer"`
	ClientID  string    `json:"client_id"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GetSignIn reads the sign-in of a flat.
func (s *Service) GetSignIn(ctx context.Context, slugName string) (SignInView, error) {
	st, err := s.st.GetSignIn(ctx, slugName)
	if err != nil {
		return SignInView{}, err
	}
	v := SignInView{Provider: st.SignIn.Provider, Issuer: st.SignIn.Issuer, ClientID: st.SignIn.ClientID, Allow: st.SignIn.Allow, Seq: st.Seq}
	if st.Secret != nil {
		v.ClientSecret = &SignInSecretView{Name: store.SignInSecretName, Issuer: st.Secret.Issuer, ClientID: st.Secret.ClientID, UpdatedAt: st.Secret.UpdatedAt}
	}
	v.Hosts, v.CallbackURLs = s.SignInHosts(slugName), []string{}
	if v.Provider == store.SignInOIDC {
		v.CallbackURLs = SignInCallbackURLs(v.Hosts)
	}
	return v, nil
}
