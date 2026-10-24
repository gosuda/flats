package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/store"
)

// AutopilotOp names an operation agents may be allowed to decide. Its
// position in AutopilotOps is its stored bit, which is never reused.
type AutopilotOp string

const (
	OpCreateVersion       AutopilotOp = "create_version"
	OpDeleteVersion       AutopilotOp = "delete_version"
	OpDeploy              AutopilotOp = "deploy"
	OpRollback            AutopilotOp = "rollback"
	OpRollbackRestoreData AutopilotOp = "rollback_restore_data"
	OpMakePublic          AutopilotOp = "make_public"
	OpMakePrivate         AutopilotOp = "make_private"
	OpPublicEditing       AutopilotOp = "public_editing"
	OpDeleteFlat          AutopilotOp = "delete_flat"
)

// AutopilotOps lists the operations in bit order: bit i is
// AutopilotOps[i]. The console shows them in its own display order.
var AutopilotOps = []AutopilotOp{
	OpCreateVersion, OpDeleteVersion, OpDeploy, OpRollback, OpRollbackRestoreData,
	OpMakePublic, OpMakePrivate, OpPublicEditing, OpDeleteFlat,
}

// AutopilotOpInfo describes an operation for people and agents.
type AutopilotOpInfo struct {
	ID       AutopilotOp
	Label    string // the console label
	Requests string // the requests the operation covers
}

// AutopilotOpInfos describes AutopilotOps, in the same order.
var AutopilotOpInfos = []AutopilotOpInfo{
	{OpCreateVersion, "Create versions", "create_version"},
	{OpDeleteVersion, "Delete versions", "delete_version"},
	{OpDeploy, "Deploy", "deploy of a saved version (activation or redeploy)"},
	{OpRollback, "Roll back", "rollback without restore_data"},
	{OpRollbackRestoreData, "Roll back with data restore", "rollback with restore_data"},
	{OpMakePublic, "Make Public", "set_visibility Private to Public"},
	{OpMakePrivate, "Make Private", "set_visibility Public to Private"},
	{OpPublicEditing, "Public editing", "a change of a Public flat's access level, view to edit or edit to view"},
	{OpDeleteFlat, "Delete flats", "delete_flat"},
}

// OpSet is a set of operations, one bit per entry of AutopilotOps.
type OpSet uint16

func opBit(op AutopilotOp) OpSet {
	for i, o := range AutopilotOps {
		if o == op {
			return 1 << i
		}
	}
	return 0
}

// Has reports whether s contains op.
func (s OpSet) Has(op AutopilotOp) bool {
	b := opBit(op)
	return b != 0 && s&b != 0
}

// List returns the operations of s in AutopilotOps order.
func (s OpSet) List() []AutopilotOp {
	out := []AutopilotOp{}
	for i, op := range AutopilotOps {
		if s&(1<<i) != 0 {
			out = append(out, op)
		}
	}
	return out
}

// Labels returns the console labels of s joined with ", ".
func (s OpSet) Labels() string {
	var out []string
	for i, info := range AutopilotOpInfos {
		if s&(1<<i) != 0 {
			out = append(out, info.Label)
		}
	}
	return strings.Join(out, ", ")
}

func (s OpSet) ids() string {
	var out []string
	for _, op := range s.List() {
		out = append(out, string(op))
	}
	return strings.Join(out, ", ")
}

// described names each operation of s by label and ID.
func (s OpSet) described() string {
	var out []string
	for i, info := range AutopilotOpInfos {
		if s&(1<<i) != 0 {
			out = append(out, fmt.Sprintf("%s (%s)", info.Label, info.ID))
		}
	}
	return strings.Join(out, ", ")
}

// ParseOpSet reads operation IDs. An unknown ID is ErrInvalid.
func ParseOpSet(ids []string) (OpSet, error) {
	var s OpSet
	for _, id := range ids {
		b := opBit(AutopilotOp(id))
		if b == 0 {
			return 0, invalidf("unknown operation %q", id)
		}
		s |= b
	}
	return s, nil
}

// AutopilotGuideMax is the longest operation guide, in Unicode code points.
const AutopilotGuideMax = 4000

