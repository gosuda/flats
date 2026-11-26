package app

import (
	"context"
	"os/user"
	"time"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

// The audit helpers of docs/spec/audit-events.md: how a change of a sealed value is said to have been made.

// consoleAudit is the Audit of a change that a console route makes. The route puts the identity of the caller in
// the context with core.WithAuditActor.
func consoleAudit(ctx context.Context) store.Audit {
	return store.Audit{Via: store.AuditViaConsole, Actor: core.AuditActorOf(ctx)}
}

// commandAudit is the Audit of a command that the operator ran on the host. The identity is the name of the
// operating-system user, which is "root" under sudo (rule 5 of the spec).
func commandAudit() store.Audit {
	return store.Audit{Via: store.AuditViaCommand, Actor: operatorUser()}
}

// commandEvent is an audit event that a command writes after its work: the kind, the channel command, the operating-system
// user and the fields.
func commandEvent(kind string, fields map[string]any) store.AuditEvent {
	return store.AuditEvent{Time: time.Now(), Kind: kind, Via: store.AuditViaCommand, Actor: operatorUser(), Data: store.AuditData(fields)}
}

// operatorUser returns the name of the operating-system user that runs the process, or "" when the system gives none.
func operatorUser() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Username
}
