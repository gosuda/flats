package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestLifecycleMigrationPublishedIdentityAndExplicitPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	for _, slug := range []string{"mixed", "never"} {
		if err := s.CreateFlat(ctx, Flat{Slug: slug, Name: slug, Visibility: PublicUnlisted, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		for n := 1; n <= 4; n++ {
			if err := s.InsertVersion(ctx, Version{Flat: slug, Number: n, Hash: slug + string(rune('0'+n)), Kind: "static", Manifest: json.RawMessage(`{}`), CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.SetLive(ctx, "mixed", 2, 0, "deploy", "", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLive(ctx, "mixed", 3, 2, "deploy", "", now); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertApproval(ctx, Approval{ID: "legacy-public", Flat: "mixed", Action: "set_visibility", Params: json.RawMessage(`{"visibility":"public-listed","from":"public-unlisted"}`), Status: "pending", Via: "api", RequestedAt: now}); err != nil {
		t.Fatal(err)
	}
	// Exercise the migration entry with the baseline schema shape, not just
	// new-store helpers. No provider permission exists in the historical DB.
	for _, stmt := range []string{`ALTER TABLE versions DROP COLUMN published`, `DROP INDEX approvals_open_key`, `ALTER TABLE approvals DROP COLUMN idempotency_key`, `ALTER TABLE approvals DROP COLUMN result_data`, `ALTER TABLE approvals DROP COLUMN decided_by`, `ALTER TABLE approvals DROP COLUMN authorized_at`, `ALTER TABLE deployments DROP COLUMN approval_id`, `ALTER TABLE previews DROP COLUMN target`, `ALTER TABLE previews DROP COLUMN revision`, `PRAGMA user_version=1`} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatal(stmt, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		s, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		vs, err := s.ListVersions(ctx, "mixed")
		if err != nil || len(vs) != 2 || vs[0].Number != 3 || vs[1].Number != 2 || vs[0].Hash != "mixed3" {
			t.Fatalf("published identity %+v %v", vs, err)
		}
		nv, _ := s.ListVersions(ctx, "never")
		if len(nv) != 0 {
			t.Fatal("never-deployed marked published")
		}
		mixed, _ := s.GetFlat(ctx, "mixed")
		never, _ := s.GetFlat(ctx, "never")
		if mixed.Visibility != Public || never.Visibility != Private || mixed.LiveVersion != 3 {
			t.Fatalf("canonical states %+v %+v", mixed, never)
		}
		ps, _ := s.PermittedProviders(ctx, "mixed")
		if len(ps) != 1 || ps[0] != ProviderLocal {
			t.Fatalf("migration invented provider grants %v", ps)
		}
		a, _ := s.GetApproval(ctx, "legacy-public")
		if a.Status != "pending" || string(a.Params) == `{"visibility":"public-listed","from":"public-unlisted"}` {
			t.Fatalf("pending reference lost/unmapped %+v", a)
		}
		hist, _ := s.ListDeployments(ctx, "mixed", 20)
		if len(hist) != 2 || hist[0].Version != 3 || hist[0].Previous != 2 {
			t.Fatal(hist)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLifecycleCommitPublicationIsAtomic(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.CreateFlat(ctx, Flat{Slug: "atomic", Name: "atomic", Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	v := Version{Flat: "atomic", Number: 1, Hash: "one", Kind: "static", Manifest: json.RawMessage(`{}`), Published: true, CreatedAt: now}
	if err := s.CommitPublished(ctx, v, 99, "wrong", now); err == nil {
		t.Fatal("invalid previous live accepted")
	}
	vs, _ := s.ListVersions(ctx, "atomic")
	hist, _ := s.ListDeployments(ctx, "atomic", 10)
	if len(vs) != 0 || len(hist) != 0 {
		t.Fatalf("partial failed publication %v %v", vs, hist)
	}
	if err := s.CommitPublished(ctx, v, 0, "approved", now); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitPublished(ctx, v, 0, "approved", now); err == nil {
		t.Fatal("duplicate publication accepted")
	}
	vs, _ = s.ListVersions(ctx, "atomic")
	hist, _ = s.ListDeployments(ctx, "atomic", 10)
	if len(vs) != 1 || len(hist) != 1 {
		t.Fatalf("duplicates %v %v", vs, hist)
	}
}

func TestLifecycleAuthorizedRejectClaimRacePersistsWinner(t *testing.T) {
	for _, order := range []string{"reject_first", "claim_first", "concurrent"} {
		t.Run(order, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "audit.sqlite")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.Close() }()
			ctx := t.Context()
			now := time.Now().Truncate(time.Millisecond)
			if err := s.CreateFlat(ctx, Flat{Slug: "race", Name: "race", Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			if err := s.InsertApproval(ctx, Approval{ID: "decision", Flat: "race", Action: "delete", Params: json.RawMessage(`{}`), Status: "pending", RequestedAt: now}); err != nil {
				t.Fatal(err)
			}
			claimTime := now.Add(time.Second)
			reject := func() error { return s.RejectApprovalAuthorized(ctx, "decision", "reject-operator", "rejected", now) }
			claim := func() error { return s.ClaimApprovalAuthorized(ctx, "decision", "approve-operator", claimTime) }
			var rejected, claimed error
			switch order {
			case "reject_first":
				rejected = reject()
				claimed = claim()
			case "claim_first":
				claimed = claim()
				rejected = reject()
			case "concurrent":
				start := make(chan struct{})
				done := make(chan error, 1)
				go func() { <-start; done <- reject() }()
				close(start)
				claimed = claim()
				rejected = <-done
			}
			if (rejected == nil) == (claimed == nil) {
				t.Fatalf("want exactly one winner: %v / %v", rejected, claimed)
			}
			actor, status, at := "reject-operator", "rejected", now
			if claimed == nil {
				actor, status, at = "approve-operator", "applying", claimTime
			}
			// Neither a late same decision nor opposite claim may overwrite persisted audit.
			if err := s.RejectApprovalAuthorized(ctx, "decision", "late-operator", "late", now.Add(2*time.Second)); err == nil {
				t.Fatal("late reject won")
			}
			if err := s.ClaimApprovalAuthorized(ctx, "decision", "late-operator", now.Add(2*time.Second)); err == nil {
				t.Fatal("late claim won")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			a, err := s.GetApproval(ctx, "decision")
			if err != nil || a.Status != status || a.DecidedBy != actor || a.AuthorizedAt == nil || !a.AuthorizedAt.Equal(at) {
				t.Fatalf("durable winner: %+v %v", a, err)
			}
			if rejected == nil && (a.DecidedAt == nil || !a.DecidedAt.Equal(at) || a.Result != "rejected") {
				t.Fatalf("rejection final audit: %+v", a)
			}
			if claimed == nil && a.DecidedAt != nil {
				t.Fatal("losing reject finalized claimed approval")
			}
		})
	}
}