// Autopilot is a copy of the Autopilot settings.
type Autopilot struct {
	Enabled   bool
	Allowed   OpSet
	Guide     string
	UpdatedAt time.Time // zero while never changed
}

// GuideState converts a copy for the agent text renderers: nil when Enabled
// is false.
func (a Autopilot) GuideState() *runtimeref.AutopilotState {
	if !a.Enabled {
		return nil
	}
	st := &runtimeref.AutopilotState{Allowed: []runtimeref.AutopilotOperation{}, Guide: a.Guide}
	for i, info := range AutopilotOpInfos {
		if a.Allowed&(1<<i) != 0 {
			st.Allowed = append(st.Allowed, runtimeref.AutopilotOperation{ID: string(info.ID), Label: info.Label, Requests: info.Requests})
		}
	}
	return st
}

func autopilotOf(r store.AutopilotRow) Autopilot {
	return Autopilot{Enabled: r.Enabled, Allowed: OpSet(r.Ops), Guide: r.Guide, UpdatedAt: r.UpdatedAt}
}

// autopilotStore is the part of the store Autopilot uses. Tests replace it
// (Service.apStore) to inject faults.
type autopilotStore interface {
	GetAutopilot(ctx context.Context) (store.AutopilotRow, error)
	UpdateAutopilot(ctx context.Context, now time.Time, change func(store.AutopilotRow) (store.AutopilotRow, string, error)) (store.AutopilotRow, bool, error)
	AutopilotSync(ctx context.Context) (store.AutopilotRow, []store.AutopilotChange, error)
	MarkAutopilotLogged(ctx context.Context, seq int64) error
	ClaimApprovalAutopilot(ctx context.Context, id, actor string, need uint16, now time.Time) error
	RejectApprovalAutopilot(ctx context.Context, id, actor, result string, need uint16, now time.Time) error
}

func (s *Service) autopilotStore() autopilotStore {
	if s.apStore != nil {
		return s.apStore
	}
	return s.st
}

// Autopilot returns a copy of the settings as last read from the database.
// When the last read failed it reads again first; if that fails too it
// reports Autopilot off, which hides the agent tools and authorizes
// nothing: agent decisions check the stored row themselves.
func (s *Service) Autopilot() Autopilot {
	if s.autopilotStale.Load() {
		a, err := s.syncAutopilot(context.Background())
		if err == nil {
			return a
		}
		now := s.now()
		if last := s.autopilotWarned.Load(); last == 0 || now.Sub(time.UnixMilli(last)) >= time.Minute {
			s.autopilotWarned.Store(now.UnixMilli())
			s.logf("autopilot: settings unavailable: %v", err)
		}
		a = s.cachedAutopilot()
		a.Enabled = false
		return a
	}
	return s.cachedAutopilot()
}

// StoredAutopilot reads the settings from the database, bypassing the
// cache. The console uses it so that a store it cannot read is an error
// there rather than Autopilot shown off.
func (s *Service) StoredAutopilot(ctx context.Context) (Autopilot, error) {
	row, err := s.autopilotStore().GetAutopilot(ctx)
	if err != nil {
		return Autopilot{}, fmt.Errorf("read the Autopilot settings: %w", err)
	}
	return autopilotOf(row), nil
}

func (s *Service) cachedAutopilot() Autopilot {
	if a := s.autopilot.Load(); a != nil {
		return *a
	}
	return Autopilot{}
}

// syncAutopilot reads the stored settings and the change lines not logged
// yet, writes those lines to the host log and replaces the cache. A failed
// read marks the cache stale and leaves it as it was. Lines whose marking
// fails are logged again at the next sync: the log is at least once.
func (s *Service) syncAutopilot(ctx context.Context) (Autopilot, error) {
	s.autopilotMu.Lock()
	defer s.autopilotMu.Unlock()
	row, changes, err := s.autopilotStore().AutopilotSync(ctx)
	if err != nil {
		s.autopilotStale.Store(true)
		return Autopilot{}, err
	}
	for _, c := range changes {
		s.logf("%s (at %s)", c.Line, c.At.Format(time.RFC3339))
	}
	if len(changes) > 0 {
		if err := s.autopilotStore().MarkAutopilotLogged(ctx, changes[len(changes)-1].Seq); err != nil {
			s.logf("autopilot: record logged changes: %v", err)
		}
	}
	a := autopilotOf(row)
	s.autopilot.Store(&a)
	s.autopilotStale.Store(false)
	return a, nil
}

