package core

import (
	"context"
	"strings"

	"github.com/gosuda/flats/internal/store"
	"github.com/gosuda/flats/internal/webhook"
)

// The events of docs/spec/event-webhook.md. The service makes an event after the change that it reports has
// committed, in the goroutine of the operation, and hands it to the Notifier without waiting (rules 1 to 5).

// Notifier receives the events. webhook.Sender is the implementation; the host sets it once.
type Notifier interface {
	Notify(webhook.Payload)
}

// notifierBox lets the service swap the notifier without a lock: the host sets it after New returns, while requests may
// already run in tests.
type notifierBox struct{ n Notifier }

// The types of the four events.
const (
	eventApprovalCreated  = "approval.created"
	eventApprovalDecided  = "approval.decided"
	eventVisibilityChange = "visibility.changed"
	eventNetworkChange    = "network.changed"
)

// SetNotifier sets the receiver of the webhook events. The host calls it after New returns and before the management
// server accepts a request (rule 17), so the events that New makes (a crash-recovered approval, a legacy draft) are not
// sent. A nil notifier turns the events off.
func (s *Service) SetNotifier(n Notifier) {
	s.notifier.Store(&notifierBox{n: n})
}

// notifying reports whether an event would reach a receiver, so that a host without a webhook builds none.
func (s *Service) notifying() bool {
	b := s.notifier.Load()
	return b != nil && b.n != nil
}

// notify hands an event to the notifier. It never blocks, and a service without a notifier drops the event.
func (s *Service) notify(p webhook.Payload) {
	b := s.notifier.Load()
	if b == nil || b.n == nil {
		return
	}
	p.At = s.now().UTC().Format("2006-01-02T15:04:05.000Z")
	b.n.Notify(p)
}

// decidedWord is the decided_by of an event body (rule 4): a fixed word, never a login or an address. It is empty
// for an approval that has no decider, such as a pending one that the deletion of its flat failed.
func decidedWord(decidedBy string) string {
	if decidedBy == "" {
		return ""
	}
	if _, ok := ParseAgentDecider(decidedBy); ok {
		return "autopilot"
	}
	return "operator"
}

// consoleBase is the console address of the request, or the one of the host without a request (rule 4, console_url).
func (s *Service) consoleBase(ctx context.Context) string {
	base := ""
	if s.cfg.ConsoleURLFor != nil {
		base = s.cfg.ConsoleURLFor(ctx)
	} else if s.cfg.ConsoleURL != nil {
		base = s.cfg.ConsoleURL()
	}
	return strings.TrimSuffix(base, "/")
}

// notifyApproval sends approval.created (state pending) or approval.decided (state approved, rejected or failed) for
// the approval a.
func (s *Service) notifyApproval(ctx context.Context, typ, state string, a store.Approval) {
	if !s.notifying() {
		return
	}
	p := webhook.Payload{
		Type: typ, Flat: webhook.FlatRef{Slug: a.Flat, ID: a.FlatID}, Operation: a.Action, State: state,
		ApprovalID: a.ID, ConsoleURL: s.ApprovalURL(ctx, a.ID),
	}
	if typ == eventApprovalCreated {
		p.Via = a.Via
	} else {
		p.DecidedBy = decidedWord(a.DecidedBy)
	}
	s.notify(p)
}

// notifyVisibility sends visibility.changed for a change that approval a applied to flat from (before) and to (after).
func (s *Service) notifyVisibility(ctx context.Context, a store.Approval, from, to store.Flat) {
	if !s.notifying() {
		return
	}
	s.notify(webhook.Payload{
		Type: eventVisibilityChange, Flat: webhook.FlatRef{Slug: from.Slug, ID: from.ID}, Operation: "set_visibility", State: "applied",
		ApprovalID: a.ID, DecidedBy: decidedWord(a.DecidedBy), ConsoleURL: s.consoleBase(ctx) + "/flats/" + from.Slug,
		Change: &webhook.Change{
			From: &webhook.Access{Visibility: string(from.Visibility.Canonical()), Access: from.PublicAccess},
			To:   &webhook.Access{Visibility: string(to.Visibility.Canonical()), Access: to.PublicAccess},
		},
	})
}

// notifyOrigins sends network.changed for a new list of the outbound origins of a flat.
func (s *Service) notifyOrigins(ctx context.Context, f store.Flat, origins []string) {
	if !s.notifying() {
		return
	}
	s.notify(webhook.Payload{
		Type: eventNetworkChange, Flat: webhook.FlatRef{Slug: f.Slug, ID: f.ID}, Operation: "set_network_origins", State: "applied",
		ConsoleURL: s.consoleBase(ctx) + "/flats/" + f.Slug,
		Change:     &webhook.Change{Grant: "origins", Origins: origins},
	})
}

// notifyProvider sends network.changed for a new permission of a flat to use a provider.
func (s *Service) notifyProvider(ctx context.Context, f store.Flat, provider string, permitted bool) {
	if !s.notifying() {
		return
	}
	s.notify(webhook.Payload{
		Type: eventNetworkChange, Flat: webhook.FlatRef{Slug: f.Slug, ID: f.ID}, Operation: "set_provider_permission", State: "applied",
		ConsoleURL: s.consoleBase(ctx) + "/flats/" + f.Slug,
		Change:     &webhook.Change{Grant: "provider", Provider: provider, Permitted: permitted},
	})
}

// visibilityAudit is the Audit of the visibility.set event of an approval that is applied (rule 21): the console actor
// for a decision of the operator, and the text of the Autopilot record for a decision of an agent.
func (s *Service) visibilityAudit(ctx context.Context, a store.Approval) store.Audit {
	if _, ok := ParseAgentDecider(a.DecidedBy); ok {
		return store.Audit{Time: s.now(), Via: store.AuditViaAutopilot, Actor: DeciderText(a.DecidedBy)}
	}
	return s.audit(ctx, ViaConsole)
}
