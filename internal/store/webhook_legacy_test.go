package store

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// docs/spec/event-webhook.md, rule 21: PreserveLegacyTailscale inserts a per-flat grant outside the setter of the
// provider permission, at the start of a legacy service that names `--network tailscale`. It writes no provider.set row.
// The fixture is the one of TestExplicitLegacyTailscaleUpgradeDoesNotInventPublicPermissions.
func TestTheLegacyTailscaleGrantWritesNoProviderSetRow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.CreateFlat(ctx, Flat{Slug: "legacy", Name: "legacy", Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	// A database from before the lifecycle: the migration notes its flats as waiting for the operator's choice.
	if _, err := s.db.Exec(`PRAGMA user_version=1`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	if err := s.PreserveLegacyTailscale(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ProviderPermitted(ctx, "legacy", ProviderTailscale); err != nil || !ok {
		t.Fatalf("tailscale is permitted = %t, %v; the test needs the grant that PreserveLegacyTailscale inserts", ok, err)
	}
	if got := auditKinds(allAudit(t, s)); len(got) != 0 {
		t.Errorf("the legacy grant wrote the audit rows %v, want none", got)
	}
	// A second start finds nothing to insert and writes nothing either.
	if err := s.PreserveLegacyTailscale(ctx); err != nil {
		t.Fatal(err)
	}
	if got := auditKinds(allAudit(t, s)); len(got) != 0 {
		t.Errorf("a second call wrote the audit rows %v, want none", got)
	}

	// The setter sees the inserted grant as the value before its change: a revocation is a change and writes the
	// one row of this log.
	changed, err := s.SetProviderPermission(ctx, "legacy", ProviderTailscale, false, Audit{Via: AuditViaConsole})
	if err != nil || !changed {
		t.Fatalf("the revocation of the legacy grant = %t, %v; want a change", changed, err)
	}
	evs := allAudit(t, s)
	if !slices.Equal(auditKinds(evs), []string{AuditProviderSet}) || evs[0].Flat != "legacy" || evs[0].Name != ProviderTailscale {
		t.Errorf("audit rows after the revocation = %+v, want one provider.set of tailscale", evs)
	}
}