// updateAutopilot runs change on the stored settings in one write
// transaction, then syncs the cache and the host log. A cancelled request
// cannot stop a started change. It returns the settings read by the sync,
// or the committed ones when only the sync failed, and the error of the
// update.
func (s *Service) updateAutopilot(ctx context.Context, change func(stored Autopilot) (Autopilot, string, error)) (Autopilot, error) {
	ctx = context.WithoutCancel(ctx)
	committed, _, uerr := s.autopilotStore().UpdateAutopilot(ctx, s.now(), func(row store.AutopilotRow) (store.AutopilotRow, string, error) {
		next, line, err := change(autopilotOf(row))
		if err != nil {
			return row, "", err
		}
		return store.AutopilotRow{Enabled: next.Enabled, Ops: uint16(next.Allowed), Guide: next.Guide}, line, nil
	})
	a, serr := s.syncAutopilot(ctx)
	if serr != nil {
		if uerr != nil {
			return s.cachedAutopilot(), uerr
		}
		return autopilotOf(committed), nil
	}
	return a, uerr
}

func changedBy(who string) string {
	if who == "" {
		return "the console"
	}
	return who
}

// SetAutopilotEnabled turns Autopilot on or off. Turning it on needs the
// operations the operator confirmed; when the stored operations differ, it
// is ErrConflict and nothing changes. who names the person for the host log.
func (s *Service) SetAutopilotEnabled(ctx context.Context, on bool, confirmed *OpSet, who string) (Autopilot, error) {
	if on && confirmed == nil {
		return s.Autopilot(), invalidf("turning Autopilot on needs the confirmed operations")
	}
	if !on && confirmed != nil {
		return s.Autopilot(), invalidf("turning Autopilot off takes no confirmed operations")
	}
	return s.updateAutopilot(ctx, func(stored Autopilot) (Autopilot, string, error) {
		next := stored
		next.Enabled = on
		if !on {
			return next, fmt.Sprintf("autopilot: turned off by %s", changedBy(who)), nil
		}
		if stored.Allowed != *confirmed {
			return stored, "", fmt.Errorf("%w: the allowed operations changed after the page loaded; review them and turn Autopilot on again", ErrConflict)
		}
		allowed := stored.Allowed.ids()
		if allowed == "" {
			allowed = "nothing"
		}
		return next, fmt.Sprintf("autopilot: turned on by %s; agents may decide: %s", changedBy(who), allowed), nil
	})
}

// SetAutopilotOp allows or disallows one operation.
func (s *Service) SetAutopilotOp(ctx context.Context, op AutopilotOp, allowed bool, who string) (Autopilot, error) {
	bit := opBit(op)
	if bit == 0 {
		return s.Autopilot(), invalidf("unknown operation %q", op)
	}
	return s.updateAutopilot(ctx, func(stored Autopilot) (Autopilot, string, error) {
		next := stored
		if allowed {
			next.Allowed |= bit
			return next, fmt.Sprintf("autopilot: agents may now decide %s; changed by %s", op, changedBy(who)), nil
		}
		next.Allowed &^= bit
		return next, fmt.Sprintf("autopilot: agents may no longer decide %s; changed by %s", op, changedBy(who)), nil
	})
}

// SetAutopilotGuide stores the operation guide as given. The host log
// records its length, never its text.
func (s *Service) SetAutopilotGuide(ctx context.Context, guide, who string) (Autopilot, error) {
	n := utf8.RuneCountInString(guide)
	if n > AutopilotGuideMax {
		return s.Autopilot(), invalidf("the operation guide has %d characters; at most %d are allowed", n, AutopilotGuideMax)
	}
	return s.updateAutopilot(ctx, func(stored Autopilot) (Autopilot, string, error) {
		next := stored
		next.Guide = guide
		return next, fmt.Sprintf("autopilot: operation guide changed by %s (%d of %d characters)", changedBy(who), n, AutopilotGuideMax), nil
	})
}

