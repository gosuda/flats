package core

import (
	"context"

	"github.com/gosuda/flats/internal/store"
)

// auditActorKey carries the identity of whoever asked for a change of a sealed value, for its audit event
// (rule 5 of docs/spec/audit-events.md).
type auditActorKey struct{}

// WithAuditActor returns ctx with the identity that the channel knows, such as the text that the console derives
// from the request. Without one the audit event has an empty actor.
func WithAuditActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, auditActorKey{}, actor)
}

// AuditActorOf returns the identity that WithAuditActor stored, or "".
func AuditActorOf(ctx context.Context) string {
	s, _ := ctx.Value(auditActorKey{}).(string)
	return s
}

// audit is the Audit of a change that the service makes for the surface via. The channel names are those of
// rule 4 of the spec.
func (s *Service) audit(ctx context.Context, via Via) store.Audit {
	return store.Audit{Time: s.now(), Via: string(via), Actor: AuditActorOf(ctx)}
}
