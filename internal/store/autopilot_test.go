package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func setAutopilot(t *testing.T, s *Store, enabled bool, ops uint16) {
	t.Helper()
	_, _, err := s.UpdateAutopilot(context.Background(), time.Now(), func(r AutopilotRow) (AutopilotRow, string, error) {
		r.Enabled, r.Ops = enabled, ops
		return r, "fixture", nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func pendingApproval(t *testing.T, s *Store, id string) {
	t.Helper()
	if err := s.InsertApproval(context.Background(), Approval{ID: id, Flat: "f", Action: "delete", Params: []byte(`{"slug":"f"}`), Status: "pending", Via: "mcp", RequestedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

// Migration 21 adds the settings row off, with no operation and no guide,
// on a new database and on a schema 20 one.
func TestMigrationAutopilot(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, s *Store) {
		t.Helper()
		r, err := s.GetAutopilot(ctx)
		if err != nil || r != (AutopilotRow{}) {
			t.Fatalf("row %+v %v", r, err)
		}
		_, changes, err := s.AutopilotSync(ctx)
		if err != nil || len(changes) != 0 {
			t.Fatalf("changes %+v %v", changes, err)
		}
	}
	t.Run("new", func(t *testing.T) { check(t, open(t)) })
	t.Run("schema 20", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f.db")
		saved := migrations
		migrations = saved[:len(saved)-1]
		s, err := Open(path)
		migrations = saved
		if err != nil {
			t.Fatal(err)
		}
		pendingApproval(t, s, "a1")
		s.Close()
		if info, err := Inspect(path); err != nil || info.Version != 20 {
			t.Fatalf("fixture %+v %v", info, err)
		}
		s, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		check(t, s)
		if a, err := s.GetApproval(ctx, "a1"); err != nil || a.Status != "pending" {
			t.Fatalf("approval %+v %v", a, err)
		}
	})
}

func TestAutopilotChecks(t *testing.T) {
	s := open(t)
	for _, stmt := range []string{
		`INSERT INTO autopilot (id) VALUES (2)`,
		`UPDATE autopilot SET enabled=2`,
		`UPDATE autopilot SET ops=-1`,
		`UPDATE autopilot SET ops=512`,
		`INSERT INTO autopilot_changes (at, line, logged) VALUES (1, 'x', 2)`,
	} {
		if _, err := s.db.Exec(stmt); err == nil {
			t.Errorf("%s accepted", stmt)
		}
	}
	if _, err := s.db.Exec(`UPDATE autopilot SET ops=511`); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateAutopilot(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.UnixMilli(1700000000123).UTC()
	r, wrote, err := s.UpdateAutopilot(ctx, now, func(r AutopilotRow) (AutopilotRow, string, error) {
		r.Enabled, r.Ops, r.Guide = true, 5, "guide"
		return r, "turned on", nil
	})
	if err != nil || !wrote || !r.Enabled || r.Ops != 5 || r.Guide != "guide" || !r.UpdatedAt.Equal(now) {
		t.Fatalf("update %+v %t %v", r, wrote, err)
	}
	if got, _ := s.GetAutopilot(ctx); got != r {
		t.Fatalf("stored %+v, returned %+v", got, r)
	}
	// The same settings write nothing and append no line.
	r2, wrote, err := s.UpdateAutopilot(ctx, now.Add(time.Hour), func(r AutopilotRow) (AutopilotRow, string, error) { return r, "again", nil })
	if err != nil || wrote || r2 != r {
		t.Fatalf("no-op %+v %t %v", r2, wrote, err)
	}
	// An error from change rolls back and is returned as is.
	boom := errors.New("boom")
	if _, wrote, err := s.UpdateAutopilot(ctx, now, func(r AutopilotRow) (AutopilotRow, string, error) {
		r.Enabled = false
		return r, "x", boom
	}); err != boom || wrote {
		t.Fatalf("change error %t %v", wrote, err)
	}
	// A cancelled context fails inside the transaction and rolls back.
	cctx, cancel := context.WithCancel(ctx)
	if _, _, err := s.UpdateAutopilot(cctx, now, func(r AutopilotRow) (AutopilotRow, string, error) {
		cancel()
		r.Enabled = false
		return r, "cancelled", nil
	}); err == nil {
		t.Fatal("cancelled update committed")
	}
	if got, _ := s.GetAutopilot(ctx); got != r {
		t.Fatalf("rolled back update changed the row: %+v", got)
	}
	row, changes, err := s.AutopilotSync(ctx)
	if err != nil || row != r || len(changes) != 1 || changes[0].Line != "turned on" || !changes[0].At.Equal(now) {
		t.Fatalf("sync %+v %+v %v", row, changes, err)
	}
	// The connection is usable again after each rollback.
	if _, err := s.GetApproval(ctx, "none"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestAutopilotChangeLog(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	for i := range 130 {
		if _, _, err := s.UpdateAutopilot(ctx, time.Now(), func(r AutopilotRow) (AutopilotRow, string, error) {
			r.Guide = fmt.Sprint(i)
			return r, fmt.Sprint("line ", i), nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	_, changes, err := s.AutopilotSync(ctx)
	if err != nil || len(changes) != 130 || changes[0].Line != "line 0" || changes[129].Line != "line 129" {
		t.Fatalf("changes %d %v", len(changes), err)
	}
	if err := s.MarkAutopilotLogged(ctx, changes[119].Seq); err != nil {
		t.Fatal(err)
	}
	_, rest, _ := s.AutopilotSync(ctx)
	if len(rest) != 10 || rest[0].Line != "line 120" {
		t.Fatalf("unlogged %+v", rest)
	}
	var kept int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM autopilot_changes WHERE logged=1`).Scan(&kept); err != nil || kept != autopilotChangesKept {
		t.Fatalf("kept %d %v", kept, err)
	}
	if err := s.MarkAutopilotLogged(ctx, rest[9].Seq); err != nil {
		t.Fatal(err)
	}
	if _, rest, _ = s.AutopilotSync(ctx); len(rest) != 0 {
		t.Fatalf("unlogged %+v", rest)
	}
}

func TestAutopilotClaimAndReject(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()
	const need = 1<<8 | 1<<5
	for _, tc := range []struct {
		name    string
		enabled bool
		ops     uint16
	}{{"off", false, 511}, {"missing bit", true, 1 << 8}, {"no bit", true, 0}} {
		setAutopilot(t, s, tc.enabled, tc.ops)
		id := "a-" + tc.name
		pendingApproval(t, s, id)
		if err := s.ClaimApprovalAutopilot(ctx, id, "agent", need, now); !errors.Is(err, ErrAutopilotDenied) {
			t.Fatalf("%s claim: %v", tc.name, err)
		}
		if err := s.RejectApprovalAutopilot(ctx, id, "agent", "rejected", need, now); !errors.Is(err, ErrAutopilotDenied) {
			t.Fatalf("%s reject: %v", tc.name, err)
		}
		if a, _ := s.GetApproval(ctx, id); a.Status != "pending" || a.DecidedBy != "" || a.AuthorizedAt != nil {
			t.Fatalf("%s changed the approval %+v", tc.name, a)
		}
	}
	setAutopilot(t, s, true, need|1)
	pendingApproval(t, s, "claim")
	if err := s.ClaimApprovalAutopilot(ctx, "claim", "agent", need, now); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.GetApproval(ctx, "claim"); a.Status != "applying" || a.DecidedBy != "agent" || a.AuthorizedAt == nil {
		t.Fatalf("claimed %+v", a)
	}
	pendingApproval(t, s, "reject")
	if err := s.RejectApprovalAutopilot(ctx, "reject", "agent", "rejected by agent", need, now); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.GetApproval(ctx, "reject"); a.Status != "rejected" || a.Result != "rejected by agent" || a.DecidedBy != "agent" || a.DecidedAt == nil {
		t.Fatalf("rejected %+v", a)
	}
	// An approval that is no longer pending is reported as such, even
	// while Autopilot would not allow it.
	setAutopilot(t, s, false, 0)
	for _, id := range []string{"claim", "reject"} {
		if err := s.ClaimApprovalAutopilot(ctx, id, "agent", need, now); !errors.Is(err, ErrApprovalNotPending) {
			t.Fatalf("claim of %s: %v", id, err)
		}
		if err := s.RejectApprovalAutopilot(ctx, id, "agent", "x", need, now); !errors.Is(err, ErrApprovalNotPending) {
			t.Fatalf("reject of %s: %v", id, err)
		}
	}
	if err := s.ClaimApprovalAutopilot(ctx, "missing", "agent", need, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := s.ClaimApprovalAutopilot(ctx, "claim", "agent", 0, now); err == nil {
		t.Fatal("need 0 accepted")
	}
}

// A setter turning a needed operation off and a claim race on separate
// connections. The setter holds the write lock from before it reads until
// it commits, so the approval status it sees there is the status at its
// commit: a claim succeeds exactly when it committed before the setter.
func TestAutopilotClaimRacesSetter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "f.db")
	setter, claimer, reader := open2(t, path), open2(t, path), open2(t, path)
	const need = 1 << 2
	claimed := 0
	for i := range 40 {
		id := fmt.Sprint("race-", i)
		setAutopilot(t, setter, true, need)
		pendingApproval(t, setter, id)
		var seen string
		var claimErr, setErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _, setErr = setter.UpdateAutopilot(ctx, time.Now(), func(r AutopilotRow) (AutopilotRow, string, error) {
				a, err := reader.GetApproval(ctx, id)
				if err != nil {
					return r, "", err
				}
				seen = a.Status
				r.Ops = 0
				return r, "off", nil
			})
		}()
		go func() {
			defer wg.Done()
			claimErr = claimer.ClaimApprovalAutopilot(ctx, id, "agent", need, time.Now())
		}()
		wg.Wait()
		if setErr != nil {
			t.Fatal(setErr)
		}
		a, _ := reader.GetApproval(ctx, id)
		switch {
		case claimErr == nil && seen == "applying" && a.Status == "applying":
			claimed++
		case errors.Is(claimErr, ErrAutopilotDenied) && seen == "pending" && a.Status == "pending":
		default:
			t.Fatalf("round %d: claim %v, setter saw %q, approval %q", i, claimErr, seen, a.Status)
		}
	}
	t.Logf("claims that won: %d of 40", claimed)
}

// Two writers changing different bits keep both changes.
func TestUpdateAutopilotConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "f.db")
	a, b := open2(t, path), open2(t, path)
	for range 20 {
		setAutopilot(t, a, false, 0)
		if err := a.MarkAutopilotLogged(ctx, 1<<62); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i, s := range []*Store{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, _, err := s.UpdateAutopilot(ctx, time.Now(), func(r AutopilotRow) (AutopilotRow, string, error) {
					r.Ops |= 1 << i
					return r, fmt.Sprint("bit ", i), nil
				}); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		r, changes, err := a.AutopilotSync(ctx)
		if err != nil || r.Ops != 3 || len(changes) != 2 {
			t.Fatalf("row %+v changes %+v %v", r, changes, err)
		}
	}
}