// UnclassifiedError reports an approval AutopilotNeeds cannot classify. No
// switch allows it.
type UnclassifiedError struct{ Reason string }

func (e *UnclassifiedError) Error() string {
	return "no operation switch allows this approval: " + e.Reason
}

func unclassified(reason string) error { return &UnclassifiedError{Reason: reason} }

// AutopilotNeeds returns the operations an approval needs. It reads only the
// action and the frozen params, and fails closed: an approval it cannot
// classify returns *UnclassifiedError.
func AutopilotNeeds(action string, params json.RawMessage) (OpSet, error) {
	switch action {
	case "create_version", "publish":
		return frozenNeeds(params, &publishParams{}, OpCreateVersion)
	case "delete_version":
		return frozenNeeds(params, &deleteVersionParams{}, OpDeleteVersion)
	case "activate":
		return frozenNeeds(params, &activateParams{}, OpDeploy)
	case "rollback":
		var p rollbackParams
		if err := readFrozen(params, &p); err != nil {
			return 0, err
		}
		if p.RestoreData {
			return opBit(OpRollbackRestoreData), nil
		}
		return opBit(OpRollback), nil
	case "restore_data":
		return frozenNeeds(params, &rollbackParams{}, OpRollbackRestoreData)
	case "delete":
		return frozenNeeds(params, &struct {
			Slug string `json:"slug"`
		}{}, OpDeleteFlat)
	case "set_visibility":
		return visibilityNeeds(params)
	}
	return 0, unclassified("unknown action")
}

func frozenNeeds(params json.RawMessage, v any, op AutopilotOp) (OpSet, error) {
	if err := readFrozen(params, v); err != nil {
		return 0, err
	}
	return opBit(op), nil
}

// readFrozen decodes frozen approval parameters into v, accepting only a
// JSON object without null members whose fields have the right types.
func readFrozen(params json.RawMessage, v any) error {
	const reason = "its frozen parameters cannot be read"
	var members map[string]json.RawMessage
	if err := json.Unmarshal(params, &members); err != nil || members == nil {
		return unclassified(reason)
	}
	for _, m := range members {
		if bytes.Equal(bytes.TrimSpace(m), []byte("null")) {
			return unclassified(reason)
		}
	}
	if err := json.Unmarshal(params, v); err != nil {
		return unclassified(reason)
	}
	return nil
}

// visibilityNeeds resolves levels as applyVisibilityApproval does, after
// checking the raw frozen visibilities.
func visibilityNeeds(params json.RawMessage) (OpSet, error) {
	var p visibilityParams
	if err := readFrozen(params, &p); err != nil {
		return 0, err
	}
	if p.From == "" {
		return 0, unclassified("no frozen starting visibility")
	}
	// Canonical maps unknown values to private, so validity is checked on
	// the raw values first.
	if !store.Visibility(p.From).Valid() || !store.Visibility(p.Visibility).Valid() {
		return 0, unclassified("unknown visibility")
	}
	from, to := store.Visibility(p.From).Canonical(), store.Visibility(p.Visibility).Canonical()
	level := ""
	if to == store.Public {
		level = store.AccessView
		if p.AccessSeq != nil {
			switch p.Access {
			case "", store.AccessView:
			case store.AccessEdit:
				level = store.AccessEdit
			default:
				return 0, unclassified("unknown public access level")
			}
		}
	}
	fromLevel := ""
	if from == store.Public {
		switch p.FromAccess {
		case "", store.AccessView, store.AccessEdit:
			fromLevel = p.FromAccess
		default:
			return 0, unclassified("unknown starting public access level")
		}
	}
	switch {
	case from == store.Private && to == store.Public:
		need := opBit(OpMakePublic)
		if level == store.AccessEdit {
			need |= opBit(OpPublicEditing)
		}
		return need, nil
	case from == store.Public && to == store.Private:
		return opBit(OpMakePrivate), nil
	case from == store.Public && to == store.Public:
		if fromLevel == "" {
			return 0, unclassified("no frozen starting access level")
		}
		if fromLevel == level {
			return 0, unclassified("requests no change")
		}
		return opBit(OpPublicEditing), nil
	}
	return 0, unclassified("requests no change")
}

// Arrival is the network an agent's request arrived on.
type Arrival struct {
	Network string // "local", "listener", "tailscale", "netbird" or "external"
	IP      string // listener only: the caller's IP address
	Login   string // tailscale only: the tailnet login of the caller
	Node    string // tailscale only, when Login is empty: the caller's node name
	Peer    string // netbird only: the calling peer's name (FQDN)
	Origin  string // external only: the operator-declared origin
}

// AgentDecision is who decides an approval through the Autopilot tools.
type AgentDecision struct {
	Agent   string // the name the agent gives for itself; not verified
	Arrival Arrival
}

var (
	ErrAutopilotOff        = errors.New("autopilot is off")
	ErrAutopilotNotAllowed = errors.New("autopilot does not allow this operation")
)

// AutopilotErrorCategories lists the ErrorCategory values that exist only
// while Autopilot is on, so their refusal pages are not in ErrorCategories.
var AutopilotErrorCategories = []string{"autopilot_off", "autopilot_not_allowed"}

// agentNameMax is the longest agent name, in Unicode code points.
const agentNameMax = 100

// DecideAsAgent approves or rejects a pending approval for an agent under
// Autopilot. It follows Decide's locking and status rules; the claim or
// rejection also requires, in the same statement, that Autopilot is on and
// allows every operation the approval needs.
func (s *Service) DecideAsAgent(ctx context.Context, id string, approve bool, d AgentDecision) (store.Approval, error) {
	if strings.TrimSpace(d.Agent) == "" || utf8.RuneCountInString(d.Agent) > agentNameMax {
		return store.Approval{}, invalidf("agent must be a name of 1 to %d characters", agentNameMax)
	}
	return s.decide(ctx, id, approve, &d)
}

// autopilotRefusal names why row does not allow an approval needing need.
// A nil result means the row allows it.
func autopilotRefusal(row store.AutopilotRow, need OpSet, classifyErr error) error {
	if !row.Enabled {
		return ErrAutopilotOff
	}
	if classifyErr != nil {
		return fmt.Errorf("%w: %w; the operator decides it in the console", ErrAutopilotNotAllowed, classifyErr)
	}
	if missing := need &^ OpSet(row.Ops); missing != 0 {
		return fmt.Errorf("%w: the operator does not allow agents to decide %s; the operator decides it in the console", ErrAutopilotNotAllowed, missing.described())
	}
	return nil
}

// autopilotDenied explains a claim or rejection the store refused because
// of the Autopilot row, which it reads again for the message.
func (s *Service) autopilotDenied(ctx context.Context, need OpSet) error {
	row, err := s.autopilotStore().GetAutopilot(ctx)
	if err != nil {
		return err
	}
	if err := autopilotRefusal(row, need, nil); err != nil {
		return err
	}
	return fmt.Errorf("%w: Autopilot changed during the call; nothing changed; call again", ErrAutopilotNotAllowed)
}

const autopilotPrefix = "autopilot:"

// agentRecord is the stored form of an AgentDecision.
type agentRecord struct {
	Agent   string `json:"agent"`
	Network string `json:"network"`
	IP      string `json:"ip,omitempty"`
	Login   string `json:"login,omitempty"`
	Node    string `json:"node,omitempty"`
	Peer    string `json:"peer,omitempty"`
	Origin  string `json:"origin,omitempty"`
}

// EncodeAgentDecider returns the decided_by value of an agent decision.
func EncodeAgentDecider(d AgentDecision) string {
	raw, _ := json.Marshal(agentRecord{Agent: d.Agent, Network: d.Arrival.Network, IP: d.Arrival.IP,
		Login: d.Arrival.Login, Node: d.Arrival.Node, Peer: d.Arrival.Peer, Origin: d.Arrival.Origin})
	return autopilotPrefix + string(raw)
}

// ParseAgentDecider reads a decided_by value written by EncodeAgentDecider.
// Any other value, including operator values, reports false.
func ParseAgentDecider(decidedBy string) (AgentDecision, bool) {
	rest, ok := strings.CutPrefix(decidedBy, autopilotPrefix)
	if !ok {
		return AgentDecision{}, false
	}
	var r agentRecord
	if err := json.Unmarshal([]byte(rest), &r); err != nil || strings.TrimSpace(r.Agent) == "" {
		return AgentDecision{}, false
	}
	switch r.Network {
	case "local", "listener", "tailscale", "netbird", "external":
	default:
		return AgentDecision{}, false
	}
	return AgentDecision{Agent: r.Agent, Arrival: Arrival{Network: r.Network, IP: r.IP, Login: r.Login, Node: r.Node, Peer: r.Peer, Origin: r.Origin}}, true
}

// DeciderText names who decided an approval in event texts: "the operator"
// for every operator value, or the Autopilot record of an agent decision.
func DeciderText(decidedBy string) string {
	d, ok := ParseAgentDecider(decidedBy)
	if !ok {
		return "the operator"
	}
	a := d.Arrival
	via := ""
	switch a.Network {
	case "local":
		via = "Local"
	case "listener":
		via = "the console listener"
		if a.IP != "" {
			via += " from " + a.IP
		}
	case "tailscale":
		switch {
		case a.Login != "":
			via = "Tailscale, " + a.Login
		case a.Node != "":
			via = "Tailscale node " + a.Node
		default:
			via = "Tailscale"
		}
	case "netbird":
		via = "NetBird"
		if a.Peer != "" {
			via = "NetBird peer " + a.Peer
		}
	case "external":
		via = "an external origin"
		if a.Origin != "" {
			via = "external origin " + a.Origin
		}
	}
	return fmt.Sprintf("Autopilot (agent %q via %s)", d.Agent, via)
}

// ViaAutopilot names the surface of operations applied after an agent
// decision.
const ViaAutopilot Via = "autopilot"

// applyVia is the surface an approved operation runs as: autopilot after
// an agent decision, else the console. It reads the stored decided_by, so a
// decision resumed after a restart keeps it.
func applyVia(a store.Approval) Via {
	if _, ok := ParseAgentDecider(a.DecidedBy); ok {
		return ViaAutopilot
	}
	return ViaConsole
}

type consoleDomainCtxKey struct{}

type consoleDomainRef struct{ prov, name string }

// WithConsoleDomain records that a request came through the console domain
// name of network prov ("tailscale" or "netbird").
func WithConsoleDomain(ctx context.Context, prov, name string) context.Context {
	return context.WithValue(ctx, consoleDomainCtxKey{}, consoleDomainRef{prov, name})
}

// ConsoleDomainOf reports the console domain recorded by WithConsoleDomain.
func ConsoleDomainOf(ctx context.Context) (prov, name string, ok bool) {
	r, ok := ctx.Value(consoleDomainCtxKey{}).(consoleDomainRef)
	return r.prov, r.name, ok
}

type tailnetPeerCtxKey struct{}

type tailnetPeer struct{ login, node string }

// WithTailnetPeer records the calling tailnet device: its user's login
// (empty for a tagged device) and its node name.
func WithTailnetPeer(ctx context.Context, login, node string) context.Context {
	return context.WithValue(ctx, tailnetPeerCtxKey{}, tailnetPeer{login, node})
}

// TailnetPeerOf returns what WithTailnetPeer recorded, or empty strings.
func TailnetPeerOf(ctx context.Context) (login, node string) {
	p, _ := ctx.Value(tailnetPeerCtxKey{}).(tailnetPeer)
	return p.login, p.node
}

type netbirdPeerCtxKey struct{}

// WithNetbirdPeer records the calling NetBird peer's name (FQDN).
func WithNetbirdPeer(ctx context.Context, fqdn string) context.Context {
	return context.WithValue(ctx, netbirdPeerCtxKey{}, fqdn)
}

// NetbirdPeerOf returns what WithNetbirdPeer recorded, or "".
func NetbirdPeerOf(ctx context.Context) string {
	s, _ := ctx.Value(netbirdPeerCtxKey{}).(string)
	return s
}
